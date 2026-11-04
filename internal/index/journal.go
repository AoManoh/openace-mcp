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
	"sort"
	"strings"
	"sync"
)

// Journal 是 per-profile 的 embedding 断点续嵌暂存区（阶段计划 D4）：
// 每批嵌入成功即 append 落盘，构建被取消/超时/kill 后已付费向量不丢失；
// revision 发布后清除已入盘条目。它不是长期缓存——向量事实源始终是
// revision（暗坑 K41），journal 只在"已付费未发布"窗口存在。
//
// 规模政策（2026-08-26 用户裁决）：journal 里每一条都是已付费向量，
// 没有条数/字节上限，重开时永不压实丢弃——丢弃等于下轮重新付费；
// 发布后由 CompactAfterPublish 清理已进入 revision 的条目。
//
// vectors.journal 记录格式（小端）：
//
//	uint32 payloadLen | payload | uint32 crc32(payload)
//	payload = uint16 hashLen | hash | uint32 dim | dim × float32
//
// 尾部半写记录在打开时截断（暗坑 K40）。文件只存 hash 与向量，
// 不存 chunk 明文（暗坑 K54）。
//
// 内存里只有键到记录起始偏移的表，向量只在文件里，用 ReadInto 按键读回。
// 全部向量新取的全量构建若把新向量留在堆上，构建期间会一直占着（mailyn-cli
// 一半副本约 700 MiB）；写分段时按偏移逐行读回，堆上只剩偏移表。
type Journal struct {
	mu        sync.Mutex
	dir       string
	dimension int
	// file 是追加写句柄，reader 是按偏移读回的只读句柄。发布后清理改写文件时两者
	// 都先关闭（Windows 上不能改名覆盖仍打开的文件），改名后重新打开。
	file   *os.File
	reader *os.File
	// offsets 是键到记录起始偏移的表；bytes 是文件当前长度，也就是下一条记录的偏移。
	offsets  map[string]int64
	rejected map[string]bool
	bytes    int64
	// scratch 是读回单条记录的复用缓冲，持锁使用。
	scratch []byte
	// stale 表示偏移表删掉了文件里仍在的记录（读回时校验失败），下一次发布后清理
	// 即使没有已发布的键也要改写文件，把坏记录去掉；否则进程崩溃后重新打开时会在
	// 这条坏记录处截断，丢掉它后面的已付费记录。
	stale bool
	// broken 非空时拒绝追加：一批写入失败后没能把文件截回写入前的长度，文件末尾
	// 可能有半条记录，之后追加的记录偏移不可信。重新打开 journal 时按坏尾截断恢复。
	broken error
}

// journalWrite 把一批编码好的记录写进追加句柄。测试替换它来模拟只写了一部分就
// 失败的情形。
var journalWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// journalRename 是发布后清理用的改名，测试替换它来模拟改名失败。
var journalRename = renameWithRetry

const (
	journalDirName   = "journal"
	journalFileName  = "vectors.journal"
	rejectedFileName = "rejected.list"
	// rejectedMaxEntries 限制拒绝集规模（病理内容计数级别）。
	rejectedMaxEntries = 10_000
)

// JournalDir 返回 store 的 journal 目录。
func (s *Store) JournalDir() string {
	return JournalDirOf(s.root)
}

// OpenJournal 打开（必要时创建）store 的 journal，载入既有条目的偏移与拒绝集。
// 维度不符的历史条目整体丢弃（profile 子树隔离下不应出现，防御路径）。
func OpenJournal(store *Store, dimension int) (*Journal, error) {
	if dimension <= 0 {
		return nil, errors.New("journal 需要正维度")
	}
	dir := store.JournalDir()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, dimension: dimension, offsets: map[string]int64{}, rejected: map[string]bool{}}
	if err := j.loadOffsets(); err != nil {
		return nil, err
	}
	if err := j.loadRejected(); err != nil {
		return nil, err
	}
	if err := j.openHandles(); err != nil {
		return nil, err
	}
	return j, nil
}

