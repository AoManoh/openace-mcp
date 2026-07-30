package managed

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
)

// TestCompatibleEngine 是暗坑 K8 的复用判定验收。
func TestCompatibleEngine(t *testing.T) {
	if err := compatibleEngine(engine.EngineACE, engine.EngineACE); err != nil {
		t.Fatalf("同引擎应兼容: %v", err)
	}
	if err := compatibleEngine(engine.EngineACE, ""); err != nil {
		t.Fatalf("旧 daemon（无 engine 字段）应按 ace 兼容: %v", err)
	}
	if err := compatibleEngine(engine.EngineLocalHybrid, engine.EngineLocalHybrid); err != nil {
		t.Fatalf("local-hybrid 对 local-hybrid 应兼容: %v", err)
	}
	err := compatibleEngine(engine.EngineLocalHybrid, engine.EngineACE)
	if err == nil || !strings.Contains(err.Error(), "engine") {
		t.Fatalf("引擎不匹配应判不可复用: %v", err)
	}
	if err := compatibleEngine(engine.EngineACE, engine.EngineLocalHybrid); err == nil {
		t.Fatal("ace 请求不得复用 local-hybrid daemon")
	}
}

// TestCompatibleEngineProfile 是暗坑 K29 的复用判定验收：
// provider/degrade 配置漂移不得静默复用。
func TestCompatibleEngineProfile(t *testing.T) {
	lexicalOnly := localengine.Options{}.Fingerprint()
	semanticOn := localengine.Options{Embedding: embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderVoyage,
		BaseURL: "https://api.voyageai.com/v1", Model: "voyage-code-3", Dimension: 1024,
	}}.Fingerprint()

	if err := compatibleEngineProfile(engine.EngineACE, "", "anything"); err != nil {
		t.Fatalf("ace 引擎不参与指纹判定: %v", err)
	}
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, semanticOn, semanticOn); err != nil {
		t.Fatalf("指纹一致应兼容: %v", err)
	}
	err := compatibleEngineProfile(engine.EngineLocalHybrid, semanticOn, lexicalOnly)
	if err == nil || !strings.Contains(err.Error(), "restart the daemon") {
		t.Fatalf("指纹不一致应判不可复用并提示重启: %v", err)
	}
	// 旧 daemon（Stage 2，无 engine_profile）按纯词法档对待（D11）。
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, lexicalOnly, ""); err != nil {
		t.Fatalf("旧 daemon × semantic off 期望应兼容: %v", err)
	}
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, semanticOn, ""); err == nil {
		t.Fatal("旧 daemon × semantic on 期望不得复用")
	}
	// 降级开关同样进入指纹：deny 与默认 allow 不可互换。
	denyProfile := localengine.Options{RetrievalDegrade: localengine.DegradeDeny}.Fingerprint()
	if denyProfile == lexicalOnly {
		t.Fatal("degrade 开关必须改变指纹")
	}
}
