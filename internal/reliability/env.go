// Package reliability 提供 provider 调用的通用可靠性核心：共享 env 解析、
// 重试策略、退避 circuit 与可选 RPM/TPM limiter。
//
// 该包与 legacy internal/provider（ACE profile registry）无关（阶段计划 D9）；
// embedding 与 rerank 客户端共用本包，状态挂在 daemon 级单例上（暗坑 K36）。
package reliability

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvProviderTimeout 是单次 provider HTTP 请求超时（embedding/rerank 共用）。
	EnvProviderTimeout = "OPENACE_PROVIDER_TIMEOUT"
	// EnvProviderMaxRetries 是单批可重试错误的重试上限。
	EnvProviderMaxRetries = "OPENACE_PROVIDER_MAX_RETRIES"

	defaultTimeout    = 60 * time.Second
	defaultMaxRetries = 5
)

// TimeoutFromEnv 解析 OPENACE_PROVIDER_TIMEOUT（默认 60s）。
func TimeoutFromEnv() (time.Duration, error) {
	return DurationEnv(EnvProviderTimeout, defaultTimeout)
}

// MaxRetriesFromEnv 解析 OPENACE_PROVIDER_MAX_RETRIES（默认 5，0 表示不重试）。
func MaxRetriesFromEnv() (int, error) {
	return IntEnv(EnvProviderMaxRetries, defaultMaxRetries, 0)
}

// DurationEnv 解析时长型环境变量；空值返回默认，非法值显式报错。
func DurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: expected Go duration like 60s", name, raw)
	}
	if value <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be positive", name, raw)
	}
	return value, nil
}

// IntEnv 解析整数型环境变量；空值返回默认，小于 min 或非法值显式报错。
func IntEnv(name string, fallback int, min int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: expected integer", name, raw)
	}
	if value < min {
		return 0, fmt.Errorf("invalid %s %q: must be >= %d", name, raw, min)
	}
	return value, nil
}
