//go:build stress

package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/ace"
)

func TestStressLargeWorkspaceScanAndSync(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())

	stressWriteFile(t, root, ".gitignore", "/docs/\n/skills/\nnode_modules/\nignored-root/\n*.tmp\n")
	stressWriteFile(t, root, ".augmentignore", "!docs/\n!docs/**\n!skills/\n!skills/**/\n!skills/**/*.md\n")
	stressWriteFile(t, root, "main.go", "package main\n")

	for dir := 0; dir < 60; dir++ {
		for file := 0; file < 60; file++ {
			rel := fmt.Sprintf("src/pkg%02d/file%02d.go", dir, file)
			stressWriteFile(t, root, rel, fmt.Sprintf("package pkg%02d\nconst File%02d = %d\n", dir, file, file))
		}
	}
	for file := 0; file < 120; file++ {
		stressWriteFile(t, root, fmt.Sprintf("docs/decision-%03d.md", file), "important private project knowledge\n")
	}
	for dir := 0; dir < 30; dir++ {
		stressWriteFile(t, root, fmt.Sprintf("skills/skill%02d/SKILL.md", dir), "skill knowledge\n")
		stressWriteFile(t, root, fmt.Sprintf("skills/skill%02d/script.py", dir), "print('not indexed')\n")
	}
	for dir := 0; dir < 20; dir++ {
		for file := 0; file < 100; file++ {
			stressWriteFile(t, root, fmt.Sprintf("node_modules/pkg%02d/file%03d.js", dir, file), "ignored dependency\n")
			stressWriteFile(t, root, fmt.Sprintf("ignored-root/dir%02d/file%03d.txt", dir, file), "ignored root\n")
		}
	}
	for _, rel := range []string{
		".env",
		"docs/private/session.json",
		"docs/private/credentials",
		"docs/private/id_ed25519",
		"docs/private/tls.crt",
		"skills/skill00/token",
		"src/pkg00/scratch.tmp",
	} {
		stressWriteFile(t, root, rel, "must not be indexed\n")
	}

	files, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3700 || len(files) > 3800 {
		t.Fatalf("unexpected stress scan size: %d", len(files))
	}
	for _, file := range files {
		rel := file.RelPath
		if strings.HasPrefix(rel, "node_modules/") || strings.HasPrefix(rel, "ignored-root/") {
			t.Fatalf("ignored directory was indexed: %s", rel)
		}
		if strings.HasSuffix(rel, ".py") || strings.Contains(rel, "session.json") || strings.Contains(rel, "credentials") || strings.HasSuffix(rel, ".crt") || strings.HasPrefix(filepath.Base(rel), ".env") {
			t.Fatalf("hard-denied file was indexed: %s", rel)
		}
	}

	client := &stressACEClient{}
	result, err := NewSyncer(client).Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != len(files) {
		t.Fatalf("sync file count = %d, scan count = %d", result.FileCount, len(files))
	}
	if result.Uploaded != len(files) {
		t.Fatalf("expected every stress blob uploaded once, got uploaded=%d files=%d", result.Uploaded, len(files))
	}
	if client.findMissingCalls != 4 {
		t.Fatalf("expected 4 find-missing batches for %d files, got %d", len(files), client.findMissingCalls)
	}
	if client.uploaded != len(files) {
		t.Fatalf("client uploaded %d files, want %d", client.uploaded, len(files))
	}
}

func stressWriteFile(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type stressACEClient struct {
	mu               sync.Mutex
	findMissingCalls int
	uploaded         int
}

func (c *stressACEClient) FindMissing(ctx context.Context, names []string) ([]string, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	c.findMissingCalls++
	c.mu.Unlock()
	return append([]string(nil), names...), nil, nil
}

func (c *stressACEClient) BatchUpload(ctx context.Context, uploads []ace.BlobUpload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.uploaded += len(uploads)
	c.mu.Unlock()
	return nil
}

func (c *stressACEClient) CheckpointBlobs(ctx context.Context, previous string, added []string, deleted []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "checkpoint-stress", nil
}

func (c *stressACEClient) CodebaseRetrieval(ctx context.Context, query string, options ace.RetrievalOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "stress result", nil
}
