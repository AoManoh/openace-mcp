package managed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/daemon"
)

func TestDaemonAddrFromEnv(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_ADDR", "")
	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", "")
	if got := daemonAddrFromEnv(); got != daemon.DefaultAddr {
		t.Fatalf("default addr = %q, want %q", got, daemon.DefaultAddr)
	}

	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", "127.0.0.1:9000")
	if got := daemonAddrFromEnv(); got != "127.0.0.1:9000" {
		t.Fatalf("listen addr = %q", got)
	}

	t.Setenv("OPENACE_DAEMON_ADDR", "http://127.0.0.1:9999")
	if got := daemonAddrFromEnv(); got != "http://127.0.0.1:9999" {
		t.Fatalf("daemon addr should win, got %q", got)
	}
}

func TestListenAddr(t *testing.T) {
	for input, want := range map[string]string{
		"":                         daemon.DefaultAddr,
		"127.0.0.1:8765":           "127.0.0.1:8765",
		"http://127.0.0.1:8765/":   "127.0.0.1:8765",
		"https://localhost:9876/x": "localhost:9876",
	} {
		if got := listenAddr(input); got != want {
			t.Fatalf("listenAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestManagedDaemonAddrUsesPlainListenAddress(t *testing.T) {
	for input, want := range map[string]string{
		"https://127.0.0.1:8765": "127.0.0.1:8765",
		"http://localhost:9876/": "localhost:9876",
		"127.0.0.1:7654":         "127.0.0.1:7654",
	} {
		if got := managedDaemonAddr(input); got != want {
			t.Fatalf("managedDaemonAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestConnectFallsBackToPlainHTTPForManagedDaemonURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	t.Setenv("OPENACE_DAEMON_ADDR", "https://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "200ms")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStartupTimeout(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "")
	if got := startupTimeout(); got != defaultStartupTimeout {
		t.Fatalf("default timeout = %s", got)
	}
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "250ms")
	if got := startupTimeout(); got.String() != "250ms" {
		t.Fatalf("custom timeout = %s", got)
	}
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "invalid")
	if got := startupTimeout(); got != defaultStartupTimeout {
		t.Fatalf("invalid timeout should fallback, got %s", got)
	}
}

func TestWithDaemonLogAppendsCapturedStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("real validation error\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := withDaemonLog(errors.New("not ready"), path)
	if !strings.Contains(err.Error(), "not ready") || !strings.Contains(err.Error(), "real validation error") {
		t.Fatalf("error should include readiness and stderr details: %v", err)
	}
}

func TestUpsertEnv(t *testing.T) {
	got := upsertEnv([]string{"A=1", "OPENACE_DAEMON_LISTEN_ADDR=old"}, "OPENACE_DAEMON_LISTEN_ADDR", "new")
	if got[1] != "OPENACE_DAEMON_LISTEN_ADDR=new" {
		t.Fatalf("existing env not replaced: %v", got)
	}
	got = upsertEnv(nil, "OPENACE_TEST_KEY", "value")
	if len(got) != 1 || got[0] != "OPENACE_TEST_KEY=value" {
		t.Fatalf("new env not appended: %v", got)
	}
}
