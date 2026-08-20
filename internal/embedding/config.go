// Package embedding 定义 local-hybrid 语义路的 embedding provider contract：
// 配置解析、profile 身份与 HTTP 客户端（voyage / OpenAI-compatible 自部署）。
//
// key 只在 HTTP 头构造点使用，不进入 manifest、状态、错误与日志（暗坑 K21）；
// provider 身份（type/base_url/model/dimension/dtype/模板版本）决定索引 profile
// 子树，key 与运维参数不参与（阶段计划 D3/D4）。
package embedding

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// Provider 类型取值。
const (
	ProviderVoyage = "voyage"
	ProviderOpenAI = "openai"
	ProviderOff    = "off"
)

// 环境变量名（阶段计划 §4 定稿）。
const (
	EnvProvider       = "OPENACE_EMBEDDING_PROVIDER"
	EnvBaseURL        = "OPENACE_EMBEDDING_BASE_URL"
	EnvAPIKey         = "OPENACE_EMBEDDING_API_KEY"
	EnvVoyageAPIKey   = "VOYAGE_API_KEY"
	EnvModel          = "OPENACE_EMBEDDING_MODEL"
	EnvDimension      = "OPENACE_EMBEDDING_DIMENSION"
	EnvBatchSize      = "OPENACE_EMBEDDING_BATCH_SIZE"
	EnvMaxConcurrency = "OPENACE_EMBEDDING_MAX_CONCURRENCY"
	// EnvThroughputGovernor 是 T6 逃生门:off 关闭治理器与车道分离。
	EnvThroughputGovernor = "OPENACE_THROUGHPUT_GOVERNOR"
	// EnvBatchAPI 选择离线批车道 adapter:目前仅 voyage;默认关闭。
	EnvBatchAPI = "OPENACE_EMBEDDING_BATCH_API"
	// EnvBatchMinChunks 是批车道触发阈值(缺失 chunk 数)。
	EnvBatchMinChunks = "OPENACE_EMBEDDING_BATCH_MIN_CHUNKS"
	EnvRPMBudget      = "OPENACE_EMBEDDING_RPM_BUDGET"
	EnvTPMBudget      = "OPENACE_EMBEDDING_TPM_BUDGET"
)

// 默认值（调研报告 B4 参考值，全部用户可覆盖）。
const (
	defaultVoyageBaseURL = "https://api.voyageai.com/v1"
	defaultVoyageModel   = "voyage-code-3"
	defaultDimension     = 1024
	defaultBatchSize     = 128
	// defaultConcurrency 4→8(灰度反馈 2026-08-07:索引吞吐 450-780
	// chunks/min 太慢)→16(2026-08-12 用户批示:高吞吐自部署 provider
	// 下索引效率应默认拉满)。嵌入是纯 I/O 等待,worker 数不受
	// GOMAXPROCS 约束;付费档 provider(如 Voyage 2000 RPM)与自部署
	// 模型吞吐随并发近线性,免费档由 provider RPM 限速+429 退避兜底,
	// 提并发不改变计费总量。自部署高吞吐环境可经
	// OPENACE_EMBEDDING_MAX_CONCURRENCY 进一步调大(如 64)。
	defaultConcurrency = 16
	// defaultBatchMinChunks:批车道触发阈值缺省。2000 chunks≈0.6M tokens,
	// 同步车道分钟级即可完成——低于此走批只会平白多等服务端排队。
	defaultBatchMinChunks = 2000
	// maxBatchSize 是单批条数硬上限（Voyage 官方 ≤1000 条）。
	maxBatchSize = 1000
)

// Dtype 是向量元素类型；Stage 3 固定 float32（量化属 Stage 5）。
const Dtype = "float32"

// 模板版本的唯一事实源在 localengine.embedTemplateVersion(嵌入输入的
// 构造者);本包 ProfileHash 经 TemplateVersion 字段注入,禁止再设第二
// 常量(M9②,诊断报告 2026-08-03:双常量在模板升级时必改漏一处)。

