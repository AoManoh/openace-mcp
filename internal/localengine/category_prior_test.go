package localengine

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 方案②机制 B 失败测试:CJK 查询下 locale/翻译目录以字级匹配淹没正当
// 内容(E1 实测 django zh 词法 top-20 87% 为 .po;纯词法 R@5 -70%)。
// 语料形态:1 个 .po 翻译目录文件(查询词高频)+ 1 个正当中文文档
// (查询词低频)——修复前 .po 居首,修复后正当文档居首且 .po 仍可召回。
func TestLocalePriorDemotesCatalog(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	po := `msgid "configuration center"
msgstr "配置中心"
msgid "configuration item"
msgstr "配置项"
msgid "where to define configuration"
msgstr "配置在哪里定义"
msgid "configuration definition"
msgstr "配置的定义"
msgid "define configuration item"
msgstr "定义配置项"
`
	writeFixture(t, root, "locale/zh_Hans/LC_MESSAGES/django.po", po)
	writeFixture(t, root, "docs/config-guide.md", "# 配置指南\n\n配置中心负责下发动态配置,修改后实时生效。\n")
	writeFixture(t, root, "app/main.go", "package app\n\nfunc Run() {}\n")
	ref := engine.WorkspaceRef{DirectoryPath: root}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Search(context.Background(), engine.SearchRequest{Workspace: ref, Query: "配置中心 配置项 定义"})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Text
	guideIdx := strings.Index(text, "docs/config-guide.md")
	poIdx := strings.Index(text, "django.po")
	if guideIdx < 0 {
		t.Fatalf("正当中文文档应可召回: %s", text)
	}
	if poIdx < 0 {
		t.Fatalf("locale 文件是惩罚不是过滤,应仍可召回: %s", text)
	}
	if poIdx < guideIdx {
		t.Fatalf("locale 目录应被负先验压到正当文档之后(机制 B):po@%d guide@%d\n%s", poIdx, guideIdx, text)
	}
}

// TestFileCategoryClassifier 是分类器规则冻结(方案② §3.2):
// 只认 locale 类,规则=路径段 locale/locales/i18n/translations 或扩展名 .po/.pot/.mo。
func TestFileCategoryClassifier(t *testing.T) {
	cases := map[string]string{
		"locale/zh_Hans/LC_MESSAGES/django.po": categoryLocale,
		"src/locales/en.json":                  categoryLocale,
		"pkg/i18n/messages.go":                 categoryLocale,
		"translations/app.pot":                 categoryLocale,
		"vendor/lib/x.mo":                      categoryLocale,
		"docs/config-guide.md":                 "",
		"internal/localengine/search.go":       "",
		"testdata/locale_test.go":              "", // 文件名含 locale 子串但非目录段,不误伤
	}
	for path, want := range cases {
		if got := fileCategory(path); got != want {
			t.Fatalf("fileCategory(%q) = %q, want %q", path, got, want)
		}
	}
}
