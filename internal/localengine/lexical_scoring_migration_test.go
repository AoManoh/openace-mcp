package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
)

// tfidfEraStoreProfile 返回 TF-IDF 时代（v0.6.0 及更早）二进制为同一配置
// 算出的索引子树名：那时的 profile 段没有词法评分标记。
func tfidfEraStoreProfile(current string) string {
	return strings.Replace(current, "+lex-bm25", "", 1)
}

// dropSharedVectors 删除测试 cache 里的共享向量库，模拟旧子树由还没有共享
// 向量库的旧版本建出。迁移测试要验证的是旧子树自身的已付费状态（journal、
// 拒绝集、Batch 作业）；测试里扮演旧版本的引擎会顺带写入共享库，这些记录
// 会在旧子树检查之前补上缺口，原本要验证的路径就测不到了。
func dropSharedVectors(t *testing.T, e *Engine) {
	t.Helper()
	dir, err := e.sharedVectorsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
}

// tfidfEraFingerprint 按 TF-IDF 时代的组成项（前缀 engine-profile-v3，不含
// 词法评分）重算配置指纹，即升级前 daemon 广播的 engine_profile。
func tfidfEraFingerprint(o Options) string {
	embedComponent := "off"
	if o.Embedding.Enabled {
		embedComponent = o.Embedding.ConfigIdentity()
	}
	rerankComponent := "off"
	if o.Rerank.Enabled {
		rerankComponent = o.Rerank.Identity()
	}
	strictComponent := "strict-off"
	if o.QualityStrict {
		strictComponent = "strict-on"
	}
	bulkComponent := "bulk-off"
	if o.Embedding.BatchAPIMode != "" {
		bulkComponent = "bulk-" + o.Embedding.BatchAPIMode
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"engine-profile-v3",
		embedComponent,
		rerankComponent,
		// 旧二进制还有检索降级开关；Options 已没有这一项，按它的默认值 allow 重算。
		string(DegradeAllow),
		string(normalizeDegrade(o.RerankDegrade)),
		strictComponent,
		bulkComponent,
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// tfidfIndexFields 与 lexical 包写入 Bleve 的文档形状逐字段一致（JSON 名与
// omitempty 相同），用于在测试里复刻基线二进制产出的 TF-IDF 段。
type tfidfIndexFields struct {
	Content   string `json:"content"`
	Path      string `json:"path"`
	Symbol    string `json:"symbol,omitempty"`
	PathRaw   string `json:"path_raw"`
	SymbolRaw string `json:"symbol_raw,omitempty"`
	Language  string `json:"language,omitempty"`
}

// persistedScoringModel 以只读方式打开一个 Bleve 目录，返回持久化 mapping
// 里的 scoring_model（空串即 Bleve 默认的 TF-IDF）。
func persistedScoringModel(t *testing.T, lexDir string) string {
	t.Helper()
	idx, err := bleve.OpenUsing(lexDir, map[string]interface{}{"read_only": true})
	if err != nil {
		t.Fatalf("打开 %s: %v", lexDir, err)
	}
	defer idx.Close()
	impl, ok := idx.Mapping().(*mapping.IndexMappingImpl)
	if !ok {
		t.Fatalf("持久化 mapping 类型意外: %T", idx.Mapping())
	}
	return impl.ScoringModel
}

// rewriteStoreAsTFIDF 把 store 内全部 revision 引用的 segment 的 Bleve 目录
// 改写成基线二进制的产物：mapping 与当前持久化 mapping 只差 scoring_model
// （基线未设置，即 Bleve 默认 TF-IDF），文档取自同段 chunks.jsonl。chunks
// 校验和与目录结构不变，manifest 照常通过校验。调用前须关闭持有这些段的
// 引擎。
func rewriteStoreAsTFIDF(t *testing.T, store *index.Store) {
	t.Helper()
	revisions, err := store.ListRevisions()
	if err != nil {
		t.Fatal(err)
	}
	done := map[string]bool{}
	for _, revision := range revisions {
		manifest, err := store.LoadManifest(revision)
		if err != nil {
			t.Fatal(err)
		}
		for _, segment := range manifest.Segments {
			if done[segment.ID] {
				continue
			}
			done[segment.ID] = true
			rewriteSegmentAsTFIDF(t, store.SegmentPathFor(segment.ID))
		}
	}
}

func rewriteSegmentAsTFIDF(t *testing.T, segmentDir string) {
	t.Helper()
	lexDir := filepath.Join(segmentDir, index.LexicalDirName)
	current, err := bleve.OpenUsing(lexDir, map[string]interface{}{"read_only": true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(current.Mapping())
	_ = current.Close()
	if err != nil {
		t.Fatal(err)
	}
	var legacy mapping.IndexMappingImpl
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.ScoringModel = ""
	records, err := readSegmentChunkRecords(segmentDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(lexDir); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := bleve.New(lexDir, &legacy)
	if err != nil {
		t.Fatal(err)
	}
	batch := rebuilt.NewBatch()
	for _, record := range records {
		fields := tfidfIndexFields{
			Content: record.Content, Path: record.RelPath, Symbol: record.Symbol,
			PathRaw: record.RelPath, SymbolRaw: record.Symbol, Language: record.Language,
		}
		if err := batch.Index(record.ID, fields); err != nil {
			t.Fatal(err)
		}
	}
	if err := rebuilt.Batch(batch); err != nil {
		t.Fatal(err)
	}
	if err := rebuilt.Close(); err != nil {
		t.Fatal(err)
	}
	if model := persistedScoringModel(t, lexDir); model != "" {
		t.Fatalf("复刻的基线段应为 TF-IDF（scoring_model 为空），实际 %q", model)
	}
}

// segmentLexicalDirs 返回 manifest 全部 segment 的 Bleve 目录（优先级升序）。
func segmentLexicalDirs(store *index.Store, manifest *index.Manifest) []string {
	dirs := make([]string, 0, len(manifest.Segments))
	for _, segment := range manifest.Segments {
		dirs = append(dirs, filepath.Join(store.SegmentPathFor(segment.ID), index.LexicalDirName))
	}
	return dirs
}

// assertManifestServesBM25 核对一个 revision 的全部 segment：持久化 mapping
// 为 bm25；按生产同样的多段聚合方式（Bleve IndexAlias）检索 content 字段，
// 每个命中的分数解释都走 BM25 分支；生产打开路径 lexical.OpenMulti 能打开。
func assertManifestServesBM25(t *testing.T, store *index.Store, manifest *index.Manifest, query string) {
	t.Helper()
	if manifest.LexicalEngine != "bleve" || manifest.LexicalVersion != lexical.EngineVersion+"+bm25" {
		t.Fatalf("manifest 应记录词法引擎与评分模型: engine=%q version=%q", manifest.LexicalEngine, manifest.LexicalVersion)
	}
	dirs := segmentLexicalDirs(store, manifest)
	for _, dir := range dirs {
		if model := persistedScoringModel(t, dir); model != "bm25" {
			t.Fatalf("revision %s 的段 %s 持久化 scoring_model=%q，期望 bm25", manifest.Revision, dir, model)
		}
	}
	members := make([]bleve.Index, 0, len(dirs))
	defer func() {
		for _, member := range members {
			_ = member.Close()
		}
	}()
	for _, dir := range dirs {
		member, err := bleve.OpenUsing(dir, map[string]interface{}{"read_only": true})
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
	}
	q := bleve.NewMatchQuery(query)
	q.SetField("content")
	res, err := bleve.NewIndexAlias(members...).Search(bleve.NewSearchRequestOptions(q, 50, 0, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("前置：查询 %q 在 revision %s 应有词法命中", query, manifest.Revision)
	}
	for _, hit := range res.Hits {
		raw, err := json.Marshal(hit.Expl)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if strings.Contains(text, "tf-idf") || !strings.Contains(text, "as per bm25 model") {
			t.Fatalf("revision %s 命中 %s 的实际评分不是 BM25: %s", manifest.Revision, hit.ID, text)
		}
	}
	lex, err := lexical.OpenMulti(dirs)
	if err != nil {
		t.Fatalf("生产打开路径应接受全 BM25 的 revision %s: %v", manifest.Revision, err)
	}
	_ = lex.Close()
}

// snapshotTree 记录目录树内每个文件的 sha256 与每个子目录，用于证明旧子树
// 在升级后逐字节不变。
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel+"/"] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSameTree(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	var diffs []string
	for path, sum := range before {
		if after[path] != sum {
			diffs = append(diffs, "changed/removed "+path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			diffs = append(diffs, "added "+path)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("%s 被改动: %v", label, diffs)
	}
}

// TestStoreProfileCarriesLexicalScoring：索引子树名的 profile 段携带词法
// 评分标记，纯词法与语义配置都与 TF-IDF 时代的子树名不同；embedding 身份
// 段保持原样，向量复用的比较项不受影响。
func TestStoreProfileCarriesLexicalScoring(t *testing.T) {
	lexOnly := newTestEngine(t)
	wantLex := lexOnly.profile.ID + "-v" + lexOnly.profile.Version + "+lex-bm25"
	if lexOnly.storeProfile != wantLex {
		t.Fatalf("纯词法子树名=%q，期望 %q", lexOnly.storeProfile, wantLex)
	}
	if tfidfEraStoreProfile(lexOnly.storeProfile) == lexOnly.storeProfile {
		t.Fatalf("BM25 子树名不得与 TF-IDF 时代相同: %q", lexOnly.storeProfile)
	}
	server := newEmbedServer(t, 8)
	semantic := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 8, "fake-model"))
	wantSemantic := wantLex + "+emb-" + semantic.embedCfg.ProfileHash() + "-" + embedTemplateVersion
	if semantic.storeProfile != wantSemantic {
		t.Fatalf("语义子树名=%q，期望 %q", semantic.storeProfile, wantSemantic)
	}
}

// TestFingerprintReflectsLexicalScoring：同一组环境变量下，BM25 二进制的
// 配置指纹必须与 TF-IDF 时代 daemon 广播的指纹不同，wrapper 才不会把仍按
// TF-IDF 评分的旧 daemon 当成兼容实例复用；纯词法零值配置同样适用。
func TestFingerprintReflectsLexicalScoring(t *testing.T) {
	semantic := embedOptions("http://127.0.0.1:1/v1", 8, 8, "fake-model")
	rerankDeny := semantic
	rerankDeny.RerankDegrade = DegradeDeny
	strict := semantic
	strict.QualityStrict = true
	cases := map[string]Options{
		"lexical-only": {},
		"semantic":     semantic,
		"rerank-deny":  rerankDeny,
		"strict":       strict,
	}
	seen := map[string]string{}
	for name, opts := range cases {
		current := opts.Fingerprint()
		if current == tfidfEraFingerprint(opts) {
			t.Fatalf("%s: 指纹 %s 与 TF-IDF 时代相同，旧 daemon 会被静默复用", name, current)
		}
		if other, dup := seen[current]; dup {
			t.Fatalf("%s 与 %s 指纹相同: %s", name, other, current)
		}
		seen[current] = name
	}
}

// TestBM25MigrationReusesVectorsAndKeepsTFIDFSubtree：TF-IDF 时代建成的语义
// 索引子树在升级后逐字节保留，可作回退；新二进制在带 +lex-bm25 的新子树
// 全量重建词法索引，向量按 embedKey 全部从旧子树复用，假 provider 收不到
// 任何文档嵌入请求；新子树每个段都按 BM25 评分，引擎重启重开后仍是这些
// BM25 段在应答。
func TestBM25MigrationReusesVectorsAndKeepsTFIDFSubtree(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 8, "same-model")
	root := newFixtureWorkspace(t)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")

	legacy, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.storeProfile = tfidfEraStoreProfile(legacy.storeProfile)
	first, err := legacy.Sync(ctx, syncRequest(root))
	if err != nil || first.SemanticCoverage != "100%" {
		t.Fatalf("TF-IDF 时代首建: %+v err=%v", first, err)
	}
	legacyManifest, legacyStore := loadActiveManifest(t, legacy, root)
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dropSharedVectors(t, legacy)
	rewriteStoreAsTFIDF(t, legacyStore)
	before := snapshotTree(t, legacyStore.Root())
	callsBefore := server.callCount()

	current, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	if current.storeProfile == legacy.storeProfile {
		t.Fatalf("BM25 引擎必须换用新子树，实际仍为 %q", current.storeProfile)
	}
	migrated, err := current.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("迁移同步: %v", err)
	}
	if calls := server.callCount(); calls != callsBefore {
		t.Fatalf("迁移不得重新发送文档嵌入: provider 调用 %d→%d texts=%q", callsBefore, calls, server.textsSince(callsBefore))
	}
	if migrated.SemanticCoverage != "100%" || migrated.CrossProfileReused == 0 {
		t.Fatalf("迁移应完整覆盖且全部来自旧子树: %+v", migrated)
	}
	manifest, store := loadActiveManifest(t, current, root)
	if store.Root() == legacyStore.Root() || !strings.Contains(filepath.Base(store.Root()), "+lex-bm25+emb-") {
		t.Fatalf("新索引应落在带词法评分标记的平行子树: new=%s legacy=%s", store.Root(), legacyStore.Root())
	}
	byFile, err := loadLiveChunkRecordsByFile(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, records := range byFile {
		for _, record := range records {
			keys[embedKey(record)] = true
		}
	}
	if migrated.CrossProfileReused != len(keys) {
		t.Fatalf("全部 %d 个 embedKey 都应来自旧子树，实际复用 %d", len(keys), migrated.CrossProfileReused)
	}
	t.Logf("provider_calls legacy_build=%d migration_delta=%d cross_profile_reused=%d unique_keys=%d coverage=%s",
		callsBefore, server.callCount()-callsBefore, migrated.CrossProfileReused, len(keys), migrated.SemanticCoverage)
	assertManifestServesBM25(t, store, manifest, "session login configuration")

	// 旧子树逐字节不变，旧段仍是 TF-IDF：回退数据完好，也证明新引擎没有
	// 在旧段上原地改评分模型。
	assertSameTree(t, "TF-IDF 时代子树", before, snapshotTree(t, legacyStore.Root()))
	for _, dir := range segmentLexicalDirs(legacyStore, legacyManifest) {
		if model := persistedScoringModel(t, dir); model != "" {
			t.Fatalf("旧段 %s 的评分模型被改动: %q", dir, model)
		}
	}

	result, err := current.Search(ctx, searchRequest(root, "HandleLogin"))
	if err != nil || result.IndexRevision != manifest.Revision || !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("迁移后检索: revision=%s want=%s err=%v text=%q", result.IndexRevision, manifest.Revision, err, result.Text)
	}
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// 进程重启：新引擎实例从磁盘重开同一 BM25 revision。
	reopened, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close(ctx) }()
	again, err := reopened.Search(ctx, searchRequest(root, "HandleLogin"))
	if err != nil || again.IndexRevision != manifest.Revision || !strings.Contains(again.Text, "HandleLogin") {
		t.Fatalf("重启后检索: revision=%s want=%s err=%v", again.IndexRevision, manifest.Revision, err)
	}
	_, workspaceKey, err := reopened.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := reopened.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	served := handle.manifest
	reopened.releaseHandle(handle)
	if served.Revision != manifest.Revision {
		t.Fatalf("重开后应服务迁移产出的 revision: %s want %s", served.Revision, manifest.Revision)
	}
	assertManifestServesBM25(t, store, served, "session login configuration")
	for _, text := range server.textsSince(callsBefore) {
		if strings.HasPrefix(text, "This chunk is from ") {
			t.Fatalf("迁移及之后的检索不得重新发送文档嵌入: %q", text)
		}
	}
}

// TestBM25MigrationLexicalOnlyRebuildsWithoutCredentials：零凭据纯词法配置
// 从 TF-IDF 时代子树（default-vN）升级：不配置任何 provider，新子树全量
// 重建出 BM25 段，旧子树逐字节保留，检索结果不带降级标记。
func TestBM25MigrationLexicalOnlyRebuildsWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	root := newFixtureWorkspace(t)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")

	legacy, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	legacy.storeProfile = tfidfEraStoreProfile(legacy.storeProfile)
	if legacy.storeProfile != legacy.profile.ID+"-v"+legacy.profile.Version {
		t.Fatalf("前置：TF-IDF 时代纯词法子树名应为 chunk profile 本身: %q", legacy.storeProfile)
	}
	if _, err := legacy.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, legacyStore := loadActiveManifest(t, legacy, root)
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dropSharedVectors(t, legacy)
	rewriteStoreAsTFIDF(t, legacyStore)
	before := snapshotTree(t, legacyStore.Root())

	current, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	res, err := current.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if res.BuildMode != "full:first-build" || res.SemanticCoverage != "" || res.DegradedReason != "" {
		t.Fatalf("纯词法迁移应是新子树首建且无语义字段: %+v", res)
	}
	manifest, store := loadActiveManifest(t, current, root)
	if filepath.Base(store.Root()) != current.profile.ID+"-v"+current.profile.Version+"+lex-bm25" {
		t.Fatalf("纯词法 BM25 子树名: %s", store.Root())
	}
	assertManifestServesBM25(t, store, manifest, "session login configuration")
	assertSameTree(t, "TF-IDF 时代纯词法子树", before, snapshotTree(t, legacyStore.Root()))
	result, err := current.Search(ctx, searchRequest(root, "HandleLogin"))
	if err != nil || result.DegradedReason != "" || !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("零凭据检索应完整可用: err=%v degraded=%q", err, result.DegradedReason)
	}
}

