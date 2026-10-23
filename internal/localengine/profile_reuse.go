package localengine

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
// 下、chunk profile 版本或词法评分模型不同的子树）读取兼容向量，注入
// prior 作为最低优先级的复用来源。chunk profile 升版或词法评分模型变化
// （如 TF-IDF 时代子树迁到 +lex-bm25 子树）会换子树，若不复用，升级用户
// 要为整个仓库重新嵌入付费；一次真实的 v6 到 v7 升级里 524,885 个键中
// 99.927% 可以直接复用。兄弟子树 journal 与 Batch 作业里的已付费状态不在
// 这里处理，由 embedRecords 在送审前经 siblingPaidStateFor 只读检查。
//
// 边界：
//   - 只枚举当前子树的父目录（cache 管理的同一工作区目录）下的子目录，
//     跳过作为符号链接的候选子树入口；子树内部文件仍按普通路径读取。
//   - 候选 manifest 的工作区身份、引擎、embedding provider、模型、维度、
//     dtype 与 ProfileHash 必须与当前配置逐项一致；chunk profile 版本与
//     词法评分模型可以不同。任一项不同都表示向量来自另一套嵌入，混用会
//     让相似度失去意义。
//   - 旧子树只读，不打开其 Bleve 段；打不开、没有可用 revision 或不兼容
//     的候选直接跳过，缺口留给 provider 正常嵌入。
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

// siblingPaidState 是同一 embedding 身份的兄弟子树里与本次缺失键有关的已付费
// 状态，由 siblingPaidStateFor 在任何文档嵌入请求之前只读收集。
type siblingPaidState struct {
	// vectors 是缺失键在兄弟子树 journal 里已付费、尚未发布的向量。
	vectors map[string][]float32
	// rejected 是兄弟子树拒绝集里的缺失键：provider 曾对这些内容返回零向量
	// 或 NaN。
	rejected map[string]bool
	// blockReason 非空时本次不得为任何缺口送审，值是给用户的原因与恢复
	// 步骤：兄弟子树的状态无法检查或已损坏、有键集合未知的提交意向、有
	// 输入键清单不可用的 Batch 作业，或扣除复用与拒绝后仍有缺口登记在
	// 未回收结果的 Batch 作业里。
	blockReason string
}