// Dir 暴露 journal 所在目录:离线批车道(T8)的作业状态文件与 journal
// 同目录同生命周期(同一 workspace 子树,发布后一并清理语义由各自负责)。
func (j *Journal) Dir() string { return j.dir }

func (j *Journal) vectorsPath() string  { return filepath.Join(j.dir, journalFileName) }
func (j *Journal) rejectedPath() string { return filepath.Join(j.dir, rejectedFileName) }

// recordLen 是键长为 hashLen 的一条记录在文件里的总字节数：长度字段、键长、键、
// 维度、向量与 CRC。
func (j *Journal) recordLen(hashLen int) int {
	return 4 + 2 + hashLen + 4 + 4*j.dimension + 4
}

// openHandles 打开追加写句柄与读句柄（调用方持锁或尚未发布 j）。
func (j *Journal) openHandles() error {
	file, err := os.OpenFile(j.vectorsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return err
	}
	reader, err := os.Open(j.vectorsPath())
	if err != nil {
		_ = file.Close()
		return err
	}
	j.file, j.reader = file, reader
	return nil
}

// closeHandles 关闭两个句柄（调用方持锁）。
func (j *Journal) closeHandles() error {
	var err error
	if j.file != nil {
		err = j.file.Close()
		j.file = nil
	}
	if j.reader != nil {
		if cerr := j.reader.Close(); err == nil {
			err = cerr
		}
		j.reader = nil
	}
	return err
}

// loadOffsets 顺序读取记录并校验 CRC 与维度，只记下每个键的起始偏移，坏尾截断
// （K40）。已付费条目无论规模全部保留（2026-08-26 裁决）——历史上这里有超上限
// 压实（含 M8 修过的字节环缺陷），因其本质是静默丢弃已付费向量而整体移除。同一
// 个键出现多次时以最后一条为准，与此前把向量逐条写进表的结果一致。
func (j *Journal) loadOffsets() error {
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
	var buf []byte
	for {
		var hash string
		var recordLen int64
		hash, _, recordLen, buf, err = readJournalPayload(reader, j.dimension, buf)
		if err != nil {
			break
		}
		j.offsets[hash] = goodOffset
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

// readJournalPayload 顺序读出一条完整记录并校验长度、CRC、键长与维度；任何不
// 一致返回错误（打开时由调用方截断，只读快照按 ErrJournalCorrupt 报告）。buf 是
// 可复用的缓冲，容量不够时重新分配并经返回值交回；vecBytes 指向 buf 里的向量
// 部分，下一次调用前有效。
func readJournalPayload(reader *bufio.Reader, wantDim int, buf []byte) (hash string, vecBytes []byte, recordLen int64, out []byte, err error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
		return "", nil, 0, buf, err
	}
	payloadLen := int(binary.LittleEndian.Uint32(lenBuf[:]))
	if payloadLen == 0 || payloadLen > 1<<20 {
		return "", nil, 0, buf, errors.New("记录长度非法")
	}
	if cap(buf) < payloadLen+4 {
		buf = make([]byte, payloadLen+4)
	}
	data := buf[:payloadLen+4]
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", nil, 0, buf, err
	}
	payload := data[:payloadLen]
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(data[payloadLen:]) {
		return "", nil, 0, buf, errors.New("crc 不匹配")
	}
	if len(payload) < 2 {
		return "", nil, 0, buf, errors.New("payload 过短")
	}
	hashLen := int(binary.LittleEndian.Uint16(payload[:2]))
	rest := payload[2:]
	if len(rest) < hashLen+4 {
		return "", nil, 0, buf, errors.New("hash 越界")
	}
	hash = string(rest[:hashLen])
	rest = rest[hashLen:]
	dim := int(binary.LittleEndian.Uint32(rest[:4]))
	rest = rest[4:]
	if dim != wantDim || len(rest) != dim*4 {
		return "", nil, 0, buf, errors.New("维度不符")
	}
	return hash, rest, int64(4 + payloadLen + 4), buf, nil
}

