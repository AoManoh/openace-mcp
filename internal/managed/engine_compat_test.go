package managed

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
)

// Stage 7 后引擎兼容判定:唯一引擎 local-hybrid;不广播 engine 的
// daemon(退役前旧构建)按不兼容处理并给出可行动错误。
func TestCompatibleEngine(t *testing.T) {
	if err := compatibleEngine(engine.EngineLocalHybrid, engine.EngineLocalHybrid); err != nil {
		t.Fatalf("同引擎应兼容: %v", err)
	}
	err := compatibleEngine(engine.EngineLocalHybrid, "")
	if err == nil || !strings.Contains(err.Error(), "pre-Stage 7") {
		t.Fatalf("无自述 daemon 应判不兼容并指名原因: %v", err)
	}
	if err := compatibleEngine(engine.EngineLocalHybrid, "something-else"); err == nil {
		t.Fatal("异引擎应不兼容")
	}
}

// K29:local-hybrid 配置指纹一致性判定不受 Stage 7 影响。
func TestCompatibleEngineProfile(t *testing.T) {
	fp := localengine.Options{}.Fingerprint()
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, fp, fp); err != nil {
		t.Fatalf("同指纹应兼容: %v", err)
	}
	// 旧 daemon 无 engine_profile 广播:按纯词法档兜底(D11)。
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, fp, ""); err != nil {
		t.Fatalf("空指纹应按零值档兜底: %v", err)
	}
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, "expected-a", "daemon-b"); err == nil {
		t.Fatal("指纹不一致应报错")
	}
}