// siblingPaidStateFor 在为 missing 送审之前，只读检查同一 embedding 身份兄弟
// 子树的 journal、拒绝集与批作业状态。换子树（如迁到 +lex-bm25 子树）后，
// 这些已付费状态仍留在旧子树；不检查就送审，同一内容会付两次费。每次有
// 缺口要送审时都检查一次（全量与 delta 构建、离线计划），所以新子树已经
// 完整之后再加回旧内容也受同样保护。旧子树的文件一律不改写、不截断、不
// 消费；确实不存在（没有这样的子树或子树里没有 journal）视为没有旧状态，
// 读不出或已损坏则阻止送审，不当作“没有旧状态”。
func (e *Engine) siblingPaidStateFor(current *index.Store, missing []string) siblingPaidState {
	state := siblingPaidState{vectors: map[string][]float32{}, rejected: map[string]bool{}}
	dirs, err := e.sameEmbeddingSiblingJournalDirs(current)
	if err != nil {
		state.blockReason = fmt.Sprintf("语义迁移已暂停：无法检查同一 embedding 身份的旧索引子树（%v），其中可能有已付费、尚未发布的向量或未回收的 Batch 作业，本次不为缺口送审；词法检索与已复用的向量照常可用。排除这个错误后重新 sync", err)
		return state
	}
	want := make(map[string]bool, len(missing))
	for _, key := range missing {
		want[key] = true
	}
	heldBy := map[string]string{}
	for _, dir := range dirs {
		vectors, rejected, err := index.ReadJournalSnapshot(dir, e.embedCfg.Dimension)
		if err != nil {
			state.blockReason = siblingJournalBlockReason(dir, err)
			return state
		}
		for key := range want {
			if vec, ok := vectors[key]; ok {
				if _, seen := state.vectors[key]; !seen {
					state.vectors[key] = vec
				}
			}
			if rejected[key] {
				state.rejected[key] = true
			}
		}
		path := filepath.Join(dir, bulkStateFileName)
		bulk, err := loadBulkState(path)
		if err != nil {
			state.blockReason = fmt.Sprintf("语义迁移已暂停：旧索引子树的批作业状态 %s 读不出来（%v），无法确认其中的作业是否已经计费，本次不为缺口送审；词法检索与已复用的向量照常可用。核对或修复该文件后重新 sync", path, err)
			return state
		}
		if bulk.ProfileHash != "" && bulk.ProfileHash != e.embedCfg.ProfileHash() {
			continue
		}
		if intent := bulk.PendingIntent; intent != nil {
			state.blockReason = fmt.Sprintf("语义迁移已暂停：旧索引子树的批作业状态 %s 有一条未收尾的提交意向（%d 条输入，keys_sha256=%s，记录于 %s），无法确定服务端是否已为它建了作业、作业覆盖哪些内容，本次不为任何缺口送审；词法检索与已复用的向量照常可用。恢复：用升级前的 openACE 版本对该工作区执行一次 sync，按它给出的核对步骤处理这条意向；或到 provider 控制台核对后人工修正该文件，再重新 sync", path, intent.Count, intent.KeysSHA256, intent.CreatedAt.Format(time.RFC3339))
			return state
		}
		for _, job := range bulk.Jobs {
			// 作业的输入键清单是判断它覆盖哪些已付费输入的唯一依据。清单缺失、
			// 为空或有不是 embedKey 格式的项时，没有匹配到的键不代表没有付费，
			// 这个作业可能覆盖任何缺口。
			if problem := unusableJobKeys(job.Keys); problem != "" {
				state.blockReason = fmt.Sprintf("语义迁移已暂停：旧索引子树的批作业状态 %s 登记的作业 %q 没有可用的输入键清单（%s），无法确定它覆盖哪些已付费输入，本次不为任何缺口送审；词法检索与已复用的向量照常可用。恢复：到 provider 控制台核对该作业，把完整的 keys（每项是 64 位小写十六进制的 embedKey）补回该文件，再用升级前的 openACE 版本对该工作区执行一次 sync 回收结果；或确认该作业已失效、不再需要后人工移走该状态文件再 sync（缺口会重新送审计费）", path, job.ID, problem)
				return state
			}
			for _, key := range job.Keys {
				if want[key] {
					heldBy[key] = path
				}
			}
		}
	}
	held := 0
	var paths []string
	seenPath := map[string]bool{}
	for key, path := range heldBy {
		if _, reused := state.vectors[key]; reused || state.rejected[key] {
			continue
		}
		held++
		if !seenPath[path] {
			seenPath[path] = true
			paths = append(paths, path)
		}
	}
	if held > 0 {
		sort.Strings(paths)
		state.blockReason = fmt.Sprintf("语义迁移已暂停：本次待送审的内容里有 %d 条登记在旧索引子树尚未回收结果的 Batch 作业中（状态文件 %s）。这些输入可能已在服务端计费，新索引子树不再送审，本次不为任何缺口送审，以免同一内容付两次费；词法检索与已复用的向量照常可用，未覆盖的 chunk 计入语义覆盖率。恢复：用升级前的 openACE 版本对该工作区执行一次 sync，由它回收作业结果并发布，之后本版本直接复用这些向量；或到 provider 控制台确认这些作业已失效或不再需要，人工移走该状态文件后重新 sync（此时缺口会重新送审计费）", held, strings.Join(paths, "、"))
	}
	return state
}

