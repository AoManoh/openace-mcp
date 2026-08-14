package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/pathutil"
)

func writeScanFixture(t *testing.T, root string) []string {
	t.Helper()
	files := map[string]string{
		"main.go":     "package main\n\nfunc main() {}\n",
		"pkg/util.go": "package pkg\n\nfunc Util() {}\n",
	}
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"main.go", "pkg/util.go"}
}

func relPaths(files []fileBlob) []string {
	rels := make([]string, 0, len(files))
	for _, f := range files {
		rels = append(rels, f.RelPath)
	}
	return rels
}

func requireSameRelPaths(t *testing.T, got []fileBlob, want []string) {
	t.Helper()
	gotRels := relPaths(got)
	if len(gotRels) != len(want) {
		t.Fatalf("扫描文件集不一致: got %v, want %v", gotRels, want)
	}
	for i := range want {
		if gotRels[i] != want[i] {
			t.Fatalf("扫描文件集不一致: got %v, want %v", gotRels, want)
		}
	}
}

// H2 回归（scan 层防御）：经 symlink 扫同一目录必须与目录本体扫描
// 结果一致，绝不允许"成功返回空集"（现状：direct=N files,
// viaLink=0 files, err=nil，legacy 路径会据此把远端索引删光）。
func TestScanViaSymlinkRootMatchesDirectScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("在 Windows 上创建符号链接需要特权，跳过")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeScanFixture(t, real)

	direct, err := scan(context.Background(), real)
	if err != nil {
		t.Fatalf("直接扫描目录本体: %v", err)
	}
	requireSameRelPaths(t, direct, want)

	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	viaLink, err := scan(context.Background(), link)
	if err != nil {
		t.Fatalf("经 symlink 扫描必须成功（或显式报错，绝不静默空集）: %v", err)
	}
	requireSameRelPaths(t, viaLink, want)
}

// H2 双层防御的第一层（pathutil）与 scan 的组合链路：生产路径
// （syncRoot / localengine build）先经 ResolveWorkspaceRoot 得
// CanonicalPath 再扫描，symlink 根经该链路必须得到同样的文件集。
func TestScanAfterResolveWorkspaceRootOnSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("在 Windows 上创建符号链接需要特权，跳过")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeScanFixture(t, real)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	root, err := pathutil.ResolveWorkspaceRoot(link)
	if err != nil {
		t.Fatal(err)
	}
	files, err := scan(context.Background(), root.CanonicalPath)
	if err != nil {
		t.Fatalf("经 ResolveWorkspaceRoot 后扫描: %v", err)
	}
	requireSameRelPaths(t, files, want)
}

// H2 防御边界：根不是目录（普通文件 / symlink→文件 / 断链 / 不存在）
// 时必须显式报错——与"根不存在时 WalkDir 报错"的行为对齐，禁止
// 静默空集或把根当单文件收录。
func TestScanRootMustBeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("在 Windows 上创建符号链接需要特权，跳过")
	}
	base := t.TempDir()

	regular := filepath.Join(base, "regular.go")
	if err := os.WriteFile(regular, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(base, "file-link")
	if err := os.Symlink(regular, fileLink); err != nil {
		t.Fatal(err)
	}
	brokenLink := filepath.Join(base, "broken-link")
	if err := os.Symlink(filepath.Join(base, "gone"), brokenLink); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		root string
	}{
		{name: "普通文件作根", root: regular},
		{name: "symlink指向文件作根", root: fileLink},
		{name: "断链作根", root: brokenLink},
		{name: "不存在路径作根", root: filepath.Join(base, "missing")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := scan(context.Background(), tc.root)
			if err == nil {
				t.Fatalf("必须显式报错，got err=nil files=%v", relPaths(files))
			}
		})
	}
}

// M1 回归：扫描中途出现无读权限文件（或 TOCTOU 消失文件）不得中止
// 整次扫描/构建；无读权限文件跳过并如实计数（K6 口径）。
// 用权限法固定复现：chmod 000 后读取必然 EACCES（root/CAP_DAC_OVERRIDE
// 会绕过权限位，此时跳过本测试）。
func TestScanSkipsUnreadableFileWithoutAborting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 权限语义在 Windows 上不适用，跳过")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "readable.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "locked.go")
	if err := os.WriteFile(locked, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if _, err := os.ReadFile(locked); err == nil {
		t.Skip("当前用户可无视权限位（root/CAP_DAC_OVERRIDE），无法构造 EACCES，跳过")
	}

	files, err := scan(context.Background(), root)
	if err != nil {
		t.Fatalf("单文件无读权限不得中止整次扫描: %v", err)
	}
	requireSameRelPaths(t, files, []string{"readable.go"})

	// 跳过必须如实计数(K6 口径),nil 缓存与带缓存两条读取路径都要覆盖。
	for _, cache := range []*StatCache{nil, NewStatCache()} {
		files, stats, err := scanWithCacheStats(context.Background(), root, cache)
		if err != nil {
			t.Fatalf("scanWithCacheStats(cache=%v): %v", cache != nil, err)
		}
		requireSameRelPaths(t, files, []string{"readable.go"})
		if stats.PermissionSkippedFiles != 1 {
			t.Fatalf("PermissionSkippedFiles = %d, want 1(cache=%v)", stats.PermissionSkippedFiles, cache != nil)
		}
	}
}

