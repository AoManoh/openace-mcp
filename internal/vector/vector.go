// Package vector 实现 local-hybrid 的 exact 向量索引（迁移方案 §11.2）：
// float32 LE 行主序持久化、L2 归一化 + dot（等价 cosine）相似度、有界并行
// 的确定性 brute-force top-k。ANN 属 Stage 5 裁决，本包只承诺 exact 语义。
//
// 文件形态（位于 segment 目录内，随 revision 不可变）：
//   - vectors.dat：count × dimension 个 float32（小端）
//   - vectors.idx：JSON 头 + 行到 chunk 身份（ID + 纯 content hash）的映射
package vector

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"context"

	mmap "github.com/blevesearch/mmap-go"
)

const (
	// DataFileName 是向量数据文件名。
	DataFileName = "vectors.dat"
	// IndexFileName 是行映射文件名。
	IndexFileName = "vectors.idx"
	// indexSchemaVersion 随 idx 结构不兼容变化而递增。
	indexSchemaVersion = 1
	// cancelCheckRows 是并行扫描的取消检查粒度。
	cancelCheckRows = 2048
)

// ErrEnvelopeExceeded 表示向量行数超出用户配置的内存预算
// （OPENACE_VECTOR_MEMORY_BUDGET 折算的行数上限），语义路应显式降级。
//
// 历史：这里曾有写死的 DefaultMaxResidentVectors 默认上限（250K→400K，
// annbench 实测 p50 86→~128ms、常驻 ≈1.6GB，记录在 sealed 报告），超限
// 一律降级词法。2026-08-26 用户裁决移除默认上限：仓库越大反而失去语义
// 检索与产品目标矛盾，能力默认不设限；资源受限环境自行配置预算，规模
// 对应的内存/延迟实测数据见 A&Q 文档。
var ErrEnvelopeExceeded = errors.New("vector index exceeds configured memory budget")

// Entry 是一行向量对应的 chunk 身份；ContentHash 是历史字段名，当前
// 子树承载 localengine embedKey(模板/path/symbol/language/content hash)，
// 供跨 revision/profile 复用；具体键语义由 profile/template 身份约束。
type Entry struct {
	ID          string `json:"id"`
	ContentHash string `json:"content_hash"`
}

// Hit 是一次 top-k 检索的命中。
type Hit struct {
	ID          string
	ContentHash string
	Score       float64
}

// indexHeader 是 vectors.idx 的 JSON 结构。
type indexHeader struct {
	SchemaVersion int     `json:"schema_version"`
	Dimension     int     `json:"dimension"`
	Count         int     `json:"count"`
	Entries       []Entry `json:"entries"`
}

// Normalize 就地 L2 归一化（阶段计划 D5）；零向量与非有限分量拒绝
// （暗坑 K35，调用方把该 chunk 记为未覆盖）。
func Normalize(v []float32) error {
	var sum float64
	for _, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("vector contains non-finite component")
		}
		sum += f * f
	}
	if sum == 0 {
		return errors.New("vector has zero norm")
	}
	inv := 1 / math.Sqrt(sum)
	for i := range v {
		v[i] = float32(float64(v[i]) * inv)
	}
	return nil
}

// Write 把已归一化的向量集写入 dir（staging 内），返回两个文件的 sha256。
// 结构校验：条目对齐、维度一致、分量有限、范数近 1（未归一化输入是编程
// 错误，直接拒绝）。count 允许为 0（空集合法）。
func Write(dir string, dimension int, entries []Entry, vectors [][]float32) (dataChecksum string, indexChecksum string, err error) {
	if len(entries) != len(vectors) {
		return "", "", fmt.Errorf("entries/vectors 数量不一致: %d vs %d", len(entries), len(vectors))
	}
	if dimension <= 0 {
		return "", "", errors.New("dimension 必须为正")
	}
	// 流式写 vectors.dat:旧实现先构造 count×dim×4 的完整 byte[]，
	// 与调用方已持有的 float32 vectors 同时常驻；k8s 345K×1024 时
	// 平白增加约1.4GiB峰值。按行复用小缓冲并同步计算 checksum。
	dataChecksum, err = writeVectorData(filepath.Join(dir, DataFileName), dimension, vectors)
	if err != nil {
		return "", "", err
	}
	header := indexHeader{SchemaVersion: indexSchemaVersion, Dimension: dimension, Count: len(entries), Entries: entries}
	idxBytes, err := json.Marshal(header)
	if err != nil {
		return "", "", err
	}
	if err := writeFileSync(filepath.Join(dir, IndexFileName), idxBytes); err != nil {
		return "", "", err
	}
	return dataChecksum, checksumBytes(idxBytes), nil
}

