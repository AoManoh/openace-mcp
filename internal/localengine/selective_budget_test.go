package localengine

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

func siblingBudgetFixture(t *testing.T, dim int) (*embedServer, Options, string, int) {
	t.Helper()
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 1, "selective-sibling")
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFixture(t, root, fmt.Sprintf("doc%02d.txt", i), fmt.Sprintf("sibling paid content %d\n", i))
	}
	legacy, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.storeProfile = tfidfEraStoreProfile(legacy.storeProfile)
	if _, err := legacy.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "doc00.txt", "sibling paid content 0 revised\n")
	writeFixture(t, root, "doc01.txt", "sibling paid content 1 revised\n")
	if res, err := legacy.Sync(ctx, syncRequest(root)); err != nil || res.BuildMode != "delta" {
		t.Fatalf("legacy fixture must keep two segments: %+v err=%v", res, err)
	}
	rows, live := physicalRows(t, legacy, root)
	if rows != 8 || live != 6 {
		t.Fatalf("legacy fixture must hold 8 physical rows and 6 live chunks: rows=%d live=%d", rows, live)
	}
	calls := server.callCount()
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return server, opts, root, calls
}

func TestBudgetSiblingMigrationSharesLoadingBudget(t *testing.T) {
	const dim = 256
	ctx := context.Background()
	server, opts, root, legacyCalls := siblingBudgetFixture(t, dim)
	plan := func(e *Engine) (EmbedPlan, int, error) {
		callbacks := 0
		p, err := e.PlanEmbedJobs(ctx, engine.WorkspaceRef{DirectoryPath: root}, func(EmbedJob) error {
			callbacks++
			return nil
		})
		return p, callbacks, err
	}
	tight := opts
	tight.VectorMemoryBudget = 5 * dim * 4
	paused, err := New(tight)
	if err != nil {
		t.Fatal(err)
	}
	tightPlan, tightCallbacks, tightPlanErr := plan(paused)
	t.Logf("offline plan under 5-row budget: err=%v pending=%d callbacks=%d", tightPlanErr, tightPlan.Pending, tightCallbacks)
	if tightPlanErr == nil || !strings.Contains(tightPlanErr.Error(), EnvVectorMemoryBudget) || tightCallbacks != 0 || tightPlan.Pending != 0 {
		t.Fatalf("the offline plan must stop before exporting any paid key when the sibling vectors do not fit")
	}
	_, pauseErr := paused.Sync(ctx, syncRequest(root))
	docs := len(documentTexts(server, legacyCalls))
	t.Logf("migration under 5-row budget: err=%v document_texts=%d", pauseErr, docs)
	if pauseErr == nil || !strings.Contains(pauseErr.Error(), EnvVectorMemoryBudget) || docs != 0 {
		t.Fatalf("sibling paid vectors beyond the budget must pause the migration without requests")
	}
	if err := paused.Close(ctx); err != nil {
		t.Fatal(err)
	}
	fits := opts
	fits.VectorMemoryBudget = 7 * dim * 4
	resumed, err := New(fits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Close(ctx) }()
	fitPlan, fitCallbacks, fitPlanErr := plan(resumed)
	t.Logf("offline plan under 7-row budget: err=%v pending=%d callbacks=%d cross_profile_reusable=%d", fitPlanErr, fitPlan.Pending, fitCallbacks, fitPlan.CrossProfileReusable)
	if fitPlanErr != nil || fitCallbacks != 0 || fitPlan.Pending != 0 || fitPlan.CrossProfileReusable != 6 {
		t.Fatalf("the offline plan must reuse all six sibling vectors under the 7-row budget")
	}
	res, err := resumed.Sync(ctx, syncRequest(root))
	docs = len(documentTexts(server, legacyCalls))
	t.Logf("migration under 7-row budget: err=%v document_texts=%d coverage=%s cross_profile_reused=%d degraded=%q", err, docs, res.SemanticCoverage, res.CrossProfileReused, res.DegradedReason)
	if err != nil || docs != 0 || res.SemanticCoverage != "100%" || res.CrossProfileReused != 6 {
		t.Fatalf("raising the budget must finish the migration from sibling vectors without requests")
	}
}

