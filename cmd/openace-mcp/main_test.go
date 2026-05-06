package main

import (
	"testing"

	"github.com/AoManoh/openace-mcp/internal/daemon"
)

func TestOpenaceMode(t *testing.T) {
	t.Setenv("OPENACE_MODE", "")
	if got := openaceMode(); got != "auto" {
		t.Fatalf("empty mode = %q", got)
	}
	t.Setenv("OPENACE_MODE", "managed-daemon")
	if got := openaceMode(); got != "auto" {
		t.Fatalf("managed mode = %q", got)
	}
	t.Setenv("OPENACE_MODE", "direct")
	if got := openaceMode(); got != "direct" {
		t.Fatalf("direct mode = %q", got)
	}
	t.Setenv("OPENACE_MODE", "daemon")
	if got := openaceMode(); got != "manual-daemon" {
		t.Fatalf("daemon mode = %q", got)
	}
}

func TestDaemonAddr(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_ADDR", "")
	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", "")
	if got := daemonAddr(); got != daemon.DefaultAddr {
		t.Fatalf("default daemon addr = %q", got)
	}
	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", "127.0.0.1:9000")
	if got := daemonAddr(); got != "127.0.0.1:9000" {
		t.Fatalf("listen daemon addr = %q", got)
	}
	t.Setenv("OPENACE_DAEMON_ADDR", "http://127.0.0.1:9999")
	if got := daemonAddr(); got != "http://127.0.0.1:9999" {
		t.Fatalf("client daemon addr should win, got %q", got)
	}
}