func writeVectorData(path string, dimension int, vectors [][]float32) (checksum string, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	hash := sha256.New()
	writer := bufio.NewWriterSize(io.MultiWriter(f, hash), 4<<20)
	row := make([]byte, dimension*4)
	for i, v := range vectors {
		if len(v) != dimension {
			return "", fmt.Errorf("第 %d 行维度 %d 与配置 %d 不符", i, len(v), dimension)
		}
		var sum float64
		for j, x := range v {
			value := float64(x)
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return "", fmt.Errorf("第 %d 行含非有限分量", i)
			}
			sum += value * value
			binary.LittleEndian.PutUint32(row[j*4:], math.Float32bits(x))
		}
		if math.Abs(sum-1) > 0.01 {
			return "", fmt.Errorf("第 %d 行未归一化（norm²=%.4f）；必须先经 Normalize", i, sum)
		}
		if _, err := writer.Write(row); err != nil {
			return "", err
		}
	}
	if err := writer.Flush(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	closed = true
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// writeFileSync 以 0600 + O_EXCL 写入并落盘（staging 内新文件）。
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func checksumBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Index 是只读 exact 索引。向量数据默认经 mmap 映射为文件后备页(内核
// 可按内存压力回收,不占 Go heap);entries 行映射仍常驻 heap。用完必须
// Close(munmap+关文件句柄),否则映射泄漏、Windows 上段目录无法删除。
type Index struct {
	dimension int
	entries   []Entry
	data      []float32
	closed    atomic.Bool
	mapped    mmap.MMap // 非 nil = data 是映射页的别名视图
	file      *os.File  // 映射期间必须存活的文件句柄
}

// Load 读取并校验向量文件（暗坑 K24/K25）：checksum、schema、尺寸对齐、
// 可选内存预算。maxVectors ≤ 0 = 不限（默认，2026-08-26 裁决：上限只
// 来自用户配置的字节预算折算）；>0 且行数超出时返回 ErrEnvelopeExceeded。
// 任何校验失败返回错误，由调用方决定语义路降级。
//
// 数据驻留形态由进程级 OPENACE_VECTOR_MMAP 决定(默认 mmap;详见
// loadUseMmap),两种形态的校验语义与检索结果逐位一致。
func Load(dir string, wantDimension int, wantDataChecksum string, wantIndexChecksum string, maxVectors int) (*Index, error) {
	return load(dir, wantDimension, wantDataChecksum, wantIndexChecksum, maxVectors, loadUseMmap())
}

func load(dir string, wantDimension int, wantDataChecksum string, wantIndexChecksum string, maxVectors int, useMmap bool) (*Index, error) {
	idxBytes, err := os.ReadFile(filepath.Join(dir, IndexFileName))
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", IndexFileName, err)
	}
	if wantIndexChecksum != "" && checksumBytes(idxBytes) != wantIndexChecksum {
		return nil, fmt.Errorf("%s checksum 不符（文件损坏）", IndexFileName)
	}
	var header indexHeader
	if err := json.Unmarshal(idxBytes, &header); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", IndexFileName, err)
	}
	if header.SchemaVersion != indexSchemaVersion {
		return nil, fmt.Errorf("%s schema version %d 不受支持（期望 %d）", IndexFileName, header.SchemaVersion, indexSchemaVersion)
	}
	if header.Dimension != wantDimension {
		return nil, fmt.Errorf("向量维度 %d 与当前 profile %d 不符（禁止混用，K24）", header.Dimension, wantDimension)
	}
	if header.Count != len(header.Entries) {
		return nil, fmt.Errorf("%s count %d 与条目数 %d 不符", IndexFileName, header.Count, len(header.Entries))
	}
	if maxVectors > 0 && header.Count > maxVectors {
		return nil, fmt.Errorf("%w: %d > %d", ErrEnvelopeExceeded, header.Count, maxVectors)
	}
	dataPath := filepath.Join(dir, DataFileName)
	info, err := os.Stat(dataPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", DataFileName, err)
	}
	wantBytes := int64(header.Count) * int64(header.Dimension) * 4
	if info.Size() != wantBytes {
		return nil, fmt.Errorf("%s 尺寸 %d 与 count×dim×4=%d 不符（K24）", DataFileName, info.Size(), wantBytes)
	}
	// 空集不建映射(零长度文件不可 mmap);data 为空切片。
	if useMmap && hostLittleEndian && header.Count > 0 {
		return loadMapped(dataPath, header, wantDataChecksum)
	}
	return loadHeap(dataPath, header, wantDataChecksum)
}

