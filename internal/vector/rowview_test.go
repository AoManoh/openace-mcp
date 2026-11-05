package vector

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestRowViewMatchesLoadBits：数据文件可映射时，RowView 返回与整读装载位级一致的映射视图；
// Close 后 RowView 返回显式错误。视图随映射失效，调用方必须在 Close 之前用完。
func TestRowViewMatchesLoadBits(t *testing.T) {
	const count, dim = 32, 8
	entries, vectors := makeVectors(t, count, dim, 41)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Load(dir, dim, dataSum, idxSum)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	reader, err := OpenRowReader(dir, dim, idxSum)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.Mapped() {
		t.Fatal("默认配置下数据文件应映射")
	}
	for i := 0; i < count; i++ {
		row, err := reader.RowView(i)
		if err != nil {
			t.Fatalf("RowView(%d): %v", i, err)
		}
		if !reflect.DeepEqual(row, ix.Row(i)) {
			t.Fatalf("第 %d 行映射视图与整读位模式不一致", i)
		}
	}
	if _, err := reader.RowView(count); err == nil {
		t.Fatal("越界行号应拒绝")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.RowView(0); err == nil {
		t.Fatal("Close 后 RowView 应返回显式错误")
	}
}

// TestRowViewRejectsCorruptedRow：映射视图做与 ReadRow 相同的检查，全零行与含 NaN 的行被拒绝，
// 健康行不受影响。
func TestRowViewRejectsCorruptedRow(t *testing.T) {
	const count, dim = 4, 8
	entries, vectors := makeVectors(t, count, dim, 42)
	dir := t.TempDir()
	_, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(dir, DataFileName)
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	for b := dim * 4; b < dim*4*2; b++ {
		raw[b] = 0
	}
	binary.LittleEndian.PutUint32(raw[dim*4*3:], math.Float32bits(float32(math.NaN())))
	if err := os.WriteFile(dataPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRowReader(dir, dim, idxSum)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, bad := range []int{1, 3} {
		if _, err := reader.RowView(bad); err == nil {
			t.Fatalf("第 %d 行损坏，RowView 应拒绝", bad)
		}
	}
	for _, good := range []int{0, 2} {
		if _, err := reader.RowView(good); err != nil {
			t.Fatalf("健康的第 %d 行不应受损坏行影响: %v", good, err)
		}
	}
}

// TestRowViewUnavailableWithoutMapping：映射关闭时不映射数据文件，RowView 返回错误，
// ReadRow 照常返回拷贝。
func TestRowViewUnavailableWithoutMapping(t *testing.T) {
	saved := rowReaderMaps
	rowReaderMaps = func() bool { return false }
	defer func() { rowReaderMaps = saved }()
	const count, dim = 4, 8
	entries, vectors := makeVectors(t, count, dim, 43)
	dir := t.TempDir()
	_, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRowReader(dir, dim, idxSum)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.Mapped() {
		t.Fatal("映射关闭时不应映射数据文件")
	}
	if _, err := reader.RowView(0); err == nil {
		t.Fatal("未映射时 RowView 应返回错误")
	}
	if _, err := reader.ReadRow(0); err != nil {
		t.Fatalf("未映射时 ReadRow 应照常工作: %v", err)
	}
}
