package localengine

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 2026-09-03 评测误停复盘:openace-bench -sync-only 的零费预检用
// PlanEmbedJobs 复核缺口,而计划面只把当前 chunk profile 子树
// (active/previous revision ∪ journal)算作复用池。二进制 profile 升到
// v8 而缓存由 v7 建成时,v8 子树为空,计划面报 57,650 个 chunk 缺向量并
// 拒绝执行;同一时刻在线构建(buildFull 冷子树分支 →
// mergeSiblingProfileVectors)会按内容哈希从 v7 兄弟子树零费复用全部
// 向量,真实缺口为 0。计划面的复用口径必须与在线构建一致。
func TestPlanEmbedJobsCountsSiblingProfileVectorsAsReusable(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 8, "same-model")
	root := newFixtureWorkspace(t)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	ref := engine.WorkspaceRef{DirectoryPath: root}

	// 旧 profile(v7)子树:真实在线构建,语义 100% 覆盖。
	e7, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e7.profile.Version = "7"
	e7.storeProfile = strings.Replace(e7.storeProfile, "default-v8", "default-v7", 1)
	first, err := e7.Sync(context.Background(), syncRequest(root))
	if err != nil || first.SemanticCoverage != "100%" {
		t.Fatalf("v7 首建失败: %+v err=%v", first, err)
	}
	if err := e7.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	callsAfterV7 := server.callCount()
	if callsAfterV7 == 0 {
		t.Fatal("v7 首建应调用 provider")
	}

	// 新 profile(v8)引擎:当前子树为空,唯一向量来源是 v7 兄弟子树。
	e8, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e8.Close(context.Background())
	var jobs []EmbedJob
	plan, err := e8.PlanEmbedJobs(context.Background(), ref, func(job EmbedJob) error {
		jobs = append(jobs, job)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.StoreProfile, "default-v8") {
		t.Fatalf("计划应落在新 profile 子树: %+v", plan)
	}
	if server.callCount() != callsAfterV7 {
		t.Fatalf("计划面不得调用 provider: calls %d→%d", callsAfterV7, server.callCount())
	}
	if plan.UniqueKeys == 0 || plan.TotalChunks == 0 {
		t.Fatalf("夹具应产出 chunk: %+v", plan)
	}
	// 核心断言:缺口 0、全部可复用(N = 唯一 embedKey 数)。
	if plan.Pending != 0 || len(jobs) != 0 {
		t.Fatalf("兄弟子树持有全部向量时计划面仍报缺口: pending=%d jobs=%d plan=%+v", plan.Pending, len(jobs), plan)
	}
	if plan.Reusable != plan.UniqueKeys {
		t.Fatalf("复用数应等于唯一键数: %+v", plan)
	}
	// 当前子树为空、journal 为空,复用只能全部归因于兄弟子树。
	if plan.CrossProfileReusable != plan.Reusable {
		t.Fatalf("复用应全部归因兄弟 profile 子树: %+v", plan)
	}
	if plan.Rejected != 0 {
		t.Fatalf("不应有拒绝: %+v", plan)
	}

	// 对账:随后真实 Sync 必须零 provider 调用,且在线跨 profile 复用数
	// 与计划面一致——计划面口径 = 在线构建口径。
	second, err := e8.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if server.callCount() != callsAfterV7 {
		t.Fatalf("在线 sync 应零 provider(与计划一致): calls %d→%d", callsAfterV7, server.callCount())
	}
	if second.CrossProfileReused != plan.CrossProfileReusable || second.SemanticCoverage != "100%" {
		t.Fatalf("在线复用数应与计划一致: plan=%+v result=%+v", plan, second)
	}
}

// 完整现役 revision 不并入兄弟子树(与 buildFull 阶段 2.5 门槛一致):
// 计划面不得因新增文件就把兄弟子树的向量算成复用——在线构建同形态下
// (delta 或 compaction 全量)同样不消费兄弟子树,否则计划少报缺口,
// -sync-only 放行后在线 sync 反而付费。
func TestPlanEmbedJobsSkipsSiblingWhenCurrentRevisionComplete(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 8, "same-model")
	root := newFixtureWorkspace(t)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	ref := engine.WorkspaceRef{DirectoryPath: root}

	// v8(当前)先完整建成;之后新增文件并只让 v7 兄弟子树嵌入它。
	e8, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e8.Close(context.Background())
	if res, err := e8.Sync(context.Background(), syncRequest(root)); err != nil || res.SemanticCoverage != "100%" {
		t.Fatalf("v8 首建失败: %+v err=%v", res, err)
	}
	writeFixture(t, root, "extra.go", "package app\n\nfunc ExtraOnlyInSibling() {}\n")
	e7, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e7.profile.Version = "7"
	e7.storeProfile = strings.Replace(e7.storeProfile, "default-v8", "default-v7", 1)
	if res, err := e7.Sync(context.Background(), syncRequest(root)); err != nil || res.SemanticCoverage != "100%" {
		t.Fatalf("v7 构建失败: %+v err=%v", res, err)
	}
	if err := e7.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// v8 现役 revision 语义完整且物理健全:新文件的 chunk 必须报缺口,
	// 兄弟子树不参与(在线 delta 构建亦会为其付费)。
	var jobs []EmbedJob
	plan, err := e8.PlanEmbedJobs(context.Background(), ref, func(job EmbedJob) error {
		jobs = append(jobs, job)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Pending == 0 || len(jobs) != plan.Pending {
		t.Fatalf("完整现役 revision 下新增 chunk 应报缺口: plan=%+v jobs=%d", plan, len(jobs))
	}
	if plan.CrossProfileReusable != 0 {
		t.Fatalf("完整现役 revision 不得并入兄弟子树: %+v", plan)
	}
	for _, job := range jobs {
		// 送审文本头固定携带 RelPath(embedDocText),据此归属文件。
		if !strings.Contains(job.Text, "This chunk is from extra.go,") {
			t.Fatalf("缺口应只含新增文件的 chunk: %q", job.Text)
		}
	}
	// 对账:在线 sync 为新增 chunk 付费的次数与计划缺口一致。
	before := server.callCount()
	second, err := e8.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if server.callCount() == before || second.CrossProfileReused != 0 {
		t.Fatalf("在线 sync 应为新增 chunk 付费且不消费兄弟子树: calls %d→%d result=%+v", before, server.callCount(), second)
	}
	sent := map[string]bool{}
	for _, text := range server.textsSince(before) {
		sent[text] = true
	}
	if len(sent) != len(jobs) {
		t.Fatalf("在线送审唯一文本数 %d != 计划缺口 %d", len(sent), len(jobs))
	}
	for _, job := range jobs {
		if !sent[job.Text] {
			t.Fatalf("计划缺口文本未出现在真实送审集: %q", job.Text)
		}
	}
}
