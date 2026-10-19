package localengine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AoManoh/openace-mcp/internal/index"
)

// qualityGap 是一条降级原因：reason 是写进 degraded_reason 的稳定原因
// token，detail 是可读说明（含原因来源与恢复办法），可为 nil。
type qualityGap struct {
	reason string
	detail error
}

// 原因的归属开关，见 gapSwitch。
const (
	gapRetrieval  = "retrieval"
	gapRerank     = "rerank"
	gapStrictOnly = "strict-only"
)

// gapSwitch 返回一条原因由哪个开关决定是否拒绝：
//   - rerank（OPENACE_RERANK_DEGRADE）：精排调用失败、无效响应或 token 预算
//     连第一条候选都装不下（rerank-skipped(类别)），精排响应不完整
//     （rerank-partial-response）。
//   - 只由 OPENACE_QUALITY_STRICT 拒绝：rerank-unconfigured(...) 与
//     rerank-not-applied。未配置精排不是调用失败；显式 rerank off 是用户的
//     选择，不产生原因。
//   - 其余都归 retrieval（OPENACE_RETRIEVAL_DEGRADE）：索引同步失败
//     （stale-index、index-building、index-refreshing）、向量不可用
//     （vector-envelope-exceeded、vector-data-unavailable）、查询嵌入失败
//     （query-embedding-failed(类别)）、语义覆盖不足
//     （semantic-coverage-partial）、语义迁移暂停（semantic-migration-blocked）。
func gapSwitch(reason string) string {
	switch {
	case strings.HasPrefix(reason, "rerank-skipped"), reason == "rerank-partial-response":
		return gapRerank
	case strings.HasPrefix(reason, "rerank-"):
		return gapStrictOnly
	default:
		return gapRetrieval
	}
}

// policyError 按已批准的降级政策判定一组缺口是否必须拒绝。提前拒绝（同步
// 失败、manifest 已知的覆盖不足与迁移暂停、向量与查询嵌入失败、精排失败）
// 与最终返回都调用它，不另维护一套判定：
//   - OPENACE_QUALITY_STRICT=on：任何缺口都拒绝，错误列出全部原因与说明。
//   - OPENACE_RETRIEVAL_DEGRADE=deny：有 retrieval 类缺口时拒绝。
//   - OPENACE_RERANK_DEGRADE=deny：有 rerank 类缺口时拒绝。
//   - 其余情况放行，返回 nil，原因随结果返回。
//
// 同时满足多条时依次按 strict、retrieval deny、rerank deny 返回第一个错误，
// 错误写明原因 token 与对应环境变量。拒绝只影响这次响应：已发布的
// revision、已付费的向量与状态跟踪都不改动。
func (e *Engine) policyError(gaps []qualityGap) error {
	if len(gaps) == 0 {
		return nil
	}
	if e.qualityStrict {
		details := gapDetails(gaps)
		if details != "" {
			details = ": " + details
		}
		return fmt.Errorf("%s=on: 语义质量缺口 [%s],拒绝返回不完整结果(改用 %s=off 可降级放行)%s",
			EnvQualityStrict, gapReasons(gaps), EnvQualityStrict, details)
	}
	rules := []struct {
		deny  bool
		class string
		env   string
	}{
		{e.retrievalDegrade == DegradeDeny, gapRetrieval, EnvRetrievalDegrade},
		{e.rerankDegrade == DegradeDeny, gapRerank, EnvRerankDegrade},
	}
	for _, rule := range rules {
		if !rule.deny {
			continue
		}
		var matched []qualityGap
		for _, gap := range gaps {
			if gapSwitch(gap.reason) == rule.class {
				matched = append(matched, gap)
			}
		}
		if len(matched) == 0 {
			continue
		}
		cause := gapDetails(matched)
		if cause == "" {
			cause = "see degraded_reason"
		}
		return degradeDeniedError(gapReasons(matched), errors.New(cause), rule.env)
	}
	return nil
}

// manifestGaps 返回从 revision 的 manifest 就能知道的缺口：语义覆盖不足与
// 迁移暂停原因；未配置语义时为空。同步结果与查询共用：同步据此给出原因
// 并判定拒绝，查询据此在发出查询嵌入之前判定。
func (e *Engine) manifestGaps(m *index.Manifest) []qualityGap {
	if !e.semanticEnabled() {
		return nil
	}
	var gaps []qualityGap
	if !m.SemanticComplete() {
		gaps = append(gaps, qualityGap{
			reason: "semantic-coverage-partial",
			detail: fmt.Errorf("semantic_coverage=%s (%d of %d live chunks have vectors)", coveragePercent(m), m.VectorCount, m.Counts.Chunks),
		})
	}
	if m.SemanticMigrationBlock != "" {
		gaps = append(gaps, qualityGap{reason: migrationBlockedReason, detail: errors.New(m.SemanticMigrationBlock)})
	}
	return gaps
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
