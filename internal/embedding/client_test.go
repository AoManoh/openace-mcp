package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// testConfig 构造指向 fake server 的最小配置。
func testConfig(baseURL string, dimension int) Config {
	return Config{
		Enabled: true, ProviderType: ProviderVoyage, BaseURL: baseURL,
		APIKey: "canary-secret-key", Model: "voyage-code-3", Dimension: dimension,
		BatchSize: 128, MaxConcurrency: 4, Timeout: 2 * time.Second, MaxRetries: 2,
	}
}

// fastRetry 让测试内重试等待近零。
func fastRetry(c *Client) {
	c.retry.BaseDelay = time.Millisecond
	c.retry.Jitter = func(d time.Duration) time.Duration { return d }
}

// vectorFor 生成确定性响应向量（首元素编码输入序号）。
func vectorFor(dim int, seed int) []float32 {
	v := make([]float32, dim)
	v[0] = float32(seed)
	return v
}

// embedRequest 是 fake server 解析的请求形状。
type embedRequest struct {
	Input           []string `json:"input"`
	Model           string   `json:"model"`
	InputType       *string  `json:"input_type"`
	OutputDimension *int     `json:"output_dimension"`
}

// respondEmbeddings 输出 index 逆序的合法响应（验证客户端按 index 重排）。
func respondEmbeddings(w http.ResponseWriter, texts []string, dim int) {
	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	items := make([]item, 0, len(texts))
	for i := len(texts) - 1; i >= 0; i-- {
		items = append(items, item{Embedding: vectorFor(dim, i), Index: i})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
}

func TestVoyageShapeAndOrdering(t *testing.T) {
	const dim = 4
	var got embedRequest
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		respondEmbeddings(w, got.Input, dim)
	}))
	defer ts.Close()

	client, err := NewClient(testConfig(ts.URL, dim))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	vectors, err := client.EmbedBatch(context.Background(), []string{"alpha", "beta", "gamma"}, InputDocument)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if auth != "Bearer canary-secret-key" {
		t.Fatalf("voyage 应携带 Bearer key: %q", auth)
	}
	if got.InputType == nil || *got.InputType != "document" || got.OutputDimension == nil || *got.OutputDimension != dim {
		t.Fatalf("voyage 形状应含 input_type/output_dimension: %+v", got)
	}
	for i, v := range vectors {
		if v[0] != float32(i) {
			t.Fatalf("index 逆序响应应被重排: pos=%d got=%v", i, v[0])
		}
	}
	if s := client.CircuitSnapshot(); s.State != "healthy" {
		t.Fatalf("成功后应 healthy: %+v", s)
	}
}

func TestOpenAIShapeKeyless(t *testing.T) {
	const dim = 3
	var got embedRequest
	var authPresent bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, authPresent = r.Header["Authorization"]
		_ = json.NewDecoder(r.Body).Decode(&got)
		respondEmbeddings(w, got.Input, dim)
	}))
	defer ts.Close()

	cfg := testConfig(ts.URL, dim)
	cfg.ProviderType = ProviderOpenAI
	cfg.APIKey = ""
	cfg.Model = "nomic-embed-code"
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.EmbedQuery(context.Background(), "q"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if authPresent {
		t.Fatalf("无 key 时不得发送 Authorization 头（K21）")
	}
	if got.InputType != nil || got.OutputDimension != nil {
		t.Fatalf("openai 形状不应含 voyage 扩展字段: %+v", got)
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	const dim = 2
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		respondEmbeddings(w, req.Input, dim)
	}))
	defer ts.Close()

	client, _ := NewClient(testConfig(ts.URL, dim))
	fastRetry(client)
	if _, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument); err != nil {
		t.Fatalf("429 后重试应成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("应恰好重试一次: %d", calls)
	}
}

func TestRetryOn503ThenSuccess(t *testing.T) {
	const dim = 2
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		respondEmbeddings(w, req.Input, dim)
	}))
	defer ts.Close()

	client, _ := NewClient(testConfig(ts.URL, dim))
	fastRetry(client)
	if _, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument); err != nil {
		t.Fatalf("503×2 后应成功: %v", err)
	}
}

