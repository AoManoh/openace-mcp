package localengine

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

func physicalRows(t *testing.T, e *Engine, root string) (int, int) {
	t.Helper()
	m, _ := loadActiveManifest(t, e, root)
	rows := 0
	for _, segment := range m.Segments {
		rows += segment.VectorCount
	}
	return rows, m.Counts.Chunks
}

func budgetSync(t *testing.T, e *Engine, server *embedServer, root string, label string) (int, string) {
	t.Helper()
	before := server.callCount()
	res, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	docs := len(documentTexts(server, before))
	rows, live := physicalRows(t, e, root)
	t.Logf("%s: document_texts=%d coverage=%s reason=%q build=%s physical_rows=%d live_chunks=%d", label, docs, res.SemanticCoverage, res.DegradedReason, res.BuildMode, rows, live)
	return docs, res.SemanticCoverage
}

func TestBudgetDeltaCountsRetainedPhysicalRows(t *testing.T) {
	server := newEmbedServer(t, 128)
	opts := embedOptions(server.ts.URL, 128, 1, "budget-model")
	opts.VectorMemoryBudget = 2048
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFixture(t, root, name, "first build content "+name+"\n")
	}
	if docs, coverage := budgetSync(t, e, server, root, "first build"); docs != 3 || coverage != "100%" {
		t.Fatalf("three rows fit in four: docs=%d coverage=%s", docs, coverage)
	}
	writeFixture(t, root, "d.txt", "added content d\n")
	writeFixture(t, root, "e.txt", "added content e\n")
	if docs, coverage := budgetSync(t, e, server, root, "delta adds two"); docs != 0 || coverage == "100%" {
		t.Fatalf("3 retained + 2 possible rows exceed 4 rows: docs=%d coverage=%s", docs, coverage)
	}
	if docs, _ := budgetSync(t, e, server, root, "repeat"); docs != 0 {
		t.Fatalf("repeat sync must not request: %d", docs)
	}
}

func TestBudgetRewriteCountsDeadRows(t *testing.T) {
	server := newEmbedServer(t, 128)
	opts := embedOptions(server.ts.URL, 128, 1, "budget-model")
	opts.VectorMemoryBudget = 2048
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	writeFixture(t, root, "one.txt", "first file\n")
	writeFixture(t, root, "two.txt", "second file\n")
	writeFixture(t, root, "decl.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n")
	docs, coverage := budgetSync(t, e, server, root, "first build")
	before, _ := physicalRows(t, e, root)
	if docs != 3 || coverage != "100%" || before != 3 {
		t.Fatalf("fixture must start with 3 rows: docs=%d coverage=%s rows=%d", docs, coverage, before)
	}
	writeFixture(t, root, "decl.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n")
	docs, _ = budgetSync(t, e, server, root, "rewrite with two declarations")
	m, _ := loadActiveManifest(t, e, root)
	newChunks := m.Files["decl.go"].ChunkCount
	t.Logf("retained_physical_rows=%d new_possible_rows=%d live_rows_after=%d", before, newChunks, m.Counts.Chunks)
	if newChunks < 2 || before+newChunks <= 4 {
		t.Fatalf("fixture must reach more than 4 physical rows: retained=%d new=%d", before, newChunks)
	}
	if docs != 0 {
		t.Fatalf("retained dead rows plus new rows exceed the budget, document requests must be 0: %d", docs)
	}
}

func TestBudgetJournalEncodedBytes(t *testing.T) {
	server := newEmbedServer(t, 8)
	opts := embedOptions(server.ts.URL, 8, 16, "budget-model")
	opts.VectorMemoryBudget = 128
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFixture(t, root, name, "journal content "+name+"\n")
	}
	if docs, coverage := budgetSync(t, e, server, root, "first build"); docs != 0 || coverage == "100%" {
		t.Fatalf("3 journal records need 330 bytes > 128: docs=%d coverage=%s", docs, coverage)
	}
	if docs, _ := budgetSync(t, e, server, root, "repeat"); docs != 0 {
		t.Fatalf("repeat sync must not request: %d", docs)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close(context.Background()) }()
	if docs, _ := budgetSync(t, restarted, server, root, "after restart"); docs != 0 {
		t.Fatalf("restart must not request: %d", docs)
	}
}

