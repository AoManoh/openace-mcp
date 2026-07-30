// Package rerank 定义 local-hybrid 可选精排的 provider contract：
// 配置解析与 HTTP 客户端（voyage 形状 = Cohere/Jina 式；tei 形状 = 自部署 TEI）。
//
// rerank 只重排已召回候选，不拥有索引事实源；失败时候选集与 RRF 序完整
// 保留（迁移方案 §12，决策 11）。rerank 身份进入 daemon 复用指纹（D11），
// 不进入索引 profile 子树（不影响存储内容）。
package rerank

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// Provider 类型取值。voyage 形状（{query,documents,model}→results[{index,
// relevance_score}]）与 Cohere/Jina 兼容：自部署这两类端点用 voyage + base_url 接入。
const (
	ProviderVoyage = "voyage"
	ProviderTEI    = "tei"
	ProviderOff    = "off"
)

// 环境变量名（阶段计划 §4 定稿）。
const (
	EnvProvider  = "OPENACE_RERANK_PROVIDER"
	EnvBaseURL   = "OPENACE_RERANK_BASE_URL"
	EnvAPIKey    = "OPENACE_RERANK_API_KEY"
	EnvModel     = "OPENACE_RERANK_MODEL"
	EnvMaxTokens = "OPENACE_RERANK_MAX_TOKENS"
)

const (
	defaultVoyageBaseURL = "https://api.voyageai.com/v1"
	defaultVoyageModel   = "rerank-2.5"
	// defaultMaxTokens 是单请求估算 token 上限（官方延迟建议 200K，调研 B2）。
	defaultMaxTokens = 200000
)

// Config 是 rerank provider 的完整配置。
type Config struct {
	// Enabled 为 false 时不做精排，返回 RRF 序（不是降级，是配置形态）。
	Enabled bool
	// DisabledReason 解释未启用原因（off / 缺 key），进入状态上报。
	DisabledReason string

	ProviderType string
	BaseURL      string
	APIKey       string
	Model        string
	// MaxTokens 是单请求估算 token 上限；超限候选按 RRF 序跟随不送审（K28）。
	MaxTokens int

	Timeout    time.Duration
	MaxRetries int
}

// ConfigFromEnv 解析 rerank 配置；语义同 embedding.ConfigFromEnv：
// 配置错误 fail-fast，voyage 缺 key 返回 Enabled=false 附原因。
func ConfigFromEnv() (Config, error) {
	provider := strings.TrimSpace(strings.ToLower(os.Getenv(EnvProvider)))
	if provider == "" {
		provider = ProviderVoyage
	}
	switch provider {
	case ProviderVoyage, ProviderTEI:
	case ProviderOff:
		return Config{Enabled: false, ProviderType: ProviderOff, DisabledReason: "rerank provider is off"}, nil
	default:
		return Config{}, fmt.Errorf("invalid %s %q; use %q, %q or %q", EnvProvider, os.Getenv(EnvProvider), ProviderVoyage, ProviderTEI, ProviderOff)
	}

	cfg := Config{ProviderType: provider}
	var err error
	if cfg.MaxTokens, err = reliability.IntEnv(EnvMaxTokens, defaultMaxTokens, 1); err != nil {
		return Config{}, err
	}
	if cfg.Timeout, err = reliability.TimeoutFromEnv(); err != nil {
		return Config{}, err
	}
	if cfg.MaxRetries, err = reliability.MaxRetriesFromEnv(); err != nil {
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
		if cfg.APIKey == "" {
			cfg.APIKey = strings.TrimSpace(os.Getenv(embeddingVoyageKeyEnv))
		}
		if cfg.APIKey == "" {
			return Config{
				Enabled:        false,
				ProviderType:   provider,
				DisabledReason: fmt.Sprintf("rerank provider %q has no API key (%s / %s); results keep RRF order", provider, EnvAPIKey, embeddingVoyageKeyEnv),
			}, nil
		}
	case ProviderTEI:
		// 自部署 TEI：base_url 必填；model 由端点决定可留空；key 允许为空。
		if baseURL == "" {
			return Config{}, fmt.Errorf("%s is required when %s=%s (self-hosted TEI endpoint)", EnvBaseURL, EnvProvider, ProviderTEI)
		}
	}

	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return Config{}, fmt.Errorf("invalid %s %q: expected absolute http(s) URL", EnvBaseURL, baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return Config{}, fmt.Errorf("invalid %s %q: scheme must be http or https", EnvBaseURL, baseURL)
	}
	cfg.BaseURL = strings.TrimRight(baseURL, "/")
	cfg.Enabled = true
	return cfg, nil
}

// embeddingVoyageKeyEnv 与 embedding 包的 VOYAGE_API_KEY 回退链一致；
// 字面量重复以避免 rerank→embedding 的包依赖。
const embeddingVoyageKeyEnv = "VOYAGE_API_KEY"

// Identity 返回不含 key 的 provider 身份描述（状态与复用指纹用）。
func (c Config) Identity() string {
	if !c.Enabled {
		return ""
	}
	if c.Model == "" {
		return fmt.Sprintf("%s@%s", c.ProviderType, c.BaseURL)
	}
	return fmt.Sprintf("%s/%s", c.ProviderType, c.Model)
}
