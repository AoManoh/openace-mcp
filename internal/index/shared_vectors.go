package index

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SharedVectors 是按 embedding 身份共享的向量库：同一仓库的多个 worktree 或
// 副本是不同的工作区，各自的索引子树互不复用；而 embedKey 由模板版本、相对
// 路径、符号、语言与内容 hash 组成，已覆盖送审文本的全部组成部分，相对路径
// 与内容相同的 chunk 在不同工作区里键相同、向量相同。构建在送审前查这里，
// 命中即复用，不再付费。
//
// 存放与并发：目录下每个写入进程一个追加式段文件（vectors-<pid>-<时刻>-<随机>.log），
// 记录格式与 journal 相同（长度、键、维度、向量、CRC32）。每个段文件只有一个
// 写入者，进程之间不需要锁；读者扫描全部段文件，遇到末尾写到一半的记录
// 就停在它之前，下次再从那里继续。CRC 不符的记录跳过并计数。
//
// 生命周期：这里的每条记录都是已付费向量，不自动删除；整个目录可以随时由
// 用户手动删除，各工作区已发布的索引各自保存向量副本，不受影响。
type SharedVectors struct {
	dir       string
	dimension int

	mu sync.Mutex
	// index 是键到记录位置的映射，scanned 是每个段文件已扫描到的完整记录
	// 末尾位置。
	index   map[string]sharedLocation
	scanned map[string]int64
	// writer 是本进程的段文件，第一次追加时创建。
	writer     *os.File
	writerName string
	writerSize int64
	// skipped 是读取时校验失败而跳过的记录数。
	skipped int
}

type sharedLocation struct {
	file   string
	offset int64
	length int
}

const (
	sharedVectorsDirName = "shared-vectors"
	sharedSegmentPrefix  = "vectors-"
	sharedSegmentSuffix  = ".log"
)

// SharedVectorsDir 返回某个 embedding 身份的共享向量库目录：
// <cacheDir>/<namespace>/shared-vectors/<identity>。它在各工作区的索引子树
// （<cacheDir>/<namespace>/engines/...）之外。
func SharedVectorsDir(cacheDir, namespace, identity string) string {
	return filepath.Join(cacheDir, namespace, sharedVectorsDirName, identity)
}

