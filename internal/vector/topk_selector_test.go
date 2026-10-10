package vector

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// TestTopKSelectorPositionTableConsistent：随机推入含重复 ID 的候选，每次推入
// 后检查三件事：ID→堆位置表与堆数组一一对应且不超过 capacity 项；每个父
// 节点都不优于子节点（堆顶是集合里最差的）；堆内集合等于到目前为止每个 ID
// 最高分构成的前 capacity 名。覆盖插入、原位提分、逐出与逐出后再出现。
func TestTopKSelectorPositionTableConsistent(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const rows = 400
	entries := make([]Entry, rows)
	scores := make([]float64, rows)
	for i := range entries {
		entries[i] = Entry{ID: fmt.Sprintf("id-%02d", rng.Intn(25))}
		// 分数取 0.05 的整数倍，制造大量同分。
		scores[i] = float64(rng.Intn(20)) * 0.05
	}
	ix := &Index{entries: entries}
	for _, capacity := range []int{1, 2, 5, 13} {
		s := topKSelector{ix: ix, capacity: capacity}
		best := map[string]topKCandidate{}
		for row := 0; row < rows; row++ {
			cand := topKCandidate{row: row, score: scores[row]}
			s.push(cand)
			id := entries[row].ID
			if prev, ok := best[id]; !ok || s.worse(prev, cand) {
				best[id] = cand
			}
			if len(s.pos) != len(s.items) || len(s.items) > capacity {
				t.Fatalf("capacity=%d row=%d：位置表 %d 项、堆 %d 项", capacity, row, len(s.pos), len(s.items))
			}
			for i, item := range s.items {
				if s.pos[entries[item.row].ID] != i {
					t.Fatalf("capacity=%d row=%d：位置表与堆不一致", capacity, row)
				}
				if i > 0 && s.worse(item, s.items[(i-1)/2]) {
					t.Fatalf("capacity=%d row=%d：位置 %d 比父节点更差，堆性质被破坏", capacity, row, i)
				}
			}
			want := make([]topKCandidate, 0, len(best))
			for _, c := range best {
				want = append(want, c)
			}
			sort.Slice(want, func(a, b int) bool { return s.worse(want[b], want[a]) })
			if len(want) > capacity {
				want = want[:capacity]
			}
			got := append([]topKCandidate(nil), s.items...)
			sort.Slice(got, func(a, b int) bool { return s.worse(got[b], got[a]) })
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("capacity=%d row=%d：堆内集合不是各 ID 最高分的前 %d 名\nwant=%v\n got=%v", capacity, row, capacity, want, got)
			}
		}
	}
}
