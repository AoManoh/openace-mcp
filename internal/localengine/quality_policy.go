package localengine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// qualityGap 是一条降级原因：reason 是写进 degraded_reason 的稳定原因
// token，detail 是可读说明（含原因来源与恢复办法），可为 nil。
type qualityGap struct {
	reason string
	detail error
}

// 原因的归属，见 gapSwitch。
const (
	gapSemanticUnavailable = "semantic-unavailable"
	gapRetrieval           = "retrieval"
	gapRerank              = "rerank"
	gapStrictOnly          = "strict-only"
)

// semanticIndexEmptyReason 表示所服务的 revision 一个向量也没有（首次构建
// 先发布的词法中间版本，或嵌入全部失败后发布的零覆盖 revision）。
const semanticIndexEmptyReason = "semantic-index-empty"

// coverageInsufficientReason 表示缺向量的文件超过 uncoveredFilesLimit 个：
// 清单不再适合放进响应，调用方无从知道哪些文件没有经过语义检索。
const coverageInsufficientReason = "semantic-coverage-insufficient"

// uncoveredFilesLimit 是检索结果里能列出的未覆盖文件数上限。它按响应的
// 可读长度取值，不是质量阈值。
const uncoveredFilesLimit = 50

// gapSwitch 返回一条原因归哪一类：
//   - semantic-unavailable：语义路跑不了，包括向量文件不可用
//     （vector-data-unavailable）、查询嵌入失败（query-embedding-failed(类别)）、
//     revision 没有任何向量（semantic-index-empty）、缺向量的文件多到列不下
//     （semantic-coverage-insufficient）与维度探测尚未成功
//     （embedding-dimension-unresolved）。配置了 embedding 时这类原因总是拒绝，
//     见 policyError。
//   - rerank（OPENACE_RERANK_DEGRADE）：精排调用失败、无效响应或 token 预算
//     连第一条候选都装不下（rerank-skipped(类别)），精排响应不完整
//     （rerank-partial-response）。
//   - 只由 OPENACE_QUALITY_STRICT 拒绝：rerank-unconfigured(...) 与
//     rerank-not-applied。未配置精排不是调用失败；显式 rerank off 是用户的
//     选择，不产生原因。
//   - 其余归 retrieval，照常作答并随结果写明，只有 OPENACE_QUALITY_STRICT
//     拒绝：索引不新鲜（stale-index、index-building、index-refreshing）、语义
//     覆盖不足（semantic-coverage-partial）、语义迁移暂停
//     （semantic-migration-blocked）。
func gapSwitch(reason string) string {
	switch {
	case reason == "vector-data-unavailable", reason == semanticIndexEmptyReason, reason == coverageInsufficientReason, reason == dimensionUnresolvedReason, strings.HasPrefix(reason, "query-embedding-failed"):
		return gapSemanticUnavailable
	case strings.HasPrefix(reason, "rerank-skipped"), reason == "rerank-partial-response":
		return gapRerank
	case strings.HasPrefix(reason, "rerank-"):
		return gapStrictOnly
	default:
		return gapRetrieval
	}
}

