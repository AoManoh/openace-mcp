package localengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 候选规则：两路召回的所需数量指存活且不重复的候选。已删除文件在旧段里的
// 记录、多次增量后同一片段在多个段里的副本，都不能占用名额。

// TestLexicalRouteSkipsDeletedRecords：25 个强匹配文件被删除后仍留在旧段里，
// 纯词法默认查询（取 20 个）仍要找到仍然存在的目标。
func TestLexicalRouteSkipsDeletedRecords(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		writeFixture(t, root, fmt.Sprintf("removed/f%03d.go", i), "package fixture\nfunc LostNeedle() {}\n")
	}
	for i := 0; i < 60; i++ {
		writeFixture(t, root, fmt.Sprintf("other/f%03d.txt", i), fmt.Sprintf("Unrelated background material %d.\n", i))
	}
	writeFixture(t, root, "keep.go", "package fixture\n// KeptHandler supports LostNeedle compatibility.\nfunc KeptHandler() {}\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "removed")); err != nil {
		t.Fatal(err)
	}
	synced, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Search(context.Background(), searchRequest(root, "LostNeedle"))
	if err != nil {
		t.Fatal(err)
	}
	if synced.BuildMode != "delta" || len(res.Hits) != 1 || res.Hits[0].Path != "keep.go" {
		t.Fatalf("已删除记录不得占用名额，应命中唯一存活的 keep.go: build=%s hits=%+v", synced.BuildMode, res.Hits)
	}
}

// TestLexicalRoutePrefixSkipsDeletedRecords：路径前缀之内的已删除记录同样不能
// 占用名额。
func TestLexicalRoutePrefixSkipsDeletedRecords(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		writeFixture(t, root, fmt.Sprintf("pkg/removed/f%03d.go", i), "package fixture\n// PrefixNeedle PrefixNeedle\nfunc Old() {}\n")
	}
	writeFixture(t, root, "pkg/keep.go", "package fixture\n// PrefixNeedle appears once here with more words.\nfunc Keep() {}\n")
	writeFixture(t, root, "elsewhere.go", "package fixture\n// PrefixNeedle PrefixNeedle PrefixNeedle\nfunc Elsewhere() {}\n")
	// 背景文件让删除后的垃圾比例低于合并阈值，旧段与其中的已删除记录保留。
	for i := 0; i < 60; i++ {
		writeFixture(t, root, fmt.Sprintf("other/f%03d.txt", i), fmt.Sprintf("Unrelated background material %d.\n", i))
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "pkg", "removed")); err != nil {
		t.Fatal(err)
	}
	if synced, err := e.Sync(context.Background(), syncRequest(root)); err != nil || synced.BuildMode != "delta" {
		t.Fatalf("前置：删除应走 delta、保留旧段: %+v err=%v", synced, err)
	}
	req := searchRequest(root, "PrefixNeedle")
	req.PathPrefix = "pkg"
	res, err := e.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Path != "pkg/keep.go" {
		t.Fatalf("前缀内只应命中存活的 pkg/keep.go: %+v", res.Hits)
	}
}

// TestStructuralQueryFallsBackWhenOnlyDeletedRecordsMatch：查询规划只用结构词
// 检索时，结构词只命中已删除记录，按存活结果判断为零命中，回到原查询。
func TestStructuralQueryFallsBackWhenOnlyDeletedRecordsMatch(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		writeFixture(t, root, fmt.Sprintf("removed/f%03d.txt", i), "legacy_wiring legacy_wiring legacy_wiring\n")
	}
	writeFixture(t, root, "current.txt", "Wiring moved: registration now lives in the bootstrap file.\n")
	for i := 0; i < 60; i++ {
		writeFixture(t, root, fmt.Sprintf("other/f%03d.txt", i), fmt.Sprintf("Unrelated background material %d.\n", i))
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "removed")); err != nil {
		t.Fatal(err)
	}
	if synced, err := e.Sync(context.Background(), syncRequest(root)); err != nil || synced.BuildMode != "delta" {
		t.Fatalf("前置：删除应走 delta、保留旧段: %+v err=%v", synced, err)
	}
	res, err := e.Search(context.Background(), searchRequest(root, "where is legacy_wiring registration now"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.QueryPlan, "fallback=original") || len(res.Hits) == 0 || res.Hits[0].Path != "current.txt" {
		t.Fatalf("结构词只命中已删除记录时应回到原查询并找到 current.txt: plan=%q hits=%+v", res.QueryPlan, res.Hits)
	}
}

