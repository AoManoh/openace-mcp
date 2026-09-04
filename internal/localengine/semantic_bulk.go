package localengine

// 本文件实现 document 嵌入的批作业路径。embedRecords 算出缺失的嵌入键与
// 文本后，若用户开启了 OPENACE_EMBEDDING_BATCH_API 且缺失数达到
// OPENACE_EMBEDDING_BATCH_MIN_CHUNKS 阈值，就不再逐批调用同步接口，而是把
// 输入按 embedding.BulkJobMaxInputs（10 万条）切成作业提交给 voyage Batch
// API，轮询到终态，回收结果写入 embedding journal（已付费向量的落盘暂存
// 区）。之后 embedRecords 用与同步路径相同的 assembleSemanticOutcome 组装
// 产物。批接口与同步接口使用同模型同维度，向量等价（实测两者余弦相似度
// 1.000000），费用低 33%；代价是作业在服务端排队，窗口最长 12 小时。
//
// 两条纪律：
//   - 崩溃安全：每提交成功一个作业，立刻把作业 id 写入 journal 同目录的
//     bulk-jobs.json；daemon 重启后接着轮询同一作业，不重复提交、不重复
//     付费。
//   - 失败显式：作业到达非 completed 终态时返回带作业 id 与处置步骤的
//     错误，不自动改走同步接口再付一次费。

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

// bulkStateFileName 是批作业状态文件名。文件与 journal 同目录，也就是同一
// embedding 身份（provider、模型、维度、模板版本）的索引子树；更换身份后
// 使用另一棵子树，读不到旧配置提交的作业。
const bulkStateFileName = "bulk-jobs.json"

// bulkState 是批作业的持久化状态，随每次提交与回收原子写回。
type bulkState struct {
	// ProfileHash 记录提交时的 embedding 身份。目录隔离已经保证一致，这里
	// 再记一份是为了识别被手工搬运到别的子树的状态文件：不一致时拒绝使
	// 用，否则会把另一身份的向量当作本身份的结果回收。
	ProfileHash string              `json:"profile_hash"`
	Jobs        []embedding.BulkJob `json:"jobs"`
	// PendingIntent 是提交前先写下的意向。上传输入并创建作业成功后、作业
	// id 落盘之前若进程崩溃，服务端已有作业而本地没有记录，重跑会把同一
	// 段输入再提交一次，同一段计费两次。因此每段先落意向，作业登记成功
	// 后清除；恢复时发现意向残留，停止并要求人工到 provider 核对，不自动
	// 重提。
	PendingIntent *bulkIntent `json:"pending_intent,omitempty"`
}

// bulkIntent 描述一段"已经准备提交、可能已在服务端创建"的输入。只保存键
// 集合的指纹、条数和时间，不含 chunk 明文。
type bulkIntent struct {
	KeysSHA256 string    `json:"keys_sha256"`
	Count      int       `json:"count"`
	CreatedAt  time.Time `json:"created_at"`
}

// bulkKeysDigest 计算一段输入键集合的指纹：键按顺序以换行连接后取
// SHA-256。键本身是 embedKey 的十六进制串，不含 chunk 明文。人工核对
// provider 侧作业时用该指纹对应本地记录。
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

// bulkEligible 判定本次缺失数是否走批作业路径：embedding 客户端支持批接
// 口（用户显式开启）且缺失数不低于阈值。阈值以下的小量缺失走同步接口，
// 不值得等服务端排队窗口。
func (e *Engine) bulkEligible(missing int) bool {
	return e.embedClient != nil && e.embedClient.BulkSupported() && missing >= e.embedClient.BulkMinChunks()
}

