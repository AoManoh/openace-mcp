package daemon

import (
	"context"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// identifiableSyncer 模拟自述引擎类型的服务。
type identifiableSyncer struct {
	fakeSyncer
	id string
}

func (s identifiableSyncer) EngineID() string { return s.id }

// TestServedByAdvertisesEngine 是暗坑 K8 的 daemon 侧验收：
// 身份广播必须如实携带引擎类型与能力位。
func TestServedByAdvertisesEngine(t *testing.T) {
	useTempTaskStore(t)
	local := NewServer(identifiableSyncer{id: engine.EngineLocalHybrid})
	t.Cleanup(func() { _ = local.Shutdown(context.Background()) })
	identity := local.servedBy()
	if identity.Engine != engine.EngineLocalHybrid {
		t.Fatalf("engine 字段应为 local-hybrid: %+v", identity)
	}
	if !identity.Capabilities["engine_local_hybrid"] {
		t.Fatal("local-hybrid daemon 应广播 engine_local_hybrid 能力")
	}
	if identity.Capabilities["provider_profiles"] {
		t.Fatal("local-hybrid daemon 不应广播 provider_profiles 能力")
	}

	legacy := NewServer(fakeSyncer{})
	t.Cleanup(func() { _ = legacy.Shutdown(context.Background()) })
	legacyIdentity := legacy.servedBy()
	if legacyIdentity.Engine != engine.EngineACE {
		t.Fatalf("无自述能力的服务应按 ace 广播: %+v", legacyIdentity)
	}
	if !legacyIdentity.Capabilities["provider_profiles"] {
		t.Fatal("ace daemon 应保留 provider_profiles 能力")
	}
}
