package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAssetSetFileBlobsRoundTrip(t *testing.T) {
	files := []fileBlob{
		{
			AbsPath:  filepath.Join("tmp", "alpha.txt"),
			RelPath:  "alpha.txt",
			BlobName: "blob-alpha",
		},
		{
			AbsPath:  filepath.Join("tmp", "nested", "beta.go"),
			RelPath:  "nested/beta.go",
			BlobName: "blob-beta",
		},
	}

	got := assetSetFromFileBlobs(files).fileBlobs()
	if !reflect.DeepEqual(got, files) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", got, files)
	}
}

func TestFileAssetSourceMatchesLegacyScan(t *testing.T) {
	root := t.TempDir()
	writeAssetTestFiles(t, root, map[string]string{
		".gitignore":      "ignored.txt\n",
		"docs/guide.md":   "workspace knowledge\n",
		"ignored.txt":     "ignored\n",
		"main.go":         "package main\n",
		"nested/data.txt": "payload\n",
	})

	want, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	gotSet, err := FileAssetSource{}.Load(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	got := gotSet.fileBlobs()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("file asset source mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestFileAssetSourceMatchesLegacyScanForKnowledgeAssets(t *testing.T) {
	root := t.TempDir()
	writeAssetTestFiles(t, root, map[string]string{
		".gitignore":                "/AGENTS.md\n/CLAUDE.md\n/guidelines.md\n/rules/\n/docs/\n/skills/\n",
		".openaceignore":            "!AGENTS.md\n!CLAUDE.md\n!guidelines.md\n!rules/**/*.md\n!docs/**/*.md\n!skills/**/SKILL.md\n!skills/**/SPEC.md\n",
		"AGENTS.md":                 "project instructions\n",
		"CLAUDE.md":                 "claude instructions\n",
		"guidelines.md":              "project guidelines\n",
		"rules/project.md":           "project rule\n",
		"rules/script.py":           "print('not included')\n",
		"docs/decision.md":          "important project knowledge\n",
		"docs/script.py":            "print('not included')\n",
		"skills/local/SKILL.md":     "local skill knowledge\n",
		"skills/local/SPEC.md":      "local skill spec\n",
		"skills/local/README.md":    "not included\n",
		"main.go":                   "package main\n",
	})

	wantFiles, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	gotAssets, err := FileAssetSource{}.Load(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotAssets.fileBlobs(), wantFiles; !reflect.DeepEqual(got, want) {
		t.Fatalf("file asset source mismatch:\n got: %#v\nwant: %#v", got, want)
	}

	got := assetRelPaths(gotAssets)
	want := ".gitignore,.openaceignore,AGENTS.md,CLAUDE.md,docs/decision.md,guidelines.md,main.go,rules/project.md,skills/local/SKILL.md,skills/local/SPEC.md"
	if got != want {
		t.Fatalf("unexpected asset paths:\n got: %s\nwant: %s", got, want)
	}
}

func TestFileAssetSourceHardDenyCannotBeReincluded(t *testing.T) {
	root := t.TempDir()
	writeAssetTestFiles(t, root, map[string]string{
		".gitignore":                "/state/\n/docs/\n",
		".openaceignore":            "!state/**\n!docs/**\n",
		"state/session.json":        `{"accessToken":"fake"}`,
		"docs/private/.env.local":   "SECRET=fake\n",
		"docs/private/cert.key":     "private key\n",
		"docs/private/cert.p12":     "keystore\n",
		"docs/private/credentials":  "secret\n",
		"docs/private/id_rsa":       "private key\n",
		"docs/private/notes.md":     "allowed knowledge\n",
		"docs/private/tokens.json":  `{"token":"fake"}`,
		"docs/private/session.json": `{"accessToken":"fake"}`,
		"main.go":                   "package main\n",
	})

	assets, err := FileAssetSource{}.Load(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := assetRelPaths(assets)
	want := ".gitignore,.openaceignore,docs/private/notes.md,main.go"
	if got != want {
		t.Fatalf("unexpected asset paths:\n got: %s\nwant: %s", got, want)
	}
}

func assetRelPaths(assets AssetSet) string {
	rels := make([]string, 0, len(assets))
	for _, asset := range assets {
		rels = append(rels, asset.RelPath)
	}
	return strings.Join(rels, ",")
}

func writeAssetTestFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
