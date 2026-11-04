package localengine

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// stat 快照让 daemon 重启后的第一次扫描跳过未变文件的重新哈希。改动前 stat 缓存只在
// 内存里，重启后第一次扫描要重新读并哈希全部文件（mailyn-cli 约 18 秒），所以首触查询
// 只能先用磁盘上的旧 revision 作答（index-refreshing），后台再扫描。现在：
//   - 每个索引子树目录下一份 stat-snapshot.json，只由持有该子树写锁的进程写入：构建
//     成功后条目有变化、且距上次写入满 statSnapshotInterval 时写一次，引擎关闭时写一次；
//   - stat 缓存首次创建时加载；归属或规则版本不符整份作废，单条内容哈希与 active manifest
//     不一致只丢这一条（workspace.LoadStatSnapshot）；
//   - 加载到条目的工作区，第一次检索走普通流程（扫描、按需 delta、有界等待），不再走首触
//     快路径；没有快照或整份作废时保持首触快路径。
// 快照丢失或损坏只多一次重新哈希，不影响正确性，也不属于索引格式。

// statSnapshotInterval 是同一工作区两次保存快照的最短间隔。进程崩溃最多丢最近这段时间的
// 条目，对应文件在下次启动后重新哈希。
const statSnapshotInterval = 10 * time.Minute

// statSnapshotState 是本进程对一个工作区快照的加载与保存记录，字段读写都在 e.mu 下。
type statSnapshotState struct {
	canonical    string
	status       string // loaded、ignored、absent
	reason       string
	loaded       int
	dropped      int
	savedGen     uint64
	savedAt      time.Time
	savedEntries int
	saveErr      string
}

func (e *Engine) statSnapshotMeta(canonical string) workspace.StatSnapshotMeta {
	return workspace.StatSnapshotMeta{Workspace: canonical, Subtree: e.storeProfile, ChunkerVersion: e.profile.Version}
}

// loadStatCache 为工作区建立 stat 缓存：有可用快照时装入其中的条目。没有可服务的
// revision 时快照无从核对，按不存在处理（首建本来就要读全部文件）。
func (e *Engine) loadStatCache(workspaceKey string, root pathutil.WorkspaceRoot) (*workspace.StatCache, *statSnapshotState) {
	state := &statSnapshotState{canonical: root.CanonicalPath, status: "absent"}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return workspace.NewStatCache(), state
	}
	manifest, _, err := store.ResolveUsable()
	if err != nil || manifest == nil {
		return workspace.NewStatCache(), state
	}
	keep := func(rel, blob string) bool {
		entry, ok := manifest.Files[rel]
		return ok && entry.ContentHash == blob
	}
	res := workspace.LoadStatSnapshot(filepath.Join(store.Root(), workspace.StatSnapshotFile), e.statSnapshotMeta(root.CanonicalPath), keep)
	switch {
	case res.Cache != nil:
		state.status, state.loaded, state.dropped = "loaded", res.Loaded, res.Dropped
		state.savedGen = res.Cache.Generation()
		return res.Cache, state
	case res.Ignored != "":
		state.status, state.reason = "ignored", res.Ignored
		if e.logf != nil {
			e.logf("stat snapshot %s ignored: %s", filepath.Join(store.Root(), workspace.StatSnapshotFile), res.Ignored)
		}
	}
	return workspace.NewStatCache(), state
}

// statSnapshotWarm 报告该工作区的 stat 缓存是否从快照装入了条目；需要时先建立缓存。
func (e *Engine) statSnapshotWarm(workspaceKey string, root pathutil.WorkspaceRoot) bool {
	e.statCacheFor(workspaceKey, root)
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.statSnapshots[workspaceKey]
	return state != nil && state.status == "loaded" && state.loaded > 0
}

// maybeSaveStatSnapshot 在本进程持有该工作区写锁时保存快照。条目自上次加载或保存以来
// 没有变化时不写；force 为 false 时距上次保存不满 statSnapshotInterval 也不写。lock 为
// nil 时取引擎登记的写锁（Close 摘除登记后显式传入）。写锁复验失败（已被其他进程接管）
// 不写，避免两个进程交替覆盖。保存失败记进状态，不影响构建结果。
func (e *Engine) maybeSaveStatSnapshot(workspaceKey string, lock *index.ProcessLock, force bool) {
	e.mu.Lock()
	cache := e.statCaches[workspaceKey]
	state := e.statSnapshots[workspaceKey]
	store := e.stores[workspaceKey]
	if lock == nil {
		lock = e.locks[workspaceKey]
	}
	var canonical string
	skip := cache == nil || state == nil || store == nil || lock == nil
	if !skip {
		canonical = state.canonical
		skip = cache.Generation() == state.savedGen ||
			(!force && !state.savedAt.IsZero() && time.Since(state.savedAt) < statSnapshotInterval)
	}
	e.mu.Unlock()
	if skip || lock.Verify() != nil {
		return
	}
	now := time.Now()
	saved, err := cache.SaveSnapshot(filepath.Join(store.Root(), workspace.StatSnapshotFile), e.statSnapshotMeta(canonical), now)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		state.saveErr = err.Error()
		return
	}
	state.savedGen, state.savedAt, state.savedEntries, state.saveErr = saved.Generation, now, saved.Entries, ""
}

// statSnapshotSummary 是 workspace_status 的 stat_snapshot 字段；本进程还没为该工作区
// 建立 stat 缓存时返回空串。
func (e *Engine) statSnapshotSummary(workspaceKey string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.statSnapshots[workspaceKey]
	if state == nil {
		return ""
	}
	var text string
	switch state.status {
	case "loaded":
		text = fmt.Sprintf("loaded %d entries", state.loaded)
		if state.dropped > 0 {
			text += fmt.Sprintf(", dropped %d (content hash differs from the active revision)", state.dropped)
		}
	case "ignored":
		text = "ignored: " + state.reason
	default:
		text = "absent at startup"
	}
	if !state.savedAt.IsZero() {
		text += fmt.Sprintf("; saved %d entries at %s", state.savedEntries, state.savedAt.UTC().Format(time.RFC3339))
	}
	if state.saveErr != "" {
		text += "; last save failed: " + state.saveErr
	}
	return text
}
