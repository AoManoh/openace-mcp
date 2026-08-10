package workspace

import (
	"context"
	"testing"
)

// 方案⑤(2026-08-02)确立 .openaceignore 为 canonical;2026-08-10 用户批示
// "augment 的所有引用都可以移除":.augmentignore 兼容别名整体退役,仅
// canonical 生效(别名文件按普通文件对待,不再解析其规则)。

// TestOpenaceignoreAloneReincludes:canonical 的 ! re-include 语义。
func TestOpenaceignoreAloneReincludes(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":     "/docs/\n",
		".openaceignore": "!docs/\n!docs/**\n",
		"docs/note.md":   "knowledge\n",
		"main.go":        "package main\n",
	} {
		writeWorkspaceTestFile(t, root, rel, content)
	}
	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	want := ".gitignore,.openaceignore,docs/note.md,main.go"
	if got != want {
		t.Fatalf("canonical 单独在场应支持 ! re-include:\n got: %s\nwant: %s", got, want)
	}
}

// TestAugmentignoreAliasIsInert:别名退役——.augmentignore 的规则不再被
// 解析,gitignored 内容不因它复活;别名文件本身按普通文件索引。
func TestAugmentignoreAliasIsInert(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":      "/docs/\n/skills/\n",
		".augmentignore":  "!docs/\n!docs/**\n!skills/\n!skills/**\n",
		"docs/note.md":    "knowledge\n",
		"skills/SKILL.md": "skill\n",
		"main.go":         "package main\n",
	} {
		writeWorkspaceTestFile(t, root, rel, content)
	}
	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	want := ".augmentignore,.gitignore,main.go"
	if got != want {
		t.Fatalf("退役后 alias 规则不得生效:\n got: %s\nwant: %s", got, want)
	}
}

// TestOpenaceignoreNestedReinclude:子目录 canonical 对父级 gitignored
// 内容的 ! re-include 仍按目录粒度生效。
func TestOpenaceignoreNestedReinclude(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":         "sub/\ndocs/\n",
		".openaceignore":     "!docs/\n!docs/**\n",
		"sub/.openaceignore": "!keep.md\n",
		"sub/keep.md":        "important\n",
		"sub/drop.md":        "ignored\n",
		"docs/note.md":       "knowledge\n",
		"main.go":            "package main\n",
	} {
		writeWorkspaceTestFile(t, root, rel, content)
	}
	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	want := ".gitignore,.openaceignore,docs/note.md,main.go,sub/keep.md"
	if got != want {
		t.Fatalf("子目录 canonical re-include 应生效:\n got: %s\nwant: %s", got, want)
	}
}

// TestOpenaceignoreHardDenyStillWins:hard safety denylist 不可被 canonical 覆盖。
func TestOpenaceignoreHardDenyStillWins(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":     "/secrets/\n",
		".openaceignore": "!secrets/\n!secrets/**\n",
		"secrets/.env":   "SECRET=x\n",
		"secrets/id_rsa": "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n",
		"main.go":        "package main\n",
	} {
		writeWorkspaceTestFile(t, root, rel, content)
	}
	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	want := ".gitignore,.openaceignore,main.go"
	if got != want {
		t.Fatalf("hard deny 不得被 canonical 放行:\n got: %s\nwant: %s", got, want)
	}
}