func TestBudgetBoundaries(t *testing.T) {
	const dim = 8
	rowBytes := int64(dim * 4)
	recordBytes := int64(14 + 64 + 4*dim)
	cases := []struct {
		name     string
		budget   int64
		wantDocs int
	}{
		{"exactly-one-record", recordBytes, 1},
		{"one-byte-short-of-record", recordBytes - 1, 0},
		{"exactly-one-row", rowBytes, 0},
		{"below-one-row", rowBytes - 1, 0},
		{"sufficient", 1 << 20, 1},
		{"max-int64", math.MaxInt64, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newEmbedServer(t, dim)
			opts := embedOptions(server.ts.URL, dim, 16, "budget-model")
			opts.VectorMemoryBudget = tc.budget
			e := newTestEngineWith(t, opts)
			root := t.TempDir()
			writeFixture(t, root, "single.txt", "single chunk content\n")
			docs, _ := budgetSync(t, e, server, root, tc.name)
			if docs != tc.wantDocs {
				t.Fatalf("budget=%d: document texts %d, want %d", tc.budget, docs, tc.wantDocs)
			}
			if docs, _ := budgetSync(t, e, server, root, tc.name+" repeat"); docs != 0 {
				t.Fatalf("repeat sync must not request again: %d", docs)
			}
		})
	}
}

func TestBudgetBelowOneRowRefusesExistingVectorsBeforeReading(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 16, "budget-model")
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifest, store := loadActiveManifest(t, e, root)
	dataPath := filepath.Join(store.SegmentPath(manifest), vector.DataFileName)
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xFF
	if err := os.WriteFile(dataPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, deny := range []bool{false, true} {
		limited := opts
		limited.VectorMemoryBudget = int64(dim*4) - 1
		if deny {
			limited.RetrievalDegrade = DegradeDeny
		}
		current, err := New(limited)
		if err != nil {
			t.Fatal(err)
		}
		before := server.callCount()
		res, searchErr := current.Search(ctx, searchRequest(root, "HandleLogin"))
		t.Logf("deny=%t err=%v mode=%s reason=%q document_texts=%d", deny, searchErr, res.RetrievalMode, res.DegradedReason, len(documentTexts(server, before)))
		if deny {
			if searchErr == nil || !strings.Contains(searchErr.Error(), "vector-envelope-exceeded") {
				t.Fatalf("deny must reject the sub-row budget before reading vectors: %v", searchErr)
			}
		} else if searchErr != nil || !strings.Contains(res.DegradedReason, "vector-envelope-exceeded") || strings.Contains(res.DegradedReason, "vector-data-unavailable") || res.RetrievalMode != "lexical" {
			t.Fatalf("allow must keep lexical results and refuse vectors before reading them: %+v err=%v", res, searchErr)
		}
		if docs := len(documentTexts(server, before)); docs != 0 {
			t.Fatalf("sub-row budget must not request documents: %d", docs)
		}
		if err := current.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBudgetParallelBatchesAllBlocked(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 1, "budget-model")
	opts.VectorMemoryBudget = 12 * dim * 4
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFixture(t, root, fmt.Sprintf("file%02d.txt", i), fmt.Sprintf("parallel batch content %d\n", i))
	}
	if docs, coverage := budgetSync(t, e, server, root, "ten single-key batches"); docs != 0 || coverage == "100%" {
		t.Fatalf("10 journal records need 1100 bytes > 384: docs=%d coverage=%s", docs, coverage)
	}
}

func TestBudgetRaisedAfterRefusalRequestsOnlyMissingOnce(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 16, "budget-model")
	opts.VectorMemoryBudget = 128
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFixture(t, root, name, "raise content "+name+"\n")
	}
	if docs, _ := budgetSync(t, e, server, root, "refused"); docs != 0 {
		t.Fatalf("refused build must not request: %d", docs)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	raised := opts
	raised.VectorMemoryBudget = 1 << 20
	current, err := New(raised)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	if docs, coverage := budgetSync(t, current, server, root, "raised"); docs != 3 || coverage != "100%" {
		t.Fatalf("raised budget must request each missing key once: docs=%d coverage=%s", docs, coverage)
	}
	if docs, _ := budgetSync(t, current, server, root, "raised repeat"); docs != 0 {
		t.Fatalf("existing vectors must be reused: %d", docs)
	}
}

