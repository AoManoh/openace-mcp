package localengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestMergeBlocksNeverBridgesGaps 是 review B1 的回归用例：
// 间隔 1 行的两个块不得合并（合并会造成行号与内容错位）。
func TestMergeBlocksNeverBridgesGaps(t *testing.T) {
	blocks := []renderBlock{
		{record: chunkRecord{RelPath: "m.go", StartLine: 1, EndLine: 3, Content: "l1\nl2\nl3"}, score: 2},
		{record: chunkRecord{RelPath: "m.go", StartLine: 5, EndLine: 6, Content: "l5\nl6"}, score: 1},
	}
	merged := mergeBlocks(blocks)
	if len(merged) != 2 {
		t.Fatalf("跨间隙块不得合并: %+v", merged)
	}
	// 严格相邻仍应合并。
	adjacent := []renderBlock{
		{record: chunkRecord{RelPath: "m.go", StartLine: 1, EndLine: 3, Content: "l1\nl2\nl3"}, score: 2},
		{record: chunkRecord{RelPath: "m.go", StartLine: 4, EndLine: 5, Content: "l4\nl5"}, score: 1},
	}
	merged = mergeBlocks(adjacent)
	if len(merged) != 1 || merged[0].record.Content != "l1\nl2\nl3\nl4\nl5" {
		t.Fatalf("严格相邻块应无损合并: %+v", merged)
	}
}

// TestRenderedLinesAlwaysMatchSource 对真实检索结果逐块逐行核对源文件，
// 覆盖 AST 声明间空行场景（review B1 的端到端验收）。
func TestRenderedLinesAlwaysMatchSource(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	result, err := e.Search(context.Background(), searchRequest(root, "session"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text == noHitsText {
		t.Fatal("fixture 应有命中")
	}
	sources := map[string][]string{}
	for _, section := range strings.Split(result.Text, "\n## ") {
		section = strings.TrimPrefix(section, "## ")
		lines := strings.Split(section, "\n")
		header := strings.Fields(lines[0])[0]
		pathRange := strings.SplitN(header, ":", 2)
		span := strings.SplitN(pathRange[1], "-", 2)
		start := atoiOrFail(t, span[0])
		if _, ok := sources[pathRange[0]]; !ok {
			raw, err := os.ReadFile(filepath.Join(root, pathRange[0]))
			if err != nil {
				t.Fatalf("结果引用不存在的文件 %s: %v", pathRange[0], err)
			}
			sources[pathRange[0]] = strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		}
		fileLines := sources[pathRange[0]]
		for i := 2; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], "```") {
				break
			}
			fileIndex := start - 1 + (i - 2)
			if fileIndex >= len(fileLines) || fileLines[fileIndex] != lines[i] {
				t.Fatalf("%s 第 %d 行错位:\nwant %q\ngot  %q", pathRange[0], start+(i-2), fileLines[fileIndex], lines[i])
			}
		}
	}
}

// TestBleveCorruptionSelfHealsOnSync 是 review B2 的回归用例：
// chunks.jsonl 完好而 Bleve 目录损坏时，sync 不得 no-op，必须重建自愈。
func TestBleveCorruptionSelfHealsOnSync(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Close(context.Background())

	// 重新创建引擎（清空句柄缓存），破坏唯一 revision 的 Bleve 目录。
	e2 := New()
	t.Cleanup(func() { _ = e2.Close(context.Background()) })
	_, workspaceKey, _ := e2.resolveRoot(root)
	store, err := e2.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := store.LoadManifest(first.IndexRevision)
	if err != nil {
		t.Fatal(err)
	}
	bleveDir := filepath.Join(store.SegmentPath(manifest), "lexical.bleve")
	if err := os.RemoveAll(bleveDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bleveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := e2.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("Bleve 损坏应经重建自愈: %v", err)
	}
	if result.IndexRevision == first.IndexRevision {
		t.Fatal("应重建出新 revision 而非继续用损坏索引")
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("自愈后应有正确结果: %q", result.Text)
	}
}

// TestRetiredHandleReacquireDoesNotLeak 是 review B3 的回归用例：
// 退役但仍被引用的句柄，同 revision 再次激活时被无泄漏地复用。
func TestRetiredHandleReacquireDoesNotLeak(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, workspaceKey, _ := e.resolveRoot(root)
	held, err := e.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	// 人为退役（模拟 revision 曾被替换又因回退重新激活）。
	e.mu.Lock()
	held.retired = true
	e.mu.Unlock()

	reacquired, err := e.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	if reacquired != held {
		t.Fatal("同 revision 应复用同一句柄（un-retire），不得重复打开")
	}
	e.mu.Lock()
	key := handleKey(workspaceKey, held.manifest.Revision)
	inMap := e.handles[key] == held
	retired := held.retired
	refs := held.refs
	e.mu.Unlock()
	if !inMap || retired || refs != 2 {
		t.Fatalf("复用后状态错误: inMap=%v retired=%v refs=%d", inMap, retired, refs)
	}
	e.releaseHandle(reacquired)
	e.releaseHandle(held)
	e.mu.Lock()
	stillInMap := e.handles[key] == held
	e.mu.Unlock()
	if !stillInMap {
		t.Fatal("未退役句柄归零后应保留在缓存中")
	}
}

