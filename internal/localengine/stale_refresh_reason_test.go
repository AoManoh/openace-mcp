package localengine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

func TestFirstTouchAfterFailedRefreshReportsStaleIndex(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	t.Setenv("OPENACE_VECTOR_MMAP", "off")
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 16, "stale-model")
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	original := "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n\nfunc Gamma() int {\n\treturn 3\n}\n"
	writeFixture(t, root, "multi.go", original)
	writeFixture(t, root, "notes.txt", "stale refresh notes\n")
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "multi.go", strings.Replace(original, "return 3", "return 30", 1))
	limited := opts
	limited.VectorMemoryBudget = dim * 4
	current, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	first, err := current.Search(ctx, searchRequest(root, "Gamma"))
	if err != nil || !strings.Contains(first.DegradedReason, "index-refreshing") {
		t.Fatalf("first touch without any finished refresh must report index-refreshing: %+v err=%v", first.DegradedReason, err)
	}
	_, workspaceKey, err := current.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	waitFailed := func(label string) engine.WorkspaceStatus {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, running := current.firstTouchKicks.Load(workspaceKey)
			status, statusErr := current.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: root})
			if !running && statusErr == nil && status.Stage == engine.IndexStageFailed && !status.InFlight {
				return status
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not finish with a failure: %+v err=%v", label, status, statusErr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	failed := waitFailed("background refresh started by the first query")
	second, err := current.Search(ctx, searchRequest(root, "Gamma"))
	afterQuery, _ := current.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: root})
	banner := strings.SplitN(second.Text, "\n", 2)[0]
	reasonSegment := strings.TrimPrefix(strings.SplitN(banner, "; ", 2)[0], "[DEGRADED] ")
	t.Logf("second query reason=%q banner_reason_segment=%q last_error_before_query=%q status_right_after_query: stage=%s in_flight=%t last_error=%q", second.DegradedReason, reasonSegment, failed.LastError, afterQuery.Stage, afterQuery.InFlight, afterQuery.LastError)
	if err != nil || !strings.Contains(second.DegradedReason, "stale-index") || strings.Contains(second.DegradedReason, "stale-index(") || strings.Contains(second.DegradedReason, "index-refreshing") {
		t.Fatalf("after a failed refresh allow must report the plain stale-index token: %q err=%v", second.DegradedReason, err)
	}
	if !strings.HasPrefix(banner, "[DEGRADED] ") || reasonSegment != second.DegradedReason {
		t.Fatalf("the banner reason segment must equal degraded_reason: banner=%q reason=%q", banner, second.DegradedReason)
	}
	if !strings.Contains(failed.LastError, EnvVectorMemoryBudget) {
		t.Fatalf("workspace_status.last_error must keep the failure text: %q", failed.LastError)
	}
	again := waitFailed("background refresh started by the second query")
	repeated, err := current.Search(ctx, searchRequest(root, "Gamma"))
	t.Logf("third query reason=%q last_error_after_second_attempt=%q", repeated.DegradedReason, again.LastError)
	if err != nil || repeated.DegradedReason != second.DegradedReason || !strings.Contains(again.LastError, EnvVectorMemoryBudget) {
		t.Fatalf("repeated queries must keep the plain stale-index token while last_error is written again after each failed attempt: %q err=%v", repeated.DegradedReason, err)
	}
	waitFailed("background refresh started by the third query")
	writeFixture(t, root, "multi.go", original)
	if _, err := current.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("reverting the edit must let the refresh succeed: %v", err)
	}
	third, err := current.Search(ctx, searchRequest(root, "Gamma"))
	if err != nil || strings.Contains(third.DegradedReason, "stale-index") || strings.Contains(third.DegradedReason, "index-refreshing") {
		t.Fatalf("a successful sync must clear the stale-index reason: %q err=%v", third.DegradedReason, err)
	}
}
