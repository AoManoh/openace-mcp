package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/daemon"
)

// 停用标记：`openace-mcp daemon stop` 写下它并停掉 managed daemon；标记存在
// 期间，wrapper 在需要拉起 daemon 的地方（启动时连接、连接被拒后的自动恢复）
// 不再拉起，而是返回写明停用时间、原因与解除命令的错误。已经在运行的 daemon
// （例如用户手动启动的）照常复用；`openace-mcp daemon resume` 删除标记后，
// 下一次调用按原流程拉起。
//
// 设立原因：共享 daemon 由任一会话的 wrapper 按该会话的环境自动拉起，只停
// 进程停不住——2026-10-26 为止住计费的后台嵌入停掉 daemon，8.5 分钟后就被另一
// 会话的下一次调用重新拉起。标记按监听地址区分，放在与 daemon token、启动锁
// 相同的用户缓存目录下，所有会话的 wrapper 都能看到。

// HoldInfo 是停用标记的内容。
type HoldInfo struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
	PID    int       `json:"pid"`
	Cwd    string    `json:"cwd,omitempty"`
}

// StopResult 是 StopDaemon 的结果。
type StopResult struct {
	Addr       string
	HoldPath   string
	StoppedPID int
}

// holdPath 返回监听地址 addr 的停用标记路径；取不到用户缓存目录时返回空串。
func holdPath(addr string) string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cache, "openace-mcp", "daemon-hold-"+safeLockName(listenAddr(addr)))
}

// ReadHold 读取 addr 的停用标记。标记存在但读不出时返回错误：把它当作不存在
// 会让 wrapper 在用户明确停用之后照样拉起 daemon。
func ReadHold(addr string) (HoldInfo, bool, error) {
	path := holdPath(addr)
	if path == "" {
		return HoldInfo{}, false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return HoldInfo{}, false, nil
	}
	if err != nil {
		return HoldInfo{}, false, fmt.Errorf("read openACE daemon stop marker %s: %w", path, err)
	}
	var info HoldInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return HoldInfo{}, true, fmt.Errorf("openACE daemon stop marker %s is unreadable (%v); run `openace-mcp daemon resume` to remove it", path, err)
	}
	return info, true, nil
}

// writeHold 原子写入 addr 的停用标记（0600）。
func writeHold(addr string, info HoldInfo) (string, error) {
	path := holdPath(addr)
	if path == "" {
		return "", errors.New("cannot locate the user cache directory for the openACE daemon stop marker")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-hold-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return path, nil
}

// heldError 是标记存在时 wrapper 不拉起 daemon 返回的错误。cause 是此前连接
// 或复用探测的错误，可以为空。
func heldError(addr string, info HoldInfo, cause error) error {
	detail := "at " + info.At.Format(time.RFC3339)
	if info.Reason != "" {
		detail += ", reason: " + info.Reason
	}
	msg := fmt.Sprintf("openACE daemon at %s was stopped with `openace-mcp daemon stop` (%s); wrappers do not start it again while the stop marker %s exists. Run `openace-mcp daemon resume` to allow restarting", addr, detail, holdPath(addr))
	if cause != nil {
		msg += "; last connection error: " + cause.Error()
	}
	return errors.New(msg)
}

// refuseIfHeld 在拉起 daemon 之前检查停用标记：存在即返回 heldError，读不出
// 同样返回错误，不拉起。
func refuseIfHeld(addr string, cause error) error {
	info, held, err := ReadHold(addr)
	if err != nil {
		return err
	}
	if held {
		return heldError(addr, info, cause)
	}
	return nil
}

// ManagedListenAddr 返回 wrapper 在 managed 模式下会连接与拉起的 daemon 监听
// 地址（与 Connect 相同的解析：OPENACE_DAEMON_ADDR / OPENACE_DAEMON_LISTEN_ADDR，
// 默认 127.0.0.1:8765）。
func ManagedListenAddr() string {
	return listenAddr(managedDaemonAddr(daemonAddrFromEnv()))
}

// StopDaemon 先写停用标记，再停掉在 managed 地址上运行的 daemon（SIGTERM，最多
// 等 takeoverExitWait）。先写标记的原因：daemon 一退出，其他会话的 wrapper 就
// 可能按连接被拒走自动恢复，标记必须早于这一刻存在。daemon 不在运行时只写标记。
// 只停本机回环地址上的 daemon。
func StopDaemon(ctx context.Context, reason string) (StopResult, error) {
	addr := managedDaemonAddr(daemonAddrFromEnv())
	cwd, _ := os.Getwd()
	path, err := writeHold(addr, HoldInfo{At: time.Now().UTC(), Reason: strings.TrimSpace(reason), PID: os.Getpid(), Cwd: cwd})
	if err != nil {
		return StopResult{}, fmt.Errorf("write openACE daemon stop marker: %w", err)
	}
	result := StopResult{Addr: listenAddr(addr), HoldPath: path}
	client := daemon.NewClient(addr)
	if !healthy(ctx, client) {
		return result, nil
	}
	if !loopbackEndpoint(client.Endpoint()) {
		return result, fmt.Errorf("daemon endpoint %s is not loopback; the stop marker is written but the daemon was not signalled", client.Endpoint())
	}
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	status, err := client.DaemonStatus(statusCtx)
	cancel()
	if err != nil {
		return result, fmt.Errorf("read daemon status before stopping: %w", err)
	}
	if status.Service != "openace-daemon" || status.PID <= 0 {
		return result, fmt.Errorf("process at %s is not an openACE daemon with a known pid; the stop marker is written but nothing was signalled", result.Addr)
	}
	if err := terminateProcess(status.PID); err != nil {
		return result, fmt.Errorf("terminate daemon pid %d: %w", status.PID, err)
	}
	deadline := time.Now().Add(takeoverExitWait)
	for time.Now().Before(deadline) {
		if !healthy(ctx, client) {
			result.StoppedPID = status.PID
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return result, fmt.Errorf("daemon pid %d still healthy after %s", status.PID, takeoverExitWait)
}

// ResumeDaemon 删除停用标记，返回标记路径与它此前是否存在。不拉起 daemon：
// 下一次 openACE 调用按原流程拉起。
func ResumeDaemon() (string, bool, error) {
	addr := managedDaemonAddr(daemonAddrFromEnv())
	path := holdPath(addr)
	if path == "" {
		return "", false, errors.New("cannot locate the user cache directory for the openACE daemon stop marker")
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, false, nil
	}
	if err != nil {
		return path, true, err
	}
	return path, true, nil
}
