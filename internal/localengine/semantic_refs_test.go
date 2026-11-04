package localengine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// refsWorkspace 建一个有 n 个小 Go 文件的工作区，每个文件一个函数。
func refsWorkspace(t *testing.T, n int) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("package main\n\nfunc refs%02d() int { return %d }\n", i, i)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("r%02d.go", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestEmbedRecordsReferencesJournalForNewVectors：首建时本次新取的向量都以 journal
// 引用进入写分段的组装结果，组装结果里不带向量本身。改动前新向量同时留在复用表与
// 组装结果里，直到写完分段（mailyn-cli 一半副本约 700 MiB）。
func TestEmbedRecordsReferencesJournalForNewVectors(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 4, "refs"))
	root := refsWorkspace(t, 60)
	rows, inMemory, inShared := -1, 0, 0
	segmentWriteHook = func(_ context.Context, seman semanticOutcome) error {
		rows = len(seman.refs)
		for _, ref := range seman.refs {
			if ref.mem != nil {
				inMemory++
			}
			if ref.shared != nil {
				inShared++
			}
		}
		return nil
	}
	t.Cleanup(func() { segmentWriteHook = nil })
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if rows != manifest.Counts.Chunks || rows == 0 {
		t.Fatalf("写分段的行数 %d 应等于 chunk 数 %d", rows, manifest.Counts.Chunks)
	}
	if inMemory != 0 || inShared != 0 {
		t.Fatalf("首建的新向量应全部按 journal 引用：内存 %d 行、共享库 %d 行", inMemory, inShared)
	}
}

// TestPublishedVectorsMatchProviderOutput：发布的 vectors.dat 与“把 provider 返回的
// 向量归一化后按记录顺序调用 vector.Write”的产物逐字节相同。
func TestPublishedVectorsMatchProviderOutput(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 4, "refs-bits"))
	root := refsWorkspace(t, 30)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	manifest, store := loadActiveManifest(t, e, root)
	segment := manifest.NewestSegment()
	ix, err := vector.Load(store.SegmentPathFor(segment.ID), dim, segment.VectorsChecksum, segment.VectorsIndexChecksum)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	byFile, err := loadLiveChunkRecordsByFile(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	textByKey := map[string]string{}
	for _, records := range byFile {
		for _, record := range records {
			textByKey[embedKey(record)] = embedDocText(record)
		}
	}
	entries := ix.Entries()
	vectors := make([][]float32, len(entries))
	for i, entry := range entries {
		text, ok := textByKey[entry.ContentHash]
		if !ok {
			t.Fatalf("第 %d 行的键找不到对应记录", i)
		}
		vec := fakeVector(dim, text)
		if err := vector.Normalize(vec); err != nil {
			t.Fatal(err)
		}
		vectors[i] = vec
	}
	dataSum, idxSum, err := vector.Write(t.TempDir(), dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	if dataSum != segment.VectorsChecksum || idxSum != segment.VectorsIndexChecksum {
		t.Fatalf("发布的向量文件与 provider 输出不一致: data %s/%s idx %s/%s", segment.VectorsChecksum, dataSum, segment.VectorsIndexChecksum, idxSum)
	}
}

// corruptJournalAfterAbortedWrite 让第一次同步在写分段前失败（journal 保留全部新
// 向量），改坏 journal 第一条记录的向量字节，再同步一次：读回校验失败，同步失败、
// 不发布、不送审，错误写明 journal 文件与键。返回被改坏的键与第二次同步前的请求数。
func corruptJournalAfterAbortedWrite(t *testing.T, e *Engine, server *embedServer, root string) (key string, callsBefore int) {
	t.Helper()
	abort := errors.New("模拟写分段前失败")
	segmentWriteHook = func(context.Context, semanticOutcome) error { return abort }
	t.Cleanup(func() { segmentWriteHook = nil })
	if _, err := e.Sync(context.Background(), syncRequest(root)); err == nil || !strings.Contains(err.Error(), abort.Error()) {
		t.Fatalf("第一次同步应在写分段前失败: %v", err)
	}
	segmentWriteHook = nil

	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := e.journalFor(workspaceKey, store)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Len() == 0 {
		t.Fatal("写分段前失败时 journal 应保留本次新取的向量")
	}
	path := filepath.Join(store.JournalDir(), "vectors.journal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hashLen := int(binary.LittleEndian.Uint16(data[4:6]))
	key = string(data[6 : 6+hashLen])
	data[6+hashLen+4+1] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	callsBefore = server.callCount()
	if _, err := e.Sync(context.Background(), syncRequest(root)); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), key) {
		t.Fatalf("读回校验失败应让同步失败并写明 journal 文件与键: %v", err)
	}
	if server.callCount() != callsBefore {
		t.Fatal("journal 里有全部向量时第二次同步不应送审")
	}
	if manifest, _ := loadActiveManifest(t, e, root); manifest.VectorCount != 0 {
		t.Fatalf("读回失败的构建不应发布: vectors=%d", manifest.VectorCount)
	}
	if journal.Has(key) {
		t.Fatal("读回校验失败的键应移出 journal")
	}
	return key, callsBefore
}

