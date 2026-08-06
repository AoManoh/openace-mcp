package localengine

import (
	"context"
	"strings"
	"testing"
)

// 路由分立接线的引擎级行为(纯词法引擎,零 provider):触发时词法路用
// 结构 token 变体、结果携带 query_plan;零命中兜底回退原查询;不触发
// 查询行为与历史一致。
func newRouteSplitWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// 定义现场:TOML 内声明 pool_size(结构 token 的唯一定义点)。
	writeFixture(t, root, "settings.toml", "pool_size = 10\nlog_level = \"info\"\n")
	// 包装词污染面:大量复现 NL 包装词的 prose,并顺带提及 pool_size,
	// 使原查询的词法匹配强烈偏向 prose。
	prose := strings.Repeat("where is the config key defined and how the config key works. ", 12) +
		"tuning notes mention pool_size once here.\n"
	writeFixture(t, root, "docs.md", prose)
	writeFixture(t, root, "notes.md", strings.Repeat("config key defined where is the value. ", 10)+"\n")
	return root
}

func TestRouteSplitLexicalFocus(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	root := newRouteSplitWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	// 原查询(不触发形态,3 token)按历史行为偏向 prose——对照面。
	control, err := e.Search(context.Background(), searchRequest(root, "config key defined"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstHitPath(control.Text), ".md") {
		t.Fatalf("对照面应由 prose 主导,得到 %q", firstHitPath(control.Text))
	}
	if control.QueryPlan != "" {
		t.Fatalf("不触发查询不应携带 query_plan,得到 %q", control.QueryPlan)
	}
	// 触发形态:词法路应聚焦 pool_size → 定义现场 settings.toml 登顶。
	result, err := e.Search(context.Background(), searchRequest(root, "where is the pool_size config key defined"))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstHitPath(result.Text); got != "settings.toml" {
		t.Fatalf("触发后首位应为定义现场 settings.toml,得到 %q(全文首 200B:%.200s)", got, result.Text)
	}
	if result.QueryPlan != "pool_size" {
		t.Fatalf("query_plan 应为结构变体 %q,得到 %q", "pool_size", result.QueryPlan)
	}
}

func TestRouteSplitZeroHitFallback(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	root := newRouteSplitWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	// 结构变体在索引中零命中 → 必须回退原查询,返回非空结果。
	result, err := e.Search(context.Background(), searchRequest(root, "where is the zzz_missing_key config key defined"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("零命中兜底失效:结果为空")
	}
	if !strings.Contains(result.QueryPlan, "fallback") {
		t.Fatalf("兜底路径应在 query_plan 标注 fallback,得到 %q", result.QueryPlan)
	}
}

// firstHitPath 从渲染文本首行 "## path:start-end symbol" 提取 path。
func firstHitPath(text string) string {
	line := strings.SplitN(strings.TrimSpace(text), "\n", 2)[0]
	line = strings.TrimPrefix(line, "## ")
	if idx := strings.IndexByte(line, ':'); idx > 0 {
		return line[:idx]
	}
	return line
}
