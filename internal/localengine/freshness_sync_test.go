package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
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

// freshnessTriggerUtilPy 改动 util.py，让下一次构建产生需要嵌入的新 chunk，构建
// 才会发出嵌入请求、停到 gatedEmbedServer 的闸门上。
const freshnessTriggerUtilPy = fixtureUtilPy + "\n\ndef trigger_rebuild():\n    return 1\n"

// freshnessLateGo 是构建扫描结束之后才写入的文件。freshnessLateBody 只出现在它的
// 正文里，检索结果含这段文字说明 late.go 已经进入作答所用的 revision。
const (
	freshnessLateGo   = "package app\n\n// LateArrivalMarker 在构建扫描结束之后才写入。\nfunc LateArrivalMarker() int {\n\treturn 9137\n}\n"
	freshnessLateBody = "return 9137"
)

// embedHoldLimit 是嵌入请求在闸门上最多停留的时长：测试漏了放行，构建也不会一直挂住。
const embedHoldLimit = 20 * time.Second

// gatedEmbedServer 是本地 fake embedding provider，向量与 embedServer 相同
// （fakeVector）。hold 之后到达的请求先报到，再停在闸门上等放行。
type gatedEmbedServer struct {
	url string

	mu   sync.Mutex
	held *embedHold
}

type embedHold struct {
	entered chan struct{}
	gate    chan struct{}
}

func newGatedEmbedServer(t *testing.T, dim int) *gatedEmbedServer {
	t.Helper()
	s := &gatedEmbedServer{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		held := s.held
		s.mu.Unlock()
		if held != nil {
			select {
			case held.entered <- struct{}{}:
			default:
			}
			select {
			case <-held.gate:
			case <-r.Context().Done():
				return
			case <-time.After(embedHoldLimit):
			}
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			items = append(items, item{Embedding: fakeVector(dim, text), Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(ts.Close)
	s.url = ts.URL
	return s
}

// hold 让之后到达的嵌入请求停在闸门上，返回到达信号与放行函数。放行可以重复调用；
// 测试结束时自动放行，清理顺序在关闭引擎与 httptest 服务之前。
func (s *gatedEmbedServer) hold(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	held := &embedHold{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(held.gate) }) }
	s.mu.Lock()
	s.held = held
	s.mu.Unlock()
	t.Cleanup(release)
	return held.entered, release
}

// freshnessGateFixture 建一个嵌入走 gatedEmbedServer、新鲜度窗口为 window 的引擎与
// 工作区，并完成一次不拦截的首建。
func freshnessGateFixture(t *testing.T, window time.Duration) (*Engine, *gatedEmbedServer, string) {
	t.Helper()
	const dim = 8
	server := newGatedEmbedServer(t, dim)
	opts := embedOptions(server.url, dim, 64, "freshness-gate-model")
	// 嵌入请求会在闸门上停留，客户端超时要长于停留时长，否则停留期间请求超时失败。
	opts.Embedding.Timeout = 2 * embedHoldLimit
	opts.FreshnessWindow = window
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	return e, server, root
}

// syncHeldInEmbedding 发起一次显式 Sync，让这次构建停在它的第一个嵌入请求上。嵌入
// 请求在扫描与切分结束之后才发出，duringEmbed 在停住期间执行，此时写入的文件不在
// 这次构建的扫描结果里。duringEmbed 返回后放行，等 Sync 返回并交回它的结果。调用前
// 工作区要有需要嵌入的改动。
func syncHeldInEmbedding(t *testing.T, e *Engine, server *gatedEmbedServer, root string, duringEmbed func()) engine.Result {
	t.Helper()
	entered, release := server.hold(t)
	type outcome struct {
		res engine.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := e.Sync(context.Background(), syncRequest(root))
		done <- outcome{res, err}
	}()
	select {
	case <-entered:
	case out := <-done:
		t.Fatalf("构建没有发出嵌入请求就结束了: %+v err=%v", out.res, out.err)
	case <-time.After(20 * time.Second):
		t.Fatal("构建没有在 20 秒内停到嵌入请求上")
	}
	duringEmbed()
	release()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("放行后的 Sync 失败: %v", out.err)
		}
		return out.res
	case <-time.After(20 * time.Second):
		t.Fatal("放行后 Sync 没有在 20 秒内返回")
	}
	return engine.Result{}
}

