package embedding

import (
	"strings"
	"testing"
	"time"
)

// clearEnv 清空全部相关环境变量，隔离宿主环境（含用户 shell 中可能存在的
// VOYAGE_API_KEY）。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvProvider, EnvBaseURL, EnvAPIKey, EnvVoyageAPIKey, EnvModel,
		EnvDimension, EnvBatchSize, EnvMaxConcurrency, EnvRPMBudget, EnvTPMBudget,
		"OPENACE_PROVIDER_TIMEOUT", "OPENACE_PROVIDER_MAX_RETRIES",
	} {
		t.Setenv(name, "")
	}
}

func TestDefaultsWithVoyageKey(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVoyageAPIKey, "canary-key-123")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.Enabled {
		t.Fatalf("有 key 时应启用，reason=%q", cfg.DisabledReason)
	}
	if cfg.ProviderType != ProviderVoyage || cfg.BaseURL != "https://api.voyageai.com/v1" ||
		cfg.Model != "voyage-code-3" || cfg.Dimension != 1024 {
		t.Fatalf("默认身份不符: %+v", cfg)
	}
	if cfg.BatchSize != 128 || cfg.MaxConcurrency != 8 || cfg.RPMBudget != 0 || cfg.TPMBudget != 0 {
		t.Fatalf("默认运维参数不符: %+v", cfg)
	}
	if cfg.Timeout != 60*time.Second || cfg.MaxRetries != 5 {
		t.Fatalf("默认超时/重试不符: timeout=%v retries=%d", cfg.Timeout, cfg.MaxRetries)
	}
	if cfg.APIKey != "canary-key-123" {
		t.Fatalf("VOYAGE_API_KEY 回退链失效")
	}
}

func TestNoKeyDisablesSemanticNotError(t *testing.T) {
	clearEnv(t)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("缺 key 不是错误（核心理念 3）: %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("缺 key 时不应启用")
	}
	if !strings.Contains(cfg.DisabledReason, "no API key") || !strings.Contains(cfg.DisabledReason, "lexical") {
		t.Fatalf("原因应说明缺 key 且词法可用: %q", cfg.DisabledReason)
	}
	if cfg.ProfileHash() != "" {
		t.Fatalf("未启用时 ProfileHash 应为空")
	}
}

func TestExplicitKeyOverridesVoyageKey(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVoyageAPIKey, "fallback")
	t.Setenv(EnvAPIKey, "explicit")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.APIKey != "explicit" {
		t.Fatalf("OPENACE_EMBEDDING_API_KEY 应优先: key=%q err=%v", cfg.APIKey, err)
	}
}

func TestProviderOff(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvProvider, "off")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled {
		t.Fatalf("off 应禁用且无错误: %+v err=%v", cfg, err)
	}
	if !strings.Contains(cfg.DisabledReason, "off") {
		t.Fatalf("原因应说明 off: %q", cfg.DisabledReason)
	}
}

func TestInvalidProviderRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvProvider, "openai-azure")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvProvider) {
		t.Fatalf("非法 provider 应显式报错并指明变量: %v", err)
	}
}

func TestOpenAIRequiresBaseURLAndModel(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvProvider, "openai")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvBaseURL) {
		t.Fatalf("openai 缺 base_url 应报错: %v", err)
	}
	t.Setenv(EnvBaseURL, "http://127.0.0.1:8080/v1")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvModel) {
		t.Fatalf("openai 缺 model 应报错: %v", err)
	}
	t.Setenv(EnvModel, "nomic-embed-code")
	cfg, err := ConfigFromEnv()
	if err != nil || !cfg.Enabled {
		t.Fatalf("自部署 keyless 应可启用（K21）: %+v err=%v", cfg, err)
	}
	if cfg.APIKey != "" {
		t.Fatalf("keyless 配置不应出现 key")
	}
}

func TestBaseURLNormalizationAndValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVoyageAPIKey, "k")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1/")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.BaseURL != "https://api.voyageai.com/v1" {
		t.Fatalf("尾部斜杠应被规范化: %q err=%v", cfg.BaseURL, err)
	}
	t.Setenv(EnvBaseURL, "not-a-url")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatalf("非法 URL 应报错")
	}
	t.Setenv(EnvBaseURL, "ftp://host/v1")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatalf("非 http(s) scheme 应报错")
	}
}

func TestBatchSizeBounds(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVoyageAPIKey, "k")
	t.Setenv(EnvBatchSize, "1001")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvBatchSize) {
		t.Fatalf("超过 1000 条应报错: %v", err)
	}
	t.Setenv(EnvBatchSize, "0")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatalf("batch=0 应报错")
	}
}

func TestOperationalEnvParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVoyageAPIKey, "k")
	t.Setenv(EnvDimension, "2048")
	t.Setenv(EnvBatchSize, "64")
	t.Setenv(EnvMaxConcurrency, "8")
	t.Setenv(EnvRPMBudget, "1000")
	t.Setenv(EnvTPMBudget, "2000000")
	t.Setenv("OPENACE_PROVIDER_TIMEOUT", "90s")
	t.Setenv("OPENACE_PROVIDER_MAX_RETRIES", "2")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Dimension != 2048 || cfg.BatchSize != 64 || cfg.MaxConcurrency != 8 ||
		cfg.RPMBudget != 1000 || cfg.TPMBudget != 2000000 ||
		cfg.Timeout != 90*time.Second || cfg.MaxRetries != 2 {
		t.Fatalf("运维参数解析不符: %+v", cfg)
	}
}

// TestProfileHashSensitivity 是 P3-T01 业务验收：换 key 不触发重建，
// 换模型/维度/端点必然触发平行子树（阶段计划 D4/K24）。
func TestProfileHashSensitivity(t *testing.T) {
	base := Config{Enabled: true, ProviderType: ProviderVoyage,
		BaseURL: "https://api.voyageai.com/v1", Model: "voyage-code-3", Dimension: 1024,
		APIKey: "key-a", BatchSize: 128, MaxConcurrency: 4, Timeout: 60 * time.Second, MaxRetries: 5}
	hash := base.ProfileHash()
	if len(hash) != 12 {
		t.Fatalf("hash 长度应为 12: %q", hash)
	}

	insensitive := base
	insensitive.APIKey = "key-b"
	insensitive.BatchSize = 16
	insensitive.MaxConcurrency = 1
	insensitive.Timeout = time.Second
	insensitive.MaxRetries = 0
	insensitive.RPMBudget = 5
	if insensitive.ProfileHash() != hash {
		t.Fatalf("key 与运维参数不得影响 profile hash")
	}

	for name, mutate := range map[string]func(*Config){
		"model":     func(c *Config) { c.Model = "voyage-4" },
		"dimension": func(c *Config) { c.Dimension = 512 },
		"base_url":  func(c *Config) { c.BaseURL = "http://127.0.0.1:9000/v1" },
		"type":      func(c *Config) { c.ProviderType = ProviderOpenAI },
	} {
		changed := base
		mutate(&changed)
		if changed.ProfileHash() == hash {
			t.Fatalf("%s 变化必须改变 profile hash", name)
		}
	}
}

func TestIdentityExcludesKey(t *testing.T) {
	cfg := Config{Enabled: true, ProviderType: ProviderVoyage, Model: "voyage-code-3",
		Dimension: 1024, APIKey: "super-secret-canary"}
	if strings.Contains(cfg.Identity(), "super-secret-canary") {
		t.Fatalf("Identity 不得含 key（K21）: %q", cfg.Identity())
	}
}
