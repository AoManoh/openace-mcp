package daemon

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 灰度/自食共同暴露的可靠性缺口:daemon 死亡后 wrapper 内全部调用永远
// connection refused。恢复回调语义:连接拒绝 → 回调重拉 → 同请求重试
// 一次;回调缺席/失败保持原错误。
func TestClientRecoverHookRetriesAfterRefused(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	// 预留一个必然拒绝连接的端口(监听后立刻关闭)。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	client := NewClient(addr)
	var recovered bool
	client.SetRecoverHook(func(ctx context.Context) error {
		// 模拟 managed 重拉:在同一地址真正起一个健康端点。
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
		})
		var lnErr error
		for i := 0; i < 20; i++ { // 端口刚释放,容忍短暂 TIME_WAIT 竞态
			var l net.Listener
			l, lnErr = net.Listen("tcp", addr)
			if lnErr == nil {
				srv := &http.Server{Handler: mux}
				go func() { _ = srv.Serve(l) }()
				t.Cleanup(func() { _ = srv.Close() })
				recovered = true
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return lnErr
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Health(ctx); err != nil {
		t.Fatalf("恢复回调后应重试成功,得到: %v", err)
	}
	if !recovered {
		t.Fatal("恢复回调未被触发")
	}
}

// 未注册回调:保持原始 connection refused(历史行为)。
func TestClientNoRecoverHookKeepsRefusedError(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	client := NewClient(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = client.Health(ctx)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("应保持 connection refused,得到: %v", err)
	}
}
