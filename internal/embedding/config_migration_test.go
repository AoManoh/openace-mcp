package embedding

import (
	"strings"
	"testing"
)

func TestRetiredConcurrencyConfigurationRequiresMigration(t *testing.T) {
	for _, env := range []struct{ key, value string }{{EnvMaxConcurrency, "16"}, {EnvMaxConcurrency, "0"}, {EnvThroughputGovernor, "off"}} {
		t.Run(env.key+"="+env.value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvProvider, ProviderOpenAI)
			t.Setenv(EnvBaseURL, "http://127.0.0.1:1")
			t.Setenv(EnvModel, "local-test")
			t.Setenv(env.key, env.value)
			_, err := ConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), env.key) || !strings.Contains(err.Error(), "remove") {
				t.Fatalf("旧配置应返回包含移除指引的错误: %v", err)
			}
		})
	}
}

func TestRetiredConcurrencyDoesNotDisableUnconfiguredLexicalSearch(t *testing.T) {
	for _, provider := range []string{ProviderOff, ProviderVoyage} {
		t.Run(provider, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvProvider, provider)
			t.Setenv(EnvMaxConcurrency, "16")
			t.Setenv(EnvThroughputGovernor, "off")
			cfg, err := ConfigFromEnv()
			if err != nil || cfg.Enabled {
				t.Fatalf("未启用语义 provider 时词法路径应可用: enabled=%t err=%v", cfg.Enabled, err)
			}
		})
	}
}
