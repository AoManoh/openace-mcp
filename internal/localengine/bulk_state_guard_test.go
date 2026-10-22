package localengine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
)

func recordBulkJob(t *testing.T, provider *fakeBulkProvider, opts Options, root string) {
	t.Helper()
	provider.mu.Lock()
	provider.holdJobs = true
	provider.mu.Unlock()
	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := first.Sync(ctx, syncRequest(root)); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		provider.mu.Lock()
		jobs, polls := provider.jobsCreated, provider.polls
		provider.mu.Unlock()
		if jobs == 1 && polls > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("fixture did not persist the submitted job")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled setup should return an error")
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.holdJobs = false
	provider.mu.Unlock()
}

func TestBulkRecordedJobRecoveredDespiteBudgetThenNewInputBlocked(t *testing.T) {
	provider := newFakeBulkProvider(t, 8)
	cache := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cache)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "bulk-guard-budget")
	root := newFixtureWorkspace(t)
	opts := bulkOptions(provider.ts.URL, 8, 1)
	recordBulkJob(t, provider, opts, root)
	statePath := findBulkStatePath(t, cache)
	tight := opts
	tight.VectorMemoryBudget = 64
	tight.Embedding.BatchAPIMode = embedding.ProviderOff
	second, err := New(tight)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close(context.Background()) }()
	_, _, beforeSync := provider.counts()
	result, syncErr := second.Sync(context.Background(), syncRequest(root))
	uploads, jobs, afterSync := provider.counts()
	_, statErr := os.Stat(statePath)
	t.Logf("recovery under 64-byte budget: err=%v coverage=%s new_sync_requests=%d uploads=%d jobs=%d state_removed=%t", syncErr, result.SemanticCoverage, afterSync-beforeSync, uploads, jobs, os.IsNotExist(statErr))
	if syncErr != nil || result.SemanticCoverage != "100%" || afterSync != beforeSync || uploads != 1 || jobs != 1 || !os.IsNotExist(statErr) {
		t.Fatalf("recorded paid results must be persisted even when they exceed the budget for new requests")
	}
	writeFixture(t, root, "extra.go", "package main\n\nfunc ExtraAfterRecovery() {}\n")
	result, syncErr = second.Sync(context.Background(), syncRequest(root))
	uploads, jobs, finalSync := provider.counts()
	t.Logf("new input after recovery: err=%v coverage=%s new_sync_requests=%d uploads=%d jobs=%d", syncErr, result.SemanticCoverage, finalSync-afterSync, uploads, jobs)
	if syncErr != nil || finalSync != afterSync || uploads != 1 || jobs != 1 || result.SemanticCoverage == "100%" {
		t.Fatalf("after recovery the stored bytes must block new inputs")
	}
}

func TestBulkStateUnknownIdentityOrKeysPauses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(state map[string]any)
	}{
		{"missing-profile", func(s map[string]any) { delete(s, "profile_hash") }},
		{"empty-job-id", func(s map[string]any) { s["jobs"].([]any)[0].(map[string]any)["id"] = "" }},
		{"missing-keys", func(s map[string]any) { delete(s["jobs"].([]any)[0].(map[string]any), "keys") }},
		{"null-keys", func(s map[string]any) { s["jobs"].([]any)[0].(map[string]any)["keys"] = nil }},
		{"invalid-keys", func(s map[string]any) { s["jobs"].([]any)[0].(map[string]any)["keys"] = []any{"not-an-embed-key"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newFakeBulkProvider(t, 8)
			cache := t.TempDir()
			t.Setenv("OPENACE_CACHE_DIR", cache)
			t.Setenv("OPENACE_CACHE_NAMESPACE", "bulk-guard-identity-"+tc.name)
			root := newFixtureWorkspace(t)
			opts := bulkOptions(provider.ts.URL, 8, 1)
			recordBulkJob(t, provider, opts, root)
			statePath := findBulkStatePath(t, cache)
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			tc.mutate(state)
			mutated, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			second, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = second.Close(context.Background()) }()
			provider.mu.Lock()
			pollsBefore := provider.polls
			provider.mu.Unlock()
			_, _, beforeSync := provider.counts()
			_, syncErr := second.Sync(context.Background(), syncRequest(root))
			uploads, jobs, afterSync := provider.counts()
			provider.mu.Lock()
			pollsAfter := provider.polls
			provider.mu.Unlock()
			kept, _ := os.ReadFile(statePath)
			t.Logf("%s: err=%v new_polls=%d new_sync_requests=%d uploads=%d jobs=%d state_unchanged=%t", tc.name, syncErr, pollsAfter-pollsBefore, afterSync-beforeSync, uploads, jobs, string(kept) == string(mutated))
			if syncErr == nil || pollsAfter != pollsBefore || afterSync != beforeSync || uploads != 1 || jobs != 1 || string(kept) != string(mutated) {
				t.Fatalf("unknown identity or unusable job keys must pause before polling or new requests and keep the state bytes")
			}
		})
	}
}

