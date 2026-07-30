package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

func testConfig(baseURL string) Config {
	return Config{
		Enabled: true, ProviderType: ProviderVoyage, BaseURL: baseURL,
		APIKey: "canary-rerank-key", Model: "rerank-2.5", MaxTokens: 200000,
		Timeout: 2 * time.Second, MaxRetries: 2,
	}
}

func fastRetry(c *Client) {
	c.retry.BaseDelay = time.Millisecond
	c.retry.Jitter = func(d time.Duration) time.Duration { return d }
}

func docs(n int) []Document {
	out := make([]Document, n)
	for i := range out {
		out[i] = Document{ID: fmt.Sprintf("chunk-%d", i), Text: fmt.Sprintf("path/file.go:%d-%d f%d\nfunc f%d() {}", i*10, i*10+5, i, i)}
	}
	return out
}

// voyageRequest 是 fake server 解析的请求形状。
type voyageRequest struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	Texts     []string `json:"texts"`
	Model     *string  `json:"model"`
	TopK      *int     `json:"top_k"`
}

func TestVoyageShapeAndOrdering(t *testing.T) {
	var got voyageRequest
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		// 逆相关性返回：最后一个文档分最高；乱序输出验证客户端排序。
		type item struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		items := []item{{Index: 0, RelevanceScore: 0.1}, {Index: 2, RelevanceScore: 0.9}, {Index: 1, RelevanceScore: 0.5}}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer ts.Close()

	client, err := NewClient(testConfig(ts.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	hits, sent, err := client.Rerank(context.Background(), "how to sync", docs(3))
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if auth != "Bearer canary-rerank-key" {
		t.Fatalf("voyage 应携带 Bearer key: %q", auth)
	}
	if got.Model == nil || *got.Model != "rerank-2.5" || got.TopK == nil || *got.TopK != 3 || len(got.Documents) != 3 {
		t.Fatalf("voyage 请求形状不符: %+v", got)
	}
	if sent != 3 || len(hits) != 3 {
		t.Fatalf("sent=%d hits=%d", sent, len(hits))
	}
	if hits[0].ID != "chunk-2" || hits[1].ID != "chunk-1" || hits[2].ID != "chunk-0" {
		t.Fatalf("应按相关性降序: %+v", hits)
	}
}

func TestCohereStyleResultsKeyParsed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.8},{"index":0,"relevance_score":0.3}]}`))
	}))
	defer ts.Close()
	client, _ := NewClient(testConfig(ts.URL))
	hits, _, err := client.Rerank(context.Background(), "q", docs(2))
	if err != nil || len(hits) != 2 || hits[0].ID != "chunk-1" {
		t.Fatalf("Cohere/Jina 式 results 键应可解析（D3）: %+v err=%v", hits, err)
	}
}

func TestTEIShapeKeyless(t *testing.T) {
	var got voyageRequest
	var authPresent bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, authPresent = r.Header["Authorization"]
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`[{"index":0,"score":0.7},{"index":1,"score":0.2}]`))
	}))
	defer ts.Close()

	cfg := testConfig(ts.URL)
	cfg.ProviderType = ProviderTEI
	cfg.APIKey = ""
	cfg.Model = ""
	client, _ := NewClient(cfg)
	hits, sent, err := client.Rerank(context.Background(), "q", docs(2))
	if err != nil || sent != 2 || len(hits) != 2 {
		t.Fatalf("TEI 形状: hits=%v sent=%d err=%v", hits, sent, err)
	}
	if authPresent {
		t.Fatalf("keyless 不得发送 Authorization 头（K21）")
	}
	if len(got.Texts) != 2 || len(got.Documents) != 0 || got.Model != nil {
		t.Fatalf("TEI 请求应为 {query,texts}: %+v", got)
	}
}

func TestTokenCapTruncatesTail(t *testing.T) {
	var got voyageRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		type item struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		items := make([]item, len(got.Documents))
		for i := range items {
			items[i] = item{Index: i, RelevanceScore: 1 - float64(i)*0.1}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer ts.Close()

	cfg := testConfig(ts.URL)
	cfg.MaxTokens = 40 // 每文档约 12 tokens + query 重复计量 → 只容纳前 2 个
	client, _ := NewClient(cfg)
	hits, sent, err := client.Rerank(context.Background(), "q", docs(10))
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if sent >= 10 || sent == 0 {
		t.Fatalf("超预算应截断尾部（K28）: sent=%d", sent)
	}
	if len(got.Documents) != sent || len(hits) != sent {
		t.Fatalf("送审数应与 sent 一致: docs=%d hits=%d sent=%d", len(got.Documents), len(hits), sent)
	}
}

func TestZeroBudgetSkipsCall(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer ts.Close()
	cfg := testConfig(ts.URL)
	cfg.MaxTokens = 1
	client, _ := NewClient(cfg)
	hits, sent, err := client.Rerank(context.Background(), strings.Repeat("q", 100), docs(3))
	if err != nil || hits != nil || sent != 0 {
		t.Fatalf("预算不足应返回空重排且不出网: hits=%v sent=%d err=%v", hits, sent, err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("不应发出请求")
	}
}

func TestInvalidIndexRejected(t *testing.T) {
	for name, body := range map[string]string{
		"越界": `{"data":[{"index":9,"relevance_score":0.5}]}`,
		"重复": `{"data":[{"index":0,"relevance_score":0.5},{"index":0,"relevance_score":0.4}]}`,
		"负数": `{"data":[{"index":-1,"relevance_score":0.5}]}`,
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		client, _ := NewClient(testConfig(ts.URL))
		_, _, err := client.Rerank(context.Background(), "q", docs(2))
		callErr := &reliability.CallError{}
		if !errors.As(err, &callErr) || callErr.Class != reliability.ClassPermanent || !strings.Contains(callErr.Message, "index") {
			t.Fatalf("%s index 应整次拒绝（K28）: %v", name, err)
		}
		ts.Close()
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"relevance_score":0.5}]}`))
	}))
	defer ts.Close()
	client, _ := NewClient(testConfig(ts.URL))
	fastRetry(client)
	if _, _, err := client.Rerank(context.Background(), "q", docs(1)); err != nil {
		t.Fatalf("429 后重试应成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("应恰好重试一次: %d", calls)
	}
}

