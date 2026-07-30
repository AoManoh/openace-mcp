package localengine

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// embedServer 是确定性 fake embedding provider。
type embedServer struct {
	ts  *httptest.Server
	dim int

	mu      sync.Mutex
	calls   int
	perCall [][]string
	// failWhen 非 nil 时对满足条件的请求返回 503。
	failWhen func(texts []string) bool
	// zeroWhen 非 nil 时对满足条件的文本返回零向量（K35 注入）。
	zeroWhen func(text string) bool
}

// fakeVector 依据文本生成确定性向量（未归一化，引擎侧负责 Normalize）。
func fakeVector(dim int, text string) []float32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	seed := h.Sum32()
	v := make([]float32, dim)
	for i := range v {
		seed = seed*1664525 + 1013904223
		v[i] = float32(seed%1000)/1000 + 0.001
	}
	return v
}

func newEmbedServer(t *testing.T, dim int) *embedServer {
	t.Helper()
	server := &embedServer{dim: dim}
	server.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		server.mu.Lock()
		server.calls++
		server.perCall = append(server.perCall, req.Input)
		fail := server.failWhen != nil && server.failWhen(req.Input)
		zero := server.zeroWhen
		server.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			vec := fakeVector(dim, text)
			if zero != nil && zero(text) {
				vec = make([]float32, dim)
			}
			items = append(items, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(server.ts.Close)
	return server
}

func (s *embedServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// textsSince 返回自第 fromCall 次调用起送审的全部文本。
func (s *embedServer) textsSince(fromCall int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i := fromCall; i < len(s.perCall); i++ {
		out = append(out, s.perCall[i]...)
	}
	return out
}

func (s *embedServer) setFailWhen(fn func([]string) bool) {
	s.mu.Lock()
	s.failWhen = fn
	s.mu.Unlock()
}

// embedOptions 构造指向 fake server 的语义配置（openai 形状、keyless）。
func embedOptions(url string, dim int, batch int, model string) Options {
	return Options{Embedding: embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderOpenAI, BaseURL: url,
		Model: model, Dimension: dim, BatchSize: batch, MaxConcurrency: 2,
		Timeout: 2 * time.Second, MaxRetries: 0,
	}}
}

// loadActiveManifest 读取工作区当前 active manifest。
func loadActiveManifest(t *testing.T, e *Engine, root string) (*index.Manifest, *index.Store) {
	t.Helper()
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := store.ResolveUsable()
	if err != nil {
		t.Fatal(err)
	}
	return manifest, store
}

// TestSyncEmbedsAllChunks 是 P3-T06 主链路：全量嵌入、manifest 字段、
// 批大小约束（K22 结构校验由 client 层保证）。
func TestSyncEmbedsAllChunks(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 2, "fake-model"))
	root := newFixtureWorkspace(t)

	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	manifest, store := loadActiveManifest(t, e, root)
	if manifest.VectorCount != manifest.Counts.Chunks || manifest.VectorCount == 0 {
		t.Fatalf("应全量覆盖: vectors=%d chunks=%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	if manifest.EmbeddingProvider != "openai" || manifest.EmbeddingModel != "fake-model" ||
		manifest.EmbeddingDimension != dim || manifest.EmbeddingDtype != "float32" ||
		manifest.EmbeddingProfileHash == "" || manifest.VectorsChecksum == "" || manifest.VectorsIndexChecksum == "" {
		t.Fatalf("manifest 语义字段不完整: %+v", manifest)
	}
	if manifest.EngineVersion != "stage3" {
		t.Fatalf("EngineVersion 应为 stage3: %q", manifest.EngineVersion)
	}
	// 向量文件可按 manifest 校验载入（K24/K25 链路）。
	ix, err := vector.Load(store.SegmentPath(manifest), dim, manifest.VectorsChecksum, manifest.VectorsIndexChecksum, 0)
	if err != nil || ix.Count() != manifest.VectorCount {
		t.Fatalf("向量载入: count=%v err=%v", ix, err)
	}
	// 批大小约束。
	for i, texts := range server.perCall {
		if len(texts) > 2 {
			t.Fatalf("第 %d 批超过 BatchSize=2: %d", i, len(texts))
		}
	}
	if result.IndexRevision != manifest.Revision {
		t.Fatalf("结果 revision 不符")
	}
}

// TestIncrementalEmbedOnlyChangedContent 是 D4/K5 费用承诺：
// 未变更内容永不重复付费。
func TestIncrementalEmbedOnlyChangedContent(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := server.callCount()

	writeFixture(t, root, "util.py", fixtureUtilPy+"\ndef reload_config():\n    return parse_config('/etc/app.conf')\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	newTexts := server.textsSince(callsAfterFirst)
	if len(newTexts) == 0 {
		t.Fatalf("变更文件应触发新嵌入")
	}
	for _, text := range newTexts {
		if strings.Contains(text, "HandleLogin") || strings.Contains(text, "Demo App") {
			t.Fatalf("未变更内容被重复嵌入（违反 D4/K5）: %q", text)
		}
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("增量后应保持全覆盖: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
}

// TestVectorReuseBitExact 是 D2 的 bit 级复用断言。
func TestVectorReuseBitExact(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	firstManifest, store := loadActiveManifest(t, e, root)
	firstIx, err := vector.Load(store.SegmentPath(firstManifest), dim, firstManifest.VectorsChecksum, firstManifest.VectorsIndexChecksum, 0)
	if err != nil {
		t.Fatal(err)
	}
	firstByHash := map[string][]float32{}
	for i, entry := range firstIx.Entries() {
		firstByHash[entry.ContentHash] = firstIx.Row(i)
	}

	writeFixture(t, root, "README.md", fixtureReadme+"\nExtended documentation line.\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	secondManifest, _ := loadActiveManifest(t, e, root)
	secondIx, err := vector.Load(store.SegmentPath(secondManifest), dim, secondManifest.VectorsChecksum, secondManifest.VectorsIndexChecksum, 0)
	if err != nil {
		t.Fatal(err)
	}
	common := 0
	for i, entry := range secondIx.Entries() {
		if old, ok := firstByHash[entry.ContentHash]; ok {
			common++
			if !reflect.DeepEqual(old, secondIx.Row(i)) {
				t.Fatalf("复用向量应 bit 级一致（D2）: hash=%s", entry.ContentHash)
			}
		}
	}
	if common == 0 {
		t.Fatalf("应存在跨 revision 复用行")
	}
}

// TestPartialEmbeddingFailurePublishesDegradedThenHeals 是 §10.2 + D10：
// provider 部分失败不阻塞发布；恢复后 sync 只补缺口并发布新 revision。
func TestPartialEmbeddingFailurePublishesDegradedThenHeals(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	// util.py 的 chunk 失败（batch=1 隔离到单请求）。
	server.setFailWhen(func(texts []string) bool {
		return len(texts) == 1 && strings.Contains(texts[0], "parse_config")
	})
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 1, "fake-model"))
	root := newFixtureWorkspace(t)

	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("部分嵌入失败不得阻塞发布（§10.2）: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if manifest.SemanticComplete() || manifest.VectorCount == 0 {
		t.Fatalf("应为部分覆盖: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}

	// 恢复 provider；新引擎（fresh circuit）执行补齐。
	server.setFailWhen(nil)
	e2, err := New(embedOptions(server.ts.URL, dim, 1, "fake-model"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e2.Close(context.Background()) })
	callsBefore := server.callCount()
	second, err := e2.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if second.IndexRevision == first.IndexRevision {
		t.Fatalf("补齐应发布新 revision（D10 improved）")
	}
	healed, _ := loadActiveManifest(t, e2, root)
	if !healed.SemanticComplete() {
		t.Fatalf("补齐后应全覆盖: %d/%d", healed.VectorCount, healed.Counts.Chunks)
	}
	healTexts := server.textsSince(callsBefore)
	for _, text := range healTexts {
		if !strings.Contains(text, "parse_config") {
			t.Fatalf("补齐只应嵌入缺失内容: %q", text)
		}
	}
	// 全覆盖后再次 sync：no-op，不再出网（D10）。
	callsBefore = server.callCount()
	third, err := e2.Sync(context.Background(), syncRequest(root))
	if err != nil || third.IndexRevision != second.IndexRevision {
		t.Fatalf("全覆盖后应 no-op: %+v err=%v", third, err)
	}
	if server.callCount() != callsBefore {
		t.Fatalf("no-op 不得出网")
	}
}

// TestBackoffPreventsRebuildStorm 是暗坑 K30。
func TestBackoffPreventsRebuildStorm(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	server.setFailWhen(func([]string) bool { return true })
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)

	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("provider 全故障时词法仍须发布（§10.2）: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if manifest.VectorCount != 0 || !manifest.HasVectors() {
		t.Fatalf("应发布零覆盖 revision: %+v", manifest.VectorCount)
	}
	callsAfterFirst := server.callCount()
	if callsAfterFirst == 0 {
		t.Fatalf("首次构建应尝试过 provider")
	}
	for i := 0; i < 5; i++ {
		result, err := e.Sync(context.Background(), syncRequest(root))
		if err != nil || result.IndexRevision != first.IndexRevision {
			t.Fatalf("退避期 sync 应 no-op: %+v err=%v", result, err)
		}
	}
	if server.callCount() != callsAfterFirst {
		t.Fatalf("退避期不得出网（K30）: %d → %d", callsAfterFirst, server.callCount())
	}
	// WorkspaceChanged 在退避期如实返回未变化（watcher 不空转）。
	changed, err := e.WorkspaceChanged(context.Background(), engineRef(root))
	if err != nil || changed {
		t.Fatalf("退避期 WorkspaceChanged 应为 false: %v %v", changed, err)
	}
}

// TestWorkspaceChangedReportsSemanticGap 是 WorkspaceChanged 语义缺口扩展。
func TestWorkspaceChangedReportsSemanticGap(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	server.setFailWhen(func([]string) bool { return true })
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	// fresh circuit（healthy）+ 覆盖缺口 → 需要同步。
	server.setFailWhen(nil)
	e2, err := New(embedOptions(server.ts.URL, dim, 16, "fake-model"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e2.Close(context.Background()) })
	changed, err := e2.WorkspaceChanged(context.Background(), engineRef(root))
	if err != nil || !changed {
		t.Fatalf("覆盖缺口 + circuit 健康应报需同步: %v %v", changed, err)
	}
}

// TestZeroVectorRejected 是暗坑 K35。
func TestZeroVectorRejected(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	server.zeroWhen = func(text string) bool { return strings.Contains(text, "parse_config") }
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("零向量拒绝不应阻塞发布: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if manifest.VectorCount != manifest.Counts.Chunks-1 {
		t.Fatalf("零向量 chunk 应记未覆盖: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
}

// TestZeroVectorNotRetriedAcrossSyncs 是 K35 修订（自审新增）：
// 已知零向量内容进程内不再送 provider，防 watcher 周期重复付费。
func TestZeroVectorNotRetriedAcrossSyncs(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	server.zeroWhen = func(text string) bool { return strings.Contains(text, "parse_config") }
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := server.callCount()
	for i := 0; i < 3; i++ {
		result, err := e.Sync(context.Background(), syncRequest(root))
		if err != nil || result.IndexRevision != first.IndexRevision {
			t.Fatalf("拒绝内容不构成改善，不应发布新 revision: %+v err=%v", result, err)
		}
	}
	if server.callCount() != callsAfterFirst {
		t.Fatalf("已知零向量内容不得重复送 provider（K35 修订）: %d → %d", callsAfterFirst, server.callCount())
	}
	// 状态仍如实反映拒绝计数。
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil || status.Semantic == nil || status.Semantic.RejectedChunks != 1 {
		t.Fatalf("拒绝计数应保持可见: %+v err=%v", status.Semantic, err)
	}
}

// TestEmbeddingOffManifestUnchanged 是 K32：semantic off 时 manifest
// 形状与 Stage 2 逐字节同构、无向量文件、无出网面。
func TestEmbeddingOffManifestUnchanged(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifest, store := loadActiveManifest(t, e, root)
	raw, err := os.ReadFile(filepath.Join(store.Root(), "manifests", manifest.Revision+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"embedding_", "vectors_", "vector_count"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("semantic off 的 manifest 不得含 %q", forbidden)
		}
	}
	segment := store.SegmentPath(manifest)
	for _, name := range []string{vector.DataFileName, vector.IndexFileName} {
		if _, err := os.Stat(filepath.Join(segment, name)); !os.IsNotExist(err) {
			t.Fatalf("semantic off 不得写向量文件: %s", name)
		}
	}
	if filepath.Base(store.Root()) != e.profile.ID+"-v"+e.profile.Version || strings.Contains(store.Root(), "+emb-") {
		t.Fatalf("semantic off 的子树路径应与 Stage 2 一致: %s", store.Root())
	}
}

// TestProfileSubtreeIsolation 是 D4/K24：模型变化写入平行子树。
func TestProfileSubtreeIsolation(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)

	engineA, err := New(embedOptions(server.ts.URL, dim, 16, "model-a"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engineA.Close(context.Background()) })
	if _, err := engineA.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifestA, storeA := loadActiveManifest(t, engineA, root)

	engineB, err := New(embedOptions(server.ts.URL, dim, 16, "model-b"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engineB.Close(context.Background()) })
	if _, err := engineB.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifestB, storeB := loadActiveManifest(t, engineB, root)

	if storeA.Root() == storeB.Root() {
		t.Fatalf("不同模型必须写入平行子树（K24）: %s", storeA.Root())
	}
	if manifestA.EmbeddingProfileHash == manifestB.EmbeddingProfileHash {
		t.Fatalf("profile hash 应随模型变化")
	}
	// A 子树不受 B 影响，仍可校验载入。
	if _, err := vector.Load(storeA.SegmentPath(manifestA), dim, manifestA.VectorsChecksum, manifestA.VectorsIndexChecksum, 0); err != nil {
		t.Fatalf("A 子树应保持完整: %v", err)
	}
}

// TestManifestBackwardCompat 是暗坑 K34：Stage 2 形状的 manifest 可被
// 新代码读取，语义字段取零值。
func TestManifestBackwardCompat(t *testing.T) {
	stage2JSON := `{"schema_version":1,"workspace":{"canonical_path":"/w"},"engine_id":"local-hybrid","engine_version":"stage2","revision":"rev-1","chunker_id":"chunk-profile","chunker_version":"1","lexical_engine":"bleve","lexical_version":"v2.5.7","segment_id":"seg-1","files":{},"counts":{"files":2,"chunks":5,"bytes":100},"chunks_checksum":"abc","created_at":"2026-07-29T00:00:00Z","activated_at":"2026-07-29T00:00:00Z"}`
	var manifest index.Manifest
	if err := json.Unmarshal([]byte(stage2JSON), &manifest); err != nil {
		t.Fatalf("Stage 2 manifest 应可读: %v", err)
	}
	if manifest.HasVectors() || manifest.VectorCount != 0 || manifest.EmbeddingProvider != "" {
		t.Fatalf("语义字段应为零值: %+v", manifest)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func engineRef(dir string) engine.WorkspaceRef {
	return engine.WorkspaceRef{DirectoryPath: dir}
}
