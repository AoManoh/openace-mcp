package localengine

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// P1「查询 waitBuild 有界化」:OPENACE_QUERY_BUILD_WAIT>0 时,查询等待
// 在建索引的时长有上界——冷启动大仓首建(分钟级)不再把首个查询钉死。
// 显式语义(D8):超时后构建继续后台推进;有旧 revision 则按 allow/deny
// 降级放行(原因 index-building),无旧 revision 则报可行动错误(带进度)。

// slowEmbedServer 返回一个每批嵌入延迟 delay 的 fake provider。
func slowEmbedServer(t *testing.T, dim int, delay time.Duration) *embedServer {
	t.Helper()
	server := newEmbedServer(t, dim)
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		inner.ServeHTTP(w, r)
	})
	return server
}

// TestQueryBuildWaitColdStart:冷启动 + 慢构建 + 有界等待——查询快速返回
// 可行动错误(指名 env 与进度口径),构建不被取消并最终完成。
func TestQueryBuildWaitColdStart(t *testing.T) {
	const dim = 8
	// lexical-first 关闭档(保险丝路径钉住):冷仓在词法中间发布缺席时
	// 仍是有界错误而非悬挂;默认档的冷仓即时可服务由 lexical_first_test
	// 钉住。
	t.Setenv(EnvLexicalFirst, "off")
	server := slowEmbedServer(t, dim, 300*time.Millisecond)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 1, "fake-model") // batch=1:逐 chunk 慢嵌
	opts.QueryBuildWait = 150 * time.Millisecond
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)

	start := time.Now()
	_, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("冷启动有界等待超时应返回显式错误")
	}
	if !strings.Contains(err.Error(), EnvQueryBuildWait) || !strings.Contains(err.Error(), "index") {
		t.Fatalf("错误应指名 env 与在建状态: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("有界等待不应接近全量构建时长: %v", elapsed)
	}

	// 构建必须继续后台推进:轮询直到完成,随后查询成功且覆盖完整。
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
		if err == nil {
			if !strings.Contains(res.Text, "HandleLogin") {
				t.Fatalf("构建完成后应命中: %q", res.Text)
			}
			if res.SemanticCoverage != "100%" {
				t.Fatalf("后台构建应最终完整: %+v", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("构建应在后台完成而非被取消: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestQueryBuildWaitServesStaleRevision:已有旧 revision + 慢重建 + 有界
// 等待——查询以旧索引放行并显式标记 index-building(allow 默认)。
func TestQueryBuildWaitServesStaleRevision(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	// 可开关延迟:首建全速,重建阶段拨慢(包一层 handler,启动即定型)。
	var delayMs int64
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := atomic.LoadInt64(&delayMs); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	})
	opts := embedOptions(server.ts.URL, dim, 1, "fake-model")
	opts.QueryBuildWait = 150 * time.Millisecond
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	// 变更触发重建,并让重建期每批嵌入变慢。
	atomic.StoreInt64(&delayMs, 400)
	writeFixture(t, root, "newfile.go", "package app\n\n// NewThing 新增声明。\nfunc NewThing() {}\n")

	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("有旧 revision 时应降级放行: %v", err)
	}
	if !strings.Contains(res.DegradedReason, "index-building") {
		t.Fatalf("降级原因应为 index-building: %+v", res)
	}
	if !strings.Contains(res.Text, "[DEGRADED]") {
		t.Fatalf("首行应有降级横幅: %q", strings.SplitN(res.Text, "\n", 2)[0])
	}
	if !strings.Contains(res.Text, "HandleLogin") {
		t.Fatalf("旧索引结果应可用: %q", res.Text)
	}
}

// TestQueryBuildWaitDetachesEarlyOnLongETA(灰度反馈三 C.2):重建 ETA
// 远超等待预算且有旧 revision 可答时,查询在切片醒来时提前脱离等待、
// 立即以旧索引降级应答——不再把整个预算等满(实测该场景 1m50s 纯延迟)。
func TestQueryBuildWaitDetachesEarlyOnLongETA(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	var delayMs int64
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := atomic.LoadInt64(&delayMs); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	})
	opts := embedOptions(server.ts.URL, dim, 1, "fake-model") // batch=1:逐 chunk 慢嵌
	opts.QueryBuildWait = 8 * time.Second
	e := newTestEngineWith(t, opts)
	e.buildWaitSlice = 200 * time.Millisecond
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	// 变更引入大量新 chunk 并拨慢每批嵌入:ETA ≈ 25×0.4s = 10s > 8s 预算。
	var b strings.Builder
	b.WriteString("package app\n")
	for i := 0; i < 25; i++ {
		b.WriteString("\n// Widget 声明。\nfunc Widget")
		b.WriteByte(byte('A' + i))
		b.WriteString("() {}\n")
	}
	atomic.StoreInt64(&delayMs, 400)
	writeFixture(t, root, "widgets.go", b.String())

	start := time.Now()
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("有旧 revision 时应降级放行: %v", err)
	}
	if !strings.Contains(res.DegradedReason, "index-building") {
		t.Fatalf("降级原因应为 index-building: %+v", res)
	}
	if !strings.Contains(res.Text, "HandleLogin") {
		t.Fatalf("旧索引结果应可用: %q", res.Text)
	}
	// 提前脱离的证据:远早于 8s 预算返回(留足调度余量)。
	if elapsed > 5*time.Second {
		t.Fatalf("ETA 超预算应提前脱离等待,实际 %v", elapsed)
	}
}

// TestQueryBuildWaitDisabledKeepsBlocking:未配置(默认 0)行为不变——
// 查询等到构建完成。
func TestQueryBuildWaitDisabledKeepsBlocking(t *testing.T) {
	const dim = 8
	server := slowEmbedServer(t, dim, 50*time.Millisecond)
	defer server.ts.Close()
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 4, "fake-model"))
	root := newFixtureWorkspace(t)
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("默认无界等待应成功: %v", err)
	}
	if res.SemanticCoverage != "100%" {
		t.Fatalf("默认路径应等到完整构建: %+v", res)
	}
}
