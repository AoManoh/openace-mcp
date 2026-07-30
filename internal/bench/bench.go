// Package bench 是 Stage 5 评测 harness 的指标核心：Recall@k / MRR@k、
// 分组报告与成对 bootstrap 置信区间（preregistration 协议 §3 的实现）。
// 本包只含指标与数据契约，不含任何数据集内容；数据资产按 D5 存放在
// 本地私有目录，永不进入仓库。
package bench

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// Query 是统一查询格式（queries.jsonl 行）。
type Query struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Group string `json:"group,omitempty"`
}

// Doc 是统一语料格式（corpus.jsonl 行）。
type Doc struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Language string `json:"language,omitempty"`
}

// Qrels 是相关性判定：qid → docid → 等级（>0 视为相关）。
type Qrels map[string]map[string]int

// Run 是一次系统输出：qid → 按排名的候选 docid（去重后）。
type Run map[string][]string

// perQueryScore 计算单查询指标；rankedIDs 去重后按位次评估。
func recallAtK(ranked []string, relevant map[string]int, k int) float64 {
	if len(relevant) == 0 {
		return math.NaN()
	}
	limit := k
	if limit > len(ranked) {
		limit = len(ranked)
	}
	for _, id := range ranked[:limit] {
		if relevant[id] > 0 {
			return 1
		}
	}
	return 0
}

func mrrAtK(ranked []string, relevant map[string]int, k int) float64 {
	if len(relevant) == 0 {
		return math.NaN()
	}
	limit := k
	if limit > len(ranked) {
		limit = len(ranked)
	}
	for i, id := range ranked[:limit] {
		if relevant[id] > 0 {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// Metric 标识一个可计算指标。
type Metric struct {
	Name string
	Eval func(ranked []string, relevant map[string]int) float64
}

// StandardMetrics 是协议 §3 冻结的指标族。
func StandardMetrics() []Metric {
	return []Metric{
		{Name: "recall@5", Eval: func(r []string, rel map[string]int) float64 { return recallAtK(r, rel, 5) }},
		{Name: "recall@10", Eval: func(r []string, rel map[string]int) float64 { return recallAtK(r, rel, 10) }},
		{Name: "mrr@10", Eval: func(r []string, rel map[string]int) float64 { return mrrAtK(r, rel, 10) }},
	}
}

// GroupScore 是一组查询在一个指标上的汇总。
type GroupScore struct {
	Group   string  `json:"group"`
	Metric  string  `json:"metric"`
	Mean    float64 `json:"mean"`
	Queries int     `json:"queries"`
	// SkippedNoQrels 是因缺 qrels 被剔除的查询数（如实上报，不并入均值）。
	SkippedNoQrels int `json:"skipped_no_qrels,omitempty"`
}

// Evaluate 计算 run 的分组 + 总体指标。查询顺序确定性（按 qid 排序）。
func Evaluate(queries []Query, qrels Qrels, run Run) []GroupScore {
	byGroup := map[string][]Query{}
	for _, query := range queries {
		group := query.Group
		if group == "" {
			group = "all"
		}
		byGroup[group] = append(byGroup[group], query)
	}
	groups := make([]string, 0, len(byGroup))
	for group := range byGroup {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	if len(byGroup) > 1 {
		groups = append(groups, "overall")
		byGroup["overall"] = queries
	}

	var out []GroupScore
	for _, metric := range StandardMetrics() {
		for _, group := range groups {
			members := byGroup[group]
			sum, n, skipped := 0.0, 0, 0
			for _, query := range members {
				relevant := qrels[query.ID]
				if len(relevant) == 0 {
					skipped++
					continue
				}
				sum += metric.Eval(run[query.ID], relevant)
				n++
			}
			score := GroupScore{Group: group, Metric: metric.Name, Queries: n, SkippedNoQrels: skipped}
			if n > 0 {
				score.Mean = sum / float64(n)
			}
			out = append(out, score)
		}
	}
	return out
}

// PairedBootstrap 对两个系统在同一查询集上的指标差（B−A）做成对
// bootstrap：对查询重采样 iters 次，返回差值点估计与 95% CI。
// 协议 §3：CI 不含 0 视为显著。确定性由 seed 保证。
type BootstrapResult struct {
	Metric   string  `json:"metric"`
	Group    string  `json:"group"`
	Delta    float64 `json:"delta"` // mean(B) - mean(A)
	CILow    float64 `json:"ci_low"`
	CIHigh   float64 `json:"ci_high"`
	Queries  int     `json:"queries"`
	Signific bool    `json:"significant"`
}

func PairedBootstrap(queries []Query, qrels Qrels, runA Run, runB Run, metric Metric, group string, iters int, seed int64) (BootstrapResult, error) {
	var deltas []float64
	for _, query := range queries {
		if group != "" && group != "overall" && query.Group != group {
			continue
		}
		relevant := qrels[query.ID]
		if len(relevant) == 0 {
			continue
		}
		a := metric.Eval(runA[query.ID], relevant)
		b := metric.Eval(runB[query.ID], relevant)
		deltas = append(deltas, b-a)
	}
	result := BootstrapResult{Metric: metric.Name, Group: group, Queries: len(deltas)}
	if len(deltas) == 0 {
		return result, fmt.Errorf("组 %q 无可评估查询", group)
	}
	mean := func(values []float64) float64 {
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		return sum / float64(len(values))
	}
	result.Delta = mean(deltas)

	rng := rand.New(rand.NewSource(seed))
	samples := make([]float64, iters)
	resample := make([]float64, len(deltas))
	for i := 0; i < iters; i++ {
		for j := range resample {
			resample[j] = deltas[rng.Intn(len(deltas))]
		}
		samples[i] = mean(resample)
	}
	sort.Float64s(samples)
	lowIdx := int(math.Floor(0.025 * float64(iters)))
	highIdx := int(math.Ceil(0.975*float64(iters))) - 1
	result.CILow = samples[lowIdx]
	result.CIHigh = samples[highIdx]
	result.Signific = result.CILow > 0 || result.CIHigh < 0
	return result, nil
}
