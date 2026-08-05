package localengine

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 机制②-B(locale 负先验)移除回归(2026-08-05 用户裁决,决策台账 §7 -8):
// 门 B 复扫证明 hybrid 下惩罚在全部防护面零收益(防护由机制 A+dense 融合
// 承担),代价集中在 locale-gold 查询——sealed v2 实例:"传输协议选项"
// gold=console-ui/src/locales/zh-CN.js 被 0.35× 压出 top-10;dev 端到端:
// 关闭惩罚 posent2-nacos R@5 0.35→0.50(work/locale-grid/verdict.md §3.1)。
// 场景:查询即菜单短标签,locale 文件是唯一正确答案;弱相关的代码文件仅
// 提及部分词。移除前 locale gold 被压到弱文件之后(本测试红),移除后按
// 自然 BM25 序居首(绿)。
func TestLocaleGoldNotDemoted(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	locale := `const messages = {
  'nav.transport': '传输协议选项',
  'nav.cluster': '集群管理',
  'nav.config': '配置列表',
};
export default messages;
`
	// 竞争文件:中文密集文档,查询的每个单字都以无关词高频出现(CJK 单字
	// 级匹配的真实干扰形态,sealed 翻转案例同构)——原生 BM25 下 gold 的
	// 整词命中仍胜,0.35× 惩罚后被反超。
	doc := `# 使用手册

上传文件与传送日志:先选择输入目录,再选择输出目录。
协作模式下的会议协商记录会输出到项目目录。
`
	writeFixture(t, root, "console/src/locales/zh-CN.js", locale)
	writeFixture(t, root, "docs/manual.md", doc)
	writeFixture(t, root, "app/transport.go", "package app\n\nfunc Dial() {}\n")
	ref := engine.WorkspaceRef{DirectoryPath: root}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Search(context.Background(), engine.SearchRequest{Workspace: ref, Query: "传输协议选项"})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Text
	goldIdx := strings.Index(text, "console/src/locales/zh-CN.js")
	if goldIdx < 0 {
		t.Fatalf("locale gold 应可召回: %s", text)
	}
	if idx := strings.Index(text, "docs/manual.md"); idx >= 0 && idx < goldIdx {
		t.Fatalf("locale gold 是查询的唯一整词命中,不得被类别先验压到单字噪声文档之后: gold@%d manual@%d\n%s",
			goldIdx, idx, text)
	}
}
