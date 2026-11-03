package localengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// lazyTextWorkspace 建一个有 40 个小 Go 文件的工作区，每个文件一个函数；批大小
// 为 2 时全量构建要送审约 20 批。
func lazyTextWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < 40; i++ {
		body := fmt.Sprintf("package main\n\nfunc lazy%02d() int { return %d }\n", i, i)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestEmbedRecordsBuildsTextsPerBatch：送审文本在投放一批时才为这一批生成。
// 改动前 embedRecords 在第一批请求前为全部缺口生成文本，整个嵌入阶段一直留在
// 内存里（mailyn-cli 一半副本 191 MiB，第 8 步堆剖析）。
func TestEmbedRecordsBuildsTextsPerBatch(t *testing.T) {
	var generated atomic.Int64
	embedDocTextHook = func() { generated.Add(1) }
	t.Cleanup(func() { embedDocTextHook = nil })
	server := newEmbedServer(t, 8)
	var atFirstRequest atomic.Int64
	atFirstRequest.Store(-1)
	server.setFailWhen(func([]string) bool {
		atFirstRequest.CompareAndSwap(-1, generated.Load())
		return false
	})
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 2, "lazy-text"))
	root := lazyTextWorkspace(t)
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	total := generated.Load()
	first := atFirstRequest.Load()
	if total < 40 {
		t.Fatalf("expected at least 40 texts in total, got %d", total)
	}
	// 准入窗口初值 2，投放循环最多在途 3 批，再加上缓存的下一批，批大小 2。
	if first < 0 || first > 10 {
		t.Fatalf("texts should be built per batch: %d of %d were built before the first request", first, total)
	}
}

// TestEmbedRecordsTextsUnchanged：按批生成后，送审文本的内容与条数和对全部缺口
// 逐条调用 embedDocText 相同。
func TestEmbedRecordsTextsUnchanged(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 2, "lazy-text"))
	root := lazyTextWorkspace(t)
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := server.textsSince(0)
	sort.Strings(got)

	manifest, store := loadActiveManifest(t, e, root)
	byFile, err := loadLiveChunkRecordsByFile(store, manifest)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	seen := map[string]bool{}
	var want []string
	for _, records := range byFile {
		for _, record := range records {
			key := embedKey(record)
			if seen[key] {
				continue
			}
			seen[key] = true
			want = append(want, embedDocText(record))
		}
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent texts differ from embedDocText over the missing records: got %d, want %d", len(got), len(want))
	}
}