// decodeJournalVector 把小端 float32 字节解码进 dst（长度为维度）。
func decodeJournalVector(vecBytes []byte, dst []float32) {
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(vecBytes[i*4:]))
	}
}

// checkJournalRecord 校验按偏移读回的一条完整记录（含长度字段与 CRC）是否就是
// key 的记录，通过时返回空串，否则返回问题描述。
func checkJournalRecord(record []byte, key string, dim int) string {
	payloadLen := int(binary.LittleEndian.Uint32(record[:4]))
	if payloadLen != len(record)-8 {
		return "长度字段不符"
	}
	payload := record[4 : 4+payloadLen]
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(record[4+payloadLen:]) {
		return "CRC 不符"
	}
	if hashLen := int(binary.LittleEndian.Uint16(payload[:2])); hashLen != len(key) || string(payload[2:2+hashLen]) != key {
		return "键不符"
	}
	rest := payload[2+len(key):]
	if int(binary.LittleEndian.Uint32(rest[:4])) != dim {
		return "维度不符"
	}
	return ""
}

// readRecordLocked 按偏移读回 key 的整条记录并校验，返回指向 j.scratch 的切片，
// 下一次读回前有效（调用方持锁）。记录不完整或校验失败时返回包装
// ErrJournalCorrupt 的错误；读文件本身失败时原样包装返回。
func (j *Journal) readRecordLocked(key string, offset int64) ([]byte, error) {
	if j.reader == nil {
		return nil, fmt.Errorf("journal %s 的读句柄没有打开（已关闭，或发布后清理失败后没能重新打开）", j.vectorsPath())
	}
	size := j.recordLen(len(key))
	if cap(j.scratch) < size {
		j.scratch = make([]byte, size)
	}
	record := j.scratch[:size]
	if _, err := j.reader.ReadAt(record, offset); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: %s 偏移 %d 处键 %s 的记录不完整", ErrJournalCorrupt, j.vectorsPath(), offset, key)
		}
		return nil, fmt.Errorf("读取 %s（偏移 %d，键 %s）: %w", j.vectorsPath(), offset, key, err)
	}
	if problem := checkJournalRecord(record, key, j.dimension); problem != "" {
		return nil, fmt.Errorf("%w: %s 偏移 %d 处键 %s 的记录%s", ErrJournalCorrupt, j.vectorsPath(), offset, key, problem)
	}
	return record, nil
}

// Has 报告 journal 里有没有 key 的已付费向量。
func (j *Journal) Has(key string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.offsets[key]
	return ok
}

// Len 返回 journal 里的条目数。
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.offsets)
}