func TestTimeoutClassifiedTransient(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer func() { close(release); ts.Close() }()

	cfg := testConfig(ts.URL, 2)
	cfg.Timeout = 50 * time.Millisecond
	cfg.MaxRetries = 0
	client, _ := NewClient(cfg)
	_, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument)
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassTransient || !strings.Contains(callErr.Message, "timed out") {
		t.Fatalf("超时应为 transient 且可行动: %v", err)
	}
	if s := client.CircuitSnapshot(); s.State != "backoff" {
		t.Fatalf("最终失败应进入退避: %+v", s)
	}
}

// TestMidBodyTimeoutClassifiedTransient 复现 F3：provider 返回 200 并开始
// 输出响应体后停滞，attempt 超时在流式解码中触发——必须分类为 transient
// 超时（可重试），不得误判为 permanent malformed（会计入 circuit 并把
// 整场构建的语义路熄火）。
func TestMidBodyTimeoutClassifiedTransient(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,`))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() { close(release); ts.Close() }()

	cfg := testConfig(ts.URL, 2)
	cfg.Timeout = 100 * time.Millisecond
	cfg.MaxRetries = 0
	client, _ := NewClient(cfg)
	_, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument)
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassTransient || !strings.Contains(callErr.Message, "timed out") {
		t.Fatalf("响应体中途超时应为 transient 超时: %v", err)
	}
}

// TestMidBodyCancelReturnsCallerError 调用方取消发生在响应体读取期间时，
// 错误必须原样返回 context.Canceled（不计 provider 失败，暗坑 K26）。
func TestMidBodyCancelReturnsCallerError(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,`))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() { close(release); ts.Close() }()

	client, _ := NewClient(testConfig(ts.URL, 2))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.EmbedBatch(ctx, []string{"a"}, InputDocument)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("调用方取消应原样返回: %v", err)
	}
	if s := client.CircuitSnapshot(); s.State == "backoff" {
		t.Fatalf("取消不得毒化 circuit: %+v", s)
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

	client, _ := NewClient(testConfig(ts.URL, 2))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.EmbedBatch(ctx, []string{"a"}, InputDocument)
		done <- err
	}()
	<-started
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 ctx 错误: %v", err)
	}
	if s := client.CircuitSnapshot(); s.State != "healthy" {
		t.Fatalf("取消不得计为 provider 失败（K26）: %+v", s)
	}
}

func TestCountMismatchRejectsBatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"embedding": vectorFor(2, 0), "index": 0},
		}})
	}))
	defer ts.Close()

	client, _ := NewClient(testConfig(ts.URL, 2))
	_, err := client.EmbedBatch(context.Background(), []string{"a", "b"}, InputDocument)
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassPermanent || !strings.Contains(callErr.Message, "count mismatch") {
		t.Fatalf("缺条应整批拒绝（K22）: %v", err)
	}
}

func TestDimensionMismatchRejectsBatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		respondEmbeddings(w, req.Input, 8) // 错误维度
	}))
	defer ts.Close()

	client, _ := NewClient(testConfig(ts.URL, 4))
	_, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument)
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || !strings.Contains(callErr.Message, "dimension mismatch") {
		t.Fatalf("维度不符应整批拒绝并指明变量（K22/D3）: %v", err)
	}
}

func TestDuplicateIndexRejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"embedding": vectorFor(2, 0), "index": 0},
			{"embedding": vectorFor(2, 0), "index": 0},
		}})
	}))
	defer ts.Close()
	client, _ := NewClient(testConfig(ts.URL, 2))
	_, err := client.EmbedBatch(context.Background(), []string{"a", "b"}, InputDocument)
	if err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("重复 index 应拒绝: %v", err)
	}
}

