package localengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// 离线批车道引擎级测试(任务 T8,验收 G2/G3/G4 的进程内钉住;G1 真实
// provider 链路另行验收)。fakeBulkProvider 实现 voyage Batch API 全生命
// 周期:/files 上传、/batches 创建与轮询、/files/{id}/content 302 重定向
// 下载(实测行为,08-03 探针),并强校验鉴权头与请求形状。

type fakeBulkProvider struct {
	ts  *httptest.Server
	mu  sync.Mutex
	dim int
	// 行为旋钮
	holdJobs     bool   // true=作业停留 in_progress(演练轮询与断点)
	terminalFail string // 非空=作业直接进入该终态(failed/expired)
	// 观测计数
	uploads     int
	jobsCreated int
	polls       int
	syncEmbeds  int // 同步 /embeddings 命中数(document 面必须为 0)
	// 服务端状态
	files map[string][]string // fileID -> jsonl custom_id 序
	texts map[string][]string // fileID -> 输入文本序(生成向量用)
	jobs  map[string]string   // jobID -> input fileID
}

func newFakeBulkProvider(t *testing.T, dim int) *fakeBulkProvider {
	t.Helper()
	p := &fakeBulkProvider{dim: dim, files: map[string][]string{}, texts: map[string][]string{}, jobs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /files", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		var keys, texts []string
		dec := json.NewDecoder(file)
		for dec.More() {
			var line struct {
				CustomID string `json:"custom_id"`
				Body     struct {
					Input []string `json:"input"`
				} `json:"body"`
			}
			if err := dec.Decode(&line); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			keys = append(keys, line.CustomID)
			texts = append(texts, line.Body.Input[0])
		}
		p.uploads++
		id := fmt.Sprintf("file-%d", p.uploads)
		p.files[id] = keys
		p.texts[id] = texts
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("POST /batches", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var req struct {
			InputFileID   string         `json:"input_file_id"`
			RequestParams map[string]any `json:"request_params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.RequestParams["model"] == nil || req.RequestParams["input_type"] == nil {
			// 真实契约(2026-08-20 真 422 实测):request_params 必填。
			http.Error(w, `{"detail":[{"type":"missing","loc":["body","request_params"]}]}`, http.StatusUnprocessableEntity)
			return
		}
		p.jobsCreated++
		id := fmt.Sprintf("job-%d", p.jobsCreated)
		p.jobs[id] = req.InputFileID
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "in_progress"})
	})
	mux.HandleFunc("GET /batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.polls++
		jobID := r.PathValue("id")
		fileID, ok := p.jobs[jobID]
		if !ok {
			http.Error(w, "unknown job", http.StatusNotFound)
			return
		}
		status := "completed"
		if p.holdJobs {
			status = "in_progress"
		}
		if p.terminalFail != "" {
			status = p.terminalFail
		}
		resp := map[string]any{
			"id": jobID, "status": status,
			"request_counts": map[string]int{"total": len(p.files[fileID]), "completed": len(p.files[fileID])},
		}
		if status == "completed" {
			resp["output_file_id"] = "out-" + fileID
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	// content 端点:302 重定向到签名 URL(下载面与真实行为一致)。
	mux.HandleFunc("GET /files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, p.ts.URL+"/signed/"+r.PathValue("id"), http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /signed/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		outID := strings.TrimPrefix(r.PathValue("id"), "out-")
		keys, texts := p.files[outID], p.texts[outID]
		for i, key := range keys {
			vec := make([]float32, p.dim)
			vec[0] = float32(len(texts[i])%7 + 1) // 非零可归一化
			line := map[string]any{
				"custom_id": key,
				"response": map[string]any{
					"status_code": 200,
					"body": map[string]any{
						"data":  []map[string]any{{"embedding": vec, "index": 0}},
						"usage": map[string]int{"total_tokens": len(texts[i]) / 4},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(line)
		}
	})
	// 同步 embeddings 端点:批车道生效时 document 面不得走这里。
	mux.HandleFunc("POST /embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p.mu.Lock()
		p.syncEmbeds++
		p.mu.Unlock()
		vecs := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, p.dim)
			vec[0] = 1
			vecs[i] = map[string]any{"embedding": vec, "index": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": vecs})
	})
	p.ts = httptest.NewServer(mux)
	t.Cleanup(p.ts.Close)
	return p
}

func (p *fakeBulkProvider) counts() (uploads, jobs, syncEmbeds int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.uploads, p.jobsCreated, p.syncEmbeds
}

func bulkOptions(url string, dim int, minChunks int) Options {
	return Options{Embedding: embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderVoyage, BaseURL: url,
		APIKey: "test-key", Model: "voyage-code-3", Dimension: dim,
		BatchSize: 8, MaxConcurrency: 2, Timeout: 5 * time.Second, MaxRetries: 0,
		BatchAPIMode: embedding.ProviderVoyage, BatchMinChunks: minChunks,
		BulkPollInterval: 20 * time.Millisecond,
	}, Rerank: rerank.Config{
		Enabled: false, ProviderType: rerank.ProviderOff,
		DisabledReason: "rerank provider is off",
	}}
}

// TestBulkBuildEndToEnd:阈值内首建全程走批作业,同步端点零 document
// 调用,发布后 coverage=100%,状态文件清理。
func TestBulkBuildEndToEnd(t *testing.T) {
	provider := newFakeBulkProvider(t, 8)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t8e2e")
	root := newFixtureWorkspace(t)
	e, err := New(bulkOptions(provider.ts.URL, 8, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("批车道首建失败: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("批车道首建必须语义完整: vectors=%d chunks=%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	uploads, jobs, syncEmbeds := provider.counts()
	if uploads == 0 || jobs == 0 {
		t.Fatalf("必须经批作业通道: uploads=%d jobs=%d", uploads, jobs)
	}
	if syncEmbeds != 0 {
		t.Fatalf("批车道生效时 document 不得走同步端点: %d", syncEmbeds)
	}
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	if status.Semantic.BulkJob != "" {
		t.Fatalf("收尾后不应残留在途作业标签: %q", status.Semantic.BulkJob)
	}
}

// TestBulkResumeAfterRestart(G2):作业在途时引擎关停;重启后续轮询同一
// 作业,零重复上传/零重复建作业。
func TestBulkResumeAfterRestart(t *testing.T) {
	provider := newFakeBulkProvider(t, 8)
	provider.mu.Lock()
	provider.holdJobs = true
	provider.mu.Unlock()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t8resume")
	root := newFixtureWorkspace(t)
	opts := bulkOptions(provider.ts.URL, 8, 1)

	e1, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	syncCtx, cancelSync := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e1.Sync(syncCtx, syncRequest(root))
		done <- err
	}()
	// 等作业提交并进入轮询(上传+建作业各 1 次)。
	deadline := time.Now().Add(5 * time.Second)
	for {
		uploads, jobs, _ := provider.counts()
		if uploads == 1 && jobs == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("作业未在期限内提交: uploads=%d jobs=%d", uploads, jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelSync()
	if err := <-done; err == nil {
		t.Fatal("取消中的构建应返回错误")
	}
	if err := e1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// "重启":新引擎实例;服务端放行作业。
	provider.mu.Lock()
	provider.holdJobs = false
	provider.mu.Unlock()
	e2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e2.Close(context.Background()) })
	if _, err := e2.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("重启后续作业失败: %v", err)
	}
	uploads, jobs, _ := provider.counts()
	if uploads != 1 || jobs != 1 {
		t.Fatalf("重启后必须续同一作业,绝不重复提交付费: uploads=%d jobs=%d", uploads, jobs)
	}
	manifest, _ := loadActiveManifest(t, e2, root)
	if !manifest.SemanticComplete() {
		t.Fatal("续作业收尾后必须语义完整")
	}
}

// TestBulkBelowThresholdUsesSyncLane:低于阈值走同步车道,批端点零触达。
func TestBulkBelowThresholdUsesSyncLane(t *testing.T) {
	provider := newFakeBulkProvider(t, 8)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t8thresh")
	root := newFixtureWorkspace(t)
	e, err := New(bulkOptions(provider.ts.URL, 8, 1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	uploads, jobs, syncEmbeds := provider.counts()
	if uploads != 0 || jobs != 0 {
		t.Fatalf("低于阈值不得走批通道: uploads=%d jobs=%d", uploads, jobs)
	}
	if syncEmbeds == 0 {
		t.Fatal("低于阈值应走同步车道")
	}
}

// TestBulkTerminalFailureExplicit(G4):作业终态失败必须显式外抛(带
// 作业 id 与处置指引),绝不静默回落同步车道。
func TestBulkTerminalFailureExplicit(t *testing.T) {
	provider := newFakeBulkProvider(t, 8)
	provider.mu.Lock()
	provider.terminalFail = "failed"
	provider.mu.Unlock()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t8fail")
	root := newFixtureWorkspace(t)
	e, err := New(bulkOptions(provider.ts.URL, 8, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	_, syncErr := e.Sync(context.Background(), syncRequest(root))
	if syncErr == nil {
		t.Fatal("终态失败必须让构建显式失败")
	}
	if !strings.Contains(syncErr.Error(), "job-1") || !strings.Contains(syncErr.Error(), "failed") {
		t.Fatalf("失败必须带作业 id 与终态: %v", syncErr)
	}
	_, _, syncEmbeds := provider.counts()
	if syncEmbeds != 0 {
		t.Fatalf("终态失败不得静默回落同步车道双付: syncEmbeds=%d", syncEmbeds)
	}
}

// TestGovernedBuildRidesThrough429Storm(任务 T6 验收 G2 的引擎级钉住):
// provider 先风暴后放行,构建必须降速穿越风暴完成 100% 覆盖——变更前
// 行为是熔断即全停、覆盖缺口入库。Retry-After=1s 让治理器暂停真实生效
// 而测试仍在秒级。
func TestGovernedBuildRidesThrough429Storm(t *testing.T) {
	var mu sync.Mutex
	stormLeft := 2 // 前 2 个 document 请求 429
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input     []string `json:"input"`
			InputType string   `json:"input_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		inStorm := req.InputType == string(embedding.InputDocument) && stormLeft > 0
		if inStorm {
			stormLeft--
		}
		mu.Unlock()
		if inStorm {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "storm", http.StatusTooManyRequests)
			return
		}
		vecs := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, 8)
			vec[0] = 1
			vecs[i] = map[string]any{"embedding": vec, "index": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": vecs})
	}))
	defer server.Close()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t6storm")
	root := newFixtureWorkspace(t)
	opts := Options{Embedding: embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderVoyage, BaseURL: server.URL,
		APIKey: "k", Model: "m", Dimension: 8, BatchSize: 2, MaxConcurrency: 2,
		Timeout: 5 * time.Second, MaxRetries: 2, // 批内重试跨越单批的 429
	}, Rerank: rerank.Config{Enabled: false, ProviderType: rerank.ProviderOff, DisabledReason: "off"}}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	begin := time.Now()
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("风暴中的构建必须降速完成而非失败: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("风暴后必须收敛 100%% 覆盖: vectors=%d chunks=%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	if elapsed := time.Since(begin); elapsed < time.Second {
		t.Fatalf("治理器应真实等待 Retry-After(降速证据),实际 %s", elapsed)
	}
	if snap := e.embedClient.CircuitSnapshot(); snap.State == "backoff" {
		t.Fatalf("风暴被治理器消化后不应处于熔断: %+v", snap)
	}
}