func compactionBudgetFixture(t *testing.T, dim int) (*embedServer, Options, string) {
	t.Helper()
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 1, "budget-model")
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeFixture(t, root, fmt.Sprintf("first%02d.txt", i), fmt.Sprintf("first segment content %d\n", i))
	}
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		writeFixture(t, root, fmt.Sprintf("second%02d.txt", i), fmt.Sprintf("second segment content %d\n", i))
	}
	if res, err := e.Sync(ctx, syncRequest(root)); err != nil || res.BuildMode != "delta" {
		t.Fatalf("fixture must add a delta segment: %+v err=%v", res, err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 1, 2, 3, 4} {
		if err := os.Remove(filepath.Join(root, fmt.Sprintf("first%02d.txt", i))); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, root, "fresh.txt", "fresh compaction content\n")
	return server, opts, root
}

func TestBudgetCompactionReusesAllPaidVectors(t *testing.T) {
	const dim = 128
	ctx := context.Background()
	server, opts, root := compactionBudgetFixture(t, dim)
	limited := opts
	limited.VectorMemoryBudget = 6 * dim * 4
	current, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	active, _ := loadActiveManifest(t, current, root)
	before := server.callCount()
	res, syncErr := current.Sync(ctx, syncRequest(root))
	after, _ := loadActiveManifest(t, current, root)
	docs := documentTexts(server, before)
	rows, live := physicalRows(t, current, root)
	t.Logf("compaction under 6-row budget: err=%v build=%s coverage=%s document_texts=%d physical_rows=%d live_chunks=%d revision_changed=%t", syncErr, res.BuildMode, res.SemanticCoverage, len(docs), rows, live, after.Revision != active.Revision)
	if syncErr != nil || res.SemanticCoverage != "100%" || len(docs) != 1 || !strings.Contains(docs[0], "fresh compaction content") || after.Revision == active.Revision || rows != 6 {
		t.Fatalf("live paid vectors that fit the budget must be reused while dead rows are dropped; only the new file may be requested")
	}
	hits, searchErr := current.Search(ctx, searchRequest(root, "fresh compaction content"))
	found := false
	for _, hit := range hits.Hits {
		found = found || hit.Path == "fresh.txt"
	}
	t.Logf("search for the new file: err=%v mode=%s reason=%q hits=%d found=%t", searchErr, hits.RetrievalMode, hits.DegradedReason, len(hits.Hits), found)
	if searchErr != nil || !found || hits.DegradedReason != "" {
		t.Fatalf("the new file must be searchable after the budgeted compaction")
	}
	if docs, coverage := budgetSync(t, current, server, root, "repeat after compaction"); docs != 0 || coverage != "100%" {
		t.Fatalf("repeat sync must not request again: docs=%d coverage=%s", docs, coverage)
	}
}

