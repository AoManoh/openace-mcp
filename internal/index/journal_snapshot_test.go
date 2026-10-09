package index

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// readTree 记录目录树内每个文件的字节内容；目录记为空串，用于证明只读
// 读取前后文件逐字节不变、也没有新建文件或目录。
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// writeSnapshotJournal 用 OpenJournal 写出两批已付费向量与两条拒绝记录。
func writeSnapshotJournal(t *testing.T, store *Store) map[string][]float32 {
	t.Helper()
	journal := openTestJournal(t, store, 4)
	first := map[string][]float32{"paid-a": {0.1, 0.2, 0.3, 0.4}}
	second := map[string][]float32{"paid-b": {1, 0, 0, 0}}
	for _, batch := range []map[string][]float32{first, second} {
		if err := journal.Append(batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.MarkRejected([]string{"zero-a", "zero-b"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	return map[string][]float32{"paid-a": first["paid-a"], "paid-b": second["paid-b"]}
}

// TestReadJournalSnapshotCleanFilesUntouched：完整文件读出全部条目与拒绝集，
// 文件逐字节不变；没有 journal 目录时返回空结果且不创建目录。
func TestReadJournalSnapshotCleanFilesUntouched(t *testing.T) {
	store := newV2TestStore(t)
	want := writeSnapshotJournal(t, store)
	before := readTree(t, store.Root())
	vectors, rejected, err := ReadJournalSnapshot(store.JournalDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vectors, want) || !reflect.DeepEqual(rejected, map[string]bool{"zero-a": true, "zero-b": true}) {
		t.Fatalf("只读快照应返回全部条目与拒绝集: vectors=%v rejected=%v", vectors, rejected)
	}
	if after := readTree(t, store.Root()); !reflect.DeepEqual(before, after) {
		t.Fatal("只读快照不得改动子树文件")
	}

	empty := newV2TestStore(t)
	emptyBefore := readTree(t, empty.Root())
	vectors, rejected, err = ReadJournalSnapshot(empty.JournalDir(), 4)
	if err != nil || len(vectors) != 0 || len(rejected) != 0 {
		t.Fatalf("没有 journal 的子树应读出空结果: %v %v err=%v", vectors, rejected, err)
	}
	if _, statErr := os.Stat(empty.JournalDir()); !os.IsNotExist(statErr) {
		t.Fatalf("只读快照不得创建 journal 目录: %v", statErr)
	}
	if after := readTree(t, empty.Root()); !reflect.DeepEqual(emptyBefore, after) {
		t.Fatal("只读快照不得在空子树里新建文件")
	}
}

// TestReadJournalSnapshotRejectsDamagedFiles：只有记录边界处的干净结尾算读完。
// CRC 翻转、半写记录、维度不符、拒绝集末行不完整都以 ErrJournalCorrupt 报错，
// 不返回部分结果，错误里带文件路径；文件保持原样，坏尾不被截断。
func TestReadJournalSnapshotRejectsDamagedFiles(t *testing.T) {
	vectorsFile := func(store *Store) string { return filepath.Join(store.JournalDir(), journalFileName) }
	rejectedFile := func(store *Store) string { return filepath.Join(store.JournalDir(), rejectedFileName) }
	cases := []struct {
		name      string
		damage    func(t *testing.T, store *Store)
		dimension int
		file      func(store *Store) string
		detail    string
	}{
		{"末条 CRC 翻转一位", func(t *testing.T, store *Store) {
			data, err := os.ReadFile(vectorsFile(store))
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)-1] ^= 1
			if err := os.WriteFile(vectorsFile(store), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}, 4, vectorsFile, "第 2 条记录"},
		{"半写记录", func(t *testing.T, store *Store) {
			file, err := os.OpenFile(vectorsFile(store), os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte{100, 0, 0, 0, 0xDE, 0xAD, 0xBE}); err != nil {
				t.Fatal(err)
			}
			_ = file.Close()
		}, 4, vectorsFile, "第 3 条记录"},
		{"维度不符", func(t *testing.T, store *Store) {}, 8, vectorsFile, "第 1 条记录"},
		{"拒绝集末行不完整", func(t *testing.T, store *Store) {
			file, err := os.OpenFile(rejectedFile(store), os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("zero-c-torn")); err != nil {
				t.Fatal(err)
			}
			_ = file.Close()
		}, 4, rejectedFile, "末行不完整"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newV2TestStore(t)
			writeSnapshotJournal(t, store)
			tc.damage(t, store)
			before := readTree(t, store.Root())
			vectors, rejected, err := ReadJournalSnapshot(store.JournalDir(), tc.dimension)
			if !errors.Is(err, ErrJournalCorrupt) || !strings.Contains(err.Error(), tc.file(store)) || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("应以 ErrJournalCorrupt 报出文件与位置 %q: vectors=%d rejected=%d err=%v", tc.detail, len(vectors), len(rejected), err)
			}
			if vectors != nil || rejected != nil {
				t.Fatalf("出错时不得返回部分结果: %v %v", vectors, rejected)
			}
			if after := readTree(t, store.Root()); !reflect.DeepEqual(before, after) {
				t.Fatal("只读快照不得改写或截断旧文件")
			}
		})
	}
}

// TestReadJournalSnapshotReportsIOErrorSeparately：读不出文件（这里是去掉读
// 权限）时返回带路径的 I/O 错误，不归为 ErrJournalCorrupt，调用方据此给出
// 不同的恢复指引。故障用 Unix 权限位注入：Windows 的 os.Chmod 只切换只读
// 属性，文件仍可读，因此跳过；root 或带 CAP_DAC_OVERRIDE 的用户不受权限位
// 限制，去掉读权限后仍能读出时同样跳过。
func TestReadJournalSnapshotReportsIOErrorSeparately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 os.Chmod 不提供 Unix 读权限语义，无法构造读取失败，跳过")
	}
	store := newV2TestStore(t)
	writeSnapshotJournal(t, store)
	path := filepath.Join(store.JournalDir(), journalFileName)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0o600) }()
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("当前用户可无视权限位（root/CAP_DAC_OVERRIDE），无法构造读取失败，跳过")
	}
	_, _, err := ReadJournalSnapshot(store.JournalDir(), 4)
	if err == nil || errors.Is(err, ErrJournalCorrupt) || !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), path) {
		t.Fatalf("读取失败应原样报出路径与权限错误，且不归为损坏: %v", err)
	}
}
