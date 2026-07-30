package main

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/localengine"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// clearAugmentEnv 清空全部上游凭据，验证 local-hybrid 的无凭据可用性。
func clearAugmentEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AUGMENT_SESSION_AUTH", "AUGMENT_TOKEN", "AUGMENT_TENANT", "OPENACE_SESSION_FILE", "OPENACE_PROFILES_FILE"} {
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

// TestDefaultEngineUnchanged 是 P2-T10 业务验收 (a)：
// 未设 OPENACE_ENGINE 时仍走 legacy ACE 路径（含凭据时返回 workspace.Syncer）。
func TestDefaultEngineUnchanged(t *testing.T) {
	clearAugmentEnv(t)
	t.Setenv("OPENACE_ENGINE", "")
	t.Setenv("AUGMENT_TOKEN", "test-token")
	t.Setenv("AUGMENT_TENANT", "https://example.test/")
	service, err := buildLocalService(context.Background())
	if err != nil {
		t.Fatalf("默认路径构建失败: %v", err)
	}
	if _, ok := service.(*workspace.Syncer); !ok {
		t.Fatalf("默认应返回 workspace.Syncer，got %T", service)
	}
}

func TestInvalidEngineRejected(t *testing.T) {
	t.Setenv("OPENACE_ENGINE", "bleve-only")
	if _, err := buildLocalService(context.Background()); err == nil || !strings.Contains(err.Error(), "OPENACE_ENGINE") {
		t.Fatalf("非法引擎值应显式报错: %v", err)
	}
}
