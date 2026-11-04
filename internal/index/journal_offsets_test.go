package index

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// journalTestVectors 生成 n 个确定性的 dim 维向量，键为 prefix-序号。journal 不检查
// 范数，这里不做归一化。
func journalTestVectors(prefix string, n, dim int, seed int64) map[string][]float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make(map[string][]float32, n)
	for i := 0; i < n; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()*2 - 1
		}
		out[fmt.Sprintf("%s-%04d", prefix, i)] = vec
	}
	return out
}

func mergeVectors(sets ...map[string][]float32) map[string][]float32 {
	out := map[string][]float32{}
	for _, set := range sets {
		for key, vec := range set {
			out[key] = vec
		}
	}
	return out
}

// assertReadBack 逐条按键读回，要求每个分量的位值与写入时相同。
func assertReadBack(t *testing.T, journal *Journal, want map[string][]float32) {
	t.Helper()
	dst := make([]float32, journal.dimension)
	for key, vec := range want {
		if !journal.Has(key) {
			t.Fatalf("journal 应有键 %s", key)
		}
		if err := journal.ReadInto(key, dst); err != nil {
			t.Fatalf("读回 %s: %v", key, err)
		}
		for i := range vec {
			if math.Float32bits(dst[i]) != math.Float32bits(vec[i]) {
				t.Fatalf("%s 第 %d 维读回 %v，写入 %v", key, i, dst[i], vec[i])
			}
		}
	}
}

// TestJournalReadIntoRoundTrip：分三批追加后按键读回的位值与写入相同，关闭重开后
// 同样；不存在的键返回错误。
func TestJournalReadIntoRoundTrip(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 8)
	all := map[string][]float32{}
	for b := 0; b < 3; b++ {
		batch := journalTestVectors(fmt.Sprintf("b%d", b), 20, 8, int64(b))
		if err := journal.Append(batch); err != nil {
			t.Fatal(err)
		}
		all = mergeVectors(all, batch)
	}
	if journal.Len() != len(all) {
		t.Fatalf("Len=%d，应为 %d", journal.Len(), len(all))
	}
	assertReadBack(t, journal, all)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 8)
	if reopened.Len() != len(all) {
		t.Fatalf("重开后 Len=%d，应为 %d", reopened.Len(), len(all))
	}
	assertReadBack(t, reopened, all)
	if reopened.Has("missing") {
		t.Fatal("不存在的键 Has 应为假")
	}
	if err := reopened.ReadInto("missing", make([]float32, 8)); err == nil {
		t.Fatal("读不存在的键应返回错误")
	}
}

// TestJournalReopenKeepsOnlyOffsets：重开一个 5,000 条 1024 维（约 20 MiB）的
// journal，只建键到偏移的表，存活堆增量应远小于向量本身。
func TestJournalReopenKeepsOnlyOffsets(t *testing.T) {
	store := newV2TestStore(t)
	writer, err := OpenJournal(store, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for b := 0; b < 5; b++ {
		if err := writer.Append(journalTestVectors(fmt.Sprintf("r%d", b), 1000, 1024, int64(b))); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writer = nil
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	reopened, err := OpenJournal(store, 1024)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	grown := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if reopened.Len() != 5000 {
		t.Fatalf("重开后 Len=%d，应为 5000", reopened.Len())
	}
	runtime.KeepAlive(reopened)
	if grown > 4<<20 {
		t.Fatalf("重开 5,000 条 1024 维 journal 后存活堆增加 %d 字节，应只保留偏移（不超过 4 MiB）", grown)
	}
	_ = reopened.Close()
}

// TestJournalAppendWriteFailureLeavesNoPartialRecord：一批记录只写了一半就失败时，
// 文件截回写入前的长度，本批不计入；之后的追加照常，重开后也只有完整的批次。
func TestJournalAppendWriteFailureLeavesNoPartialRecord(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	path := filepath.Join(store.JournalDir(), journalFileName)
	first := journalTestVectors("first", 3, 4, 1)
	if err := journal.Append(first); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sizeAfterFirst := info.Size()

	second := journalTestVectors("second", 3, 4, 2)
	original := journalWrite
	journalWrite = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2])
		return n, errors.New("模拟磁盘写满")
	}
	err = journal.Append(second)
	journalWrite = original
	if err == nil {
		t.Fatal("写入失败应返回错误")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != sizeAfterFirst {
		t.Fatalf("写入失败后文件应截回 %d 字节: size=%v err=%v", sizeAfterFirst, info.Size(), err)
	}
	for key := range second {
		if journal.Has(key) {
			t.Fatalf("写入失败的批次不应计入: %s", key)
		}
	}

	third := journalTestVectors("third", 3, 4, 3)
	if err := journal.Append(third); err != nil {
		t.Fatal(err)
	}
	assertReadBack(t, journal, mergeVectors(first, third))
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 4)
	if reopened.Len() != 6 {
		t.Fatalf("重开后应有 6 条，实际 %d", reopened.Len())
	}
	assertReadBack(t, reopened, mergeVectors(first, third))
}

