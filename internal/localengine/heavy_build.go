package localengine

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// 重型构建排队。同一台机器上的多个 openACE 进程（共享 daemon、预建或另起的 daemon）
// 原本互不感知，各自的大仓全量构建同时运行时内存叠加，可能把 WSL 推进 swap。现在：
//   - 选路之后、切分之前按内容字节估算本次构建的匿名内存峰值，不低于 1 GiB 为重型构建；
//   - 重型构建先取同一用户跨进程共享的名额（OPENACE_HEAVY_BUILD_SLOTS，默认 1，见
//     index.HeavyBuildSlots），名额被占时状态为 queued 并列出持有者，取得后才切分，
//     构建结束（含失败与取消）时释放；
//   - 有可服务 revision 的工作区，检索不为重型构建等待，先用已有索引作答
//     （index-building 加 [INDEX] 行，见 syncWorkspaceForQuery）；
//   - 名额目录不可用时不排队、照常构建，原因写进 workspace_status 的 heavy_build.error。
// 加锁顺序：先持有子树写锁（引擎生命周期内已持有），再取名额，没有反向依赖。

// heavyBuildPeakThreshold 是重型构建的估算峰值下限；var 仅为测试可调小。
var heavyBuildPeakThreshold int64 = 1 << 30

const (
	// estimatedChunkContentBytes 是每个 chunk 的平均内容字节数（lark-cli 742、mailyn-cli 858）。
	estimatedChunkContentBytes = 800
	// estimatedPeakBytesPerChunk 是构建期间每个 chunk 的匿名内存峰值（5.7 KiB）。第 8 步 B1
	// 之后新取的向量与复用来的向量都不再常驻内存：全部向量新取的首建实测约 5.5 KiB/chunk
	// （mailyn-cli 一半副本 188,146 个 chunk，1,007 MiB），复用向量的全量构建实测约
	// 5.7 KiB/chunk（E1），两者已接近，统一按较大的值估算。
	estimatedPeakBytesPerChunk = 5837
)

// buildEstimate 是一次构建开始切分前的规模估算：mode 为 full 或 delta，bytes 是要切分的
// 文件字节数（全量为全部文件，delta 为新增与修改的文件），peak 是估算的匿名内存峰值。
type buildEstimate struct {
	mode   string
	files  int
	bytes  int64
	chunks int64
	peak   int64
}

func (b buildEstimate) heavy() bool { return b.peak >= heavyBuildPeakThreshold }

// estimateBuild 估算一次构建的峰值：全量构建要读入并重写全部文件的 chunk（内容没变的文件
// 也一样），按 assets 计；delta 只切分新增与修改的文件，按 changed 计，删除不计。
func estimateBuild(delta bool, assets workspace.AssetSet, changed []workspace.ContextAsset) buildEstimate {
	files, mode := []workspace.ContextAsset(assets), "full"
	if delta {
		files, mode = changed, "delta"
	}
	est := buildEstimate{mode: mode, files: len(files)}
	for _, asset := range files {
		est.bytes += asset.Size
	}
	est.chunks = (est.bytes + estimatedChunkContentBytes - 1) / estimatedChunkContentBytes
	est.peak = est.chunks * estimatedPeakBytesPerChunk
	return est
}

// heavyBuildState 是在建重型构建的排队情况；state 为空表示没有在建的重型构建。
type heavyBuildState struct {
	state    string // queued 或 running
	since    time.Time
	estimate buildEstimate
	err      string
}

func (s *wsStatus) heavyQueued(est buildEstimate) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heavy = heavyBuildState{state: "queued", since: now, estimate: est}
	s.stage = engine.IndexStageQueued
	s.updatedAt = now
}

func (s *wsStatus) heavyRunning(est buildEstimate, errText string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heavy = heavyBuildState{state: "running", since: now, estimate: est, err: errText}
	s.updatedAt = now
}