// TestTFIDFSegmentInsideBM25SubtreeIsNotServed：BM25 子树里出现 TF-IDF 段时
// （人工复制目录或实现回归），生产打开路径拒绝该 revision，不与 BM25 段
// 混排；检索经既有损坏自愈路径重建出全 BM25 的新 revision。
func TestTFIDFSegmentInsideBM25SubtreeIsNotServed(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	first, err := e.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	_, store := loadActiveManifest(t, e, root)
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	tampered, err := store.LoadManifest(first.IndexRevision)
	if err != nil {
		t.Fatal(err)
	}
	rewriteStoreAsTFIDF(t, store)
	if _, err := lexical.OpenMulti(segmentLexicalDirs(store, tampered)); !errors.Is(err, lexical.ErrScoringModelMismatch) {
		t.Fatalf("生产打开路径必须以评分模型不符拒绝 TF-IDF 段，不得把它当作 BM25 段服务: %v", err)
	}

	e2, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e2.Close(ctx) }()
	result, err := e2.Search(ctx, searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("TF-IDF 段应按损坏处理并自愈: %v", err)
	}
	if result.IndexRevision == first.IndexRevision {
		t.Fatal("不得继续服务含 TF-IDF 段的 revision")
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("自愈后应有正确结果: %q", result.Text)
	}
	healed, _ := loadActiveManifest(t, e2, root)
	assertManifestServesBM25(t, store, healed, "session login configuration")
}

