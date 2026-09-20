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
		EnvAdapter, EnvBaseURL, EnvAPIKey, legacyEnvVoyageAPIKey, legacyEnvProvider, EnvModel,
		EnvDimension, EnvBatchSize, EnvMaxConcurrency, EnvRPMBudget, EnvTPMBudget,
		EnvThroughputGovernor, EnvBatchAPI, EnvBatchMinChunks,
		"OPENACE_PROVIDER_TIMEOUT", "OPENACE_PROVIDER_MAX_RETRIES",
	} {
		t.Setenv(name, "")
	}
}

// TestVoyageRequiresExplicitBaseURLAndModel：有 key 但缺端点或模型 id 时必须报配置错误，
// 错误里要指明缺哪个变量并给示例；不再回落到写死的 provider 默认值（2026-09-20 用户裁决）。
func TestVoyageRequiresExplicitBaseURLAndModel(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKey, "canary-key-123")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvBaseURL) || !strings.Contains(err.Error(), "example") {
		t.Fatalf("有 key 缺 base_url 应报错并给示例: %v", err)
	}
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvModel) || !strings.Contains(err.Error(), "example") {
		t.Fatalf("有 key 缺 model 应报错并给示例: %v", err)
	}
}

func TestDefaultsWithVoyageKey(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKey, "canary-key-123")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "voyage-code-3")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.Enabled {
		t.Fatalf("有 key 时应启用，reason=%q", cfg.DisabledReason)
	}
	if cfg.ProviderType != ProviderVoyage || cfg.BaseURL != "https://api.voyageai.com/v1" ||
		cfg.Model != "voyage-code-3" || cfg.Dimension != 0 || !cfg.DimensionAuto {
		t.Fatalf("显式身份不符（adapter 应由地址识别为 voyage，维度未配置应为待探测）: %+v", cfg)
	}
	if cfg.BatchSize != 128 || cfg.InitialConcurrency != 16 || cfg.RPMBudget != 0 || cfg.TPMBudget != 0 {
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
	if !strings.Contains(cfg.DisabledReason, "not configured") || !strings.Contains(cfg.DisabledReason, "lexical") {
		t.Fatalf("原因应说明未配置且词法可用: %q", cfg.DisabledReason)
	}
	if cfg.ProfileHash() != "" {
		t.Fatalf("未启用时 ProfileHash 应为空")
	}
}

// TestOwnKeyOnly：embedding 只读自己的 key（不再有 VOYAGE_API_KEY 回退链）。
func TestOwnKeyOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKey, "explicit")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "voyage-code-3")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.APIKey != "explicit" {
		t.Fatalf("OPENACE_EMBEDDING_API_KEY 应生效: key=%q err=%v", cfg.APIKey, err)
	}
}

func TestProviderOff(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAdapter, "off")
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
	t.Setenv(EnvAdapter, "openai-azure")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvAdapter) {
		t.Fatalf("非法 adapter 应显式报错并指明变量: %v", err)
	}
}

func TestOpenAIRequiresBaseURLAndModel(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAdapter, "openai")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled {
		t.Fatalf("只写 adapter、地址与模型都空 = 未配置，不是错误: %+v err=%v", cfg, err)
	}
	t.Setenv(EnvModel, "nomic-embed-code")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvBaseURL) {
		t.Fatalf("有模型缺 base_url 应报错: %v", err)
	}
	t.Setenv(EnvBaseURL, "http://127.0.0.1:8080/v1")
	cfg, err = ConfigFromEnv()
	if err != nil || !cfg.Enabled {
		t.Fatalf("自部署 keyless 应可启用（K21）: %+v err=%v", cfg, err)
	}
	if cfg.APIKey != "" {
		t.Fatalf("keyless 配置不应出现 key")
	}
}

