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

// TestWeightedEqualMatchesLegacy 等权 (1,1) 必须与无权重 RRF 逐位一致
// （T10b：加权能力不得改变现状默认行为）。
func TestWeightedEqualMatchesLegacy(t *testing.T) {
	lex := []string{"a", "b", "c", "e"}
	dense := []string{"b", "d", "a"}
	legacy := RRF(lex, dense)
	weighted := RRFWeighted(lex, dense, DefaultParams())
	if !reflect.DeepEqual(legacy, weighted) {
		t.Fatalf("等权应与历史公式逐位一致:\nlegacy=%+v\nweighted=%+v", legacy, weighted)
	}
}

// TestWeightedFavorsDense 加权 golden：LexWeight=0.15/DenseWeight=0.85 时，
// dense 首位应压过 lexical 首位（手工计算：a=0.15/21≈0.00714，
// d=0.85/21≈0.04048，b=0.15/22+0.85/22≈0.04545 仍以双路居首）。
func TestWeightedFavorsDense(t *testing.T) {
	params := Params{K: 20, LexWeight: 0.15, DenseWeight: 0.85}
	fused := RRFWeighted([]string{"a", "b"}, []string{"d", "b"}, params)
	wantIDs := []string{"b", "d", "a"}
	for i, want := range wantIDs {
		if fused[i].ID != want {
			t.Fatalf("位置 %d: got=%s want=%s (全部=%+v)", i, fused[i].ID, want, fused)
		}
	}
	wantScores := []float64{0.15/22 + 0.85/22, 0.85 / 21, 0.15 / 21}
	for i := range wantScores {
		if math.Abs(fused[i].Score-wantScores[i]) > 1e-12 {
			t.Fatalf("位置 %d 分数: got=%v want=%v", i, fused[i].Score, wantScores[i])
		}
	}
}

// TestWeightedParamClamps 非法参数收敛：负权重按 0、K<=0 回落默认。
func TestWeightedParamClamps(t *testing.T) {
	fused := RRFWeighted([]string{"a"}, []string{"d"}, Params{K: -1, LexWeight: -5, DenseWeight: 1})
	if fused[0].ID != "d" {
		t.Fatalf("负词法权重应按 0 处理，dense 应居首: %+v", fused)
	}
	if math.Abs(fused[0].Score-1.0/61) > 1e-12 {
		t.Fatalf("K<=0 应回落默认 60: %v", fused[0].Score)
	}
	// 权重归零的一路仍参与来源标记与去重，只是不贡献分数。
	if fused[1].ID != "a" || fused[1].Score != 0 {
		t.Fatalf("零权重路候选应保留但零分: %+v", fused[1])
	}
}
