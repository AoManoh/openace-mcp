package localengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

const priorLoadFiles = 10

// priorLoadWorkspace 建 n 个各含 3 个函数的 Python 文件。之后在每个文件末尾追加一个函数，
// 原有函数的 chunk 不变，delta 要从旧段复用它们的向量。
func priorLoadWorkspace(t *testing.T, n int) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		var body strings.Builder
		for j := 0; j < 3; j++ {
			fmt.Fprintf(&body, "def handler_%02d_%d(value):\n    total = value * %d\n    return total + %d\n\n\n", i, j, i+1, j)
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("mod%02d.py", i)), []byte(body.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func appendFunctionToAll(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		f, err := os.OpenFile(filepath.Join(root, fmt.Sprintf("mod%02d.py", i)), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(f, "def added_%02d(value):\n    return value - %d\n", i, i)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

type priorLoadRun struct {
	copies  int
	sent    int
	segment index.SegmentRef
	lastErr string
}

// runPriorLoadScenario：首建后删掉共享向量库（让旧向量只能从旧段复用），可选地把旧段第 0 行
// 改成全零，再给全部文件追加函数并同步。返回 delta 期间按行拷贝的行数、送审条数、新段与
// semantic.last_error。views 为 false 时强制按行拷贝，作为改动前行为的对照。
func runPriorLoadScenario(t *testing.T, views bool, corruptRow bool) priorLoadRun {
	t.Helper()
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "prior-load"))
	root := priorLoadWorkspace(t, priorLoadFiles)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if corruptRow {
		manifest, store := loadActiveManifest(t, e, root)
		dataPath := filepath.Join(store.SegmentPathFor(manifest.Segments[0].ID), vector.DataFileName)
		raw, err := os.ReadFile(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		for b := 0; b < dim*4; b++ {
			raw[b] = 0
		}
		if err := os.WriteFile(dataPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sharedDir, err := e.sharedVectorsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sharedDir); err != nil {
		t.Fatal(err)
	}
	savedViews := priorRowViews
	priorRowViews = views
	copies := 0
	priorRowCopyHook = func() { copies++ }
	defer func() {
		priorRowViews = savedViews
		priorRowCopyHook = nil
	}()
	appendFunctionToAll(t, root, priorLoadFiles)
	callsBefore := server.callCount()
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if result.BuildMode != "delta" {
		t.Fatalf("追加函数应走 delta: %s", result.BuildMode)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("delta 后覆盖应完整: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	return priorLoadRun{
		copies:  copies,
		sent:    len(server.textsSince(callsBefore)),
		segment: manifest.Segments[len(manifest.Segments)-1],
		lastErr: lastSemanticError(t, e, root),
	}
}

// TestDeltaPriorLoadUsesRowViews：数据文件可映射时，delta 复用的旧向量取映射视图，按行拷贝 0 行；
// 关闭视图（改动前的行为）时按行拷贝。两种方式发布的新段逐字节相同、送审条数相同，且确有
// 旧向量被复用（送审条数少于新段 chunk 数）。
func TestDeltaPriorLoadUsesRowViews(t *testing.T) {
	viewed := runPriorLoadScenario(t, true, false)
	copied := runPriorLoadScenario(t, false, false)
	if viewed.copies != 0 {
		t.Fatalf("映射可用时不应按行拷贝旧向量，实际 %d 行", viewed.copies)
	}
	if copied.copies == 0 {
		t.Fatal("关闭视图时应按行拷贝旧向量")
	}
	if viewed.sent != copied.sent {
		t.Fatalf("送审条数应相同: 视图 %d，拷贝 %d", viewed.sent, copied.sent)
	}
	if viewed.sent >= viewed.segment.Counts.Chunks {
		t.Fatalf("应有旧向量被复用: 送审 %d，新段 %d 个 chunk", viewed.sent, viewed.segment.Counts.Chunks)
	}
	if viewed.segment.ChunksChecksum != copied.segment.ChunksChecksum ||
		viewed.segment.VectorsChecksum != copied.segment.VectorsChecksum ||
		viewed.segment.VectorsIndexChecksum != copied.segment.VectorsIndexChecksum {
		t.Fatalf("两种读取方式发布的新段应逐字节相同: %+v vs %+v", viewed.segment, copied.segment)
	}
}

// TestDeltaPriorLoadReportsRejectedRows：旧段某行读出时检查不通过，这一行不复用、重新送审
// （共享向量库已删，只多 1 条），并写进 semantic.last_error；改动前这类行被静默跳过。
func TestDeltaPriorLoadReportsRejectedRows(t *testing.T) {
	control := runPriorLoadScenario(t, true, false)
	corrupt := runPriorLoadScenario(t, true, true)
	if corrupt.sent != control.sent+1 {
		t.Fatalf("检查不通过的行应重新送审 1 条: 对照 %d，损坏 %d", control.sent, corrupt.sent)
	}
	if !strings.Contains(corrupt.lastErr, "1 行") || !strings.Contains(corrupt.lastErr, "检查不通过") {
		t.Fatalf("semantic.last_error 应写明检查不通过的行数: %q", corrupt.lastErr)
	}
	if strings.Contains(control.lastErr, "检查不通过") {
		t.Fatalf("没有损坏时不应报告: %q", control.lastErr)
	}
}