// loadMapped 把 vectors.dat 映射为只读文件后备页并整读校验 checksum
// (校验语义与 heap 形态一致:损坏段在 Load 期拒载,保 K25 自愈链;顺序
// 触读同时完成页预热,冷启动读盘量与 heap 形态相同)。磁盘格式为小端
// float32,小端主机上直接把映射字节按主机序转 []float32 视图,零解码
// 零 heap 拷贝;大端主机由调用方走 heap 解码路径。
//
// 崩溃面(与 bleve zap 词法段同姿态,不设 SetPanicOnFault):segment 文件
// 随 revision 不可变、0600、进程锁纪律护住写路径;映射存活期间文件被
// 外部截断在 Unix 上是 SIGBUS 进程崩溃,属部署红线而非可恢复错误。
func loadMapped(dataPath string, header indexHeader, wantDataChecksum string) (*Index, error) {
	f, err := os.Open(dataPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", DataFileName, err)
	}
	mapped, err := mmap.Map(f, mmap.RDONLY, 0)
	if err != nil {
		// 文件系统不支持映射(部分网络盘/特殊挂载):回退 heap 解码。
		// 两形态检索结果逐位一致,不构成能力降级;驻留形态可经
		// Mapped() 观测。
		_ = f.Close()
		return loadHeap(dataPath, header, wantDataChecksum)
	}
	if wantDataChecksum != "" {
		hash := sha256.New()
		_, _ = hash.Write(mapped)
		if hex.EncodeToString(hash.Sum(nil)) != wantDataChecksum {
			_ = mapped.Unmap()
			_ = f.Close()
			return nil, fmt.Errorf("%s checksum 不符（文件损坏）", DataFileName)
		}
	}
	data := unsafe.Slice((*float32)(unsafe.Pointer(&mapped[0])), header.Count*header.Dimension)
	return &Index{dimension: header.Dimension, entries: header.Entries, data: data, mapped: mapped, file: f}, nil
}

// loadHeap 流式校验+解码进 Go heap(mmap 之前的原始驻留形态,保留作
// 逃生门与大端主机路径):旧实现同时常驻完整 dataBytes 与 float32 data,
// 大仓单段瞬时约翻倍,现仅保留最终 float32 与 64KiB 缓冲。
func loadHeap(dataPath string, header indexHeader, wantDataChecksum string) (*Index, error) {
	f, err := os.Open(dataPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", DataFileName, err)
	}
	defer f.Close()
	hash := sha256.New()
	reader := io.TeeReader(bufio.NewReaderSize(f, 4<<20), hash)
	data := make([]float32, header.Count*header.Dimension)
	const floatsPerBlock = 16 * 1024
	buf := make([]byte, floatsPerBlock*4)
	for offset := 0; offset < len(data); {
		count := len(data) - offset
		if count > floatsPerBlock {
			count = floatsPerBlock
		}
		block := buf[:count*4]
		if _, err := io.ReadFull(reader, block); err != nil {
			return nil, fmt.Errorf("读取 %s: %w", DataFileName, err)
		}
		for i := 0; i < count; i++ {
			data[offset+i] = math.Float32frombits(binary.LittleEndian.Uint32(block[i*4:]))
		}
		offset += count
	}
	if wantDataChecksum != "" && hex.EncodeToString(hash.Sum(nil)) != wantDataChecksum {
		return nil, fmt.Errorf("%s checksum 不符（文件损坏）", DataFileName)
	}
	return &Index{dimension: header.Dimension, entries: header.Entries, data: data}, nil
}