// policyError 判定一组缺口是否必须拒绝。提前拒绝（同步失败、manifest 已知
// 的缺口、向量与查询嵌入失败、精排失败）与最终返回都调用它，不另维护一套
// 判定：
//   - 有 semantic-unavailable 类缺口：总是拒绝。配置了 embedding 就只用语义
//     检索的结果作答：纯词法结果的召回只有语义检索的四分之一左右（R02、R03
//     评测），调用方会把没找到当成不存在；精确关键词搜索调用方自己的 grep
//     更完整。错误列出全部原因与说明。
//   - OPENACE_QUALITY_STRICT=on：任何缺口都拒绝，错误列出全部原因与说明。
//   - OPENACE_RERANK_DEGRADE=deny：有 rerank 类缺口时拒绝。
//   - 其余情况放行，返回 nil，原因随结果返回。
//
// 同时满足多条时依次按以上顺序返回第一个错误。拒绝只影响这次响应：已发布
// 的 revision、已付费的向量与状态跟踪都不改动。
func (e *Engine) policyError(gaps []qualityGap) error {
	if len(gaps) == 0 {
		return nil
	}
	for _, gap := range gaps {
		if gapSwitch(gap.reason) == gapSemanticUnavailable {
			return semanticUnavailableError(gaps)
		}
	}
	if e.qualityStrict {
		details := gapDetails(gaps)
		if details != "" {
			details = ": " + details
		}
		return fmt.Errorf("%s=on: 语义质量缺口 [%s],拒绝返回不完整结果(改用 %s=off 可降级放行)%s",
			EnvQualityStrict, gapReasons(gaps), EnvQualityStrict, details)
	}
	if e.rerankDegrade == DegradeDeny {
		var matched []qualityGap
		for _, gap := range gaps {
			if gapSwitch(gap.reason) == gapRerank {
				matched = append(matched, gap)
			}
		}
		if len(matched) > 0 {
			cause := gapDetails(matched)
			if cause == "" {
				cause = "see degraded_reason"
			}
			return degradeDeniedError(gapReasons(matched), errors.New(cause), EnvRerankDegrade)
		}
	}
	return nil
}

// semanticUnavailableError 是语义路跑不了时的统一错误，列出全部原因与说明。
// 维度待定的服务（deferredEngine）也用它，调用方看到的格式一致。
func semanticUnavailableError(gaps []qualityGap) error {
	return fmt.Errorf("semantic retrieval unavailable [%s]: %s; an embedding provider is configured, so openACE does not answer with lexical-only results; retry after the cause above is resolved, or search with your own tools meanwhile",
		gapReasons(gaps), gapDetails(gaps))
}

// manifestGaps 返回从 revision 的 manifest 就能知道的缺口：没有任何向量
// （semantic-index-empty）或语义覆盖不足，以及迁移暂停原因；未配置语义时
// 为空。同步结果与查询共用：同步据此给出原因并判定拒绝，查询据此在发出
// 查询嵌入之前判定。没有 chunk 的空工作区不算缺口。
func (e *Engine) manifestGaps(m *index.Manifest) []qualityGap {
	if !e.semanticEnabled() {
		return nil
	}
	var gaps []qualityGap
	switch {
	case m.Counts.Chunks > 0 && m.VectorCount == 0:
		gaps = append(gaps, qualityGap{
			reason: semanticIndexEmptyReason,
			detail: fmt.Errorf("revision %s has no vectors yet (0 of %d live chunks embedded)", m.Revision, m.Counts.Chunks),
		})
	case !m.SemanticComplete():
		gaps = append(gaps, qualityGap{
			reason: "semantic-coverage-partial",
			detail: fmt.Errorf("semantic_coverage=%s (%d of %d embeddable chunks have vectors; %d chunks rejected by the provider are excluded)", coveragePercent(m), m.VectorCount, m.EmbeddableChunks(), m.RejectedCount),
		})
	}
	if m.SemanticMigrationBlock != "" {
		gaps = append(gaps, qualityGap{reason: migrationBlockedReason, detail: errors.New(m.SemanticMigrationBlock)})
	}
	return gaps
}

// withBuildProgress 给 semantic-index-empty 缺口的说明补上该工作区的构建
// 进度与索引嵌入熔断器状态。调用方据此判断要等多久，或先用自己的工具。
// 没有构建在进行时只写明这一点：上次构建结束后计数器已归零，照常写出
// “已嵌入 0、待嵌入 0”会让人误以为没有内容等待嵌入。
func (e *Engine) withBuildProgress(gaps []qualityGap, workspaceKey string) []qualityGap {
	for i, gap := range gaps {
		if gap.reason != semanticIndexEmptyReason && gap.reason != coverageInsufficientReason {
			continue
		}
		e.mu.Lock()
		_, building := e.inflight[workspaceKey]
		e.mu.Unlock()
		extra := "no build in progress"
		if building {
			extra = "build progress: " + e.buildProgressLabel(workspaceKey)
		}
		if snap := e.embedClient.CircuitSnapshot(); snap.State != "healthy" {
			extra += "; embedding provider " + circuitLabel(snap)
		}
		gaps[i].detail = fmt.Errorf("%v; %s", gap.detail, extra)
	}
	return gaps
}

