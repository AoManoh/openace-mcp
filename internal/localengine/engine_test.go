package localengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
)

// fixtureMainGo 是测试仓库的主文件；HandleLogin 的行号被断言，
// 修改本常量需同步更新相关断言与 golden。
const fixtureMainGo = `package app

import "fmt"

// HandleLogin 校验凭据并建立会话。
func HandleLogin(user string, password string) error {
	if user == "" || password == "" {
		return fmt.Errorf("missing credentials")
	}
	return establishSession(user)
}

// establishSession 建立用户会话。
func establishSession(user string) error {
	fmt.Println("session for", user)
	return nil
}
`

const fixtureUtilPy = `def parse_config(path):
    """Parse the configuration file."""
    with open(path) as handle:
        return handle.read()
`

const fixtureReadme = `# Demo App

This demo application handles user login and configuration parsing.
`

// newFixtureWorkspace 构建含敏感文件的测试仓库（敏感文件必须被 AssetPolicy 拦截）。
func newFixtureWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, "main.go", fixtureMainGo)
	writeFixture(t, root, "util.py", fixtureUtilPy)
	writeFixture(t, root, "README.md", fixtureReadme)
	writeFixture(t, root, ".env", "SECRET_TOKEN=super-secret-value-canary\n")
	writeFixture(t, root, "id_rsa", "-----BEGIN OPENSSH PRIVATE KEY-----\ncanary-private-key\n-----END OPENSSH PRIVATE KEY-----\n")
	return root
}

func writeFixture(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newTestEngine 隔离 cache 目录并返回纯词法引擎（Stage 2 行为）。
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return newTestEngineWith(t, Options{})
}

// newTestEngineWith 隔离 cache 目录并按 opts 构造引擎。
func newTestEngineWith(t *testing.T, opts Options) *Engine {
	t.Helper()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

func syncRequest(dir string) engine.SyncRequest {
	return engine.SyncRequest{Workspace: engine.WorkspaceRef{DirectoryPath: dir}}
}

func searchRequest(dir string, query string) engine.SearchRequest {
	return engine.SearchRequest{Workspace: engine.WorkspaceRef{DirectoryPath: dir}, Query: query}
}

func TestSyncFullThenNoop(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("首次 sync: %v", err)
	}
	if first.Engine != EngineID || first.IndexRevision == "" {
		t.Fatalf("结果应携带引擎与 revision: %+v", first)
	}
	if first.FileCount != 3 {
		t.Fatalf("应索引 3 个文件（敏感文件被过滤），got %d", first.FileCount)
	}
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("重复 sync: %v", err)
	}
	if second.IndexRevision != first.IndexRevision {
		t.Fatalf("无变更 sync 应为 no-op：%s vs %s", second.IndexRevision, first.IndexRevision)
	}
}

func TestSyncIncrementalCreatesNewRevisionKeepsPrevious(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "util.py", fixtureUtilPy+"\ndef reload_config():\n    return parse_config('/etc/app.conf')\n")
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if second.IndexRevision == first.IndexRevision {
		t.Fatal("内容变化后应产生新 revision")
	}
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := store.LoadManifest(first.IndexRevision)
	if err != nil {
		t.Fatalf("previous revision 应保留: %v", err)
	}
	if err := store.VerifyManifest(previous); err != nil {
		t.Fatalf("previous segment 应完整: %v", err)
	}
}

// TestDenylistedFilesNeverIndexed 是暗坑 K4 的业务验收：
// 敏感文件内容在索引中零命中。
func TestDenylistedFilesNeverIndexed(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"super-secret-value-canary", "canary-private-key", "SECRET_TOKEN"} {
		result, err := e.Search(context.Background(), searchRequest(root, query))
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if strings.Contains(result.Text, "canary") || strings.Contains(result.Text, ".env") || strings.Contains(result.Text, "id_rsa") {
			t.Fatalf("敏感内容泄漏进索引: query=%q result=%q", query, result.Text)
		}
	}
}

func TestEmptyWorkspace(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("空仓库 sync 应成功: %v", err)
	}
	if result.FileCount != 0 {
		t.Fatalf("空仓库 file count 应为 0: %+v", result)
	}
	search, err := e.Search(context.Background(), searchRequest(root, "anything"))
	if err != nil {
		t.Fatalf("空仓库 search 应成功: %v", err)
	}
	if search.Text != noHitsText {
		t.Fatalf("空仓库应返回无命中文案: %q", search.Text)
	}
}

