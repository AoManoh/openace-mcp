package index

import (
	"math"
	"strings"
	"testing"
)

func TestJournalRecordBytesMatchesEncoding(t *testing.T) {
	key := strings.Repeat("a", 64)
	for _, dim := range []int{8, 128} {
		got := int64(len(encodeJournalRecord(key, make([]float32, dim))))
		if want := JournalRecordBytes(key, dim); got != want {
			t.Fatalf("dim=%d: encoded %d bytes, rule says %d", dim, got, want)
		}
	}
	if JournalRecordBytes(key, 8) != 110 || JournalRecordBytes(key, 128) != 590 {
		t.Fatalf("64-char key records must be 110 and 590 bytes: %d %d", JournalRecordBytes(key, 8), JournalRecordBytes(key, 128))
	}
}

func TestJournalCapacityForCountsNewUniqueKeys(t *testing.T) {
	store := newTestStore(t)
	j, err := OpenJournal(store, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	existing := strings.Repeat("e", 64)
	if err := j.Append(map[string][]float32{existing: make([]float32, 8)}); err != nil {
		t.Fatal(err)
	}
	k1, k2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
	j.LimitBytes(330)
	current, needed, ok := j.CapacityFor([]string{k1, k1, k2, existing})
	if current != 110 || needed != 220 || !ok {
		t.Fatalf("existing 110 + two new unique records 220 fit 330: current=%d needed=%d ok=%t", current, needed, ok)
	}
	j.LimitBytes(329)
	if _, _, ok := j.CapacityFor([]string{k1, k2}); ok {
		t.Fatal("one byte short must not fit")
	}
	if err := j.Append(map[string][]float32{k1: make([]float32, 8), k2: make([]float32, 8)}); err == nil {
		t.Fatal("Append must apply the same rule and reject 110+220 > 329")
	}
	j.LimitBytes(330)
	if err := j.Append(map[string][]float32{k1: make([]float32, 8), k2: make([]float32, 8)}); err != nil {
		t.Fatalf("Append must accept 110+220 == 330: %v", err)
	}
	j.LimitBytes(0)
	if _, needed, ok := j.CapacityFor([]string{strings.Repeat("3", 64)}); !ok || needed != 110 {
		t.Fatalf("unlimited journal must fit: needed=%d ok=%t", needed, ok)
	}
	j.LimitBytes(math.MaxInt64)
	j.mu.Lock()
	j.bytes = math.MaxInt64 - 100
	j.mu.Unlock()
	if _, _, ok := j.CapacityFor([]string{strings.Repeat("4", 64), strings.Repeat("5", 64)}); ok {
		t.Fatal("capacity check must not overflow into a fit")
	}
}