// Config 是 embedding provider 的完整配置。
type Config struct {
	// Enabled 为 false 时语义路不启用；词法路径照常（核心理念 3）。
	Enabled bool
	// DisabledReason 解释未启用原因（off / 缺 key），进入状态上报。
	DisabledReason string

	ProviderType string
	BaseURL      string
	APIKey       string
	Model        string
	Dimension    int

	BatchSize      int
	MaxConcurrency int
	// RPMBudget/TPMBudget 为 0 表示不限（决策 14：默认不限预算）。
	RPMBudget int
	TPMBudget int

	Timeout time.Duration
	// QueryTimeout 是查询期(InputQuery)单次尝试超时;0=回落 Timeout
	// (RS3:未配置=现状行为)。
	QueryTimeout time.Duration
	MaxRetries   int

	// TemplateVersion 是 embedding 输入模板版本,唯一事实源在
	// localengine.embedTemplateVersion,由引擎构造期注入(参与
	// ProfileHash;M9② 单一化,禁止本包再设常量)。
	TemplateVersion string

	// GovernorDisabled 关闭吞吐治理器与车道分离(T6 逃生门,env
	// OPENACE_THROUGHPUT_GOVERNOR=off):回到共用单熔断+固定并发的
	// 旧行为。运行时参数,不参与 ProfileHash。
	GovernorDisabled bool

	// BatchAPIMode 是离线批车道(T8):""=关闭(默认),"voyage"=大额
	// document 嵌入改走 voyage Batch API(-33% 费用,服务端 12h 窗)。
	// 行为模式,参与引擎指纹(localengine.Fingerprint)但不参与
	// ProfileHash——批与实时 API 产出同模型同维度的等价向量
	// (2026-08-03 实测余弦=1.000000),索引子树身份不变。
	BatchAPIMode string
	// BatchMinChunks 是走批车道的缺失量阈值,低于此走同步车道
	// (小活不值得 12h 完成窗)。
	BatchMinChunks int
	// BulkPollInterval 是批作业轮询间隔(仅程序化覆盖,测试用;0=默认
	// 30s。轮询是纯只读状态查询,不计费)。
	BulkPollInterval time.Duration
}

