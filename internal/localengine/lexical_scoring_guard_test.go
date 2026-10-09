package localengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// 本文件验证换子树迁移时防止重复付费的保护：只读检查同一 embedding 身份
// 的旧子树（TF-IDF 时代）里与本次缺口有关的已付费状态。计数对象说明：
// documentTexts 只统计文档嵌入请求（送审文本以 "This chunk is from " 开头），
// 查询嵌入另行统计；这些测试显式配置维度，不发生构造期维度探测。

const docTextPrefix = "This chunk is from "

// documentTexts 返回自第 fromCall 次调用起送审的文档嵌入文本。
func documentTexts(server *embedServer, fromCall int) []string {
	var out []string
	for _, text := range server.textsSince(fromCall) {
		if strings.HasPrefix(text, docTextPrefix) {
			out = append(out, text)
		}
	}
	return out
}

// queryTexts 返回自第 fromCall 次调用起的查询嵌入文本。
func queryTexts(server *embedServer, fromCall int) []string {
	var out []string
	for _, text := range server.textsSince(fromCall) {
		if !strings.HasPrefix(text, docTextPrefix) {
			out = append(out, text)
		}
	}
	return out
}

// guardFixture 是一份 TF-IDF 时代的语义索引：failMarker 所在 chunk 的文档嵌入
// 失败，旧 revision 里没有它的向量。
type guardFixture struct {
	server      *embedServer
	opts        Options
	root        string
	legacyStore *index.Store
	// failedKeys 是嵌入失败的 chunk 在旧子树里的 embedKey。
	failedKeys []string
	// failedRecords 是这些 chunk 的记录，供测试追加 journal 向量。
	failedRecords []chunkRecord
	// profileHash 是旧引擎实际使用的 embedding ProfileHash（含模板版本），
	// 写入批作业状态文件。
	profileHash string
}

// newGuardFixture 建出 guardFixture；failFile 非空时先写入该文件。failFile 要
// 排在其他文件之后（用 zz_ 前缀）：嵌入按路径顺序进行，失败后熔断会让排在
// 它后面的批次也拿不到向量。
func newGuardFixture(t *testing.T, failFile string, failContent string, failMarker string) guardFixture {
	t.Helper()
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	server.setFailWhen(func(texts []string) bool {
		for _, text := range texts {
			if strings.Contains(text, failMarker) {
				return true
			}
		}
		return false
	})
	opts := embedOptions(server.ts.URL, dim, 1, "same-model")
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)
	if failFile != "" {
		writeFixture(t, root, failFile, failContent)
	}
	legacy, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.storeProfile = tfidfEraStoreProfile(legacy.storeProfile)
	if res, err := legacy.Sync(ctx, syncRequest(root)); err != nil || res.SemanticCoverage == "100%" {
		t.Fatalf("前置：%s 嵌入失败，旧索引覆盖不完整: %+v err=%v", failMarker, res, err)
	}
	manifest, store := loadActiveManifest(t, legacy, root)
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	byFile, err := loadLiveChunkRecordsByFile(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture := guardFixture{server: server, opts: opts, root: root, legacyStore: store, profileHash: legacy.embedCfg.ProfileHash()}
	for _, records := range byFile {
		for _, record := range records {
			if strings.Contains(embedDocText(record), failMarker) {
				fixture.failedKeys = append(fixture.failedKeys, embedKey(record))
				fixture.failedRecords = append(fixture.failedRecords, record)
			}
		}
	}
	if len(fixture.failedKeys) == 0 {
		t.Fatalf("前置：应找到 %s 的 chunk", failMarker)
	}
	rewriteStoreAsTFIDF(t, store)
	server.setFailWhen(nil)
	return fixture
}

func (f guardFixture) bulkPath() string {
	return filepath.Join(f.legacyStore.JournalDir(), bulkStateFileName)
}

func (f guardFixture) writeBulkJobs(t *testing.T, state bulkState) {
	t.Helper()
	if err := saveBulkState(f.bulkPath(), state); err != nil {
		t.Fatal(err)
	}
}

func lastSemanticError(t *testing.T, e *Engine, root string) string {
	t.Helper()
	status, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil || status.Semantic == nil {
		t.Fatalf("workspace_status: %+v err=%v", status, err)
	}
	return status.Semantic.LastError
}

