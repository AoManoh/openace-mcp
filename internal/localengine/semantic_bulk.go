package localengine

// 离线批车道的引擎侧编排(任务 T8)。职责:把 embedRecords 算出的缺失
// 键/文本切成 ≤10 万输入的批作业,提交给 voyage Batch API,轮询到完成,
// 回收校验后写入 embedding journal——之后与同步车道共享同一段尾部组装,
// 产物完全等价(同模型同维度,批与实时 API 实测余弦=1.000000)。
//
// 崩溃安全(验收 G2):每提交一个作业立即持久化到 journal 同目录的
// bulk-jobs.json;daemon 重启后续轮询同一作业 id,绝不重复提交付费。
// 失败纪律(验收 G4):作业终态失败显式外抛(带作业 id 与处置指引),
// 绝不静默回落同步 API 双付。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// bulkStateFileName 是作业状态文件名,与 journal 同目录(同一 profile
// 子树;profile 变更=不同子树,天然不会捡到旧配置的作业)。
const bulkStateFileName = "bulk-jobs.json"

// bulkState 是批作业的持久化状态。
type bulkState struct {
	// ProfileHash 冗余记录提交时的嵌入身份:目录隔离已保证一致,此处
	// 是防御纵深——不一致说明状态文件被手工搬运,显式拒绝。
	ProfileHash string              `json:"profile_hash"`
	Jobs        []embedding.BulkJob `json:"jobs"`
}

func bulkStatePath(journal *index.Journal) string {
	return filepath.Join(journal.Dir(), bulkStateFileName)
}

func loadBulkState(path string) (bulkState, error) {
	var state bulkState
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("读取批作业状态 %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("批作业状态损坏 %s(手工清理后重跑 sync 会重新提交并重新计费): %w", path, err)
	}
	return state, nil
}

