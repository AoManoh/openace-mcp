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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// PendingIntent 是提交意向的先行记录(A5,2026-08-26 裁决):上传+
	// 创建成功与作业落盘之间的崩溃会让服务端作业"本地失忆",重跑重提
	// =同段双份计费。提交前先落意向,登记作业成功即清;恢复时发现
	// 意向残留则显式停待人工核对,绝不自动重提。
	PendingIntent *bulkIntent `json:"pending_intent,omitempty"`
}

// bulkIntent 描述一段"即将/可能已经提交"的输入(不含明文,只留对账指纹)。
type bulkIntent struct {
	KeysSHA256 string    `json:"keys_sha256"`
	Count      int       `json:"count"`
	CreatedAt  time.Time `json:"created_at"`
}

// bulkKeysDigest 计算段内键集的对账指纹(键为 embedKey hex,无明文)。
func bulkKeysDigest(keys []string) string {
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
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

	// 恢复面前置(A5):存在未收尾的提交意向 = 上一次运行可能已在服务端
	// 创建作业但本地没记下作业 ID(创建成功与落盘之间崩溃,或提交调用
	// 报错时无法区分"未创建"与"已创建但响应丢失")。为防同段双份计费,
	// 显式停,绝不自动重提。
	if intent := state.PendingIntent; intent != nil {
		return fmt.Errorf("批车道存在未收尾的提交意向(%d 输入, keys_sha256=%s, 记录于 %s):请到 provider 控制台核对该时刻附近的批作业——不存在则删除 %s 中的 pending_intent 字段后重跑;存在则把该作业的 id/input_file_id/keys 手工补入 jobs 数组后重跑续轮询。引擎不自动重提该段(防双份计费)",
			intent.Count, intent.KeysSHA256, intent.CreatedAt.Format(time.RFC3339), statePath)
	}

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

	// 2) 提交面:剩余键按单作业上限切段;每段"意向先行→提交→登记"
	// (A5 write-ahead):意向落盘后才发起服务端调用,提交报错时意向
	// 保留——下次运行停在恢复面核对步,而不是盲目重提双付。
	for start := 0; start < len(pendingKeys); start += embedding.BulkJobMaxInputs {
		end := start + embedding.BulkJobMaxInputs
		if end > len(pendingKeys) {
			end = len(pendingKeys)
		}
		segKeys, segTexts := pendingKeys[start:end], pendingTexts[start:end]
		state.PendingIntent = &bulkIntent{KeysSHA256: bulkKeysDigest(segKeys), Count: len(segKeys), CreatedAt: time.Now().UTC()}
		if err := saveBulkState(statePath, state); err != nil {
			return fmt.Errorf("提交意向落盘失败(尚未发生任何服务端调用,可直接重跑): %w", err)
		}
		job, err := e.embedClient.BulkSubmit(ctx, segKeys, segTexts)
		if err != nil {
			return fmt.Errorf("批车道提交失败,且无法确认服务端是否已创建作业(意向已留档,重跑会显式停在核对步;已登记的 %d 个作业不受影响): %w", len(state.Jobs), err)
		}
		state.PendingIntent = nil
		state.Jobs = append(state.Jobs, job)
		if err := saveBulkState(statePath, state); err != nil {
			return fmt.Errorf("批作业状态落盘失败(作业 %s 已在服务端创建!必须修复落盘问题后重跑,否则该作业会被遗忘): %w", job.ID, err)
		}
	}

	// 3) 轮询与回收:逐作业等待终态。completed 全量回收入 journal;
	// 非 completed 终态先回收已产出部分再显式外抛(A4)。
	total := len(missingHashes)
	ingested := 0
	// ingest 是与同步车道同口径的向量门禁:归一化失败(零向量/NaN)进
	// 持久化拒绝集;其余回收失败行(unknown/缺行/坏形状)保持未覆盖并在
	// lastError 汇总——它们可能是服务端瞬态,不该永久拉黑。
	ingest := func(jobID string, result embedding.BulkResult) (int, error) {
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
			return 0, fmt.Errorf("批作业 %s 结果落 journal 失败: %w", jobID, err)
		}
		if err := journal.MarkRejected(normRejected); err != nil {
			return 0, fmt.Errorf("批作业 %s 拒绝集落盘失败: %w", jobID, err)
		}
		for key, vec := range good {
			reuse[key] = vec
			out.newlyEmbedded++
		}
		out.rejected += len(normRejected)
		if n := len(result.RejectedKeys); n > 0 {
			out.lastError = sanitizeError(fmt.Errorf("bulk job %s: %d rows rejected (first: %s=%s)",
				jobID, n, result.RejectedKeys[0], result.RejectReason[result.RejectedKeys[0]]))
		}
		ingested += len(good)
		status.setEmbedProgress(total-ingested, ingested)
		return len(good), nil
	}
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
			// 非 completed 终态(partially_completed/failed/expired/
			// cancelled)。A4(2026-08-26 裁决):输出文件里已产出的行是
			// **已计费**向量,弃之等于重跑双付——先回收入 journal,再摘除
			// 作业并显式外抛;journal 复用保证重跑只对缺口重新提交计费。
			// 显式失败(而非静默续跑)的语义保持不变。
			recovered := 0
			if jobStatus.OutputFileID != "" {
				result, fetchErr := e.embedClient.BulkFetchResults(ctx, jobStatus, job.Keys)
				if fetchErr != nil {
					return fmt.Errorf("批作业 %s 终态 %s,已产出部分回收失败(状态保留,重跑续回收,不会重复提交): %w", job.ID, jobStatus.Status, fetchErr)
				}
				n, ingestErr := ingest(job.ID, result)
				if ingestErr != nil {
					return ingestErr
				}
				recovered = n
			}
			state.Jobs = state.Jobs[1:]
			if saveErr := saveBulkState(statePath, state); saveErr != nil {
				return fmt.Errorf("批作业 %s 终态 %s 且状态落盘失败: %v (原始失败见前)", job.ID, jobStatus.Status, saveErr)
			}
			return fmt.Errorf("批作业 %s 终态 %s:已回收 %d 条已计费向量入 journal,其余 %d 条输入未产出向量;重跑 sync 只对缺口重新提交并计费(journal 复用保证已回收部分不重付),或改用同步车道(unset %s)",
				job.ID, jobStatus.Status, recovered, len(job.Keys)-recovered, embedding.EnvBatchAPI)
		}
		result, err := e.embedClient.BulkFetchResults(ctx, jobStatus, job.Keys)
		if err != nil {
			return fmt.Errorf("批作业 %s 结果回收失败(可重跑续回收): %w", job.ID, err)
		}
		if _, err := ingest(job.ID, result); err != nil {
			return err
		}
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