// TestMigrationBlocksOnCorruptSiblingJournal：旧 journal 末条记录 CRC 损坏时，
// 新子树不为缺口送审（文档嵌入请求 0 次），同步结果带阻止原因，状态给出
// 文件、位置与针对损坏的恢复步骤，而不是笼统的“修复权限”；旧文件不被改动。
func TestMigrationBlocksOnCorruptSiblingJournal(t *testing.T) {
	ctx := context.Background()
	f := newGuardFixture(t, "", "", "parse_config")
	journal, err := index.OpenJournal(f.legacyStore, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range []string{f.failedKeys[0], "paid-other"} {
		vec := fakeVector(8, key)
		if err := vector.Normalize(vec); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(map[string][]float32{key: vec}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journalFile := filepath.Join(f.legacyStore.JournalDir(), "vectors.journal")
	data, err := os.ReadFile(journalFile)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(journalFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, f.legacyStore.Root())
	callsBefore := f.server.callCount()

	current, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	res, err := current.Sync(ctx, syncRequest(f.root))
	if err != nil {
		t.Fatalf("默认 allow 下阻止语义迁移不应让同步失败: %v", err)
	}
	if docs := documentTexts(f.server, callsBefore); len(docs) != 0 {
		t.Fatalf("旧 journal 损坏时文档嵌入请求必须为 0: %q", docs)
	}
	if !strings.Contains(res.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("同步结果应带阻止原因: %+v", res)
	}
	reason := lastSemanticError(t, current, f.root)
	for _, want := range []string{journalFile, "第 2 条记录", "不完整或校验失败", "升级前"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("阻止原因应包含 %q: %s", want, reason)
		}
	}
	if strings.Contains(reason, "读取权限") {
		t.Fatalf("损坏不应被说成权限问题: %s", reason)
	}
	assertSameTree(t, "损坏 journal 的旧子树", before, snapshotTree(t, f.legacyStore.Root()))
	t.Logf("corrupt_journal document_requests=%d degraded=%q", len(documentTexts(f.server, callsBefore)), res.DegradedReason)
}

// TestMigrationBlocksWhenSiblingStateCannotBeChecked：列不出工作区索引目录、
// 或同一身份的旧子树读不进去时，不能当作“没有旧的已付费状态”继续送审。
// 故障用 Unix 目录权限位注入：Windows 的 os.Chmod 只切换只读属性，不能让
// 目录不可列出或不可进入，因此跳过；root 或带 CAP_DAC_OVERRIDE 的用户不受
// 权限位限制，收紧权限后探测仍能访问时同样跳过。
func TestMigrationBlocksWhenSiblingStateCannotBeChecked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 os.Chmod 不提供 Unix 读、执行权限语义，无法构造检查失败，跳过")
	}
	cases := []struct {
		name   string
		target func(f guardFixture) string
		mode   os.FileMode
		// probe 在收紧权限后访问 target；仍然成功说明当前用户不受权限位限制。
		probe func(target string) error
		want  string
	}{
		{"工作区索引目录不可列出", func(f guardFixture) string { return filepath.Dir(f.legacyStore.Root()) }, 0o300,
			func(target string) error { _, err := os.ReadDir(target); return err }, "列出"},
		{"同一身份的旧子树不可进入", func(f guardFixture) string { return f.legacyStore.Root() }, 0o000,
			func(target string) error { _, err := os.Stat(index.JournalDirOf(target)); return err }, "检查"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newGuardFixture(t, "", "", "parse_config")
			current, err := New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close(ctx) }()
			// 先让新子树存在，再收紧权限：新子树自身的读写不受影响。
			_, workspaceKey, err := current.resolveRoot(f.root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := current.storeFor(workspaceKey); err != nil {
				t.Fatal(err)
			}
			target := tc.target(f)
			if err := os.Chmod(target, tc.mode); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Chmod(target, 0o700) }()
			if tc.probe(target) == nil {
				t.Skip("当前用户可无视权限位（root/CAP_DAC_OVERRIDE），无法构造检查失败，跳过")
			}
			callsBefore := f.server.callCount()
			res, err := current.Sync(ctx, syncRequest(f.root))
			if err != nil {
				t.Fatalf("同步: %v", err)
			}
			if docs := documentTexts(f.server, callsBefore); len(docs) != 0 {
				t.Fatalf("无法检查旧子树时文档嵌入请求必须为 0: %q", docs)
			}
			reason := lastSemanticError(t, current, f.root)
			if !strings.Contains(res.DegradedReason, "semantic-migration-blocked") || !strings.Contains(reason, tc.want) || !strings.Contains(reason, target) || !strings.Contains(reason, "permission denied") {
				t.Fatalf("应带出无法检查的路径与实际错误: degraded=%q reason=%s", res.DegradedReason, reason)
			}
			t.Logf("%s document_requests=0 reason=%s", tc.name, reason)
		})
	}
}

