package rerank

import (
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvProvider, EnvBaseURL, EnvAPIKey, EnvModel, EnvMaxTokens,
		embeddingVoyageKeyEnv, "OPENACE_PROVIDER_TIMEOUT", "OPENACE_PROVIDER_MAX_RETRIES",
	} {
		t.Setenv(name, "")
	}
}

func TestDefaultsWithVoyageKey(t *testing.T) {
	clearEnv(t)
	t.Setenv(embeddingVoyageKeyEnv, "canary")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.Enabled || cfg.ProviderType != ProviderVoyage ||
		cfg.BaseURL != "https://api.voyageai.com/v1" || cfg.Model != "rerank-2.5" {
		t.Fatalf("默认身份不符: %+v", cfg)
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

func TestExplicitKeyOverridesVoyageKey(t *testing.T) {
	clearEnv(t)
	t.Setenv(embeddingVoyageKeyEnv, "fallback")
	t.Setenv(EnvAPIKey, "explicit")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.APIKey != "explicit" {
		t.Fatalf("OPENACE_RERANK_API_KEY 应优先: %+v err=%v", cfg, err)
	}
}

func TestProviderOff(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvProvider, "off")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled {
		t.Fatalf("off 应禁用且无错误: %+v err=%v", cfg, err)
	}
}

func TestInvalidProviderRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvProvider, "cohere")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvProvider) {
		t.Fatalf("非法 provider 应显式报错: %v", err)
	}
}

func TestTEIRequiresBaseURLAllowsKeyless(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvProvider, "tei")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvBaseURL) {
		t.Fatalf("tei 缺 base_url 应报错: %v", err)
	}
	t.Setenv(EnvBaseURL, "http://127.0.0.1:8081/")
	cfg, err := ConfigFromEnv()
	if err != nil || !cfg.Enabled {
		t.Fatalf("自部署 TEI keyless 应可启用: %+v err=%v", cfg, err)
	}
	if cfg.BaseURL != "http://127.0.0.1:8081" {
		t.Fatalf("尾部斜杠应被规范化: %q", cfg.BaseURL)
	}
	if cfg.Model != "" {
		t.Fatalf("tei 的 model 允许为空（端点决定）")
	}
}

func TestMaxTokensBounds(t *testing.T) {
	clearEnv(t)
	t.Setenv(embeddingVoyageKeyEnv, "k")
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