// OpenSharedVectors 打开（必要时创建）共享向量库目录。段文件在第一次查询时
// 才扫描，在第一次追加时才创建。
func OpenSharedVectors(dir string, dimension int) (*SharedVectors, error) {
	if dimension <= 0 {
		return nil, errors.New("共享向量库需要正维度")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	return &SharedVectors{dir: dir, dimension: dimension, index: map[string]sharedLocation{}, scanned: map[string]int64{}}, nil
}

// Dir 返回共享向量库目录。
func (s *SharedVectors) Dir() string { return s.dir }

// Lookup 返回 keys 中在共享库里有合法记录的向量。读出的记录再校验一次 CRC、
// 维度、有限值与单位长度（构建只写入归一化后的向量）；不合格的记录跳过并
// 计数，对应内容照常送审。
func (s *SharedVectors) Lookup(keys []string) (map[string][]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resetIfRemovedLocked(); err != nil {
		return nil, err
	}
	if err := s.refreshLocked(); err != nil {
		return nil, err
	}
	out := make(map[string][]float32)
	files := map[string]*os.File{}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	for _, key := range keys {
		loc, ok := s.index[key]
		if !ok {
			continue
		}
		file, ok := files[loc.file]
		if !ok {
			opened, err := os.Open(filepath.Join(s.dir, loc.file))
			if errors.Is(err, os.ErrNotExist) {
				// 段文件在扫描之后被用户删除：这些键当作没有记录，照常送审。
				s.forgetSegmentLocked(loc.file)
				continue
			}
			if err != nil {
				return nil, err
			}
			files[loc.file], file = opened, opened
		}
		record := make([]byte, loc.length)
		if _, err := file.ReadAt(record, loc.offset); err != nil {
			return nil, err
		}
		gotKey, vec, ok := decodeSharedRecord(record, s.dimension)
		if !ok || gotKey != key || !unitVector(vec) {
			s.skipped++
			delete(s.index, key)
			continue
		}
		out[key] = vec
	}
	return out, nil
}

// Append 把一批已付费、已归一化的向量追加到本进程的段文件，写入后 fsync；
// 共享库里已有的键不重复写。
func (s *SharedVectors) Append(vectors map[string][]float32) error {
	if len(vectors) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resetIfRemovedLocked(); err != nil {
		return err
	}
	type pending struct {
		key            string
		offset, length int
	}
	var buf []byte
	var added []pending
	for key, vec := range vectors {
		if len(vec) != s.dimension {
			return fmt.Errorf("共享向量维度不符: %d vs %d", len(vec), s.dimension)
		}
		if _, exists := s.index[key]; exists {
			continue
		}
		record := encodeJournalRecord(key, vec)
		added = append(added, pending{key: key, offset: len(buf), length: len(record)})
		buf = append(buf, record...)
	}
	if len(buf) == 0 {
		return nil
	}
	if s.writer == nil {
		if err := s.createWriterLocked(); err != nil {
			return err
		}
	}
	if _, err := s.writer.Write(buf); err != nil {
		return err
	}
	if err := s.writer.Sync(); err != nil {
		return err
	}
	for _, entry := range added {
		s.index[entry.key] = sharedLocation{file: s.writerName, offset: s.writerSize + int64(entry.offset), length: entry.length}
	}
	s.writerSize += int64(len(buf))
	s.scanned[s.writerName] = s.writerSize
	return nil
}

// Skipped 返回读取时校验失败而跳过的记录数。
func (s *SharedVectors) Skipped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped
}

// Bytes 返回目录下全部段文件的总字节数。
func (s *SharedVectors) Bytes() (int64, error) {
	return SharedVectorsSize(s.dir)
}

// SharedVectorsSize 返回共享向量库目录下全部段文件的总字节数；目录不存在
// 时返回 0。
func SharedVectorsSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if !isSharedSegment(entry) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

// Close 关闭本进程的段文件。
func (s *SharedVectors) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return nil
	}
	err := s.writer.Close()
	s.writer = nil
	return err
}

func isSharedSegment(entry os.DirEntry) bool {
	name := entry.Name()
	return entry.Type().IsRegular() && strings.HasPrefix(name, sharedSegmentPrefix) && strings.HasSuffix(name, sharedSegmentSuffix)
}

// createWriterLocked 创建本进程的段文件。文件名含进程号、纳秒时刻与随机串，
// 以 O_EXCL 创建，不会与其他进程的段文件同名。
func (s *SharedVectors) createWriterLocked() error {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := fmt.Sprintf("%s%d-%d-%s%s", sharedSegmentPrefix, os.Getpid(), time.Now().UnixNano(), hex.EncodeToString(random[:]), sharedSegmentSuffix)
	file, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return err
	}
	syncDirBestEffort(s.dir)
	s.writer, s.writerName, s.writerSize = file, name, 0
	s.scanned[name] = 0
	return nil
}

// resetIfRemovedLocked 处理用户手动删除共享库的情况（允许随时整目录删除）：
// 目录不在了就清空内存里的索引、关闭本进程的段文件并重建目录；只是本进程
// 的段文件被删除时关闭它，下次追加新建一个。不处理的话，长驻 daemon 会继续
// 往已删除的文件里写，这些向量谁也读不到。
func (s *SharedVectors) resetIfRemovedLocked() error {
	if _, err := os.Stat(s.dir); errors.Is(err, os.ErrNotExist) {
		if s.writer != nil {
			_ = s.writer.Close()
			s.writer = nil
		}
		s.index = map[string]sharedLocation{}
		s.scanned = map[string]int64{}
		return os.MkdirAll(s.dir, dirPerm)
	} else if err != nil {
		return err
	}
	if s.writer != nil {
		if _, err := os.Stat(filepath.Join(s.dir, s.writerName)); errors.Is(err, os.ErrNotExist) {
			_ = s.writer.Close()
			s.writer = nil
			s.forgetSegmentLocked(s.writerName)
		}
	}
	return nil
}

