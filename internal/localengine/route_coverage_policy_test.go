package localengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

func routeIDs(refs []CandidateRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids
}

func TestSearchRoutesCoverageGapPolicy(t *testing.T) {
	partial := func(text string) bool { return strings.Contains(text, "util.py") }
	none := func(string) bool { return true }
	deny := func(o *Options) { o.RetrievalDegrade = DegradeDeny }
	strict := func(o *Options) { o.QualityStrict = true }
	cases := []struct {
		name    string
		zero    func(string) bool
		mutate  func(*Options)
		wantEnv string
	}{
		{"partial-allow", partial, nil, ""},
		{"partial-deny", partial, deny, EnvRetrievalDegrade},
		{"partial-strict", partial, strict, EnvQualityStrict},
		{"zero-allow", none, nil, ""},
		{"zero-deny", none, deny, EnvRetrievalDegrade},
		{"zero-strict", none, strict, EnvQualityStrict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			server := newEmbedServer(t, 8)
			server.zeroWhen = tc.zero
			opts := embedOptions(server.ts.URL, 8, 16, "routes-model")
			if tc.mutate != nil {
				tc.mutate(&opts)
			}
			e := newTestEngineWith(t, opts)
			root := newFixtureWorkspace(t)
			synced, _ := e.Sync(ctx, syncRequest(root))
			if synced.IndexRevision == "" || synced.SemanticCoverage == "100%" {
				t.Fatalf("fixture must publish a revision with a coverage gap: %+v", synced)
			}
			before := server.callCount()
			routes, err := e.SearchRoutes(ctx, searchRequest(root, "HandleLogin"), 60)
			queries := len(queryTexts(server, before))
			t.Logf("%s coverage=%s err=%v reasons=%v lex=%v dense=%v query_embeds=%d", tc.name, synced.SemanticCoverage, err, routes.Reasons, routeIDs(routes.Lex), routeIDs(routes.Dense), queries)
			if tc.wantEnv != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEnv) || !strings.Contains(err.Error(), "semantic-coverage-partial") || queries != 0 {
					t.Fatalf("known coverage gap must be rejected before query embedding: err=%v query_embeds=%d", err, queries)
				}
				return
			}
			if err != nil || strings.Join(routes.Reasons, ",") != "semantic-coverage-partial" || len(routes.Lex) == 0 {
				t.Fatalf("allow must return candidates and report the coverage gap: routes=%+v err=%v", routes, err)
			}
			if tc.name == "zero-allow" && (routes.Dense != nil || queries != 0) {
				t.Fatalf("zero coverage must not run the dense route: dense=%v query_embeds=%d", routes.Dense, queries)
			}
			if tc.name == "partial-allow" && (len(routes.Dense) == 0 || queries != 1) {
				t.Fatalf("partial coverage must still run the dense route once: dense=%v query_embeds=%d", routes.Dense, queries)
			}
		})
	}
}

func TestSearchRoutesMigrationBlockPolicy(t *testing.T) {
	modes := []struct {
		name    string
		mutate  func(o *Options)
		wantEnv string
	}{
		{"allow", func(o *Options) {}, ""},
		{"deny", func(o *Options) { o.RetrievalDegrade = DegradeDeny }, EnvRetrievalDegrade},
		{"strict", func(o *Options) { o.QualityStrict = true }, EnvQualityStrict},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			f := newGuardFixture(t, "", "", "parse_config")
			writeRawBulkJob(t, f, true, "null", nil)
			opts := f.opts
			mode.mutate(&opts)
			current, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close(ctx) }()
			synced, _ := current.Sync(ctx, syncRequest(f.root))
			if !strings.Contains(synced.DegradedReason, migrationBlockedReason) {
				t.Fatalf("fixture must publish a migration-blocked revision: %+v", synced)
			}
			before := f.server.callCount()
			routes, err := current.SearchRoutes(ctx, searchRequest(f.root, "HandleLogin"), 60)
			queries := len(queryTexts(f.server, before))
			t.Logf("%s err=%v reasons=%v query_embeds=%d", mode.name, err, routes.Reasons, queries)
			if mode.wantEnv != "" {
				if err == nil || !strings.Contains(err.Error(), mode.wantEnv) || !strings.Contains(err.Error(), migrationBlockedReason) || queries != 0 {
					t.Fatalf("migration block must be rejected before query embedding: err=%v query_embeds=%d", err, queries)
				}
				return
			}
			if err != nil || !strings.Contains(strings.Join(routes.Reasons, ","), migrationBlockedReason) {
				t.Fatalf("allow must report the migration block: routes=%+v err=%v", routes, err)
			}
		})
	}
}

