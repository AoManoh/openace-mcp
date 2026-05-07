package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestSyncerSingleflightSharesConcurrentWorkspaceSync(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := newBlockingSyncClient()
	syncer := NewSyncer(client)

	const workers = 5
	var wg sync.WaitGroup
	results := make([]Result, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = syncer.Sync(context.Background(), root)
		}(i)
	}

	client.waitForFindMissing(t)
	client.release()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d returned error: %v", i, err)
		}
		if results[i].CheckpointID != "checkpoint-1" {
			t.Fatalf("worker %d got checkpoint %q", i, results[i].CheckpointID)
		}
	}
	if got := client.findMissingCallCount(); got != 1 {
		t.Fatalf("expected one shared find-missing call, got %d", got)
	}
	if got := client.batchUploadCallCount(); got != 1 {
		t.Fatalf("expected one shared batch-upload call, got %d", got)
	}
	if got := client.checkpointCallCount(); got != 1 {
		t.Fatalf("expected one shared checkpoint call, got %d", got)
	}
}

func TestSyncerSingleflightCallerCancelDoesNotCancelOtherWaiters(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := newBlockingSyncClient()
	syncer := NewSyncer(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstErr := make(chan error, 1)
	go func() {
		_, err := syncer.Sync(ctx, root)
		firstErr <- err
	}()
	client.waitForFindMissing(t)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	secondResult := make(chan Result, 1)
	secondErr := make(chan error, 1)
	go func() {
		result, err := syncer.Sync(context.Background(), root)
		secondResult <- result
		secondErr <- err
	}()
	waitForInflightWaiters(t, syncer, absRoot, 2)

	cancel()
	if err := waitForError(t, firstErr); !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter should see context.Canceled, got %v", err)
	}

	client.release()
	if err := waitForError(t, secondErr); err != nil {
		t.Fatalf("second waiter should complete shared sync: %v", err)
	}
	if result := waitForResult(t, secondResult); result.CheckpointID != "checkpoint-1" {
		t.Fatalf("unexpected second result: %+v", result)
	}
	if got := client.findMissingCallCount(); got != 1 {
		t.Fatalf("expected one find-missing call, got %d", got)
	}
}

func TestSyncerSingleflightCancelsSharedSyncWhenAllWaitersCancel(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := newBlockingSyncClient()
	syncer := NewSyncer(client)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := syncer.Sync(ctx, root)
		errCh <- err
	}()
	client.waitForFindMissing(t)

	cancel()
	if err := waitForError(t, errCh); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller should see context.Canceled, got %v", err)
	}
	client.waitForSharedCancel(t)
}