// TestCompleteRevisionStillHonoursSiblingBatchJobs：新子树已经完整之后，再加回
// 旧 Batch 作业登记过的内容，delta 与离线计划都不得为它送审；提交意向（键
// 集合未知）存在时，任何缺口都不送审。人工移走状态文件后才补齐，每段输入
// 只送审一次。
func TestCompleteRevisionStillHonoursSiblingBatchJobs(t *testing.T) {
	ctx := context.Background()
	const restored = "def restored_job():\n    return 'held by old batch job'\n"
	f := newGuardFixture(t, "zz_restored.py", restored, "restored_job")
	f.writeBulkJobs(t, bulkState{
		ProfileHash: f.profileHash,
		Jobs:        []embedding.BulkJob{{ID: "batch-old", InputFileID: "file-old", Keys: f.failedKeys, SubmittedAt: time.Now().UTC()}},
	})
	if err := os.Remove(filepath.Join(f.root, "zz_restored.py")); err != nil {
		t.Fatal(err)
	}
	current, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	callsBefore := f.server.callCount()
	migrated, err := current.Sync(ctx, syncRequest(f.root))
	if err != nil || migrated.SemanticCoverage != "100%" || len(documentTexts(f.server, callsBefore)) != 0 {
		t.Fatalf("前置：不含旧作业内容的语料应零送审迁移到完整覆盖: %+v err=%v", migrated, err)
	}

	writeFixture(t, f.root, "zz_restored.py", restored)
	exported := 0
	_, planErr := current.PlanEmbedJobs(ctx, engine.WorkspaceRef{DirectoryPath: f.root}, func(EmbedJob) error {
		exported++
		return nil
	})
	if planErr == nil || exported != 0 || !strings.Contains(planErr.Error(), "Batch") {
		t.Fatalf("离线计划不得导出旧作业已登记的内容: exported=%d err=%v", exported, planErr)
	}
	delta, err := current.Sync(ctx, syncRequest(f.root))
	if err != nil || delta.BuildMode != "delta" {
		t.Fatalf("恢复内容应走 delta: %+v err=%v", delta, err)
	}
	if docs := documentTexts(f.server, callsBefore); len(docs) != 0 {
		t.Fatalf("旧作业已登记的内容不得再次送审: %q", docs)
	}
	if !strings.Contains(delta.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("delta 结果应带阻止原因: %+v", delta)
	}

	// 只有提交意向（键集合未知）：新增一段与旧作业无关的内容也不送审。
	f.writeBulkJobs(t, bulkState{
		ProfileHash:   f.profileHash,
		PendingIntent: &bulkIntent{KeysSHA256: "unknown", Count: 3, CreatedAt: time.Now().UTC()},
	})
	writeFixture(t, f.root, "zz_fresh.py", "def fresh_content():\n    return 'never submitted'\n")
	intentDelta, err := current.Sync(ctx, syncRequest(f.root))
	if err != nil || !strings.Contains(intentDelta.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("提交意向存在时 delta 应被阻止: %+v err=%v", intentDelta, err)
	}
	if docs := documentTexts(f.server, callsBefore); len(docs) != 0 {
		t.Fatalf("提交意向存在时不得送审任何缺口: %q", docs)
	}
	if reason := lastSemanticError(t, current, f.root); !strings.Contains(reason, "提交意向") || !strings.Contains(reason, f.bulkPath()) {
		t.Fatalf("阻止原因应指出提交意向与状态文件: %s", reason)
	}

	// 用户到 provider 控制台核对后人工移走状态文件。
	if err := os.Remove(f.bulkPath()); err != nil {
		t.Fatal(err)
	}
	resumed, err := current.Sync(ctx, syncRequest(f.root))
	if err != nil || resumed.SemanticCoverage != "100%" {
		t.Fatalf("移走状态文件后应补齐: %+v err=%v", resumed, err)
	}
	sent := map[string]int{}
	for _, text := range documentTexts(f.server, callsBefore) {
		sent[text]++
	}
	for text, count := range sent {
		if count != 1 {
			t.Fatalf("同一输入被送审 %d 次: %q", count, text)
		}
	}
	if len(sent) != 2 {
		t.Fatalf("恢复后只应送审 restored.py 与 fresh.py 各一次: %v", sent)
	}
	t.Logf("complete_revision_batch_guard plan_exported=%d blocked_delta_documents=0 intent_delta_documents=0 after_recovery_unique_inputs=%d", exported, len(sent))
}