// bulkEmbedMissing 执行批作业路径的全流程：读取状态文件、跳过已提交的键、
// 提交剩余输入、轮询到终态、回收结果写入 journal 与 reuse。正常返回时全部
// 可回收向量都已进入 journal 与 reuse，调用方接着做与同步路径相同的组装；
// 任何作业到达非 completed 终态时返回带作业 id 与处置步骤的错误。
// 分支：
//   - 状态文件属于另一 embedding 身份：报错并要求清理后重跑。
//   - 存在未收尾的提交意向：报错停止，要求人工核对 provider 侧作业，
//     不自动重提（理由见 bulkState.PendingIntent）。
//   - 已提交作业覆盖的键不再提交，只提交剩余键；每段先写意向再提交，
//     登记成功后清意向。
//   - 逐作业轮询；completed 作业全量回收；非 completed 终态先回收已产出
//     的向量再报错，因为输出文件里已有的行已经计费，丢弃等于重跑时再付
//     一次。
//   - 全部作业处理完后删除状态文件。
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

	// 未收尾的提交意向说明上一次运行可能已在服务端创建了作业却没记下
	// 作业 id：创建成功与落盘之间崩溃，或提交调用报错时分不清"未创建"
	// 与"已创建但响应丢失"。自动重提会让同一段输入计费两次，所以这里
	// 停下来交给人工核对。
	if intent := state.PendingIntent; intent != nil {
		return fmt.Errorf("Batch API 路径存在未收尾的提交意向(%d 输入, keys_sha256=%s, 记录于 %s):请到 provider 控制台核对该时刻附近的批作业——不存在则删除 %s 中的 pending_intent 字段后重跑;存在则把该作业的 id/input_file_id/keys 手工补入 jobs 数组后重跑续轮询。引擎不自动重提该段(防双份计费)",
			intent.Count, intent.KeysSHA256, intent.CreatedAt.Format(time.RFC3339), statePath)
	}

	// 1) 恢复：状态文件里已登记作业覆盖的键不再提交，进程重启后接着处理
	// 同一批作业，不为同一输入再付费。
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

	// 2) 提交：剩余键按单作业输入上限切段。每段先把意向写进状态文件，
	// 落盘成功后才调用服务端；提交调用报错时意向保留在文件里，下次运行
	// 会停在上面的人工核对步，而不是把同一段再提交一次。
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
			return fmt.Errorf("Batch API 提交失败,且未能确认服务端是否已创建作业(意向已留档,重跑会显式停在核对步;已登记的 %d 个作业不受影响): %w", len(state.Jobs), err)
		}
		state.PendingIntent = nil
		state.Jobs = append(state.Jobs, job)
		if err := saveBulkState(statePath, state); err != nil {
			return fmt.Errorf("批作业状态落盘失败(作业 %s 已在服务端创建!必须修复落盘问题后重跑,否则该作业会被遗忘): %w", job.ID, err)
		}
	}

	// 3) 轮询与回收：逐作业等到终态。completed 的作业全量回收进 journal；
	// 非 completed 终态先回收已产出的部分，再返回错误。
	total := len(missingHashes)
	ingested := 0
	// ingest 对回收的向量做与同步路径相同的检查：L2 归一化失败（零向量或
	// 含 NaN）的键写入持久化拒绝集，以后不再为它付费；provider 在结果文件
	// 里标记拒绝、缺行或形状不对的键只保留为未覆盖并汇总到 lastError。
	// 后一类可能是服务端的临时故障，进拒绝集会让它以后一直没有向量。
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
			// 非 completed 终态（partially_completed、failed、expired、
			// cancelled）。输出文件里已经产出的行 provider 已经计费，直接丢弃
			// 等于重跑时为它们再付一次，所以先回收进 journal，再把作业从状
			// 态里摘除并返回错误。重跑时 journal 里的向量直接复用，只有缺口
			// 部分会重新提交计费。这里返回错误而不是继续处理后续作业，让
			// 调用方看到失败原因。
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
			return fmt.Errorf("批作业 %s 终态 %s:已回收 %d 条已计费向量入 journal,其余 %d 条输入未产出向量;重跑 sync 只对缺口重新提交并计费(journal 复用保证已回收部分不重付),或改用同步嵌入路径(unset %s)",
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
	// 全部作业回收完毕，作业列表为空，状态文件没有保留价值，删除它。
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("批作业状态清理失败: %w", err)
	}
	status.setBulkJob("")
	return nil
}
