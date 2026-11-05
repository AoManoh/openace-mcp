package localengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// 第 4 步（docs/development/2026-11-03-feedback-triage-fixes.md 第 6 节、第 7 节 T4.4–T4.8）：
// 估算峰值内存不低于 1 GiB 的构建是重型构建，先取同一用户跨进程共享的名额再切分。

// syntheticAssets 返回 n 个各 size 字节的资产。
func syntheticAssets(n int, size int64) workspace.AssetSet {
	assets := make(workspace.AssetSet, 0, n)
	for i := 0; i < n; i++ {
		assets = append(assets, workspace.ContextAsset{RelPath: fmt.Sprintf("f%05d.go", i), Size: size})
	}
	return assets
}

// TestEstimateBuildPeak（T4.4）：按内容字节 ÷ 800 折算 chunk 数、每个 chunk 5.7 KiB 估算峰值，
// 不低于 1 GiB 为重型。系数取第 8 步 B1 之后的复测值：新取向量的首建每 chunk 约 5.5 KiB、
// 复用向量的全量构建约 5.7 KiB（E1），两者已接近，统一按较大的 5.7 KiB 计。
func TestEstimateBuildPeak(t *testing.T) {
	cases := []struct {
		name   string
		delta  bool
		assets workspace.AssetSet
		heavy  bool
	}{
		{"首建 200 MB 内容", false, syntheticAssets(2000, 100_000), true},
		{"首建 100 MB 内容（B1 之后约 0.68 GiB）", false, syntheticAssets(1000, 100_000), false},
		{"复用向量的全量重建 400 MB", false, syntheticAssets(4000, 100_000), true},
		{"delta 改 10 个小文件", true, syntheticAssets(10, 4_000), false},
		{"delta 改 3,000 个文件共 300 MB", true, syntheticAssets(3000, 100_000), true},
	}
	for _, tc := range cases {
		var est buildEstimate
		if tc.delta {
			est = estimateBuild(true, nil, tc.assets)
		} else {
			est = estimateBuild(false, tc.assets, nil)
		}
		if est.heavy() != tc.heavy {
			t.Fatalf("%s: heavy=%v want %v (%+v)", tc.name, est.heavy(), tc.heavy, est)
		}
	}
	// 阈值边界：估算峰值恰好 1 GiB 为重型，少一个 chunk 不是。
	chunks := (heavyBuildPeakThreshold + estimatedPeakBytesPerChunk - 1) / estimatedPeakBytesPerChunk
	at := estimateBuild(false, syntheticAssets(1, chunks*estimatedChunkContentBytes), nil)
	below := estimateBuild(false, syntheticAssets(1, (chunks-1)*estimatedChunkContentBytes), nil)
	if !at.heavy() || below.heavy() || at.mode != "full" || estimateBuild(true, nil, nil).mode != "delta" {
		t.Fatalf("阈值边界判定不符: at=%+v below=%+v", at, below)
	}
}

// forceHeavyBuilds 让之后的每个构建都按重型构建处理，名额目录放进本用例的临时目录；
// 必须在 New 之前调用（名额目录在 New 时按 TMPDIR 定下）。
func forceHeavyBuilds(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	old := heavyBuildPeakThreshold
	heavyBuildPeakThreshold = 1
	t.Cleanup(func() { heavyBuildPeakThreshold = old })
}

