package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/localengine"
)

// TestFusionOfflineReplayConsistency 是 T10b 离线复算的守恒探针（env
// 门控）：同进程内对同一查询取 SearchRoutes 双路候选，按 fusion.RRF
// 语义本地复算，与 SearchCandidates 的在线结果逐位对比。用于裁决
// "离线扫描工具是否忠实复刻引擎融合"。
func TestFusionOfflineReplayConsistency(t *testing.T) {
	workspace := os.Getenv("OPENACE_FUSION_PROBE_WS")
	if workspace == "" {
		t.Skip("OPENACE_FUSION_PROBE_WS 未设置")
	}
	// 本探针只裁决"离线 RRF 复算是否忠实"，精排必须关闭：rerank 在
	// key 存在时默认开启，会在融合后重排头部（T10b 排障发现）。
	t.Setenv("OPENACE_RERANK_PROVIDER", "off")
	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := localengine.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close(context.Background())
	ctx := context.Background()
	ref := engine.WorkspaceRef{DirectoryPath: workspace}
	if _, err := eng.Sync(ctx, engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatal(err)
	}
	queries := []string{
		"sort by a token in string python",
		"python print results of query loop",
		"get wechat access token python",
	}
	for _, q := range queries {
		routes, err := eng.SearchRoutes(ctx, engine.SearchRequest{Workspace: ref, Query: q}, 60)
		if err != nil {
			t.Fatal(err)
		}
		// 重复性：同 API 双调用的 dense 路是否逐位一致。
		routes2, err := eng.SearchRoutes(ctx, engine.SearchRequest{Workspace: ref, Query: q}, 60)
		if err != nil {
			t.Fatal(err)
		}
		denseStable := len(routes.Dense) == len(routes2.Dense)
		if denseStable {
			for i := range routes.Dense {
				if routes.Dense[i].ID != routes2.Dense[i].ID {
					denseStable = false
					t.Logf("  dense 不稳定 @%d: %s vs %s", i, routes.Dense[i].ID[:8], routes2.Dense[i].ID[:8])
					break
				}
			}
		}
		lexStable := len(routes.Lex) == len(routes2.Lex)
		if lexStable {
			for i := range routes.Lex {
				if routes.Lex[i].ID != routes2.Lex[i].ID {
					lexStable = false
					t.Logf("  lex 不稳定 @%d: %s vs %s", i, routes.Lex[i].ID[:8], routes2.Lex[i].ID[:8])
					break
				}
			}
		}
		t.Logf("重复性 q=%q denseStable=%v lexStable=%v", q, denseStable, lexStable)
		lexIDs := make([]string, 0, len(routes.Lex))
		for _, ref := range routes.Lex {
			lexIDs = append(lexIDs, ref.ID)
		}
		denseIDs := make([]string, 0, len(routes.Dense))
		for _, ref := range routes.Dense {
			denseIDs = append(denseIDs, ref.ID)
		}
		fused := fusion.RRF(lexIDs, denseIDs)
		offline := make([]string, 0, len(fused))
		for _, f := range fused {
			offline = append(offline, f.ID)
		}
		candidates, err := eng.SearchCandidates(ctx, engine.SearchRequest{Workspace: ref, Query: q})
		if err != nil {
			t.Fatal(err)
		}
		online := make([]string, 0, len(candidates))
		for _, c := range candidates {
			online = append(online, c.ID)
		}
		limit := len(online)
		if len(offline) < limit {
			limit = len(offline)
		}
		mismatch := -1
		for i := 0; i < limit; i++ {
			if offline[i] != online[i] {
				mismatch = i
				break
			}
		}
		t.Logf("q=%q lex=%d dense=%d online=%d offline=%d firstMismatch=%d",
			q, len(lexIDs), len(denseIDs), len(online), len(offline), mismatch)
		if mismatch >= 0 {
			lo, hi := mismatch-2, mismatch+3
			if lo < 0 {
				lo = 0
			}
			if hi > limit {
				hi = limit
			}
			for i := lo; i < hi; i++ {
				t.Logf("  [%d] online=%s offline=%s", i, online[i], offline[i])
			}
			// 定位 online[mismatch] 在双路的 rank。
			pos := func(ids []string, target string) int {
				for i, id := range ids {
					if id == target {
						return i
					}
				}
				return -1
			}
			target := online[mismatch]
			t.Logf("  online[%d]=%s 在 lex rank=%d dense rank=%d", mismatch, target, pos(lexIDs, target), pos(denseIDs, target))
			var scores []string
			for _, f := range fused[:min(8, len(fused))] {
				scores = append(scores, fmt.Sprintf("%s=%.6f", f.ID[:8], f.Score))
			}
			sort.Strings(scores)
			t.Logf("  offline fused 头部分数: %v", scores)
			t.Fail()
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
