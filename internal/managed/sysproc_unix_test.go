//go:build unix

package managed

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// M6 回归:managed daemon 子进程必须脱离 wrapper 会话(Ctrl+C 不连坐)
// 且退出后被收尸(无僵尸)。以 /bin/sleep 代理验证进程属性与收尸路径,
// 与 startDaemon 同一注入函数。
func TestDaemonProcessDetachedAndReaped(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	applyDaemonSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()

	selfSid, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	childSid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatalf("getsid(child): %v", err)
	}
	if childSid == selfSid {
		t.Fatalf("子进程应脱离 wrapper 会话(Setsid): self=%d child=%d", selfSid, childSid)
	}
	if childSid != pid {
		t.Fatalf("Setsid 后子进程应为新会话首领: sid=%d pid=%d", childSid, pid)
	}

	// 杀掉后必须被 Wait 收尸(不留僵尸)。
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("子进程退出后应被收尸(Wait 返回)")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		// 进程表仍可见:若为僵尸,kill(0) 返回 nil——收尸后应 ESRCH。
		t.Fatalf("收尸后进程 %d 不应仍在进程表", pid)
	}
}
