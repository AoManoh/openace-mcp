package localengine

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
)

// compatibleProfileCandidate 是同 workspace 下可复用向量的旧 profile。
type compatibleProfileCandidate struct {
	store    *index.Store
	manifest *index.Manifest
}

// mergeSiblingProfileVectors 从当前 Store 的兄弟 profile 子树读取兼容
// 向量，注入最低优先级 prior。真实 v6→v7 迁移证明 524,885 keys 中
// 99.927% 可复用；自动化避免正常用户升级全量重付。
//
// 安全边界:
//   - 只枚举 cache 管理的同 workspace 目录,不跟随 symlink；
//   - workspace identity + engine + embedding provider/model/dim/dtype/
//     profile hash 必须精确一致；chunk version 可不同；
//   - 旧子树只读；损坏/无 active/不兼容均静默跳过,回退正常 provider；
//   - 只取向量数最多且可成功加载的一棵,避免叠加常驻内存。
func (e *Engine) mergeSiblingProfileVectors(current *index.Store, root pathutil.WorkspaceRoot, prior *priorVectors) {
	if !e.semanticEnabled() || current == nil || prior == nil {
		return
	}
	parent := filepath.Dir(current.Root())
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	var candidates []compatibleProfileCandidate
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		candidateRoot := filepath.Join(parent, entry.Name())
		if candidateRoot == current.Root() {
			continue
		}
		store, err := index.OpenExistingStore(candidateRoot)
		if err != nil {
			continue
		}
		manifest, _, err := store.ResolveUsable()
		if err != nil || !e.compatibleSiblingManifest(manifest, root) {
			continue
		}
		candidates = append(candidates, compatibleProfileCandidate{store: store, manifest: manifest})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].manifest.VectorCount != candidates[j].manifest.VectorCount {
			return candidates[i].manifest.VectorCount > candidates[j].manifest.VectorCount
		}
		return candidates[i].manifest.ActivatedAt.After(candidates[j].manifest.ActivatedAt)
	})
	for _, candidate := range candidates {
		loaded := e.loadPriorVectors(candidate.store, candidate.manifest)
		// 候选 active 物理载入必须覆盖 manifest 宣称的全部向量；部分
		// 损坏候选不能因"尚存一条"就阻止后续健康 sibling 参与。
		if loaded.activeLoadedRows < loaded.activeExpectedRows || loaded.activeLoadedRows == 0 {
			continue
		}
		if prior.crossProfileByHash == nil {
			prior.crossProfileByHash = make(map[string][]float32)
		}
		for key, vec := range loaded.activeByHash {
			prior.crossProfileByHash[key] = vec
		}
		for key, vec := range loaded.olderByHash {
			if _, ok := prior.crossProfileByHash[key]; !ok {
				prior.crossProfileByHash[key] = vec
			}
		}
		return
	}
}

func (e *Engine) compatibleSiblingManifest(manifest *index.Manifest, root pathutil.WorkspaceRoot) bool {
	if manifest == nil || manifest.EngineID != EngineID || manifest.VectorCount == 0 {
		return false
	}
	if manifest.Workspace.CanonicalPath != root.CanonicalPath ||
		manifest.Workspace.PathKind != string(root.PathKind) || manifest.Workspace.HostOS != root.HostOS {
		return false
	}
	return manifest.EmbeddingProvider == e.embedCfg.ProviderType &&
		manifest.EmbeddingModel == e.embedCfg.Model &&
		manifest.EmbeddingDimension == e.embedCfg.Dimension &&
		manifest.EmbeddingDtype == embedding.Dtype &&
		manifest.EmbeddingProfileHash == e.embedCfg.ProfileHash()
}
