package localengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serveFakeEmbedding 以 openai 形状返回确定性向量(复用 fakeVector)。
func serveFakeEmbedding(t *testing.T, w http.ResponseWriter, r *http.Request, dim int) {
	t.Helper()
	var req struct {
		Input []string `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	items := make([]item, 0, len(req.Input))
	for i, text := range req.Input {
		items = append(items, item{Embedding: fakeVector(dim, text), Index: i})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
}

// 灰度反馈 P1 定性判定(2026-08-06):"一个 workspace 的全量 embedding
// 会阻塞其他【索引已就绪】workspace 的同步检索"。本测试以慢速 fake
// provider 复现该场景——若就绪仓检索被拖到秒级以上,P1 成立(全局锁);
// 若毫秒级返回,P1 的正确定性是"冷仓首建撞 provider 带宽/工具超时",
// 修复方向为 P2(有界等待默认值)而非锁改造。
func TestReadyWorkspaceUnblockedDuringForeignEmbedding(t *testing.T) {
	var slow atomic.Bool
	dim := 8
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			time.Sleep(300 * time.Millisecond) // 模拟大仓嵌入期的 provider 排队
		}
		serveFakeEmbedding(t, w, r, dim)
	}))
	defer ts.Close()
	e := newTestEngineWith(t, embedOptions(ts.URL, dim, 4, "fake-model"))

	// 仓 B:小仓,先完成索引(coverage 100%,就绪)。
	rootB := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(rootB)); err != nil {
		t.Fatal(err)
	}

	// 仓 A:36 文件,慢 provider 下首建需 ≥2.7s(36/4 批 × 300ms)。
	rootA := t.TempDir()
	for i := 0; i < 36; i++ {
		writeFixture(t, rootA, fmt.Sprintf("mod_%02d.py", i),
			fmt.Sprintf("def handler_%02d(x):\n    return x + %d\n", i, i))
	}
	slow.Store(true)
	buildDone := make(chan error, 1)
	go func() {
		_, err := e.Sync(context.Background(), syncRequest(rootA))
		buildDone <- err
	}()
	// 确认 A 已进入在建(避免竞态空窗)。
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		_, inflight := e.inflight[mustKey(t, e, rootA)]
		e.mu.Unlock()
		if inflight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A 仓构建未启动")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 判定:B 仓(就绪)同步检索必须在远小于 A 构建时长内完成。
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	result, err := e.Search(ctx, searchRequest(rootB, "HandleLogin"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("就绪仓检索被外仓嵌入阻塞(P1 成立): %v(耗时 %s)", err, elapsed)
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("就绪仓检索空窗: %q", result.Text)
	}
	// B 的查询嵌入也走同一慢 provider(≈300ms),放宽到 1.2s 仍远小于
	// A 首建时长——超过即说明存在跨仓串行化。
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("就绪仓检索耗时 %s,疑似跨仓阻塞", elapsed)
	}
	slow.Store(false)
	if err := <-buildDone; err != nil {
		t.Fatal(err)
	}
}

func mustKey(t *testing.T, e *Engine, dir string) string {
	t.Helper()
	_, key, err := e.resolveRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
