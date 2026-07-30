package localengine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestCompactionAtSegmentThreshold 是 D3/G5：第 8 段触发全量合并回单段，
// tombstone 清零；合并本地纯搬运，未变更内容零 provider 调用（K54 口径）。
func TestCompactionAtSegmentThreshold(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	// 7 次新增文件：delta 链增长到阈值 8 段（新增不产生死 chunk，
	// 隔离垃圾占比触发，专测段数触发）。
	for i := 1; i <= 7; i++ {
		writeFixture(t, root, fmt.Sprintf("gen%d.py", i), fmt.Sprintf("def gen%d():\n    return %d  # edit\n", i, i))
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if len(manifest.Segments) != compactSegmentThreshold {
		t.Fatalf("7 次新增后应达阈值段数: %d", len(manifest.Segments))
	}

	// 第 8 次变更：previous 段数 == 阈值 → 全量合并。
	callsBefore := server.callCount()
	writeFixture(t, root, "gen8.py", "def gen8():\n    return 8  # final\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	compacted, _ := loadActiveManifest(t, e, root)
	if len(compacted.Segments) != 1 {
		t.Fatalf("compaction 应合并回单段: %d", len(compacted.Segments))
	}
	if len(compacted.Tombstones) != 0 {
		t.Fatalf("compaction 应清空 tombstone: %v", compacted.Tombstones)
	}
	if !compacted.SemanticComplete() {
		t.Fatalf("合并后覆盖应完整: %d/%d", compacted.VectorCount, compacted.Counts.Chunks)
	}
	// 合并期间只有触发变更本身的新内容被送 provider（零搬运付费，D3）。
	for _, text := range server.textsSince(callsBefore) {
		if !strings.Contains(text, "return 8") {
			t.Fatalf("compaction 搬运不得付费（D3）: %q", text[:min(60, len(text))])
		}
	}
	// 合并后检索仍正确（新旧内容都在单段中可查）。
	result, err := e.Search(context.Background(), searchRequest(root, "gen8"))
	if err != nil || !strings.Contains(result.Text, "return 8") {
		t.Fatalf("合并后检索: %v %q", err, result.Text)
	}
}

// TestCompactionOnGarbageRatio 是 D3 第二触发条件：删除让死 chunk 占比
// 超阈值后，下一次内容变更走全量合并。
func TestCompactionOnGarbageRatio(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	// 追加三个可删除文件，撑大基段。
	for i := 0; i < 3; i++ {
		writeFixture(t, root, fmt.Sprintf("extra%d.py", i), fmt.Sprintf("def extra%d():\n    return %d\n", i, i))
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	base, _ := loadActiveManifest(t, e, root)
	if len(base.Segments) != 1 {
		t.Fatalf("首建单段: %d", len(base.Segments))
	}

	// 删除大部分文件（manifest-only delta）→ 垃圾占比越阈。
	for _, name := range []string{"extra0.py", "extra1.py", "extra2.py", "README.md", "util.py"} {
		if err := os.Remove(rootJoin(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	afterDelete, _ := loadActiveManifest(t, e, root)
	if garbageRatio(afterDelete) < compactGarbageRatio {
		t.Fatalf("场景应超垃圾阈值: %.2f", garbageRatio(afterDelete))
	}

	// 下一次内容变更：应走全量而非 delta。
	writeFixture(t, root, "main.go", fixtureMainGo+"\n// trigger compaction\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	compacted, _ := loadActiveManifest(t, e, root)
	if len(compacted.Segments) != 1 {
		t.Fatalf("垃圾占比触发应全量合并: %d 段", len(compacted.Segments))
	}
	if len(compacted.Tombstones) != 0 {
		t.Fatalf("合并应清空 tombstone: %v", compacted.Tombstones)
	}
	if garbageRatio(compacted) != 0 {
		t.Fatalf("合并后垃圾应清零: %.2f", garbageRatio(compacted))
	}
}

// TestQueryLoopSurvivesCompaction 是暗坑 K42：跨越 compaction 的连续
// 发布下，并发查询无错误无空窗（K11 扩展）。
func TestQueryLoopSurvivesCompaction(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	queryErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
			if err != nil {
				select {
				case queryErr <- err:
				default:
				}
				return
			}
			if !strings.Contains(result.Text, "HandleLogin") {
				select {
				case queryErr <- fmt.Errorf("查询空窗: %q", result.Text):
				default:
				}
				return
			}
		}
	}()

	// 9 次编辑跨越 compaction 阈值。
	for i := 1; i <= 9; i++ {
		writeFixture(t, root, "util.py", fmt.Sprintf("def parse_config(path):\n    return %d\n", i))
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-queryErr:
		t.Fatalf("并发查询失败（K42）: %v", err)
	default:
	}
	final, _ := loadActiveManifest(t, e, root)
	if len(final.Segments) > compactSegmentThreshold {
		t.Fatalf("段数应受阈值约束（K49）: %d", len(final.Segments))
	}
}
