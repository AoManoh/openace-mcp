package index

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unit(dim int, seed float32) []float32 {
	vec := make([]float32, dim)
	var sum float64
	for i := range vec {
		vec[i] = seed + float32(i+1)
		sum += float64(vec[i]) * float64(vec[i])
	}
	norm := float32(math.Sqrt(sum))
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if isSharedSegment(entry) {
			names = append(names, filepath.Join(dir, entry.Name()))
		}
	}
	return names
}

// TestSharedVectorsAcrossInstances：两个实例（模拟两个进程）各写各的段文件，
// 第三个实例能查到两边写入的全部向量。
func TestSharedVectorsAcrossInstances(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	first, err := OpenSharedVectors(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenSharedVectors(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(map[string][]float32{"a": unit(4, 1), "b": unit(4, 2)}); err != nil {
		t.Fatal(err)
	}
	if err := second.Append(map[string][]float32{"c": unit(4, 3)}); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	_ = second.Close()
	if files := segmentFiles(t, dir); len(files) != 2 {
		t.Fatalf("每个写入者应有自己的段文件: %v", files)
	}
	reader, err := OpenSharedVectors(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Lookup([]string{"a", "b", "c", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["c"][0] != unit(4, 3)[0] {
		t.Fatalf("应查到 a、b、c: %v", got)
	}
	if size, err := reader.Bytes(); err != nil || size == 0 {
		t.Fatalf("Bytes: %d %v", size, err)
	}
}

// TestSharedVectorsSkipCorruptRecordAndPartialTail：CRC 不符的记录跳过并计数，
// 之后的记录照常可用；末尾写到一半的记录（写入时进程被杀）被忽略。
func TestSharedVectorsSkipCorruptRecordAndPartialTail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	writer, err := OpenSharedVectors(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(map[string][]float32{"first": unit(4, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(map[string][]float32{"second": unit(4, 2)}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	files := segmentFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("段文件: %v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	// 翻转第一条记录向量区的一个字节（长度字段不动），再追加半条记录。
	data[4+2+len("first")+4+1] ^= 0xff
	partial := encodeJournalRecord("third", unit(4, 3))
	data = append(data, partial[:len(partial)/2]...)
	if err := os.WriteFile(files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenSharedVectors(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Lookup([]string{"first", "second", "third"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["first"]; ok || len(got) != 1 || got["second"] == nil || reader.Skipped() != 1 {
		t.Fatalf("应跳过损坏记录、忽略半条记录: got=%v skipped=%d", got, reader.Skipped())
	}
}

// TestSharedVectorsRejectInvalidVectors：读出的向量含非有限值或不是单位长度
// 时不复用（构建只写入归一化后的向量）。
func TestSharedVectorsRejectInvalidVectors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	writer, err := OpenSharedVectors(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(map[string][]float32{"long": {1, 1, 1, 1}, "nan": {float32(math.NaN()), 0, 0, 0}, "ok": unit(4, 5)}); err != nil {
		t.Fatal(err)
	}
	got, err := writer.Lookup([]string{"long", "nan", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["ok"] == nil || writer.Skipped() != 2 {
		t.Fatalf("只应复用合法的单位向量: got=%v skipped=%d", got, writer.Skipped())
	}
	if err := writer.Append(map[string][]float32{"short": {1}}); err == nil || !strings.Contains(err.Error(), "维度") {
		t.Fatalf("维度不符应报错: %v", err)
	}
}
