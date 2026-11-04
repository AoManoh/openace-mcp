package index

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeJournalFile 按给定顺序拼接字节写出 journal 文件，用来构造文件中间有坏数据
// 的情形；返回文件路径。
func writeJournalFile(t *testing.T, store *Store, parts ...[]byte) string {
	t.Helper()
	if err := os.MkdirAll(store.JournalDir(), dirPerm); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, part := range parts {
		data = append(data, part...)
	}
	path := filepath.Join(store.JournalDir(), journalFileName)
	if err := os.WriteFile(path, data, filePerm); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// assertJournalVectors 检查 want 里的键都能按偏移读回原向量，absent 里的键不在
// journal 里。
func assertJournalVectors(t *testing.T, journal *Journal, want map[string][]float32, absent ...string) {
	t.Helper()
	for key, vec := range want {
		got := make([]float32, len(vec))
		if err := journal.ReadInto(key, got); err != nil {
			t.Fatalf("键 %s 应保留并能读回: %v", key, err)
		}
		if !reflect.DeepEqual(got, vec) {
			t.Fatalf("键 %s 读回 %v，应为 %v", key, got, vec)
		}
	}
	for _, key := range absent {
		if journal.Has(key) {
			t.Fatalf("键 %s 的记录读不通过，不应留在 journal 里", key)
		}
	}
	if journal.Len() != len(want) {
		t.Fatalf("journal 条目数 %d，应为 %d", journal.Len(), len(want))
	}
}

// TestJournalSkipsDamagedDataInMiddle 是 K40 改规则：打开时遇到文件中间读不通过的
// 数据，只跳过这一段，其后的已付费记录保留；文件随即改写成只含有效记录，严格的
// 只读扫描不再报损坏。改动前遇到第一处坏数据就截断，其后记录全部丢失。
func TestJournalSkipsDamagedDataInMiddle(t *testing.T) {
	a, c, d := []float32{1, 0, 0, 0}, []float32{0, 0, 1, 0}, []float32{0, 0, 0, 1}
	recA, recB := encodeJournalRecord("key-a", a), encodeJournalRecord("key-b", []float32{0, 1, 0, 0})
	recC, recD := encodeJournalRecord("key-c", c), encodeJournalRecord("key-d", d)
	payloadBitFlip := append([]byte(nil), recB...)
	payloadBitFlip[len(payloadBitFlip)-6] ^= 0xFF
	hugeLength := append([]byte(nil), recB...)
	copy(hugeLength, []byte{0xFF, 0xFF, 0xFF, 0x7F})
	shiftedLength := append([]byte(nil), recB...)
	shiftedLength[0] += 4
	otherDimension := encodeJournalRecord("key-x", []float32{1, 0, 0, 0, 0, 0, 0, 0})
	cases := []struct {
		name    string
		damaged []byte
		records int
	}{
		{"向量字节坏了", payloadBitFlip, 1},
		{"长度字段超出上限", hugeLength, 1},
		{"长度字段多了 4 字节", shiftedLength, 1},
		{"旧版本留下的半条记录", recB[:20], 1},
		{"一段全零", make([]byte, 2*len(recB)), 2},
		{"维度不同的完整记录", otherDimension, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newV2TestStore(t)
			path := writeJournalFile(t, store, recA, tc.damaged, recC, recD)
			journal := openTestJournal(t, store, 4)
			assertJournalVectors(t, journal, map[string][]float32{"key-a": a, "key-c": c, "key-d": d}, "key-b", "key-x")
			repair := journal.Repair()
			want := JournalRepair{SkippedRegions: 1, SkippedBytes: int64(len(tc.damaged)), EstimatedRecords: tc.records, Rewritten: true}
			if repair != want {
				t.Fatalf("修复记录 %+v，应为 %+v", repair, want)
			}
			if !strings.Contains(repair.Summary(), "跳过 1 处") {
				t.Fatalf("修复说明应写明跳过的段数: %q", repair.Summary())
			}
			if got, wantSize := fileSize(t, path), int64(len(recA)+len(recC)+len(recD)); got != wantSize {
				t.Fatalf("改写后文件 %d 字节，应为 %d", got, wantSize)
			}
			if _, _, err := ReadJournalSnapshot(store.JournalDir(), 4); err != nil {
				t.Fatalf("改写后的文件应能被严格只读扫描完整读出: %v", err)
			}
		})
	}
}