// TestJournalRewriteFailureKeepsHandles：发布后清理时改名失败，原文件不动，读写
// 句柄重新打开，之后的追加与读回照常。
func TestJournalRewriteFailureKeepsHandles(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 2)
	if err := journal.Append(map[string][]float32{"published": {1, 0}, "pending": {0.6, 0.8}}); err != nil {
		t.Fatal(err)
	}
	original := journalRename
	journalRename = func(oldPath, newPath string) error { return errors.New("模拟改名失败") }
	err := journal.CompactAfterPublish(map[string]bool{"published": true})
	journalRename = original
	if err == nil {
		t.Fatal("改名失败应返回错误")
	}
	if err := journal.Append(map[string][]float32{"after": {0, 1}}); err != nil {
		t.Fatalf("清理失败后追加应照常: %v", err)
	}
	assertReadBack(t, journal, map[string][]float32{"pending": {0.6, 0.8}, "after": {0, 1}})
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	// 改名没有发生，原文件仍有已发布的条目；重开后它们与之后追加的条目都在。
	reopened := openTestJournal(t, store, 2)
	assertReadBack(t, reopened, map[string][]float32{"published": {1, 0}, "pending": {0.6, 0.8}, "after": {0, 1}})
}

// TestJournalReadIntoDetectsCorruption：记录在磁盘上被改坏时，读回返回
// ErrJournalCorrupt 并写明文件与键，该键移出偏移表，其他键照常；下一次清理不再
// 写出这条记录。
func TestJournalReadIntoDetectsCorruption(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	for i, key := range []string{"k0", "k1", "k2"} {
		if err := journal.Append(map[string][]float32{key: {float32(i + 1), 0, 0, 0}}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(store.JournalDir(), journalFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 每条记录：4 字节长度、2 字节键长、2 字节键、4 字节维度、16 字节向量、4 字节 CRC。
	const recordLen = 4 + 2 + 2 + 4 + 16 + 4
	data[recordLen+4+2+2+4+1] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = journal.ReadInto("k1", make([]float32, 4))
	if !errors.Is(err, ErrJournalCorrupt) || !strings.Contains(err.Error(), "k1") || !strings.Contains(err.Error(), path) {
		t.Fatalf("应返回带文件与键的 ErrJournalCorrupt: %v", err)
	}
	if journal.Has("k1") {
		t.Fatal("校验失败的键应移出偏移表")
	}
	assertReadBack(t, journal, map[string][]float32{"k0": {1, 0, 0, 0}, "k2": {3, 0, 0, 0}})
	if err := journal.CompactAfterPublish(map[string]bool{"k0": true}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 4)
	if reopened.Len() != 1 || !reopened.Has("k2") {
		t.Fatalf("清理后只应剩 k2: Len=%d", reopened.Len())
	}
	assertReadBack(t, reopened, map[string][]float32{"k2": {3, 0, 0, 0}})
}
