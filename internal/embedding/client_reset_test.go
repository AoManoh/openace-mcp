package embedding

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// F6 家族缺口回归(sealed v2 实跑发现,2026-08-04):响应体流式读取中
// TCP connection reset 到达 json 解码层时不是 io.EOF 家族,曾被误判
// malformed(permanent)→ circuit 熄火 → 94% 覆盖入库。连接层中断必须
// transient 走重试。
func TestDecodeConnectionResetIsTransient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 写半截 JSON 后强制 RST(SetLinger(0) + Close)。
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1`))
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
		}
	}))
	defer ts.Close()

	cfg := Config{
		Enabled: true, ProviderType: ProviderOpenAI, BaseURL: ts.URL,
		Model: "fake", Dimension: 4, BatchSize: 8, MaxConcurrency: 1,
		Timeout: 5 * time.Second, MaxRetries: 0,
	}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.EmbedBatch(context.Background(), []string{"a"}, InputDocument)
	if err == nil {
		t.Fatal("半截响应应报错")
	}
	callErr := &reliability.CallError{}
	if !errors.As(err, &callErr) {
		t.Fatalf("应为 CallError: %v", err)
	}
	if callErr.Class != reliability.ClassTransient {
		t.Fatalf("连接中断必须 transient(勿判 malformed/permanent): class=%v msg=%s", callErr.Class, callErr.Message)
	}
	if strings.Contains(callErr.Message, "malformed") {
		t.Fatalf("不得标记 malformed: %s", callErr.Message)
	}
}
