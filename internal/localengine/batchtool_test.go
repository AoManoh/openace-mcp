package localengine

import (
	"context"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// Batch API 驱动工具的引擎侧钩子(2026-08-05 排期主线;台账 §7 -8(e)):
// PlanEmbedJobs 枚举待嵌任务(key+送审文本,零 provider 调用),
// ImportEmbeddings 把离线批量结果回灌 journal,随后 sync 零 provider
// 收编发布(2026-08-03 batch verdict §2 预设接口)。

// 计划面与在线路径的逐字节一致性:PlanEmbedJobs 产出的 (key,text) 集合
// 必须与随后真实 Sync 送 provider 的文本集合完全一致——键/模板由引擎
// 单一实现导出,工具侧零重组(防 e3embed 式复制漂移)。
func TestPlanEmbedJobsMatchesOnlinePath(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 2, "fake-model"))
	root := t.TempDir()
	writeFixture(t, root, "pkg/auth/login.go", "package auth\n\nfunc HandleLogin() {}\n")
	writeFixture(t, root, "pkg/auth/logout.go", "package auth\n\nfunc HandleLogout() {}\n")
	writeFixture(t, root, "docs/guide.md", "# 指南\n\n登录流程说明。\n")
	ref := engine.WorkspaceRef{DirectoryPath: root}

	var jobs []EmbedJob
	plan, err := e.PlanEmbedJobs(context.Background(), ref, func(job EmbedJob) error {
		jobs = append(jobs, job)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.callCount() != 0 {
		t.Fatalf("计划面不得调用 provider: calls=%d", server.callCount())
	}
	if plan.Pending != len(jobs) || plan.Pending == 0 {
		t.Fatalf("计划摘要与产出不符: plan=%+v jobs=%d", plan, len(jobs))
	}
	if plan.Reusable != 0 || plan.Rejected != 0 {
		t.Fatalf("首建应全部待嵌: %+v", plan)
	}

	// 真实 Sync:送审文本集合必须与计划完全一致。
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatal(err)
	}
	sent := map[string]bool{}
	for _, text := range server.textsSince(0) {
		sent[text] = true
	}
	if len(sent) != len(jobs) {
		t.Fatalf("送审唯一文本数 %d != 计划任务数 %d", len(sent), len(jobs))
	}
	seenKeys := map[string]bool{}
	for _, job := range jobs {
		if !sent[job.Text] {
			t.Fatalf("计划文本未出现在真实送审集: %q", job.Text)
		}
		if seenKeys[job.Key] {
			t.Fatalf("计划内 key 重复: %s", job.Key)
		}
		seenKeys[job.Key] = true
	}

	// 已发布后再计划:全部可复用,零待嵌。
	jobs = jobs[:0]
	plan2, err := e.PlanEmbedJobs(context.Background(), ref, func(job EmbedJob) error {
		jobs = append(jobs, job)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Pending != 0 || len(jobs) != 0 || plan2.Reusable != plan.Pending {
		t.Fatalf("发布后计划应零待嵌全复用: %+v jobs=%d", plan2, len(jobs))
	}
}

// 回灌闭环:计划 → 离线造向量 → ImportEmbeddings → Sync 零 provider
// 调用发布,语义覆盖完整(batch verdict §2:"回灌 journal 后引擎 sync
// 零 provider 调用直接收编")。
func TestImportEmbeddingsZeroProviderSync(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 2, "fake-model"))
	root := t.TempDir()
	writeFixture(t, root, "pkg/a.go", "package pkg\n\nfunc A() {}\n")
	writeFixture(t, root, "pkg/b.go", "package pkg\n\nfunc B() {}\n")
	ref := engine.WorkspaceRef{DirectoryPath: root}

	var jobs []EmbedJob
	if _, err := e.PlanEmbedJobs(context.Background(), ref, func(job EmbedJob) error {
		jobs = append(jobs, job)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(jobs) == 0 {
		t.Fatal("首建应有待嵌任务")
	}

	// 离线结果:1 条零向量 + 1 条错维度(均用合法键,R1 后陌生键在向量
	// 校验前即被拒,病理向量路径必须以合法键触达)+ 全量合法向量 +
	// 1 条重复键。坏行在前、同键好行在后:坏行按对应计数跳过,好行照常
	// 落盘,发布仍完整。
	type row struct {
		key string
		vec []float32
	}
	rows := make([]row, 0, len(jobs)+3)
	rows = append(rows, row{key: jobs[0].Key, vec: make([]float32, 8)}) // 零向量(合法键)
	rows = append(rows, row{key: jobs[1].Key, vec: make([]float32, 4)}) // 错维度(合法键)
	for _, job := range jobs {
		rows = append(rows, row{key: job.Key, vec: fakeVector(8, job.Text)})
	}
	rows = append(rows, rows[2]) // 重复键(首个合法向量行)
	i := 0
	report, err := e.ImportEmbeddings(context.Background(), ref, func() (string, []float32, bool, error) {
		if i >= len(rows) {
			return "", nil, false, nil
		}
		r := rows[i]
		i++
		return r.key, r.vec, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Appended != len(jobs) || report.BadVector != 1 || report.WrongDim != 1 || report.Existing != 1 {
		t.Fatalf("回灌报告不符: %+v (期望 appended=%d bad=1 wrongdim=1 existing=1)", report, len(jobs))
	}

	// Sync 必须零 provider 调用且语义完整发布。
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatal(err)
	}
	if server.callCount() != 0 {
		t.Fatalf("回灌后 sync 不得调用 provider: calls=%d texts=%v", server.callCount(), server.textsSince(0))
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("回灌后发布应语义完整: counts=%+v", manifest.Counts)
	}
}

// 语义未启用时计划/回灌都必须显式报错(子树身份依赖 embedding profile,
// 无 provider 配置时无从定位,静默空转是坑)。
func TestBatchToolRequiresSemantic(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	writeFixture(t, root, "a.go", "package a\n")
	ref := engine.WorkspaceRef{DirectoryPath: root}
	if _, err := e.PlanEmbedJobs(context.Background(), ref, func(EmbedJob) error { return nil }); err == nil {
		t.Fatal("semantic off 时 PlanEmbedJobs 应报错")
	}
	if _, err := e.ImportEmbeddings(context.Background(), ref, func() (string, []float32, bool, error) {
		return "", nil, false, nil
	}); err == nil {
		t.Fatal("semantic off 时 ImportEmbeddings 应报错")
	}
}

// R1(review 2026-08-06):陌生 key + 合法向量必须被拒——历史实现只做
// 查重/维度/归一化,错误归属的向量可入 journal 并永久屏蔽该 chunk 的
// 正确嵌入(journal 命中即不再送 provider)。
func TestImportEmbeddingsRejectsForeignKeys(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 2, "fake-model"))
	root := t.TempDir()
	writeFixture(t, root, "pkg/a.go", "package pkg\n\nfunc A() {}\n")
	ref := engine.WorkspaceRef{DirectoryPath: root}
	var jobs []EmbedJob
	if _, err := e.PlanEmbedJobs(context.Background(), ref, func(job EmbedJob) error {
		jobs = append(jobs, job)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	foreign := "0123456789abcdef0123456789abcdef" // 形状合法、不属本仓 pending 集
	rows := [][2]any{}
	for _, job := range jobs {
		rows = append(rows, [2]any{job.Key, fakeVector(8, job.Text)})
	}
	rows = append(rows, [2]any{foreign, fakeVector(8, "foreign-but-valid")})
	i := 0
	report, err := e.ImportEmbeddings(context.Background(), ref, func() (string, []float32, bool, error) {
		if i >= len(rows) {
			return "", nil, false, nil
		}
		r := rows[i]
		i++
		return r[0].(string), r[1].([]float32), true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.UnknownKey != 1 {
		t.Fatalf("陌生 key 应计入 UnknownKey=1,得到 %+v", report)
	}
	if report.Appended != len(jobs) {
		t.Fatalf("合法键应全部落盘: %+v", report)
	}
	// journal 内不得含陌生 key(防垃圾永驻/挤占容量)。
	store, err := e.storeFor(mustKey(t, e, root))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := e.journalFor(mustKey(t, e, root), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := journal.Snapshot()[foreign]; ok {
		t.Fatal("陌生 key 进入了 journal")
	}
}
