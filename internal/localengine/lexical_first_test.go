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

	// 构建中查询:词法中间 revision 一个向量也没有,配置了 embedding 时不以
	// 纯词法结果作答,返回带构建进度的错误,远早于全量构建结束。
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err == nil {
		t.Fatalf("构建中查询不得返回纯词法结果: %+v", res)
	}
	if !strings.Contains(err.Error(), semanticIndexEmptyReason) || !strings.Contains(err.Error(), "build progress: stage=") {
		t.Fatalf("错误应写明没有向量与构建进度: %v", err)
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

// TestColdBuildLexicalFirstRecoveryWithContentChange：词法中间 revision 发布、
// 两批向量已写入 journal 后构建被取消，下一次同步前工作区有一个文件变了。
// 这次同步应在同一次构建里补齐全部缺口（full:semantic-fill、单段、覆盖
// 100%），取消前已写入 journal 的内容不再送审。只按“内容是否变化”选路时，
// 这次同步走 delta，只为变化的文件嵌入；中间 revision 留下的缺口要等某次
// 内容没变的同步才补上，期间缺向量的文件过多，检索直接报错。
func TestColdBuildLexicalFirstRecoveryWithContentChange(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	// proceed 每收到一个值放行一个请求，关闭后全部放行。
	proceed := make(chan struct{}, 64)
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-proceed
		inner.ServeHTTP(w, r)
	})
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 1, "fake-model"))
	root := newFixtureWorkspace(t)
	var b strings.Builder
	b.WriteString("package app\n")
	for i := 0; i < 26; i++ {
		b.WriteString("\n// Widget 声明。\nfunc Widget")
		b.WriteByte(byte('A' + i))
		b.WriteString("() {}\n")
	}
	writeFixture(t, root, "widgets.go", b.String())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Sync(ctx, syncRequest(root))
		done <- err
	}()
	proceed <- struct{}{}
	proceed <- struct{}{}
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
		if err == nil && status.IndexRevision != "" && status.Semantic != nil && status.Semantic.JournalEntries >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("词法中间 revision 未发布，或两批向量未写入 journal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	journaled := server.textsSince(0)
	if len(journaled) != 2 {
		t.Fatalf("取消前应恰好完成 2 个请求: %d", len(journaled))
	}
	cancel()
	close(proceed)
	<-done
	interim, _ := loadActiveManifest(t, e, root)
	if interim.VectorCount != 0 || interim.Counts.Chunks == 0 {
		t.Fatalf("取消后 active 应是没有向量的词法中间 revision: vectors=%d chunks=%d", interim.VectorCount, interim.Counts.Chunks)
	}

	callsBefore := server.callCount()
	writeFixture(t, root, "README.md", fixtureReadme+"\nLogin settings are documented here.\n")
	res, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("恢复同步失败: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if res.BuildMode != "full:semantic-fill" || res.SemanticCoverage != "100%" || len(manifest.Segments) != 1 || !manifest.SemanticComplete() {
		t.Fatalf("内容有变化的恢复同步应在这次构建里补齐缺口: build=%s coverage=%s segments=%d vectors=%d/%d",
			res.BuildMode, res.SemanticCoverage, len(manifest.Segments), manifest.VectorCount, manifest.Counts.Chunks)
	}
	for _, text := range server.textsSince(callsBefore) {
		for _, paid := range journaled {
			if text == paid {
				t.Fatalf("取消前已写入 journal 的内容不得再次送审: %q", text[:min(60, len(text))])
			}
		}
	}
}