// unusableJobKeys 检查旧 Batch 作业登记的输入键清单能否确定作业覆盖的输入：
// 清单为空（keys 字段缺失、为 null 或空列表），或有一项不是 embedKey 的格式，
// 返回问题描述；清单可用时返回空串。
func unusableJobKeys(keys []string) string {
	if len(keys) == 0 {
		return "keys 缺失或为空"
	}
	for i, key := range keys {
		if !isEmbedKeyFormat(key) {
			shown := key
			if len(shown) > 80 {
				shown = shown[:80] + "…"
			}
			return fmt.Sprintf("第 %d 个键 %q 不是 64 位小写十六进制的 embedKey", i+1, shown)
		}
	}
	return ""
}

// isEmbedKeyFormat 判断 s 是否符合 embedKey 的格式：embedKey 是
// hex.EncodeToString(sha256) 的结果，恰好 64 个小写十六进制字符。
func isEmbedKeyFormat(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// siblingJournalBlockReason 按错误来源给出旧 journal 读不出时的原因与恢复步骤：
// 记录损坏与 I/O 失败的处理办法不同。
func siblingJournalBlockReason(dir string, err error) string {
	if errors.Is(err, index.ErrJournalCorrupt) {
		return fmt.Sprintf("语义迁移已暂停：旧索引子树的 embedding journal %s 不能完整读出（%v），损坏处及之后的已付费向量无法确认，本次不为缺口送审；词法检索与已复用的向量照常可用。恢复：用升级前的 openACE 版本对该工作区执行一次 sync（它会截掉损坏的尾部、补齐并发布缺口），之后本版本复用其发布的向量；或确认不再需要这些已付费向量后，人工把该 journal 目录移出旧子树再 sync（缺口会重新送审计费）", dir, err)
	}
	return fmt.Sprintf("语义迁移已暂停：读取旧索引子树的 embedding journal %s 失败（%v），无法确认其中是否有已付费、尚未发布的向量，本次不为缺口送审；词法检索与已复用的向量照常可用。排除这个读取错误（例如文件权限、磁盘或挂载问题）后重新 sync", dir, err)
}

// sameEmbeddingSiblingJournalDirs 返回同一工作区目录下、子树名以当前
// embeddingSubtreeSuffix 结尾的其他索引子树里存在的 journal 目录，即 embedding
// 身份相同而 chunk profile 或词法评分模型不同的子树。按目录名判断身份，不
// 要求子树有可用 revision：首建在嵌入中途被打断的旧子树只有 journal，没有
// 带向量的 revision。没有这样的子树、或子树里没有 journal 目录，返回空；
// 目录列不出、journal 路径状态读不出或不是目录、同名入口是符号链接（按复用
// 策略不跟随）时返回错误，调用方据此阻止送审。
func (e *Engine) sameEmbeddingSiblingJournalDirs(current *index.Store) ([]string, error) {
	parent := filepath.Dir(current.Root())
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, fmt.Errorf("列出工作区索引目录 %s: %w", parent, err)
	}
	suffix := embeddingSubtreeSuffix(e.embedCfg)
	var dirs []string
	for _, entry := range entries {
		candidateRoot := filepath.Join(parent, entry.Name())
		if !strings.HasSuffix(entry.Name(), suffix) || candidateRoot == current.Root() {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("旧索引子树入口 %s 是符号链接，按复用策略不跟随，无法确认其中的已付费状态", candidateRoot)
		}
		if !entry.IsDir() {
			continue
		}
		dir := index.JournalDirOf(candidateRoot)
		info, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("检查旧索引子树 journal %s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("旧索引子树 journal %s 不是目录", dir)
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

// compatibleSiblingManifest 判断一棵兄弟子树的 manifest 能否作为向量复用
// 来源：必须是本引擎产出、有向量、工作区身份（规范路径、路径类型、宿主
// OS）与当前一致，且 embedding provider、模型、维度、dtype、ProfileHash
// 与当前配置逐项相同。chunk profile 版本不在比较项内，升版前后的子树
// 正是要复用的对象；词法评分模型同样不在比较项内，向量只取决于嵌入
// 身份与 embedKey，与词法怎么打分无关。
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
