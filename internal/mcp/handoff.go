package mcp

// 升级自愈 handoff(T2,docs/tasks/T2-wrapper-reexec-on-upgrade.md):
// daemon 升级后旧 wrapper 镜像与新 daemon 强校验冲突(known-issue #13,
// 3 个真实样本)。硬校验保持不变;新增自愈:检出 identity 变化时保存
// 在途请求与 stdin 缓冲残余,exec 磁盘上的自身路径(升级动作已把它换成
// 新版),新镜像恢复会话并重放在途请求。exec 仅 Unix(Windows 保持既有
// 硬错文案,平台门控见 handoff_exec_*.go)。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// EnvHandoffState 携带 handoff 状态文件路径穿越 exec。
const EnvHandoffState = "OPENACE_MCP_HANDOFF_STATE"

// handoffCooldown 是 re-exec 循环护栏:resume 后短窗内再次触发不再
// exec(如 daemon 比磁盘 binary 更新,exec 无法收敛),回落既有硬错。
const handoffCooldown = 30 * time.Second

// handoffState 是跨 exec 的最小会话状态。含在途请求原文(用户查询
// 文本),文件 0600 且 resume 即删。
type handoffState struct {
	// PendingLine 是触发 identity 冲突的完整 JSON-RPC 请求行,resume
	// 后最先重放(对新 daemon 重新执行,响应不丢)。
	PendingLine string `json:"pending_line"`
	// StdinRest 是 bufio 已缓冲未处理的字节(exec 丢用户态缓冲,管线化
	// 客户端的后续请求必须随状态转移)。
	StdinRest []byte `json:"stdin_rest,omitempty"`
	// At 是 handoff 时刻(冷却窗口锚点)。
	At time.Time `json:"at"`
}

// writeHandoffState 落盘状态文件(0600,私有 tmp)。
func writeHandoffState(state handoffState) (string, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", "openace-mcp-handoff-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if chmodErr := file.Chmod(0o600); chmodErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", chmodErr
	}
	if _, writeErr := file.Write(raw); writeErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", writeErr
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(path)
		return "", closeErr
	}
	return path, nil
}

// loadHandoffState 读取并清理 resume 状态(文件即删、env 即清,异常路径
// 同样尽力清理,不留含用户查询文本的残档)。无状态时返回 nil。
func loadHandoffState() *handoffState {
	path := os.Getenv(EnvHandoffState)
	if path == "" {
		return nil
	}
	_ = os.Unsetenv(EnvHandoffState)
	raw, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil {
		return nil
	}
	var state handoffState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil
	}
	return &state
}

// bufferedRest 提取 bufio 已缓冲未消费的字节副本。
func bufferedRest(reader *bufio.Reader) []byte {
	n := reader.Buffered()
	if n <= 0 {
		return nil
	}
	peek, err := reader.Peek(n)
	if err != nil {
		return nil
	}
	rest := make([]byte, n)
	copy(rest, peek)
	return rest
}

// tryUpgradeHandoff 在 identity 冲突时尝试 exec 自愈。成功时不返回
// (进程镜像被替换);任何失败返回 error,调用方回落既有硬错响应。
func (s *Server) tryUpgradeHandoff(line string, reader *bufio.Reader) error {
	if s.selfExec == "" {
		return fmt.Errorf("handoff disabled: self exec path unknown")
	}
	if !s.handoffResumedAt.IsZero() && time.Since(s.handoffResumedAt) < handoffCooldown {
		return fmt.Errorf("handoff cooldown: resumed %s ago, daemon still mismatched (is the on-disk binary older than the daemon?)", time.Since(s.handoffResumedAt).Round(time.Second))
	}
	if _, err := os.Stat(s.selfExec); err != nil {
		return fmt.Errorf("handoff self path unavailable: %w", err)
	}
	path, err := writeHandoffState(handoffState{
		PendingLine: line,
		StdinRest:   bufferedRest(reader),
		At:          time.Now(),
	})
	if err != nil {
		return err
	}
	if err := execSelf(s.selfExec, path); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// EnableUpgradeHandoff 注册自愈 exec 目标(启动期解析的自身绝对路径);
// 空路径或不支持的平台维持既有硬错行为。
func (s *Server) EnableUpgradeHandoff(selfPath string) {
	s.selfExec = selfPath
}
