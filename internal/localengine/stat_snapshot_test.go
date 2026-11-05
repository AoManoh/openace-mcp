package localengine

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// backdateWorkspace 把工作区里全部文件的修改时间拨回一小时前,再等 ctime 离开 racy 窗口:
// 新写的文件 ctime 是当前时刻,窗口内的哈希结果不进 stat 缓存,快照里也就没有它们。
func backdateWorkspace(t *testing.T, root string) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(p, past, past)
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
}

func statSnapshotPath(t *testing.T, e *Engine, root string) string {
	t.Helper()
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(store.Root(), workspace.StatSnapshotFile)
}

// TestStatSnapshotFirstQuery 引擎关闭时写出 stat 快照;重启后快照加载成功的工作区,第一次
// 检索走普通流程(扫描命中快照、没有改动就直接作答),不带 index-refreshing,revision 与结果
// 不变。快照里某条的内容哈希与 active manifest 不一致时只丢这一条;没有快照时保持首触快路径。
func TestStatSnapshotFirstQuery(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "stat-snapshot")
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "statsnap")
	root := newFixtureWorkspace(t)
	backdateWorkspace(t, root)
	ref := engine.WorkspaceRef{DirectoryPath: root}

	e1, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	first, err := e1.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	path := statSnapshotPath(t, e1, root)
	if err := e1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("引擎关闭时应写出 stat 快照: %v", err)
	}

	t.Run("loaded", func(t *testing.T) {
		e2, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer e2.Close(context.Background())
		result, err := e2.Search(context.Background(), searchRequest(root, "parse_config"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(result.DegradedReason, "index-refreshing") {
			t.Fatalf("快照加载成功后第一次检索不应走首触快路径: %q", result.DegradedReason)
		}
		if result.IndexRevision != first.IndexRevision || len(result.Hits) == 0 || result.Hits[0].Path != first.Hits[0].Path {
			t.Fatalf("revision 与结果应不变: %s %+v", result.IndexRevision, result.Hits)
		}
		status, err := e2.WorkspaceStatus(context.Background(), ref)
		if err != nil || !strings.Contains(status.StatSnapshot, "loaded") {
			t.Fatalf("状态应写明快照已加载: %v %q", err, status.StatSnapshot)
		}
	})

	t.Run("entry hash mismatch drops only that entry", func(t *testing.T) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file map[string]any
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		entries := file["entries"].([]any)
		if len(entries) < 2 {
			t.Fatalf("快照条目太少,无法验证单条丢弃: %d", len(entries))
		}
		entries[0].(map[string]any)["blob"] = "not-the-manifest-hash"
		data, _ := json.Marshal(file)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		e3, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer e3.Close(context.Background())
		result, err := e3.Search(context.Background(), searchRequest(root, "parse_config"))
		if err != nil || strings.Contains(result.DegradedReason, "index-refreshing") {
			t.Fatalf("其余条目照常加载,第一次检索不走首触快路径: %v %q", err, result.DegradedReason)
		}
		status, err := e3.WorkspaceStatus(context.Background(), ref)
		if err != nil || !strings.Contains(status.StatSnapshot, "dropped 1") {
			t.Fatalf("状态应写明丢弃 1 条: %v %q", err, status.StatSnapshot)
		}
	})

	t.Run("platform without ctime and inode neither loads nor saves", func(t *testing.T) {
		// Windows 原生的文件元数据没有 ctime 与 inode：快照跨重启加载后，保留修改时间的
		// 等长改写会一直命中旧哈希，所以这类平台不用快照。
		statSnapshotSupported = false
		t.Cleanup(func() { statSnapshotSupported = workspace.StatSnapshotSupported })
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		e5, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		result, err := e5.Search(context.Background(), searchRequest(root, "parse_config"))
		if err != nil || !strings.Contains(result.DegradedReason, "index-refreshing") {
			t.Fatalf("不支持的平台不应加载快照,第一次检索保持首触快路径: %v %q", err, result.DegradedReason)
		}
		waitFirstTouchConverged(t, e5, root)
		status, err := e5.WorkspaceStatus(context.Background(), ref)
		if err != nil || !strings.Contains(status.StatSnapshot, "not used on this platform") {
			t.Fatalf("状态应写明本平台不用快照: %v %q", err, status.StatSnapshot)
		}
		if err := e5.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("不支持的平台不应改写快照文件: %v", err)
		}
	})

	t.Run("absent snapshot keeps first-touch fast path", func(t *testing.T) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		e4, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer e4.Close(context.Background())
		result, err := e4.Search(context.Background(), searchRequest(root, "parse_config"))
		if err != nil || !strings.Contains(result.DegradedReason, "index-refreshing") {
			t.Fatalf("没有快照时应保持首触快路径: %v %q", err, result.DegradedReason)
		}
	})
}
