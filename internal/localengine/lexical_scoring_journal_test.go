package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
)

// TestBM25MigrationReusesInterruptedTFIDFJournal：TF-IDF 时代的首建在嵌入中途
// 被打断，旧子树只有词法中间 revision（没有向量）、journal 里已付费的
// main.go 向量，以及 README 的零向量拒绝记录。升级后的首次同步只为从未
// 付费的 util.py 送审：main.go 从旧 journal 只读复用，README 沿用拒绝集
// 不再送审，整个过程没有任何文本被付费两次；旧子树逐字节不变。
func TestBM25MigrationReusesInterruptedTFIDFJournal(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	var mu sync.Mutex
	var paid []string
	holding := true
	release := make(chan struct{})
	held := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		hold := holding && len(req.Input) == 1 && strings.Contains(req.Input[0], "parse_config")
		mu.Unlock()
		if hold {
			select {
			case held <- struct{}{}:
			default:
			}
			<-release
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		paid = append(paid, req.Input...)
		mu.Unlock()
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			vec := fakeVector(dim, text)
			if strings.Contains(text, "Demo App") {
				vec = make([]float32, dim)
			}
			items = append(items, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		ts.Close()
	}()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)
	opts := embedOptions(ts.URL, dim, 1, "same-model")
	// 窗口为 1：逐批顺序嵌入，被扣住的 util.py 之前的批次都已落 journal。
	opts.Embedding.InitialConcurrency = 1

	legacy, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.storeProfile = tfidfEraStoreProfile(legacy.storeProfile)
	syncDone := make(chan error, 1)
	go func() {
		_, err := legacy.Sync(ctx, syncRequest(root))
		syncDone <- err
	}()
	<-held
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := legacy.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	dropSharedVectors(t, legacy)
	if err := <-syncDone; err == nil {
		t.Fatal("被打断的 TF-IDF 时代首建不应成功")
	}
	_, workspaceKey, err := legacy.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	legacyStore, err := legacy.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	legacyManifest, _, err := legacyStore.ResolveUsable()
	if err != nil || legacyManifest.VectorCount != 0 {
		t.Fatalf("前置：旧子树应只有没有向量的词法中间 revision: %+v err=%v", legacyManifest, err)
	}
	journalVectors, journalRejected, err := index.ReadJournalSnapshot(legacyStore.JournalDir(), dim)
	if err != nil || len(journalVectors) == 0 || len(journalRejected) != 1 {
		t.Fatalf("前置：旧 journal 应有已付费向量与 1 条拒绝记录: vectors=%d rejected=%d err=%v", len(journalVectors), len(journalRejected), err)
	}
	rewriteStoreAsTFIDF(t, legacyStore)
	before := snapshotTree(t, legacyStore.Root())
	mu.Lock()
	holding = false
	paidBefore := append([]string(nil), paid...)
	mu.Unlock()

	current, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	migrated, err := current.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("迁移同步: %v", err)
	}
	mu.Lock()
	migrationTexts := append([]string(nil), paid[len(paidBefore):]...)
	mu.Unlock()
	sawUtil := false
	for _, text := range migrationTexts {
		if strings.Contains(text, "Demo App") {
			t.Fatalf("旧子树已拒绝的 README 不得再次送审: %q", text)
		}
		if strings.Contains(text, "parse_config") {
			sawUtil = true
		}
	}
	if !sawUtil {
		t.Fatalf("从未付费的 util.py 应在迁移时送审: %q", migrationTexts)
	}
	seen := map[string]int{}
	mu.Lock()
	for _, text := range paid {
		seen[text]++
	}
	mu.Unlock()
	for text, count := range seen {
		if count > 1 {
			t.Fatalf("同一文本被付费 %d 次: %q", count, text)
		}
	}
	if migrated.CrossProfileReused != len(journalVectors) {
		t.Fatalf("旧 journal 的 %d 条已付费向量应全部复用，实际 %d", len(journalVectors), migrated.CrossProfileReused)
	}
	t.Logf("legacy_paid_texts=%d legacy_journal_vectors=%d legacy_rejected=%d migration_texts=%d cross_profile_reused=%d",
		len(paidBefore), len(journalVectors), len(journalRejected), len(migrationTexts), migrated.CrossProfileReused)
	if migrated.SemanticCoverage != "100%" || strings.Contains(migrated.DegradedReason, "semantic-coverage-partial") || strings.Contains(migrated.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("README 沿用的拒绝不计入覆盖率分母，覆盖完整，也不属于迁移阻止: %+v", migrated)
	}
	manifest, store := loadActiveManifest(t, current, root)
	assertManifestServesBM25(t, store, manifest, "session login configuration")
	if manifest.VectorCount != manifest.Counts.Chunks-1 {
		t.Fatalf("除 README 外都应有向量: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	_, inherited, err := index.ReadJournalSnapshot(store.JournalDir(), dim)
	if err != nil || len(inherited) != 1 {
		t.Fatalf("README 的拒绝记录应写进新子树的拒绝集: %v err=%v", inherited, err)
	}
	for key := range journalRejected {
		if !inherited[key] {
			t.Fatalf("新子树拒绝集缺少旧子树的键 %s", key)
		}
	}
	assertSameTree(t, "被打断的 TF-IDF 时代子树", before, snapshotTree(t, legacyStore.Root()))

	again, err := current.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	extra := len(paid) - len(paidBefore) - len(migrationTexts)
	mu.Unlock()
	if extra != 0 || again.IndexRevision != manifest.Revision {
		t.Fatalf("迁移完成后的同步不得再送审: extra=%d revision=%s want=%s", extra, again.IndexRevision, manifest.Revision)
	}
}

// TestBM25MigrationBlockedBySiblingBatchJobs：旧子树的 bulk-jobs.json 还登记着
// 未回收结果的 Batch 作业时，待送审的缺口里一旦有作业登记过的内容，新子树
// 在任何 provider 请求之前停下：全量迁移零次文档嵌入请求，同步结果带
// semantic-migration-blocked，workspace_status 给出状态文件路径与恢复步骤，
// 离线计划拒绝导出任务；随后只新增与作业无关内容的 delta 照常送审这段新
// 内容（只送一次），作业登记的 util.py 始终不送，阻止原因继续可见；词法
// 检索照常可用，旧子树逐字节不变。用户确认后移走状态文件，下一次同步才为
// util.py 送审并补齐覆盖。
func TestBM25MigrationBlockedBySiblingBatchJobs(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	server.setFailWhen(func(texts []string) bool {
		for _, text := range texts {
			if strings.Contains(text, "parse_config") {
				return true
			}
		}
		return false
	})
	opts := embedOptions(server.ts.URL, dim, 1, "same-model")
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)

	legacy, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.storeProfile = tfidfEraStoreProfile(legacy.storeProfile)
	first, err := legacy.Sync(ctx, syncRequest(root))
	if err != nil || first.SemanticCoverage == "100%" {
		t.Fatalf("前置：util.py 嵌入失败，旧索引覆盖不完整: %+v err=%v", first, err)
	}
	legacyManifest, legacyStore := loadActiveManifest(t, legacy, root)
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dropSharedVectors(t, legacy)
	byFile, err := loadLiveChunkRecordsByFile(legacyStore, legacyManifest)
	if err != nil || len(byFile["util.py"]) == 0 {
		t.Fatalf("前置：应能读到 util.py 的 chunk: %v", err)
	}
	var jobKeys []string
	for _, record := range byFile["util.py"] {
		jobKeys = append(jobKeys, embedKey(record))
	}
	bulkPath := filepath.Join(legacyStore.JournalDir(), bulkStateFileName)
	if err := saveBulkState(bulkPath, bulkState{
		ProfileHash: legacy.embedCfg.ProfileHash(),
		Jobs:        []embedding.BulkJob{{ID: "batch-inflight", InputFileID: "file-inflight", Keys: jobKeys, SubmittedAt: time.Now().UTC()}},
	}); err != nil {
		t.Fatal(err)
	}
	rewriteStoreAsTFIDF(t, legacyStore)
	before := snapshotTree(t, legacyStore.Root())
	server.setFailWhen(nil)
	callsBefore := server.callCount()

	current, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	blocked, err := current.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("阻止语义迁移不应让词法同步失败: %v", err)
	}
	blockedFullCalls := server.callCount() - callsBefore
	if blockedFullCalls != 0 {
		t.Fatalf("迁移被阻止时 provider 调用必须为零: %d→%d texts=%q", callsBefore, server.callCount(), server.textsSince(callsBefore))
	}
	if blocked.SemanticCoverage == "100%" || !strings.Contains(blocked.DegradedReason, "semantic-coverage-partial") || !strings.Contains(blocked.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("同步结果必须显式带出缺口与阻止原因: %+v", blocked)
	}
	status, err := current.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: root})
	if err != nil || status.Semantic == nil || !strings.Contains(status.Semantic.LastError, "语义迁移已暂停") || !strings.Contains(status.Semantic.LastError, bulkPath) || !strings.Contains(status.Semantic.LastError, "恢复") {
		t.Fatalf("workspace_status 应给出状态文件路径与恢复步骤: %+v err=%v", status.Semantic, err)
	}
	manifest, store := loadActiveManifest(t, current, root)
	assertManifestServesBM25(t, store, manifest, "session login configuration")

	exported := 0
	if _, err := current.PlanEmbedJobs(ctx, engine.WorkspaceRef{DirectoryPath: root}, func(EmbedJob) error {
		exported++
		return nil
	}); err == nil || !strings.Contains(err.Error(), "语义迁移已暂停") || exported != 0 {
		t.Fatalf("离线计划在阻止期间必须报错且不导出任务: exported=%d err=%v", exported, err)
	}
	writeFixture(t, root, "extra.go", "package app\n\nfunc ExtraSession() {}\n")
	delta, err := current.Sync(ctx, syncRequest(root))
	if err != nil || delta.BuildMode != "delta" || !strings.Contains(delta.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("delta 结果应继续带出阻止原因: %+v err=%v", delta, err)
	}
	deltaTexts := server.textsSince(callsBefore)
	if len(deltaTexts) != 1 || !strings.Contains(deltaTexts[0], "ExtraSession") {
		t.Fatalf("delta 只应为与旧作业无关的新内容送审一次: %q", deltaTexts)
	}
	blockedDeltaCalls := server.callCount() - callsBefore
	assertSameTree(t, "带未回收 Batch 作业的旧子树", before, snapshotTree(t, legacyStore.Root()))

	// 用户到 provider 控制台确认作业已不需要后，人工移走状态文件。
	if err := os.Remove(bulkPath); err != nil {
		t.Fatal(err)
	}
	resumed, err := current.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SemanticCoverage != "100%" || resumed.DegradedReason != "" {
		t.Fatalf("移走状态文件后应补齐覆盖: %+v", resumed)
	}
	texts := server.textsSince(callsBefore)
	sent := map[string]int{}
	for _, text := range texts {
		sent[text]++
		if !strings.Contains(text, "parse_config") && !strings.Contains(text, "ExtraSession") {
			t.Fatalf("只应为从未付费的缺口送审: %q", text)
		}
	}
	for text, count := range sent {
		if count != 1 {
			t.Fatalf("同一输入被送审 %d 次: %q", count, text)
		}
	}
	if len(sent) != 2 {
		t.Fatalf("恢复后 util.py 应送审一次，加上 delta 的 extra.go 共 2 段输入: %v", sent)
	}
	t.Logf("provider_calls legacy=%d blocked_full=%d delta_unrelated_documents=%d plan_exported=%d total_unique_inputs_after_recovery=%d blocked_reason=%q",
		callsBefore, blockedFullCalls, blockedDeltaCalls, exported, len(sent), blocked.DegradedReason)
}