// ConfigFromEnv 解析 embedding 配置；配置错误在启动即报（fail-fast），
// 缺 key 不是错误——返回 Enabled=false 并附原因（阶段计划 D1）。
func ConfigFromEnv() (Config, error) {
	provider := strings.TrimSpace(strings.ToLower(os.Getenv(EnvProvider)))
	if provider == "" {
		provider = ProviderVoyage
	}
	switch provider {
	case ProviderVoyage, ProviderOpenAI:
	case ProviderOff:
		return Config{Enabled: false, ProviderType: ProviderOff, DisabledReason: "embedding provider is off"}, nil
	default:
		return Config{}, fmt.Errorf("invalid %s %q; use %q, %q or %q", EnvProvider, os.Getenv(EnvProvider), ProviderVoyage, ProviderOpenAI, ProviderOff)
	}

	cfg := Config{ProviderType: provider}
	var err error
	if cfg.Dimension, err = reliability.IntEnv(EnvDimension, defaultDimension, 1); err != nil {
		return Config{}, err
	}
	if cfg.BatchSize, err = reliability.IntEnv(EnvBatchSize, defaultBatchSize, 1); err != nil {
		return Config{}, err
	}
	if cfg.BatchSize > maxBatchSize {
		return Config{}, fmt.Errorf("invalid %s %d: must be <= %d (provider batch limit)", EnvBatchSize, cfg.BatchSize, maxBatchSize)
	}
	if cfg.MaxConcurrency, err = reliability.IntEnv(EnvMaxConcurrency, defaultConcurrency, 1); err != nil {
		return Config{}, err
	}
	if cfg.RPMBudget, err = reliability.IntEnv(EnvRPMBudget, 0, 0); err != nil {
		return Config{}, err
	}
	if cfg.TPMBudget, err = reliability.IntEnv(EnvTPMBudget, 0, 0); err != nil {
		return Config{}, err
	}
	if cfg.Timeout, err = reliability.TimeoutFromEnv(); err != nil {
		return Config{}, err
	}
	if cfg.QueryTimeout, err = reliability.QueryTimeoutFromEnv(); err != nil {
		return Config{}, err
	}
	if cfg.MaxRetries, err = reliability.MaxRetriesFromEnv(); err != nil {
		return Config{}, err
	}
	switch governor := strings.TrimSpace(strings.ToLower(os.Getenv(EnvThroughputGovernor))); governor {
	case "", "on":
	case "off":
		cfg.GovernorDisabled = true
	default:
		return Config{}, fmt.Errorf("invalid %s %q: use \"on\" or \"off\"", EnvThroughputGovernor, governor)
	}
	switch batchAPI := strings.TrimSpace(strings.ToLower(os.Getenv(EnvBatchAPI))); batchAPI {
	case "", "off":
	case "voyage":
		// 批车道要求 provider 就是 voyage:不支持的组合 fail-fast,
		// 绝不静默忽略(用户裁决 2026-08-20:错误显式外抛)。
		if provider != ProviderVoyage {
			return Config{}, fmt.Errorf("%s=voyage requires %s=voyage (got %q)", EnvBatchAPI, EnvProvider, provider)
		}
		cfg.BatchAPIMode = ProviderVoyage
	default:
		return Config{}, fmt.Errorf("invalid %s %q: use \"voyage\" or \"off\"", EnvBatchAPI, batchAPI)
	}
	if cfg.BatchMinChunks, err = reliability.IntEnv(EnvBatchMinChunks, defaultBatchMinChunks, 1); err != nil {
		return Config{}, err
	}

	cfg.Model = strings.TrimSpace(os.Getenv(EnvModel))
	baseURL := strings.TrimSpace(os.Getenv(EnvBaseURL))
	cfg.APIKey = strings.TrimSpace(os.Getenv(EnvAPIKey))

	switch provider {
	case ProviderVoyage:
		if baseURL == "" {
			baseURL = defaultVoyageBaseURL
		}
		if cfg.Model == "" {
			cfg.Model = defaultVoyageModel
		}
		// key 解析链：OPENACE_EMBEDDING_API_KEY → VOYAGE_API_KEY（阶段计划 D3）。
		if cfg.APIKey == "" {
			cfg.APIKey = strings.TrimSpace(os.Getenv(EnvVoyageAPIKey))
		}
		if cfg.APIKey == "" {
			return Config{
				Enabled:        false,
				ProviderType:   provider,
				DisabledReason: fmt.Sprintf("embedding provider %q has no API key (%s / %s); semantic path disabled, lexical retrieval fully available", provider, EnvAPIKey, EnvVoyageAPIKey),
			}, nil
		}
	case ProviderOpenAI:
		// 自部署 OpenAI-compatible：base_url/model 必填，key 允许为空（K21：
		// 无 key 时不发送 Authorization 头）。
		if baseURL == "" {
			return Config{}, fmt.Errorf("%s is required when %s=%s (self-hosted OpenAI-compatible endpoint)", EnvBaseURL, EnvProvider, ProviderOpenAI)
		}
		if cfg.Model == "" {
			return Config{}, fmt.Errorf("%s is required when %s=%s", EnvModel, EnvProvider, ProviderOpenAI)
		}
	}

	normalized, err := normalizeBaseURL(EnvBaseURL, baseURL)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseURL = normalized
	cfg.Enabled = true
	return cfg, nil
}

// normalizeBaseURL 校验并规范化 base URL（去尾部斜杠，保证 hash 稳定）。
func normalizeBaseURL(envName string, raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid %s %q: expected absolute http(s) URL", envName, raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid %s %q: scheme must be http or https", envName, raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// ProfileHash 返回 embedding profile 身份短 hash（12 hex），决定索引子树；
// 对 provider_type/base_url/model/dimension/dtype/模板版本敏感，对 key 与
// 运维参数（batch/并发/超时/预算）不敏感（阶段计划 D4）。未启用时为空。
func (c Config) ProfileHash() string {
	if !c.Enabled {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		c.ProviderType, c.BaseURL, c.Model, fmt.Sprintf("%d", c.Dimension), Dtype, c.TemplateVersion,
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// Identity 返回不含 key 的 provider 身份描述（manifest 与状态用）。
func (c Config) Identity() string {
	if !c.Enabled {
		return ""
	}
	return fmt.Sprintf("%s/%s dim=%d dtype=%s", c.ProviderType, c.Model, c.Dimension, Dtype)
}