func TestSyncerWorkspaceStatusTracksInflightAndCompletion(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := newBlockingSyncClient()
	syncer := NewSyncer(client)

	errCh := make(chan error, 1)
	go func() {
		_, err := syncer.Sync(context.Background(), root)
		errCh <- err
	}()
	client.waitForFindMissing(t)

	status, err := syncer.WorkspaceStatus(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !status.InFlight {
		t.Fatalf("workspace should be in flight: %+v", status)
	}
	if status.Stage != IndexStageReconciling {
		t.Fatalf("workspace should expose reconciling stage while find-missing is blocked: %+v", status)
	}
	if status.LastSyncReason != SyncReasonManual {
		t.Fatalf("workspace should record manual sync reason: %+v", status)
	}
	if status.LastStartedAt == nil {
		t.Fatalf("workspace should include last_started_at: %+v", status)
	}
	if status.StageStartedAt == nil {
		t.Fatalf("workspace should include stage_started_at: %+v", status)
	}

	client.release()
	if err := waitForError(t, errCh); err != nil {
		t.Fatal(err)
	}

	status, err = syncer.WorkspaceStatus(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if status.InFlight {
		t.Fatalf("workspace should not be in flight after sync: %+v", status)
	}
	if status.CheckpointID != "checkpoint-1" || status.FileCount != 1 {
		t.Fatalf("unexpected completed status: %+v", status)
	}
	if status.Stage != IndexStageReady {
		t.Fatalf("successful sync should end in ready stage: %+v", status)
	}
	if status.LastUploaded != 1 || status.LastAdded != 1 || status.LastDeleted != 0 {
		t.Fatalf("successful sync should expose last sync counters: %+v", status)
	}
	if status.LastError != "" {
		t.Fatalf("successful sync should clear last error: %+v", status)
	}
	if status.UpdatedAt == nil || status.LastFinishedAt == nil {
		t.Fatalf("completed status should include timestamps: %+v", status)
	}
}

func TestSyncerWorkspaceStatusLoadsDiskState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := stateFile(absRoot)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Now().UTC()
	if err := saveState(statePath, state{
		CheckpointID: "checkpoint-disk",
		BlobNames: map[string]string{
			"main.go": "blob-1",
		},
		UpdatedAt: updated,
	}); err != nil {
		t.Fatal(err)
	}

	status, err := NewSyncer(nil).WorkspaceStatus(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if status.DirectoryPath != absRoot {
		t.Fatalf("unexpected directory path: %+v", status)
	}
	if status.CheckpointID != "checkpoint-disk" || status.FileCount != 1 {
		t.Fatalf("unexpected disk status: %+v", status)
	}
	if status.Stage != IndexStageReady {
		t.Fatalf("disk status with checkpoint should be ready: %+v", status)
	}
	if status.UpdatedAt == nil || !status.UpdatedAt.Equal(updated) {
		t.Fatalf("unexpected updated_at: %+v", status)
	}
}

func TestSyncerWorkspaceStatusRecordsFailureStage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	syncer := NewSyncer(errorFindMissingClient{})
	_, err := syncer.Sync(context.Background(), root)
	if err == nil {
		t.Fatal("sync should fail")
	}
	status, err := syncer.WorkspaceStatus(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Stage != IndexStageFailed || status.LastErrorStage != IndexStageReconciling {
		t.Fatalf("failure status should retain failing stage: %+v", status)
	}
	if !strings.Contains(status.LastError, "find-missing failed") {
		t.Fatalf("failure status should retain error: %+v", status)
	}
}

func TestSyncerWorkspaceStatusIncludesUpstreamHealth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	backoffUntil := time.Now().Add(time.Minute).UTC()
	failureAt := time.Now().UTC()
	client := &healthReportingClient{
		health: ace.HealthSnapshot{
			Status:         "backoff",
			LastStatusCode: 429,
			LastError:      "find-missing returned HTTP 429: quota exhausted",
			RetryAfter:     time.Minute,
			BackoffUntil:   &backoffUntil,
			LastFailureAt:  &failureAt,
		},
	}

	status, err := NewSyncer(client).WorkspaceStatus(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if status.UpstreamStatus != "backoff" || status.UpstreamLastStatusCode != 429 {
		t.Fatalf("status should include upstream health: %+v", status)
	}
	if status.UpstreamRetryAfter != "1m0s" || status.UpstreamBackoffUntil == nil || status.UpstreamLastFailure == nil {
		t.Fatalf("status should include upstream retry timing: %+v", status)
	}
	if !strings.Contains(status.UpstreamLastError, "HTTP 429") {
		t.Fatalf("status should include upstream last error: %+v", status)
	}
}

func TestSyncerWorkspaceChangedComparesCurrentScanToState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nconst Version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	syncer := NewSyncer(&recordingClient{})
	changed, err := syncer.WorkspaceChanged(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("workspace without state should require sync")
	}

	if _, err := syncer.Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	changed, err = syncer.WorkspaceChanged(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("workspace should be unchanged immediately after sync")
	}

	if err := os.WriteFile(path, []byte("package main\nconst Version = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err = syncer.WorkspaceChanged(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("workspace change should be detected")
	}
}

func TestSyncerWorkspaceChangedAssetTransitions(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		initial map[string]string
		mutate  func(t *testing.T, root string)
		want    bool
	}{
		{
			name: "indexed file added",
			initial: map[string]string{
				"main.go": "package main\n",
			},
			mutate: func(t *testing.T, root string) {
				writeWorkspaceTestFile(t, root, "new.go", "package new\n")
			},
			want: true,
		},
		{
			name: "indexed file deleted",
			initial: map[string]string{
				"main.go": "package main\n",
				"old.go":  "package old\n",
			},
			mutate: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "old.go")); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name: "ignored file changed",
			initial: map[string]string{
				".gitignore":  "*.tmp\n",
				"ignored.tmp": "before\n",
				"main.go":     "package main\n",
			},
			mutate: func(t *testing.T, root string) {
				writeWorkspaceTestFile(t, root, "ignored.tmp", "after\n")
			},
			want: false,
		},
		{
			name: "secret file changed",
			initial: map[string]string{
				".env":    "SECRET=before\n",
				"main.go": "package main\n",
			},
			mutate: func(t *testing.T, root string) {
				writeWorkspaceTestFile(t, root, ".env", "SECRET=after\n")
			},
			want: false,
		},
		{
			name: "ignore policy reincludes existing asset",
			initial: map[string]string{
				".gitignore":  "docs/\n",
				"docs/one.md": "knowledge\n",
				"main.go":     "package main\n",
			},
			mutate: func(t *testing.T, root string) {
				writeWorkspaceTestFile(t, root, ".augmentignore", "!docs/**/*.md\n")
			},
			want: true,
		},
		{
			name: "indexed file becomes too large",
			env: map[string]string{
				"OPENACE_MAX_FILE_BYTES": "10",
			},
			initial: map[string]string{
				"main.go":   "package main\n",
				"small.txt": "1234567890",
			},
			mutate: func(t *testing.T, root string) {
				writeWorkspaceTestFile(t, root, "small.txt", "12345678901")
			},
			want: true,
		},
		{
			name: "indexed file becomes invalid utf8",
			initial: map[string]string{
				"main.go": "package main\n",
				"text.md": "valid\n",
			},
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "text.md")
				if err := os.WriteFile(path, []byte{0xff, 0xfe, 'x'}, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			for rel, content := range tt.initial {
				writeWorkspaceTestFile(t, root, rel, content)
			}
			syncer := NewSyncer(&recordingClient{})
			if _, err := syncer.Sync(context.Background(), root); err != nil {
				t.Fatal(err)
			}

			tt.mutate(t, root)
			changed, err := syncer.WorkspaceChanged(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tt.want {
				t.Fatalf("WorkspaceChanged = %v, want %v", changed, tt.want)
			}
		})
	}
}

