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
	"sync"

	"context"
)

const (
	// DataFileName 是向量数据文件名。
	DataFileName = "vectors.dat"
	// IndexFileName 是行映射文件名。
	IndexFileName = "vectors.idx"
	// indexSchemaVersion 随 idx 结构不兼容变化而递增。
	indexSchemaVersion = 1
	// DefaultMaxResidentVectors 是 exact 路径常驻内存的已验证 envelope
	// （§18：超出必须显式降级词法，禁止靠 OOM/超时兜底）。
	// Stage 5 复审定值：250K（2026-07-31,270,373 条真实向量 p50=86ms）→
	// 400K（2026-08-01 用户批准选项 B,kubernetes 全量语料需求）：
	// 线性外推 p50≈128ms(0.32ms/K 实测斜率),常驻 400K×1024×4B≈1.6GB;
	// 定值以 k8s 全量嵌入后 annbench 实测复核(sealed 报告记录),
	// 超出仍显式降级。
	DefaultMaxResidentVectors = 400_000
	// cancelCheckRows 是并行扫描的取消检查粒度。
	cancelCheckRows = 2048
)

// ErrEnvelopeExceeded 表示向量规模超出已验证 envelope，语义路应显式降级。
var ErrEnvelopeExceeded = errors.New("vector index exceeds verified exact-search envelope")

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

// Index 是常驻内存的只读 exact 索引。
type Index struct {
	dimension int
	entries   []Entry
	data      []float32
}

// Load 读取并校验向量文件（暗坑 K24/K25）：checksum、schema、尺寸对齐、
// envelope。任何校验失败返回错误，由调用方决定语义路降级。
func Load(dir string, wantDimension int, wantDataChecksum string, wantIndexChecksum string, maxVectors int) (*Index, error) {
	if maxVectors <= 0 {
		maxVectors = DefaultMaxResidentVectors
	}
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
	if header.Count > maxVectors {
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
	// 流式校验+解码:旧实现同时常驻完整 dataBytes 与 float32 data,
	// 大仓单段瞬时约翻倍。现在仅保留最终 float32 与64KiB缓冲。
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
	scores := make([]float64, count)

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers > count {
		workers = 1
	}
	chunkRows := (count + workers - 1) / workers
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
		go func(start, end int) {
			defer wg.Done()
			dim := ix.dimension
			for row := start; row < end; row++ {
				if (row-start)%cancelCheckRows == 0 && ctx.Err() != nil {
					cancelMu.Lock()
					cancelled = true
					cancelMu.Unlock()
					return
				}
				base := row * dim
				var dot float64
				for i := 0; i < dim; i++ {
					dot += float64(ix.data[base+i]) * float64(query[i])
				}
				scores[row] = dot
			}
		}(start, end)
	}
	wg.Wait()
	if cancelled || ctx.Err() != nil {
		return nil, ctx.Err()
	}

	order := make([]int, count)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ra, rb := order[a], order[b]
		if scores[ra] != scores[rb] {
			return scores[ra] > scores[rb]
		}
		return ix.entries[ra].ID < ix.entries[rb].ID
	})
	if topK > count {
		topK = count
	}
	hits := make([]Hit, 0, topK)
	for _, row := range order[:topK] {
		hits = append(hits, Hit{ID: ix.entries[row].ID, ContentHash: ix.entries[row].ContentHash, Score: scores[row]})
	}
	return hits, nil
}