// writeRawBulkJob 把只含一个作业的批作业状态按原始 JSON 写进旧子树，keysMode
// 为 absent 时不写 keys 字段，为 null 时写 null，其余写 keys 列表；jobs 为
// false 时写一个没有作业的合法状态。返回写入的字节。
func writeRawBulkJob(t *testing.T, f guardFixture, jobs bool, keysMode string, keys []string) []byte {
	t.Helper()
	state := map[string]any{"profile_hash": f.profileHash, "jobs": []any{}}
	if jobs {
		job := map[string]any{"id": "batch-unknown", "input_file_id": "file-unknown", "submitted_at": time.Now().UTC().Format(time.RFC3339)}
		switch keysMode {
		case "absent":
		case "null":
			job["keys"] = nil
		default:
			job["keys"] = keys
		}
		state["jobs"] = []any{job}
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.bulkPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// TestSiblingBatchJobWithUnknownKeysBlocksSends：旧子树登记了 Batch 作业，但作业
// 的 keys 缺失、为 null、为空列表，或其中有不是 embedKey 格式（64 位小写
// 十六进制）的项时，无法确定这个作业覆盖了哪些已付费输入。新子树的全量迁移、
// 随后的 delta 与离线计划都不得发出文档嵌入请求，同步结果带
// semantic-migration-blocked，状态文件逐字节保留。对照组保持原行为：键集合
// 完整且覆盖缺口时，迁移与离线计划受阻，之后只新增无关内容的 delta 只为
// 这段新内容送审一次；只含无关的有效键、或没有作业时照常送审，每段输入
// 只送一次。
func TestSiblingBatchJobWithUnknownKeysBlocksSends(t *testing.T) {
	ctx := context.Background()
	validUnrelated := strings.Repeat("ab", 32)
	cases := []struct {
		name     string
		jobs     bool
		keysMode string
		keys     func(f guardFixture) []string
		blocked  bool
		detail   string
		// deltaSendsNew 为 true 时，键集合可确定且不含新内容，delta 照常为新内容
		// 送审一次。
		deltaSendsNew bool
	}{
		{"keys 字段缺失", true, "absent", nil, true, "keys 缺失或为空", false},
		{"keys 为 null", true, "null", nil, true, "keys 缺失或为空", false},
		{"keys 为空列表", true, "list", func(guardFixture) []string { return []string{} }, true, "keys 缺失或为空", false},
		{"keys 含空串", true, "list", func(guardFixture) []string { return []string{""} }, true, "第 1 个键", false},
		{"keys 不是十六进制", true, "list", func(guardFixture) []string { return []string{"not-an-embed-key"} }, true, "第 1 个键", false},
		{"keys 是大写十六进制", true, "list", func(f guardFixture) []string { return []string{strings.ToUpper(f.failedKeys[0])} }, true, "第 1 个键", false},
		{"keys 少一位", true, "list", func(f guardFixture) []string { return []string{f.failedKeys[0][:63]} }, true, "第 1 个键", false},
		{"有效键后跟一个异常项", true, "list", func(guardFixture) []string { return []string{validUnrelated, "bad"} }, true, "第 2 个键", false},
		{"对照：完整键集合覆盖缺口", true, "list", func(f guardFixture) []string { return f.failedKeys }, true, "尚未回收结果的 Batch 作业", true},
		{"对照：只含无关的有效键", true, "list", func(guardFixture) []string { return []string{validUnrelated} }, false, "", false},
		{"对照：没有作业", false, "", nil, false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "", "", "parse_config")
			var keys []string
			if tc.keys != nil {
				keys = tc.keys(f)
			}
			written := writeRawBulkJob(t, f, tc.jobs, tc.keysMode, keys)
			current, err := New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close(ctx) }()
			callsBefore := f.server.callCount()
			res, err := current.Sync(ctx, syncRequest(f.root))
			if err != nil {
				t.Fatalf("默认 allow 下同步不应失败: %v", err)
			}
			docs := documentTexts(f.server, callsBefore)
			if !tc.blocked {
				if len(docs) != 1 || !strings.Contains(docs[0], "parse_config") || res.SemanticCoverage != "100%" || res.DegradedReason != "" {
					t.Fatalf("对照组应照常为缺口送审一次并补齐: docs=%q res=%+v", docs, res)
				}
				t.Logf("%s document_requests=%d coverage=%s", tc.name, len(docs), res.SemanticCoverage)
				return
			}
			if len(docs) != 0 || !strings.Contains(res.DegradedReason, "semantic-migration-blocked") {
				t.Fatalf("迁移不得发出文档嵌入请求且应带阻止原因: docs=%q res=%+v", docs, res)
			}
			reason := lastSemanticError(t, current, f.root)
			for _, want := range []string{f.bulkPath(), tc.detail} {
				if !strings.Contains(reason, want) {
					t.Fatalf("阻止原因应包含 %q: %s", want, reason)
				}
			}
			exported := 0
			_, planErr := current.PlanEmbedJobs(ctx, engine.WorkspaceRef{DirectoryPath: f.root}, func(EmbedJob) error {
				exported++
				return nil
			})
			if planErr == nil || exported != 0 || !strings.Contains(planErr.Error(), tc.detail) {
				t.Fatalf("离线计划应报错且不导出任务: exported=%d err=%v", exported, planErr)
			}
			writeFixture(t, f.root, "extra.go", "package app\n\nfunc ExtraSession() {}\n")
			delta, err := current.Sync(ctx, syncRequest(f.root))
			if err != nil || delta.BuildMode != "delta" || !strings.Contains(delta.DegradedReason, "semantic-migration-blocked") {
				t.Fatalf("delta 应同样受阻: %+v err=%v", delta, err)
			}
			deltaDocs := documentTexts(f.server, callsBefore)
			if tc.deltaSendsNew {
				if len(deltaDocs) != 1 || !strings.Contains(deltaDocs[0], "ExtraSession") {
					t.Fatalf("键集合可确定时 delta 只应为无关的新内容送审一次: %q", deltaDocs)
				}
			} else if len(deltaDocs) != 0 {
				t.Fatalf("delta 不得发出文档嵌入请求: %q", deltaDocs)
			}
			if kept, err := os.ReadFile(f.bulkPath()); err != nil || string(kept) != string(written) {
				t.Fatalf("旧批作业状态必须逐字节保留: err=%v", err)
			}
			t.Logf("%s migration_documents=0 plan_exported=0 delta_documents=%d state_preserved=true", tc.name, len(deltaDocs))
		})
	}
}

