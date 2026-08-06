package localengine

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// lexical-first 冷仓发布(框架 18.1/S4,灰度最痛:启用语义的冷仓在
// embedding 结束前完全不可检索,大仓 30 分钟不可用):冷仓首建在
// embedding 开始前先发布词法 revision(coverage 如实 0%,语义缺口
// 显式),查询立即可服务;embedding 完成后发布语义完整 revision。
// 显式 Sync 语义不变(阻塞到最终发布,返回 100% 覆盖)。

// TestColdBuildPublishesLexicalFirst:慢嵌入冷仓,后台 Sync;词法
// revision 应在 embedding 完成前可服务,查询拿到降级词法结果;放行后
// Sync 返回语义完整。
func TestColdBuildPublishesLexicalFirst(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	var delayMs int64 = 300 // 每批嵌入 300ms,batch=1 逐 chunk 慢嵌
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := atomic.LoadInt64(&delayMs); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	})
	opts := embedOptions(server.ts.URL, dim, 1, "fake-model")
	// 预算 2s << 嵌入总时长(≈35 chunks × 300ms ≈ 10s):查询经 ETA
	// 提前脱离,以词法中间 revision 降级应答。
	opts.QueryBuildWait = 2 * time.Second
	e := newTestEngineWith(t, opts)
	e.buildWaitSlice = 100 * time.Millisecond
	root := newFixtureWorkspace(t)
	var b strings.Builder
	b.WriteString("package app\n")
	for i := 0; i < 26; i++ {
		b.WriteString("\n// Widget 声明。\nfunc Widget")
		b.WriteByte(byte('A' + i))
		b.WriteString("() {}\n")
	}
	writeFixture(t, root, "widgets.go", b.String())

	syncDone := make(chan engine.Result, 1)
	syncErr := make(chan error, 1)
	go func() {
		res, err := e.Sync(context.Background(), syncRequest(root))
		syncDone <- res
		syncErr <- err
	}()

	// 词法 revision 应在嵌入完成前就绪(轮询状态直到出现 revision)。
	deadline := time.Now().Add(15 * time.Second)
	var lexicalRevision string
	for {
		status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
		if err == nil && status.IndexRevision != "" && status.InFlight {
			lexicalRevision = status.IndexRevision
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("词法 revision 应在 embedding 完成前发布")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 构建中查询:应以词法 revision 降级放行(index-building),
	// 命中词法内容,远快于全量构建。
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("词法先行后构建中查询应可服务: %v", err)
	}
	if !strings.Contains(res.Text, "HandleLogin") {
		t.Fatalf("词法结果应命中: %q", res.Text)
	}
	if !strings.Contains(res.DegradedReason, "index-building") && !strings.Contains(res.DegradedReason, "semantic-coverage-partial") {
		t.Fatalf("语义缺口必须显式: %+v", res.DegradedReason)
	}

	// 放行嵌入,Sync 返回语义完整;最终 revision 覆盖 100%。
	atomic.StoreInt64(&delayMs, 0)
	select {
	case final := <-syncDone:
		if err := <-syncErr; err != nil {
			t.Fatal(err)
		}
		if final.SemanticCoverage != "100%" {
			t.Fatalf("显式 Sync 应等到语义完整: %+v", final)
		}
		if final.IndexRevision == lexicalRevision {
			t.Fatalf("最终 revision 应区别于词法中间 revision: %s", final.IndexRevision)
		}
		if final.BuildMode != "full:first-build" {
			t.Fatalf("最终构建形态标注错误: %q", final.BuildMode)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Sync 未在放行后完成")
	}

	// 收尾:语义完整后查询无降级。
	after, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil || after.DegradedReason != "" {
		t.Fatalf("最终态不应降级: %+v err=%v", after, err)
	}
}

// TestColdBuildLexicalFirstCrashRecovery:词法中间 revision 发布后构建
// 被取消(模拟崩溃)——下次 Sync 走 semantic-fill 补齐到 100%,内容
// 不重扫重付(既有 journal/fill 机制收敛)。
func TestColdBuildLexicalFirstCrashRecovery(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	var delayMs int64 = 200
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := atomic.LoadInt64(&delayMs); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	})
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 1, "fake-model"))
	root := newFixtureWorkspace(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Sync(ctx, syncRequest(root))
		done <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
		if err == nil && status.IndexRevision != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("词法 revision 未发布")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel() // 模拟嵌入中途崩溃
	<-done

	atomic.StoreInt64(&delayMs, 0)
	res, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("恢复 Sync 应成功: %v", err)
	}
	if res.SemanticCoverage != "100%" {
		t.Fatalf("恢复后应语义完整: %+v", res)
	}
	if !strings.HasPrefix(res.BuildMode, "full:semantic-fill") {
		t.Fatalf("恢复路径应为 semantic-fill: %q", res.BuildMode)
	}
}
