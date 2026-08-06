package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanSkipsInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "valid.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "invalid.txt"), []byte{0xff, 0xfe, 'a'}, 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 valid text file, got %d: %#v", len(files), files)
	}
	if files[0].RelPath != "valid.txt" {
		t.Fatalf("unexpected scanned file: %s", files[0].RelPath)
	}
}

func TestScanHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := scan(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestScanSkipsSecretLikeFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"main.go":       "package main\n",
		".env":          "AUGMENT_TOKEN=fake-token\n",
		"id_ed25519":    "private-key\n",
		"cert.pem":      "private-cert\n",
		"cert.crt":      "public-cert\n",
		"cert.cer":      "public-cert\n",
		".npmrc":        "//registry/:_authToken=fake-token\n",
		"session.json":  `{"accessToken":"fake"}`,
		"nested/.envrc": "export SECRET=fake\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 1 || scanned[0].RelPath != "main.go" {
		t.Fatalf("expected only main.go to be scanned, got %#v", scanned)
	}
}

func TestScanHonorsRootGitignore(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":       "ignored.txt\nlogs/\n*.tmp\n!important.tmp\n",
		"kept.go":          "package kept\n",
		"ignored.txt":      "ignored\n",
		"logs/app.log":     "ignored\n",
		"scratch.tmp":      "ignored\n",
		"important.tmp":    "kept\n",
		"nested/other.txt": "kept\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, file := range scanned {
		rels = append(rels, file.RelPath)
	}
	got := strings.Join(rels, ",")
	if got != ".gitignore,important.tmp,kept.go,nested/other.txt" {
		t.Fatalf("unexpected scanned files: %s", got)
	}
}

func TestScanHonorsNestedGitignore(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"main.go":          "package main\n",
		"sub/.gitignore":   "secret.txt\n",
		"sub/secret.txt":   "secret\n",
		"sub/visible.txt":  "visible\n",
		"other/secret.txt": "visible elsewhere\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	if got != "main.go,other/secret.txt,sub/.gitignore,sub/visible.txt" {
		t.Fatalf("unexpected scanned files: %s", got)
	}
}

func TestScanHonorsNestedIgnoreAndOverridesParent(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".ignore":         "*.txt\n",
		"root.txt":        "ignored\n",
		"sub/.ignore":     "!keep.txt\n",
		"sub/drop.txt":    "ignored\n",
		"sub/keep.txt":    "kept\n",
		"sub/keep.go":     "package keep\n",
		"other/keep.txt":  "ignored\n",
		"other/keep.go":   "package other\n",
		"nested/main.go":  "package nested\n",
		"nested/main.txt": "ignored\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	want := ".ignore,nested/main.go,other/keep.go,sub/.ignore,sub/keep.go,sub/keep.txt"
	if got != want {
		t.Fatalf("unexpected scanned files:\n got: %s\nwant: %s", got, want)
	}
}

func TestScanAugmentignoreCanReincludeGitignoredAssets(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":                 "/AGENTS.md\n/CLAUDE.md\n/.augment-guidelines\n/.augment/\n/docs/\n/skills/\n",
		".augmentignore":             "!AGENTS.md\n!CLAUDE.md\n!.augment-guidelines\n!.augment/\n!.augment/rules/\n!.augment/rules/**/\n!.augment/rules/**/*.md\n!docs/\n!docs/**\n!skills/\n!skills/**/\n!skills/**/*.md\n",
		"AGENTS.md":                  "project instructions\n",
		"CLAUDE.md":                  "claude instructions\n",
		".augment-guidelines":        "project guidelines\n",
		".augment/rules/project.md":  "project rule\n",
		".augment/rules/script.py":   "print('not included')\n",
		"docs/decision.md":           "important project knowledge\n",
		"skills/local/SKILL.md":      "local skill knowledge\n",
		"skills/local/script.py":     "print('not included')\n",
		"main.go":                    "package main\n",
		"docs/private/session.json":  `{"accessToken":"fake"}`,
		"docs/private/id_ed25519":    "private-key\n",
		"docs/private/tls.crt":       "certificate\n",
		"docs/private/credentials":   "secret\n",
		"docs/private/.env.override": "SECRET=fake\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	want := ".augment-guidelines,.augment/rules/project.md,.augmentignore,.gitignore,AGENTS.md,CLAUDE.md,docs/decision.md,main.go,skills/local/SKILL.md"
	if got != want {
		t.Fatalf("unexpected scanned files:\n got: %s\nwant: %s", got, want)
	}
}

func TestScanNestedAugmentignoreCanReincludeParentIgnoredFile(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":         "sub/\n",
		"main.go":            "package main\n",
		"sub/.augmentignore": "!keep.md\n",
		"sub/keep.md":        "important local knowledge\n",
		"sub/drop.md":        "still ignored\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := scannedRelPaths(scanned)
	want := ".gitignore,main.go,sub/keep.md"
	if got != want {
		t.Fatalf("unexpected scanned files:\n got: %s\nwant: %s", got, want)
	}
}

func scannedRelPaths(files []fileBlob) string {
	rels := make([]string, 0, len(files))
	for _, file := range files {
		rels = append(rels, file.RelPath)
	}
	return strings.Join(rels, ",")
}

func writeWorkspaceTestFile(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