// ReadInto 把 key 的向量从文件读进 dst（长度必须等于维度）。记录不完整或校验
// 失败时返回包装 ErrJournalCorrupt 的错误，错误里写明文件、偏移与键，并把该键移
// 出偏移表：这条向量已经读不出来，留在表里每次构建都会在同一处失败；移出后下一
// 次构建会为它重新送审，原因由这个错误交给调用方报告。
func (j *Journal) ReadInto(key string, dst []float32) error {
	if len(dst) != j.dimension {
		return fmt.Errorf("journal 读回缓冲维度 %d 与配置 %d 不符", len(dst), j.dimension)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	offset, ok := j.offsets[key]
	if !ok {
		return fmt.Errorf("journal %s 没有键 %s", j.vectorsPath(), key)
	}
	record, err := j.readRecordLocked(key, offset)
	if err != nil {
		if errors.Is(err, ErrJournalCorrupt) {
			delete(j.offsets, key)
			j.stale = true
		}
		return err
	}
	decodeJournalVector(record[4+2+len(key)+4:], dst)
	return nil
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
//   - 写入失败（例如磁盘满）时可能只写了一部分：把文件截回写入前的长度再返回
//     错误，本批不计入。不截回的话半条记录留在文件中间，之后追加的记录在下次
//     打开时会随它一起被截掉（K40 遇到坏记录即截断其后内容），同进程里按偏移
//     读回也会错位。截回也失败时停止追加（broken），已有条目照常可读。
//   - 写入成功即记入偏移表，之后 fsync 失败只返回错误：数据已在文件里，同一进
//     程之后的构建直接复用，不为它再付费；本次构建由调用方按落盘失败处理。
func (j *Journal) Append(vectors map[string][]float32) error {
	if len(vectors) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.broken != nil {
		return fmt.Errorf("journal %s 已停止追加: %w", j.vectorsPath(), j.broken)
	}
	if j.file == nil {
		return fmt.Errorf("journal %s 的追加句柄没有打开（已关闭，或发布后清理失败后没能重新打开）", j.vectorsPath())
	}
	type placed struct {
		hash   string
		offset int64
	}
	var buf []byte
	var added []placed
	for hash, vector := range vectors {
		if len(vector) != j.dimension {
			return fmt.Errorf("journal 维度不符: %d vs %d", len(vector), j.dimension)
		}
		if _, exists := j.offsets[hash]; exists {
			continue
		}
		added = append(added, placed{hash: hash, offset: j.bytes + int64(len(buf))})
		buf = append(buf, encodeJournalRecord(hash, vector)...)
	}
	if len(buf) == 0 {
		return nil
	}
	if _, err := journalWrite(j.file, buf); err != nil {
		if terr := j.truncateLocked(j.bytes); terr != nil {
			j.broken = fmt.Errorf("写入失败（%v）后截回 %d 字节也失败（%v），文件末尾可能有半条记录，重新打开 journal 时会截掉", err, j.bytes, terr)
			return j.broken
		}
		return fmt.Errorf("journal %s 写入失败，已截回写入前的 %d 字节: %w", j.vectorsPath(), j.bytes, err)
	}
	for _, entry := range added {
		j.offsets[entry.hash] = entry.offset
	}
	j.bytes += int64(len(buf))
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("journal %s fsync 失败（本批已写进文件并可复用）: %w", j.vectorsPath(), err)
	}
	return nil
}

// truncateLocked 把文件截到 size 字节。追加句柄以 O_APPEND 打开，在 Windows 上
// 这类句柄没有截断权限，先试句柄、不行再按路径截断。
func (j *Journal) truncateLocked(size int64) error {
	if err := j.file.Truncate(size); err == nil {
		return nil
	}
	return os.Truncate(j.vectorsPath(), size)
}

// Snapshot 从文件读出全部条目（键到向量）。内存与条目数成正比，构建路径不调用
// 它，只给测试与人工核对用；读回校验失败的条目不在结果里，并按 ReadInto 的规则
// 移出偏移表。
func (j *Journal) Snapshot() map[string][]float32 {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string][]float32, len(j.offsets))
	for key, offset := range j.offsets {
		record, err := j.readRecordLocked(key, offset)
		if err != nil {
			if errors.Is(err, ErrJournalCorrupt) {
				delete(j.offsets, key)
				j.stale = true
			}
			continue
		}
		vector := make([]float32, j.dimension)
		decodeJournalVector(record[4+2+len(key)+4:], vector)
		out[key] = vector
	}
	return out
}

