package localengine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

func writeHandleVectorSegment(t *testing.T, count, dim int, prefix string) (string, index.SegmentRef) {
	t.Helper()
	dir := t.TempDir()
	entries := make([]vector.Entry, count)
	vectors := make([][]float32, count)
	for i := 0; i < count; i++ {
		entries[i] = vector.Entry{ID: fmt.Sprintf("%s-%d", prefix, i), ContentHash: fmt.Sprintf("key-%s-%d", prefix, i)}
		row := make([]float32, dim)
		row[i%dim] = 1
		vectors[i] = row
	}
	dataSum, indexSum, err := vector.Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	return dir, index.SegmentRef{
		ID: prefix, VectorsChecksum: dataSum, VectorsIndexChecksum: indexSum,
		VectorCount: count, Counts: index.Counts{Chunks: count},
	}
}

// 累计 envelope 必须在第二段完整数据分配前拒绝,并释放首段缓存引用。
func TestRevisionHandleChecksCumulativeVectorEnvelopeBeforeLoad(t *testing.T) {
	const dim = 4
	dir1, seg1 := writeHandleVectorSegment(t, 6, dim, "one")
	dir2, seg2 := writeHandleVectorSegment(t, 6, dim, "two")
	e := &Engine{vectorSegments: make(map[string]*sharedVectorIndex), vectorMaxResident: 10}
	h := &revisionHandle{
		engine:      e,
		manifest:    &index.Manifest{VectorCount: 12, Segments: []index.SegmentRef{seg1, seg2}},
		segmentDirs: []string{dir1, dir2},
	}
	if _, err := h.vectorIndexes(dim); !errors.Is(err, vector.ErrEnvelopeExceeded) {
		t.Fatalf("累计 12>10 应在第二段分配前拒绝: %v", err)
	}
	e.vectorMu.Lock()
	cached := len(e.vectorSegments)
	e.vectorMu.Unlock()
	if cached != 0 {
		t.Fatalf("失败路径应释放已取得段引用: cached=%d", cached)
	}
}

// delta active 与 previous 共享基段时,两个 revisionHandle 应指向同一
// vector.Index；新 active 只为 delta 增加一份常驻数据。
func TestRevisionHandlesShareImmutableVectorSegments(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "same-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := e.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	previousIndexes, err := previous.vectorIndexes(dim)
	if err != nil || len(previousIndexes) != 1 {
		t.Fatalf("previous vectors: %d err=%v", len(previousIndexes), err)
	}

	writeFixture(t, root, "new.go", "package app\n\nfunc NewThing() {}\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	current, err := e.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	currentIndexes, err := current.vectorIndexes(dim)
	if err != nil || len(currentIndexes) != 2 {
		t.Fatalf("current vectors: %d err=%v", len(currentIndexes), err)
	}
	if previousIndexes[0] != currentIndexes[0] {
		t.Fatal("active/previous 共享基段应复用同一 vector.Index")
	}
	e.vectorMu.Lock()
	cached := len(e.vectorSegments)
	e.vectorMu.Unlock()
	if cached != 2 {
		t.Fatalf("应仅常驻 base+delta 两个唯一段: %d", cached)
	}
	e.releaseHandle(previous)
	e.releaseHandle(current)
	// 下一次 full/compaction 发布后旧 active 不再留作内存句柄；previous
	// 磁盘 revision 仍在，需要回退时可重新打开。
	e.retireHandles(workspaceKey, "future-revision", "")
	e.vectorMu.Lock()
	cached = len(e.vectorSegments)
	e.vectorMu.Unlock()
	if cached != 0 {
		t.Fatalf("退役闲置 active 后应释放全部段缓存: %d", cached)
	}
}
