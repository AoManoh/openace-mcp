package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/runtimeinfo"
)

// TestIdentityChangedGuidance：会话中途发现 daemon 换了版本或配置时，报错保留
// 身份变化标记，写明处置是重启本会话，不建议从本会话停掉共享 daemon。
func TestIdentityChangedGuidance(t *testing.T) {
	client := NewClient("127.0.0.1:1")
	client.SetExpectedIdentity(buildinfo.Info{VCSRevision: "aaa"}, engine.EngineLocalHybrid, "profile-a")
	err := client.validateServedBy(&runtimeinfo.ServedBy{
		Service: "openace-daemon", Engine: engine.EngineLocalHybrid,
		EngineProfile: "profile-b", Build: buildinfo.Info{VCSRevision: "aaa"},
	})
	if err == nil {
		t.Fatal("配置指纹变化应报错")
	}
	msg := err.Error()
	for _, want := range []string{engine.DaemonIdentityChangedMarker, "profile-a", "profile-b", "restart this MCP session", "do not stop the shared daemon"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("报错缺少 %q: %s", want, msg)
		}
	}
}

// TestClientTimeoutGuidance：daemon 在调用方截止时间内没有响应时，报错写明
// daemon 地址、等了多久与处置，并保留原错误，调用方仍能用 errors.Is 识别超时。
func TestClientTimeoutGuidance(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	client := NewClient(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := client.DaemonStatus(ctx)
	if err == nil {
		t.Fatal("daemon 不响应时应报错")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应保留原超时错误: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"did not respond within", server.URL, "daemon_status", "do not stop the shared daemon"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("报错缺少 %q: %s", want, msg)
		}
	}
}