// M1 边界锁定:目录级错误(ReadDir 失败)保持致命——子树整体缺失时
// 静默继续会产出系统性残缺的索引(与单文件跳过语义相对照)。该注入
// 方式(chmod 000 子目录)也是 sync 失败类测试在 M1 之后仍然有效的
// 故障注入口径。
func TestScanUnreadableDirectoryStaysFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 权限语义在 Windows 上不适用，跳过")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "hidden.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if _, err := os.ReadDir(sub); err == nil {
		t.Skip("当前用户可无视权限位（root/CAP_DAC_OVERRIDE），无法构造 EACCES，跳过")
	}

	if _, err := scan(context.Background(), root); err == nil {
		t.Fatal("目录级 ReadDir 错误必须保持致命，不得静默继续")
	}
}

// M1 错误分类契约:fs.ErrNotExist 的 TOCTOU 窗口(ReadDir 与 stat/read
// 之间文件消失)无法在测试里稳定注入,按报告建议以错误分类函数单测
// 钉住语义;真实 os 错误形状(*fs.PathError 包 syscall errno)一并覆盖。
func TestScanFileSkipDispositionClassification(t *testing.T) {
	cases := []struct {
		name           string
		err            error
		wantSkip       bool
		wantPermission bool
	}{
		{name: "nil 错误不跳过", err: nil, wantSkip: false, wantPermission: false},
		{name: "裸 ErrNotExist 跳过不计数", err: fs.ErrNotExist, wantSkip: true, wantPermission: false},
		{name: "包装 ErrNotExist 跳过不计数", err: fmt.Errorf("stat: %w", fs.ErrNotExist), wantSkip: true, wantPermission: false},
		{name: "PathError+ENOENT 跳过不计数", err: &fs.PathError{Op: "stat", Path: "x", Err: syscall.ENOENT}, wantSkip: true, wantPermission: false},
		{name: "裸 ErrPermission 跳过并计数", err: fs.ErrPermission, wantSkip: true, wantPermission: true},
		{name: "PathError+EACCES 跳过并计数", err: &fs.PathError{Op: "open", Path: "x", Err: syscall.EACCES}, wantSkip: true, wantPermission: true},
		{name: "ctx 取消保持致命", err: context.Canceled, wantSkip: false, wantPermission: false},
		{name: "ctx 超时保持致命", err: context.DeadlineExceeded, wantSkip: false, wantPermission: false},
		{name: "其他 IO 错误保持致命", err: errors.New("input/output error"), wantSkip: false, wantPermission: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, permission := scanFileSkipDisposition(tc.err)
			if skip != tc.wantSkip || permission != tc.wantPermission {
				t.Fatalf("scanFileSkipDisposition(%v) = (skip=%v, permission=%v), want (skip=%v, permission=%v)",
					tc.err, skip, permission, tc.wantSkip, tc.wantPermission)
			}
		})
	}
}

// TestScanSkipsWhitespaceOnlyFiles 回归 T1-churn(docs/tasks/T1,2026-08-14):
// 纯空白/空文件可通过文本门禁但 chunker 必然零产出,收入资产集会造成
// "扫描见之、manifest 无之"的永真 assetsChanged→每次 sync 全都重建发版
// (gradle 真实仓 1 字节换行文件实证)。资产集与 manifest 的可索引判定
// 必须同一口径:零 chunk 内容在扫描层剔除。
func TestScanSkipsWhitespaceOnlyFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.go"), []byte("package a\n\nfunc A() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "newline.gradle"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "spaces.md"), []byte("  \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assets, err := FileAssetSource{}.Load(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, asset := range assets {
		paths[asset.RelPath] = true
	}
	if !paths["real.go"] {
		t.Fatalf("真实文件必须在资产集: %v", paths)
	}
	for _, ws := range []string{"newline.gradle", "empty.txt", "spaces.md"} {
		if paths[ws] {
			t.Fatalf("纯空白文件 %s 不得进入资产集(零 chunk 必然造成 manifest 永真差异)", ws)
		}
	}
}
