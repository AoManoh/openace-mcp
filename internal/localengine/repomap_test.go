package localengine

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// repo_map R1(D4 呈批件,框架文档"条件 dogfood 候选"):快照只读的
// 仓库地图——按目录聚合、符号密度+路径负先验+顶层目录保底配额的
// 确定性排序、预算内渲染。零新增索引产物、零 provider 调用。

func repoMapFixture(t *testing.T) (*Engine, string) {
	t.Helper()
	e := newTestEngine(t)
	root := t.TempDir()
	writeFixture(t, root, "core/service.go", "package core\n\n// Establish 建立会话。\nfunc Establish() {}\n\nfunc Revoke() {}\n\nfunc Renew() {}\n")
	writeFixture(t, root, "core/util.go", "package core\n\nfunc helper() {}\n")
	writeFixture(t, root, "web/handler.go", "package web\n\nfunc Handle() {}\n\nfunc Route() {}\n")
	writeFixture(t, root, "web/handler_test.go", "package web\n\nfunc TestHandle(t *testing.T) {}\n")
	writeFixture(t, root, "README.md", "# demo\n\nquick start.\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	return e, root
}

// R1-1:基本形态——目录小计 + 文件行 + 符号,确定性(3 次逐字节一致),
// 结构化字段齐。
func TestRepoMapBasicShape(t *testing.T) {
	e, root := repoMapFixture(t)
	req := engine.RepoMapRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}}
	first, err := e.RepoMap(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"core/", "web/", "core/service.go:", "Establish", "Handle", "repo map:"} {
		if !strings.Contains(first.Text, want) {
			t.Fatalf("地图缺 %q:\n%s", want, first.Text)
		}
	}
	if first.IndexRevision == "" || first.FileCount != 5 {
		t.Fatalf("结构化字段错误: revision=%q files=%d", first.IndexRevision, first.FileCount)
	}
	if first.Timings == nil || first.Timings.TotalMs < 0 || first.Timings.RenderMs < 0 {
		t.Fatalf("repo_map 应携带总耗时/渲染耗时: %+v", first.Timings)
	}
	for i := 0; i < 2; i++ {
		again, err := e.RepoMap(context.Background(), req)
		if err != nil || again.Text != first.Text {
			t.Fatalf("地图应确定性(第 %d 次): err=%v", i+2, err)
		}
	}
}

// immutable revision 的聚合缓存只构建一次；focus 从缓存过滤且返回独立
// 切片，后续排序/标签修改不污染全仓地图。
func TestRepoMapCachesRevisionAggregation(t *testing.T) {
	handle := &revisionHandle{chunks: map[string]chunkMeta{
		"a": {RelPath: "src/a.go", EndLine: 10, Symbol: "Alpha"},
		"b": {RelPath: "docs/b.md", EndLine: 5, Symbol: "Beta"},
	}}
	full := cachedMapFiles(handle, "")
	if len(full) != 2 || len(handle.repoMapFiles) != 2 {
		t.Fatalf("首次聚合异常: full=%+v cache=%+v", full, handle.repoMapFiles)
	}
	// 模拟不应发生的底层 map 变化，证明 once 后不重扫；revision 本身
	// 在生产中不可变。
	handle.chunks["c"] = chunkMeta{RelPath: "src/c.go", EndLine: 3, Symbol: "Gamma"}
	focus := cachedMapFiles(handle, "src")
	if len(focus) != 1 || focus[0].path != "src/a.go" || focus[0].topDir != "src" {
		t.Fatalf("focus 缓存过滤异常: %+v", focus)
	}
	focus[0].symbols[0] = "mutated"
	again := cachedMapFiles(handle, "")
	var srcSymbol string
	for _, file := range again {
		if file.path == "src/a.go" {
			srcSymbol = file.symbols[0]
		}
	}
	if len(again) != 2 || srcSymbol != "Alpha" {
		t.Fatalf("调用方修改污染缓存: %+v", again)
	}
}

// R1-2:测试文件负先验——同目录下 handler.go 应排在 handler_test.go 前。
func TestRepoMapTestFileDeprioritized(t *testing.T) {
	e, root := repoMapFixture(t)
	res, err := e.RepoMap(context.Background(), engine.RepoMapRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}})
	if err != nil {
		t.Fatal(err)
	}
	main := strings.Index(res.Text, "web/handler.go:")
	test := strings.Index(res.Text, "web/handler_test.go:")
	if main == -1 || (test != -1 && test < main) {
		t.Fatalf("测试文件应负先验排后: main=%d test=%d\n%s", main, test, res.Text)
	}
}

// R1-3:focus 前缀过滤——只出 core/ 子树。
func TestRepoMapFocus(t *testing.T) {
	e, root := repoMapFixture(t)
	res, err := e.RepoMap(context.Background(), engine.RepoMapRequest{
		Workspace: engine.WorkspaceRef{DirectoryPath: root}, Focus: "core"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "core/ (") || !strings.Contains(res.Text, "core/service.go:") || strings.Contains(res.Text, "web/handler.go") {
		t.Fatalf("focus 过滤/header 错误:\n%s", res.Text)
	}
}

// R1-4:预算截断可行动 + 顶层目录保底(小预算下 core 与 web 都应有代表,
// 防单目录刷屏——反馈二 §3.4 预算错配教训)。
func TestRepoMapBudgetAndQuota(t *testing.T) {
	e, root := repoMapFixture(t)
	res, err := e.RepoMap(context.Background(), engine.RepoMapRequest{
		Workspace: engine.WorkspaceRef{DirectoryPath: root}, MaxOutputLen: 260})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "core/") || !strings.Contains(res.Text, "web/") {
		t.Fatalf("小预算下顶层目录应各有代表:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "[map truncated") {
		t.Fatalf("超预算应带可行动截断标记:\n%s", res.Text)
	}
}

// R1-5:冷仓(无 revision)= 显式 not-ready,不隐式 Sync、不触发付费
// (呈批件暗坑:repo-map 承诺零 provider 调用)。
func TestRepoMapColdWorkspaceNotReady(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	writeFixture(t, root, "a.go", "package a\n")
	_, err := e.RepoMap(context.Background(), engine.RepoMapRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}})
	if err == nil || !strings.Contains(err.Error(), "index_not_ready") {
		t.Fatalf("冷仓应显式 not-ready: %v", err)
	}
}
