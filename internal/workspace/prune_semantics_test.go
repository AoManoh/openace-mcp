package workspace

import (
	"context"
	"testing"
)

// P1「目录级剪枝」语义钉住(2026-08-03):扫描器对被忽略目录的下钻剪枝
// **早已存在**(scanWithCache:Match(dir)=ignored 且本目录无 include 层
// include 且活动规则无 descendant-include 时 SkipDir)。本文件把剪枝的
// 语义边界固化为回归契约,防后续优化悄悄改变行为:
//
//   契约:re-include(!)声明必须位于「被忽略目录的顶层或其祖先链」才
//   生效;埋在被剪枝子树更深处的 ignore 文件不会被读取(目录已 SkipDir,
//   这是剪枝的定义性代价,也是现状行为)。
//
// 更深层的"进入被忽略目录探测嵌套 ignore 文件"属语义扩展,涉及剪枝
// 收益回吐,须另行呈批(决策台账 2026-08-03 项)。

// TestPruneTopLevelReincludeWorks:re-include 位于被忽略目录顶层→生效
// (既有语义,防回归)。
func TestPruneTopLevelReincludeWorks(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":         "sub/\n",
		"sub/.openaceignore": "!keep.md\n",
		"sub/keep.md":        "knowledge\n",
		"sub/drop.md":        "ignored\n",
		"main.go":            "package main\n",
	} {
		writeWorkspaceTestFile(t, root, rel, content)
	}
	files, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(files)
	// 注:sub/.openaceignore 自身仍被父层 sub/ 规则忽略(它只 re-include
	// keep.md,不含自身)——与既有嵌套语义一致。
	want := ".gitignore,main.go,sub/keep.md"
	if got != want {
		t.Fatalf("顶层 re-include 应生效:\n got: %s\nwant: %s", got, want)
	}
}

// TestPruneDeepNestedReincludeIsNotHonored:re-include 埋在被忽略目录的
// 更深子目录 → 不生效(剪枝代价,钉住现状;若未来语义扩展须先改本测试
// 并走呈批)。
func TestPruneDeepNestedReincludeIsNotHonored(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":               "sub/\n",
		"sub/inner/.openaceignore": "!deep.md\n",
		"sub/inner/deep.md":        "buried knowledge\n",
		"main.go":                  "package main\n",
	} {
		writeWorkspaceTestFile(t, root, rel, content)
	}
	files, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(files)
	want := ".gitignore,main.go"
	if got != want {
		t.Fatalf("深层嵌套 re-include 不应生效(剪枝契约):\n got: %s\nwant: %s", got, want)
	}
}

// TestPruneAncestorDescendantIncludeKeepsDescent:祖先层声明了指向被忽略
// 目录内部的 re-include(如根 !docs/**)→ 不剪枝、下钻按文件级判定
// (既有语义;这也是宽 re-include 模式在大仓上的成本来源,见台账)。
func TestPruneAncestorDescendantIncludeKeepsDescent(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":     "docs/\n",
		".openaceignore": "!docs/**/*.md\n",
		"docs/a.md":      "keep\n",
		"docs/b.bin":     "drop\n",
		"docs/sub/c.md":  "keep too\n",
		"main.go":        "package main\n",
	} {
		writeWorkspaceTestFile(t, root, rel, content)
	}
	files, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(files)
	want := ".gitignore,.openaceignore,docs/a.md,docs/sub/c.md,main.go"
	if got != want {
		t.Fatalf("祖先 descendant-include 应保下钻且按文件判定:\n got: %s\nwant: %s", got, want)
	}
}
