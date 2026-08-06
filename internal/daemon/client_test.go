package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/runtimeinfo"
)

func TestClientHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL).Health(context.Background()); err != nil {
		t.Fatalf("health should accept openace daemon: %v", err)
	}
}

func TestClientHealthRejectsUnexpectedService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"other"}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL).Health(context.Background()); err == nil {
		t.Fatal("health should reject non-openace service")
	}
}

func TestClientDaemonStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daemon/status" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","pid":123,"capabilities":{"runtime_identity":true},"cache_namespace":"test"}`))
	}))
	defer server.Close()

	status, err := NewClient(server.URL).DaemonStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.PID != 123 || !status.Capabilities["runtime_identity"] || status.CacheNamespace != "test" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

// 会话中途 daemon 换血的响应身份必须 fail-closed(加注 28 实证:
// 旧 wrapper 静默对话新 daemon,未知结构化字段被剥掉)。校验复用响应
// 自带 served_by,零额外 HTTP 请求。
func TestClientRejectsMidSessionDaemonIdentityChange(t *testing.T) {
	served := runtimeinfo.ServedBy{
		Service: "openace-daemon", Engine: engine.EngineLocalHybrid,
		EngineProfile: "profile-a",
		Build:         buildinfo.Info{Version: "v1", VCSRevision: "aaa"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(engine.Result{Text: "ok", ServedBy: &served})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.SetExpectedIdentity(buildinfo.Info{Version: "v1", VCSRevision: "aaa"}, engine.EngineLocalHybrid, "profile-a")
	if _, err := client.Search(context.Background(), searchReqP("/tmp/project", "", "find code", 0)); err != nil {
		t.Fatalf("相同身份应通过: %v", err)
	}

	served.Build = buildinfo.Info{Version: "v2", VCSRevision: "bbb"}
	if _, err := client.Search(context.Background(), searchReqP("/tmp/project", "", "find code", 0)); err == nil || !strings.Contains(err.Error(), "daemon identity changed during this MCP session") || !strings.Contains(err.Error(), "aaa") || !strings.Contains(err.Error(), "bbb") {
		t.Fatalf("换血身份应明确拒绝: %v", err)
	}
}

func TestClientIdentityValidationAllowsLegacyMissingServedBy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(engine.Result{Text: "legacy"})
	}))
	defer server.Close()
	client := NewClient(server.URL)
	client.SetExpectedIdentity(buildinfo.Info{Version: "v1", VCSRevision: "aaa"}, engine.EngineLocalHybrid, "profile-a")
	if _, err := client.Search(context.Background(), searchReqP("/tmp/project", "", "find code", 0)); err != nil {
		t.Fatalf("served_by 缺失的旧响应保持兼容: %v", err)
	}
}

func TestClientRequiresProviderCapabilityBeforeProviderRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("provider request should stop at health check, got %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Sync(context.Background(), syncReqP("/tmp/project", "standby"))
	if err == nil || !strings.Contains(err.Error(), "provider profile support") {
		t.Fatalf("expected provider capability error, got %v", err)
	}
}

func TestClientSendsProviderProfileWhenDaemonAdvertisesCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","capabilities":{"provider_profiles":true}}`))
		case "/v1/retrieve":
			var req retrieveRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.ProviderProfileID != "standby" {
				t.Fatalf("provider_profile_id = %q, want standby", req.ProviderProfileID)
			}
			_ = json.NewEncoder(w).Encode(engine.Result{Text: "ok", ProviderProfileID: req.ProviderProfileID})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	result, err := NewClient(server.URL).Search(context.Background(), searchReqP("/tmp/project", "standby", "find code", 0))
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderProfileID != "standby" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestClientCachesProviderCapability(t *testing.T) {
	healthCalls := 0
	retrieveCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			healthCalls++
			_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","capabilities":{"provider_profiles":true}}`))
		case "/v1/retrieve":
			retrieveCalls++
			var req retrieveRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.ProviderProfileID != "standby" {
				t.Fatalf("provider_profile_id = %q, want standby", req.ProviderProfileID)
			}
			_ = json.NewEncoder(w).Encode(engine.Result{Text: "ok", ProviderProfileID: req.ProviderProfileID})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	for i := 0; i < 2; i++ {
		if _, err := client.Search(context.Background(), searchReqP("/tmp/project", "standby", "find code", 0)); err != nil {
			t.Fatal(err)
		}
	}
	if healthCalls != 1 {
		t.Fatalf("health calls = %d, want 1", healthCalls)
	}
	if retrieveCalls != 2 {
		t.Fatalf("retrieve calls = %d, want 2", retrieveCalls)
	}
}

