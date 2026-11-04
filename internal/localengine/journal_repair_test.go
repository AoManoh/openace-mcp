package localengine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestJournalMidFileDamageAfterRestartReembedsOnlyDamagedKey 是 K40 改规则的引擎用例：
// 构建在写分段前中断、journal 留下本次新取的全部向量；进程重启前 journal 中间一条
// 记录坏掉。新进程打开 journal 时只跳过这一条：共享向量库也没有同键记录时只为它送审
// 1 条，有同键记录时 0 条；最终发布完整覆盖，workspace_status 与日志写明跳过的数据。
// 改动前打开时在坏处截断，其后的已付费记录全部重新送审。
func TestJournalMidFileDamageAfterRestartReembedsOnlyDamagedKey(t *testing.T) {
	cases := []struct {
		name         string
		removeShared bool
		wantSent     int
	}{
		{"共享向量库没有同键记录", true, 1},
		{"共享向量库有同键记录", false, 0},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const dim = 8
			ctx := context.Background()
			// 两个引擎实例共用同一个隔离的 cache 目录；不设置时会写进本机真实缓存。
			t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
			t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
			server := newEmbedServer(t, dim)
			opts := embedOptions(server.ts.URL, dim, 4, fmt.Sprintf("k40-repair-%d", i))
			root := refsWorkspace(t, 20)

			first, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			abort := errors.New("模拟写分段前失败")
			segmentWriteHook = func(context.Context, semanticOutcome) error { return abort }
			_, syncErr := first.Sync(ctx, syncRequest(root))
			segmentWriteHook = nil
			if syncErr == nil || !strings.Contains(syncErr.Error(), abort.Error()) {
				t.Fatalf("第一次同步应在写分段前失败: %v", syncErr)
			}
			_, workspaceKey, err := first.resolveRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			store, err := first.storeFor(workspaceKey)
			if err != nil {
				t.Fatal(err)
			}
			sharedDir, err := first.sharedVectorsDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Close(ctx); err != nil {
				t.Fatal(err)
			}

			// 生产的键是 64 位十六进制，记录等长；改坏第 2 条记录的向量字节。
			journalPath := filepath.Join(store.JournalDir(), "vectors.journal")
			data, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			recordLen := 4 + 2 + 64 + 4 + 4*dim + 4
			if len(data) < 3*recordLen || len(data)%recordLen != 0 {
				t.Fatalf("journal 应有至少 3 条等长记录: %d 字节", len(data))
			}
			data[recordLen+4+2+64+4+1] ^= 0xFF
			if err := os.WriteFile(journalPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if hashLen := int(binary.LittleEndian.Uint16(data[recordLen+4:])); hashLen != 64 {
				t.Fatalf("第 2 条记录的键长应为 64: %d", hashLen)
			}
			if tc.removeShared {
				if err := os.RemoveAll(sharedDir); err != nil {
					t.Fatal(err)
				}
			}

			var logMu sync.Mutex
			var logged []string
			opts.Logf = func(format string, args ...any) {
				logMu.Lock()
				defer logMu.Unlock()
				logged = append(logged, fmt.Sprintf(format, args...))
			}
			callsBefore := server.callCount()
			restarted, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close(ctx) })
			if _, err := restarted.Sync(ctx, syncRequest(root)); err != nil {
				t.Fatalf("重启后的同步: %v", err)
			}
			if sent := server.textsSince(callsBefore); len(sent) != tc.wantSent {
				t.Fatalf("应只为坏掉的那一条送审 %d 条，实际 %d 条", tc.wantSent, len(sent))
			}
			manifest, _ := loadActiveManifest(t, restarted, root)
			if !manifest.SemanticComplete() {
				t.Fatalf("应发布完整覆盖: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
			}
			status, err := restarted.WorkspaceStatus(ctx, engineRef(root))
			if err != nil {
				t.Fatal(err)
			}
			if status.Semantic == nil || !strings.Contains(status.Semantic.JournalRepair, "跳过 1 处") {
				t.Fatalf("workspace_status 应写明跳过的数据: %+v", status.Semantic)
			}
			logMu.Lock()
			defer logMu.Unlock()
			if len(logged) != 1 || !strings.Contains(logged[0], journalPath[:len(journalPath)-len("/vectors.journal")]) || !strings.Contains(logged[0], "跳过 1 处") {
				t.Fatalf("日志应有一行写明 journal 目录与跳过的数据: %q", logged)
			}
		})
	}
}
