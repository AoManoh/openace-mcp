// Package fusion 实现 lexical 与 dense 召回的 RRF 融合（迁移方案 §12）：
// 按 rank 融合，不把不同 score 域直接相加；chunk 级去重；确定性 tie-break。
// 纯函数无状态，便于 golden 锁定。
package fusion

import "sort"

// K 是 RRF 常数（§12 冻结为 60；记录在案的受测常数，非针对测试集校准）。
const K = 60

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
	type entry struct {
		score   float64
		lexical bool
		dense   bool
	}
	entries := make(map[string]*entry, len(lexical)+len(dense))
	order := make([]string, 0, len(lexical)+len(dense))

	accumulate := func(ids []string, markDense bool) {
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
			e.score += 1.0 / float64(K+rank+1)
			if markDense {
				e.dense = true
			} else {
				e.lexical = true
			}
		}
	}
	accumulate(lexical, false)
	accumulate(dense, true)

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
