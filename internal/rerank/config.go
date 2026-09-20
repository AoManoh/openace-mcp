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

// adapter 标识（内部值）。2026-09-20 配置收束：用户只给"地址 + 模型 id (+ key)"，
// 请求形状由地址自动识别、可用 OPENACE_RERANK_ADAPTER 覆盖；服务商差异全部收在 adapter 里。
const (
	// ProviderStandard 是通用 rerank 形状：{query, documents, model, top_k} →
	// data|results[{index, relevance_score}]，覆盖 Voyage、Cohere、Jina 与多数网关。
	ProviderStandard = "standard"
	// ProviderVoyage 是 ProviderStandard 的旧名，保留给既有调用方，值相同。
	ProviderVoyage = ProviderStandard
	// ProviderTEI 是自部署 TEI 形状：{query, texts} → [{index, score}]；地址无法识别，需显式指定。
	ProviderTEI = "tei"
	ProviderOff = "off"
	adapterAuto = "auto"
)

// 环境变量名。
const (
	EnvAdapter   = "OPENACE_RERANK_ADAPTER"
	EnvBaseURL   = "OPENACE_RERANK_BASE_URL"
	EnvAPIKey    = "OPENACE_RERANK_API_KEY"
	EnvModel     = "OPENACE_RERANK_MODEL"
	EnvMaxTokens = "OPENACE_RERANK_MAX_TOKENS"
)

// 已淘汰的变量：设置了就在启动期报迁移错误。
const legacyEnvProvider = "OPENACE_RERANK_PROVIDER"

const (
	// defaultMaxTokens 是单请求估算 token 上限（官方延迟建议 200K，调研 B2）。
	defaultMaxTokens = 200000
	// 示例值只用于报错提示；没有默认地址与默认模型。
	exampleBaseURL = "https://api.voyageai.com/v1"
	exampleModel   = "rerank-3"
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

// ConfigFromEnv 解析 rerank 配置（2026-09-20 配置收束契约）：
//   - 地址与模型都为空 = 精排未配置（Enabled=false，结果保持 RRF 序，状态如实上报）；
//     只给其中一项或只给 key = 配置错误 fail-fast；tei 形状允许模型为空（由端点决定）。
//   - adapter 由地址识别（api.voyageai.com / api.cohere.com / api.jina.ai → standard，
//     其余也按 standard），tei 必须显式 OPENACE_RERANK_ADAPTER=tei。
//   - 不再读 VOYAGE_API_KEY，每个阶段只用自己的 key；旧变量 OPENACE_RERANK_PROVIDER 报迁移错误。
func ConfigFromEnv() (Config, error) {
	if v := strings.TrimSpace(os.Getenv(legacyEnvProvider)); v != "" {
		return Config{}, fmt.Errorf("%s is no longer read (value %q): the request shape is detected from %s; remove it, or set %s=%s|%s|%s to force one",
			legacyEnvProvider, v, EnvBaseURL, EnvAdapter, ProviderStandard, ProviderTEI, ProviderOff)
	}
	adapter := strings.TrimSpace(strings.ToLower(os.Getenv(EnvAdapter)))
	if adapter == "" {
		adapter = adapterAuto
	}
	switch adapter {
	case adapterAuto, ProviderStandard, ProviderTEI:
	case "voyage":
		adapter = ProviderStandard
	case ProviderOff:
		return Config{Enabled: false, ProviderType: ProviderOff, DisabledReason: "rerank adapter is off"}, nil
	default:
		return Config{}, fmt.Errorf("invalid %s %q; use %s, %s, %s or %s", EnvAdapter, os.Getenv(EnvAdapter), adapterAuto, ProviderStandard, ProviderTEI, ProviderOff)
	}

	baseURL := strings.TrimSpace(os.Getenv(EnvBaseURL))
	model := strings.TrimSpace(os.Getenv(EnvModel))
	apiKey := strings.TrimSpace(os.Getenv(EnvAPIKey))
	if baseURL == "" && model == "" {
		if apiKey != "" {
			return Config{}, fmt.Errorf("%s is set but %s and %s are missing: set the endpoint and model id (example: %s, %s), or remove the key to keep RRF order",
				EnvAPIKey, EnvBaseURL, EnvModel, exampleBaseURL, exampleModel)
		}
		return Config{
			Enabled:        false,
			ProviderType:   adapter,
			DisabledReason: fmt.Sprintf("rerank not configured (set %s and %s); results keep RRF order", EnvBaseURL, EnvModel),
		}, nil
	}
	if baseURL == "" {
		return Config{}, fmt.Errorf("%s is required when %s is set (example: %s)", EnvBaseURL, EnvModel, exampleBaseURL)
	}
	if adapter == adapterAuto {
		adapter = DetectAdapter(baseURL)
	}
	if model == "" && adapter != ProviderTEI {
		return Config{}, fmt.Errorf("%s is required when %s is set (example: %s); only %s=%s may leave it empty", EnvModel, EnvBaseURL, exampleModel, EnvAdapter, ProviderTEI)
	}

	cfg := Config{ProviderType: adapter, Model: model, APIKey: apiKey}
	var err error
	if cfg.MaxTokens, err = reliability.IntEnv(EnvMaxTokens, defaultMaxTokens, 1); err != nil {
		return Config{}, err
	}
	if cfg.Timeout, err = reliability.TimeoutFromEnv(); err != nil {
		return Config{}, err
	}
	// rerank 只发生在查询期:配置了查询期独立超时(RS3)即覆盖。
	if queryTimeout, qerr := reliability.QueryTimeoutFromEnv(); qerr != nil {
		return Config{}, qerr
	} else if queryTimeout > 0 {
		cfg.Timeout = queryTimeout
	}
	if cfg.MaxRetries, err = reliability.MaxRetriesFromEnv(); err != nil {
		return Config{}, err
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

// DetectAdapter 按地址选择 rerank adapter：已知托管服务与未知地址都按通用 standard 形状；
// TEI 没有固定主机名，需要用户显式指定。
func DetectAdapter(baseURL string) string {
	return ProviderStandard
}

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
