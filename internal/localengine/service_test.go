package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// dimensionProbeText 与 embedding 包的探测文本一致，用来区分探测请求与文档、
// 查询请求。
const dimensionProbeText = "openACE embedding dimension probe"

// autoDimensionOptions 构造未显式配置维度、需要启动时探测的语义配置。
func autoDimensionOptions(url string) Options {
	opts := embedOptions(url, 0, 8, "probe-model")
	opts.Embedding.DimensionAuto = true
	return opts
}

func waitFor(d time.Duration) func(reliability.CircuitSnapshot) time.Duration {
	return func(reliability.CircuitSnapshot) time.Duration { return d }
}

func closeService(t *testing.T, service engine.Service) {
	t.Helper()
	if lifecycle, ok := service.(engine.Lifecycle); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := lifecycle.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}
}

// onlyProbeRequests 报告 provider 收到的请求是否全是维度探测：维度定下来之前
// 不应有文档或查询嵌入请求。
func onlyProbeRequests(server *embedServer) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, texts := range server.perCall {
		if len(texts) != 1 || texts[0] != dimensionProbeText {
			return false
		}
	}
	return true
}

// TestNewServiceStartsWhenDimensionProbeFails：配置了 embedding、未设维度、缓存
// 里也没有维度，启动时 provider 返回 503。服务应照常构造；检索、同步、仓库地图
// 与变更检测返回语义检索不可用的错误，写明探测失败原因、变量名与探测进度；
// 工作区状态照常返回并给出同样的原因；配置指纹与探测成功后的引擎一致；
// provider 只收到探测请求。
func TestNewServiceStartsWhenDimensionProbeFails(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	server := newEmbedServer(t, 8)
	server.setFailWhen(func([]string) bool { return true })
	opts := autoDimensionOptions(server.ts.URL)
	service, err := newService(opts, t.Logf, serviceTiming{startupProbe: time.Second, retryWait: waitFor(time.Hour)})
	if err != nil {
		t.Fatalf("维度探测失败时服务应照常构造: %v", err)
	}
	t.Cleanup(func() { closeService(t, service) })
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root}
	ctx := context.Background()

	wantInError := []string{"semantic retrieval unavailable", dimensionUnresolvedReason, "embedding dimension probe against", "HTTP 503", embedding.EnvDimension, "probe attempts failed so far"}
	checkErr := func(op string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s 在维度未定时应返回错误", op)
		}
		for _, want := range wantInError {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s 的错误缺少 %q: %v", op, want, err)
			}
		}
	}
	// 后台第一次探测在构造后立即进行，等它结束再核对“下次探测时刻”。
	deadline := time.Now().Add(5 * time.Second)
	for server.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	_, err = service.Search(ctx, engine.SearchRequest{Workspace: ref, Query: "user login"})
	checkErr("Search", err)
	if !strings.Contains(err.Error(), "next probe at") {
		t.Fatalf("后台探测失败后错误应写明下次探测时刻: %v", err)
	}
	_, err = service.Sync(ctx, engine.SyncRequest{Workspace: ref})
	checkErr("Sync", err)
	mapper, ok := service.(engine.RepoMapper)
	if !ok {
		t.Fatalf("服务应提供 repo_map")
	}
	_, err = mapper.RepoMap(ctx, engine.RepoMapRequest{Workspace: ref})
	checkErr("RepoMap", err)
	detector, ok := service.(engine.ChangeDetector)
	if !ok {
		t.Fatalf("服务应提供变更检测")
	}
	changed, err := detector.WorkspaceChanged(ctx, ref)
	checkErr("WorkspaceChanged", err)
	if changed {
		t.Fatalf("维度未定时不应报告需要同步")
	}

	inspector, ok := service.(engine.WorkspaceInspector)
	if !ok {
		t.Fatalf("服务应提供 workspace_status")
	}
	status, err := inspector.WorkspaceStatus(ctx, ref)
	if err != nil {
		t.Fatalf("维度未定时 workspace_status 应照常返回: %v", err)
	}
	if status.Stage != engine.IndexStageIdle || !strings.Contains(status.LastError, dimensionUnresolvedReason) {
		t.Fatalf("状态应为 idle 并写明原因: stage=%s last_error=%q", status.Stage, status.LastError)
	}
	if status.Semantic == nil || !status.Semantic.Enabled || status.Semantic.Dimension != 0 ||
		status.Semantic.ProviderState != "backoff" || status.Semantic.BackoffUntil == nil ||
		!strings.Contains(status.Semantic.LastError, "embedding dimension probe against") {
		t.Fatalf("semantic 状态应给出探测失败原因与下次探测时刻: %+v", status.Semantic)
	}
	statuses, err := inspector.ListWorkspaceStatuses(ctx)
	if err != nil || len(statuses) != 0 {
		t.Fatalf("维度未定时没有工作区跟踪器: %v %v", statuses, err)
	}

	identified, ok := service.(engine.ProfileIdentifier)
	if !ok || identified.EngineProfileFingerprint() != engineFingerprint(opts) {
		t.Fatalf("配置指纹应与 New 构造的引擎一致")
	}
	if !onlyProbeRequests(server) {
		t.Fatalf("维度定下来之前不应发出文档或查询嵌入请求: %v", server.perCall)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("OPENACE_CACHE_DIR"), "embedding-dimensions.json")); !os.IsNotExist(err) {
		t.Fatalf("探测失败时不应写维度缓存: %v", err)
	}
}

