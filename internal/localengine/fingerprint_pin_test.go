package localengine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// pinnedFingerprintOptions 是指纹固定值测试用的四组配置：纯词法、embedding 加
// 精排、精排降级为 deny、批量嵌入加严格档。
func pinnedFingerprintOptions() map[string]Options {
	embed := embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderVoyage,
		BaseURL: "https://api.voyageai.com/v1", Model: "voyage-code-4", Dimension: 1024,
		TemplateVersion: embedTemplateVersion,
	}
	rr := rerank.Config{Enabled: true, ProviderType: rerank.ProviderStandard, BaseURL: "https://api.voyageai.com/v1", Model: "rerank-3"}
	full := Options{Embedding: embed, Rerank: rr}
	deny := full
	deny.RerankDegrade = DegradeDeny
	bulkStrict := full
	bulkStrict.Embedding.BatchAPIMode = embedding.ProviderVoyage
	bulkStrict.QualityStrict = true
	return map[string]Options{
		"lexical":          {},
		"embedding+rerank": full,
		"rerank-deny":      deny,
		"bulk+strict":      bulkStrict,
	}
}

// TestFingerprintPinnedValues 固定四组配置的指纹值（取自改为按组成项计算之前
// 的实现）。指纹值一变，已部署的 wrapper 会把同样配置的新 daemon 判成配置
// 不一致，所以组成项的重构不得改变指纹。
func TestFingerprintPinnedValues(t *testing.T) {
	want := map[string]string{
		"lexical":          "df9c1c8cf029",
		"embedding+rerank": "055a5e4ca404",
		"rerank-deny":      "ba2a154434d6",
		"bulk+strict":      "0b5b293478d5",
	}
	for name, opts := range pinnedFingerprintOptions() {
		if got := opts.Fingerprint(); got != want[name] {
			t.Fatalf("%s: fingerprint %s, want %s", name, got, want[name])
		}
	}
}

// TestFingerprintComponents 核对组成项：六项齐全且顺序固定、不含 key 与 URL
// 里的用户信息和查询参数；两组配置的组成项相同当且仅当指纹相同；只改精排
// 模型时只有 rerank 一项不同。
func TestFingerprintComponents(t *testing.T) {
	opts := pinnedFingerprintOptions()["embedding+rerank"]
	opts.Embedding.APIKey = "secret-embed-key"
	opts.Rerank.APIKey = "secret-rerank-key"
	components := opts.FingerprintComponents()
	names := FingerprintComponentNames()
	if want := []string{"lexical", "embedding", "rerank", "rerank_degrade", "quality_strict", "bulk"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("component names %v, want %v", names, want)
	}
	if len(components) != len(names) {
		t.Fatalf("components %v, want exactly %v", components, names)
	}
	for _, name := range names {
		value := components[name]
		if value == "" {
			t.Fatalf("component %s is empty: %v", name, components)
		}
		if strings.Contains(value, "secret") {
			t.Fatalf("component %s leaks a credential: %q", name, value)
		}
	}
	if !strings.Contains(components["embedding"], "voyage-code-4") || !strings.Contains(components["rerank"], "rerank-3") {
		t.Fatalf("components must name the models: %v", components)
	}

	changed := opts
	changed.Rerank.Model = "rerank-2.5"
	other := changed.FingerprintComponents()
	for _, name := range names {
		if (components[name] != other[name]) != (name == "rerank") {
			t.Fatalf("changing only the rerank model must change only the rerank component; %s: %q vs %q", name, components[name], other[name])
		}
	}

	sets := pinnedFingerprintOptions()
	for a, optsA := range sets {
		for b, optsB := range sets {
			same := reflect.DeepEqual(optsA.FingerprintComponents(), optsB.FingerprintComponents())
			if same != (optsA.Fingerprint() == optsB.Fingerprint()) {
				t.Fatalf("%s vs %s: components equal=%v but fingerprints equal=%v", a, b, same, optsA.Fingerprint() == optsB.Fingerprint())
			}
		}
	}

	withUser := opts
	withUser.Embedding.BaseURL = "https://user:pass@gateway.example/v1?token=abc"
	label := withUser.FingerprintComponents()["embedding"]
	if strings.Contains(label, "pass") || strings.Contains(label, "token=abc") {
		t.Fatalf("embedding component must not carry URL user info or query: %q", label)
	}
	if label == components["embedding"] {
		t.Fatalf("a different endpoint must change the embedding component: %q", label)
	}
}