// occupySlot 以另一个持有者的身份占住引擎的唯一名额，返回释放函数。
func occupySlot(t *testing.T, e *Engine, note string) func() {
	t.Helper()
	release, err := index.NewHeavyBuildSlots(e.heavySlots.Dir(), e.heavySlots.Count()).TryAcquire(note)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

func waitHeavyState(t *testing.T, e *Engine, root string, want string) engine.WorkspaceStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
		if err == nil && st.HeavyBuild != nil && st.HeavyBuild.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("15 秒内没有进入 heavy_build.state=%s: %+v err=%v", want, st.HeavyBuild, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const otherHolderNote = "workspace=/srv/other build=full estimated_peak_mib=1700"

// TestHeavyBuildQueuesUntilSlotFree（T4.5、T4.8）：重型构建在名额被占时排队，状态为
// queued 并列出持有者；名额释放后取得名额、构建完成，结束后名额释放、状态清空。
func TestHeavyBuildQueuesUntilSlotFree(t *testing.T) {
	forceHeavyBuilds(t)
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	release := occupySlot(t, e, otherHolderNote)
	done := make(chan error, 1)
	go func() {
		_, err := e.Sync(context.Background(), syncRequest(root))
		done <- err
	}()
	st := waitHeavyState(t, e, root, "queued")
	hb := st.HeavyBuild
	if st.Stage != engine.IndexStageQueued || hb.BuildMode != "full" || hb.Slots != 1 || hb.EstimatedPeakMiB < 0 || hb.Since == nil ||
		len(hb.Holders) != 1 || !strings.Contains(hb.Holders[0], fmt.Sprintf("pid=%d", os.Getpid())) || !strings.Contains(hb.Holders[0], otherHolderNote) {
		t.Fatalf("排队时状态应为 queued 并列出持有者: stage=%s heavy=%+v", st.Stage, hb)
	}
	select {
	case err := <-done:
		t.Fatalf("名额释放前构建不应完成: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("名额释放后构建应完成: %v", err)
	}
	st, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil || st.Stage != engine.IndexStageReady || st.HeavyBuild != nil {
		t.Fatalf("构建结束后不应再有 heavy_build: stage=%s heavy=%+v err=%v", st.Stage, st.HeavyBuild, err)
	}
	if holders := e.heavySlots.Holders(); len(holders) != 0 {
		t.Fatalf("构建结束后应释放名额: %+v", holders)
	}
}

// TestHeavyBuildReleasesSlotOnFailureAndCancel（T4.5）：构建失败与等待期间被取消时都不留下
// 名额，状态清空。
func TestHeavyBuildReleasesSlotOnFailureAndCancel(t *testing.T) {
	forceHeavyBuilds(t)
	const dim = 8
	server := newEmbedServer(t, dim)
	t.Cleanup(server.ts.Close)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "heavy-model"))
	root := newFixtureWorkspace(t)

	abort := errors.New("写分段前中断（测试注入）")
	segmentWriteHook = func(context.Context, semanticOutcome) error { return abort }
	_, err := e.Sync(context.Background(), syncRequest(root))
	segmentWriteHook = nil
	if !errors.Is(err, abort) {
		t.Fatalf("注入的写分段失败应让构建失败: %v", err)
	}
	if holders := e.heavySlots.Holders(); len(holders) != 0 {
		t.Fatalf("构建失败后应释放名额: %+v", holders)
	}
	if st, _ := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root}); st.HeavyBuild != nil {
		t.Fatalf("构建失败后不应再有 heavy_build: %+v", st.HeavyBuild)
	}

	occupySlot(t, e, otherHolderNote)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Sync(ctx, syncRequest(root))
		done <- err
	}()
	waitHeavyState(t, e, root, "queued")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("排队时取消应返回 context.Canceled: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后同步没有在 10 秒内返回")
	}
	waitBuildIdle(t, e, root)
	holders := e.heavySlots.Holders()
	if len(holders) != 1 || !strings.HasPrefix(holders[0].Note, otherHolderNote+" acquired=") {
		t.Fatalf("取消的构建不应占名额: %+v", holders)
	}
	if st, _ := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root}); st.HeavyBuild != nil {
		t.Fatalf("取消后不应再有 heavy_build: %+v", st.HeavyBuild)
	}
}

// TestStatusHeavyBuildQueueUnavailable（T4.8）：名额目录不可用时不排队，照常构建，原因写进
// heavy_build.error；构建结束后清空。
func TestStatusHeavyBuildQueueUnavailable(t *testing.T) {
	forceHeavyBuilds(t)
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	// 名额目录的位置被一个普通文件占住。
	if err := os.WriteFile(e.heavySlots.Dir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	drainParserPoolsHookCalls := 0
	var seen *engine.HeavyBuildStatus
	drainParserPoolsHook = func(engine.IndexStage) {
		drainParserPoolsHookCalls++
		st, _ := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
		seen = st.HeavyBuild
	}
	t.Cleanup(func() { drainParserPoolsHook = nil })
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("名额目录不可用时应照常构建: %v", err)
	}
	if drainParserPoolsHookCalls == 0 || seen == nil || seen.State != "running" || !strings.Contains(seen.Error, e.heavySlots.Dir()) {
		t.Fatalf("构建期间 heavy_build 应为 running 并写明名额目录不可用: %+v", seen)
	}
	if st, _ := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root}); st.HeavyBuild != nil {
		t.Fatalf("构建结束后不应再有 heavy_build: %+v", st.HeavyBuild)
	}
}

// TestHeavyBuildSlotsEnv（T4.8）：OPENACE_HEAVY_BUILD_SLOTS 默认 1，整数且不小于 1，非法值报错。
func TestHeavyBuildSlotsEnv(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{{"", 1, false}, {"2", 2, false}, {"0", 0, true}, {"-1", 0, true}, {"two", 0, true}}
	for _, tc := range cases {
		t.Setenv(EnvHeavyBuildSlots, tc.raw)
		opts, err := OptionsFromEnv()
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), EnvHeavyBuildSlots) {
				t.Fatalf("%q 应报错并指名变量: %v", tc.raw, err)
			}
			continue
		}
		if err != nil || opts.HeavyBuildSlots != tc.want {
			t.Fatalf("%q: slots=%d err=%v want %d", tc.raw, opts.HeavyBuildSlots, err, tc.want)
		}
	}
}
