package localengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestSemanticDispatchStartsPendingBatchesDuringRetryWait(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	successes := map[string]int{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Input) != 1 {
			http.Error(w, "input", 400)
			return
		}
		mu.Lock()
		starts = append(starts, time.Now())
		n := len(starts)
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		successes[req.Input[0]]++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": fakeVector(8, req.Input[0])}}})
	}))
	defer ts.Close()
	opts := embedOptions(ts.URL, 8, 1, "fake-model")
	opts.Embedding.InitialConcurrency = 1
	opts.Embedding.MaxRetries = 1
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFixture(t, root, fmt.Sprintf("f%d.go", i), fmt.Sprintf("package app\nfunc F%d() int { return %d }\n", i, i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	m, _ := loadActiveManifest(t, e, root)
	if !m.SemanticComplete() || m.VectorCount != 6 {
		t.Fatalf("完整索引未发布: %d/%d", m.VectorCount, m.Counts.Chunks)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 8 || len(successes) != 6 {
		t.Fatalf("重试或成功数错误: attempts=%d unique=%d", len(starts), len(successes))
	}
	if starts[2].Sub(starts[0]) >= 900*time.Millisecond {
		t.Fatalf("两个批次睡眠期间未开始待处理批次: %v", starts[2].Sub(starts[0]))
	}
	for text, count := range successes {
		if count != 1 {
			t.Fatalf("内容成功嵌入多次: %q count=%d", text, count)
		}
	}
	if s := e.embedClient.GovernorSnapshot(); s.InFlight != 0 {
		t.Fatalf("构建结束后许可未释放: %+v", s)
	}
}

func TestSemanticDispatchGrowsBeyondInitialWindow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("自动扩窗仅在 Linux 有资源采样；其他平台维持当前窗口")
	}
	const initial = 16
	var mu sync.Mutex
	active, peak, calls := 0, 0, 0
	initialReady := make(chan struct{})
	releaseInitial := make(chan struct{})
	releaseNext := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		calls++
		n := calls
		peak = max(peak, active)
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		if n == initial {
			close(initialReady)
		}
		if n == 2*initial+1 {
			close(releaseNext)
		}
		gate := releaseNext
		if n <= initial {
			gate = releaseInitial
		}
		// 第二组须同时收到 17 个请求才返回，避免本地 fsync 耗时使一个
		// 很快的服务从未积累足够在途请求，从而把磁盘瓶颈误判为固定上限。
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, .2, .3, .4, .5, .6, .7, .8}}}})
	}))
	defer ts.Close()
	opts := embedOptions(ts.URL, 8, 1, "fake-model")
	opts.Embedding.InitialConcurrency = initial
	e := newTestEngineWith(t, opts)
	root := t.TempDir()
	for i := 0; i < 128; i++ {
		writeFixture(t, root, fmt.Sprintf("f%03d.go", i), fmt.Sprintf("package app\nfunc F%03d() int { return %d }\n", i, i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.Sync(ctx, syncRequest(root)); done <- err }()
	select {
	case <-initialReady:
		if e.embedClient.GovernorSnapshot().ResourceReason == "resource-unknown" {
			cancel()
			<-done
			t.Skip("当前 Linux 资源信息不完整，产品按约定维持窗口，不进行扩张验收")
		}
		close(releaseInitial)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m, _ := loadActiveManifest(t, e, root)
	mu.Lock()
	defer mu.Unlock()
	if !m.SemanticComplete() || m.VectorCount != 128 || calls != 128 {
		t.Fatalf("完整性或请求数: %d/%d calls=%d", m.VectorCount, m.Counts.Chunks, calls)
	}
	if peak <= initial {
		t.Fatalf("实际 HTTP 并发未越过初始窗口: peak=%d snapshot=%+v", peak, e.embedClient.GovernorSnapshot())
	}
}