// TestJournalResyncRejectsUnnormalizedRecord：跳过坏数据后查找下一条记录时，CRC 正确
// 但向量不是单位长度的位置不算记录起点（生产写进 journal 的向量都先归一化），继续
// 往后找。
func TestJournalResyncRejectsUnnormalizedRecord(t *testing.T) {
	store := newV2TestStore(t)
	a, c := []float32{1, 0, 0, 0}, []float32{0, 0.6, 0.8, 0}
	recA, recC := encodeJournalRecord("key-a", a), encodeJournalRecord("key-c", c)
	garbage := []byte{1, 2, 3, 4, 5, 6, 7}
	unnormalized := encodeJournalRecord("key-n", []float32{2, 0, 0, 0})
	writeJournalFile(t, store, recA, garbage, unnormalized, recC)
	journal := openTestJournal(t, store, 4)
	assertJournalVectors(t, journal, map[string][]float32{"key-a": a, "key-c": c}, "key-n")
	if repair := journal.Repair(); repair.SkippedBytes != int64(len(garbage)+len(unnormalized)) {
		t.Fatalf("应跳过垃圾字节与未归一化的记录: %+v", repair)
	}
}

// TestJournalTornTailTruncatedAndReported：末尾没有完整记录时仍按原规则截掉，并在修复
// 记录里写明截掉的字节数；不算跳过、不改写文件。
func TestJournalTornTailTruncatedAndReported(t *testing.T) {
	store := newV2TestStore(t)
	a := []float32{1, 0, 0, 0}
	recA := encodeJournalRecord("key-a", a)
	tail := encodeJournalRecord("key-b", []float32{0, 1, 0, 0})[:30]
	path := writeJournalFile(t, store, recA, tail)
	journal := openTestJournal(t, store, 4)
	assertJournalVectors(t, journal, map[string][]float32{"key-a": a}, "key-b")
	if repair := journal.Repair(); repair != (JournalRepair{TruncatedTailBytes: int64(len(tail))}) {
		t.Fatalf("修复记录应只有截掉的尾部: %+v", repair)
	}
	if got := fileSize(t, path); got != int64(len(recA)) {
		t.Fatalf("不完整的尾部应截掉: %d", got)
	}
	if err := journal.Append(map[string][]float32{"key-c": {0, 0, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 4)
	if reopened.Len() != 2 || reopened.Repair() != (JournalRepair{}) {
		t.Fatalf("截断后追加的文件应干净: len=%d repair=%+v", reopened.Len(), reopened.Repair())
	}
}

// failAfterReader 在读出 after 字节后返回 err，模拟磁盘读错误。
type failAfterReader struct {
	r     io.Reader
	after int
	err   error
}

func (f *failAfterReader) Read(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, f.err
	}
	if len(p) > f.after {
		p = p[:f.after]
	}
	n, err := f.r.Read(p)
	f.after -= n
	return n, err
}

// TestJournalOpenReadErrorKeepsFile：打开时读文件本身报错（不是记录内容读不通过）
// 不截断、不改写，OpenJournal 返回包装 ErrJournalRead 的错误并写明路径。改动前读错误
// 也按坏记录截断，暂时性的读错误会删掉报错位置之后的全部已付费记录。
func TestJournalOpenReadErrorKeepsFile(t *testing.T) {
	store := newV2TestStore(t)
	recA := encodeJournalRecord("key-a", []float32{1, 0, 0, 0})
	recB := encodeJournalRecord("key-b", []float32{0, 1, 0, 0})
	path := writeJournalFile(t, store, recA, recB)
	diskErr := errors.New("模拟磁盘读错误")
	journalLoadReader = func(f *os.File) io.Reader {
		return &failAfterReader{r: f, after: len(recA), err: &os.PathError{Op: "read", Path: f.Name(), Err: diskErr}}
	}
	t.Cleanup(func() { journalLoadReader = defaultJournalLoadReader })
	journal, err := OpenJournal(store, 4)
	if err == nil {
		_ = journal.Close()
		t.Fatal("读文件报错时 OpenJournal 应返回错误")
	}
	if !errors.Is(err, ErrJournalRead) || !errors.Is(err, diskErr) || !strings.Contains(err.Error(), path) {
		t.Fatalf("错误应包装 ErrJournalRead 与原始读错误并写明路径: %v", err)
	}
	if errors.Is(err, ErrJournalCorrupt) {
		t.Fatalf("读错误不应被报成记录损坏: %v", err)
	}
	if got := fileSize(t, path); got != int64(len(recA)+len(recB)) {
		t.Fatalf("读错误不应截断文件: %d", got)
	}
}

// shrinkOnFirstRead 在第一次读之前把文件截到 size 字节，模拟读取期间文件被别的
// 写入者改短。
type shrinkOnFirstRead struct {
	f    *os.File
	size int64
	done bool
}

func (s *shrinkOnFirstRead) Read(p []byte) (int, error) {
	if !s.done {
		s.done = true
		if err := os.Truncate(s.f.Name(), s.size); err != nil {
			return 0, err
		}
	}
	return s.f.Read(p)
}

// TestJournalOpenFileShrinkingDuringReadKeepsFile：打开后、读取期间文件变短时，读到的
// “不完整”不可信，按读文件报错处理，不截断、不跳过。
func TestJournalOpenFileShrinkingDuringReadKeepsFile(t *testing.T) {
	recA := encodeJournalRecord("key-a", []float32{1, 0, 0, 0})
	recB := encodeJournalRecord("key-b", []float32{0, 1, 0, 0})
	recC := encodeJournalRecord("key-c", []float32{0, 0, 1, 0})
	for name, shrinkTo := range map[string]int64{
		"变短到记录边界":   int64(len(recA)),
		"变短到一条记录中间": int64(len(recA) + 10),
	} {
		t.Run(name, func(t *testing.T) {
			store := newV2TestStore(t)
			path := writeJournalFile(t, store, recA, recB, recC)
			journalLoadReader = func(f *os.File) io.Reader { return &shrinkOnFirstRead{f: f, size: shrinkTo} }
			t.Cleanup(func() { journalLoadReader = defaultJournalLoadReader })
			journal, err := OpenJournal(store, 4)
			if err == nil {
				_ = journal.Close()
				t.Fatal("读取期间文件变短时 OpenJournal 应返回错误")
			}
			if !errors.Is(err, ErrJournalRead) || !strings.Contains(err.Error(), "变短") {
				t.Fatalf("应按读文件报错返回并写明文件变短: %v", err)
			}
			if got := fileSize(t, path); got != shrinkTo {
				t.Fatalf("不应再截断文件: %d，应保持 %d", got, shrinkTo)
			}
		})
	}
}

// TestJournalRepairRewriteFailureKeepsFile：跳过坏数据后改写失败时，打开照常成功、
// 文件不动，偏移表可用；修复记录写明改写失败，下一次发布后清理（即使没有已发布的键）
// 再改写。
func TestJournalRepairRewriteFailureKeepsFile(t *testing.T) {
	store := newV2TestStore(t)
	a, c := []float32{1, 0, 0, 0}, []float32{0, 0, 1, 0}
	recA, recC := encodeJournalRecord("key-a", a), encodeJournalRecord("key-c", c)
	damaged := encodeJournalRecord("key-b", []float32{0, 1, 0, 0})
	damaged[len(damaged)-6] ^= 0xFF
	path := writeJournalFile(t, store, recA, damaged, recC)
	journalRename = func(string, string) error { return errors.New("模拟改名失败") }
	t.Cleanup(func() { journalRename = renameWithRetry })
	journal := openTestJournal(t, store, 4)
	assertJournalVectors(t, journal, map[string][]float32{"key-a": a, "key-c": c}, "key-b")
	repair := journal.Repair()
	if repair.Rewritten || !strings.Contains(repair.RewriteError, "模拟改名失败") || !strings.Contains(repair.Summary(), "改写失败") {
		t.Fatalf("修复记录应写明改写失败: %+v %q", repair, repair.Summary())
	}
	if got := fileSize(t, path); got != int64(len(recA)+len(damaged)+len(recC)) {
		t.Fatalf("改写失败时文件不应改动: %d", got)
	}
	journalRename = renameWithRetry
	if err := journal.CompactAfterPublish(nil); err != nil {
		t.Fatal(err)
	}
	if got := fileSize(t, path); got != int64(len(recA)+len(recC)) {
		t.Fatalf("下一次发布后清理应去掉坏数据: %d", got)
	}
	assertJournalVectors(t, journal, map[string][]float32{"key-a": a, "key-c": c})
}

// TestJournalOpenRemovesLeftoverTempFiles：发布后清理改写到一半时进程被终止会留下
// .tmp-journal-* 临时文件；打开 journal 时删除它们，其他文件不动。
func TestJournalOpenRemovesLeftoverTempFiles(t *testing.T) {
	store := newV2TestStore(t)
	dir := store.JournalDir()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(dir, ".tmp-journal-12345")
	other := filepath.Join(dir, "bulk-state.json")
	for _, path := range []string{leftover, other} {
		if err := os.WriteFile(path, []byte("x"), filePerm); err != nil {
			t.Fatal(err)
		}
	}
	journal := openTestJournal(t, store, 4)
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("残留临时文件应删除: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("其他文件不应删除: %v", err)
	}
	if repair := journal.Repair(); repair != (JournalRepair{RemovedTempFiles: 1}) || !strings.Contains(repair.Summary(), "临时文件 1 个") {
		t.Fatalf("修复记录应写明删除的临时文件数: %+v %q", repair, repair.Summary())
	}
}

// TestJournalCleanOpenHasNoRepair：没有坏数据、没有残留文件时修复记录为空。
func TestJournalCleanOpenHasNoRepair(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	if err := journal.Append(map[string][]float32{"key-a": {1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 4)
	if repair := reopened.Repair(); repair != (JournalRepair{}) || repair.Summary() != "" {
		t.Fatalf("干净的 journal 不应有修复记录: %+v %q", repair, repair.Summary())
	}
}
