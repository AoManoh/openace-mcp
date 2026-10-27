package localengine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 前一 revision 有语义缺口时内容变化的同步怎样选路：缺口能补就在这次构建
// 里一并补齐；熔断器退避期间、或上一次补缺口没有进展时保持 delta。

// gapFillPausedMarker 是暂停说明里的固定文字，测试据此判断状态是否写明暂停。
const gapFillPausedMarker = "内容变化时的全量补缺已暂停"

// syncedWithGap 先把 fixture 建成完整覆盖，再让 failing 匹配的内容在一次
// delta 里嵌入失败，留下语义缺口。
func syncedWithGap(t *testing.T, e *Engine, root string, server *embedServer) {
	t.Helper()
	ctx := context.Background()
	if res, err := e.Sync(ctx, syncRequest(root)); err != nil || res.SemanticCoverage != "100%" {
		t.Fatalf("前置：首建应完整覆盖: %+v err=%v", res, err)
	}
	server.setFailWhen(func(texts []string) bool {
		return len(texts) == 1 && strings.Contains(texts[0], "FailingGap")
	})
	writeFixture(t, root, "gap.go", "package app\n\nfunc FailingGap() {}\n")
	if res, err := e.Sync(ctx, syncRequest(root)); err != nil || res.BuildMode != "delta" || res.SemanticCoverage == "100%" {
		t.Fatalf("前置：gap.go 嵌入失败应留下缺口: %+v err=%v", res, err)
	}
}

// TestPartialGapFilledOnChangedSync：一次 delta 的嵌入失败留下语义缺口，
// provider 恢复后的下一次同步有内容变化，应在这次构建里一并补齐缺口
// （full:semantic-fill、覆盖 100%），只为缺口与新文件送审，其余向量全部复用。
func TestPartialGapFilledOnChangedSync(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 1, "fake-model")
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	syncedWithGap(t, e, root, server)

	// provider 恢复；换新引擎让熔断器回到初始状态，做法与
	// TestPartialEmbeddingFailurePublishesDegradedThenHeals 相同。
	server.setFailWhen(nil)
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e2.Close(context.Background()) })
	writeFixture(t, root, "extra.go", "package app\n\nfunc ExtraSession() {}\n")
	callsBefore := server.callCount()
	res, err := e2.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if res.BuildMode != "full:semantic-fill" || res.SemanticCoverage != "100%" {
		t.Fatalf("内容有变化的同步应一并补齐缺口: build=%s coverage=%s", res.BuildMode, res.SemanticCoverage)
	}
	sawGap, sawExtra := false, false
	for _, text := range server.textsSince(callsBefore) {
		switch {
		case strings.Contains(text, "FailingGap"):
			sawGap = true
		case strings.Contains(text, "ExtraSession"):
			sawExtra = true
		default:
			t.Fatalf("只应为缺口与新文件送审: %q", text[:min(60, len(text))])
		}
	}
	if !sawGap || !sawExtra {
		t.Fatalf("缺口与新文件都应送审: gap=%v extra=%v", sawGap, sawExtra)
	}
}

// TestGapFillSkippedDuringBackoff：有缺口时熔断器处于退避，内容变化的同步
// 保持 delta，且不发文档请求。退避期内送审一定被拒，此时走全量只会白白重写
// 整个索引。
func TestGapFillSkippedDuringBackoff(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 1, "fake-model"))
	root := newFixtureWorkspace(t)
	syncedWithGap(t, e, root, server)
	if state := e.embedClient.CircuitSnapshot().State; state != "backoff" {
		t.Fatalf("前置：唯一的请求失败后熔断器应处于退避: %s", state)
	}

	writeFixture(t, root, "extra.go", "package app\n\nfunc ExtraSession() {}\n")
	callsBefore := server.callCount()
	res, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil || res.BuildMode != "delta" {
		t.Fatalf("退避期间内容变化的同步应保持 delta: %+v err=%v", res, err)
	}
	if n := server.callCount() - callsBefore; n != 0 {
		t.Fatalf("退避期间不得发出文档请求: %d", n)
	}
}

// stuckGapFixture 是缺口里的内容一直被 provider 拒绝的场景：fixture 建成
// 完整覆盖后加入 stuck.go，它的 chunk 每次都收到 429（Retry-After 1 秒，
// 熔断器退避 1 秒），在一次 delta 里留下缺口。failing 置 false 后恢复正常。
type stuckGapFixture struct {
	e       *Engine
	root    string
	failing *atomic.Bool
}