func TestClientCachesProviderCapabilityForConcurrentRequests(t *testing.T) {
	var healthCalls atomic.Int64
	var retrieveCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			healthCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","capabilities":{"provider_profiles":true}}`))
		case "/v1/retrieve":
			retrieveCalls.Add(1)
			var req retrieveRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			if req.ProviderProfileID != "standby" {
				t.Errorf("provider_profile_id = %q, want standby", req.ProviderProfileID)
			}
			_ = json.NewEncoder(w).Encode(engine.Result{Text: "ok", ProviderProfileID: req.ProviderProfileID})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	start := make(chan struct{})
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := client.Search(context.Background(), searchReqP("/tmp/project", "standby", "find code", 0))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := healthCalls.Load(); got != 1 {
		t.Fatalf("health calls = %d, want 1", got)
	}
	if got := retrieveCalls.Load(); got != int64(cap(errs)) {
		t.Fatalf("retrieve calls = %d, want %d", got, cap(errs))
	}
}

func TestClientDecodesWorkspaceUpstreamHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workspaces": []engine.WorkspaceStatus{{
					DirectoryPath:          "/tmp/project",
					UpstreamStatus:         "backoff",
					UpstreamLastStatusCode: 429,
					UpstreamRetryAfter:     "30s",
				}},
			})
		case "/v1/workspace/status":
			_ = json.NewEncoder(w).Encode(engine.WorkspaceStatus{
				DirectoryPath:          "/tmp/project",
				UpstreamStatus:         "backoff",
				UpstreamLastStatusCode: 429,
				UpstreamRetryAfter:     "30s",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	statuses, err := client.ListWorkspaceStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].UpstreamStatus != "backoff" || statuses[0].UpstreamLastStatusCode != 429 {
		t.Fatalf("workspace list should decode upstream health: %+v", statuses)
	}

	status, err := client.WorkspaceStatus(context.Background(), wsRef("/tmp/project"))
	if err != nil {
		t.Fatal(err)
	}
	if status.UpstreamStatus != "backoff" || status.UpstreamRetryAfter != "30s" {
		t.Fatalf("workspace status should decode upstream health: %+v", status)
	}
}

// P6(review 二批):响应体超 4MiB 被 LimitReader 截断后,不得报裸
// "unexpected end of JSON input"——错误须带端点与"响应过大/缩小请求"
// 的可行动提示。
func TestClientReportsOversizedResponse(t *testing.T) {
	big := strings.Repeat("x", (4<<20)+100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"` + big + `"}]}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).ListTasks(context.Background(), 0)
	if err == nil {
		t.Fatal("oversized response should fail")
	}
	if !strings.Contains(err.Error(), "/v1/tasks") || !strings.Contains(err.Error(), "4MiB") {
		t.Fatalf("error should carry endpoint and size hint: %v", err)
	}
}

// P6 附带:正常大小但畸形的响应,decode 错误须带端点上下文。
func TestClientWrapsDecodeErrorsWithEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{broken`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).ListTasks(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "/v1/tasks") {
		t.Fatalf("decode error should carry endpoint context: %v", err)
	}
}

// P12(review 二批):默认 token 档下 wrapper 侧 token 文件读取失败被
// 静默吞掉(return "")→ 全部请求无解释 401。失败原因必须随 401 外显,
// 并给出 OPENACE_DAEMON_TOKEN 恢复路径。
func TestClientSurfacesTokenLoadFailureOn401(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// UserCacheDir → XDG_CACHE_HOME;路径分量是普通文件 → token 目录
	// 创建必然失败。
	t.Setenv("XDG_CACHE_HOME", filepath.Join(blocker, "cache"))
	t.Setenv("OPENACE_DAEMON_TOKEN", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	err := NewClient(server.URL).Health(context.Background())
	if err == nil {
		t.Fatal("401 must fail")
	}
	if !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "OPENACE_DAEMON_TOKEN") {
		t.Fatalf("401 error should surface token load failure with recovery hint: %v", err)
	}
}