// TestSiblingBatchJobWithUnknownKeysUnderDenyStrict：旧作业 keys 无法确定时，
// deny 与 strict 下同步返回带原因的错误，文档嵌入请求为 0。
func TestSiblingBatchJobWithUnknownKeysUnderDenyStrict(t *testing.T) {
	modes := []struct {
		name    string
		mutate  func(o *Options)
		wantEnv string
	}{
		{"deny", func(o *Options) { o.RetrievalDegrade = DegradeDeny }, EnvRetrievalDegrade},
		{"strict", func(o *Options) { o.QualityStrict = true }, EnvQualityStrict},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			f := newGuardFixture(t, "", "", "parse_config")
			writeRawBulkJob(t, f, true, "null", nil)
			opts := f.opts
			mode.mutate(&opts)
			current, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close(ctx) }()
			callsBefore := f.server.callCount()
			_, syncErr := current.Sync(ctx, syncRequest(f.root))
			if docs := documentTexts(f.server, callsBefore); len(docs) != 0 {
				t.Fatalf("%s: 文档嵌入请求必须为 0: %q", mode.name, docs)
			}
			if syncErr == nil || !strings.Contains(syncErr.Error(), mode.wantEnv) || !strings.Contains(syncErr.Error(), "keys 缺失或为空") || !strings.Contains(syncErr.Error(), f.bulkPath()) {
				t.Fatalf("%s 下同步应返回带原因的错误: %v", mode.name, syncErr)
			}
			t.Logf("%s document_requests=0 sync_error=%q", mode.name, syncErr.Error())
		})
	}
}