func newStuckGapFixture(t *testing.T) stuckGapFixture {
	t.Helper()
	const dim = 8
	server := newEmbedServer(t, dim)
	failing := &atomic.Bool{}
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if failing.Load() && bytes.Contains(body, []byte("StuckGap")) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner.ServeHTTP(w, r)
	})
	f := stuckGapFixture{e: newTestEngineWith(t, embedOptions(server.ts.URL, dim, 1, "fake-model")), root: newFixtureWorkspace(t), failing: failing}
	ctx := context.Background()
	if res, err := f.e.Sync(ctx, syncRequest(f.root)); err != nil || res.SemanticCoverage != "100%" {
		t.Fatalf("前置：首建应完整覆盖: %+v err=%v", res, err)
	}
	failing.Store(true)
	writeFixture(t, f.root, "stuck.go", "package app\n\nfunc StuckGap() {}\n")
	if res, err := f.e.Sync(ctx, syncRequest(f.root)); err != nil || res.BuildMode != "delta" || res.SemanticCoverage == "100%" {
		t.Fatalf("前置：stuck.go 被拒应留下缺口: %+v err=%v", res, err)
	}
	return f
}

// waitOutOfBackoff 等索引请求的熔断器离开退避（429 声明的 1 秒）。
func (f stuckGapFixture) waitOutOfBackoff(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.e.embedClient.CircuitSnapshot().State == "backoff" {
		if time.Now().After(deadline) {
			t.Fatal("熔断器没有离开退避")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gap 是 active revision 里还没有向量、也没有被拒绝的 chunk 数。
func (f stuckGapFixture) gap(t *testing.T) int {
	t.Helper()
	m, _ := loadActiveManifest(t, f.e, f.root)
	return m.EmbeddableChunks() - m.VectorCount
}

// pause 走到“内容变化时暂停全量补缺”：一次全量补缺没有进展，下一次内容
// 变化的同步回到 delta，状态写明暂停。
func (f stuckGapFixture) pause(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	before := f.gap(t)
	f.waitOutOfBackoff(t)
	writeFixture(t, f.root, "one.go", "package app\n\nfunc OneMore() {}\n")
	first, err := f.e.Sync(ctx, syncRequest(f.root))
	if err != nil || first.BuildMode != "full:semantic-fill" {
		t.Fatalf("有缺口且熔断器不在退避时应尝试一次全量补缺: %+v err=%v", first, err)
	}
	if after := f.gap(t); after < before {
		t.Fatalf("前置：补缺口应没有进展: %d -> %d", before, after)
	}
	attempted, _ := loadActiveManifest(t, f.e, f.root)

	f.waitOutOfBackoff(t)
	writeFixture(t, f.root, "two.go", "package app\n\nfunc TwoMore() {}\n")
	second, err := f.e.Sync(ctx, syncRequest(f.root))
	paused, _ := loadActiveManifest(t, f.e, f.root)
	if err != nil || second.BuildMode != "delta" || len(paused.Segments) != len(attempted.Segments)+1 {
		t.Fatalf("补缺口没有进展后，内容变化的同步应回到 delta: build=%s segments %d -> %d err=%v",
			second.BuildMode, len(attempted.Segments), len(paused.Segments), err)
	}
	if reason := lastSemanticError(t, f.e, f.root); !strings.Contains(reason, gapFillPausedMarker) {
		t.Fatalf("状态应写明内容变化时暂停全量补缺: %q", reason)
	}
}

// TestGapFillWithoutProgressPausesFullRebuild：为补缺口走全量的构建发布后
// 缺口没有减少，之后内容变化的同步回到 delta，不再每次整份重写，
// workspace_status 写明暂停与解除条件；provider 恢复后内容没变的同步照常
// 补齐，暂停随之解除。
func TestGapFillWithoutProgressPausesFullRebuild(t *testing.T) {
	f := newStuckGapFixture(t)
	f.pause(t)

	f.failing.Store(false)
	f.waitOutOfBackoff(t)
	healed, err := f.e.Sync(context.Background(), syncRequest(f.root))
	if err != nil || healed.SemanticCoverage != "100%" {
		t.Fatalf("provider 恢复后内容没变的同步应补齐: %+v err=%v", healed, err)
	}
	if reason := lastSemanticError(t, f.e, f.root); strings.Contains(reason, gapFillPausedMarker) {
		t.Fatalf("补齐后暂停说明应消失: %q", reason)
	}
}

// TestGapFillPauseClearedWhenDeltaCompletesCoverage：暂停期间删掉一直被拒的
// stuck.go，delta 把覆盖补完整，暂停随之解除，状态不再在覆盖 100% 时显示
// 暂停说明。
func TestGapFillPauseClearedWhenDeltaCompletesCoverage(t *testing.T) {
	f := newStuckGapFixture(t)
	f.pause(t)

	if err := os.Remove(filepath.Join(f.root, "stuck.go")); err != nil {
		t.Fatal(err)
	}
	f.waitOutOfBackoff(t)
	cleared, err := f.e.Sync(context.Background(), syncRequest(f.root))
	if err != nil || cleared.BuildMode != "delta" || cleared.SemanticCoverage != "100%" {
		t.Fatalf("删掉被拒的文件后 delta 应把覆盖补完整: %+v err=%v", cleared, err)
	}
	if reason := lastSemanticError(t, f.e, f.root); strings.Contains(reason, gapFillPausedMarker) {
		t.Fatalf("覆盖完整后暂停说明应消失: %q", reason)
	}
}