// forgetSegmentLocked 删除指向某个段文件的全部索引项。
func (s *SharedVectors) forgetSegmentLocked(name string) {
	for key, loc := range s.index {
		if loc.file == name {
			delete(s.index, key)
		}
	}
	delete(s.scanned, name)
}

// refreshLocked 扫描各段文件上次扫描位置之后新增的完整记录；已扫描过、
// 现在不在的段文件，其索引项一并删除。
func (s *SharedVectors) refreshLocked() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if isSharedSegment(entry) {
			present[entry.Name()] = true
		}
	}
	for name := range s.scanned {
		if !present[name] && name != s.writerName {
			s.forgetSegmentLocked(name)
		}
	}
	for _, entry := range entries {
		if !isSharedSegment(entry) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name := entry.Name()
		if info.Size() <= s.scanned[name] {
			continue
		}
		end, err := s.scanSegmentLocked(name, s.scanned[name])
		if err != nil {
			return err
		}
		s.scanned[name] = end
	}
	return nil
}

// scanSegmentLocked 从 from 开始顺序读取一个段文件的记录，返回最后一条完整
// 记录的末尾位置。长度字段不合法或记录不完整（另一个进程正在写入，或写入
// 时进程被杀）时停在该记录之前；CRC 不符或维度不符的记录长度字段仍可信，
// 跳过它继续读。
func (s *SharedVectors) scanSegmentLocked(name string, from int64) (int64, error) {
	file, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		return from, err
	}
	defer file.Close()
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		return from, err
	}
	reader := bufio.NewReaderSize(file, 1<<20)
	offset := from
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
			return offset, nil
		}
		payloadLen := binary.LittleEndian.Uint32(lenBuf[:])
		if payloadLen == 0 || payloadLen > 1<<20 {
			return offset, nil
		}
		record := make([]byte, 4+int(payloadLen)+4)
		copy(record, lenBuf[:])
		if _, err := io.ReadFull(reader, record[4:]); err != nil {
			return offset, nil
		}
		key, _, ok := decodeSharedRecord(record, s.dimension)
		if !ok {
			s.skipped++
		} else if _, exists := s.index[key]; !exists {
			s.index[key] = sharedLocation{file: name, offset: offset, length: len(record)}
		}
		offset += int64(len(record))
	}
}

// decodeSharedRecord 解码一条完整记录（含长度前缀与 CRC），校验 CRC 与维度。
func decodeSharedRecord(record []byte, dimension int) (string, []float32, bool) {
	if len(record) < 4+2+4+4 {
		return "", nil, false
	}
	payloadLen := int(binary.LittleEndian.Uint32(record[:4]))
	if len(record) != 4+payloadLen+4 {
		return "", nil, false
	}
	payload := record[4 : 4+payloadLen]
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(record[4+payloadLen:]) {
		return "", nil, false
	}
	hashLen := int(binary.LittleEndian.Uint16(payload[:2]))
	rest := payload[2:]
	if len(rest) < hashLen+4 {
		return "", nil, false
	}
	key := string(rest[:hashLen])
	rest = rest[hashLen:]
	dim := int(binary.LittleEndian.Uint32(rest[:4]))
	rest = rest[4:]
	if dim != dimension || len(rest) != dim*4 {
		return "", nil, false
	}
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(rest[i*4:]))
	}
	return key, vec, true
}

// unitVector 报告向量的分量是否全为有限值且长度约为 1。
func unitVector(vec []float32) bool {
	var sum float64
	for _, value := range vec {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
		sum += f * f
	}
	return math.Abs(math.Sqrt(sum)-1) <= 1e-3
}
