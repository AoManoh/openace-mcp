package main

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/localengine"
)

// clearAugmentEnv 清空历史上游凭据变量,验证零凭据可用性(Stage 7 后
// 这些变量已无消费方,清空仅保证宿主 shell 残留不影响断言)。
func clearAugmentEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AUGMENT_SESSION_AUTH", "AUGMENT_TOKEN", "AUGMENT_TENANT"} {
		t.Setenv(key, "")
	}
}

// clearProviderEnv 清空语义 provider 配置（含宿主 shell 可能存在的
// VOYAGE_API_KEY），保证测试确定性。
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OPENACE_EMBEDDING_PROVIDER", "OPENACE_EMBEDDING_BASE_URL", "OPENACE_EMBEDDING_API_KEY",
		"VOYAGE_API_KEY", "OPENACE_EMBEDDING_MODEL", "OPENACE_RERANK_PROVIDER",
		"OPENACE_RETRIEVAL_DEGRADE", "OPENACE_RERANK_DEGRADE",
	} {
		t.Setenv(key, "")
	}
}

// TestLocalHybridStartsWithoutCredentials 是 P2-T10 业务验收 (b)：
// OPENACE_ENGINE=local-hybrid 且无任何 AUGMENT_*/VOYAGE key 时可构建服务
// （缺 key = semantic off，词法完整可用，Stage 3 D1）。
func TestLocalHybridStartsWithoutCredentials(t *testing.T) {
	clearAugmentEnv(t)
	clearProviderEnv(t)
	t.Setenv("OPENACE_ENGINE", "local-hybrid")
	service, err := buildLocalService(context.Background())
	if err != nil {
		t.Fatalf("local-hybrid 无凭据应可启动: %v", err)
	}
	if _, ok := service.(*localengine.Engine); !ok {
		t.Fatalf("应返回 localengine.Engine，got %T", service)
	}
}

// TestLocalHybridInvalidProviderEnvRejected 是 Stage 3 P3-T09：
// provider 配置错误在启动即报且指明变量。
func TestLocalHybridInvalidProviderEnvRejected(t *testing.T) {
	clearAugmentEnv(t)
	clearProviderEnv(t)
	t.Setenv("OPENACE_ENGINE", "local-hybrid")
	t.Setenv("OPENACE_EMBEDDING_PROVIDER", "azure-openai")
	if _, err := buildLocalService(context.Background()); err == nil || !strings.Contains(err.Error(), "OPENACE_EMBEDDING_PROVIDER") {
		t.Fatalf("非法 provider 应在启动显式报错: %v", err)
	}
	t.Setenv("OPENACE_EMBEDDING_PROVIDER", "")
	t.Setenv("OPENACE_RETRIEVAL_DEGRADE", "silent")
	if _, err := buildLocalService(context.Background()); err == nil || !strings.Contains(err.Error(), "OPENACE_RETRIEVAL_DEGRADE") {
		t.Fatalf("非法降级值应在启动显式报错: %v", err)
	}
}

// TestDefaultEngineIsLocalHybrid:未设 OPENACE_ENGINE 走 local-hybrid
// (Stage 6,2026-08-02 批准)。
func TestDefaultEngineIsLocalHybrid(t *testing.T) {
	clearAugmentEnv(t)
	clearProviderEnv(t)
	t.Setenv("OPENACE_ENGINE", "")
	service, err := buildLocalService(context.Background())
	if err != nil {
		t.Fatalf("默认路径构建失败: %v", err)
	}
	if _, ok := service.(*localengine.Engine); !ok {
		t.Fatalf("默认应为 localengine.Engine,got %T", service)
	}
}

// TestExplicitACERemoved:Stage 7(2026-08-04 用户裁决)后 OPENACE_ENGINE=ace
// 显式报可行动错误,不静默回退。
func TestExplicitACERemoved(t *testing.T) {
	clearAugmentEnv(t)
	t.Setenv("OPENACE_ENGINE", "ace")
	_, err := buildLocalService(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Stage 7") {
		t.Fatalf("ace 选项应显式报 Stage 7 移除: %v", err)
	}
}

func TestInvalidEngineRejected(t *testing.T) {
	t.Setenv("OPENACE_ENGINE", "bleve-only")
	if _, err := buildLocalService(context.Background()); err == nil || !strings.Contains(err.Error(), "OPENACE_ENGINE") {
		t.Fatalf("非法引擎值应显式报错: %v", err)
	}
}