// activeManifestJSON 返回工作区 active revision 的 manifest 文件原文。
func activeManifestJSON(t *testing.T, e *Engine, root string) string {
	t.Helper()
	manifest, store := loadActiveManifest(t, e, root)
	data, err := os.ReadFile(filepath.Join(store.Root(), "manifests", manifest.Revision+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// heldJobFixture 建出旧 Batch 作业登记了 util.py、新子树迁移受阻的状态。
func heldJobFixture(t *testing.T) guardFixture {
	t.Helper()
	f := newGuardFixture(t, "", "", "parse_config")
	f.writeBulkJobs(t, bulkState{
		ProfileHash: f.profileHash,
		Jobs:        []embedding.BulkJob{{ID: "batch-held", InputFileID: "file-held", Keys: f.failedKeys, SubmittedAt: time.Now().UTC()}},
	})
	return f
}

// parseConfigTexts 统计自 fromCall 起送审的 util.py（parse_config）文档文本数。
func parseConfigTexts(f guardFixture, fromCall int) int {
	n := 0
	for _, text := range documentTexts(f.server, fromCall) {
		if strings.Contains(text, "parse_config") {
			n++
		}
	}
	return n
}

// TestMigrationBlockSurvivesRestart：迁移暂停原因随 revision 持久化。重启后
// 不等任何同步：workspace_status 的 last_error 给出状态文件路径与恢复办法；
// 首次查询走首触快路径服务已有 revision，同时带 semantic-migration-blocked；
// 重启后第一次构建尚未结束时（跟踪器存在但还没就绪）状态同样可见。全程
// 文档嵌入请求为 0。
func TestMigrationBlockSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	f := heldJobFixture(t)
	first, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := first.Sync(ctx, syncRequest(f.root)); err != nil || !strings.Contains(res.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("前置：迁移应受阻: %+v err=%v", res, err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	callsBefore := f.server.callCount()

	second := restartedEngine(t, f.opts)
	if reason := lastSemanticError(t, second, f.root); !strings.Contains(reason, f.bulkPath()) || !strings.Contains(reason, "恢复") {
		t.Fatalf("重启后状态应直接给出暂停原因与恢复办法: %q", reason)
	}

	third := restartedEngine(t, f.opts)
	root, workspaceKey, err := third.resolveRoot(f.root)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟重启后第一次构建正在进行：跟踪器已建立但还没有就绪过。
	third.statusFor(root, workspaceKey).begin()
	if reason := lastSemanticError(t, third, f.root); !strings.Contains(reason, f.bulkPath()) {
		t.Fatalf("重启后构建进行中，状态也应给出暂停原因: %q", reason)
	}

	fourth := restartedEngine(t, f.opts)
	result, err := fourth.Search(ctx, searchRequest(f.root, "HandleLogin"))
	if err != nil {
		t.Fatalf("重启后首次查询应可用: %v", err)
	}
	if !strings.Contains(result.DegradedReason, "index-refreshing") || !strings.Contains(result.DegradedReason, "semantic-migration-blocked") || !strings.Contains(result.Text, "semantic-migration-blocked") {
		t.Fatalf("首触查询应同时带 index-refreshing 与暂停原因: %q", result.DegradedReason)
	}
	waitFirstTouchConverged(t, fourth, f.root)
	if n := len(documentTexts(f.server, callsBefore)); n != 0 {
		t.Fatalf("重启后不得发出文档嵌入请求: %d", n)
	}
	t.Logf("restart document_requests=0 first_query_reason=%q", result.DegradedReason)
}

// TestUnrelatedDeltaKeepsMigrationBlockVisible：迁移受阻后新增一个无关文件，
// delta 只为新内容送审一次并提高覆盖率；暂停原因仍有效，同步结果、
// workspace_status 的 last_error 与重启后的状态都继续给出它；旧内容不发送。
func TestUnrelatedDeltaKeepsMigrationBlockVisible(t *testing.T) {
	ctx := context.Background()
	f := heldJobFixture(t)
	e, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	callsBefore := f.server.callCount()
	blocked, err := e.Sync(ctx, syncRequest(f.root))
	if err != nil || !strings.Contains(blocked.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("前置：迁移应受阻: %+v err=%v", blocked, err)
	}
	writeFixture(t, f.root, "extra.go", "package app\n\nfunc ExtraSession() {}\n")
	delta, err := e.Sync(ctx, syncRequest(f.root))
	if err != nil || delta.BuildMode != "delta" || !strings.Contains(delta.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("delta 结果应继续带暂停原因: %+v err=%v", delta, err)
	}
	if docs := documentTexts(f.server, callsBefore); len(docs) != 1 || !strings.Contains(docs[0], "ExtraSession") {
		t.Fatalf("delta 只应为无关的新内容送审一次: %q", docs)
	}
	if reason := lastSemanticError(t, e, f.root); !strings.Contains(reason, f.bulkPath()) || !strings.Contains(reason, "恢复") {
		t.Fatalf("无关 delta 之后 last_error 仍应给出暂停原因: %q", reason)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if reason := lastSemanticError(t, restarted, f.root); !strings.Contains(reason, f.bulkPath()) {
		t.Fatalf("重启后仍应给出暂停原因: %q", reason)
	}
	if n := parseConfigTexts(f, callsBefore); n != 0 {
		t.Fatalf("旧作业登记的内容不得发送: %d", n)
	}

	// 删除被暂停的 util.py：只删文件的 delta 让语义覆盖补齐，暂停原因随
	// 新 revision 清除，重启后也不再出现。
	if err := os.Remove(filepath.Join(f.root, "util.py")); err != nil {
		t.Fatal(err)
	}
	cleared, err := restarted.Sync(ctx, syncRequest(f.root))
	if err != nil || cleared.SemanticCoverage != "100%" || cleared.DegradedReason != "" {
		t.Fatalf("覆盖补齐后不应再带暂停原因: %+v err=%v", cleared, err)
	}
	if manifestJSON := activeManifestJSON(t, restarted, f.root); strings.Contains(manifestJSON, "semantic_migration_block") {
		t.Fatal("覆盖完整的 manifest 不应带暂停原因字段")
	}
	if err := restarted.Close(ctx); err != nil {
		t.Fatal(err)
	}
	again := restartedEngine(t, f.opts)
	if reason := lastSemanticError(t, again, f.root); strings.Contains(reason, "语义迁移已暂停") {
		t.Fatalf("覆盖补齐后重启不应再出现暂停原因: %q", reason)
	}
	t.Logf("unrelated_delta coverage %s -> %s -> %s (build %s) document_requests=%d util_py_requests=0", blocked.SemanticCoverage, delta.SemanticCoverage, cleared.SemanticCoverage, cleared.BuildMode, len(documentTexts(f.server, callsBefore)))
}

// TestMigrationBlockDiagnosticUpdatesAcrossRestart：只有诊断改变、覆盖没有
// 提高时也要更新持久化的原因。依次验证：暂停原因从 Batch 作业换成提交意向；
// 旧状态移走但 provider 失败、仍有其他语义缺口时原因清除；补齐覆盖后原因
// 不再出现，active manifest 也不带该字段。每一步都在重启后核对。
func TestMigrationBlockDiagnosticUpdatesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	f := heldJobFixture(t)
	syncWith := func(label string) (engine.Result, *Engine) {
		t.Helper()
		e, err := New(f.opts)
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.Sync(ctx, syncRequest(f.root))
		if err != nil {
			t.Fatalf("%s: 同步: %v", label, err)
		}
		return res, e
	}
	// restartReason 用新引擎模拟重启，读状态并做一次首触查询；引擎在返回前
	// 关闭，释放工作区写锁，下一步的引擎才能构建。
	restartReason := func() (string, engine.Result) {
		t.Helper()
		e, err := New(f.opts)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = e.Close(ctx) }()
		reason := lastSemanticError(t, e, f.root)
		result, err := e.Search(ctx, searchRequest(f.root, "HandleLogin"))
		if err != nil {
			t.Fatalf("重启后查询: %v", err)
		}
		waitFirstTouchConverged(t, e, f.root)
		return reason, result
	}
	callsBefore := f.server.callCount()

	first, e := syncWith("Batch 作业")
	if !strings.Contains(first.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("前置：应受阻: %+v", first)
	}
	_ = e.Close(ctx)

	// 换成提交意向：内容与覆盖都不变，只有原因改变。
	f.writeBulkJobs(t, bulkState{ProfileHash: f.profileHash, PendingIntent: &bulkIntent{KeysSHA256: "unknown", Count: 1, CreatedAt: time.Now().UTC()}})
	second, e := syncWith("提交意向")
	if !strings.Contains(second.DegradedReason, "semantic-migration-blocked") || second.SemanticCoverage != first.SemanticCoverage {
		t.Fatalf("应仍受阻且覆盖不变: %+v", second)
	}
	_ = e.Close(ctx)
	reason, result := restartReason()
	if !strings.Contains(reason, "提交意向") || strings.Contains(reason, "尚未回收结果的 Batch 作业") || !strings.Contains(result.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("重启后应是新的暂停原因: reason=%q degraded=%q", reason, result.DegradedReason)
	}

	if n := parseConfigTexts(f, callsBefore); n != 0 {
		t.Fatalf("暂停期间不得发送旧作业登记的内容: %d", n)
	}
	// 移走状态文件，但 provider 全部失败：暂停解除，覆盖仍不完整。
	if err := os.Remove(f.bulkPath()); err != nil {
		t.Fatal(err)
	}
	f.server.setFailWhen(func([]string) bool { return true })
	third, e := syncWith("暂停解除但 provider 失败")
	if strings.Contains(third.DegradedReason, "semantic-migration-blocked") || third.SemanticCoverage == "100%" {
		t.Fatalf("暂停解除后不应再带暂停原因，覆盖仍不完整: %+v", third)
	}
	_ = e.Close(ctx)
	reason, result = restartReason()
	if strings.Contains(reason, "语义迁移已暂停") || strings.Contains(result.DegradedReason, "semantic-migration-blocked") || !strings.Contains(result.DegradedReason, "semantic-coverage-partial") {
		t.Fatalf("重启后不应留下旧的暂停原因，其他缺口照常可见: reason=%q degraded=%q", reason, result.DegradedReason)
	}

	// provider 恢复，补齐覆盖。
	f.server.setFailWhen(nil)
	fourth, e := syncWith("补齐")
	if fourth.SemanticCoverage != "100%" || fourth.DegradedReason != "" {
		t.Fatalf("应补齐覆盖且没有降级原因: %+v", fourth)
	}
	if manifestJSON := activeManifestJSON(t, e, f.root); strings.Contains(manifestJSON, "semantic_migration_block") {
		t.Fatal("覆盖完整的 manifest 不应带暂停原因字段")
	}
	_ = e.Close(ctx)
	reason, result = restartReason()
	if strings.Contains(reason, "语义迁移已暂停") || strings.Contains(result.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("补齐后重启不应再出现暂停原因: reason=%q degraded=%q", reason, result.DegradedReason)
	}
	attempts := parseConfigTexts(f, callsBefore)
	t.Logf("diagnostic_updates util_py_requests_total=%d (only after the block was lifted: failed attempts while the provider was down plus the successful one)", attempts)
}

// TestNormalIndexesDoNotCarryMigrationBlock：零凭据纯词法索引和语义覆盖完整
// 的索引都不带暂停原因字段，同步结果、状态和检索也没有误报。
func TestNormalIndexesDoNotCarryMigrationBlock(t *testing.T) {
	ctx := context.Background()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	server := newEmbedServer(t, 8)
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"零凭据纯词法", Options{}},
		{"语义覆盖完整", embedOptions(server.ts.URL, 8, 8, "same-model")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newFixtureWorkspace(t)
			e, err := New(tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = e.Close(ctx) }()
			res, err := e.Sync(ctx, syncRequest(root))
			if err != nil || strings.Contains(res.DegradedReason, "semantic-migration-blocked") {
				t.Fatalf("同步不应带暂停原因: %+v err=%v", res, err)
			}
			if manifestJSON := activeManifestJSON(t, e, root); strings.Contains(manifestJSON, "semantic_migration_block") {
				t.Fatal("正常索引的 manifest 不应带暂停原因字段")
			}
			status, err := e.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: root})
			if err != nil || status.Semantic != nil && strings.Contains(status.Semantic.LastError, "语义迁移已暂停") {
				t.Fatalf("状态不应带暂停原因: %+v err=%v", status.Semantic, err)
			}
			result, err := e.Search(ctx, searchRequest(root, "HandleLogin"))
			if err != nil || strings.Contains(result.DegradedReason, "semantic-migration-blocked") {
				t.Fatalf("检索不应带暂停原因: %q err=%v", result.DegradedReason, err)
			}
		})
	}
}

