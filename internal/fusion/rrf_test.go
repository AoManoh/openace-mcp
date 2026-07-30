package fusion

import (
	"math"
	"reflect"
	"testing"
)

// TestRRFGolden 手工计算的融合 golden：
// lexical=[a,b,c]，dense=[b,d]：
//
//	b = 1/62 + 1/61 ≈ 0.032524（both）
//	a = 1/61 ≈ 0.016393（lexical）
//	d = 1/62 ≈ 0.016129（dense）
//	c = 1/63 ≈ 0.015873（lexical）
func TestRRFGolden(t *testing.T) {
	fused := RRF([]string{"a", "b", "c"}, []string{"b", "d"})
	wantIDs := []string{"b", "a", "d", "c"}
	wantSources := []string{SourceBoth, SourceLexical, SourceDense, SourceLexical}
	wantScores := []float64{1.0/62 + 1.0/61, 1.0 / 61, 1.0 / 62, 1.0 / 63}
	if len(fused) != 4 {
		t.Fatalf("应有 4 个候选: %d", len(fused))
	}
	for i := range fused {
		if fused[i].ID != wantIDs[i] || fused[i].Source != wantSources[i] {
			t.Fatalf("位置 %d: got=%+v want id=%s source=%s", i, fused[i], wantIDs[i], wantSources[i])
		}
		if math.Abs(fused[i].Score-wantScores[i]) > 1e-12 {
			t.Fatalf("位置 %d 分数: got=%v want=%v", i, fused[i].Score, wantScores[i])
		}
	}
}

// TestSingleRoutePreservesOrder 是单路缺席的退化断言（D8 降级路径依赖）。
func TestSingleRoutePreservesOrder(t *testing.T) {
	fused := RRF([]string{"x", "y", "z"}, nil)
	got := []string{fused[0].ID, fused[1].ID, fused[2].ID}
	if !reflect.DeepEqual(got, []string{"x", "y", "z"}) {
		t.Fatalf("纯词法应保持原序: %v", got)
	}
	for _, f := range fused {
		if f.Source != SourceLexical {
			t.Fatalf("来源应为 lexical: %+v", f)
		}
	}
	fused = RRF(nil, []string{"p", "q"})
	if fused[0].ID != "p" || fused[1].ID != "q" || fused[0].Source != SourceDense {
		t.Fatalf("纯语义应保持原序: %+v", fused)
	}
}

// TestTieBreakByID 是暗坑 K27 的确定性断言。
func TestTieBreakByID(t *testing.T) {
	fused := RRF([]string{"zz"}, []string{"aa"})
	if fused[0].ID != "aa" || fused[1].ID != "zz" {
		t.Fatalf("同分应按 ID 升序: %+v", fused)
	}
}

func TestEmptyRoutes(t *testing.T) {
	if fused := RRF(nil, nil); len(fused) != 0 {
		t.Fatalf("双空应为空: %v", fused)
	}
}

// TestDuplicateWithinRouteCountsFirstRank 防御性去重：同路重复 ID 只计首个 rank。
func TestDuplicateWithinRouteCountsFirstRank(t *testing.T) {
	fused := RRF([]string{"a", "a", "b"}, nil)
	if len(fused) != 2 {
		t.Fatalf("应去重: %v", fused)
	}
	if math.Abs(fused[0].Score-1.0/61) > 1e-12 {
		t.Fatalf("重复 ID 应只计首个 rank: %v", fused[0].Score)
	}
}

// TestLexicalTopHitStaysInHead 是 P3-T05 业务验收的单元级形态：
// 词法强命中（exact identifier 定义处）融合后仍在头部（≤2 位，
// 完整端到端验收见 P3-T07/T11）。
func TestLexicalTopHitStaysInHead(t *testing.T) {
	lexical := []string{"target-def", "l2", "l3", "l4"}
	dense := []string{"d1", "d2", "d3", "d4"}
	fused := RRF(lexical, dense)
	pos := -1
	for i, f := range fused {
		if f.ID == "target-def" {
			pos = i
			break
		}
	}
	if pos < 0 || pos > 1 {
		t.Fatalf("词法第一命中融合后应在头部: pos=%d %+v", pos, fused[:3])
	}
}
