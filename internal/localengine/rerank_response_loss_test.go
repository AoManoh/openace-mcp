package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// 本文件是 H1 的常驻回归（诊断报告 2026-08-03 §4-H1 探针场景）：rerank
// 端点返回空/部分响应时，"已送审但 provider 未返回"的候选绝不静默丢弃
// （P3-T04），且降级必须显式（决策 11）——mode 不得虚标 +rerank。

// newEmptyRerankServer 构造恒返 200 空 data 的 voyage 形状端点（H1 触发
// 面：网关兜底页/误配端点返回合法 JSON 但零结果）。
func newEmptyRerankServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newTruncatingRerankServer 构造恒丢弃最后一条的 voyage 形状端点（H1
// 触发面：忽略非标准 top_k、自带默认截断的自部署 rerank 服务）。
func newTruncatingRerankServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		items := make([]item, 0, len(req.Documents))
		for i := range req.Documents {
			if i == len(req.Documents)-1 {
				break
			}
			items = append(items, item{Index: i, RelevanceScore: 1 - float64(i)*0.1})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRerankEmptyResponseKeepsCandidatesAndDegrades 复现 §4-H1 极端形状：
// 端点 200 + {"data":[]}。修复前实测 mode="lexical+rerank" degraded=""
// text="No relevant code sections were found."——头部 50 候选全丢、零降级
// 信号。修复后：候选保全 + rerank-skipped 显式降级 + mode 不虚标。
func TestRerankEmptyResponseKeepsCandidatesAndDegrades(t *testing.T) {
	ts := newEmptyRerankServer(t)
	e := newTestEngineWith(t, Options{Rerank: rerankOptions(ts.URL)})
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatalf("allow 模式不得报错: %v", err)
	}
	if !strings.Contains(result.Text, "parse_config") {
		t.Fatalf("已送审候选不得因空响应丢失（P3-T04）: %q", result.Text)
	}
	if result.DegradedReason == "" || !strings.Contains(result.DegradedReason, "rerank-skipped(permanent)") {
		t.Fatalf("空响应必须显式降级（决策 11）: %+v", result)
	}
	if strings.Contains(result.RetrievalMode, "+rerank") {
		t.Fatalf("精排未生效时 mode 不得虚标 +rerank: %q", result.RetrievalMode)
	}
	if !strings.HasPrefix(result.Text, "[DEGRADED] ") {
		t.Fatalf("首行应为 DEGRADED 横幅: %q", strings.SplitN(result.Text, "\n", 2)[0])
	}
}

// TestRerankPartialResponseKeepsCandidatesAndDegrades 复现 §4-H1 部分返回
// 形状：端点恒返回 sent-1 条。修复前"已送审未返回"的那条直接消失；
// 修复后候选集与无 rerank 基线逐块一致，且显式降级。
func TestRerankPartialResponseKeepsCandidatesAndDegrades(t *testing.T) {
	const query = "login parse_config"
	baseline := newTestEngineWith(t, Options{})
	root := newFixtureWorkspace(t)
	baseResult, err := baseline.Search(context.Background(), searchRequest(root, query))
	if err != nil {
		t.Fatal(err)
	}
	baseHeaders := headerSet(t, baseResult.Text)
	if len(baseHeaders) < 2 {
		t.Fatalf("基线应命中多个块: %v", baseHeaders)
	}

	ts := newTruncatingRerankServer(t)
	e := newTestEngineWith(t, Options{Rerank: rerankOptions(ts.URL)})
	result, err := e.Search(context.Background(), searchRequest(root, query))
	if err != nil {
		t.Fatalf("allow 模式不得报错: %v", err)
	}
	gotHeaders := headerSet(t, result.Text)
	if len(gotHeaders) != len(baseHeaders) {
		t.Fatalf("部分返回不得增删候选（P3-T04）: base=%v got=%v", baseHeaders, gotHeaders)
	}
	for header := range baseHeaders {
		if !gotHeaders[header] {
			t.Fatalf("已送审候选 %q 丢失: %v", header, gotHeaders)
		}
	}
	if !strings.Contains(result.DegradedReason, "rerank-skipped(permanent)") {
		t.Fatalf("部分返回必须显式降级（决策 11）: %+v", result)
	}
	if strings.Contains(result.RetrievalMode, "+rerank") {
		t.Fatalf("精排未生效时 mode 不得虚标 +rerank: %q", result.RetrievalMode)
	}
}

// TestRerankAssembleOrderBackfillsUnreturnedSent 是调用方兜底的单元锁定
// （防御纵深：应对未来 client 层 all-or-nothing 校验被绕过的形状）：
// "已送审但未返回"的条目按原序补在重排段之后，任何形状下不丢失、
// 不重复；missing 计数供 rerankOrder 显式上报。
func TestRerankAssembleOrderBackfillsUnreturnedSent(t *testing.T) {
	included := []rankedHit{{id: "a"}, {id: "b"}, {id: "c"}, {id: "d"}} // sent=3，d 为预算未送审尾
	skipped := []rankedHit{{id: "s1"}}
	tail := []rankedHit{{id: "t1"}, {id: "t2"}}

	cases := map[string]struct {
		hits        []rerank.Hit
		wantIDs     []string
		wantMissing int
	}{
		"部分返回:a/b 未返回按原序补在重排段后": {
			hits:        []rerank.Hit{{ID: "c", Score: 0.9}},
			wantIDs:     []string{"c", "a", "b", "d", "s1", "t1", "t2"},
			wantMissing: 2,
		},
		"空返回:送审集整体原序补回": {
			hits:        nil,
			wantIDs:     []string{"a", "b", "c", "d", "s1", "t1", "t2"},
			wantMissing: 3,
		},
		"完整返回:重排生效且 missing=0(K28 既有语义不回归)": {
			hits:        []rerank.Hit{{ID: "b", Score: 0.9}, {ID: "a", Score: 0.5}, {ID: "c", Score: 0.1}},
			wantIDs:     []string{"b", "a", "c", "d", "s1", "t1", "t2"},
			wantMissing: 0,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ids, missing := rerankAssembleOrder(tc.hits, included, 3, skipped, tail)
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("最终序不符: got=%v want=%v", ids, tc.wantIDs)
			}
			if missing != tc.wantMissing {
				t.Fatalf("missing 计数不符: got=%d want=%d", missing, tc.wantMissing)
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if seen[id] {
					t.Fatalf("候选重复: %s in %v", id, ids)
				}
				seen[id] = true
			}
		})
	}
}