func TestBudgetCompactionPausesWhenNeededPaidVectorsExceedBudget(t *testing.T) {
	const dim = 128
	ctx := context.Background()
	server, opts, root := compactionBudgetFixture(t, dim)
	limited := opts
	limited.VectorMemoryBudget = 4 * dim * 4
	current, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	active, _ := loadActiveManifest(t, current, root)
	before := server.callCount()
	_, pauseErr := current.Sync(ctx, syncRequest(root))
	after, _ := loadActiveManifest(t, current, root)
	docs := len(documentTexts(server, before))
	t.Logf("compaction needing 5 paid vectors under 4-row budget: err=%v document_texts=%d revision_unchanged=%t", pauseErr, docs, after.Revision == active.Revision)
	if pauseErr == nil || !strings.Contains(pauseErr.Error(), EnvVectorMemoryBudget) || docs != 0 || after.Revision != active.Revision {
		t.Fatalf("a rebuild whose needed paid vectors exceed the budget must pause without requests and keep the served revision")
	}
	status, statusErr := current.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: root})
	t.Logf("workspace_status after pause: stage=%s last_error=%q err=%v", status.Stage, status.LastError, statusErr)
	if statusErr != nil || !strings.Contains(status.LastError, EnvVectorMemoryBudget) {
		t.Fatalf("workspace_status must report the pause reason")
	}
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
	denyOpts := limited
	denyOpts.RetrievalDegrade = DegradeDeny
	denied, err := New(denyOpts)
	if err != nil {
		t.Fatal(err)
	}
	_, searchErr := denied.Search(ctx, searchRequest(root, "fresh compaction content"))
	docs = len(documentTexts(server, before))
	t.Logf("deny search while paused: err=%v document_texts=%d", searchErr, docs)
	if searchErr == nil || !strings.Contains(searchErr.Error(), EnvVectorMemoryBudget) || docs != 0 {
		t.Fatalf("deny search must return the pause reason without requests")
	}
	if err := denied.Close(ctx); err != nil {
		t.Fatal(err)
	}
	raised := opts
	raised.VectorMemoryBudget = 6 * dim * 4
	resumed, err := New(raised)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Close(ctx) }()
	before = server.callCount()
	res, err := resumed.Sync(ctx, syncRequest(root))
	texts := documentTexts(server, before)
	t.Logf("compaction after raising budget to 6 rows: err=%v coverage=%s document_texts=%d", err, res.SemanticCoverage, len(texts))
	if err != nil || res.SemanticCoverage != "100%" || len(texts) != 1 || !strings.Contains(texts[0], "fresh compaction content") {
		t.Fatalf("raising the budget must resume and request only the new file")
	}
}

func TestBudgetBuildReuseRespectsLoadingBudget(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	t.Setenv("OPENACE_VECTOR_MMAP", "off")
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 16, "budget-model")
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	writeFixture(t, root, "one.txt", "loading budget first file\n")
	writeFixture(t, root, "two.txt", "loading budget second file\n")
	writeFixture(t, root, "multi.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n\nfunc Gamma() int {\n\treturn 3\n}\n")
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	limited := opts
	limited.VectorMemoryBudget = dim * 4
	current, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	active, store := loadActiveManifest(t, current, root)
	rows, _ := physicalRows(t, current, root)
	prior := current.loadPriorVectors(store, active, nil)
	loaded, unloaded := prior.loadedRows, len(prior.unloaded)
	prior.release()
	t.Logf("budget_rows=1 previous_rows=%d loaded_rows=%d unloaded_keys=%d", rows, loaded, unloaded)
	if loaded > 1 || unloaded != rows {
		t.Fatalf("build reuse must stay within the one-row budget and record every unloaded key: loaded=%d unloaded=%d rows=%d", loaded, unloaded, rows)
	}
	writeFixture(t, root, "multi.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n\nfunc Gamma() int {\n\treturn 30\n}\n")
	before := server.callCount()
	_, pauseErr := current.Sync(ctx, syncRequest(root))
	after, _ := loadActiveManifest(t, current, root)
	docs := documentTexts(server, before)
	t.Logf("edit under one-row budget: err=%v document_texts=%d revision_unchanged=%t", pauseErr, len(docs), after.Revision == active.Revision)
	if pauseErr == nil || !strings.Contains(pauseErr.Error(), EnvVectorMemoryBudget) || len(docs) != 0 || after.Revision != active.Revision {
		t.Fatalf("unchanged paid chunks that cannot be loaded must pause the build instead of being requested again")
	}
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
	raised := opts
	raised.VectorMemoryBudget = 1 << 20
	resumed, err := New(raised)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Close(ctx) }()
	before = server.callCount()
	res, err := resumed.Sync(ctx, syncRequest(root))
	docs = documentTexts(server, before)
	t.Logf("after raising budget: err=%v coverage=%s document_texts=%d", err, res.SemanticCoverage, len(docs))
	if err != nil || res.SemanticCoverage != "100%" || len(docs) == 0 {
		t.Fatalf("raised budget must finish the build: %+v err=%v", res, err)
	}
	for _, text := range docs {
		if strings.Contains(text, "Alpha") || strings.Contains(text, "Beta") {
			t.Fatalf("unchanged paid chunks must be reused, not requested again: %q", text)
		}
	}
}