func TestBatchTooLargeAdaptiveSplit(t *testing.T) {
	const dim = 2
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) > 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"The max allowed tokens per submitted batch is 120000, your batch exceeds the limit"}`))
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			items = append(items, item{Embedding: vectorFor(dim, int(text[0])), Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer ts.Close()

	client, _ := NewClient(testConfig(ts.URL, dim))
	fastRetry(client)
	texts := []string{"a", "b", "c", "d", "e"}
	vectors, err := client.EmbedBatch(context.Background(), texts, InputDocument)
	if err != nil {
		t.Fatalf("超限批应拆分收敛（K23）: %v", err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("拆分后应返回全部向量: %d", len(vectors))
	}
	for i, text := range texts {
		if vectors[i][0] != float32(text[0]) {
			t.Fatalf("拆分后顺序错位: pos=%d", i)
		}
	}
	if calls < 4 {
		t.Fatalf("应发生实际拆分: calls=%d", calls)
	}
	if s := client.CircuitSnapshot(); s.State != "healthy" {
		t.Fatalf("拆批不是 provider 健康问题: %+v", s)
	}
}

func TestAuthFailureNoRetryAndKeyNotLeaked(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Provided API key is invalid"}`))
	}))
	defer ts.Close()

	client, _ := NewClient(testConfig(ts.URL, 2))
	fastRetry(client)
	_, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument)
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassAuth {
		t.Fatalf("401 应为 auth 类: %v", err)
	}
	if calls != 1 {
		t.Fatalf("auth 失败不应重试: %d", calls)
	}
	if strings.Contains(err.Error(), "canary-secret-key") {
		t.Fatalf("错误不得含 key（K21）: %v", err)
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("应给出可行动诊断（K33）: %v", err)
	}
	snapshot := client.CircuitSnapshot()
	if snapshot.State != "backoff" || strings.Contains(snapshot.LastError, "canary-secret-key") {
		t.Fatalf("状态不得含 key 且应退避: %+v", snapshot)
	}
}

func TestQuotaClassified(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"detail":"insufficient balance"}`))
	}))
	defer ts.Close()
	client, _ := NewClient(testConfig(ts.URL, 2))
	_, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument)
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassQuota {
		t.Fatalf("402 应为 quota 类（K33 欠费分类）: %v", err)
	}
}

func TestCircuitGateShortCircuits(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	cfg := testConfig(ts.URL, 2)
	cfg.MaxRetries = 0
	client, _ := NewClient(cfg)
	if _, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument); err == nil {
		t.Fatalf("503 应失败")
	}
	before := atomic.LoadInt32(&calls)
	_, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument)
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) || callErr.Class != reliability.ClassBackoff {
		t.Fatalf("退避期应返回 ClassBackoff（D10 依据）: %v", err)
	}
	if atomic.LoadInt32(&calls) != before {
		t.Fatalf("退避期不得发出请求")
	}
}

func TestConcurrencyBounded(t *testing.T) {
	const dim = 2
	var current, peak int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := atomic.AddInt32(&current, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if now <= old || atomic.CompareAndSwapInt32(&peak, old, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		respondEmbeddings(w, req.Input, dim)
	}))
	defer ts.Close()

	cfg := testConfig(ts.URL, dim)
	cfg.MaxConcurrency = 2
	client, _ := NewClient(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := client.EmbedBatch(context.Background(), []string{fmt.Sprintf("t%d", i)}, InputDocument); err != nil {
				t.Errorf("并发批 %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if p := atomic.LoadInt32(&peak); p > 2 {
		t.Fatalf("并发应受 MaxConcurrency 约束（K36）: peak=%d", p)
	}
}

func TestRPMBudgetBlocks(t *testing.T) {
	const dim = 2
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		respondEmbeddings(w, req.Input, dim)
	}))
	defer ts.Close()

	cfg := testConfig(ts.URL, dim)
	cfg.RPMBudget = 1
	client, _ := NewClient(cfg)
	if _, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument); err != nil {
		t.Fatalf("预算内应放行: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.EmbedBatch(ctx, []string{"b"}, InputDocument); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超预算应阻塞至窗口结束（此处以 ctx 截断验证）: %v", err)
	}
}

func TestNewClientRequiresEnabled(t *testing.T) {
	if _, err := NewClient(Config{Enabled: false, DisabledReason: "off"}); err == nil {
		t.Fatalf("未启用配置应报错")
	}
}