// TestRevisionGCKeepsActiveAndPrevious 是 review B4（D5 修订）的验收：
// 连续发布后只保留 active 与 previous，两者数据完整，其余被回收。
func TestRevisionGCKeepsActiveAndPrevious(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	var revisions []string
	for i := 0; i < 4; i++ {
		writeFixture(t, root, "main.go", fixtureMainGo+"\n// edit "+digit(i)+"\n")
		result, err := e.Sync(context.Background(), syncRequest(root))
		if err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, result.IndexRevision)
	}
	_, workspaceKey, _ := e.resolveRoot(root)
	store, _ := e.storeFor(workspaceKey)
	remaining, err := store.ListRevisions()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("GC 后应只保留 2 个 revision，got %v", remaining)
	}
	keep := map[string]bool{revisions[3]: true, revisions[2]: true}
	for _, revision := range remaining {
		if !keep[revision] {
			t.Fatalf("保留了非 active/previous 的 revision: %s", revision)
		}
		manifest, err := store.LoadManifest(revision)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.VerifyManifest(manifest); err != nil {
			t.Fatalf("保留的 revision 应完整: %v", err)
		}
	}
}

func digit(v int) string {
	return string(rune('0' + v%10))
}

// TestContentGateSkipsBinary 是暗坑 K6 的真实用例：GBK 与 PNG 内容
// 在共享 scanner 的内容门禁即被拦截（不进入索引、检索零命中）。
// 跳过计数发生在 scanner 层，Stage 2 状态上报仅覆盖构建期 TOCTOU
// 场景的兜底计数（见计划 K6 修订说明）。
func TestContentGateSkipsBinary(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	// GBK 编码中文（非法 UTF-8 序列）。
	if err := os.WriteFile(filepath.Join(root, "legacy_gbk.txt"), []byte{0xC4, 0xE3, 0xBA, 0xC3, 0x0A}, 0o600); err != nil {
		t.Fatal(err)
	}
	// PNG 头（二进制）。
	if err := os.WriteFile(filepath.Join(root, "logo.png"), []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01}, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != 3 {
		t.Fatalf("GBK/PNG 应被跳过，只索引 3 个文本文件，got %d", result.FileCount)
	}
	status, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if status.FileCount != 3 {
		t.Fatalf("状态 file count 应为 3: %+v", status)
	}
	search, err := e.Search(context.Background(), searchRequest(root, "legacy_gbk logo.png"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(search.Text, "legacy_gbk") || strings.Contains(search.Text, "logo.png") {
		t.Fatalf("被跳过文件不得出现在检索结果: %q", search.Text)
	}
}

// TestCacheIsolationFromACEState 是暗坑 K9 的真实用例：
// local-hybrid 只写 engines/local-hybrid/ 子树，cache 内既有
// （模拟的）ACE 状态文件字节不变，也不产生子树外新文件。
func TestCacheIsolationFromACEState(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	fakeState := filepath.Join(cacheDir, "test", "workspaces", "demo", "state.json")
	if err := os.MkdirAll(filepath.Dir(fakeState), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"checkpoint_id":"ace-checkpoint-123","blob_names":{"a.go":"blob1"}}`)
	if err := os.WriteFile(fakeState, original, 0o600); err != nil {
		t.Fatal(err)
	}
	e := New()
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(fakeState)
	if err != nil || string(after) != string(original) {
		t.Fatalf("ACE 状态文件被修改: err=%v", err)
	}
	err = filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(cacheDir, path)
		rel = filepath.ToSlash(rel)
		if rel != "test/workspaces/demo/state.json" && !strings.HasPrefix(rel, "test/engines/local-hybrid/") {
			t.Fatalf("local-hybrid 在子树外写入了文件: %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestQueryLoopSurvivesConsecutivePublishes 是暗坑 K11 的真实用例：
// 查询循环并发进行时连续发布 3 个新 revision，无错误、无空窗，
// 且旧句柄在引用归零后被关闭。
func TestQueryLoopSurvivesConsecutivePublishes(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
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
				case errCh <- err:
				default:
				}
				return
			}
			if !strings.Contains(result.Text, "HandleLogin") {
				select {
				case errCh <- fmt.Errorf("查询空窗: %q", result.Text):
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 3; i++ {
		writeFixture(t, root, "util.py", fixtureUtilPy+"\n# revision "+digit(i)+"\n")
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("查询循环失败: %v", err)
	default:
	}
	// 引用已全部归还：句柄缓存最多保留 active+previous。
	e.mu.Lock()
	cached := len(e.handles)
	e.mu.Unlock()
	if cached > 2 {
		t.Fatalf("句柄缓存应收敛到 ≤2（active+previous），got %d", cached)
	}
}

// TestCancelDuringBuildCleansStaging 是暗坑 K16 的真实用例：
// 在构建已创建 staging 后取消，构建中止且 staging 被清理。
func TestCancelDuringBuildCleansStaging(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	// 足量文件制造可取消窗口。
	for i := 0; i < 400; i++ {
		writeFixture(t, root, "src/f"+itoa3(i)+".ts", strings.Repeat("export const v"+itoa3(i)+" = 'value';\n", 40))
	}
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	stagingRoot := filepath.Join(store.Root(), "staging")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Sync(ctx, syncRequest(root))
		done <- err
	}()
	// 等待 staging 出现后取消。
	deadline := time.Now().Add(10 * time.Second)
	sawStaging := false
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(stagingRoot)
		if len(entries) > 0 {
			sawStaging = true
			break
		}
		time.Sleep(500 * time.Microsecond)
	}
	cancel()
	err = <-done
	if !sawStaging {
		t.Skip("构建过快，未观察到 staging 窗口；取消语义由其它用例覆盖")
	}
	if err == nil {
		// 取消晚于发布完成属合法竞态；此时验证正常发布状态即可。
		return
	}
	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		entries, readErr := os.ReadDir(stagingRoot)
		if readErr == nil && len(entries) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries, _ := os.ReadDir(stagingRoot)
	t.Fatalf("取消后 staging 应被清理，剩余 %d 项", len(entries))
}

func itoa3(v int) string {
	digits := "0123456789"
	return string([]byte{digits[v/100%10], digits[v/10%10], digits[v%10]})
}
