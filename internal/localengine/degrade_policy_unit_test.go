package localengine

import (
	"errors"
	"strings"
	"testing"
)

// TestPolicyErrorClassifiesGaps：同一判定函数按原因类别选择开关；strict 优先，
// 错误写明全部原因与对应环境变量；放行时返回 nil。
func TestPolicyErrorClassifiesGaps(t *testing.T) {
	gaps := func(reasons ...string) []qualityGap {
		out := make([]qualityGap, 0, len(reasons))
		for _, r := range reasons {
			out = append(out, qualityGap{reason: r, detail: errors.New("detail of " + r)})
		}
		return out
	}
	cases := []struct {
		name    string
		e       *Engine
		gaps    []qualityGap
		wantEnv string
	}{
		{"allow 放行全部", &Engine{}, gaps("semantic-coverage-partial", "rerank-skipped(transient)", "rerank-unconfigured(x)"), ""},
		{"retrieval deny 拒绝覆盖不足", &Engine{retrievalDegrade: DegradeDeny}, gaps("semantic-coverage-partial"), EnvRetrievalDegrade},
		{"retrieval deny 拒绝查询嵌入失败", &Engine{retrievalDegrade: DegradeDeny}, gaps("query-embedding-failed(transient)"), EnvRetrievalDegrade},
		{"retrieval deny 不管精排", &Engine{retrievalDegrade: DegradeDeny}, gaps("rerank-skipped(token-budget)"), ""},
		{"rerank deny 拒绝 token-budget", &Engine{rerankDegrade: DegradeDeny}, gaps("rerank-skipped(token-budget)"), EnvRerankDegrade},
		{"rerank deny 拒绝无效响应", &Engine{rerankDegrade: DegradeDeny}, gaps("rerank-partial-response"), EnvRerankDegrade},
		{"rerank deny 不管未配置", &Engine{rerankDegrade: DegradeDeny, retrievalDegrade: DegradeDeny}, gaps("rerank-unconfigured(x)"), ""},
		{"strict 拒绝未配置", &Engine{qualityStrict: true}, gaps("rerank-unconfigured(x)"), EnvQualityStrict},
		{"strict 优先", &Engine{qualityStrict: true, retrievalDegrade: DegradeDeny}, gaps("semantic-coverage-partial"), EnvQualityStrict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.e.policyError(tc.gaps)
			if tc.wantEnv == "" {
				if err != nil {
					t.Fatalf("应放行: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantEnv) || !strings.Contains(err.Error(), tc.gaps[0].reason) || !strings.Contains(err.Error(), "detail of") {
				t.Fatalf("应返回带 %s、原因与说明的错误: %v", tc.wantEnv, err)
			}
		})
	}
}
