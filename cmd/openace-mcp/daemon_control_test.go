package main

import (
	"bytes"
	"net"
	"strings"
	"testing"
)

// TestDaemonControlStopResume：daemon 不在运行时 stop 只写停用标记，resume
// 删除标记；未知子命令退出码 2。
func TestDaemonControlStopResume(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	t.Setenv("OPENACE_DAEMON_ADDR", addr)

	var out, errOut bytes.Buffer
	if code := daemonControl([]string{"stop", "--reason", "停止计费"}, &out, &errOut); code != 0 {
		t.Fatalf("stop 退出码 %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "stop marker written") || !strings.Contains(out.String(), "no openACE daemon was running") {
		t.Fatalf("stop 输出不对: %s", out.String())
	}
	out.Reset()
	if code := daemonControl([]string{"resume"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "stop marker removed") {
		t.Fatalf("resume 应删除标记: code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := daemonControl([]string{"resume"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "no stop marker") {
		t.Fatalf("再次 resume 应报告没有标记: code=%d out=%s", code, out.String())
	}
	if code := daemonControl([]string{"restart"}, &out, &errOut); code != 2 {
		t.Fatalf("未知子命令应退出 2，实际 %d", code)
	}
}
