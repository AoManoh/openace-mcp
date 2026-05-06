package managed

import (
	"testing"

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
