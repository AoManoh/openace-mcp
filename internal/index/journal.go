package index

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// Journal 是 per-profile 的 embedding 断点续嵌暂存区（阶段计划 D4）：
// 每批嵌入成功即 append 落盘，构建被取消/超时/kill 后已付费向量不丢失；
// revision 发布后清除已入盘条目。它不是长期缓存——向量事实源始终是
// revision（暗坑 K41），journal 只在"已付费未发布"窗口存在。
//
// 规模政策（2026-08-26 用户裁决）：journal 里每一条都是已付费向量，
// 默认没有条数/字节上限，重开时永不压实丢弃——丢弃等于下轮重新付费。
// 资源受限环境经 LimitBytes 注入 opt-in 字节预算（引擎从
// OPENACE_VECTOR_MEMORY_BUDGET 派生），预算语义是"付费前拦截新增"，
// 永不作用于既有条目；磁盘/重开内存代价见 A&Q 文档。
//
// vectors.journal 记录格式（小端）：
//
//	uint32 payloadLen | payload | uint32 crc32(payload)
//	payload = uint16 hashLen | hash | uint32 dim | dim × float32
//
// 尾部半写记录在打开时截断（暗坑 K40）。文件只存 hash 与向量，
// 不存 chunk 明文（暗坑 K54）。
type Journal struct {
	mu        sync.Mutex
	dir       string
	dimension int
	file      *os.File
	entries   map[string][]float32
	rejected  map[string]bool
	bytes     int64
	// maxBytes 是 opt-in 字节预算(0=不限,默认)。历史上这里是一对写死的
	// 400K 条/2GiB 上限(K40 时代随 envelope 上调),超限重开时压实丢最旧
	// ——那会静默丢弃已付费向量。2026-08-26 裁决后默认无上限,预算改由
	// 引擎按用户配置注入,且只拦截新增付费,见 LimitBytes。
	maxBytes int64
}

const (
	journalDirName   = "journal"
	journalFileName  = "vectors.journal"
	rejectedFileName = "rejected.list"
	// rejectedMaxEntries 限制拒绝集规模（病理内容计数级别）。
	rejectedMaxEntries = 10_000
)

// JournalDir 返回 store 的 journal 目录。
func (s *Store) JournalDir() string {
	return filepath.Join(s.root, journalDirName)
}

// OpenJournal 打开（必要时创建）store 的 journal，载入既有条目与拒绝集。
// 维度不符的历史条目整体丢弃（profile 子树隔离下不应出现，防御路径）。
func OpenJournal(store *Store, dimension int) (*Journal, error) {
	if dimension <= 0 {
		return nil, errors.New("journal 需要正维度")
	}
	dir := store.JournalDir()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, dimension: dimension, entries: map[string][]float32{}, rejected: map[string]bool{}}
	if err := j.loadVectors(); err != nil {
		return nil, err
	}
	if err := j.loadRejected(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(j.vectorsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, err
	}
	j.file = file
	return j, nil
}

// Dir 暴露 journal 所在目录:离线批车道(T8)的作业状态文件与 journal
// 同目录同生命周期(同一 workspace 子树,发布后一并清理语义由各自负责)。
func (j *Journal) Dir() string { return j.dir }

func (j *Journal) vectorsPath() string  { return filepath.Join(j.dir, journalFileName) }
func (j *Journal) rejectedPath() string { return filepath.Join(j.dir, rejectedFileName) }

// loadVectors 顺序读取记录，坏尾截断（K40）。已付费条目无论规模全部
// 保留（2026-08-26 裁决）——历史上这里有超上限压实（含 M8 修过的字节环
// 缺陷），因其本质是静默丢弃已付费向量而整体移除。
func (j *Journal) loadVectors() error {
	file, err := os.Open(j.vectorsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	var goodOffset int64
	for {
		hash, vector, recordLen, err := readJournalRecord(reader, j.dimension)
		if err != nil {
			break
		}
		j.entries[hash] = vector
		goodOffset += recordLen
	}
	info, err := os.Stat(j.vectorsPath())
	if err != nil {
		return err
	}
	if info.Size() > goodOffset {
		// 坏尾/维度不符残留：截断到最后一条完整记录。
		if err := os.Truncate(j.vectorsPath(), goodOffset); err != nil {
			return err
		}
	}
	j.bytes = goodOffset
	return nil
}

// readJournalRecord 读取单条记录；任何不一致返回错误（调用方截断）。
func readJournalRecord(reader *bufio.Reader, wantDim int) (string, []float32, int64, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
		return "", nil, 0, err
	}
	payloadLen := binary.LittleEndian.Uint32(lenBuf[:])
	if payloadLen == 0 || payloadLen > 1<<20 {
		return "", nil, 0, errors.New("记录长度非法")
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", nil, 0, err
	}
	var crcBuf [4]byte
	if _, err := io.ReadFull(reader, crcBuf[:]); err != nil {
		return "", nil, 0, err
	}
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(crcBuf[:]) {
		return "", nil, 0, errors.New("crc 不匹配")
	}
	if len(payload) < 2 {
		return "", nil, 0, errors.New("payload 过短")
	}
	hashLen := int(binary.LittleEndian.Uint16(payload[:2]))
	rest := payload[2:]
	if len(rest) < hashLen+4 {
		return "", nil, 0, errors.New("hash 越界")
	}
	hash := string(rest[:hashLen])
	rest = rest[hashLen:]
	dim := int(binary.LittleEndian.Uint32(rest[:4]))
	rest = rest[4:]
	if dim != wantDim || len(rest) != dim*4 {
		return "", nil, 0, errors.New("维度不符")
	}
	vector := make([]float32, dim)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(rest[i*4:]))
	}
	return hash, vector, int64(4 + payloadLen + 4), nil
}