// TestSegmentWriteJournalCorruptionReembedsOnlyThatKey：journal 记录读回校验失败、
// 共享向量库里也没有同键记录时，下一次同步只为这个键送审 1 条并发布完整覆盖，
// journal 随后清空。
func TestSegmentWriteJournalCorruptionReembedsOnlyThatKey(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 4, "refs-corrupt"))
	root := refsWorkspace(t, 20)
	_, callsBefore := corruptJournalAfterAbortedWrite(t, e, server, root)
	sharedDir, err := e.sharedVectorsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sharedDir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("第三次同步: %v", err)
	}
	if sent := server.textsSince(callsBefore); len(sent) != 1 {
		t.Fatalf("只应为校验失败的那个键重新送审 1 条，实际 %d 条", len(sent))
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("应发布完整覆盖: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
	_, workspaceKey, _ := e.resolveRoot(root)
	store, _ := e.storeFor(workspaceKey)
	journal, err := e.journalFor(workspaceKey, store)
	if err != nil || journal.Len() != 0 {
		t.Fatalf("发布后 journal 应清空: len=%d err=%v", journal.Len(), err)
	}
}

// TestSegmentWriteJournalCorruptionReusesSharedCopy：同一批向量写 journal 时也写进
// 了共享向量库；journal 那份读回校验失败后，下一次同步从共享库复用同键记录，
// 不再送审。
func TestSegmentWriteJournalCorruptionReusesSharedCopy(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 4, "refs-corrupt-shared"))
	root := refsWorkspace(t, 20)
	_, callsBefore := corruptJournalAfterAbortedWrite(t, e, server, root)
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("第三次同步: %v", err)
	}
	if server.callCount() != callsBefore || result.SharedReused != 1 {
		t.Fatalf("应从共享库复用这 1 个键、不送审: 新增请求 %d，共享复用 %d", server.callCount()-callsBefore, result.SharedReused)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("应发布完整覆盖: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
}

// TestSiblingJournalRewrittenBetweenPassesBlocks：旧子树 journal 第一遍校验时有缺口
// 需要的记录，第二遍转存前被别的进程改写（这里截成空文件）时，不为缺口送审，
// 同步结果带阻止原因，状态写明 journal 在读取期间被改写。
func TestSiblingJournalRewrittenBetweenPassesBlocks(t *testing.T) {
	ctx := context.Background()
	f := newGuardFixture(t, "", "", "parse_config")
	journal, err := index.OpenJournal(f.legacyStore, 8)
	if err != nil {
		t.Fatal(err)
	}
	vec := fakeVector(8, f.failedKeys[0])
	if err := vector.Normalize(vec); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(map[string][]float32{f.failedKeys[0]: vec}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journalFile := filepath.Join(f.legacyStore.JournalDir(), "vectors.journal")
	siblingSecondPassHook = func(dir string) {
		if err := os.Truncate(journalFile, 0); err != nil {
			t.Errorf("模拟改写旧 journal: %v", err)
		}
	}
	t.Cleanup(func() { siblingSecondPassHook = nil })
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
		t.Fatalf("旧 journal 在读取期间被改写时文档嵌入请求必须为 0: %q", docs)
	}
	if !strings.Contains(res.DegradedReason, "semantic-migration-blocked") {
		t.Fatalf("同步结果应带阻止原因: %+v", res)
	}
	if reason := lastSemanticError(t, current, f.root); !strings.Contains(reason, "在读取期间被改写") || !strings.Contains(reason, f.legacyStore.JournalDir()) {
		t.Fatalf("阻止原因应写明 journal 在读取期间被改写: %s", reason)
	}
}

// TestSegmentWriteCancelKeepsJournal：写分段期间引擎关闭、构建被取消，journal 保留
// 全部新向量；新引擎再同步不发文档请求即发布完整覆盖。
func TestSegmentWriteCancelKeepsJournal(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	opts := embedOptions(server.ts.URL, dim, 4, "refs-cancel")
	root := refsWorkspace(t, 20)
	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	segmentWriteHook = func(ctx context.Context, _ semanticOutcome) error {
		go func() { closed <- first.Close(context.Background()) }()
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { segmentWriteHook = nil })
	if _, err := first.Sync(context.Background(), syncRequest(root)); err == nil {
		t.Fatal("写分段期间被取消的构建不应成功")
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	segmentWriteHook = nil

	callsBefore := server.callCount()
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	if _, err := second.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("恢复构建: %v", err)
	}
	if server.callCount() != callsBefore {
		t.Fatalf("journal 里已付费的向量不应重新送审: 新增 %d 次请求", server.callCount()-callsBefore)
	}
	manifest, _ := loadActiveManifest(t, second, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("恢复后应全覆盖: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
}