// hostLittleEndian 在进程启动时判定主机字节序;磁盘格式固定小端,大端
// 主机走 heap 解码保证正确性(目标发布平台 amd64/arm64 均为小端)。
var hostLittleEndian = func() bool {
	var probe uint16 = 1
	return *(*byte)(unsafe.Pointer(&probe)) == 1
}()

// loadUseMmap 解析进程级驻留形态开关:OPENACE_VECTOR_MMAP 空或真值=
// mmap(默认),假值(0/false/off/no)=heap 解码逃生门(mmap 语义异常的
// 文件系统/内核环境用)。运维参数,不入 profile 指纹(对齐预算先例)。
var loadModeOnce struct {
	once sync.Once
	mmap bool
}

func loadUseMmap() bool {
	loadModeOnce.once.Do(func() {
		raw := strings.TrimSpace(strings.ToLower(os.Getenv("OPENACE_VECTOR_MMAP")))
		switch raw {
		case "0", "false", "off", "no":
			loadModeOnce.mmap = false
		default:
			loadModeOnce.mmap = true
		}
	})
	return loadModeOnce.mmap
}

// Close 释放向量数据(幂等):映射形态 munmap 并关闭文件句柄,heap 形态
// 仅断开引用交还 GC。调用方必须保证无在飞检索/Row 使用(引擎侧由段缓存
// 引用计数与句柄 refcount 保证);Close 后 Search/SearchFiltered 返回
// 显式错误,Row 对已释放数据 panic(契约违规,快速失败优于脏读)。
func (ix *Index) Close() error {
	if !ix.closed.CompareAndSwap(false, true) {
		return nil
	}
	ix.data = nil
	var err error
	if ix.mapped != nil {
		err = ix.mapped.Unmap()
		ix.mapped = nil
	}
	if ix.file != nil {
		if cerr := ix.file.Close(); err == nil {
			err = cerr
		}
		ix.file = nil
	}
	return err
}

// Mapped 报告向量数据当前是否为文件后备驻留(诊断/探针用)。
func (ix *Index) Mapped() bool {
	return ix.mapped != nil
}

// RowReader 按行读取向量文件:只整读并校验 vectors.idx(行映射),
// vectors.dat 不整读、不做全文件 checksum——为"delta 构建只需极少数
// prior 行"的场景省去整段装载。
//
// 完整性口径(显式弱化,与 Load 不同):数据文件只做尺寸校验+每行读取
// 时的范数合理性探针(写路径强制单位范数,norm² 偏离 1 即拒绝该行)。
// 范数探针能拦截错位/截断/大面积损坏,不能拦截保范数的位翻转;需要
// 全文件强校验的路径(查询索引/整段复用)继续走 Load。
type RowReader struct {
	dimension int
	entries   []Entry
	file      *os.File
	closed    atomic.Bool
}

// OpenRowReader 打开按行读取器;idx 校验语义与 Load 一致(checksum/
// schema/维度/count/数据尺寸对齐)。
func OpenRowReader(dir string, wantDimension int, wantIndexChecksum string) (*RowReader, error) {
	idxBytes, err := os.ReadFile(filepath.Join(dir, IndexFileName))
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", IndexFileName, err)
	}
	if wantIndexChecksum != "" && checksumBytes(idxBytes) != wantIndexChecksum {
		return nil, fmt.Errorf("%s checksum 不符（文件损坏）", IndexFileName)
	}
	var header indexHeader
	if err := json.Unmarshal(idxBytes, &header); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", IndexFileName, err)
	}
	if header.SchemaVersion != indexSchemaVersion {
		return nil, fmt.Errorf("%s schema version %d 不受支持（期望 %d）", IndexFileName, header.SchemaVersion, indexSchemaVersion)
	}
	if header.Dimension != wantDimension {
		return nil, fmt.Errorf("向量维度 %d 与当前 profile %d 不符（禁止混用，K24）", header.Dimension, wantDimension)
	}
	if header.Count != len(header.Entries) {
		return nil, fmt.Errorf("%s count %d 与条目数 %d 不符", IndexFileName, header.Count, len(header.Entries))
	}
	dataPath := filepath.Join(dir, DataFileName)
	info, err := os.Stat(dataPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", DataFileName, err)
	}
	if wantBytes := int64(header.Count) * int64(header.Dimension) * 4; info.Size() != wantBytes {
		return nil, fmt.Errorf("%s 尺寸 %d 与 count×dim×4=%d 不符（K24）", DataFileName, info.Size(), wantBytes)
	}
	f, err := os.Open(dataPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", DataFileName, err)
	}
	return &RowReader{dimension: header.Dimension, entries: header.Entries, file: f}, nil
}

