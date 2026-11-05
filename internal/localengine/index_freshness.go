package localengine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// 用旧 revision 降级作答（index-building、index-refreshing、stale-index）时，正文在
// [DEGRADED] 横幅下一行写 [INDEX]：所用快照的 revision 与发布时刻，以及最近一次扫描
// 看到的、快照之后磁盘上有改动的文件。改动前结果只说明索引不新鲜，调用方不知道哪些
// 文件要直接读；10-22 那次 index-building 等满 40 秒后用快照作答，也没说快照有多旧。
//   - 清单在扫描完成后按相对当前可服务 revision 的新增、修改、删除记录，发布成功时
//     清空并改为相对新 revision；构建在扫描之后失败时保留，stale-index 作答时列出。
//   - 扫描没有完成就失败时清单作废：上一次扫描的结果已不能说明磁盘现状。
//   - 清单比对的 revision 与本次作答所用的不同（例如作答时回退到了更旧的 revision）
//     时写 pending=unknown，不拿别的 revision 的差异冒充。
//   - 只记录扫描时刻的状态：扫描之后的改动不在清单里，行里的 scanned= 写明这个时刻。

// pendingFilesLimit 是 [INDEX] 行与 index_freshness 列出的文件数上限；切分支时改动
// 可能有几千个文件，超出部分只给总数。
const pendingFilesLimit = 50

// PendingFile.Change 的取值。
const (
	pendingAdded    = "added"
	pendingModified = "modified"
	pendingDeleted  = "deleted"
)

// pendingScan 是最近一次完成的扫描相对 base revision 的改动文件。known 为 false 时
// 没有可用的清单；scannedAt 非零表示本次构建已完成扫描（首建没有可比的 revision，
// 只记时刻，词法中间发布或最终发布后改为相对新 revision 的空清单）。
type pendingScan struct {
	known     bool
	base      string
	scannedAt time.Time
	total     int
	files     []engine.PendingFile
}

// scanStartedHook 在构建记下扫描开始、开始遍历文件之前调用。只供测试在扫描进行中
// 停住构建，生产路径为 nil。
var scanStartedHook func()

// scanStarted 记下扫描开始的时刻，扫描完成或构建结束时清除。
func (s *wsStatus) scanStarted() {
	now := time.Now().UTC()
	s.mu.Lock()
	s.scanStartedAt = &now
	s.mu.Unlock()
}