// TestDenseRouteSkipsDeletedRows：受控向量让已删除文件的旧行与查询最接近，
// 每段前 60 名都会被这些行占满；向量选择阶段必须先排除它们。
func TestDenseRouteSkipsDeletedRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		items := make([]map[string]any, 0, len(req.Input))
		for i, input := range req.Input {
			v := []float32{0, 1}
			switch {
			case input == "densequery" || strings.Contains(input, "removed/"):
				v = []float32{1, 0}
			case strings.Contains(input, "keep.txt"):
				v = []float32{0.8, 0.6}
			}
			items = append(items, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer server.Close()
	e := newTestEngineWith(t, embedOptions(server.URL, 2, 128, "controlled-model"))
	root := t.TempDir()
	for i := 0; i < 65; i++ {
		writeFixture(t, root, fmt.Sprintf("removed/f%03d.txt", i), "Historical implementation.\n")
	}
	for i := 0; i < 100; i++ {
		writeFixture(t, root, fmt.Sprintf("other/f%03d.txt", i), "Background content.\n")
	}
	writeFixture(t, root, "keep.txt", "Current implementation.\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "removed")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	res, err := e.Search(context.Background(), searchRequest(root, "densequery"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RetrievalMode != "hybrid" || len(res.Hits) == 0 || res.Hits[0].Path != "keep.txt" {
		t.Fatalf("已删除向量不得占用名额，hybrid 首位应是 keep.txt: mode=%q hits=%+v", res.RetrievalMode, res.Hits)
	}
}

// TestDenseRouteRepeatedIDsWithinOneSegment：一个文件是同一 2048 字节片段重复
// 62 次的单行，切出的 62 个字节片段内容相同、ID 相同，同一个段里保存 62 行。
// 受控向量让这些行与查询最接近。取 60 个不重复候选时，另一个文件仍要在
// dense 路里；单 worker 与 8 个 worker 结果一致。
func TestDenseRouteRepeatedIDsWithinOneSegment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		items := make([]map[string]any, 0, len(req.Input))
		for i, input := range req.Input {
			v := []float32{0.8, 0.6}
			if input == "densequery" || strings.Contains(input, "repeated.txt") {
				v = []float32{1, 0}
			}
			items = append(items, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer server.Close()
	for _, workers := range []int{1, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			old := runtime.GOMAXPROCS(workers)
			t.Cleanup(func() { runtime.GOMAXPROCS(old) })
			e := newTestEngineWith(t, embedOptions(server.URL, 2, 128, "controlled-model"))
			root := t.TempDir()
			part := "repeatedpart " + strings.Repeat("x", 2048-len("repeatedpart "))
			writeFixture(t, root, "repeated.txt", strings.Repeat(part, 62))
			writeFixture(t, root, "other.txt", "A distinct currently relevant implementation.\n")
			if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
				t.Fatal(err)
			}
			m, _ := loadActiveManifest(t, e, root)
			routes, err := e.SearchRoutes(context.Background(), searchRequest(root, "densequery"), 60)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			seen := map[string]bool{}
			for _, ref := range routes.Dense {
				if seen[ref.ID] {
					t.Fatalf("dense 候选含重复 ID %s", ref.ID)
				}
				seen[ref.ID] = true
				paths = append(paths, ref.RelPath)
			}
			t.Logf("segments=%d vector_rows=%d dense=%v", len(m.Segments), m.VectorCount, paths)
			if m.VectorCount != 63 || len(paths) != 2 || paths[0] != "repeated.txt" || paths[1] != "other.txt" {
				t.Fatalf("前置为 63 个向量行；dense 路应依次返回 repeated.txt 与 other.txt: rows=%d dense=%v", m.VectorCount, paths)
			}
		})
	}
}

// TestRouteCandidatesUniqueAcrossSegments：同一文件增量更新三次，未变化的声明
// 在三个段里都有同一 ID。两路导出的候选里每个 ID 只出现一次。
func TestRouteCandidatesUniqueAcrossSegments(t *testing.T) {
	s := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(s.ts.URL, 8, 16, "controlled-model"))
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFixture(t, root, fmt.Sprintf("other%02d.txt", i), fmt.Sprintf("Background content %d.\n", i))
	}
	for revision := 0; revision < 3; revision++ {
		writeFixture(t, root, "service.go", fmt.Sprintf("package fixture\nfunc StableNeedle() {}\nfunc ChangeMe() int { return %d }\n", revision))
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := loadActiveManifest(t, e, root)
	if len(m.Segments) != 3 {
		t.Fatalf("前置：应有三个段，实际 %d", len(m.Segments))
	}
	routes, err := e.SearchRoutes(context.Background(), searchRequest(root, "StableNeedle"), 60)
	if err != nil {
		t.Fatal(err)
	}
	for name, refs := range map[string][]CandidateRef{"lexical": routes.Lex, "dense": routes.Dense} {
		seen := map[string]bool{}
		for _, ref := range refs {
			if seen[ref.ID] {
				t.Fatalf("%s 路候选含重复 ID %s: %+v", name, ref.ID, refs)
			}
			seen[ref.ID] = true
		}
		if len(refs) == 0 {
			t.Fatalf("%s 路应有候选", name)
		}
		t.Logf("route=%s segments=%d returned=%d unique=%d", name, len(m.Segments), len(refs), len(seen))
	}
}