// Entries 返回行映射(调用方不得修改)。
func (r *RowReader) Entries() []Entry {
	return r.entries
}

// ReadRow 读取第 i 行向量(pread+小端解码+范数探针);返回的切片为
// 独立拷贝,不随 Close 失效。
func (r *RowReader) ReadRow(i int) ([]float32, error) {
	if r.closed.Load() {
		return nil, errors.New("row reader 已关闭")
	}
	if i < 0 || i >= len(r.entries) {
		return nil, fmt.Errorf("行号 %d 越界(count=%d)", i, len(r.entries))
	}
	buf := make([]byte, r.dimension*4)
	if _, err := r.file.ReadAt(buf, int64(i)*int64(r.dimension)*4); err != nil {
		return nil, fmt.Errorf("读取第 %d 行: %w", i, err)
	}
	row := make([]float32, r.dimension)
	var sum float64
	for j := range row {
		value := math.Float32frombits(binary.LittleEndian.Uint32(buf[j*4:]))
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("第 %d 行含非有限分量(疑似损坏)", i)
		}
		sum += f * f
		row[j] = value
	}
	// 写路径强制单位范数(writeVectorData);读出偏离即结构性损坏。
	if math.Abs(sum-1) > 0.01 {
		return nil, fmt.Errorf("第 %d 行范数异常(norm²=%.4f,疑似错位/损坏)", i, sum)
	}
	return row, nil
}

// Close 关闭数据文件句柄(幂等)。
func (r *RowReader) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	return r.file.Close()
}

// Count 返回向量行数。
func (ix *Index) Count() int {
	return len(ix.entries)
}

// Dimension 返回向量维度。
func (ix *Index) Dimension() int {
	return ix.dimension
}

// Entries 返回行映射（复用键控与调试用；调用方不得修改）。
func (ix *Index) Entries() []Entry {
	return ix.entries
}

// Row 返回第 i 行向量的只读视图（跨 revision 复用拷贝用）。
func (ix *Index) Row(i int) []float32 {
	return ix.data[i*ix.dimension : (i+1)*ix.dimension]
}

// Search 返回 query 的 exact top-k：结果与单线程 brute-force 完全一致
// （§11.2），并行仅是加速手段；tie-break 按 (score desc, ID asc) 保证
// 确定性（暗坑 K27）。query 会被就地归一化。
func (ix *Index) Search(ctx context.Context, query []float32, topK int) ([]Hit, error) {
	return ix.SearchFiltered(ctx, query, topK, nil)
}