func TestSyncRejectsMidSyncMutation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nconst Version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &mutatingSyncClient{
		path:    path,
		content: []byte("package main\nconst Version = 2\n"),
	}
	_, err := NewSyncer(client).Sync(context.Background(), root)
	if err == nil {
		t.Fatal("sync should fail when a file changes after asset discovery")
	}
	if !strings.Contains(err.Error(), "file changed during sync: main.go") {
		t.Fatalf("unexpected sync error: %v", err)
	}
	if client.batchUploadCalls != 0 {
		t.Fatalf("batch upload should not run after mid-sync mutation, got %d", client.batchUploadCalls)
	}
	if client.checkpointCalls != 0 {
		t.Fatalf("checkpoint should not run after mid-sync mutation, got %d", client.checkpointCalls)
	}
	st, _, err := loadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.CheckpointID != "" || len(st.BlobNames) != 0 {
		t.Fatalf("failed sync should not write state: %#v", st)
	}
}

func TestSyncerListsWorkspaceStatuses(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, root := range []string{rootB, rootA} {
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	syncer := NewSyncer(&recordingClient{})
	if _, err := syncer.Sync(context.Background(), rootB); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.Sync(context.Background(), rootA); err != nil {
		t.Fatal(err)
	}

	statuses, err := syncer.ListWorkspaceStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d: %+v", len(statuses), statuses)
	}
	if statuses[0].DirectoryPath > statuses[1].DirectoryPath {
		t.Fatalf("statuses should be sorted by directory path: %+v", statuses)
	}
}

func TestRetrieveAppliesRetrievalTimeout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_RETRIEVAL_TIMEOUT", "10ms")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &blockingRetrievalClient{started: make(chan struct{})}
	_, err := NewSyncer(client).Retrieve(context.Background(), root, "find code", 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retrieve error = %v, want deadline exceeded", err)
	}
	select {
	case <-client.started:
	default:
		t.Fatal("retrieval did not start")
	}
}

type recordingClient struct {
	batches            [][]ace.BlobUpload
	findMissingBatches [][]string
	unknownByName      map[string]bool
	nonindexedByName   map[string]bool
}

type healthReportingClient struct {
	recordingClient
	health ace.HealthSnapshot
}

