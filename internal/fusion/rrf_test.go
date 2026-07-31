package fusion

import (
	"math"
	"reflect"
	"testing"
)

// TestRRFGolden 手工计算的默认融合 golden（T10b-3 定值 K=20/0.15/0.85）：
// lexical=[a,b,c]，dense=[b,d]：
//
//	b = 0.15/22 + 0.85/21 ≈ 0.047294（both）
//	d = 0.85/22 ≈ 0.038636（dense）
//	a = 0.15/21 ≈ 0.007143（lexical）
//	c = 0.15/23 ≈ 0.006522（lexical）
func TestRRFGolden(t *testing.T) {
	fused := RRF([]string{"a", "b", "c"}, []string{"b", "d"})
	wantIDs := []string{"b", "d", "a", "c"}
	wantSources := []string{SourceBoth, SourceDense, SourceLexical, SourceLexical}
	wantScores := []float64{0.15/22 + 0.85/21, 0.85 / 22, 0.15 / 21, 0.15 / 23}
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
	if math.Abs(fused[0].Score-0.15/21) > 1e-12 {
		t.Fatalf("重复 ID 应只计首个 rank: %v", fused[0].Score)
	}
}

// TestLexicalTopHitStaysInTopFive 是 P3-T05 验收在 T10b-3 定值后的
// 修订形态：加权默认（0.15/0.85）下两路完全不相交时，词法第一命中
// 沉到全部 dense 深度之后是数学必然；修订后的契约是词法强命中保持
// 在 top-5 窗口内（doc 级 R@5 口径），头部排序保护由 rerank 精排
// （head=50 全指标最优，tuning §2.7）与 dense 路自身召回承担。
// exact-symbol 端到端保护的 symprobe 复验登记于 tuning §2.8。
func TestLexicalTopHitStaysInTopFive(t *testing.T) {
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
	if pos < 0 || pos > 4 {
		t.Fatalf("词法第一命中融合后应留在 top-5: pos=%d %+v", pos, fused)
	}
}

// TestRRFMatchesDefaultParams RRF() 必须与 RRFWeighted(DefaultParams)
// 逐位一致（引擎默认路径与便捷入口同语义）。
func TestRRFMatchesDefaultParams(t *testing.T) {
	lex := []string{"a", "b", "c", "e"}
	dense := []string{"b", "d", "a"}
	if !reflect.DeepEqual(RRF(lex, dense), RRFWeighted(lex, dense, DefaultParams())) {
		t.Fatal("RRF 应与 DefaultParams 加权逐位一致")
	}
}

// TestWeightedEqualMatchesHistoricalFormula 等权 {60,1,1} 必须复现历史
// 无权重公式的 golden（迁移方案 §12 原冻结行为可按参数复现，用于
// 回归对照与旧 run 复算）。
func TestWeightedEqualMatchesHistoricalFormula(t *testing.T) {
	fused := RRFWeighted([]string{"a", "b", "c"}, []string{"b", "d"}, Params{K: 60, LexWeight: 1, DenseWeight: 1})
	wantIDs := []string{"b", "a", "d", "c"}
	wantScores := []float64{1.0/62 + 1.0/61, 1.0 / 61, 1.0 / 62, 1.0 / 63}
	for i := range wantIDs {
		if fused[i].ID != wantIDs[i] || math.Abs(fused[i].Score-wantScores[i]) > 1e-12 {
			t.Fatalf("位置 %d: got=%+v want id=%s score=%v", i, fused[i], wantIDs[i], wantScores[i])
		}
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
