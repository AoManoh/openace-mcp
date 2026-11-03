package managed

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
	"github.com/AoManoh/openace-mcp/internal/rerank"
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
	zero := localengine.Options{}
	expected := profileExpectation{fingerprint: zero.Fingerprint(), components: zero.FingerprintComponents()}
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, expected, zero.Fingerprint(), zero.FingerprintComponents()); err != nil {
		t.Fatalf("同指纹应兼容: %v", err)
	}
	// 旧 daemon 无 engine_profile 广播:按纯词法档兜底(D11)。
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, expected, "", nil); err != nil {
		t.Fatalf("空指纹应按零值档兜底: %v", err)
	}
	if err := compatibleEngineProfile(engine.EngineLocalHybrid, profileExpectation{fingerprint: "expected-a"}, "daemon-b", nil); err == nil {
		t.Fatal("指纹不一致应报错")
	}
}

// TestProfileMismatchListsComponents：配置指纹不一致时，报错逐项列出不一致的
// 组成项与两边的取值，处置是让本会话配置与运行中的 daemon 一致或另起端口，
// 不再提示重启共享 daemon；daemon 不提供组成项时如实说明无法逐项比较。
func TestProfileMismatchListsComponents(t *testing.T) {
	wrapper := localengine.Options{
		Embedding: embedding.Config{
			Enabled: true, ProviderType: embedding.ProviderVoyage,
			BaseURL: "https://api.voyageai.com/v1", Model: "voyage-code-4", Dimension: 1024,
		},
		Rerank: rerank.Config{Enabled: true, ProviderType: rerank.ProviderStandard, BaseURL: "https://api.voyageai.com/v1", Model: "rerank-3"},
	}
	daemonOpts := wrapper
	daemonOpts.Rerank.Model = "rerank-2.5"
	expected := profileExpectation{fingerprint: wrapper.Fingerprint(), components: wrapper.FingerprintComponents()}

	err := compatibleEngineProfile(engine.EngineLocalHybrid, expected, daemonOpts.Fingerprint(), daemonOpts.FingerprintComponents())
	if err == nil {
		t.Fatal("指纹不一致应报错")
	}
	msg := err.Error()
	for _, want := range []string{"rerank", "rerank-3", "rerank-2.5", "OPENACE_DAEMON_ADDR", "match the running daemon"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("报错缺少 %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "restart the daemon") || strings.Contains(msg, "voyage-code-4") {
		t.Fatalf("报错不得提示重启共享 daemon，也不得把没变的 embedding 列为不一致项: %s", msg)
	}

	err = compatibleEngineProfile(engine.EngineLocalHybrid, expected, daemonOpts.Fingerprint(), nil)
	if err == nil || !strings.Contains(err.Error(), "does not report its profile components") {
		t.Fatalf("daemon 不提供组成项时应如实说明: %v", err)
	}
}