func TestBudgetSelectiveLoadCountsUniqueKeys(t *testing.T) {
	const dim = 128
	ctx := context.Background()
	_, opts, root := compactionBudgetFixture(t, dim)
	compacted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := compacted.Sync(ctx, syncRequest(root)); err != nil || res.BuildMode == "delta" {
		t.Fatalf("fixture must compact into one segment: %+v err=%v", res, err)
	}
	active, store := loadActiveManifest(t, compacted, root)
	whole := compacted.loadPriorVectors(store, active, nil)
	needed := make(map[string]bool, len(whole.activeByHash))
	for key := range whole.activeByHash {
		needed[key] = true
	}
	duplicates := 0
	for key := range whole.olderByHash {
		if needed[key] {
			duplicates++
		}
	}
	whole.release()
	if err := compacted.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if duplicates == 0 {
		t.Fatalf("fixture must keep the needed keys in the previous revision as well")
	}
	for _, budgetRows := range []int{len(needed), 4 * len(needed)} {
		limited := opts
		limited.VectorMemoryBudget = int64(budgetRows) * dim * 4
		current, err := New(limited)
		if err != nil {
			t.Fatal(err)
		}
		active, store = loadActiveManifest(t, current, root)
		verified := current.loadPriorVectorsFor(store, active, needed, true)
		verifiedRows, verifiedUnloaded, verifiedOlder := verified.loadedRows, len(verified.unloaded), len(verified.olderByHash)
		verified.release()
		rowReader := current.loadPriorVectors(store, active, needed)
		readerRows, readerUnloaded, readerOlder := rowReader.loadedRows, len(rowReader.unloaded), len(rowReader.olderByHash)
		rowReader.release()
		if err := current.Close(ctx); err != nil {
			t.Fatal(err)
		}
		t.Logf("budget_rows=%d needed_keys=%d keys_also_in_previous=%d verified_loaded=%d verified_unloaded=%d verified_older=%d row_reader_loaded=%d row_reader_unloaded=%d row_reader_older=%d",
			budgetRows, len(needed), duplicates, verifiedRows, verifiedUnloaded, verifiedOlder, readerRows, readerUnloaded, readerOlder)
		if verifiedRows != len(needed) || verifiedUnloaded != 0 || verifiedOlder != 0 || readerRows != len(needed) || readerUnloaded != 0 || readerOlder != 0 {
			t.Fatalf("a key present in active and previous revisions must be loaded once")
		}
	}
}

func TestBudgetSelectiveLoadDetectsSameNormCorruption(t *testing.T) {
	const dim = 128
	ctx := context.Background()
	server, opts, root := compactionBudgetFixture(t, dim)
	probe, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	active, store := loadActiveManifest(t, probe, root)
	var segment = active.Segments[len(active.Segments)-1]
	segmentDir := store.SegmentPathFor(segment.ID)
	reader, err := vector.OpenRowReader(segmentDir, dim, segment.VectorsIndexChecksum)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := reader.Entries()[0].ContentHash
	_ = reader.Close()
	if err := probe.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(segmentDir, vector.DataFileName)
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	swapped := false
	for j := 0; j+1 < dim && !swapped; j++ {
		a := binary.LittleEndian.Uint32(raw[j*4:])
		b := binary.LittleEndian.Uint32(raw[(j+1)*4:])
		if a != b {
			binary.LittleEndian.PutUint32(raw[j*4:], b)
			binary.LittleEndian.PutUint32(raw[(j+1)*4:], a)
			swapped = true
		}
	}
	if !swapped {
		t.Fatal("fixture row must contain two different components")
	}
	if err := os.WriteFile(dataPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err = vector.OpenRowReader(segmentDir, dim, segment.VectorsIndexChecksum)
	if err != nil {
		t.Fatal(err)
	}
	_, rowErr := reader.ReadRow(0)
	_ = reader.Close()
	if rowErr != nil {
		t.Fatalf("the swap keeps the norm, so the per-row norm probe must accept it: %v", rowErr)
	}
	limited := opts
	limited.VectorMemoryBudget = 12 * dim * 4
	current, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	before := server.callCount()
	res, err := current.Sync(ctx, syncRequest(root))
	texts := documentTexts(server, before)
	second := 0
	for _, text := range texts {
		if strings.Contains(text, "second segment content") {
			second++
		}
	}
	t.Logf("budgeted rebuild after same-norm swap in segment %s (first key %s): err=%v build=%s coverage=%s document_texts=%d second_segment_texts=%d", segment.ID, firstKey, err, res.BuildMode, res.SemanticCoverage, len(texts), second)
	if err != nil || res.SemanticCoverage != "100%" || second != 5 || len(texts) != 6 {
		t.Fatalf("the corrupted segment must fail the whole-file checksum and its keys must be embedded again instead of reused or paused")
	}
}
