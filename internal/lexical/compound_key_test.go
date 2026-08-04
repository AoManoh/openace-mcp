package lexical

import (
	"context"
	"path/filepath"
	"testing"
)

// a' 机制(G-config 诊断,2026-08-04 批准):kebab/dot/snake 复合 key
// (order-by-type / blake3.workspace / use-boolean-and)被标准分词拆成
// 泛词,且 by/and 等停用词被删,gold 配置文件被含单个泛 token 的高频
// 文件淹没。查询扩展:对查询中的复合 token 追加 content 字段的
// AND-match 子句(与索引分析器同口径,停用段自动一致丢弃)。

func newCompoundFixtureIndex(t *testing.T) *Index {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ix.bleve")
	docs := buildCompoundDocs()
	if err := Build(context.Background(), dir, docs); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func buildCompoundDocs() []Doc {
	docs := []Doc{
		{ID: "gold-pyproject", Path: "pyproject.toml", Language: "toml",
			Content: "[tool.ruff.lint.isort]\norder-by-type = false\nknown-first-party = [\"flask\"]\n"},
		{ID: "gold-cargo", Path: "Cargo.toml", Language: "toml",
			Content: "[workspace.dependencies]\nblake3.workspace = true\nserde = \"1\"\n"},
	}
	// 噪声:大量只含单个泛 token 的文件(order/type/workspace 高频)。
	for i := 0; i < 12; i++ {
		docs = append(docs,
			Doc{ID: idOf("order", i), Path: pathOf("order", i), Language: "python",
				Content: "def order_items(order):\n    # sort order for the order queue\n    return order\n"},
			Doc{ID: idOf("type", i), Path: pathOf("type", i), Language: "python",
				Content: "class TypeInfo:\n    # runtime type registry for type checks\n    kind: type = None\n"},
			Doc{ID: idOf("ws", i), Path: pathOf("ws", i), Language: "rust",
				Content: "// workspace resolver walks the workspace members of the workspace\nfn workspace() {}\n"},
		)
	}
	return docs
}

func idOf(kind string, i int) string   { return kind + "-" + string(rune('a'+i)) }
func pathOf(kind string, i int) string { return kind + "/" + string(rune('a'+i)) + ".py" }

// TestCompoundKeyLiftsGoldConfig:复合 key 查询下 gold 配置文件必须进
// top-3(修复前:泛 token 噪声文件淹没 gold)。
func TestCompoundKeyLiftsGoldConfig(t *testing.T) {
	ix := newCompoundFixtureIndex(t)
	cases := []struct {
		query string
		gold  string
	}{
		{"where is order-by-type configured", "gold-pyproject"},
	}
	for _, tc := range cases {
		hits, err := ix.SearchWeighted(context.Background(), tc.query, 10, DefaultWeights())
		if err != nil {
			t.Fatal(err)
		}
		if r := rankOf(hits, tc.gold); r < 0 || r > 2 {
			t.Fatalf("复合 key %q 的 gold %s 应进 top-3,rank=%d hits=%v", tc.query, tc.gold, r, headIDs(hits, 5))
		}
	}
}

// TestCompoundKeyTokenExtraction:触发边界钉死——复合形状才扩展,普通
// 标识符/纯版本号不扩展。
func TestCompoundKeyTokenExtraction(t *testing.T) {
	cases := map[string][]string{
		"where is order-by-type configured": {"order-by-type"},
		"blake3.workspace 在哪里配置":            nil, // dot 复合不触发(网格证据:反伤 nacos-raw)
		"use-boolean-and 的默认值":              {"use-boolean-and"},
		"HandleLogin 在哪里定义":                 nil,
		"upgrade to v1.2.3 today":           nil, // 纯数字段不扩展
		"nacos.core.auth.enabled 配置":        nil, // dot 复合不触发
		"a-b":                               nil, // 过短
	}
	for query, want := range cases {
		got := compoundKeyTokens(query)
		if len(got) != len(want) {
			t.Fatalf("compoundKeyTokens(%q) = %v, want %v", query, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("compoundKeyTokens(%q) = %v, want %v", query, got, want)
			}
		}
	}
}

func headIDs(hits []Hit, n int) []string {
	out := []string{}
	for i, h := range hits {
		if i >= n {
			break
		}
		out = append(out, h.ID)
	}
	return out
}
