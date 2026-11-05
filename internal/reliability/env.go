// Package reliability 提供 embedding 与 rerank 客户端共用的 provider
// 调用控制逻辑：读取超时与重试次数环境变量、把一次失败归类为
// CallError、在单次调用内按类别重试（RetryPolicy）、连续失败后暂停向
// provider 发请求（Circuit，下称熔断器）、按用户显式配置的每分钟请求数
// 与 token 数预算限速（RateLimiter），以及只作用于索引批嵌入请求的
// 吞吐治理器（Governor）。
//
// 本包对象一律挂在 daemon 级客户端单例上，由全部 workspace 的构建与
// 查询共享，不按任务新建。原因：按任务各自实例化时，并发任务互不知情
// 地同时发请求和重试，请求量随任务数成倍放大，上游按账户返回 429，
// 所有任务一起失败。
package reliability

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvProviderTimeout 是单次 provider HTTP 请求的超时环境变量，
	// embedding 与 rerank 共用。重试中的每次尝试各自计时。未设置时两侧的
	// 默认值不同：建索引一侧（文档嵌入与 Batch API）为 defaultIndexTimeout，
	// 查询一侧（查询嵌入、维度探测与 rerank）为 defaultTimeout。
	EnvProviderTimeout = "OPENACE_PROVIDER_TIMEOUT"
	// EnvProviderMaxRetries 是一次调用内可重试类错误的最多重试次数。
	// 值为 0 时只尝试一次。
	EnvProviderMaxRetries = "OPENACE_PROVIDER_MAX_RETRIES"
	// EnvQueryTimeout 是查询期 provider 调用（查询文本嵌入与 rerank）
	// 单独使用的超时环境变量。未设置时调用方沿用 EnvProviderTimeout。
	// 单独设置的原因：为构建期大批请求把 EnvProviderTimeout 调到
	// 120s 以上时，交互查询最坏也要等同样久。分开配置后，交互查询的
	// 最坏等待不随构建期超时变长。
	EnvQueryTimeout = "OPENACE_QUERY_PROVIDER_TIMEOUT"

	// defaultTimeout 是查询一侧在两个超时变量都没有设置时的单次尝试超时。
	defaultTimeout = 60 * time.Second
	// defaultIndexTimeout 是建索引一侧在 EnvProviderTimeout 没有设置时的单次
	// 尝试超时。比查询一侧长的原因：大批文档请求在服务商那边可能要一分多钟
	// （实测一批 128 条、19 万字节用了 78 秒）；到点放弃后 openACE 重发，而服务商
	// 照常处理并计费被放弃的那次，同一批内容付两次费。查询一侧的请求小，等待
	// 上限仍由 EnvQueryTimeout 与 defaultTimeout 决定，不随它变长。
	defaultIndexTimeout = 180 * time.Second
	defaultMaxRetries   = 5
)

// TimeoutFromEnv 读取 OPENACE_PROVIDER_TIMEOUT，未设置时返回 60s。查询一侧
// （查询嵌入、维度探测与 rerank）在没有设置 EnvQueryTimeout 时用这个值。
func TimeoutFromEnv() (time.Duration, error) {
	return DurationEnv(EnvProviderTimeout, defaultTimeout)
}

// IndexTimeoutFromEnv 读取 OPENACE_PROVIDER_TIMEOUT，未设置时返回 180s，
// 供建索引一侧（文档嵌入与 Batch API）使用。
func IndexTimeoutFromEnv() (time.Duration, error) {
	return DurationEnv(EnvProviderTimeout, defaultIndexTimeout)
}

// QueryTimeoutFromEnv 读取 OPENACE_QUERY_PROVIDER_TIMEOUT。未设置时返回
// 0，调用方据此改用 TimeoutFromEnv 的值，使未配置该变量的用户行为与
// 引入该变量之前完全相同。
func QueryTimeoutFromEnv() (time.Duration, error) {
	return DurationEnv(EnvQueryTimeout, 0)
}

// MaxRetriesFromEnv 读取 OPENACE_PROVIDER_MAX_RETRIES。未设置时返回 5。
// 允许为 0，表示失败后不重试。
func MaxRetriesFromEnv() (int, error) {
	return IntEnv(EnvProviderMaxRetries, defaultMaxRetries, 0)
}

// DurationEnv 读取时长型环境变量 name：值为空或全为空白时返回 fallback。
// 值不是 Go 时长格式（如 60s、2m）或不大于 0 时返回错误，错误文本带
// 变量名与原始值，让用户能直接定位改哪个变量。
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

// IntEnv 读取整数型环境变量 name：值为空或全为空白时返回 fallback。
// 值不是整数或小于 min 时返回错误，错误文本带变量名、原始值与下限。
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
