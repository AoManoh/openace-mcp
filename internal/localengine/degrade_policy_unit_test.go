package localengine

import (
	"errors"
	"strings"
	"testing"
)

// TestPolicyErrorClassifiesGaps：同一判定函数按原因类别决定是否拒绝。语义路
// 跑不了的原因总是拒绝；strict 拒绝任何缺口；rerank deny 只拒绝精排类原因；
// 其余放行。错误写明全部原因与说明。
func TestPolicyErrorClassifiesGaps(t *testing.T) {
	gaps := func(reasons ...string) []qualityGap {
		out := make([]qualityGap, 0, len(reasons))
		for _, r := range reasons {
			out = append(out, qualityGap{reason: r, detail: errors.New("detail of " + r)})
		}
		return out
	}
	const semanticUnavailable = "semantic retrieval unavailable"
	cases := []struct {
		name     string
		e        *Engine
		gaps     []qualityGap
		wantText string
	}{
		{"默认放行覆盖不足、索引不新鲜与精排缺口", &Engine{}, gaps("semantic-coverage-partial", "stale-index", "index-refreshing", "rerank-skipped(transient)", "rerank-unconfigured(x)"), ""},
		{"默认拒绝查询嵌入失败", &Engine{}, gaps("query-embedding-failed(transient)"), semanticUnavailable},
		{"默认拒绝向量不可用", &Engine{}, gaps("vector-data-unavailable"), semanticUnavailable},
		{"默认拒绝没有向量的 revision", &Engine{}, gaps(semanticIndexEmptyReason, migrationBlockedReason), semanticUnavailable},
		{"rerank deny 拒绝 token-budget", &Engine{rerankDegrade: DegradeDeny}, gaps("rerank-skipped(token-budget)"), EnvRerankDegrade},
		{"rerank deny 拒绝无效响应", &Engine{rerankDegrade: DegradeDeny}, gaps("rerank-partial-response"), EnvRerankDegrade},
		{"rerank deny 不管未配置", &Engine{rerankDegrade: DegradeDeny}, gaps("rerank-unconfigured(x)"), ""},
		{"rerank deny 不管覆盖不足", &Engine{rerankDegrade: DegradeDeny}, gaps("semantic-coverage-partial"), ""},
		{"strict 拒绝未配置", &Engine{qualityStrict: true}, gaps("rerank-unconfigured(x)"), EnvQualityStrict},
		{"strict 拒绝覆盖不足", &Engine{qualityStrict: true}, gaps("semantic-coverage-partial"), EnvQualityStrict},
		{"语义路不可用优先于 strict", &Engine{qualityStrict: true}, gaps("semantic-coverage-partial", "query-embedding-failed(auth)"), semanticUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.e.policyError(tc.gaps)
			if tc.wantText == "" {
				if err != nil {
					t.Fatalf("应放行: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantText) || !strings.Contains(err.Error(), tc.gaps[0].reason) || !strings.Contains(err.Error(), "detail of") {
				t.Fatalf("应返回带 %q、原因与说明的错误: %v", tc.wantText, err)
			}
		})
	}
}