// TestSiblingJournalOfOtherEmbeddingIdentityIsNotReused：另一 embedding 身份
// （不同模型）子树 journal 里的向量即使 embedKey 相同也不复用：embedKey 不含
// 模型，混用会把旧模型的向量当作新模型的结果。新模型按正常流程为全部
// chunk 送审，旧子树不被改动。
func TestSiblingJournalOfOtherEmbeddingIdentityIsNotReused(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)

	old, err := New(embedOptions(server.ts.URL, dim, 8, "old-model"))
	if err != nil {
		t.Fatal(err)
	}
	old.storeProfile = tfidfEraStoreProfile(old.storeProfile)
	if _, err := old.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	oldManifest, oldStore := loadActiveManifest(t, old, root)
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dropSharedVectors(t, old)
	byFile, err := loadLiveChunkRecordsByFile(oldStore, oldManifest)
	if err != nil {
		t.Fatal(err)
	}
	paidByOldModel := map[string][]float32{}
	for _, records := range byFile {
		for _, record := range records {
			paidByOldModel[embedKey(record)] = fakeVector(dim, "old-model:"+record.Content)
		}
	}
	journal, err := index.OpenJournal(oldStore, dim)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(paidByOldModel); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, oldStore.Root())
	callsBefore := server.callCount()

	current, err := New(embedOptions(server.ts.URL, dim, 8, "new-model"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	res, err := current.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if res.CrossProfileReused != 0 || res.SemanticCoverage != "100%" {
		t.Fatalf("不同模型的 journal 不得复用: %+v", res)
	}
	sent := map[string]bool{}
	for _, text := range server.textsSince(callsBefore) {
		sent[text] = true
	}
	if len(sent) != len(paidByOldModel) {
		t.Fatalf("新模型应为全部 %d 个键送审，实际 %d", len(paidByOldModel), len(sent))
	}
	assertSameTree(t, "其他 embedding 身份的旧子树", before, snapshotTree(t, oldStore.Root()))
}