func TestBulkStateCorruptOrForeignBlocksSyncPath(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"corrupt", "{not json", "批作业状态损坏"},
		{"foreign-profile", `{"profile_hash":"other-profile","jobs":[]}`, "属于其他嵌入身份"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newFakeBulkProvider(t, 8)
			provider.mu.Lock()
			provider.failCreate = true
			provider.mu.Unlock()
			cache := t.TempDir()
			t.Setenv("OPENACE_CACHE_DIR", cache)
			t.Setenv("OPENACE_CACHE_NAMESPACE", "bulk-guard-"+tc.name)
			root := newFixtureWorkspace(t)
			opts := bulkOptions(provider.ts.URL, 8, 1)
			first, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.Sync(context.Background(), syncRequest(root)); err == nil {
				t.Fatal("fixture must leave a state file")
			}
			if err := first.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			statePath := findBulkStatePath(t, cache)
			if err := os.WriteFile(statePath, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			off := opts
			off.Embedding.BatchAPIMode = embedding.ProviderOff
			second, err := New(off)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = second.Close(context.Background()) }()
			_, _, beforeSync := provider.counts()
			_, syncErr := second.Sync(context.Background(), syncRequest(root))
			uploads, _, afterSync := provider.counts()
			kept, _ := os.ReadFile(statePath)
			t.Logf("%s: err=%v new_sync_requests=%d uploads=%d state_kept=%t", tc.name, syncErr, afterSync-beforeSync, uploads, string(kept) == tc.content)
			if syncErr == nil || !strings.Contains(syncErr.Error(), tc.want) || afterSync != beforeSync || string(kept) != tc.content {
				t.Fatalf("unreadable or foreign Batch state must block new sync requests and stay on disk")
			}
		})
	}
}

func TestBulkFirstSubmissionBudgetStopsBeforeUpload(t *testing.T) {
	provider := newFakeBulkProvider(t, 8)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "bulk-guard-first-submission")
	root := newFixtureWorkspace(t)
	opts := bulkOptions(provider.ts.URL, 8, 1)
	opts.VectorMemoryBudget = 6 * 8 * 4
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	for _, label := range []string{"first", "repeat"} {
		result, syncErr := e.Sync(context.Background(), syncRequest(root))
		uploads, jobs, syncEmbeds := provider.counts()
		t.Logf("%s: err=%v coverage=%s uploads=%d jobs=%d sync_embeds=%d", label, syncErr, result.SemanticCoverage, uploads, jobs, syncEmbeds)
		if syncErr != nil || uploads != 0 || jobs != 0 || syncEmbeds != 0 || result.SemanticCoverage == "100%" {
			t.Fatalf("journal bytes over budget must stop before any upload, job or sync request")
		}
	}
}

func TestBulkRecordedJobRecoveredBeforeUnloadedPause(t *testing.T) {
	provider := newFakeBulkProvider(t, 8)
	cache := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cache)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "bulk-guard-unloaded")
	root := t.TempDir()
	original := "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n\nfunc Gamma() int {\n\treturn 3\n}\n"
	writeFixture(t, root, "multi.go", original)
	writeFixture(t, root, "notes.txt", "recovery before pause notes\n")
	opts := bulkOptions(provider.ts.URL, 8, 1)
	syncOnly := opts
	syncOnly.Embedding.BatchAPIMode = embedding.ProviderOff
	first, err := New(syncOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "multi.go", strings.Replace(original, "return 3", "return 30", 1))
	recordBulkJob(t, provider, opts, root)
	statePath := findBulkStatePath(t, cache)
	tight := syncOnly
	tight.VectorMemoryBudget = 8 * 4
	second, err := New(tight)
	if err != nil {
		t.Fatal(err)
	}
	uploadsBefore, jobsBefore, syncBefore := provider.counts()
	_, pauseErr := second.Sync(context.Background(), syncRequest(root))
	uploads, jobs, syncAfter := provider.counts()
	_, statErr := os.Stat(statePath)
	t.Logf("one-row budget with a recorded job: err=%v uploads=%d->%d jobs=%d->%d new_sync_requests=%d state_removed=%t", pauseErr, uploadsBefore, uploads, jobsBefore, jobs, syncAfter-syncBefore, os.IsNotExist(statErr))
	if pauseErr == nil || !strings.Contains(pauseErr.Error(), EnvVectorMemoryBudget) || uploads != uploadsBefore || jobs != jobsBefore || syncAfter != syncBefore || !os.IsNotExist(statErr) {
		t.Fatalf("the recorded paid job must be recovered and its state removed before the build pauses for unloaded vectors, without new requests")
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	raised := syncOnly
	raised.VectorMemoryBudget = 1 << 20
	third, err := New(raised)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close(context.Background()) }()
	result, err := third.Sync(context.Background(), syncRequest(root))
	finalUploads, finalJobs, finalSync := provider.counts()
	t.Logf("after raising budget: err=%v coverage=%s uploads=%d jobs=%d new_sync_requests=%d", err, result.SemanticCoverage, finalUploads, finalJobs, finalSync-syncAfter)
	if err != nil || result.SemanticCoverage != "100%" || finalUploads != uploads || finalJobs != jobs || finalSync != syncAfter {
		t.Fatalf("the recovered result must be reused after raising the budget without any new request")
	}
}