// CompactAfterPublish 清除已随 revision 入盘的条目（K41：发布即 GC），
// 并压实文件。读回时校验失败、已移出偏移表的记录也在这时从文件里去掉。
// 返回包装 ErrJournalCorrupt 的错误时压实已经完成，只是复制时又发现了校验失败
// 的记录，它们没有写进新文件，错误里有条数。
func (j *Journal) CompactAfterPublish(published map[string]bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	changed := j.stale
	for hash := range j.offsets {
		if published[hash] {
			delete(j.offsets, hash)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	// 移出偏移表的记录还在文件里，改写成功前一直按 stale 处理：这次改写失败时，
	// 下一次清理即使没有新的已发布键也会重试。
	j.stale = true
	return j.rewriteLocked()
}

// rewriteLocked 以 temp+rename 原子重写 journal（调用方持锁）：偏移表里的记录按
// 偏移顺序逐条从原文件读出、校验后写进临时文件（1 MiB 缓冲，不在内存里拼整个
// 文件），fsync 后改名覆盖原文件，再重新打开两个句柄并换上新偏移。
//   - 复制完成前失败：删掉临时文件，原文件与句柄都不动。
//   - 改名失败：原文件不动，句柄重新打开到原文件，偏移表仍与原文件一致。此前的
//     实现改名失败后追加句柄保持关闭，之后每次追加都失败，直到进程重启。
//   - 复制时校验失败的记录不写出，从偏移表移除并计数返回。
func (j *Journal) rewriteLocked() error {
	path := j.vectorsPath()
	keys := make([]string, 0, len(j.offsets))
	for key := range j.offsets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool { return j.offsets[keys[a]] < j.offsets[keys[b]] })
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
	newOffsets := make(map[string]int64, len(keys))
	var total int64
	damaged := 0
	for _, key := range keys {
		record, err := j.readRecordLocked(key, j.offsets[key])
		if errors.Is(err, ErrJournalCorrupt) {
			damaged++
			continue
		}
		if err != nil {
			cleanup()
			return err
		}
		if _, err := writer.Write(record); err != nil {
			cleanup()
			return err
		}
		newOffsets[key] = total
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
	_ = j.closeHandles()
	if err := journalRename(tmpName, path); err != nil {
		os.Remove(tmpName)
		if reopenErr := j.openHandles(); reopenErr != nil {
			j.broken = fmt.Errorf("发布后清理改名失败（%v）后重新打开 journal 也失败（%v）", err, reopenErr)
			return j.broken
		}
		return fmt.Errorf("journal %s 发布后清理改名失败，原文件未改动: %w", path, err)
	}
	syncDirBestEffort(filepath.Dir(path))
	j.offsets = newOffsets
	j.bytes = total
	j.stale = false
	if err := j.openHandles(); err != nil {
		j.broken = fmt.Errorf("发布后清理完成但重新打开 journal 失败（%v）", err)
		return j.broken
	}
	if damaged > 0 {
		return fmt.Errorf("%w: 发布后清理 %s 时有 %d 条记录校验失败，没有写进新文件，对应内容下次构建会重新送审", ErrJournalCorrupt, path, damaged)
	}
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

// JournalDirOf 返回索引子树根 root 下的 journal 目录路径，不检查也不创建。
// 引擎用它在不打开 Store 的情况下判断旧子树有没有 journal。
func JournalDirOf(root string) string {
	return filepath.Join(root, journalDirName)
}

// ErrJournalCorrupt 表示只读快照在 journal 目录里遇到不完整、长度非法、校验
// 失败或维度不符的记录，或拒绝集末行没写完。这类文件里至少有一部分已付费
// 记录读不出来，调用方不能把读出的部分当作完整状态。
var ErrJournalCorrupt = errors.New("journal 记录不完整或校验失败")

// ReadJournalSnapshot 以只读方式读出 journal 目录 dir 里的已付费向量与拒绝集，
// 供引擎在换用新索引子树时复用旧子树的已付费状态。它不创建目录或文件、
// 不截断、不改写、不保留文件句柄，也不需要该子树的写锁，旧文件保持原样。
// 只有在记录边界处干净结束才算读完：不完整的记录、长度非法、CRC 不符、
// 维度不符与拒绝集末行不完整都以 ErrJournalCorrupt 返回，错误里带文件路径、
// 记录序号与偏移；打开或读取时的 I/O 错误原样包装返回（不是
// ErrJournalCorrupt）。出错时不返回部分结果。dir 或其中的文件不存在时返回
// 空结果。拒绝集与 loadRejected 一样只保留最近的 rejectedMaxEntries 条。
func ReadJournalSnapshot(dir string, dimension int) (map[string][]float32, map[string]bool, error) {
	vectors := map[string][]float32{}
	rejected, err := ScanJournalSnapshot(dir, dimension, nil, func(key string, vec []float32) error {
		vectors[key] = append([]float32(nil), vec...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return vectors, rejected, nil
}

// ScanJournalSnapshot 与 ReadJournalSnapshot 的只读方式与错误约定相同，但不把向量
// 收进返回值：按文件顺序每读到一条键在 want 里的记录（want 为 nil 时是每一条）
// 就调用 fn(key, vec)，vec 是复用缓冲，fn 返回后失效；读完向量文件再读拒绝集并
// 返回。换用新索引子树时，引擎用它把旧子树 journal 里需要的已付费向量逐批转存进
// 新子树的 journal，不必把整个旧 journal 读进内存。fn 返回错误时停止并原样返回；
// 出错时 fn 可能已经收到错误位置之前的记录，调用方不能把收到的部分当作完整状态。
func ScanJournalSnapshot(dir string, dimension int, want map[string]bool, fn func(key string, vec []float32) error) (map[string]bool, error) {
	if dimension <= 0 {
		return nil, errors.New("journal 需要正维度")
	}
	if err := scanVectorsSnapshot(filepath.Join(dir, journalFileName), dimension, want, fn); err != nil {
		return nil, err
	}
	return readRejectedSnapshot(filepath.Join(dir, rejectedFileName))
}

// scanVectorsSnapshot 逐条读取向量文件，见 ScanJournalSnapshot 的约定。
func scanVectorsSnapshot(path string, dimension int, want map[string]bool, fn func(key string, vec []float32) error) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("打开 %s: %w", path, err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	var offset int64
	var buf []byte
	vector := make([]float32, dimension)
	for record := 1; ; record++ {
		if _, err := reader.Peek(1); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("读取 %s（偏移 %d）: %w", path, offset, err)
		}
		var hash string
		var vecBytes []byte
		var size int64
		hash, vecBytes, size, buf, err = readJournalPayload(reader, dimension, buf)
		if err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				return fmt.Errorf("读取 %s（第 %d 条记录，偏移 %d）: %w", path, record, offset, err)
			}
			return fmt.Errorf("%w: %s 第 %d 条记录（偏移 %d）: %v", ErrJournalCorrupt, path, record, offset, err)
		}
		if want == nil || want[hash] {
			decodeJournalVector(vecBytes, vector)
			if err := fn(hash, vector); err != nil {
				return err
			}
		}
		offset += size
	}
}

// readRejectedSnapshot 只读解析拒绝集文件：按首次出现的顺序去重，超过
// rejectedMaxEntries 时保留最近的条目，与 loadRejected 的保留结果一致，但
// 不改写文件。MarkRejected 每条都以换行结尾，文件非空却不以换行结尾说明
// 最后一条没写完，按 ErrJournalCorrupt 返回。
func readRejectedSnapshot(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("%w: %s 末行不完整（文件不以换行结尾）", ErrJournalCorrupt, path)
	}
	seen := map[string]bool{}
	var order []string
	for _, hash := range strings.Split(string(data), "\n") {
		if hash == "" || seen[hash] {
			continue
		}
		seen[hash] = true
		order = append(order, hash)
	}
	if len(order) > rejectedMaxEntries {
		order = order[len(order)-rejectedMaxEntries:]
	}
	rejected := make(map[string]bool, len(order))
	for _, hash := range order {
		rejected[hash] = true
	}
	return rejected, nil
}

// Close 关闭 journal 的追加与读句柄。
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.closeHandles()
}
