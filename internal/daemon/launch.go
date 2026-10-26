package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

// EnvLaunchedBy 是 managed wrapper 拉起 daemon 时写给子进程的环境变量，
// 值为 EncodeLaunchInfo 的 JSON：拉起者的进程号、工作目录与时间。
//
// 设立原因：共享 daemon 由任一会话的 wrapper 按需拉起，停掉之后谁的下一次
// 调用又把它拉了起来、带的是哪个会话的环境，过去只能从进程表与时间倒推
// （2026-10-26 停止后 8.5 分钟被重新拉起，日志第一条只有 GET /v1/daemon/status）。
// 它只用于显示，不参与引擎配置指纹与兼容判定。
const EnvLaunchedBy = "OPENACE_DAEMON_LAUNCHED_BY"

// LaunchInfo 描述拉起本 daemon 的 wrapper 进程。
type LaunchInfo struct {
	PID int       `json:"pid"`
	Cwd string    `json:"cwd,omitempty"`
	At  time.Time `json:"at"`
}

// EncodeLaunchInfo 把 info 编码为 EnvLaunchedBy 的值。
func EncodeLaunchInfo(info LaunchInfo) string {
	raw, err := json.Marshal(info)
	if err != nil {
		return ""
	}
	return string(raw)
}

// launchInfoFromEnv 读取 EnvLaunchedBy。手动启动的 daemon 没有这个变量，值
// 无法解析时同样返回 nil：这项信息只用于显示，不影响启动。
func launchInfoFromEnv() *LaunchInfo {
	value := strings.TrimSpace(os.Getenv(EnvLaunchedBy))
	if value == "" {
		return nil
	}
	var info LaunchInfo
	if err := json.Unmarshal([]byte(value), &info); err != nil || info.PID <= 0 {
		return nil
	}
	return &info
}
