package localengine

import (
	"context"
	"strings"
	"testing"
)

// 碎片密度门 spike(候选 (l),研究报告 §4:裸字符阈值被反例证伪——
// eff<20 误伤正常短文档段/TODO)。复合判据:仅 fallback 切分 + 微小 +
// "空洞"(无字母词 token 或行形态为多行×每行≈1-2 有效字符)才判碎片。
// opt-in(OPENACE_FRAGMENT_GATE=on),默认关闭零行为变化;默认翻转
// 待碎片密集真实语料(mailing web*.md)验证。

const fragmentNoiseDoc = `2021
年
12
月
8
日

2022
年
1
月
3
日
`

// TestFragmentPredicate:判据单元面——现场碎片拦截,合法短内容放行。
func TestFragmentPredicate(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		fallback bool
		want     bool
	}{
		{"mailing 日期碎片", "2021\n年\n12\n月\n8\n日", true, true},
		{"单行纯日期", "2021年12月8日", true, true},
		{"版本号不是日期", "1.26.5", true, false},
		{"端口号不是日期", "8765", true, false},
		{"纯符号行", "----\n====\n****", true, true},
		{"正常短文档段", "## 安装\n\n执行 make install 即可。", true, false},
		{"一行 TODO", "TODO: fix race", true, false},
		{"短代码块", "func Ping() error { return nil }", true, false},
		{"中文说明句", "该模块负责会话恢复。", true, false},
		{"AST chunk 永不拦", "2021\n年\n12\n月", false, false},
	}
	for _, c := range cases {
		record := chunkRecord{Content: c.content, Capability: "fallback"}
		if !c.fallback {
			record.Capability = "ast"
		}
		if got := isFragmentNoise(record); got != c.want {
			t.Fatalf("%s: 判定=%v 期望=%v", c.name, got, c.want)
		}
	}
}

// TestFragmentGateFiltersNoiseFromResults:开门后碎片不进结果,真实
// 内容保留;关门(默认)行为不变。
func TestFragmentGateFiltersNoiseFromResults(t *testing.T) {
	build := func(t *testing.T) (*Engine, string) {
		e := newTestEngine(t)
		root := t.TempDir()
		writeFixture(t, root, "notes/dates.md", fragmentNoiseDoc)
		writeFixture(t, root, "notes/real.md", "# 出网监测\n\nnetwatch cron 每 10 分钟探测三域名,日志见 netwatch-egress.log。\n")
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
		return e, root
	}

	t.Run("默认关闭:碎片照常可检索(行为不变)", func(t *testing.T) {
		e, root := build(t)
		res, err := e.Search(context.Background(), searchRequest(root, "2021 12"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Text, "dates.md") {
			t.Fatalf("默认档不应过滤: %q", res.Text)
		}
	})

	t.Run("开门:碎片出结果面,真实内容保留", func(t *testing.T) {
		e := newTestEngineWith(t, Options{FragmentGate: true})
		root := t.TempDir()
		writeFixture(t, root, "notes/dates.md", fragmentNoiseDoc)
		writeFixture(t, root, "notes/real.md", "# 出网监测\n\nnetwatch cron 每 10 分钟探测三域名,日志见 netwatch-egress.log。\n")
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
		res, err := e.Search(context.Background(), searchRequest(root, "netwatch 出网监测 2021 12"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Text, "dates.md") {
			t.Fatalf("碎片应被过滤: %q", res.Text)
		}
		if !strings.Contains(res.Text, "real.md") {
			t.Fatalf("真实内容应保留: %q", res.Text)
		}
	})
}