// TestSiblingJournalKeysReusedForLaterEdits：旧 journal 里已付费、迁移时语料里
// 没有的内容，迁移完成后被加回，delta 从旧 journal 只读复用，不送审。
func TestSiblingJournalKeysReusedForLaterEdits(t *testing.T) {
	ctx := context.Background()
	const content = "def journaled_only():\n    return 'paid but unpublished'\n"
	f := newGuardFixture(t, "zz_journaled.py", content, "journaled_only")
	journal, err := index.OpenJournal(f.legacyStore, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range f.failedRecords {
		vec := fakeVector(8, embedDocText(record))
		if err := vector.Normalize(vec); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(map[string][]float32{embedKey(record): vec}); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.root, "zz_journaled.py")); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, f.legacyStore.Root())
	current, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close(ctx) }()
	callsBefore := f.server.callCount()
	if res, err := current.Sync(ctx, syncRequest(f.root)); err != nil || res.SemanticCoverage != "100%" {
		t.Fatalf("前置：迁移应完整覆盖: %+v err=%v", res, err)
	}
	writeFixture(t, f.root, "zz_journaled.py", content)
	delta, err := current.Sync(ctx, syncRequest(f.root))
	if err != nil || delta.BuildMode != "delta" || delta.SemanticCoverage != "100%" || delta.DegradedReason != "" {
		t.Fatalf("加回的内容应从旧 journal 复用并补齐: %+v err=%v", delta, err)
	}
	if delta.CrossProfileReused != len(f.failedKeys) {
		t.Fatalf("应从旧 journal 复用 %d 个键，实际 %d", len(f.failedKeys), delta.CrossProfileReused)
	}
	if docs := documentTexts(f.server, callsBefore); len(docs) != 0 {
		t.Fatalf("旧 journal 已付费的内容不得再次送审: %q", docs)
	}
	assertSameTree(t, "旧子树", before, snapshotTree(t, f.legacyStore.Root()))
	t.Logf("later_edit_journal_reuse document_requests=0 cross_profile_reused=%d", delta.CrossProfileReused)
}

