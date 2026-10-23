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

// TestSearchRoutesCoverageGapPolicy：部分 chunk 被 provider 永久拒绝时默认
// 返回两路候选并写明 semantic-coverage-partial，strict 在查询嵌入前拒绝；
// 全部 chunk 都没有向量时总是在查询嵌入前拒绝（semantic-index-empty），不返回
// 只有词法路的候选。
func TestSearchRoutesCoverageGapPolicy(t *testing.T) {
	// 部分覆盖：util.py 的片段（parse_config）嵌入请求失败，批大小 1。被拒绝
	// （零向量）的片段不计入覆盖率分母，只用来制造“全部没有向量”。
	partial := func(texts []string) bool { return len(texts) == 1 && strings.Contains(texts[0], "parse_config") }
	none := func(string) bool { return true }
	strict := func(o *Options) { o.QualityStrict = true }
	cases := []struct {
		name       string
		fail       func([]string) bool
		zero       func(string) bool
		mutate     func(*Options)
		wantReject string
		wantReason string
	}{
		{"partial-default", partial, nil, nil, "", ""},
		{"partial-strict", partial, nil, strict, EnvQualityStrict, "semantic-coverage-partial"},
		{"zero-default", nil, none, nil, "semantic retrieval unavailable", semanticIndexEmptyReason},
		{"zero-strict", nil, none, strict, "semantic retrieval unavailable", semanticIndexEmptyReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			server := newEmbedServer(t, 8)
			batch := 16
			if tc.fail != nil {
				server.setFailWhen(tc.fail)
				batch = 1
			}
			server.zeroWhen = tc.zero
			opts := embedOptions(server.ts.URL, 8, batch, "routes-model")
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
			if tc.wantReject != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantReject) || !strings.Contains(err.Error(), tc.wantReason) || queries != 0 {
					t.Fatalf("known gap must be rejected before query embedding: err=%v query_embeds=%d", err, queries)
				}
				return
			}
			if err != nil || strings.Join(routes.Reasons, ",") != "semantic-coverage-partial" || len(routes.Lex) == 0 {
				t.Fatalf("partial coverage must return candidates and report the gap: routes=%+v err=%v", routes, err)
			}
			if len(routes.Dense) == 0 || queries != 1 {
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
		{"default", func(o *Options) {}, ""},
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
				t.Fatalf("default must report the migration block: routes=%+v err=%v", routes, err)
			}
		})
	}
}

func TestSearchRoutesWithoutCoverageGaps(t *testing.T) {
	ctx := context.Background()
	lexical := newTestEngineWith(t, Options{RerankDegrade: DegradeDeny})
	lexicalRoot := newFixtureWorkspace(t)
	routes, err := lexical.SearchRoutes(ctx, searchRequest(lexicalRoot, "HandleLogin"), 10)
	if err != nil || routes.Dense != nil || len(routes.Reasons) != 0 || len(routes.Lex) == 0 {
		t.Fatalf("lexical-only engine under rerank deny: routes=%+v err=%v", routes, err)
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

// TestSearchRoutesFailureBehaviorUnchanged：向量文件损坏时返回错误
// （vector-data-unavailable），不返回只有词法路的候选；ctx 取消与同步失败
// 原样返回。
func TestSearchRoutesFailureBehaviorUnchanged(t *testing.T) {
	ctx := context.Background()
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 16, "routes-model"))
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
	t.Logf("vector-corruption err=%v reasons=%v lex=%d", err, routes.Reasons, len(routes.Lex))
	if err == nil || !strings.Contains(err.Error(), "semantic retrieval unavailable") || !strings.Contains(err.Error(), "vector-data-unavailable") || len(routes.Lex) != 0 {
		t.Fatalf("unavailable vectors must be rejected without lexical candidates: routes=%+v err=%v", routes, err)
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