// TestAdapterDetectedFromBaseURL：不写 OPENACE_EMBEDDING_ADAPTER 时按地址识别形状。
func TestAdapterDetectedFromBaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvModel, "m")
	t.Setenv(EnvBaseURL, "http://127.0.0.1:8080/v1")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.ProviderType != ProviderOpenAI {
		t.Fatalf("未知主机应识别为 openai 形状: %+v err=%v", cfg, err)
	}
	t.Setenv(EnvBaseURL, "https://API.voyageai.com/v1")
	cfg, err = ConfigFromEnv()
	if err != nil || cfg.ProviderType != ProviderVoyage {
		t.Fatalf("api.voyageai.com 应识别为 voyage 形状: %+v err=%v", cfg, err)
	}
	t.Setenv(EnvAdapter, "openai")
	cfg, err = ConfigFromEnv()
	if err != nil || cfg.ProviderType != ProviderOpenAI {
		t.Fatalf("显式 adapter 应覆盖识别结果: %+v err=%v", cfg, err)
	}
}

// TestLegacyVariablesRejected：旧变量设置了就报带迁移指引的错误，不静默忽略。
func TestLegacyVariablesRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv(legacyEnvProvider, "voyage")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), legacyEnvProvider) || !strings.Contains(err.Error(), EnvAdapter) {
		t.Fatalf("旧 PROVIDER 应报错并指向 ADAPTER: %v", err)
	}
	clearEnv(t)
	t.Setenv(legacyEnvVoyageAPIKey, "k")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), legacyEnvVoyageAPIKey) || !strings.Contains(err.Error(), EnvAPIKey) {
		t.Fatalf("VOYAGE_API_KEY 应报错并指向各阶段自己的 key: %v", err)
	}
	clearEnv(t)
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "voyage-code-3")
	t.Setenv(EnvBatchAPI, "voyage")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "on") {
		t.Fatalf("BATCH_API=voyage 应报错并提示改为 on: %v", err)
	}
	t.Setenv(EnvBatchAPI, "on")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.BatchAPIMode != ProviderVoyage {
		t.Fatalf("voyage 形状 + BATCH_API=on 应启用批车道: %+v err=%v", cfg, err)
	}
	t.Setenv(EnvBaseURL, "http://127.0.0.1:8080/v1")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "batch API") {
		t.Fatalf("openai 形状不支持批车道应报错: %v", err)
	}
}

// TestDimensionExplicitOrAuto：显式维度记为非 auto；未设为待探测。
func TestDimensionExplicitOrAuto(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvBaseURL, "http://127.0.0.1:8080/v1")
	t.Setenv(EnvModel, "m")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Dimension != 0 || !cfg.DimensionAuto || cfg.ConfigIdentity() == "" {
		t.Fatalf("未设维度应为待探测: %+v err=%v", cfg, err)
	}
	autoIdentity := cfg.ConfigIdentity()
	t.Setenv(EnvDimension, "1536")
	cfg, err = ConfigFromEnv()
	if err != nil || cfg.Dimension != 1536 || cfg.DimensionAuto || cfg.ConfigIdentity() == autoIdentity {
		t.Fatalf("显式维度应生效且配置身份随之变化: %+v err=%v", cfg, err)
	}
}

func TestBaseURLNormalizationAndValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvModel, "voyage-code-3")
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
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "voyage-code-3")
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
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "voyage-code-3")
	t.Setenv(EnvDimension, "2048")
	t.Setenv(EnvBatchSize, "64")
	// 固定并发配置已退役，由迁移测试验证拒绝行为。
	t.Setenv(EnvRPMBudget, "1000")
	t.Setenv(EnvTPMBudget, "2000000")
	t.Setenv("OPENACE_PROVIDER_TIMEOUT", "90s")
	t.Setenv("OPENACE_PROVIDER_MAX_RETRIES", "2")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Dimension != 2048 || cfg.BatchSize != 64 || cfg.InitialConcurrency != 16 ||
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
		APIKey: "key-a", BatchSize: 128, InitialConcurrency: 4, Timeout: 60 * time.Second, MaxRetries: 5}
	hash := base.ProfileHash()
	if len(hash) != 12 {
		t.Fatalf("hash 长度应为 12: %q", hash)
	}

	insensitive := base
	insensitive.APIKey = "key-b"
	insensitive.BatchSize = 16
	insensitive.InitialConcurrency = 1
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
