package rerank

import (
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvAdapter, EnvBaseURL, EnvAPIKey, EnvModel, EnvMaxTokens, legacyEnvProvider,
		"VOYAGE_API_KEY", "OPENACE_PROVIDER_TIMEOUT", "OPENACE_PROVIDER_MAX_RETRIES",
	} {
		t.Setenv(name, "")
	}
}

// TestVoyageRequiresExplicitBaseURLAndModel：有 key 但缺端点或模型 id 必须报配置错误并给示例，
// 不回落到写死的模型（2026-09-20 用户裁决：按用户给的模型 url 与 id 透传）。
func TestVoyageRequiresExplicitBaseURLAndModel(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKey, "canary")
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
	t.Setenv(EnvAPIKey, "canary")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "rerank-3")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.Enabled || cfg.ProviderType != ProviderVoyage ||
		cfg.BaseURL != "https://api.voyageai.com/v1" || cfg.Model != "rerank-3" {
		t.Fatalf("显式身份不符: %+v", cfg)
	}
	if cfg.MaxTokens != 200000 || cfg.Timeout != 60*time.Second || cfg.MaxRetries != 5 {
		t.Fatalf("默认参数不符: %+v", cfg)
	}
}

func TestNoKeyDisablesRerankNotError(t *testing.T) {
	clearEnv(t)
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled {
		t.Fatalf("缺 key 应禁用且无错误: %+v err=%v", cfg, err)
	}
	if !strings.Contains(cfg.DisabledReason, "RRF") {
		t.Fatalf("原因应说明保持 RRF 序: %q", cfg.DisabledReason)
	}
}

// TestOwnKeyOnly：rerank 只读自己的 key。
func TestOwnKeyOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKey, "explicit")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "rerank-3")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.APIKey != "explicit" {
		t.Fatalf("OPENACE_RERANK_API_KEY 应生效: %+v err=%v", cfg, err)
	}
}

func TestProviderOff(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAdapter, "off")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled {
		t.Fatalf("off 应禁用且无错误: %+v err=%v", cfg, err)
	}
}

func TestInvalidProviderRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAdapter, "cohere")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvAdapter) {
		t.Fatalf("非法 adapter 应显式报错: %v", err)
	}
}

func TestTEIRequiresBaseURLAllowsKeyless(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAdapter, "tei")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled {
		t.Fatalf("只写 adapter、地址与模型都空 = 未配置，不是错误: %+v err=%v", cfg, err)
	}
	t.Setenv(EnvBaseURL, "http://127.0.0.1:8081/")
	cfg, err = ConfigFromEnv()
	if err != nil || !cfg.Enabled {
		t.Fatalf("自部署 TEI keyless、无模型应可启用: %+v err=%v", cfg, err)
	}
	if cfg.BaseURL != "http://127.0.0.1:8081" {
		t.Fatalf("尾部斜杠应被规范化: %q", cfg.BaseURL)
	}
	if cfg.Model != "" || cfg.ProviderType != ProviderTEI {
		t.Fatalf("tei 的 model 允许为空（端点决定）: %+v", cfg)
	}
}

// TestStandardShapeRequiresModelAndDetectsFromURL：未知地址与已知托管服务都按 standard 形状，模型必填。
func TestStandardShapeRequiresModelAndDetectsFromURL(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvBaseURL, "https://api.cohere.com/v2")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvModel) {
		t.Fatalf("standard 形状缺 model 应报错: %v", err)
	}
	t.Setenv(EnvModel, "rerank-v3.5")
	cfg, err := ConfigFromEnv()
	if err != nil || !cfg.Enabled || cfg.ProviderType != ProviderStandard {
		t.Fatalf("应识别为 standard 形状并启用: %+v err=%v", cfg, err)
	}
}

// TestNotConfiguredAndLegacyVariables：地址与模型都空 = 未配置；只给 key = 错误；旧变量报迁移错误；
// VOYAGE_API_KEY 不再被 rerank 读取（设置它不会启用也不会报错——由 embedding 包负责报迁移错误）。
func TestNotConfiguredAndLegacyVariables(t *testing.T) {
	clearEnv(t)
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled || !strings.Contains(cfg.DisabledReason, "not configured") {
		t.Fatalf("未配置应禁用且说明原因: %+v err=%v", cfg, err)
	}
	t.Setenv(EnvAPIKey, "k")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvBaseURL) {
		t.Fatalf("只给 key 应报错并指明缺地址: %v", err)
	}
	clearEnv(t)
	t.Setenv("VOYAGE_API_KEY", "k")
	cfg, err = ConfigFromEnv()
	if err != nil || cfg.Enabled || cfg.APIKey != "" {
		t.Fatalf("rerank 不再读 VOYAGE_API_KEY: %+v err=%v", cfg, err)
	}
	clearEnv(t)
	t.Setenv(legacyEnvProvider, "voyage")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), legacyEnvProvider) || !strings.Contains(err.Error(), EnvAdapter) {
		t.Fatalf("旧 PROVIDER 应报错并指向 ADAPTER: %v", err)
	}
}

func TestMaxTokensBounds(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvBaseURL, "https://api.voyageai.com/v1")
	t.Setenv(EnvModel, "rerank-3")
	t.Setenv(EnvMaxTokens, "0")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatalf("max tokens=0 应报错")
	}
	t.Setenv(EnvMaxTokens, "50000")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.MaxTokens != 50000 {
		t.Fatalf("max tokens 解析失败: %+v err=%v", cfg, err)
	}
}

func TestIdentityExcludesKey(t *testing.T) {
	cfg := Config{Enabled: true, ProviderType: ProviderVoyage, Model: "rerank-2.5", APIKey: "super-secret"}
	if strings.Contains(cfg.Identity(), "super-secret") {
		t.Fatalf("Identity 不得含 key（K21）: %q", cfg.Identity())
	}
	tei := Config{Enabled: true, ProviderType: ProviderTEI, BaseURL: "http://127.0.0.1:8081"}
	if tei.Identity() == "" {
		t.Fatalf("tei 无 model 时 Identity 应含端点标识")
	}
}