func TestTimeoutTransientAndCircuit(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer func() { close(release); ts.Close() }()

	cfg := testConfig(ts.URL)
	cfg.Timeout = 50 * time.Millisecond
	cfg.MaxRetries = 0
	client, _ := NewClient(cfg)
	_, _, err := client.Rerank(context.Background(), "q", docs(1))
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassTransient {
		t.Fatalf("超时应为 transient: %v", err)
	}
	// 退避期直接短路（D8 rerank 降级判定依据）。
	_, _, err = client.Rerank(context.Background(), "q", docs(1))
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassBackoff {
		t.Fatalf("退避期应返回 ClassBackoff: %v", err)
	}
}

func TestCancelDoesNotPoisonCircuit(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		started <- struct{}{}
		<-release
	}))
	defer func() { close(release); ts.Close() }()

	client, _ := NewClient(testConfig(ts.URL))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := client.Rerank(ctx, "q", docs(1))
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 ctx 错误: %v", err)
	}
	if s := client.CircuitSnapshot(); s.State != "healthy" {
		t.Fatalf("取消不得计为失败（K26）: %+v", s)
	}
}

func TestAuthErrorKeyNotLeaked(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid api key"}`))
	}))
	defer ts.Close()
	client, _ := NewClient(testConfig(ts.URL))
	_, _, err := client.Rerank(context.Background(), "q", docs(1))
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassAuth {
		t.Fatalf("401 应为 auth 类: %v", err)
	}
	if strings.Contains(err.Error(), "canary-rerank-key") {
		t.Fatalf("错误不得含 key（K21）: %v", err)
	}
}

func TestEmptyDocsNoop(t *testing.T) {
	client, _ := NewClient(testConfig("http://127.0.0.1:1"))
	hits, sent, err := client.Rerank(context.Background(), "q", nil)
	if hits != nil || sent != 0 || err != nil {
		t.Fatalf("空候选应 no-op: %v %d %v", hits, sent, err)
	}
}