// TestSearchSymbolWithVerifiableLines 是 P2-T09 的业务验收：
// path:line 引用按行号打开即是该内容。
func TestSearchSymbolWithVerifiableLines(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "main.go:") {
		t.Fatalf("结果应引用 main.go: %q", result.Text)
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("结果应包含符号内容: %q", result.Text)
	}
	block := extractFirstBlock(t, result.Text)
	fileLines := strings.Split(strings.ReplaceAll(fixtureMainGo, "\r\n", "\n"), "\n")
	for i, line := range block.lines {
		fileIndex := block.start - 1 + i
		if fileIndex >= len(fileLines) {
			t.Fatalf("行号越界: block=%d+%d 文件共 %d 行", block.start, i, len(fileLines))
		}
		if fileLines[fileIndex] != line {
			t.Fatalf("第 %d 行内容不匹配:\nwant %q\ngot  %q", block.start+i, fileLines[fileIndex], line)
		}
	}
	if result.IndexRevision == "" || result.Engine != EngineID {
		t.Fatalf("结果应携带 revision 与 engine: %+v", result)
	}
}

type renderedBlock struct {
	path  string
	start int
	end   int
	lines []string
}

// extractFirstBlock 解析渲染文本的第一个块（格式由 golden 锁定）。
func extractFirstBlock(t *testing.T, text string) renderedBlock {
	t.Helper()
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "## ") {
		t.Fatalf("渲染格式异常: %q", text)
	}
	header := strings.TrimPrefix(lines[0], "## ")
	fields := strings.Fields(header)
	pathRange := strings.SplitN(fields[0], ":", 2)
	span := strings.SplitN(pathRange[1], "-", 2)
	block := renderedBlock{path: pathRange[0]}
	block.start = atoiOrFail(t, span[0])
	block.end = atoiOrFail(t, span[1])
	if !strings.HasPrefix(lines[1], "```") {
		t.Fatalf("缺少代码围栏: %q", lines[1])
	}
	for _, line := range lines[2:] {
		if strings.HasPrefix(line, "```") {
			break
		}
		block.lines = append(block.lines, line)
	}
	return block
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("非法数字 %q", s)
		}
		v = v*10 + int(r-'0')
	}
	return v
}

func TestProviderProfileRejected(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root, ProviderProfileID: "p1"}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err == nil || !strings.Contains(err.Error(), "provider_profile_id") {
		t.Fatalf("local-hybrid 应拒绝 provider_profile_id: %v", err)
	}
	if _, err := e.Search(context.Background(), engine.SearchRequest{Workspace: ref, Query: "q"}); err == nil {
		t.Fatal("search 也应拒绝 provider_profile_id")
	}
	if _, err := e.WorkspaceStatus(context.Background(), ref); err == nil {
		t.Fatal("status 也应拒绝 provider_profile_id")
	}
}

// TestSyncReportsDeltaAddedAndBuildMode(灰度反馈四 §6.1):Result.Added
// 此前对 delta 构建误报 revision 总 chunk 数(finishPublish 统一取
// manifest.Counts.Chunks)——灰度现场 1975 文件小改动被报成
// "Added=19028",被解读为"升级触发全量重嵌"虚惊。修复后 Added=本轮
// 实际写入的 chunk 数;BuildMode 显式说明构建形态与原因(他们的诉求:
// "重建时直说为什么",不再让调用方从进度条猜)。
func TestSyncReportsDeltaAddedAndBuildMode(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if first.BuildMode != "full:first-build" {
		t.Fatalf("首建 BuildMode 应为 full:first-build: %q", first.BuildMode)
	}
	if first.Added <= 0 {
		t.Fatalf("首建 Added 应为全部写入数: %+v", first.Added)
	}
	writeFixture(t, root, "tiny.go", "package app\n\n// Tiny 新增声明。\nfunc Tiny() {}\n")
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if second.BuildMode != "delta" {
		t.Fatalf("小变更应走 delta: %q", second.BuildMode)
	}
	if second.Added <= 0 || second.Added >= first.Added {
		t.Fatalf("delta Added 应为本轮写入数(远小于总量 %d): %d", first.Added, second.Added)
	}
	if !strings.Contains(second.Summary(), "build=delta") {
		t.Fatalf("Summary 应携带构建形态: %q", second.Summary())
	}
	// 无变更 no-op:不发布、Added=0、BuildMode 空。
	third, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if third.Added != 0 || third.BuildMode != "" || third.IndexRevision != second.IndexRevision {
		t.Fatalf("no-op 语义被破坏: %+v", third)
	}
}

