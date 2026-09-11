package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
)

// 先索引两个内容并恢复一次 500，再增加三个内容并令服务持续返回 429。
// 失败按构建阶段注入，不依赖 HTTP 到达次序：动态并发与重试释放名额后，
// “第几次请求”不再固定对应某个内容。验收仍要求 2/5 部分发布、退避期
// 不请求、不发布，以及恢复后 5/5 覆盖且每个内容只成功嵌入一次。
func TestProviderFaultConvergence(t *testing.T) {
	const dim = 8
	type callRecord struct {
		text string
		ok   bool
		at   time.Time
	}
	var mu sync.Mutex
	var calls []callRecord
	rateLimited := false
	transientSent := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		record := callRecord{text: req.Input[0], at: time.Now()}
		switch {
		case !transientSent:
			transientSent = true
			calls = append(calls, record)
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		case rateLimited:
			calls = append(calls, record)
			mu.Unlock()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		record.ok = true
		calls = append(calls, record)
		mu.Unlock()
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []item{{Embedding: fakeVector(dim, req.Input[0]), Index: 0}}})
	}))
	defer ts.Close()

	opts := Options{Embedding: embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderOpenAI, BaseURL: ts.URL,
		Model: "fake-model", Dimension: dim, BatchSize: 1, InitialConcurrency: 1,
		Timeout: 2 * time.Second, MaxRetries: 1,
	}}
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	writeFixture(t, root, "one.go", "package app\nfunc One() int { return 1 }\n")
	writeFixture(t, root, "two.go", "package app\nfunc Two() int { return 2 }\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	warm, _ := loadActiveManifest(t, e, root)
	if !warm.SemanticComplete() || warm.VectorCount != 2 {
		t.Fatalf("瞬时错误恢复后应覆盖 2 个内容: %+v", warm)
	}
	mu.Lock()
	if len(calls) != 3 {
		t.Fatalf("两个成功与一次暂态失败应为 3 次请求: %d", len(calls))
	}
	rateLimited = true
	mu.Unlock()
	writeFixture(t, root, "three.go", "package app\nfunc Three() int { return 3 }\n")
	writeFixture(t, root, "four.go", "package app\nfunc Four() int { return 4 }\n")
	writeFixture(t, root, "five.go", "package app\nfunc Five() int { return 5 }\n")

	// 限流结束前，新增内容保持未覆盖，已有向量仍参与检索。
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("provider 故障不得阻塞词法发布: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if manifest.SemanticComplete() || manifest.VectorCount != 2 {
		t.Fatalf("应发布部分覆盖(2/5): %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	mu.Lock()
	callsAfterBuild1 := len(calls)
	openedAt := calls[len(calls)-1].at
	mu.Unlock()

	// 退避窗口内同步保持当前 revision，也不产生 provider 请求。
	during, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil || during.IndexRevision != first.IndexRevision {
		t.Fatalf("退避窗口应 no-op: %+v err=%v", during, err)
	}
	mu.Lock()
	if len(calls) != callsAfterBuild1 {
		t.Fatalf("退避窗口不得出网: %d → %d", callsAfterBuild1, len(calls))
	}
	rateLimited = false
	mu.Unlock()

	// 服务恢复后仅补齐三个新增内容。
	deadline := time.After(15 * time.Second)
	var final string
	for {
		if e.embedClient.CircuitSnapshot().State != "backoff" {
			result, err := e.Sync(context.Background(), syncRequest(root))
			if err != nil {
				t.Fatal(err)
			}
			final = result.IndexRevision
			healed, _ := loadActiveManifest(t, e, root)
			if healed.SemanticComplete() {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatalf("恢复后未收敛至全覆盖")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if final == first.IndexRevision {
		t.Fatalf("补齐应发布新 revision")
	}

	mu.Lock()
	defer mu.Unlock()
	// Retry-After 为 1 秒；允许 100ms 测量误差。
	for _, record := range calls[callsAfterBuild1:] {
		if record.at.Sub(openedAt) < 900*time.Millisecond {
			t.Fatalf("退避开始后 %v 即发送请求", record.at.Sub(openedAt))
		}
	}
	// 每个内容只成功嵌入一次，已有向量不能因补齐而重复请求。
	succeeded := map[string]int{}
	for _, record := range calls {
		if record.ok {
			succeeded[record.text]++
		}
	}
	if len(succeeded) != 5 {
		t.Fatalf("应恰好覆盖 5 个唯一内容: %d", len(succeeded))
	}
	for text, count := range succeeded {
		if count != 1 {
			t.Fatalf("内容重复成功嵌入: %q ×%d", text[:min(30, len(text))], count)
		}
	}
	// 最多为两个初始成功、一次 500、三个新增内容各两次 429、三个恢复成功。
	// 熔断可能提前取消未完成的重试，所以按每批预算约束，不固定到达次序。
	if len(calls) > 12 || callsAfterBuild1 <= 3 {
		t.Fatalf("请求数未符合故障与重试预算: total=%d after_fault=%d", len(calls), callsAfterBuild1)
	}
}
