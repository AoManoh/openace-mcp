package workspace

import (
	"context"
	"errors"
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

func TestStateFileUsesCacheNamespace(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "tenant/a")

	path, err := stateFile("/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cacheDir, "workspaces", "tenant-a") + string(os.PathSeparator)
	if !strings.HasPrefix(path, want) {
		t.Fatalf("state file %q does not use namespace prefix %q", path, want)
	}
}

func TestLoadStateRecoversCorruptState(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)

	_, path, err := loadState("/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, gotPath, err := loadState("/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Fatalf("state path changed: %s != %s", gotPath, path)
	}
	if st.CheckpointID != "" {
		t.Fatalf("corrupt state should be reset: %#v", st)
	}
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected corrupt backup, got %#v", matches)
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

func TestUploadBatchesSplitsByEstimatedPayloadSize(t *testing.T) {
	uploads := []ace.BlobUpload{
		{BlobName: "a", Path: "a.go", Content: strings.Repeat("a", 60)},
		{BlobName: "b", Path: "b.go", Content: strings.Repeat("b", 60)},
		{BlobName: "c", Path: "c.go", Content: strings.Repeat("c", 60)},
	}

	batches := uploadBatches(uploads, 160)
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d: %#v", len(batches), batches)
	}
	for i, batch := range batches {
		if len(batch) != 1 {
			t.Fatalf("batch %d should have 1 upload, got %d", i, len(batch))
		}
	}
}

func TestBatchConfigReadsPositiveEnvironmentValues(t *testing.T) {
	t.Setenv("OPENACE_UPLOAD_BATCH_BYTES", "12345")
	t.Setenv("OPENACE_FIND_MISSING_BATCH_SIZE", "77")
	t.Setenv("OPENACE_MAX_FILE_BYTES", "99")

	if got := uploadBatchBytes(); got != 12345 {
		t.Fatalf("upload batch bytes = %d", got)
	}
	if got := findMissingBatchSize(); got != 77 {
		t.Fatalf("find-missing batch size = %d", got)
	}
	if got := maxFileBytes(); got != 99 {
		t.Fatalf("max file bytes = %d", got)
	}
}

func TestBatchConfigFallsBackForInvalidEnvironmentValues(t *testing.T) {
	t.Setenv("OPENACE_UPLOAD_BATCH_BYTES", "-1")
	t.Setenv("OPENACE_FIND_MISSING_BATCH_SIZE", "not-a-number")
	t.Setenv("OPENACE_MAX_FILE_BYTES", "0")

	if got := uploadBatchBytes(); got != defaultUploadBatchBytes {
		t.Fatalf("upload batch bytes fallback = %d", got)
	}
	if got := findMissingBatchSize(); got != defaultFindMissingBatchSize {
		t.Fatalf("find-missing batch size fallback = %d", got)
	}
	if got := maxFileBytes(); got != defaultMaxFileBytes {
		t.Fatalf("max file bytes fallback = %d", got)
	}
}

func TestBatchUploadSendsEveryBatch(t *testing.T) {
	client := &recordingClient{}
	uploads := []ace.BlobUpload{
		{BlobName: "a", Path: "a.go", Content: strings.Repeat("a", 60)},
		{BlobName: "b", Path: "b.go", Content: strings.Repeat("b", 60)},
		{BlobName: "c", Path: "c.go", Content: strings.Repeat("c", 60)},
	}

	if err := batchUpload(context.Background(), client, uploads, 160); err != nil {
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