// TestSearchCarriesStageTimings(框架 18.3/灰度候选 (e)):热检索 13.1s
// 无法归因是 sync、query embed、rerank 还是渲染——Result 恒带阶段耗时
// 分解(加性 omitempty 字段),延迟从此可归因。
func TestSearchCarriesStageTimings(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatal(err)
	}
	tm := res.Timings
	if tm == nil {
		t.Fatal("Result 应携带阶段耗时")
	}
	if tm.TotalMs < 0 || tm.SyncMs < 0 || tm.LexicalMs < 0 || tm.RenderMs < 0 {
		t.Fatalf("耗时字段不应为负: %+v", tm)
	}
	if tm.TotalMs < tm.LexicalMs || tm.TotalMs < tm.RenderMs {
		t.Fatalf("total 应覆盖各阶段: %+v", tm)
	}
	// 纯词法路径:query embed/vector/rerank 为零。
	if tm.QueryEmbedMs != 0 || tm.VectorMs != 0 || tm.RerankMs != 0 {
		t.Fatalf("纯词法路径语义阶段应为零: %+v", tm)
	}
}

// TestWorkspaceStatusTopLevelFileCounts(灰度反馈三 C.1 残余):状态面
// 按顶层目录给出现役索引文件计数,让 ignore 链的排除面可见——预期目录
// 计数为 0/缺失即选择面问题,使用方无需黑盒对照实验。根文件归 "."。
func TestWorkspaceStatusTopLevelFileCounts(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	writeFixture(t, root, "docs/plan.md", "# plan\n")
	writeFixture(t, root, "docs/adr/adr-001.md", "# adr\n")
	writeFixture(t, root, "src/app.go", "package app\n")
	// 被 gitignore 排除的目录不应出现在计数里(排除可见性的另一半:
	// 计数缺失即被排除的信号)。
	writeFixture(t, root, ".gitignore", "/private/\n")
	writeFixture(t, root, "private/secret.md", "# hidden\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	counts := status.TopLevelFileCounts
	if counts["docs"] != 2 || counts["src"] != 1 {
		t.Fatalf("顶层目录计数错误: %+v", counts)
	}
	if counts["."] < 3 { // main.go/util.py/README.md/.gitignore(敏感文件被拦截)
		t.Fatalf("根文件应归 \".\": %+v", counts)
	}
	if _, ok := counts["private"]; ok {
		t.Fatalf("被 ignore 的目录不应计数: %+v", counts)
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total != status.FileCount {
		t.Fatalf("计数总和(%d)应与 file_count(%d)一致", total, status.FileCount)
	}
}

// TestConcurrentSyncSingleBuild 是暗坑 K12 的验收：并发 sync 合并为一次构建。
func TestConcurrentSyncSingleBuild(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	const clients = 8
	var wg sync.WaitGroup
	revisions := make([]string, clients)
	errs := make([]error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			result, err := e.Sync(context.Background(), syncRequest(root))
			revisions[slot], errs[slot] = result.IndexRevision, err
		}(i)
	}
	wg.Wait()
	for i := 0; i < clients; i++ {
		if errs[i] != nil {
			t.Fatalf("并发 sync #%d 失败: %v", i, errs[i])
		}
		if revisions[i] != revisions[0] {
			t.Fatalf("并发 sync 应共享同一次构建: %v", revisions)
		}
	}
	_, workspaceKey, _ := e.resolveRoot(root)
	store, _ := e.storeFor(workspaceKey)
	manifest, _, err := store.ResolveUsable()
	if err != nil {
		t.Fatal(err)
	}
	if revisionCount(store, manifest) != 1 {
		t.Fatalf("8 并发 sync 只应产生 1 个 revision，got %d", revisionCount(store, manifest))
	}
}

// TestCancelledBuildDiscardsStaging 是暗坑 K16 的验收。
func TestCancelledBuildDiscardsStaging(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Sync(ctx, syncRequest(root)); err == nil {
		t.Fatal("已取消的 sync 应报错")
	}
	_, workspaceKey, _ := e.resolveRoot(root)
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("取消后 staging 应为空，剩余 %d 项", len(entries))
	}
}

func TestWorkspaceChangedDetection(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root}
	changed, err := e.WorkspaceChanged(context.Background(), ref)
	if err != nil || !changed {
		t.Fatalf("无索引时应视为已变化: changed=%v err=%v", changed, err)
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	changed, err = e.WorkspaceChanged(context.Background(), ref)
	if err != nil || changed {
		t.Fatalf("刚同步后应无变化: changed=%v err=%v", changed, err)
	}
	writeFixture(t, root, "main.go", fixtureMainGo+"\n// trailing comment\n")
	changed, err = e.WorkspaceChanged(context.Background(), ref)
	if err != nil || !changed {
		t.Fatalf("修改后应检测到变化: changed=%v err=%v", changed, err)
	}
}