func encodeJournalRecord(hash string, vector []float32) []byte {
	payload := make([]byte, 2+len(hash)+4+len(vector)*4)
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(hash)))
	copy(payload[2:], hash)
	offset := 2 + len(hash)
	binary.LittleEndian.PutUint32(payload[offset:], uint32(len(vector)))
	offset += 4
	for _, value := range vector {
		binary.LittleEndian.PutUint32(payload[offset:], math.Float32bits(value))
		offset += 4
	}
	record := make([]byte, 0, 4+len(payload)+4)
	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(payload)))
	record = append(record, scratch[:]...)
	record = append(record, payload...)
	binary.LittleEndian.PutUint32(scratch[:], crc32.ChecksumIEEE(payload))
	record = append(record, scratch[:]...)
	return record
}

// Append 落盘一批已付费向量（批成功即调用，D4）；每批一次 fsync。
func (j *Journal) Append(vectors map[string][]float32) error {
	if len(vectors) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var buf []byte
	for hash, vector := range vectors {
		if len(vector) != j.dimension {
			return fmt.Errorf("journal 维度不符: %d vs %d", len(vector), j.dimension)
		}
		if _, exists := j.entries[hash]; exists {
			continue
		}
		buf = append(buf, encodeJournalRecord(hash, vector)...)
	}
	if len(buf) == 0 {
		return nil
	}
	if j.maxBytes > 0 && j.bytes+int64(len(buf)) > j.maxBytes {
		// 预算拦截发生在写盘与付费链路继续之前:上层收到错误即停止
		// 继续调用 provider,既有已付费条目原样保留(绝不丢弃)。
		return fmt.Errorf("journal 字节预算耗尽: 现有 %d + 本批 %d 超过预算 %d;停止继续嵌入,提高 OPENACE_VECTOR_MEMORY_BUDGET 或等待发布压实后重试", j.bytes, len(buf), j.maxBytes)
	}
	if _, err := j.file.Write(buf); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	for hash, vector := range vectors {
		if _, exists := j.entries[hash]; !exists {
			j.entries[hash] = vector
		}
	}
	j.bytes += int64(len(buf))
	return nil
}

// LimitBytes 设置 journal 字节预算(0=不限,默认不限)。引擎在用户配置
// OPENACE_VECTOR_MEMORY_BUDGET 时注入;预算只做"付费前拦截"(Append 报错
// 停止新付费),永不触发对既有已付费条目的丢弃。
func (j *Journal) LimitBytes(max int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.maxBytes = max
}

// Snapshot 返回当前条目视图（复用源；调用方不得修改向量）。
func (j *Journal) Snapshot() map[string][]float32 {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string][]float32, len(j.entries))
	for hash, vector := range j.entries {
		out[hash] = vector
	}
	return out
}

// CompactAfterPublish 清除已随 revision 入盘的条目（K41：发布即 GC），
// 并压实文件。
func (j *Journal) CompactAfterPublish(published map[string]bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	changed := false
	for hash := range j.entries {
		if published[hash] {
			delete(j.entries, hash)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return j.rewriteLocked()
}

// rewriteLocked 以 temp+rename 原子重写 journal（调用方持锁）。逐条流式
// 写入(1MiB 缓冲):历史实现先在内存拼出完整文件字节,400K×1024 维时
// 单块缓冲可达 ~1.6GiB,低内存环境重写即 OOM 风险(2026-08-26 裁决修复)。
func (j *Journal) rewriteLocked() error {
	if j.file != nil {
		_ = j.file.Close()
		j.file = nil
	}
	path := j.vectorsPath()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-journal-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	writer := bufio.NewWriterSize(tmp, 1<<20)
	var total int64
	for hash, vector := range j.entries {
		record := encodeJournalRecord(hash, vector)
		if _, err := writer.Write(record); err != nil {
			cleanup()
			return err
		}
		total += int64(len(record))
	}
	if err := writer.Flush(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := renameWithRetry(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDirBestEffort(filepath.Dir(path))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return err
	}
	j.file = file
	j.bytes = total
	return nil
}

// Rejected 报告 hash 是否在持久化拒绝集（K35 修订，跨重启生效）。
func (j *Journal) Rejected(hash string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.rejected[hash]
}

// RejectedCount 返回拒绝集规模（状态上报）。
func (j *Journal) RejectedCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.rejected)
}

// MarkRejected 持久化记录零向量/NaN 内容 hash。
func (j *Journal) MarkRejected(hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.OpenFile(j.rejectedPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, hash := range hashes {
		if j.rejected[hash] {
			continue
		}
		if _, err := fmt.Fprintln(file, hash); err != nil {
			return err
		}
		j.rejected[hash] = true
	}
	return file.Sync()
}

func (j *Journal) loadRejected() error {
	file, err := os.Open(j.rejectedPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var order []string
	for scanner.Scan() {
		hash := scanner.Text()
		if hash == "" {
			continue
		}
		if !j.rejected[hash] {
			order = append(order, hash)
		}
		j.rejected[hash] = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(order) > rejectedMaxEntries {
		keep := order[len(order)-rejectedMaxEntries:]
		j.rejected = make(map[string]bool, len(keep))
		var buf []byte
		for _, hash := range keep {
			j.rejected[hash] = true
			buf = append(buf, hash...)
			buf = append(buf, '\n')
		}
		return writeFileAtomic(j.rejectedPath(), buf)
	}
	return nil
}

// Close 关闭 journal 文件句柄。
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}
