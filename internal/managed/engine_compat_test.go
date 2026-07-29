package managed

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
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
