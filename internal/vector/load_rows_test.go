package vector

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRowsFixture(t *testing.T, rows int, dim int) (string, string, string, []Entry, [][]float32) {
	t.Helper()
	dir := t.TempDir()
	entries := make([]Entry, rows)
	vectors := make([][]float32, rows)
	for i := range entries {
		entries[i] = Entry{ID: fmt.Sprintf("id-%d", i), ContentHash: fmt.Sprintf("key-%d", i)}
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i+1) + float32(j)*0.25
		}
		if err := Normalize(v); err != nil {
			t.Fatal(err)
		}
		vectors[i] = v
	}
	data, idx, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	return dir, data, idx, entries, vectors
}

func TestLoadRowsKeepsOnlyWantedRows(t *testing.T) {
	const dim = 4
	dir, data, idx, _, vectors := writeRowsFixture(t, 4, dim)
	want := map[string]bool{"key-1": true, "key-3": true}
	rows, err := LoadRows(dir, dim, data, idx, func(e Entry) bool { return want[e.ContentHash] }, -1)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Count != 4 || len(rows.Entries) != 2 || len(rows.Data) != 2 || len(rows.Skipped) != 0 {
		t.Fatalf("unexpected selection: count=%d entries=%d data=%d skipped=%d", rows.Count, len(rows.Entries), len(rows.Data), len(rows.Skipped))
	}
	for i, entry := range rows.Entries {
		source := vectors[map[string]int{"key-1": 1, "key-3": 3}[entry.ContentHash]]
		for j := range source {
			if rows.Data[i][j] != source[j] {
				t.Fatalf("row %s component %d = %v want %v", entry.ContentHash, j, rows.Data[i][j], source[j])
			}
		}
	}
}

func TestLoadRowsLimitReportsSkippedRowsAfterVerification(t *testing.T) {
	const dim = 4
	dir, data, idx, _, _ := writeRowsFixture(t, 4, dim)
	rows, err := LoadRows(dir, dim, data, idx, func(Entry) bool { return true }, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Data) != 1 || len(rows.Skipped) != 3 || rows.Entries[0].ContentHash != "key-0" {
		t.Fatalf("limit must keep the first wanted row and report the rest: data=%d skipped=%d", len(rows.Data), len(rows.Skipped))
	}
	none, err := LoadRows(dir, dim, data, idx, func(Entry) bool { return true }, 0)
	if err != nil || len(none.Data) != 0 || len(none.Skipped) != 4 {
		t.Fatalf("zero limit must keep no rows and report every wanted row: data=%d skipped=%d err=%v", len(none.Data), len(none.Skipped), err)
	}
}

func TestLoadRowsDetectsSameNormSwapWithWholeFileChecksum(t *testing.T) {
	const dim = 4
	dir, data, idx, _, _ := writeRowsFixture(t, 3, dim)
	path := filepath.Join(dir, DataFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rowStart := 1 * dim * 4
	first := binary.LittleEndian.Uint32(raw[rowStart:])
	second := binary.LittleEndian.Uint32(raw[rowStart+4:])
	if first == second {
		t.Fatal("fixture components must differ so the swap changes the bytes")
	}
	binary.LittleEndian.PutUint32(raw[rowStart:], second)
	binary.LittleEndian.PutUint32(raw[rowStart+4:], first)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRowReader(dir, dim, idx)
	if err != nil {
		t.Fatal(err)
	}
	_, rowErr := reader.ReadRow(1)
	_ = reader.Close()
	if rowErr != nil {
		t.Fatalf("the swap keeps the norm, so the per-row norm probe must accept it: %v", rowErr)
	}
	_, err = LoadRows(dir, dim, data, idx, func(e Entry) bool { return e.ContentHash == "key-0" }, -1)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("whole-file checksum must reject the segment even when only another row is wanted: %v", err)
	}
	unread, err := LoadRows(dir, dim, data, idx, func(Entry) bool { return false }, -1)
	if err != nil || unread.Count != 3 || len(unread.Data) != 0 {
		t.Fatalf("a segment with no wanted rows is not read and keeps its entry count: count=%d err=%v", unread.Count, err)
	}
}
