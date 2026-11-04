package vector

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// referenceWrite 是 WriteRows 出现之前 Write 的产物格式：vectors.dat 为逐行小端
// float32，vectors.idx 为 JSON 头。用它锁定按行写入与原写法逐字节相同。
func referenceWrite(t *testing.T, dir string, dimension int, entries []Entry, vectors [][]float32) {
	t.Helper()
	var data bytes.Buffer
	for _, v := range vectors {
		for _, x := range v {
			var word [4]byte
			binary.LittleEndian.PutUint32(word[:], math.Float32bits(x))
			data.Write(word[:])
		}
	}
	if err := os.WriteFile(filepath.Join(dir, DataFileName), data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := json.Marshal(indexHeader{SchemaVersion: indexSchemaVersion, Dimension: dimension, Count: len(entries), Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, IndexFileName), idx, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestWriteRowsMatchesWrite：同一组单位向量，按行写入、Write 与原写法的两个文件
// 逐字节相同，校验和与 Load 校验一致。
func TestWriteRowsMatchesWrite(t *testing.T) {
	const dim = 64
	entries, vectors := makeVectors(t, 300, dim, 7)
	rowsDir, writeDir, refDir := t.TempDir(), t.TempDir(), t.TempDir()
	rowsData, rowsIdx, err := WriteRows(rowsDir, dim, entries, func(i int, dst []float32) error {
		copy(dst, vectors[i])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	writeData, writeIdx, err := Write(writeDir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	if rowsData != writeData || rowsIdx != writeIdx {
		t.Fatalf("校验和不同: rows=%s/%s write=%s/%s", rowsData, rowsIdx, writeData, writeIdx)
	}
	referenceWrite(t, refDir, dim, entries, vectors)
	for _, name := range []string{DataFileName, IndexFileName} {
		want := readFile(t, filepath.Join(refDir, name))
		if got := readFile(t, filepath.Join(rowsDir, name)); !bytes.Equal(got, want) {
			t.Fatalf("WriteRows 的 %s 与原写法不同", name)
		}
		if got := readFile(t, filepath.Join(writeDir, name)); !bytes.Equal(got, want) {
			t.Fatalf("Write 的 %s 与原写法不同", name)
		}
	}
	ix, err := Load(rowsDir, dim, rowsData, rowsIdx)
	if err != nil {
		t.Fatalf("按行写入的文件应通过校验载入: %v", err)
	}
	_ = ix.Close()
}

// TestWriteRowsPropagatesRowError：读取函数返回的错误原样返回。
func TestWriteRowsPropagatesRowError(t *testing.T) {
	entries, vectors := makeVectors(t, 10, 8, 3)
	boom := errors.New("第 5 行读不出")
	_, _, err := WriteRows(t.TempDir(), 8, entries, func(i int, dst []float32) error {
		if i == 5 {
			return boom
		}
		copy(dst, vectors[i])
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应返回读取函数的错误: %v", err)
	}
}

// TestWriteRowsRejectsInvalidRow：读出的行未归一化或含非有限分量时报错，与 Write
// 的校验相同。
func TestWriteRowsRejectsInvalidRow(t *testing.T) {
	entries := []Entry{{ID: "a"}}
	if _, _, err := WriteRows(t.TempDir(), 2, entries, func(i int, dst []float32) error {
		dst[0], dst[1] = 3, 4
		return nil
	}); err == nil {
		t.Fatal("未归一化的行应报错")
	}
	if _, _, err := WriteRows(t.TempDir(), 2, entries, func(i int, dst []float32) error {
		dst[0], dst[1] = float32(math.NaN()), 0
		return nil
	}); err == nil {
		t.Fatal("含 NaN 的行应报错")
	}
}