func saveBulkState(path string, state bulkState) error {
	raw, err := json.MarshalIndent(state, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// bulkEligible 判定本次缺失量是否走批车道。
func (e *Engine) bulkEligible(missing int) bool {
	return e.embedClient != nil && e.embedClient.BulkSupported() && missing >= e.embedClient.BulkMinChunks()
}

// bulkEmbedMissing 执行批车道全流程。成功返回后,全部可回收向量已入
// journal 与 reuse 映射(调用方走共享尾部组装);终态失败返回显式错误。
func (e *Engine) bulkEmbedMissing(ctx context.Context, journal *index.Journal, status *wsStatus,
	missingHashes []string, missingTexts []string, reuse map[string][]float32, out *semanticOutcome) error {
	statePath := bulkStatePath(journal)
	state, err := loadBulkState(statePath)
	if err != nil {
		return err
	}
	profile := e.embedCfg.ProfileHash()
	if state.ProfileHash != "" && state.ProfileHash != profile {
		return fmt.Errorf("批作业状态 %s 属于其他嵌入身份(%s != %s);该文件不应被手工搬运,清理后重跑 sync", statePath, state.ProfileHash, profile)
	}
	state.ProfileHash = profile

	// 1) 恢复面:已提交作业覆盖的键不再提交(断点续作业,零重复付费)。
	submitted := make(map[string]bool)
	for _, job := range state.Jobs {
		for _, key := range job.Keys {
			submitted[key] = true
		}
	}
	var pendingKeys []string
	var pendingTexts []string
	for i, key := range missingHashes {
		if !submitted[key] {
			pendingKeys = append(pendingKeys, key)
			pendingTexts = append(pendingTexts, missingTexts[i])
		}
	}

	// 2) 提交面:剩余键按单作业上限切段,逐段提交并立即持久化。
	for start := 0; start < len(pendingKeys); start += embedding.BulkJobMaxInputs {
		end := start + embedding.BulkJobMaxInputs
		if end > len(pendingKeys) {
			end = len(pendingKeys)
		}
		job, err := e.embedClient.BulkSubmit(ctx, pendingKeys[start:end], pendingTexts[start:end])
		if err != nil {
			return fmt.Errorf("批车道提交失败(已提交的 %d 个作业已持久化,重跑 sync 会续轮询而非重复提交): %w", len(state.Jobs), err)
		}
		state.Jobs = append(state.Jobs, job)
		if err := saveBulkState(statePath, state); err != nil {
			return fmt.Errorf("批作业状态落盘失败(作业 %s 已在服务端创建!必须修复落盘问题后重跑,否则该作业会被遗忘): %w", job.ID, err)
		}
	}

	// 3) 轮询与回收:逐作业等待终态;completed 即回收入 journal 并从
	// 状态摘除,失败终态显式外抛。
	total := len(missingHashes)
	ingested := 0
	for len(state.Jobs) > 0 {
		job := state.Jobs[0]
		jobStatus, err := e.embedClient.BulkPoll(ctx, job.ID)
		if err != nil {
			return fmt.Errorf("批作业 %s 轮询失败(状态已持久化,可重跑续轮询): %w", job.ID, err)
		}
		status.setBulkJob(fmt.Sprintf("voyage:%s %s %d/%d", jobStatus.ID, jobStatus.Status, jobStatus.Done, jobStatus.Total))
		if !jobStatus.Terminal() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(e.embedClient.BulkPollInterval()):
			}
			continue
		}
		if jobStatus.Status != "completed" {
			// 终态失败:该作业从状态摘除(其输入未产出向量,重跑 sync 会
			// 重新提交该段并重新计费——这必须是用户看得见的显式决定,
			// 不是引擎的静默兜底)。其余作业保留继续可恢复。
			state.Jobs = state.Jobs[1:]
			if saveErr := saveBulkState(statePath, state); saveErr != nil {
				return fmt.Errorf("批作业 %s 终态 %s 且状态落盘失败: %v (原始失败见前)", job.ID, jobStatus.Status, saveErr)
			}
			return fmt.Errorf("批作业 %s 终态 %s(输入 %d 条未产出向量);重跑 sync 将重新提交该段并重新计费,或改用同步车道(unset %s)", job.ID, jobStatus.Status, len(job.Keys), embedding.EnvBatchAPI)
		}
		result, err := e.embedClient.BulkFetchResults(ctx, jobStatus, job.Keys)
		if err != nil {
			return fmt.Errorf("批作业 %s 结果回收失败(可重跑续回收): %w", job.ID, err)
		}
		// 与同步车道同口径的向量门禁:归一化失败(零向量/NaN)进持久化
		// 拒绝集;其余回收失败行(unknown/缺行/坏形状)保持未覆盖并在
		// lastError 汇总——它们可能是服务端瞬态,不该永久拉黑。
		good := make(map[string][]float32, len(result.Vectors))
		var normRejected []string
		for key, vec := range result.Vectors {
			if err := vector.Normalize(vec); err != nil {
				normRejected = append(normRejected, key)
				continue
			}
			good[key] = vec
		}
		if err := journal.Append(good); err != nil {
			return fmt.Errorf("批作业 %s 结果落 journal 失败: %w", job.ID, err)
		}
		if err := journal.MarkRejected(normRejected); err != nil {
			return fmt.Errorf("批作业 %s 拒绝集落盘失败: %w", job.ID, err)
		}
		for key, vec := range good {
			reuse[key] = vec
			out.newlyEmbedded++
		}
		out.rejected += len(normRejected)
		if n := len(result.RejectedKeys); n > 0 {
			out.lastError = sanitizeError(fmt.Errorf("bulk job %s: %d rows rejected (first: %s=%s)",
				job.ID, n, result.RejectedKeys[0], result.RejectReason[result.RejectedKeys[0]]))
		}
		ingested += len(good)
		status.setEmbedProgress(total-ingested, ingested)
		state.Jobs = state.Jobs[1:]
		if err := saveBulkState(statePath, state); err != nil {
			return fmt.Errorf("批作业状态更新失败(作业 %s 已回收入 journal,重跑安全——journal 复用会跳过已付费键): %w", job.ID, err)
		}
	}
	// 全部作业收尾:状态文件清理(空作业列表无需保留)。
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("批作业状态清理失败: %w", err)
	}
	status.setBulkJob("")
	return nil
}
