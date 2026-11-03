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

}

// profiledSyncer 模拟自述配置指纹的服务（Stage 3 暗坑 K29）。
type profiledSyncer struct {
	identifiableSyncer
	profile string
}

func (s profiledSyncer) EngineProfileFingerprint() string { return s.profile }

// TestServedByAdvertisesEngineProfile 是暗坑 K29 的 daemon 侧验收。
func TestServedByAdvertisesEngineProfile(t *testing.T) {
	useTempTaskStore(t)
	server := NewServer(profiledSyncer{
		identifiableSyncer: identifiableSyncer{id: engine.EngineLocalHybrid},
		profile:            "abc123def456",
	})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	identity := server.servedBy()
	if identity.EngineProfile != "abc123def456" {
		t.Fatalf("engine_profile 应广播配置指纹: %+v", identity)
	}

	// 无自述能力（legacy ACE / 旧实现）不广播该字段。
}

// componentSyncer 模拟同时自述配置指纹组成项的服务。
type componentSyncer struct {
	profiledSyncer
	components map[string]string
}

func (s componentSyncer) EngineProfileComponents() map[string]string { return s.components }

// TestDaemonStatusProfileComponents：daemon 状态带配置指纹的组成项，wrapper 在
// 指纹不一致时据此逐项列出不同的设置；服务不自述组成项时不输出该字段。
func TestDaemonStatusProfileComponents(t *testing.T) {
	useTempTaskStore(t)
	profiled := profiledSyncer{
		identifiableSyncer: identifiableSyncer{id: engine.EngineLocalHybrid},
		profile:            "abc123def456",
	}
	server := NewServer(componentSyncer{profiledSyncer: profiled, components: map[string]string{"rerank": "standard/rerank-3"}})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	status := server.statusSnapshot(context.Background())
	if status.EngineProfileComponents["rerank"] != "standard/rerank-3" || status.EngineProfile != "abc123def456" {
		t.Fatalf("daemon 状态应带配置指纹与组成项: %+v", status)
	}

	plain := NewServer(profiled)
	t.Cleanup(func() { _ = plain.Shutdown(context.Background()) })
	if components := plain.statusSnapshot(context.Background()).EngineProfileComponents; components != nil {
		t.Fatalf("不自述组成项的服务不应输出该字段: %v", components)
	}
}