// 新鲜度窗口从成功构建的开始时刻计算。构建停在嵌入请求上超过一个窗口，停住期间
// （扫描已结束）新增 late.go；构建成功后的下一次查询距构建开始已超过窗口，应重新
// 扫描、发布新 revision 并检索到 late.go。改动前窗口从完成时刻计算，这次查询仍在
// 窗口内，沿用不含 late.go 的 revision，late.go 要再等一个完整窗口才可见。
func TestFreshnessWindowCountsFromBuildStart(t *testing.T) {
	const window = 400 * time.Millisecond
	e, server, root := freshnessGateFixture(t, window)
	writeFixture(t, root, "util.py", freshnessTriggerUtilPy)
	built := syncHeldInEmbedding(t, e, server, root, func() {
		writeFixture(t, root, "late.go", freshnessLateGo)
		// 构建在嵌入请求到达之前就已开始，这里再停满一个窗口，构建完成时距开始
		// 一定超过窗口。
		time.Sleep(window + 100*time.Millisecond)
	})
	res, err := e.Search(context.Background(), searchRequest(root, "LateArrivalMarker"))
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexRevision == built.IndexRevision || !strings.Contains(res.Text, freshnessLateBody) {
		t.Fatalf("构建耗时超过窗口，下一次查询应重新扫描并检索到嵌入期间新增的 late.go: built=%s served=%s text=%q", built.IndexRevision, res.IndexRevision, res.Text)
	}
}

// 构建很快结束时，下一次查询距构建开始仍不足一个窗口，照常跳过扫描：构建期间新增的
// 文件暂不可见，显式 Sync 不受窗口约束，立即扫描到它。关闭窗口作对照：同样的时序下，
// 下一次查询就重新扫描。
func TestFreshnessWindowShortBuildLateFile(t *testing.T) {
	for _, tc := range []struct {
		name          string
		window        time.Duration
		visibleOnNext bool
	}{
		{name: "window still open after a short build", window: time.Minute, visibleOnNext: false},
		{name: "window disabled rescans on the next query", window: 0, visibleOnNext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e, server, root := freshnessGateFixture(t, tc.window)
			writeFixture(t, root, "util.py", freshnessTriggerUtilPy)
			built := syncHeldInEmbedding(t, e, server, root, func() {
				writeFixture(t, root, "late.go", freshnessLateGo)
			})
			res, err := e.Search(ctx, searchRequest(root, "LateArrivalMarker"))
			if err != nil {
				t.Fatal(err)
			}
			visible := strings.Contains(res.Text, freshnessLateBody)
			rescanned := res.IndexRevision != built.IndexRevision
			if visible != tc.visibleOnNext || rescanned != tc.visibleOnNext {
				t.Fatalf("下一次查询: 期望重新扫描=%v，实际 late.go 可见=%v、revision %s→%s", tc.visibleOnNext, visible, built.IndexRevision, res.IndexRevision)
			}
			if tc.visibleOnNext {
				return
			}
			synced, err := e.Sync(ctx, syncRequest(root))
			if err != nil {
				t.Fatal(err)
			}
			if synced.IndexRevision == built.IndexRevision {
				t.Fatalf("显式 Sync 应不受窗口约束，扫描到 late.go 并发布新 revision: %s", synced.IndexRevision)
			}
			res, err = e.Search(ctx, searchRequest(root, "LateArrivalMarker"))
			if err != nil {
				t.Fatal(err)
			}
			if res.IndexRevision != synced.IndexRevision || !strings.Contains(res.Text, freshnessLateBody) {
				t.Fatalf("显式 Sync 之后的查询应检索到 late.go: synced=%s served=%s text=%q", synced.IndexRevision, res.IndexRevision, res.Text)
			}
		})
	}
}

// 失败的构建不改动新鲜度窗口的起点：既不刷新成失败那次构建的完成时刻，也不用它的
// 开始时刻覆盖上次成功构建的开始时刻。首建成功后过大半个窗口，新增 late.go 并让一次
// 构建在写分段前失败；等到距首建超过一个窗口、距失败那次构建开始仍不足一个窗口时
// 查询，应重新扫描并检索到失败那次没能发布的 late.go。
func TestFreshnessWindowFailedBuildKeepsLastSuccessStart(t *testing.T) {
	const window = 1500 * time.Millisecond
	ctx := context.Background()
	e, _, root := freshnessGateFixture(t, window)
	firstDone := time.Now()
	time.Sleep(window * 3 / 5)
	writeFixture(t, root, "late.go", freshnessLateGo)
	abort := errors.New("写分段前中断（测试注入）")
	segmentWriteHook = func(context.Context, semanticOutcome) error { return abort }
	t.Cleanup(func() { segmentWriteHook = nil })
	failStart := time.Now()
	if _, err := e.Sync(ctx, syncRequest(root)); err == nil || !strings.Contains(err.Error(), abort.Error()) {
		t.Fatalf("注入的写分段失败应让 Sync 失败: %v", err)
	}
	// Sync 返回时失败的构建已经结束，之后不会再读钩子。
	segmentWriteHook = nil
	time.Sleep(time.Until(firstDone.Add(window + 50*time.Millisecond)))
	if since := time.Since(failStart); since >= window {
		t.Fatalf("失败的构建开始至今 %s，已超过窗口 %s，本测试分不出失败是否改动了窗口起点", since, window)
	}
	res, err := e.Search(ctx, searchRequest(root, "LateArrivalMarker"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, freshnessLateBody) {
		t.Fatalf("失败的构建不应改动窗口起点，距上次成功构建开始已超过窗口的查询应重新扫描: %q", res.Text)
	}
}
