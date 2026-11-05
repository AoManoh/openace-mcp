package reliability

import (
	"strings"
	"testing"
	"time"
)

func TestDurationEnvDefaults(t *testing.T) {
	t.Setenv(EnvProviderTimeout, "")
	value, err := TimeoutFromEnv()
	if err != nil || value != 60*time.Second {
		t.Fatalf("默认超时应为 60s: %v err=%v", value, err)
	}
}

func TestDurationEnvInvalid(t *testing.T) {
	t.Setenv(EnvProviderTimeout, "sixty")
	if _, err := TimeoutFromEnv(); err == nil || !strings.Contains(err.Error(), EnvProviderTimeout) {
		t.Fatalf("非法时长应报错并指明变量: %v", err)
	}
	t.Setenv(EnvProviderTimeout, "-5s")
	if _, err := TimeoutFromEnv(); err == nil {
		t.Fatalf("负时长应报错")
	}
}

// TestIndexTimeoutFromEnv：建索引一侧（文档嵌入与 Batch API）没有设置
// OPENACE_PROVIDER_TIMEOUT 时默认 180 秒，设置了就用设置值；查询一侧回落用的
// TimeoutFromEnv 仍是 60 秒，不随建索引的默认值变长。
func TestIndexTimeoutFromEnv(t *testing.T) {
	t.Setenv(EnvProviderTimeout, "")
	if value, err := IndexTimeoutFromEnv(); err != nil || value != 180*time.Second {
		t.Fatalf("建索引一侧默认超时应为 180s: %v err=%v", value, err)
	}
	if value, err := TimeoutFromEnv(); err != nil || value != 60*time.Second {
		t.Fatalf("查询一侧的回落值应仍为 60s: %v err=%v", value, err)
	}
	t.Setenv(EnvProviderTimeout, "90s")
	if value, err := IndexTimeoutFromEnv(); err != nil || value != 90*time.Second {
		t.Fatalf("显式设置应用于建索引一侧: %v err=%v", value, err)
	}
	t.Setenv(EnvProviderTimeout, "sixty")
	if _, err := IndexTimeoutFromEnv(); err == nil || !strings.Contains(err.Error(), EnvProviderTimeout) {
		t.Fatalf("非法时长应报错并指明变量: %v", err)
	}
}

func TestIntEnvBounds(t *testing.T) {
	t.Setenv(EnvProviderMaxRetries, "")
	value, err := MaxRetriesFromEnv()
	if err != nil || value != 5 {
		t.Fatalf("默认重试应为 5: %d err=%v", value, err)
	}
	t.Setenv(EnvProviderMaxRetries, "0")
	if value, err = MaxRetriesFromEnv(); err != nil || value != 0 {
		t.Fatalf("0 重试合法（不重试）: %d err=%v", value, err)
	}
	t.Setenv(EnvProviderMaxRetries, "-1")
	if _, err = MaxRetriesFromEnv(); err == nil {
		t.Fatalf("负数应报错")
	}
	t.Setenv(EnvProviderMaxRetries, "many")
	if _, err = MaxRetriesFromEnv(); err == nil {
		t.Fatalf("非整数应报错")
	}
}
