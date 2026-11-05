package localengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/rerank"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// 第 5 步（docs/development/2026-11-03-feedback-triage-fixes.md 第 7 节 T5.1–T5.4）：
// 用旧 revision 降级作答（index-building、index-refreshing、stale-index）时，[DEGRADED]
// 横幅下一行的 [INDEX] 写明所用快照的 revision 与发布时刻，以及最近一次扫描看到的、
// 快照之后磁盘上有改动的文件。改动前结果只说明索引不新鲜，调用方不知道哪些文件要直接读。

// freshnessFixture 建一个带语义路的引擎与工作区并完成首建，返回首建发布的 manifest。
func freshnessFixture(t *testing.T, adjust func(*Options)) (*Engine, string, *index.Manifest) {
	t.Helper()
	const dim = 8
	server := newEmbedServer(t, dim)
	t.Cleanup(server.ts.Close)
	opts := embedOptions(server.ts.URL, dim, 8, "freshness-model")
	if adjust != nil {
		adjust(&opts)
	}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	writeFixture(t, root, "lib.go", "package app\n\n// Lib 之后会被删除。\nfunc Lib() {}\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	return e, root, manifest
}

// editFreshnessFixture 改 2 个文件、新增 1 个、删除 1 个，返回按路径排序的期望清单。
func editFreshnessFixture(t *testing.T, root string) []engine.PendingFile {
	t.Helper()
	writeFixture(t, root, "main.go", fixtureMainGo+"\n// PendingEdit 改动后的声明。\nfunc PendingEdit() {}\n")
	writeFixture(t, root, "util.py", fixtureUtilPy+"\n\ndef pending_edit():\n    return 2\n")
	writeFixture(t, root, "added.go", "package app\n\n// Added 新增的文件。\nfunc Added() {}\n")
	if err := os.Remove(filepath.Join(root, "lib.go")); err != nil {
		t.Fatal(err)
	}
	return []engine.PendingFile{
		{Path: "added.go", Change: "added"},
		{Path: "lib.go", Change: "deleted"},
		{Path: "main.go", Change: "modified"},
		{Path: "util.py", Change: "modified"},
	}
}

// waitBuildIdle 等到该工作区没有在建的构建。
func waitBuildIdle(t *testing.T, e *Engine, root string) {
	t.Helper()
	_, key, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		e.mu.Lock()
		_, building := e.inflight[key]
		e.mu.Unlock()
		if !building {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("构建没有在 20 秒内结束")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdAt 返回一个停住构建的钩子函数：held 在构建停住时收到一个值，release 放行。
// 清理时先放行、等该工作区的构建结束，再由 reset 清除钩子，后台构建读钩子与这里写
// 钩子不并发。
func holdAt(t *testing.T, e *Engine, root string, reset func()) (hook func(), held <-chan struct{}, release func()) {
	t.Helper()
	reached := make(chan struct{}, 16)
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	hook = func() {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-gate
	}
	t.Cleanup(func() {
		release()
		waitBuildIdle(t, e, root)
		reset()
	})
	return hook, reached, release
}

// holdBeforeEmbedding 让之后开始的构建在切分结束、进入嵌入之前停住。
func holdBeforeEmbedding(t *testing.T, e *Engine, root string) (<-chan struct{}, func()) {
	t.Helper()
	hook, held, release := holdAt(t, e, root, func() { drainParserPoolsHook = nil })
	drainParserPoolsHook = func(engine.IndexStage) { hook() }
	return held, release
}

func waitHeld(t *testing.T, held <-chan struct{}) {
	t.Helper()
	select {
	case <-held:
	case <-time.After(20 * time.Second):
		t.Fatal("构建没有在 20 秒内停到钩子处")
	}
}

// pendingOf 读状态跟踪器里记录的扫描结果。
func pendingOf(t *testing.T, e *Engine, root string) pendingScan {
	t.Helper()
	_, key, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	tracker := e.statuses[key]
	e.mu.Unlock()
	if tracker == nil {
		t.Fatal("工作区没有状态跟踪器")
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	p := tracker.pending
	p.files = append([]engine.PendingFile(nil), p.files...)
	return p
}

func rfc3339(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// TestScanRecordsPendingFiles（T5.1）：扫描完成后，状态跟踪器记下扫描时刻与相对当前
// 可服务 revision 新增、修改、删除的路径；发布成功后清单清空、改为相对新 revision。
func TestScanRecordsPendingFiles(t *testing.T) {
	ctx := context.Background()
	e, root, base := freshnessFixture(t, nil)
	want := editFreshnessFixture(t, root)
	held, release := holdBeforeEmbedding(t, e, root)
	done := make(chan error, 1)
	go func() {
		_, err := e.Sync(ctx, syncRequest(root))
		done <- err
	}()
	waitHeld(t, held)
	got := pendingOf(t, e, root)
	if !got.known || got.base != base.Revision || got.scannedAt.IsZero() || got.total != len(want) || !reflect.DeepEqual(got.files, want) {
		t.Fatalf("扫描后应记录相对 %s 的 4 个改动文件: %+v", base.Revision, got)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	published, _ := loadActiveManifest(t, e, root)
	after := pendingOf(t, e, root)
	if published.Revision == base.Revision || !after.known || after.base != published.Revision || after.total != 0 || len(after.files) != 0 {
		t.Fatalf("发布成功后清单应清空并改为相对新 revision %s: %+v", published.Revision, after)
	}
}

// TestIndexLine（T5.3）：[INDEX] 行的各情形。
func TestIndexLine(t *testing.T) {
	ctx := context.Background()

	t.Run("index-building lists files changed since the snapshot", func(t *testing.T) {
		e, root, base := freshnessFixture(t, func(o *Options) { o.QueryBuildWait = time.Second })
		want := editFreshnessFixture(t, root)
		_, release := holdBeforeEmbedding(t, e, root)
		defer release()
		res, err := e.Search(ctx, searchRequest(root, "HandleLogin"))
		if err != nil || res.DegradedReason != "index-building" {
			t.Fatalf("构建停在嵌入前时检索应以旧索引作答并标 index-building: reason=%q err=%v", res.DegradedReason, err)
		}
		lines := strings.SplitN(res.Text, "\n", 4)
		if len(lines) < 4 {
			t.Fatalf("正文行数不足: %q", res.Text)
		}
		// 横幅首行与改动前的格式逐字相同。
		if lines[0] != "[DEGRADED] index-building; mode=hybrid; semantic_coverage=100%" {
			t.Fatalf("[DEGRADED] 首行格式变了: %q", lines[0])
		}
		f := res.IndexFreshness
		if f == nil || f.ScannedAt == nil {
			t.Fatalf("结果应带 index_freshness 与扫描时刻: %+v", f)
		}
		wantLine := fmt.Sprintf("[INDEX] snapshot=%s published=%s scanned=%s pending=4: added.go, lib.go (deleted), main.go, util.py",
			base.Revision, rfc3339(base.ActivatedAt), rfc3339(*f.ScannedAt))
		if lines[1] != wantLine || lines[2] != "" {
			t.Fatalf("第二行应为 [INDEX] 行、其后空一行:\n got: %q\nwant: %q\nnext: %q", lines[1], wantLine, lines[2])
		}
		if f.SnapshotRevision != base.Revision || f.PublishedAt == nil || !f.PublishedAt.Equal(base.ActivatedAt) || !f.PendingKnown || f.PendingCount != 4 || !reflect.DeepEqual(f.PendingFiles, want) || f.ScanStartedAt != nil {
			t.Fatalf("index_freshness 字段不符: %+v", f)
		}
		if !strings.Contains(res.Text, "HandleLogin") {
			t.Fatalf("旧索引的结果应照常返回: %q", res.Text)
		}
	})

	t.Run("more than 50 changed files list 50 and the remainder", func(t *testing.T) {
		e, root, _ := freshnessFixture(t, func(o *Options) { o.QueryBuildWait = time.Second })
		for i := 0; i < 53; i++ {
			writeFixture(t, root, fmt.Sprintf("gen/file%02d.go", i), fmt.Sprintf("package gen\n\n// F%02d 生成的声明。\nfunc F%02d() {}\n", i, i))
		}
		_, release := holdBeforeEmbedding(t, e, root)
		defer release()
		res, err := e.Search(ctx, searchRequest(root, "HandleLogin"))
		if err != nil || res.IndexFreshness == nil {
			t.Fatalf("应降级作答并带 index_freshness: %+v err=%v", res.IndexFreshness, err)
		}
		f := res.IndexFreshness
		if f.PendingCount != 53 || len(f.PendingFiles) != 50 || f.PendingFiles[0].Path != "gen/file00.go" || f.PendingFiles[49].Path != "gen/file49.go" {
			t.Fatalf("超过 50 个时只列按路径排序的前 50 个: count=%d listed=%d", f.PendingCount, len(f.PendingFiles))
		}
		line := strings.SplitN(res.Text, "\n", 3)[1]
		if !strings.Contains(line, " pending=53: gen/file00.go, ") || !strings.HasSuffix(line, ", gen/file49.go (+3 more)") {
			t.Fatalf("[INDEX] 行应列 50 个再写剩余数: %q", line)
		}
	})

	t.Run("stale-index lists the files seen by the failed build", func(t *testing.T) {
		e, root, base := freshnessFixture(t, nil)
		editFreshnessFixture(t, root)
		abort := errors.New("写分段前中断（测试注入）")
		segmentWriteHook = func(context.Context, semanticOutcome) error { return abort }
		t.Cleanup(func() { segmentWriteHook = nil })
		res, err := e.Search(ctx, searchRequest(root, "HandleLogin"))
		if err != nil || res.DegradedReason != "stale-index" {
			t.Fatalf("构建失败时应以旧索引作答并标 stale-index: reason=%q err=%v", res.DegradedReason, err)
		}
		f := res.IndexFreshness
		if f == nil || !f.PendingKnown || f.PendingCount != 4 || f.SnapshotRevision != base.Revision {
			t.Fatalf("失败那次构建扫描到的文件应保留下来: %+v", f)
		}
		line := strings.SplitN(res.Text, "\n", 3)[1]
		if !strings.HasPrefix(line, "[INDEX] snapshot="+base.Revision+" ") || !strings.HasSuffix(line, " pending=4: added.go, lib.go (deleted), main.go, util.py") {
			t.Fatalf("stale-index 的 [INDEX] 行不符: %q", line)
		}
	})

	t.Run("a failed scan leaves the pending files unknown", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 不受权限位约束，无法注入扫描失败")
		}
		e, root, base := freshnessFixture(t, nil)
		editFreshnessFixture(t, root)
		abort := errors.New("写分段前中断（测试注入）")
		segmentWriteHook = func(context.Context, semanticOutcome) error { return abort }
		first, err := e.Search(ctx, searchRequest(root, "HandleLogin"))
		segmentWriteHook = nil
		if err != nil || first.IndexFreshness == nil || !first.IndexFreshness.PendingKnown {
			t.Fatalf("前一次失败的构建应留下已知清单: %+v err=%v", first.IndexFreshness, err)
		}
		writeFixture(t, root, "blocked/extra.py", "def extra():\n    return 1\n")
		blocked := filepath.Join(root, "blocked")
		if err := os.Chmod(blocked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
		res, err := e.Search(ctx, searchRequest(root, "HandleLogin"))
		if err != nil || res.DegradedReason != "stale-index" {
			t.Fatalf("扫描失败时应以旧索引作答并标 stale-index: reason=%q err=%v", res.DegradedReason, err)
		}
		wantLine := fmt.Sprintf("[INDEX] snapshot=%s published=%s pending=unknown", base.Revision, rfc3339(base.ActivatedAt))
		if line := strings.SplitN(res.Text, "\n", 3)[1]; line != wantLine {
			t.Fatalf("扫描没有完成时不应沿用上一次的清单:\n got: %q\nwant: %q", line, wantLine)
		}
		if f := res.IndexFreshness; f == nil || f.PendingKnown || f.ScannedAt != nil || f.PendingCount != 0 || len(f.PendingFiles) != 0 {
			t.Fatalf("index_freshness 应写明清单未知: %+v", f)
		}
	})

	t.Run("index-refreshing before the background scan finishes", func(t *testing.T) {
		const dim = 8
		server := newEmbedServer(t, dim)
		t.Cleanup(server.ts.Close)
		opts := embedOptions(server.ts.URL, dim, 8, "freshness-model")
		t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
		t.Setenv("OPENACE_CACHE_NAMESPACE", "freshness")
		root := newFixtureWorkspace(t)
		e1, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e1.Sync(ctx, syncRequest(root)); err != nil {
			t.Fatal(err)
		}
		base, _ := loadActiveManifest(t, e1, root)
		if err := e1.Close(ctx); err != nil {
			t.Fatal(err)
		}
		e2 := restartedEngine(t, opts)
		// 没有 stat 快照时重启后的第一次检索走首触快路径（index-refreshing）。
		_, key, err := e2.resolveRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		store, err := e2.storeFor(key)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(store.Root(), workspace.StatSnapshotFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		hook, held, release := holdAt(t, e2, root, func() { scanStartedHook = nil })
		scanStartedHook = hook
		prefix := fmt.Sprintf("[INDEX] snapshot=%s published=%s ", base.Revision, rfc3339(base.ActivatedAt))

		first, err := e2.Search(ctx, searchRequest(root, "parse_config"))
		if err != nil || first.DegradedReason != "index-refreshing" {
			t.Fatalf("首触应以磁盘上的 revision 作答并标 index-refreshing: reason=%q err=%v", first.DegradedReason, err)
		}
		if line := strings.SplitN(first.Text, "\n", 3)[1]; !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " pending=unknown") {
			t.Fatalf("后台扫描完成前清单未知: %q", line)
		}
		waitHeld(t, held)
		second, err := e2.Search(ctx, searchRequest(root, "parse_config"))
		if err != nil || second.DegradedReason != "index-refreshing" || second.IndexFreshness == nil || second.IndexFreshness.ScanStartedAt == nil {
			t.Fatalf("扫描进行中时应写出扫描开始时刻: %+v err=%v", second.IndexFreshness, err)
		}
		wantLine := prefix + "scan_started=" + rfc3339(*second.IndexFreshness.ScanStartedAt) + " pending=unknown"
		if line := strings.SplitN(second.Text, "\n", 3)[1]; line != wantLine {
			t.Fatalf("扫描进行中的 [INDEX] 行不符:\n got: %q\nwant: %q", line, wantLine)
		}
		release()
		waitFirstTouchConverged(t, e2, root)
		clean, err := e2.Search(ctx, searchRequest(root, "parse_config"))
		if err != nil || clean.DegradedReason != "" || clean.IndexFreshness != nil || strings.Contains(clean.Text, "[INDEX]") {
			t.Fatalf("刷新完成后不应再有 [INDEX] 行: reason=%q freshness=%+v err=%v", clean.DegradedReason, clean.IndexFreshness, err)
		}
	})

	t.Run("no [INDEX] line without a freshness reason", func(t *testing.T) {
		// rerank 未配置（不是显式 off）时结果标 rerank-unconfigured，索引本身是最新的。
		e, root, _ := freshnessFixture(t, func(o *Options) { o.Rerank = rerank.Config{} })
		res, err := e.Search(ctx, searchRequest(root, "HandleLogin"))
		if err != nil || !strings.HasPrefix(res.DegradedReason, "rerank-unconfigured") {
			t.Fatalf("应只标 rerank-unconfigured: reason=%q err=%v", res.DegradedReason, err)
		}
		if res.IndexFreshness != nil || strings.Contains(res.Text, "[INDEX]") {
			t.Fatalf("与索引新鲜度无关的降级不应带 [INDEX] 行: %q", strings.SplitN(res.Text, "\n", 3)[:2])
		}
		writeFixture(t, root, "main.go", fixtureMainGo+"\n// Fresh 改动。\nfunc Fresh() {}\n")
		fresh, err := e.Search(ctx, searchRequest(root, "Fresh"))
		if err != nil || strings.Contains(fresh.DegradedReason, "index-") || fresh.IndexFreshness != nil || strings.Contains(fresh.Text, "[INDEX]") {
			t.Fatalf("等到构建完成的检索不应带 [INDEX] 行: reason=%q err=%v", fresh.DegradedReason, err)
		}
	})
}

// TestIndexLineFormat：[INDEX] 行的文本格式。
func TestIndexLineFormat(t *testing.T) {
	published := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	scanned := published.Add(5 * time.Minute)
	started := published.Add(7 * time.Minute)
	many := make([]engine.PendingFile, 0, 50)
	for i := 0; i < 50; i++ {
		many = append(many, engine.PendingFile{Path: fmt.Sprintf("f%02d.go", i), Change: "added"})
	}
	cases := []struct {
		name string
		in   engine.IndexFreshness
		want string
	}{
		{"known list", engine.IndexFreshness{SnapshotRevision: "rev-a", PublishedAt: &published, ScannedAt: &scanned, PendingKnown: true, PendingCount: 3,
			PendingFiles: []engine.PendingFile{{Path: "a.go", Change: "modified"}, {Path: "b.md", Change: "deleted"}, {Path: "c.py", Change: "added"}}},
			"[INDEX] snapshot=rev-a published=2026-11-05T01:02:03Z scanned=2026-11-05T01:07:03Z pending=3: a.go, b.md (deleted), c.py"},
		{"nothing pending", engine.IndexFreshness{SnapshotRevision: "rev-a", PublishedAt: &published, ScannedAt: &scanned, PendingKnown: true},
			"[INDEX] snapshot=rev-a published=2026-11-05T01:02:03Z scanned=2026-11-05T01:07:03Z pending=0"},
		{"capped list", engine.IndexFreshness{SnapshotRevision: "rev-a", PublishedAt: &published, ScannedAt: &scanned, PendingKnown: true, PendingCount: 1234, PendingFiles: many},
			"[INDEX] snapshot=rev-a published=2026-11-05T01:02:03Z scanned=2026-11-05T01:07:03Z pending=1234: f00.go, " + "f01.go, f02.go, f03.go, f04.go, f05.go, f06.go, f07.go, f08.go, f09.go, f10.go, f11.go, f12.go, f13.go, f14.go, f15.go, f16.go, f17.go, f18.go, f19.go, f20.go, f21.go, f22.go, f23.go, f24.go, f25.go, f26.go, f27.go, f28.go, f29.go, f30.go, f31.go, f32.go, f33.go, f34.go, f35.go, f36.go, f37.go, f38.go, f39.go, f40.go, f41.go, f42.go, f43.go, f44.go, f45.go, f46.go, f47.go, f48.go, f49.go (+1184 more)"},
		{"scan in progress", engine.IndexFreshness{SnapshotRevision: "rev-a", PublishedAt: &published, ScanStartedAt: &started},
			"[INDEX] snapshot=rev-a published=2026-11-05T01:02:03Z scan_started=2026-11-05T01:09:03Z pending=unknown"},
		{"no scan yet", engine.IndexFreshness{SnapshotRevision: "rev-a", PublishedAt: &published},
			"[INDEX] snapshot=rev-a published=2026-11-05T01:02:03Z pending=unknown"},
		{"manifest without a publish time", engine.IndexFreshness{SnapshotRevision: "rev-old"},
			"[INDEX] snapshot=rev-old pending=unknown"},
	}
	for _, tc := range cases {
		if got := indexLine(&tc.in); got != tc.want {
			t.Fatalf("%s:\n got: %q\nwant: %q", tc.name, got, tc.want)
		}
	}
}