// waitHeavySlot 在 est 属于重型构建时取名额，返回释放函数；不是重型构建时立即返回空操作。
// 名额被占时先把状态置为 queued 再等待；ctx 取消时返回 ctx 的错误。名额目录不可用时不排队，
// 状态写明原因，照常构建。
func (e *Engine) waitHeavySlot(ctx context.Context, status *wsStatus, root pathutil.WorkspaceRoot, est buildEstimate) (func(), error) {
	if !est.heavy() {
		return func() {}, nil
	}
	// 取得名额的时刻由 HeavyBuildSlots 追加（acquired=）。
	note := fmt.Sprintf("workspace=%s build=%s estimated_peak_mib=%d", root.CanonicalPath, est.mode, est.peak>>20)
	release, err := e.heavySlots.TryAcquire(note)
	if errors.Is(err, index.ErrHeavySlotsBusy) {
		status.heavyQueued(est)
		e.logf("heavy build queued: workspace=%s build=%s estimated_peak_mib=%d holders=%s",
			root.CanonicalPath, est.mode, est.peak>>20, strings.Join(e.heavyHolderLabels(), "; "))
		release, err = e.heavySlots.Acquire(ctx, note)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		text := sanitizeError(fmt.Errorf("重型构建名额不可用，本次不排队、直接构建: %w", err))
		status.heavyRunning(est, text)
		e.logf("%s", text)
		return func() {}, nil
	}
	status.heavyRunning(est, "")
	return func() {
		// 先把本次构建释放的堆还给操作系统，再放出名额。Go 运行时把释放的内存留着复用、
		// 只在后台慢慢归还：端到端里一个 daemon 刚建完约 1.8 GiB 的仓库、匿名内存还没降，
		// 另一个 daemon 随即取得名额开建，两份叠加让本机可用内存跌破 1.5 GiB。
		debug.FreeOSMemory()
		release()
	}, nil
}

// heavyHolderLabels 列出当前持有名额的进程号与说明。
func (e *Engine) heavyHolderLabels() []string {
	holders := e.heavySlots.Holders()
	labels := make([]string, 0, len(holders))
	for _, holder := range holders {
		labels = append(labels, fmt.Sprintf("pid=%d %s", holder.PID, holder.Note))
	}
	return labels
}

func (e *Engine) heavyStateOf(workspaceKey string) heavyBuildState {
	e.mu.Lock()
	tracker := e.statuses[workspaceKey]
	e.mu.Unlock()
	if tracker == nil {
		return heavyBuildState{}
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.heavy
}

// attachHeavyBuild 在有在建重型构建时填写 workspace_status 的 heavy_build。
func (e *Engine) attachHeavyBuild(status *engine.WorkspaceStatus, workspaceKey string) {
	heavy := e.heavyStateOf(workspaceKey)
	if heavy.state == "" {
		return
	}
	since := heavy.since
	status.HeavyBuild = &engine.HeavyBuildStatus{
		State: heavy.state, Since: &since, BuildMode: heavy.estimate.mode,
		EstimatedPeakMiB: heavy.estimate.peak >> 20, Slots: e.heavySlots.Count(),
		Holders: e.heavyHolderLabels(), Error: heavy.err,
	}
}

// heavyBuildWaitReason 在该工作区有排队或进行中的重型构建时返回一句说明，否则返回空串。
func (e *Engine) heavyBuildWaitReason(ref engine.WorkspaceRef) string {
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return ""
	}
	heavy := e.heavyStateOf(workspaceKey)
	switch heavy.state {
	case "queued":
		return fmt.Sprintf("重型构建在排队（估算峰值 %d MiB，名额持有者：%s）", heavy.estimate.peak>>20, strings.Join(e.heavyHolderLabels(), "; "))
	case "running":
		return fmt.Sprintf("重型构建进行中（估算峰值 %d MiB）", heavy.estimate.peak>>20)
	}
	return ""
}

// heavyProgressLabel 是构建进度文本里的重型构建部分（前导空格），没有在建重型构建时为空。
func (e *Engine) heavyProgressLabel(workspaceKey string) string {
	heavy := e.heavyStateOf(workspaceKey)
	if heavy.state == "" {
		return ""
	}
	label := fmt.Sprintf(" heavy_build=%s estimated_peak_mib=%d", heavy.state, heavy.estimate.peak>>20)
	if heavy.state == "queued" {
		label += " holders=[" + strings.Join(e.heavyHolderLabels(), "; ") + "]"
	}
	return label
}