// SearchFiltered 同 Search,allow 非 nil 时只在谓词放行的条目中选 topK
// (P-gray-02 前缀下推:被海量无关子树淹没时,受限深度的全局 topK 再过滤
// 会漏掉目标子树;谓词在选择阶段生效,放行行的评分与无谓词时逐位相同)。
// allow 会被多个评分 worker 并发调用,必须是并发安全的纯函数(包内唯一
// 实现 chunkPrefixPredicate 只读不可变 map,满足)。
//
// 选择用每 worker 有界堆+单线程归并取代全量排序:临时内存从 O(行数)
// 降到 O(topK×workers),行序遍历/逐行求和序/全序比较器均与全排版一致,
// 结果逐位等价(等价性由 referenceTopK 对照测试锁定)。
func (ix *Index) SearchFiltered(ctx context.Context, query []float32, topK int, allow func(id string) bool) ([]Hit, error) {
	if ix.closed.Load() {
		return nil, errors.New("vector index 已关闭(use-after-close 是调用方生命周期缺陷)")
	}
	if len(query) != ix.dimension {
		return nil, fmt.Errorf("查询维度 %d 与索引 %d 不符", len(query), ix.dimension)
	}
	if topK <= 0 || ix.Count() == 0 {
		return nil, nil
	}
	if err := Normalize(query); err != nil {
		return nil, fmt.Errorf("查询向量非法: %w", err)
	}
	count := ix.Count()

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers > count {
		workers = 1
	}
	chunkRows := (count + workers - 1) / workers
	locals := make([][]topKCandidate, workers)
	var wg sync.WaitGroup
	cancelled := false
	var cancelMu sync.Mutex
	for w := 0; w < workers; w++ {
		start := w * chunkRows
		end := start + chunkRows
		if end > count {
			end = count
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			dim := ix.dimension
			capacity := topK
			if rows := end - start; capacity > rows {
				capacity = rows
			}
			local := topKSelector{ix: ix, capacity: capacity}
			for row := start; row < end; row++ {
				if (row-start)%cancelCheckRows == 0 && ctx.Err() != nil {
					cancelMu.Lock()
					cancelled = true
					cancelMu.Unlock()
					return
				}
				if allow != nil && !allow(ix.entries[row].ID) {
					continue
				}
				base := row * dim
				var dot float64
				for i := 0; i < dim; i++ {
					dot += float64(ix.data[base+i]) * float64(query[i])
				}
				local.push(topKCandidate{row: row, score: dot})
			}
			locals[w] = local.items
		}(w, start, end)
	}
	wg.Wait()
	if cancelled || ctx.Err() != nil {
		return nil, ctx.Err()
	}

	total := 0
	for _, items := range locals {
		total += len(items)
	}
	merged := make([]topKCandidate, 0, total)
	for _, items := range locals {
		merged = append(merged, items...)
	}
	sort.Slice(merged, func(a, b int) bool {
		if merged[a].score != merged[b].score {
			return merged[a].score > merged[b].score
		}
		return ix.entries[merged[a].row].ID < ix.entries[merged[b].row].ID
	})
	if topK > len(merged) {
		topK = len(merged)
	}
	hits := make([]Hit, 0, topK)
	for _, cand := range merged[:topK] {
		entry := ix.entries[cand.row]
		hits = append(hits, Hit{ID: entry.ID, ContentHash: entry.ContentHash, Score: cand.score})
	}
	return hits, nil
}

// topKCandidate 是选择阶段的行级候选;ID 经 row 间接取 entries,避免
// 每候选复制字符串。
type topKCandidate struct {
	row   int
	score float64
}

// topKSelector 维护"目前最优的 ≤capacity 个"候选:数组式二叉堆,堆顶=
// 集合内 (score desc, ID asc) 全序的末位(最差者),新候选优于堆顶时替换
// 下沉。分数与 ID 构成全序(段内 ID 唯一),无并列歧义。
type topKSelector struct {
	ix       *Index
	capacity int
	items    []topKCandidate
}

// worse 判断 a 是否排序在 b 之后,与归并/全排比较器同式取反。
func (s *topKSelector) worse(a, b topKCandidate) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	return s.ix.entries[a.row].ID > s.ix.entries[b.row].ID
}

func (s *topKSelector) push(cand topKCandidate) {
	if len(s.items) < s.capacity {
		s.items = append(s.items, cand)
		s.up(len(s.items) - 1)
		return
	}
	if s.worse(cand, s.items[0]) {
		return
	}
	s.items[0] = cand
	s.down(0)
}

func (s *topKSelector) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !s.worse(s.items[i], s.items[parent]) {
			return
		}
		s.items[i], s.items[parent] = s.items[parent], s.items[i]
		i = parent
	}
}

func (s *topKSelector) down(i int) {
	n := len(s.items)
	for {
		worst := i
		if left := 2*i + 1; left < n && s.worse(s.items[left], s.items[worst]) {
			worst = left
		}
		if right := 2*i + 2; right < n && s.worse(s.items[right], s.items[worst]) {
			worst = right
		}
		if worst == i {
			return
		}
		s.items[i], s.items[worst] = s.items[worst], s.items[i]
		i = worst
	}
}
