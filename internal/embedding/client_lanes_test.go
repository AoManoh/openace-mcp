package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// 车道分离与治理器升级路径测试(任务 T6,验收 G1/G3/G4 的包级钉住)。
// fake provider 按 input_type 区分行为:document 全 429、query 正常——
// 模拟"索引风暴中交互查询是否幸存"。

// laneServer 返回按车道分流的 fake provider:document 请求按 docStatus
// 应答(429/500 等),query 请求恒 200。
func laneServer(t *testing.T, dim int, docStatus int, docCalls, queryCalls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input     []string `json:"input"`
			InputType string   `json:"input_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.InputType == string(InputQuery) {
			queryCalls.Add(1)
		} else {
			docCalls.Add(1)
			if docStatus != http.StatusOK {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "injected", docStatus)
				return
			}
		}
		vecs := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			embedding := make([]float32, dim)
			embedding[0] = 1
			vecs[i] = map[string]any{"embedding": embedding, "index": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": vecs})
	}))
}

func laneConfig(url string, governorOff bool) Config {
	return Config{
		Enabled: true, ProviderType: ProviderVoyage, BaseURL: url,
		Model: "m", Dimension: 4, BatchSize: 8, MaxConcurrency: 2,
		MaxRetries: 0, Timeout: 5 * time.Second, TemplateVersion: "t",
		GovernorDisabled: governorOff,
	}
}

// TestQueryLaneSurvivesIndexStorm(G1 包级):索引车道持续 429 直至熔断,
// 查询车道全程正常——独立熔断成立。
func TestQueryLaneSurvivesIndexStorm(t *testing.T) {
	var docCalls, queryCalls atomic.Int64
	server := laneServer(t, 4, http.StatusTooManyRequests, &docCalls, &queryCalls)
	defer server.Close()
	client, err := NewClient(laneConfig(server.URL, false))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 索引风暴:反复失败直到索引车道熔断(首个 429 冷启动即达速率地板,
	// 第二次最终失败进熔断)。
	sawBackoff := false
	for i := 0; i < 5; i++ {
		_, err := client.EmbedBatch(ctx, []string{"doc"}, InputDocument)
		if err == nil {
			t.Fatal("document 车道应持续失败")
		}
		if callErr := asCallError(err); callErr != nil && callErr.Class == reliability.ClassBackoff {
			sawBackoff = true
			break
		}
	}
	if !sawBackoff {
		t.Fatalf("速率地板后持续 429 必须升级为熔断(保险丝语义): index snapshot=%+v", client.CircuitSnapshot())
	}
	// 索引车道熔断中,查询车道必须照常成功。
	if _, err := client.EmbedQuery(ctx, "interactive"); err != nil {
		t.Fatalf("索引熔断期查询车道必须存活: %v", err)
	}
	if queryCalls.Load() == 0 {
		t.Fatal("查询请求未到达 provider")
	}
}

// TestEscapeHatchRestoresSharedCircuit(G4/逃生门):governor=off 时回到
// 共用单熔断旧行为——索引失败后查询同样被熔断拦截。
func TestEscapeHatchRestoresSharedCircuit(t *testing.T) {
	var docCalls, queryCalls atomic.Int64
	server := laneServer(t, 4, http.StatusTooManyRequests, &docCalls, &queryCalls)
	defer server.Close()
	client, err := NewClient(laneConfig(server.URL, true))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.EmbedBatch(ctx, []string{"doc"}, InputDocument); err == nil {
		t.Fatal("document 应失败")
	}
	_, err = client.EmbedQuery(ctx, "interactive")
	callErr := asCallError(err)
	if callErr == nil || callErr.Class != reliability.ClassBackoff {
		t.Fatalf("逃生门下必须回到共用熔断旧行为(查询被退避拦截),得到: %v", err)
	}
	if queryCalls.Load() != 0 {
		t.Fatal("逃生门共用熔断下查询请求不应发出")
	}
}

// TestRateLimitBelowFloorDoesNotTripCircuit:治理器未到地板的 429 最终
// 失败不进熔断(油门先于保险丝)。构造:先积累高实测吞吐,再单次 429。
func TestRateLimitBelowFloorDoesNotTripCircuit(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	var docCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input     []string `json:"input"`
			InputType string   `json:"input_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		docCalls.Add(1)
		if int(status.Load()) != http.StatusOK {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "injected", int(status.Load()))
			return
		}
		vecs := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vecs[i] = map[string]any{"embedding": []float32{1, 0, 0, 0}, "index": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": vecs})
	}))
	defer server.Close()
	client, err := NewClient(laneConfig(server.URL, false))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 大 token 量成功调用,把实测吞吐抬高——之后首个 429 的学习速率
	// 远在地板之上。
	big := strings.Repeat("x", 400_000)
	if _, err := client.EmbedBatch(ctx, []string{big}, InputDocument); err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusTooManyRequests)
	if _, err := client.EmbedBatch(ctx, []string{"doc"}, InputDocument); err == nil {
		t.Fatal("429 应失败")
	}
	if snap := client.CircuitSnapshot(); snap.State != "healthy" {
		t.Fatalf("未到速率地板的 429 不得熔断: %+v", snap)
	}
	if client.GovernorSnapshot().TargetTokensPerMin <= governorFloorForTest() {
		t.Fatalf("学习速率应远高于地板: %+v", client.GovernorSnapshot())
	}
}

func governorFloorForTest() int { return 40_000 }