func TestSearchRoutesWithoutCoverageGaps(t *testing.T) {
	ctx := context.Background()
	lexical := newTestEngineWith(t, Options{RetrievalDegrade: DegradeDeny, RerankDegrade: DegradeDeny})
	lexicalRoot := newFixtureWorkspace(t)
	routes, err := lexical.SearchRoutes(ctx, searchRequest(lexicalRoot, "HandleLogin"), 10)
	if err != nil || routes.Dense != nil || len(routes.Reasons) != 0 || len(routes.Lex) == 0 {
		t.Fatalf("lexical-only engine under deny: routes=%+v err=%v", routes, err)
	}
	for _, withRerank := range []bool{false, true} {
		server := newEmbedServer(t, 8)
		opts := embedOptions(server.ts.URL, 8, 16, "routes-model")
		opts.QualityStrict = true
		if withRerank {
			rr := newRerankServer(t, func(string) float64 { return 0.5 }, nil)
			defer rr.Close()
			opts.Rerank = rerankOptions(rr.URL)
		}
		e := newTestEngineWith(t, opts)
		root := newFixtureWorkspace(t)
		before := server.callCount()
		routes, err := e.SearchRoutes(ctx, searchRequest(root, "HandleLogin"), 10)
		t.Logf("rerank_configured=%t err=%v reasons=%v dense=%d query_embeds=%d", withRerank, err, routes.Reasons, len(routes.Dense), len(queryTexts(server, before)))
		if err != nil || len(routes.Reasons) != 0 || len(routes.Dense) == 0 {
			t.Fatalf("complete coverage under strict must not require rerank: routes=%+v err=%v", routes, err)
		}
	}
}

func TestSearchRoutesFailureBehaviorUnchanged(t *testing.T) {
	for _, deny := range []bool{false, true} {
		ctx := context.Background()
		server := newEmbedServer(t, 8)
		opts := embedOptions(server.ts.URL, 8, 16, "routes-model")
		if deny {
			opts.RetrievalDegrade = DegradeDeny
		}
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
		routes, err := e.SearchRoutes(ctx, searchRequest(root, "HandleLogin"), 10)
		t.Logf("vector-corruption deny=%t err=%v reasons=%v lex=%d", deny, err, routes.Reasons, len(routes.Lex))
		if deny {
			if err == nil || !strings.Contains(err.Error(), EnvRetrievalDegrade) || !strings.Contains(err.Error(), "vector-data-unavailable") {
				t.Fatalf("deny must reject unavailable vectors: %v", err)
			}
		} else if err != nil || strings.Join(routes.Reasons, ",") != "vector-data-unavailable" || routes.Dense != nil || len(routes.Lex) == 0 {
			t.Fatalf("allow must keep lexical candidates and report unavailable vectors: routes=%+v err=%v", routes, err)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := e.SearchRoutes(cancelled, searchRequest(root, "HandleLogin"), 10); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled context must be returned as is: %v", err)
		}
		missing := filepath.Join(t.TempDir(), "missing")
		if _, err := e.SearchRoutes(ctx, searchRequest(missing, "HandleLogin"), 10); err == nil || !engine.IsInvalidRequest(err) {
			t.Fatalf("sync failure must be returned as an error: %v", err)
		}
	}
}
