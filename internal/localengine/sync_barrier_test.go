package localengine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 语义收紧(review -15 (1),用户裁决方向 (i)):Sync 返回的索引必须包含
// 调用发起前已完成的全部写入。历史缺陷:caller 加入一个扫描早于其写入
// 的在建构建(join-in-flight 无扫描时点校验),拿回旧 revision,
// TestMixedLoadEightClients 低频 flake 即此。
//
// 确定性复现:手工植入一个"到达前已开始"的假构建(startedAt=过去,
// 完成时返回旧 revision),真实写入新内容后调用 Sync——收紧前 Sync 会
// join 假构建拿到旧 revision;收紧后应等待其结束并自跑新一轮,返回
// 包含新写入的 revision。
func TestSyncBarrierRefusesStaleInFlightBuild(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}

	// 植入假在建构建:startedAt 在过去(早于即将发生的写入与 Sync 到达),
	// 200ms 后以"旧 revision"完成——模拟扫描早于写入的真实构建。
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	stale := &buildCall{
		done:      make(chan struct{}),
		cancel:    func() {},
		waiters:   1,
		startedAt: time.Now().Add(-time.Second),
	}
	e.mu.Lock()
	e.inflight[workspaceKey] = stale
	e.mu.Unlock()
	var once sync.Once
	go func() {
		time.Sleep(200 * time.Millisecond)
		stale.result = engine.Result{Engine: EngineID, IndexRevision: first.IndexRevision}
		e.mu.Lock()
		if e.inflight[workspaceKey] == stale {
			delete(e.inflight, workspaceKey)
		}
		e.mu.Unlock()
		once.Do(func() { close(stale.done) })
	}()

	// 写入发生在 Sync 到达前——收紧语义要求结果包含它。
	writeFixture(t, root, "util.py", "def parse_config(path):\n    return 99  # barrier\n")
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if result.IndexRevision == first.IndexRevision {
		t.Fatalf("Sync 加入了扫描早于写入的在建构建,返回旧 revision %s(语义收紧失效)", first.IndexRevision)
	}
	search, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(search.Text, "return 99") {
		t.Fatalf("新写入未入索引: %q", search.Text)
	}
}

// TestSyncBarrierJoinsFreshBuild:到达晚于构建开始的 caller 正常 join
// (singleflight 不放大)——收紧只拒绝"到达前已开始"的构建。
func TestSyncBarrierJoinsFreshBuild(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "util.py", "def parse_config(path):\n    return 7  # fresh\n")
	var wg sync.WaitGroup
	revisions := make([]string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			r, err := e.Sync(context.Background(), syncRequest(root))
			if err != nil {
				t.Error(err)
				return
			}
			revisions[slot] = r.IndexRevision
		}(i)
	}
	wg.Wait()
	for i := 1; i < 8; i++ {
		if revisions[i] != revisions[0] {
			t.Fatalf("同波次 caller 应收敛同一 revision: %v", revisions)
		}
	}
}
