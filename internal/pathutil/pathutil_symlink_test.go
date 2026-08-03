package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// H2 回归：根为符号链接时 ResolveWorkspaceRoot 必须解析到真实目录，
// 否则 WalkDir 对根用 Lstat 会把 symlink 根当"非常规文件"跳过，
// 静默产出空索引（legacy 路径连带删远端索引）。
func TestResolveWorkspaceRootResolvesSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("在 Windows 上创建符号链接需要特权，跳过")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// base（/tmp 等）自身可能含符号链接组件，以 EvalSymlinks(real) 为基准。
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ResolveWorkspaceRoot(link)
	if err != nil {
		t.Fatalf("ResolveWorkspaceRoot(%q): %v", link, err)
	}
	if got.CanonicalPath != want {
		t.Fatalf("CanonicalPath = %q, want 解析后的真实目录 %q", got.CanonicalPath, want)
	}
	if got.InputPath != link {
		t.Fatalf("InputPath = %q, want 保留原始输入 %q", got.InputPath, link)
	}
	if got.PathKind != WorkspacePathNative {
		t.Fatalf("PathKind = %q, want native", got.PathKind)
	}
}

// 契约锁定：ResolveWorkspaceRoot 对不存在的路径必须继续可用并保持
// 纯词法 Abs 结果（现状契约：TestResolveWorkspaceRootForLinuxKeepsWSLMountPath
// 以不存在的 /mnt/d/... 断言 lexical 结果；daemon 对已删除 workspace
// 查询状态也依赖该行为）。EvalSymlinks 修复不得改变这一契约。
func TestResolveWorkspaceRootNonexistentPathKeepsLexicalAbs(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "sub")
	got, err := ResolveWorkspaceRoot(missing)
	if err != nil {
		t.Fatalf("ResolveWorkspaceRoot 对不存在路径必须不报错，got: %v", err)
	}
	if got.CanonicalPath != missing {
		t.Fatalf("CanonicalPath = %q, want 词法 Abs 结果 %q", got.CanonicalPath, missing)
	}
}

// 中间组件为符号链接、叶子不存在时同样保持词法结果（EvalSymlinks
// 要求全路径存在，此时必须回退而非报错）。
func TestResolveWorkspaceRootSymlinkParentWithMissingLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("在 Windows 上创建符号链接需要特权，跳过")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(link, "not-created-yet")
	got, err := ResolveWorkspaceRoot(missing)
	if err != nil {
		t.Fatalf("ResolveWorkspaceRoot 对不存在叶子必须不报错，got: %v", err)
	}
	if got.CanonicalPath != missing {
		t.Fatalf("CanonicalPath = %q, want 词法 Abs 结果 %q", got.CanonicalPath, missing)
	}
}
