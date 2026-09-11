package localengine

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
)

// compatibleProfileCandidate 是同一工作区下一个可复用向量的旧 chunk
// profile 索引子树：它的 Store 与当前可用的 manifest。
type compatibleProfileCandidate struct {
	store    *index.Store
	manifest *index.Manifest
}

// mergeSiblingProfileVectors 从当前 Store 的兄弟索引子树（同一工作区目录
// 下、其他 chunk profile 版本的子树）读取兼容向量，注入 prior 作为最低
// 优先级的复用来源。chunk profile 升版会换子树，若不复用，升级用户要为
// 整个仓库重新嵌入付费；一次真实的 v6 到 v7 升级里 524,885 个键中 99.927%
// 可以直接复用。
//
// 边界：
//   - 只枚举当前子树的父目录（cache 管理的同一工作区目录）下的子目录，
//     跳过作为符号链接的候选子树入口；子树内部文件仍按普通路径读取。
//   - 候选 manifest 的工作区身份、引擎、embedding provider、模型、维度、
//     dtype 与 ProfileHash 必须与当前配置逐项一致；chunk profile 版本可以
//     不同。任一项不同都表示向量来自另一套嵌入，混用会让相似度失去
//     意义。
//   - 旧子树只读；打不开、没有可用 revision 或不兼容的候选直接跳过，
//     缺口留给 provider 正常嵌入。
//   - 候选按向量数多、激活时间新排序，只采纳第一个能完整加载的：多棵
//     子树同时常驻会成倍占用内存。候选 active 段的实际加载数必须等于
//     manifest 宣称的段数且大于 0，部分损坏的候选被跳过，后面健康的
//     候选仍有机会被采纳。
//   - 采纳的候选其数据被 prior 的哈希表引用，索引归 prior 持有，随构建
//     收尾统一释放；落选候选立即释放。
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
		loaded := e.loadPriorVectors(candidate.store, candidate.manifest, nil)
		// 候选 active 段必须全部加载成功且至少有一段：部分损坏的候选若
		// 因为还剩几条向量就被采纳，后面健康的候选就没有机会了。
		if loaded.activeLoadedSegments != loaded.activeExpectedSegments || loaded.activeLoadedSegments == 0 {
			loaded.release() // 落选候选立即释放，映射与堆数据不留到进程退出
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
		// 采纳：crossProfileByHash 的值引用候选的向量数据，底层索引归 prior
		// 持有，随构建收尾统一释放。
		prior.indexes = append(prior.indexes, loaded.indexes...)
		return
	}
}

// compatibleSiblingManifest 判断一棵兄弟子树的 manifest 能否作为向量复用
// 来源：必须是本引擎产出、有向量、工作区身份（规范路径、路径类型、宿主
// OS）与当前一致，且 embedding provider、模型、维度、dtype、ProfileHash
// 与当前配置逐项相同。chunk profile 版本不在比较项内，升版前后的子树
// 正是要复用的对象。
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
