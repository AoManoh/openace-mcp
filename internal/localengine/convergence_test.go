package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
)

// TestProviderFaultConvergence 是 G7 端到端剧本：健康 → 瞬时 500（重试
// 预算内自愈）→ 429 风暴（circuit 打开，词法照常发布部分覆盖）→ 退避
// 窗口内零出网零发布（无风暴）→ circuit 恢复后补齐收敛至 100%，全程
// 每内容成功嵌入恰一次、总调用数有界。timeout 与 500 同属 ClassTransient
// （分类与退避在 reliability 单测覆盖），剧本用 500 代表该类以保持
// circuit 窗口秒级（429 的 Retry-After 可控）。
func TestProviderFaultConvergence(t *testing.T) {
	const dim = 8
	type callRecord struct {
		text string
		ok   bool
		at   time.Time
	}
	var mu sync.Mutex
	var calls []callRecord
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		n := len(calls) + 1
		record := callRecord{text: req.Input[0], at: time.Now()}
		// 剧本（batch=1，MaxRetries=1）：
		//   call1 OK | call2 500→call3 重试 OK | call4/5 429(RA:1) →
		//   batch 失败，circuit backoff(1s) | call6+ 恢复 OK。
		switch {
		case n == 2:
			calls = append(calls, record)
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		case n == 4 || n == 5:
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
		Model: "fake-model", Dimension: dim, BatchSize: 1, MaxConcurrency: 1,
		Timeout: 2 * time.Second, MaxRetries: 1,
	}}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)

	// 构建 1：健康段自愈 500，429 打开 circuit → 部分覆盖发布。
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

	// 退避窗口内：sync 是 no-op，零出网零发布（K30/G7 无风暴）。
	during, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil || during.IndexRevision != first.IndexRevision {
		t.Fatalf("退避窗口应 no-op: %+v err=%v", during, err)
	}
	mu.Lock()
	if len(calls) != callsAfterBuild1 {
		t.Fatalf("退避窗口不得出网: %d → %d", callsAfterBuild1, len(calls))
	}
	mu.Unlock()

	// circuit 恢复（candidate）后补齐收敛。
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
	// 无风暴：退避期（打开后 ~0.9s 内）零请求到达。
	for _, record := range calls[callsAfterBuild1:] {
		if record.at.Sub(openedAt) < 900*time.Millisecond {
			t.Fatalf("circuit 打开后 %v 即出网（违反 G7 gate）", record.at.Sub(openedAt))
		}
	}
	// 每内容成功嵌入恰一次（G7 费用口径）。
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
			t.Fatalf("内容重复付费（违反 G7）: %q ×%d", text[:min(30, len(text))], count)
		}
	}
	// 总调用有界：1 OK + (500+重试) + 429×2 + 补齐 3 = 8。
	if len(calls) != 8 {
		texts := make([]string, 0, len(calls))
		for _, record := range calls {
			texts = append(texts, record.text[:min(20, len(record.text))])
		}
		t.Fatalf("总调用数应为 8（有界，无风暴）: %d %v", len(calls), strings.Join(texts, " | "))
	}
}
