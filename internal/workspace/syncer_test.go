package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/ace"
)

func TestStateFileUsesOpenACECacheDir(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)

	path, err := stateFile(filepath.Join("tmp", "workspace"))
	if err != nil {
		t.Fatal(err)
	}

	wantPrefix := filepath.Join(cacheDir, "workspaces") + string(os.PathSeparator)
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("state file %q does not use OPENACE_CACHE_DIR prefix %q", path, wantPrefix)
	}
	if filepath.Ext(path) != ".json" {
		t.Fatalf("state file should be json: %q", path)
	}
}

func TestScanSkipsInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "valid.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "invalid.txt"), []byte{0xff, 0xfe, 'a'}, 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := scan(root)
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

func TestUploadBatchesSplitsByEstimatedPayloadSize(t *testing.T) {
	uploads := []ace.BlobUpload{
		{BlobName: "a", Path: "a.go", Content: strings.Repeat("a", 60)},
		{BlobName: "b", Path: "b.go", Content: strings.Repeat("b", 60)},
		{BlobName: "c", Path: "c.go", Content: strings.Repeat("c", 60)},
	}

	batches := uploadBatches(uploads, 220)
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d: %#v", len(batches), batches)
	}
	for i, batch := range batches {
		if len(batch) != 1 {
			t.Fatalf("batch %d should have 1 upload, got %d", i, len(batch))
		}
	}
}

func TestBatchUploadSendsEveryBatch(t *testing.T) {
	client := &recordingClient{}
	uploads := []ace.BlobUpload{
		{BlobName: "a", Path: "a.go", Content: strings.Repeat("a", 60)},
		{BlobName: "b", Path: "b.go", Content: strings.Repeat("b", 60)},
		{BlobName: "c", Path: "c.go", Content: strings.Repeat("c", 60)},
	}

	if err := batchUpload(context.Background(), client, uploads, 220); err != nil {
		t.Fatal(err)
	}

	if len(client.batches) != 3 {
		t.Fatalf("expected 3 upload calls, got %d", len(client.batches))
	}
	var names []string
	for _, batch := range client.batches {
		for _, upload := range batch {
			names = append(names, upload.BlobName)
		}
	}
	if got := strings.Join(names, ","); got != "a,b,c" {
		t.Fatalf("uploads were not all sent in order: %s", got)
	}
}

func TestFindMissingBatchedAggregatesResults(t *testing.T) {
	client := &recordingClient{
		unknownByName: map[string]bool{
			"b": true,
			"d": true,
		},
		nonindexedByName: map[string]bool{
			"c": true,
			"e": true,
		},
	}

	unknown, nonindexed, err := findMissingBatched(context.Background(), client, []string{"a", "b", "c", "d", "e"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.findMissingBatches) != 3 {
		t.Fatalf("expected 3 find-missing calls, got %d", len(client.findMissingBatches))
	}
	if got := strings.Join(unknown, ","); got != "b,d" {
		t.Fatalf("unknown = %s", got)
	}
	if got := strings.Join(nonindexed, ","); got != "c,e" {
		t.Fatalf("nonindexed = %s", got)
	}
}

func TestFindMissingBatchedDefaultBatchSize(t *testing.T) {
	client := &recordingClient{}

	if _, _, err := findMissingBatched(context.Background(), client, []string{"a"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(client.findMissingBatches) != 1 {
		t.Fatalf("expected 1 find-missing call, got %d", len(client.findMissingBatches))
	}
}

type recordingClient struct {
	batches            [][]ace.BlobUpload
	findMissingBatches [][]string
	unknownByName      map[string]bool
	nonindexedByName   map[string]bool
}

func (c *recordingClient) FindMissing(ctx context.Context, names []string) ([]string, []string, error) {
	c.findMissingBatches = append(c.findMissingBatches, append([]string(nil), names...))
	var unknown []string
	var nonindexed []string
	for _, name := range names {
		if c.unknownByName[name] {
			unknown = append(unknown, name)
		}
		if c.nonindexedByName[name] {
			nonindexed = append(nonindexed, name)
		}
	}
	return unknown, nonindexed, nil
}

func (c *recordingClient) BatchUpload(ctx context.Context, uploads []ace.BlobUpload) error {
	copied := append([]ace.BlobUpload(nil), uploads...)
	c.batches = append(c.batches, copied)
	return nil
}

func (c *recordingClient) CheckpointBlobs(context.Context, string, []string, []string) (string, error) {
	return "", nil
}

func (c *recordingClient) CodebaseRetrieval(context.Context, string, ace.RetrievalOptions) (string, error) {
	return "", nil
}
