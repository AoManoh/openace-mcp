package localengine

import "testing"

// 路由分立查询规划(§7 -13/-14 批准立项;方案:
// docs/development/2026-08-06-route-split-query-proposal.md)。
// 触发门三条全部满足才改写词法路查询,不触发=行为逐字节不变。

func TestPlanLexicalQueryTriggers(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		want    string
		trigger bool
	}{
		{"EN NL + dotted key", "where is the serde.workspace config key defined", "serde.workspace", true},
		{"EN NL + kebab key", "where is the license-files config key defined", "license-files", true},
		{"EN NL + snake key", "how does hash_password store the salt", "hash_password", true},
		{"EN NL + camelCase", "where is the maxOutputLength limit enforced", "maxOutputLength", true},
		{"EN NL + path token", "what writes to internal/lexical/bleve.go during compaction", "internal/lexical/bleve.go", true},
		{"多结构 token 保序", "how does buildDelta reuse content_hash across revisions", "buildDelta content_hash", true},
		{"纯 key(token<4)不触发", "serde.workspace", "", false},
		{"三 token 不触发", "find serde.workspace definition", "", false},
		{"无结构 token 不触发", "how does the engine recover from crashes", "", false},
		{"zh 包装不触发(CJK 已天然分立)", "serde.workspace 配置项在哪里定义", "", false},
		{"zh 混合长句不触发", "查询 maxOutputLength 在哪里被限制了输出长度", "", false},
		{"版本号不算结构 token", "what changed in version 1.2 of the api", "", false},
		{"纯大写缩写词不触发", "how does the HTTP API handle GET requests", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planLexicalQuery(tc.query)
			if plan.Triggered != tc.trigger {
				t.Fatalf("triggered=%v want %v (query=%q)", plan.Triggered, tc.trigger, tc.query)
			}
			if tc.trigger && plan.LexicalQuery != tc.want {
				t.Fatalf("lexical=%q want %q", plan.LexicalQuery, tc.want)
			}
			if !tc.trigger && plan.LexicalQuery != "" {
				t.Fatalf("未触发时 LexicalQuery 应为空,得到 %q", plan.LexicalQuery)
			}
		})
	}
}

// TestPlanLexicalQueryDeterministic 同输入多次调用逐字段一致。
func TestPlanLexicalQueryDeterministic(t *testing.T) {
	q := "where is the serde.workspace config key defined"
	first := planLexicalQuery(q)
	for i := 0; i < 10; i++ {
		if got := planLexicalQuery(q); got != first {
			t.Fatalf("第 %d 次结果漂移: %+v vs %+v", i, got, first)
		}
	}
}