// TestMigrationBlockVisibleUnderAllowDenyStrict：迁移受阻在三种配置下都可见。
// allow：每次同步（包括没有发布新 revision 的同步）与检索结果都带
// semantic-migration-blocked；deny 与 strict：同步与检索返回带原因的错误。
// 三种配置下文档嵌入请求都是 0；allow 的检索另有查询嵌入请求，单独计数。
func TestMigrationBlockVisibleUnderAllowDenyStrict(t *testing.T) {
	modes := []struct {
		name    string
		mutate  func(o *Options)
		wantErr string
	}{
		{"allow", func(o *Options) {}, ""},
		{"deny", func(o *Options) { o.RetrievalDegrade = DegradeDeny }, EnvRetrievalDegrade},
		{"strict", func(o *Options) { o.QualityStrict = true }, EnvQualityStrict},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			f := newGuardFixture(t, "", "", "parse_config")
			f.writeBulkJobs(t, bulkState{
				ProfileHash: f.profileHash,
				Jobs:        []embedding.BulkJob{{ID: "batch-inflight", InputFileID: "file-inflight", Keys: f.failedKeys, SubmittedAt: time.Now().UTC()}},
			})
			opts := f.opts
			mode.mutate(&opts)
			current, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close(ctx) }()
			callsBefore := f.server.callCount()
			first, firstErr := current.Sync(ctx, syncRequest(f.root))
			second, secondErr := current.Sync(ctx, syncRequest(f.root))
			result, searchErr := current.Search(ctx, searchRequest(f.root, "HandleLogin"))
			if docs := documentTexts(f.server, callsBefore); len(docs) != 0 {
				t.Fatalf("%s: 文档嵌入请求必须为 0: %q", mode.name, docs)
			}
			queries := len(queryTexts(f.server, callsBefore))
			if mode.wantErr == "" {
				for i, res := range []engine.Result{first, second} {
					if i == 0 && firstErr != nil || i == 1 && secondErr != nil {
						t.Fatalf("allow 下同步不应失败: %v %v", firstErr, secondErr)
					}
					if !strings.Contains(res.DegradedReason, "semantic-migration-blocked") {
						t.Fatalf("allow 第 %d 次同步应带阻止原因: %+v", i+1, res)
					}
				}
				if searchErr != nil || !strings.Contains(result.DegradedReason, "semantic-migration-blocked") || !strings.Contains(result.Text, "semantic-migration-blocked") {
					t.Fatalf("allow 下检索应带阻止原因与横幅: err=%v reason=%q", searchErr, result.DegradedReason)
				}
				t.Logf("allow document_requests=0 query_requests=%d sync_reasons=%q|%q search_reason=%q", queries, first.DegradedReason, second.DegradedReason, result.DegradedReason)
				return
			}
			for i, err := range []error{firstErr, secondErr, searchErr} {
				if err == nil || !strings.Contains(err.Error(), mode.wantErr) || !strings.Contains(err.Error(), "semantic-migration-blocked") && !strings.Contains(err.Error(), "semantic migration blocked") {
					t.Fatalf("%s 下第 %d 个调用应返回带原因的错误: %v", mode.name, i+1, err)
				}
			}
			if !strings.Contains(firstErr.Error(), f.bulkPath()) {
				t.Fatalf("%s 下同步错误应带出状态文件路径: %v", mode.name, firstErr)
			}
			t.Logf("%s document_requests=0 query_requests=%d sync_error=%q search_error=%q", mode.name, queries, firstErr.Error(), searchErr.Error())
		})
	}
}
