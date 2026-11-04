package localengine

import (
	"fmt"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// sharedVectorsDir 返回当前 cache 与 embedding 身份对应的共享向量库目录。
// 身份段与索引子树后缀相同（ProfileHash 加模板版本），不同 provider、端点、
// 模型、维度或模板的向量分开存放，不会混用。
func (e *Engine) sharedVectorsDir() (string, error) {
	snap, err := workspace.CurrentCacheSnapshot()
	if err != nil {
		return "", err
	}
	return index.SharedVectorsDir(snap.Dir, snap.Namespace, e.embedCfg.ProfileHash()+"-"+embedTemplateVersion), nil
}

// sharedVectors 返回共享向量库，第一次调用时打开。
func (e *Engine) sharedVectors() (*index.SharedVectors, error) {
	e.sharedMu.Lock()
	defer e.sharedMu.Unlock()
	if e.shared != nil {
		return e.shared, nil
	}
	dir, err := e.sharedVectorsDir()
	if err != nil {
		return nil, err
	}
	shared, err := index.OpenSharedVectors(dir, e.embedCfg.Dimension)
	if err != nil {
		return nil, err
	}
	e.shared = shared
	return shared, nil
}

// reuseSharedVectors 在共享向量库里查找缺口键，命中记录的位置写入 reuse 并计入
// out.sharedReused，返回仍需送审的键与对应的记录下标（idx 与 hashes 逐项对应）。
// 查找时整条读出校验，但只保留位置，写分段时再按位置读回：另一个 worktree 首建
// 时几乎每个 chunk 都会命中，把命中的向量留在堆上会占用数百 MiB 到 GiB。
// 共享库读不出时全部缺口照常送审，原因写进 out.sharedError：共享库只用来省
// 费用与时间，不影响结果是否正确。
func (e *Engine) reuseSharedVectors(hashes []string, idx []int, reuse map[string]vecRef, out *semanticOutcome) ([]string, []int) {
	shared, err := e.sharedVectors()
	var hits map[string]index.SharedRef
	if err == nil {
		hits, err = shared.LookupRefs(hashes)
	}
	if err != nil {
		out.sharedError = sanitizeError(fmt.Errorf("读取共享向量库失败，缺口照常送审: %w", err))
		return hashes, idx
	}
	if len(hits) == 0 {
		return hashes, idx
	}
	keptHashes := make([]string, 0, len(hashes)-len(hits))
	keptIdx := make([]int, 0, len(hashes)-len(hits))
	for i, key := range hashes {
		if ref, ok := hits[key]; ok {
			reuse[key] = vecRef{shared: &ref}
			out.sharedReused++
			continue
		}
		keptHashes = append(keptHashes, key)
		keptIdx = append(keptIdx, idx[i])
	}
	return keptHashes, keptIdx
}

// appendSharedVectors 把一批已付费、已归一化的向量记进共享向量库。
func (e *Engine) appendSharedVectors(vectors map[string][]float32) error {
	if len(vectors) == 0 {
		return nil
	}
	shared, err := e.sharedVectors()
	if err != nil {
		return fmt.Errorf("打开共享向量库: %w", err)
	}
	if err := shared.Append(vectors); err != nil {
		return fmt.Errorf("写入共享向量库: %w", err)
	}
	return nil
}

// sharedVectorsBytes 返回共享向量库在磁盘上的字节数，供状态显示；目录不存在
// 或读不出时返回 0，状态查询不因这项附加信息失败，也不为它创建目录。
func (e *Engine) sharedVectorsBytes() int64 {
	dir, err := e.sharedVectorsDir()
	if err != nil {
		return 0
	}
	size, err := index.SharedVectorsSize(dir)
	if err != nil {
		return 0
	}
	return size
}