func (c *healthReportingClient) HealthSnapshot() ace.HealthSnapshot {
	return c.health
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

type mutatingSyncClient struct {
	path             string
	content          []byte
	batchUploadCalls int
	checkpointCalls  int
}

func (c *mutatingSyncClient) FindMissing(ctx context.Context, names []string) ([]string, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(c.path, c.content, 0o600); err != nil {
		return nil, nil, err
	}
	return append([]string(nil), names...), nil, nil
}

func (c *mutatingSyncClient) BatchUpload(ctx context.Context, uploads []ace.BlobUpload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.batchUploadCalls++
	return nil
}

func (c *mutatingSyncClient) CheckpointBlobs(ctx context.Context, previous string, added []string, deleted []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.checkpointCalls++
	return "checkpoint-mutated", nil
}

func (c *mutatingSyncClient) CodebaseRetrieval(context.Context, string, ace.RetrievalOptions) (string, error) {
	return "", nil
}

type errorFindMissingClient struct{}

func (errorFindMissingClient) FindMissing(context.Context, []string) ([]string, []string, error) {
	return nil, nil, errors.New("find-missing failed")
}

func (errorFindMissingClient) BatchUpload(context.Context, []ace.BlobUpload) error {
	return nil
}

func (errorFindMissingClient) CheckpointBlobs(context.Context, string, []string, []string) (string, error) {
	return "", nil
}

func (errorFindMissingClient) CodebaseRetrieval(context.Context, string, ace.RetrievalOptions) (string, error) {
	return "", nil
}

type blockingRetrievalClient struct {
	startOnce sync.Once
	started   chan struct{}
}

func (c *blockingRetrievalClient) FindMissing(ctx context.Context, names []string) ([]string, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return append([]string(nil), names...), nil, nil
}

func (c *blockingRetrievalClient) BatchUpload(ctx context.Context, uploads []ace.BlobUpload) error {
	return ctx.Err()
}

func (c *blockingRetrievalClient) CheckpointBlobs(ctx context.Context, previous string, added []string, deleted []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "checkpoint-timeout", nil
}

func (c *blockingRetrievalClient) CodebaseRetrieval(ctx context.Context, query string, options ace.RetrievalOptions) (string, error) {
	c.startOnce.Do(func() { close(c.started) })
	<-ctx.Done()
	return "", ctx.Err()
}

type blockingSyncClient struct {
	mu                 sync.Mutex
	startOnce          sync.Once
	cancelOnce         sync.Once
	releaseOnce        sync.Once
	findMissingStarted chan struct{}
	findMissingCancel  chan struct{}
	releaseFindMissing chan struct{}
	findMissingCalls   int
	batchUploadCalls   int
	checkpointCalls    int
}

func newBlockingSyncClient() *blockingSyncClient {
	return &blockingSyncClient{
		findMissingStarted: make(chan struct{}),
		findMissingCancel:  make(chan struct{}),
		releaseFindMissing: make(chan struct{}),
	}
}

func (c *blockingSyncClient) FindMissing(ctx context.Context, names []string) ([]string, []string, error) {
	c.mu.Lock()
	c.findMissingCalls++
	c.startOnce.Do(func() { close(c.findMissingStarted) })
	c.mu.Unlock()

	select {
	case <-c.releaseFindMissing:
	case <-ctx.Done():
		c.cancelOnce.Do(func() { close(c.findMissingCancel) })
		return nil, nil, ctx.Err()
	}
	return append([]string(nil), names...), nil, nil
}

func (c *blockingSyncClient) BatchUpload(ctx context.Context, uploads []ace.BlobUpload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.batchUploadCalls++
	c.mu.Unlock()
	return nil
}

func (c *blockingSyncClient) CheckpointBlobs(ctx context.Context, previous string, added []string, deleted []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkpointCalls++
	return "checkpoint-1", nil
}

func (c *blockingSyncClient) CodebaseRetrieval(context.Context, string, ace.RetrievalOptions) (string, error) {
	return "retrieved", nil
}

func (c *blockingSyncClient) waitForFindMissing(t *testing.T) {
	t.Helper()
	select {
	case <-c.findMissingStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("find-missing did not start")
	}
}

func (c *blockingSyncClient) waitForSharedCancel(t *testing.T) {
	t.Helper()
	select {
	case <-c.findMissingCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("shared sync was not cancelled")
	}
}

func (c *blockingSyncClient) release() {
	c.releaseOnce.Do(func() { close(c.releaseFindMissing) })
}

func (c *blockingSyncClient) findMissingCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.findMissingCalls
}

func (c *blockingSyncClient) batchUploadCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.batchUploadCalls
}

func (c *blockingSyncClient) checkpointCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkpointCalls
}

func waitForInflightWaiters(t *testing.T, syncer *Syncer, root string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		syncer.mu.Lock()
		got := 0
		if call, ok := syncer.inflight[root]; ok {
			got = call.waiters
		}
		syncer.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("inflight waiters did not reach %d", want)
}

func waitForError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error")
		return nil
	}
}

func waitForResult(t *testing.T, ch <-chan Result) Result {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
		return Result{}
	}
}
