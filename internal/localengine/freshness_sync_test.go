package localengine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// F2(review 2026-08-06):freshness 窗口的注释契约是"只作用于查询期
// 内联同步",但检查位于 Sync 与查询共用入口——显式 Sync 在窗口内被
// 短路,编辑后 sync 返回旧 revision 且无信号。修复后:显式 Sync 永远
// 真实扫描;查询期内联同步保留窗口短路。
func TestFreshnessWindowDoesNotShortCircuitExplicitSync(t *testing.T) {
	e := newTestEngineWith(t, Options{FreshnessWindow: time.Hour})
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "util.py", "def parse_config(path):\n    return 41  # fresh-window\n")
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if second.IndexRevision == first.IndexRevision {
		t.Fatalf("显式 Sync 被 freshness 窗口短路,revision 未推进: %s", first.IndexRevision)
	}
	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "return 41") {
		t.Fatalf("显式 Sync 后新内容不可检索: %q", result.Text)
	}
}

// 查询期内联同步保留窗口短路(既有语义):窗口内查询不触发扫描。
func TestFreshnessWindowStillShortCircuitsQueryPath(t *testing.T) {
	e := newTestEngineWith(t, Options{FreshnessWindow: time.Hour})
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "util.py", "def parse_config(path):\n    return 42  # not-scanned\n")
	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "return 42") {
		t.Fatal("查询期窗口短路失效:窗口内查询触发了扫描")
	}
}