// coverageListing 列出所服务 revision 中语义覆盖有缺口的文件：uncovered 是
// 还有 chunk 没有向量、也没有被 provider 拒绝的文件；rejected 是含被拒绝
// chunk 的文件，最多列 uncoveredFilesLimit 个，rejectedTotal 是总数。两份
// 清单按路径排序。未覆盖文件超过 uncoveredFilesLimit 个时返回
// semantic-coverage-insufficient 缺口，调用方按语义路不可用处理。只在
// manifest 显示覆盖不完整或有被拒绝内容时调用，覆盖完整的常态没有开销。
func coverageListing(m *index.Manifest) (uncovered, rejected []string, rejectedTotal int, gap *qualityGap) {
	for path, entry := range m.Files {
		if entry.RejectedChunks > 0 {
			rejected = append(rejected, path)
		}
		if entry.CoveredChunks+entry.RejectedChunks < entry.ChunkCount {
			uncovered = append(uncovered, path)
		}
	}
	sort.Strings(uncovered)
	sort.Strings(rejected)
	rejectedTotal = len(rejected)
	if len(rejected) > uncoveredFilesLimit {
		rejected = rejected[:uncoveredFilesLimit]
	}
	if len(uncovered) > uncoveredFilesLimit {
		gap = &qualityGap{
			reason: coverageInsufficientReason,
			detail: fmt.Errorf("%d files still have chunks without vectors (semantic_coverage=%s), more than the %d that can be listed; semantic search would miss them", len(uncovered), coveragePercent(m), uncoveredFilesLimit),
		}
		return nil, rejected, rejectedTotal, gap
	}
	return uncovered, rejected, rejectedTotal, nil
}

// coverageNote 在正文开头写出语义检索没有覆盖的文件，调用方据此自己查这些
// 文件，不把没检索到当成不存在。
func coverageNote(uncovered, rejected []string, rejectedTotal int) string {
	var b strings.Builder
	if len(uncovered) > 0 {
		fmt.Fprintf(&b, "[COVERAGE] semantic search does not cover these %d files yet (chunks without vectors); only lexical matching can find them: %s\n", len(uncovered), strings.Join(uncovered, ", "))
	}
	if len(rejected) > 0 {
		more := ""
		if rejectedTotal > len(rejected) {
			more = fmt.Sprintf(" (+%d more)", rejectedTotal-len(rejected))
		}
		fmt.Fprintf(&b, "[COVERAGE] the embedding provider rejected some chunks in these %d files; those chunks are found only by lexical matching: %s%s\n", rejectedTotal, strings.Join(rejected, ", "), more)
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// circuitLabel 把熔断器快照写成一句说明：状态、下次允许请求的时刻、连续
// 失败次数与最后错误。
func circuitLabel(snap reliability.CircuitSnapshot) string {
	label := "state=" + snap.State
	if !snap.BackoffUntil.IsZero() {
		label += " next_attempt_after=" + snap.BackoffUntil.UTC().Format(time.RFC3339)
	}
	if snap.ConsecutiveFailures > 0 {
		label += fmt.Sprintf(" consecutive_failures=%d", snap.ConsecutiveFailures)
	}
	if snap.LastError != "" {
		label += " last_error=" + snap.LastError
	}
	return label
}

// gapReasons 用逗号连接原因 token，与 degraded_reason 的格式一致。
func gapReasons(gaps []qualityGap) string {
	return strings.Join(gapReasonList(gaps), ",")
}

// gapReasonList 返回原因 token 列表，顺序与缺口出现的顺序一致。
func gapReasonList(gaps []qualityGap) []string {
	reasons := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		reasons = append(reasons, gap.reason)
	}
	return reasons
}

// gapDetails 用分号连接有说明的缺口的说明文字。
func gapDetails(gaps []qualityGap) string {
	var details []string
	for _, gap := range gaps {
		if gap.detail != nil {
			details = append(details, gap.detail.Error())
		}
	}
	return strings.Join(details, "; ")
}