// recordPending 在扫描完成后记录相对 previous（当前可服务的 revision）的改动文件：
// changed 与 removed 是 diffDeltaAssets 的结果。previous 为 nil（首建）时只记扫描时刻。
func (s *wsStatus) recordPending(previous *index.Manifest, changed []workspace.ContextAsset, removed []string) {
	scan := pendingScan{scannedAt: time.Now().UTC()}
	if previous != nil {
		files := make([]engine.PendingFile, 0, len(changed)+len(removed))
		for _, asset := range changed {
			change := pendingModified
			if _, ok := previous.Files[asset.RelPath]; !ok {
				change = pendingAdded
			}
			files = append(files, engine.PendingFile{Path: asset.RelPath, Change: change})
		}
		for _, path := range removed {
			files = append(files, engine.PendingFile{Path: path, Change: pendingDeleted})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		scan.known, scan.base, scan.total = true, previous.Revision, len(files)
		if len(files) > pendingFilesLimit {
			// 复制前 50 个，不让整份清单的底层数组一直留在内存里。
			files = append([]engine.PendingFile(nil), files[:pendingFilesLimit]...)
		}
		scan.files = files
	}
	s.mu.Lock()
	s.pending = scan
	s.scanStartedAt = nil
	s.mu.Unlock()
}

// pendingPublishedLocked 在 revision 发布或确认无需发布后调用（ready 与
// publishedInterim）：本次构建完成过扫描时，清单改为相对该 revision 为空；没有扫描
// 记录（重启后从磁盘恢复状态）时保持未知。调用方持有 s.mu。
func (s *wsStatus) pendingPublishedLocked(revision string) {
	s.scanStartedAt = nil
	if s.pending.scannedAt.IsZero() {
		return
	}
	s.pending = pendingScan{known: true, base: revision, scannedAt: s.pending.scannedAt}
}

// pendingFailedLocked 在构建失败时调用：扫描已完成时保留清单（失败的构建没有发布，
// 快照仍缺这些改动）；扫描没有完成时清单作废。调用方持有 s.mu。
func (s *wsStatus) pendingFailedLocked() {
	if s.scanStartedAt != nil {
		s.pending = pendingScan{}
		s.scanStartedAt = nil
	}
}

// isFreshnessReason 报告降级原因是否表示用旧 revision 作答。
func isFreshnessReason(reason string) bool {
	switch reason {
	case "index-building", "index-refreshing", "stale-index":
		return true
	}
	return false
}

func hasFreshnessGap(gaps []qualityGap) bool {
	for _, gap := range gaps {
		if isFreshnessReason(gap.reason) {
			return true
		}
	}
	return false
}

// indexFreshness 汇总本次作答所用 revision 与状态跟踪器里的扫描记录。
func (e *Engine) indexFreshness(workspaceKey string, manifest *index.Manifest) *engine.IndexFreshness {
	freshness := &engine.IndexFreshness{SnapshotRevision: manifest.Revision}
	if !manifest.ActivatedAt.IsZero() {
		published := manifest.ActivatedAt.UTC()
		freshness.PublishedAt = &published
	}
	e.mu.Lock()
	tracker := e.statuses[workspaceKey]
	e.mu.Unlock()
	if tracker == nil {
		return freshness
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if scan := tracker.pending; scan.known && scan.base == manifest.Revision {
		scanned := scan.scannedAt
		freshness.PendingKnown = true
		freshness.ScannedAt = &scanned
		freshness.PendingCount = scan.total
		freshness.PendingFiles = append([]engine.PendingFile(nil), scan.files...)
	} else if tracker.scanStartedAt != nil {
		started := *tracker.scanStartedAt
		freshness.ScanStartedAt = &started
	}
	return freshness
}

// indexLine 渲染 [INDEX] 行（不含换行）：
// "[INDEX] snapshot=<revision>[ published=<时刻>] <扫描情况>"，扫描情况为
// "scanned=<时刻> pending=<总数>[: <路径>, ...[ (+N more)]]"（删除的文件后缀
// " (deleted)"）、"scan_started=<时刻> pending=unknown" 或 "pending=unknown"。
// 时刻为 UTC 的 RFC 3339。
func indexLine(f *engine.IndexFreshness) string {
	var b strings.Builder
	b.WriteString("[INDEX] snapshot=")
	b.WriteString(f.SnapshotRevision)
	if f.PublishedAt != nil {
		b.WriteString(" published=")
		b.WriteString(f.PublishedAt.UTC().Format(time.RFC3339))
	}
	switch {
	case f.PendingKnown:
		if f.ScannedAt != nil {
			b.WriteString(" scanned=")
			b.WriteString(f.ScannedAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, " pending=%d", f.PendingCount)
		if len(f.PendingFiles) > 0 {
			names := make([]string, 0, len(f.PendingFiles))
			for _, file := range f.PendingFiles {
				name := file.Path
				if file.Change == pendingDeleted {
					name += " (deleted)"
				}
				names = append(names, name)
			}
			b.WriteString(": ")
			b.WriteString(strings.Join(names, ", "))
			if more := f.PendingCount - len(f.PendingFiles); more > 0 {
				fmt.Fprintf(&b, " (+%d more)", more)
			}
		}
	case f.ScanStartedAt != nil:
		b.WriteString(" scan_started=")
		b.WriteString(f.ScanStartedAt.UTC().Format(time.RFC3339))
		b.WriteString(" pending=unknown")
	default:
		b.WriteString(" pending=unknown")
	}
	return b.String()
}
