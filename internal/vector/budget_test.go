package vector

import (
	"errors"
	"fmt"
	"testing"
)

// TestLoadUnlimitedByDefaultBeyondLegacyEnvelope 是 2026-08-26 裁决的核心
// 承诺:maxVectors=0(默认)时不再替换成历史 400K 硬限——超过 40 万行的
// 索引必须可以完整加载,大仓不因规模失去语义检索。用维度 1 把 40 万+行
// 压到 ~1.6MB 数据 + ~50MB idx,直打旧默认替换路径。
func TestLoadUnlimitedByDefaultBeyondLegacyEnvelope(t *testing.T) {
	if testing.Short() {
		t.Skip("40 万行加载用例在 -short 下跳过")
	}
	const rows = 400_050
	dir := t.TempDir()
	entries := make([]Entry, rows)
	vectors := make([][]float32, rows)
	for i := range entries {
		entries[i] = Entry{ID: fmt.Sprintf("id-%032d", i), ContentHash: fmt.Sprintf("hash-%059d", i)}
		vectors[i] = []float32{1} // 维度 1 的单位向量,天然归一化
	}
	dataSum, idxSum, err := Write(dir, 1, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Load(dir, 1, dataSum, idxSum, 0)
	if err != nil {
		t.Fatalf("默认(0=不限)必须加载超过历史 400K 硬限的索引: %v", err)
	}
	if ix.Count() != rows {
		t.Fatalf("行数不符: got=%d want=%d", ix.Count(), rows)
	}
	// 配置了预算折算行数时,超限仍显式拒绝(语义路降级由调用方处理)。
	if _, err := Load(dir, 1, dataSum, idxSum, rows-1); !errors.Is(err, ErrEnvelopeExceeded) {
		t.Fatalf("配置预算后超限必须返回 ErrEnvelopeExceeded: %v", err)
	}
}
