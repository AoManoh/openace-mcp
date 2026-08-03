package daemon

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// M5(诊断报告 2026-08-03,批准选项 A):daemon HTTP 面默认随机 token
// (0600 状态文件,wrapper 自动读取),封死多用户机上回环端口的跨用户
// 访问;OPENACE_DAEMON_TOKEN=off 显式关闭;显式 token 行为不变。

func newTokenTestServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer(nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.tasks.Shutdown(ctx)
	})
	return server
}

// TestDefaultTokenProtectsEndpoints:env 未设 → 自动生成 token 文件
// (0600),无凭据请求 401,持文件 token 的客户端可通行。
func TestDefaultTokenProtectsEndpoints(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	os.Unsetenv("OPENACE_DAEMON_TOKEN")

	server := newTokenTestServer(t)
	ts := httptest.NewServer(server.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/daemon/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("默认档无凭据应 401: %d", resp.StatusCode)
	}

	path, token, err := LoadOrCreateToken(DefaultAddr)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token 文件权限应 0600: %v", info.Mode().Perm())
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/daemon/status", nil)
	req.Header.Set("authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("持文件 token 应通行: %d", resp.StatusCode)
	}
}

// TestTokenOffDisablesAuth:显式 off → 零认证(自担风险),不生成文件。
func TestTokenOffDisablesAuth(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")

	server := newTokenTestServer(t)
	ts := httptest.NewServer(server.routes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/daemon/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("off 档应放行: %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		// openace-mcp 子目录若存在,内部不得有 token 文件。
		if _, err := os.Stat(TokenFilePath(DefaultAddr)); err == nil {
			t.Fatalf("off 档不得生成 token 文件: %v", names)
		}
	}
}

// TestExplicitTokenKeepsLegacyBehavior:显式 token 行为与历史一致。
func TestExplicitTokenKeepsLegacyBehavior(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENACE_DAEMON_TOKEN", "sekrit")
	server := newTokenTestServer(t)
	ts := httptest.NewServer(server.routes())
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/v1/daemon/status")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("显式 token 下无凭据应 401: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/daemon/status", nil)
	req.Header.Set("authorization", "Bearer sekrit")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("显式 token 应通行: %d", resp.StatusCode)
	}
}

// TestRequestBodyCapped:请求体超上限被拒(本地 DoS 面收敛)。
func TestRequestBodyCapped(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	server := newTokenTestServer(t)
	ts := httptest.NewServer(server.routes())
	defer ts.Close()
	huge := bytes.Repeat([]byte("a"), int(maxRequestBodyBytes)+1024)
	resp, err := http.Post(ts.URL+"/v1/retrieve", "application/json", bytes.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("超限请求体不得成功: %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("应显式拒绝超限请求体: %d", resp.StatusCode)
	}
}

// TestClientAutoReadsTokenFile:client 在 env 未设时自动读取 token 文件。
func TestClientAutoReadsTokenFile(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	os.Unsetenv("OPENACE_DAEMON_TOKEN")
	if _, _, err := LoadOrCreateToken(DefaultAddr); err != nil {
		t.Fatal(err)
	}
	client := NewClient(DefaultAddr)
	if strings.TrimSpace(client.token) == "" {
		t.Fatal("client 应自动读取 token 文件")
	}
}
