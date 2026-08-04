package workspace

import (
	"context"
	"testing"
)

// 方案⑤(2026-08-02 批准,B 语义):.openaceignore 为 canonical;同目录
// 存在 .openaceignore 时 .augmentignore 规则整体被屏蔽(逐目录遮蔽);
// 仅 alias 时行为与历史一致(兼容迁移=改名)。

// TestOpenaceignoreAloneReincludes:仅 canonical——与 .augmentignore 同语义。
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

// TestOpenaceignoreShadowsAugmentignore:双文件并存——canonical 生效,
// alias 规则整体失效(B 语义;alias 文件本身仍被索引)。
func TestOpenaceignoreShadowsAugmentignore(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore": "/docs/\n/skills/\n",
		// alias 想放行 skills;canonical 只放行 docs——若 alias 生效,
		// skills/SKILL.md 会出现在扫描集,即违反 B 语义。
		".augmentignore":  "!skills/\n!skills/**\n",
		".openaceignore":  "!docs/\n!docs/**\n",
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
	want := ".augmentignore,.gitignore,.openaceignore,docs/note.md,main.go"
	if got != want {
		t.Fatalf("canonical 在场时 alias 规则应整体被屏蔽:\n got: %s\nwant: %s", got, want)
	}
}

// TestOpenaceignoreNestedShadowing:遮蔽按目录粒度——根用 canonical,
// 子目录仅有 alias 时子目录 alias 仍生效(迁移可渐进)。
func TestOpenaceignoreNestedShadowing(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":         "sub/\ndocs/\n",
		".openaceignore":     "!docs/\n!docs/**\n",
		"sub/.augmentignore": "!keep.md\n",
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
		t.Fatalf("子目录 alias(无同目录 canonical)应仍生效:\n got: %s\nwant: %s", got, want)
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
