package rerank

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// TestShortResponseRejectedAsPermanent 锁定 H1 client 层修复（诊断报告
// 2026-08-03 §4-H1）：返回条数少于送审条数（空 data、部分返回、data/results
// 键双缺失）一律按 malformed 响应整体拒绝（ClassPermanent），与 embedding
// K22 all-or-nothing 语义对齐——"部分返回"绝不当成功交给调用方，调用方
// 由此走既有 rerank-skipped 降级路（候选保全，P3-T04）。判定基准是送审
// 条数（len(docs)，即 token 预算截断后的送审集），不是 head：sent 截断下
// 返回恰好 sent 条仍是成功（TestTokenCapTruncatesTail 锁定）。
func TestShortResponseRejectedAsPermanent(t *testing.T) {
	cases := map[string]struct {
		provider string
		body     string
		sent     int
	}{
		"voyage 空 data":          {ProviderVoyage, `{"object":"list","data":[]}`, 3},
		"voyage 部分返回 sent-1":    {ProviderVoyage, `{"data":[{"index":0,"relevance_score":0.9},{"index":1,"relevance_score":0.5}]}`, 3},
		"data/results 键双缺失":     {ProviderVoyage, `{"object":"list"}`, 2},
		"TEI 空数组":               {ProviderTEI, `[]`, 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var calls int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			cfg := testConfig(ts.URL)
			cfg.ProviderType = tc.provider
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			fastRetry(client)
			hits, sent, rerankErr := client.Rerank(context.Background(), "q", docs(tc.sent))
			callErr := &reliability.CallError{}
			if !errors.As(rerankErr, &callErr) || callErr.Class != reliability.ClassPermanent {
				t.Fatalf("条数不足应按 malformed 整体拒绝（ClassPermanent）: hits=%v sent=%d err=%v", hits, sent, rerankErr)
			}
			if !strings.Contains(callErr.Message, "count mismatch") {
				t.Fatalf("错误应指明条数不匹配: %v", callErr)
			}
			if atomic.LoadInt32(&calls) != 1 {
				t.Fatalf("permanent 不可重试，应恰好一次调用: calls=%d", calls)
			}
		})
	}
}
