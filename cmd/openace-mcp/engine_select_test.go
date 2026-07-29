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

// TestLocalHybridStartsWithoutCredentials 是 P2-T10 业务验收 (b)：
// OPENACE_ENGINE=local-hybrid 且无任何 AUGMENT_* 时可构建服务。
func TestLocalHybridStartsWithoutCredentials(t *testing.T) {
	clearAugmentEnv(t)
	t.Setenv("OPENACE_ENGINE", "local-hybrid")
	service, err := buildLocalService(context.Background())
	if err != nil {
		t.Fatalf("local-hybrid 无凭据应可启动: %v", err)
	}
	if _, ok := service.(*localengine.Engine); !ok {
		t.Fatalf("应返回 localengine.Engine，got %T", service)
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
