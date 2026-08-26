package index

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func openTestJournal(t *testing.T, store *Store, dim int) *Journal {
	t.Helper()
	journal, err := OpenJournal(store, dim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	return journal
}

// TestJournalRoundTrip 是 D4 基线：批落盘后跨打开 bit 级复原。
func TestJournalRoundTrip(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	want := map[string][]float32{
		"hash-a": {0.1, 0.2, 0.3, 0.4},
		"hash-b": {1, 0, 0, 0},
	}
	if err := journal.Append(want); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 4)
	got := reopened.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("journal 应 bit 级复原: got=%v want=%v", got, want)
	}
}

// TestJournalTornTailTruncated 是暗坑 K40：坏尾自动截断，完整条目保留。
func TestJournalTornTailTruncated(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	if err := journal.Append(map[string][]float32{"good": {1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	// 注入半写记录（长度头声称 100 字节但只有 3 字节垃圾）。
	path := filepath.Join(store.JournalDir(), journalFileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{100, 0, 0, 0, 0xDE, 0xAD, 0xBE}); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	reopened := openTestJournal(t, store, 4)
	if got := reopened.Snapshot(); len(got) != 1 || got["good"] == nil {
		t.Fatalf("完整条目应保留: %v", got)
	}
	// 坏尾已物理截断：再次追加与读取一致。
	if err := reopened.Append(map[string][]float32{"next": {0, 1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	final := openTestJournal(t, store, 4)
	if got := final.Snapshot(); len(got) != 2 {
		t.Fatalf("截断后追加应正常: %v", got)
	}
}

// TestJournalCompactAfterPublish 是暗坑 K41：发布即 GC。
func TestJournalCompactAfterPublish(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 2)
	if err := journal.Append(map[string][]float32{
		"published-1": {1, 0}, "published-2": {0, 1}, "pending": {0.6, 0.8},
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.CompactAfterPublish(map[string]bool{"published-1": true, "published-2": true}); err != nil {
		t.Fatal(err)
	}
	if got := journal.Snapshot(); len(got) != 1 || got["pending"] == nil {
		t.Fatalf("已发布条目应清除: %v", got)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 2)
	if got := reopened.Snapshot(); len(got) != 1 || got["pending"] == nil {
		t.Fatalf("压实应持久化: %v", got)
	}
}

// TestJournalReopenNeverDropsPaidEntries 是 2026-08-26 裁决的核心行为:
// journal 里的每一条都是已付费向量,重开时无论规模多大都必须完整保留,
// 不允许任何"超上限压实丢最旧"的静默丢弃(丢弃=下轮重新付费)。
// 用小维度(4)把 40 万+条目的文件压到 ~15MB,直打旧默认上限路径。
func TestJournalReopenNeverDropsPaidEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("40 万条重开用例在 -short 下跳过")
	}
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	const total = 401_000
	batch := make(map[string][]float32, 10_000)
	for i := 0; i < total; i++ {
		batch[fmt.Sprintf("h%06d", i)] = []float32{1, 0, 0, 0}
		if len(batch) == 10_000 || i == total-1 {
			if err := journal.Append(batch); err != nil {
				t.Fatal(err)
			}
			batch = make(map[string][]float32, 10_000)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 4)
	if got := len(reopened.Snapshot()); got != total {
		t.Fatalf("重开后已付费条目被丢弃: got=%d want=%d", got, total)
	}
}

// TestJournalAppendRespectsConfiguredByteBudget 是预算语义(opt-in):
// 预算只在"付费前拦截"新增,绝不丢弃既有条目;拦截必须显式报错。
func TestJournalAppendRespectsConfiguredByteBudget(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	first := map[string][]float32{"paid-1": {1, 0, 0, 0}}
	if err := journal.Append(first); err != nil {
		t.Fatal(err)
	}
	journal.LimitBytes(journal.bytes + 10) // 预算只够现有条目,容不下下一批
	err := journal.Append(map[string][]float32{"paid-2": {0, 1, 0, 0}})
	if err == nil {
		t.Fatal("超预算追加必须显式报错,不得静默接受或丢弃")
	}
	if got := journal.Snapshot(); len(got) != 1 || got["paid-1"] == nil {
		t.Fatalf("拦截不得影响既有已付费条目: %v", got)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 4)
	if got := reopened.Snapshot(); len(got) != 1 || got["paid-1"] == nil {
		t.Fatalf("拦截批不得部分落盘: %v", got)
	}
}

// TestJournalRejectedPersistence 是 K35 修订：拒绝史跨打开生效。
func TestJournalRejectedPersistence(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 2)
	if err := journal.MarkRejected([]string{"zero-hash"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 2)
	if !reopened.Rejected("zero-hash") || reopened.RejectedCount() != 1 {
		t.Fatalf("拒绝史应跨打开保留")
	}
}

// TestJournalDimensionMismatchDiscarded 是防御路径：维度不符的历史
// 条目整体丢弃（profile 子树隔离下不应出现）。
func TestJournalDimensionMismatchDiscarded(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 4)
	if err := journal.Append(map[string][]float32{"old-dim": {1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestJournal(t, store, 8)
	if got := reopened.Snapshot(); len(got) != 0 {
		t.Fatalf("维度不符条目应丢弃: %v", got)
	}
}

// TestJournalContainsNoPlaintext 是暗坑 K54：journal 只存 hash 与向量。
func TestJournalContainsNoPlaintext(t *testing.T) {
	store := newV2TestStore(t)
	journal := openTestJournal(t, store, 2)
	// 调用方只传 hash——本用例断言文件层面不含任何源码样式文本。
	if err := journal.Append(map[string][]float32{"a1b2c3": {1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.MarkRejected([]string{"d4e5f6"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{journalFileName, rejectedFileName} {
		raw, err := os.ReadFile(filepath.Join(store.JournalDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range [][]byte{[]byte("func "), []byte("package "), []byte("return")} {
			if bytes.Contains(raw, marker) {
				t.Fatalf("%s 不得含源码样式内容: %q", name, marker)
			}
		}
	}
}