func TestStatusLifecycle(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	ref := engine.WorkspaceRef{DirectoryPath: root}
	status, err := e.WorkspaceStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.Engine != EngineID || status.Stage != engine.IndexStageReady {
		t.Fatalf("状态应为 ready 且带引擎标识: %+v", status)
	}
	if status.FileCount != 3 || status.IndexRevision == "" {
		t.Fatalf("状态应带计数与 revision: %+v", status)
	}
	if !strings.Contains(status.UpstreamStatus, "go=ast") {
		t.Fatalf("能力上报应含 go=ast: %q", status.UpstreamStatus)
	}
	if !strings.Contains(status.UpstreamStatus, "markdown=fallback") {
		t.Fatalf("markdown 应如实上报 fallback: %q", status.UpstreamStatus)
	}
	statuses, err := e.ListWorkspaceStatuses(context.Background())
	if err != nil || len(statuses) != 1 {
		t.Fatalf("list 应返回 1 个工作区: %d err=%v", len(statuses), err)
	}
}

// TestHandleFallsBackToPreviousOnCorruption 是暗坑 K1/K11 的句柄级验收：
// active 损坏时 acquireHandle 直接回退 previous，不返回空结果。
func TestHandleFallsBackToPreviousOnCorruption(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "extra.go", "package app\n\nfunc Extra() {}\n")
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	_, workspaceKey, _ := e.resolveRoot(root)
	store, _ := e.storeFor(workspaceKey)
	active, err := store.LoadManifest(second.IndexRevision)
	if err != nil {
		t.Fatal(err)
	}
	// 破坏 active revision 的 chunks 数据。
	if err := os.WriteFile(filepath.Join(store.SegmentPath(active), "chunks.jsonl"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatalf("损坏时应回退 previous: %v", err)
	}
	defer e.releaseHandle(handle)
	if handle.manifest.Revision != first.IndexRevision {
		t.Fatalf("应回退到首个 revision %s，got %s", first.IndexRevision, handle.manifest.Revision)
	}
}

// TestSearchSelfHealsAfterCorruption 是暗坑 K1/K11 的检索侧验收：
// active 损坏时 Search 经 sync 自愈重建，仍返回正确结果，绝不空结果伪装成功。
func TestSearchSelfHealsAfterCorruption(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "extra.go", "package app\n\nfunc Extra() {}\n")
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	_, workspaceKey, _ := e.resolveRoot(root)
	store, _ := e.storeFor(workspaceKey)
	active, err := store.LoadManifest(second.IndexRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.SegmentPath(active), "chunks.jsonl"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("损坏后 Search 应自愈: %v", err)
	}
	if result.IndexRevision == second.IndexRevision {
		t.Fatal("不应继续使用已损坏的 revision")
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("自愈后检索应有结果: %q", result.Text)
	}
}

// TestClosedEngineRejects 是暗坑 K3 的生命周期验收。
func TestClosedEngineRejects(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Search(context.Background(), searchRequest(root, "HandleLogin")); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err == nil || !strings.Contains(err.Error(), "已关闭") {
		t.Fatalf("关闭后应拒绝检索: %v", err)
	}
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Search(context.Background(), searchRequest(root, "  ")); err == nil {
		t.Fatal("空查询应报错")
	}
}

func TestNoUsableRevisionSurfaced(t *testing.T) {
	e := newTestEngine(t)
	_, err := e.acquireHandle("nonexistent-ws")
	if !errors.Is(err, index.ErrNoUsableRevision) {
		t.Fatalf("无 revision 应返回 ErrNoUsableRevision，got %v", err)
	}
}

// TestFreshnessWindowSkipsInlineScan 新鲜度窗口业务语义:窗口内查询
// 不感知磁盘变更(跳过内联扫描),窗口过期后第一次查询恢复同步并
// 看到新内容;窗口=0(默认)保持每查询即时可见。
func TestFreshnessWindowSkipsInlineScan(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc OldToken() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{FreshnessWindow: 300 * time.Millisecond}
	e := newTestEngineWith(t, opts)
	ctx := context.Background()
	ref := engine.WorkspaceRef{DirectoryPath: root}
	if _, err := e.Sync(ctx, engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatal(err)
	}
	// 磁盘变更(窗口内):查询不应看到新 token。
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package a\nfunc FreshToken() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := e.Search(ctx, searchRequest(root, "FreshToken"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "FreshToken") {
		t.Fatalf("窗口内不应看到未同步内容: %q", result.Text)
	}
	// 窗口过期:恢复内联同步,新内容可检索。
	time.Sleep(350 * time.Millisecond)
	result, err = e.Search(ctx, searchRequest(root, "FreshToken"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "FreshToken") {
		t.Fatalf("窗口过期后应看到新内容: %q", result.Text)
	}
}