// TestDeltaAndCompactionSegmentsStayBM25：增量 delta 段与 compaction 合并段
// 都按 BM25 写盘；多段 revision 经 alias 聚合检索时每个命中都走 BM25 分支。
func TestDeltaAndCompactionSegmentsStayBM25(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "gen1.py", "def gen1():\n    return 'session one'\n")
	delta, err := e.Sync(ctx, syncRequest(root))
	if err != nil || delta.BuildMode != "delta" {
		t.Fatalf("新增文件应走 delta: %+v err=%v", delta, err)
	}
	manifest, store := loadActiveManifest(t, e, root)
	if len(manifest.Segments) != 2 {
		t.Fatalf("delta 后应为两段: %d", len(manifest.Segments))
	}
	assertManifestServesBM25(t, store, manifest, "session")

	var last string
	for i := 2; i <= compactSegmentThreshold; i++ {
		writeFixture(t, root, fmt.Sprintf("gen%d.py", i), "def gen():\n    return 'session'\n")
		res, err := e.Sync(ctx, syncRequest(root))
		if err != nil {
			t.Fatal(err)
		}
		last = res.BuildMode
	}
	compacted, _ := loadActiveManifest(t, e, root)
	if last != "full:compaction-segments" || len(compacted.Segments) != 1 {
		t.Fatalf("应触发段数 compaction 合并回单段: mode=%s segments=%d", last, len(compacted.Segments))
	}
	assertManifestServesBM25(t, store, compacted, "session")
}
