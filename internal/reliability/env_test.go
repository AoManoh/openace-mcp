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
