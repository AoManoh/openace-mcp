// Package fusion 实现 lexical 与 dense 召回的 RRF 融合（迁移方案 §12）：
// 按 rank 融合，不把不同 score 域直接相加；chunk 级去重；确定性 tie-break。
// 纯函数无状态，便于 golden 锁定。
package fusion

import "sort"

// K 是 RRF 常数（§12 冻结为 60；记录在案的受测常数，非针对测试集校准）。
const K = 60

// Params 是 RRF 融合参数（P5B-T10b 加权定值扫描用；默认值 = 现状行为）。
// 权重只影响两路贡献的相对比例：score(id) = LexWeight/(K+r_lex) +
// DenseWeight/(K+r_dense)。等权 (1,1) 与历史无权重公式逐位一致。
type Params struct {
	K           int
	LexWeight   float64
	DenseWeight float64
}

// DefaultParams 返回当前默认融合参数（未配置 = 现状行为，Stage 5 计划 §4）。
func DefaultParams() Params {
	return Params{K: K, LexWeight: 1, DenseWeight: 1}
}

// 结果来源标记（set 级 retrieval_mode 之下的 per-hit 调试信息）。
const (
	SourceLexical = "lexical"
	SourceDense   = "dense"
	SourceBoth    = "both"
)

// Fused 是融合后的候选。
type Fused struct {
	ID     string
	Score  float64
	Source string
}

// RRF 融合两路排名（rank 1-based 隐含于切片顺序）：
// score(id) = Σ 1/(K+rank)。同一路内重复 ID 只计首个 rank（防御性去重）；
// 排序按 (score desc, ID asc) 保证确定性（暗坑 K27）。单路缺席时
// 退化为该路原序。
func RRF(lexical []string, dense []string) []Fused {
	return RRFWeighted(lexical, dense, DefaultParams())
}

// RRFWeighted 是带参数的 RRF 融合（T10b 加权定值）。语义与 RRF 相同，
// 仅按 Params 对两路贡献加权；权重 <0 按 0 处理，K<=0 回落默认。
func RRFWeighted(lexical []string, dense []string, params Params) []Fused {
	if params.K <= 0 {
		params.K = K
	}
	if params.LexWeight < 0 {
		params.LexWeight = 0
	}
	if params.DenseWeight < 0 {
		params.DenseWeight = 0
	}
	type entry struct {
		score   float64
		lexical bool
		dense   bool
	}
	entries := make(map[string]*entry, len(lexical)+len(dense))
	order := make([]string, 0, len(lexical)+len(dense))

	accumulate := func(ids []string, weight float64, markDense bool) {
		seen := make(map[string]bool, len(ids))
		for rank, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			e, ok := entries[id]
			if !ok {
				e = &entry{}
				entries[id] = e
				order = append(order, id)
			}
			e.score += weight / float64(params.K+rank+1)
			if markDense {
				e.dense = true
			} else {
				e.lexical = true
			}
		}
	}
	accumulate(lexical, params.LexWeight, false)
	accumulate(dense, params.DenseWeight, true)

	fused := make([]Fused, 0, len(order))
	for _, id := range order {
		e := entries[id]
		source := SourceLexical
		switch {
		case e.lexical && e.dense:
			source = SourceBoth
		case e.dense:
			source = SourceDense
		}
		fused = append(fused, Fused{ID: id, Score: e.score, Source: source})
	}
	sort.Slice(fused, func(a, b int) bool {
		if fused[a].Score != fused[b].Score {
			return fused[a].Score > fused[b].Score
		}
		return fused[a].ID < fused[b].ID
	})
	return fused
}
