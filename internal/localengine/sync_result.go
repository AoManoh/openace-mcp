package localengine

import (
	"strings"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
)

// revisionResult 从一个明确的 manifest 构造同步结果。revision、文件数、语义
// 覆盖、检索能力模式与 revision 记录的迁移暂停原因都取自这一个 manifest，
// 不读跟踪器或其他 revision 的状态；full、delta、未发布、无变化与熔断退避
// 等分支都经这里构造，不各自维护覆盖、模式与原因。
//   - previous 是本次同步开始时的 active revision（首次构建为 nil）。Deleted
//     是 previous 的文件集合里、在 manifest 中已不存在的路径数，包括改为忽略
//     或不再收录的文件；被改写文件的旧 chunk 不算删除文件。没有发布新
//     revision（manifest 就是 previous）时为 0，首次构建为 0。
//   - RetrievalMode 说明这个 revision 已建立的检索能力，不表示执行过查询或
//     精排：未配置语义或 revision 没有任何向量时为 lexical，有向量时为
//     hybrid。
//   - 配置了语义时携带覆盖率；覆盖不完整给出 semantic-coverage-partial，
//     revision 记录了迁移暂停原因时再追加 semantic-migration-blocked。
//
// Added、BuildMode、CrossProfileReused 只在发布新 revision 时由调用方填写。
func (e *Engine) revisionResult(manifest, previous *index.Manifest) engine.Result {
	result := engine.Result{
		Engine:        EngineID,
		IndexRevision: manifest.Revision,
		FileCount:     manifest.Counts.Files,
		Deleted:       deletedFiles(previous, manifest),
		RetrievalMode: "lexical",
	}
	if !e.semanticEnabled() {
		return result
	}
	if manifest.VectorCount > 0 {
		result.RetrievalMode = "hybrid"
	}
	result.SemanticCoverage = coveragePercent(manifest)
	var reasons []string
	if !manifest.SemanticComplete() {
		reasons = append(reasons, "semantic-coverage-partial")
	}
	if manifest.SemanticMigrationBlock != "" {
		reasons = append(reasons, migrationBlockedReason)
	}
	result.DegradedReason = strings.Join(reasons, ",")
	return result
}

// deletedFiles 返回 previous 的文件集合中、在 manifest 里已不存在的路径数；
// previous 为空或与 manifest 是同一 revision 时为 0。
func deletedFiles(previous, manifest *index.Manifest) int {
	if previous == nil || previous.Revision == manifest.Revision {
		return 0
	}
	removed := 0
	for path := range previous.Files {
		if _, ok := manifest.Files[path]; !ok {
			removed++
		}
	}
	return removed
}

// servedResult 返回查询结果的基础字段：revision 与文件数取自查询实际持有的
// revision。同步结果描述的是同一 revision 时才沿用它的构建字段（Added、
// Deleted、BuildMode、CrossProfileReused）；不是同一 revision 时丢弃，避免
// 把另一个 revision 的构建信息写进本次结果。模式、覆盖率与原因由查询按
// 本次实际执行的检索填写，不沿用同步结果的同名字段。
func servedResult(synced engine.Result, served *index.Manifest) engine.Result {
	result := engine.Result{Engine: EngineID, IndexRevision: served.Revision, FileCount: served.Counts.Files}
	if synced.IndexRevision == served.Revision {
		result.Added = synced.Added
		result.Deleted = synced.Deleted
		result.BuildMode = synced.BuildMode
		result.CrossProfileReused = synced.CrossProfileReused
	}
	return result
}