// TestNewServiceRecoversWhenProviderReturns：provider 恢复后后台探测成功，之后的
// 检索按正常流程建索引并以 hybrid 作答；状态给出探测到的维度；指纹不变；维度
// 写入缓存。
func TestNewServiceRecoversWhenProviderReturns(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	server := newEmbedServer(t, 8)
	var down atomic.Bool
	down.Store(true)
	server.setFailWhen(func([]string) bool { return down.Load() })
	opts := autoDimensionOptions(server.ts.URL)
	service, err := newService(opts, t.Logf, serviceTiming{startupProbe: time.Second, retryWait: waitFor(20 * time.Millisecond)})
	if err != nil {
		t.Fatalf("维度探测失败时服务应照常构造: %v", err)
	}
	t.Cleanup(func() { closeService(t, service) })
	pending := service.(engine.ProfileIdentifier).EngineProfileFingerprint()
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root}
	ctx := context.Background()
	if _, err := service.Search(ctx, engine.SearchRequest{Workspace: ref, Query: "user login"}); err == nil {
		t.Fatalf("provider 恢复前检索应报错")
	}

	down.Store(false)
	var result engine.Result
	deadline := time.Now().Add(10 * time.Second)
	for {
		result, err = service.Search(ctx, engine.SearchRequest{Workspace: ref, Query: "user login"})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider 恢复后检索应在后台探测成功后正常作答: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(result.RetrievalMode, "hybrid") || result.SemanticCoverage != "100%" {
		t.Fatalf("恢复后应以 hybrid 作答: mode=%s coverage=%s reason=%s", result.RetrievalMode, result.SemanticCoverage, result.DegradedReason)
	}
	status, err := service.(engine.WorkspaceInspector).WorkspaceStatus(ctx, ref)
	if err != nil || status.Semantic == nil || status.Semantic.Dimension != 8 {
		t.Fatalf("恢复后状态应给出探测到的维度: %+v err=%v", status.Semantic, err)
	}
	if got := service.(engine.ProfileIdentifier).EngineProfileFingerprint(); got != pending {
		t.Fatalf("探测成功前后指纹应一致: %s → %s", pending, got)
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, "embedding-dimensions.json"))
	if err != nil {
		t.Fatalf("探测成功后应写维度缓存: %v", err)
	}
	var table map[string]int
	if err := json.Unmarshal(raw, &table); err != nil || len(table) != 1 {
		t.Fatalf("维度缓存内容不符: %s", raw)
	}
	for _, dim := range table {
		if dim != 8 {
			t.Fatalf("维度缓存应为 8: %s", raw)
		}
	}
}

// TestNewServiceBoundsStartupProbeAndCloseCancelsRetry：provider 挂起不响应时，
// 服务构造在启动探测时限内返回（不等满 provider 超时）；后台探测进行中状态为
// candidate；Close 取消在途探测并及时返回。
func TestNewServiceBoundsStartupProbeAndCloseCancelsRetry(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	release := make(chan struct{})
	var requests atomic.Int32
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		hang.Close()
	})
	opts := autoDimensionOptions(hang.URL)
	opts.Embedding.Timeout = time.Minute
	started := time.Now()
	service, err := newService(opts, t.Logf, serviceTiming{startupProbe: 200 * time.Millisecond, retryWait: waitFor(time.Hour)})
	if err != nil {
		t.Fatalf("启动探测超时时服务应照常构造: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("启动探测应受时限约束，实际用了 %s", elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for requests.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	root := t.TempDir()
	status, err := service.(engine.WorkspaceInspector).WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil || status.Semantic == nil || status.Semantic.ProviderState != "candidate" {
		t.Fatalf("后台探测进行中状态应为 candidate: %+v err=%v", status.Semantic, err)
	}
	if _, err := service.Search(context.Background(), engine.SearchRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}, Query: "x"}); err == nil || !strings.Contains(err.Error(), "a probe request is in progress") {
		t.Fatalf("探测进行中错误应写明: %v", err)
	}
	closeStarted := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.(engine.Lifecycle).Close(ctx); err != nil {
		t.Fatalf("Close 应取消在途探测并返回: %v", err)
	}
	if elapsed := time.Since(closeStarted); elapsed > 3*time.Second {
		t.Fatalf("Close 用了 %s，没有取消在途探测", elapsed)
	}
}

// TestNewServiceWithoutProbeReturnsEngine：不需要探测（未配置 embedding、显式
// 配置了维度）时 NewService 与 New 相同，返回 *Engine，不发任何请求。
func TestNewServiceWithoutProbeReturnsEngine(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	server := newEmbedServer(t, 8)
	for name, opts := range map[string]Options{
		"lexical-only":       {},
		"explicit-dimension": embedOptions(server.ts.URL, 8, 8, "explicit"),
	} {
		service, err := newService(opts, t.Logf, serviceTiming{startupProbe: time.Second, retryWait: waitFor(time.Hour)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := service.(*Engine); !ok {
			t.Fatalf("%s: 不需要探测时应直接返回 *Engine，got %T", name, service)
		}
		closeService(t, service)
	}
	if calls := server.callCount(); calls != 0 {
		t.Fatalf("不需要探测时不应发请求，实际 %d 次", calls)
	}
}
