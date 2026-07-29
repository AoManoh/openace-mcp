// Package localengine 实现 local-hybrid 检索引擎的 Stage 2 形态：
// 本地扫描（复用 workspace AssetPolicy）→ 确定性 chunk → Bleve BM25
// 词法索引 → immutable revision 发布与检索。无任何远程依赖；词法
// 检索是本模式的完整能力，不是降级（阶段计划 D2）。
package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AoManoh/openace-mcp/internal/chunk"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

const (
	// EngineID 是引擎标识，进入 manifest、状态与检索结果。
	EngineID = "local-hybrid"
	// EngineVersion 随引擎行为不兼容变化而更新。
	EngineVersion = "stage2"
	// policyHash 标识当前复用的 workspace AssetPolicy 版本。
	policyHash = "workspace-assetpolicy-v1"
	// defaultTopK 是词法召回的候选深度。
	defaultTopK = 20
)

// Engine 是 local-hybrid 引擎；daemon 进程内唯一实例，
// 持有全部索引句柄与构建 singleflight。
type Engine struct {
	profile chunk.Profile

	mu       sync.Mutex
	inflight map[string]*buildCall
	statuses map[string]*wsStatus
	stores   map[string]*index.Store
	handles  map[string]*revisionHandle
	closed   bool
}

// 编译期断言：Engine 满足全部通用 contract。
var (
	_ engine.Service            = (*Engine)(nil)
	_ engine.WorkspaceInspector = (*Engine)(nil)
	_ engine.ChangeDetector     = (*Engine)(nil)
	_ engine.BackgroundSyncer   = (*Engine)(nil)
	_ engine.Lifecycle          = (*Engine)(nil)
)

// New 创建 local-hybrid 引擎。
func New() *Engine {
	return &Engine{
		profile:  chunk.DefaultProfile(),
		inflight: make(map[string]*buildCall),
		statuses: make(map[string]*wsStatus),
		stores:   make(map[string]*index.Store),
		handles:  make(map[string]*revisionHandle),
	}
}

// EngineID 实现 engine.Identifier。
func (e *Engine) EngineID() string {
	return engine.EngineLocalHybrid
}

// rejectProfileID 拒绝 legacy ACE 专属的 provider_profile_id（迁移方案 §7.3）。
func rejectProfileID(ref engine.WorkspaceRef) error {
	if strings.TrimSpace(ref.ProviderProfileID) != "" {
		return fmt.Errorf("provider_profile_id 仅适用于 legacy ACE 引擎；local-hybrid 不接受该参数（收到 %q）", ref.ProviderProfileID)
	}
	return nil
}

// resolveRoot 规范化工作区并返回 store 键身份。
func (e *Engine) resolveRoot(dir string) (pathutil.WorkspaceRoot, string, error) {
	root, err := pathutil.ResolveWorkspaceRoot(dir)
	if err != nil {
		return pathutil.WorkspaceRoot{}, "", err
	}
	sum := sha256.Sum256([]byte(root.CanonicalPath + "\x00" + string(root.PathKind) + "\x00" + root.HostOS))
	key := sanitizeKey(filepath.Base(root.CanonicalPath)) + "-" + hex.EncodeToString(sum[:])[:12]
	return root, key, nil
}

// sanitizeKey 把目录名收敛为安全的路径片段。
func sanitizeKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "ws"
	}
	const maxLen = 32
	out := b.String()
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}

// storeFor 返回（必要时创建）工作区的索引 Store，并完成一次性 staging 清理。
func (e *Engine) storeFor(workspaceKey string) (*index.Store, error) {
	e.mu.Lock()
	if store, ok := e.stores[workspaceKey]; ok {
		e.mu.Unlock()
		return store, nil
	}
	e.mu.Unlock()

	cache, err := workspace.CurrentCacheSnapshot()
	if err != nil {
		return nil, err
	}
	store, err := index.NewStore(cache.Dir, cache.Namespace, workspaceKey, e.profile.ID+"-v"+e.profile.Version)
	if err != nil {
		return nil, err
	}
	if err := store.CleanupStaging(); err != nil {
		return nil, fmt.Errorf("清理残留 staging: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.stores[workspaceKey]; ok {
		return existing, nil
	}
	e.stores[workspaceKey] = store
	return store, nil
}

// Sync 实现 engine.Service。
func (e *Engine) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return e.syncWorkspace(ctx, req.Workspace)
}

// SyncBackground 实现 engine.BackgroundSyncer；Stage 2 与 Sync 同路径。
func (e *Engine) SyncBackground(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return e.syncWorkspace(ctx, req.Workspace)
}

// buildCall 是同工作区并发构建的共享执行体（暗坑 K12）。
type buildCall struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	cancelled bool
	result    engine.Result
	err       error
}

// syncWorkspace 以 singleflight 语义执行一次索引构建。
func (e *Engine) syncWorkspace(ctx context.Context, ref engine.WorkspaceRef) (engine.Result, error) {
	if err := rejectProfileID(ref); err != nil {
		return engine.Result{}, err
	}
	root, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return engine.Result{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return engine.Result{}, err
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return engine.Result{}, errors.New("local-hybrid 引擎已关闭")
		}
		if call, ok := e.inflight[workspaceKey]; ok {
			if call.cancelled {
				done := call.done
				e.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return engine.Result{}, ctx.Err()
				}
			}
			call.waiters++
			e.mu.Unlock()
			return e.waitBuild(ctx, workspaceKey, call)
		}
		runCtx, cancel := context.WithCancel(context.Background())
		call := &buildCall{done: make(chan struct{}), cancel: cancel, waiters: 1}
		e.inflight[workspaceKey] = call
		e.mu.Unlock()

		go func() {
			result, err := e.runBuild(runCtx, root, workspaceKey)
			call.result, call.err = result, err
			close(call.done)
			e.mu.Lock()
			delete(e.inflight, workspaceKey)
			e.mu.Unlock()
		}()
		return e.waitBuild(ctx, workspaceKey, call)
	}
}

// waitBuild 等待共享构建完成；最后一个等待者取消时中止构建。
func (e *Engine) waitBuild(ctx context.Context, workspaceKey string, call *buildCall) (engine.Result, error) {
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
	}
	e.mu.Lock()
	call.waiters--
	if call.waiters <= 0 && !call.cancelled {
		call.cancelled = true
		call.cancel()
	}
	e.mu.Unlock()
	return engine.Result{}, ctx.Err()
}

// WorkspaceChanged 实现 engine.ChangeDetector：对比当前文件内容身份
// 与 active manifest 的差异；无 revision 时视为已变化。
func (e *Engine) WorkspaceChanged(ctx context.Context, ref engine.WorkspaceRef) (bool, error) {
	if err := rejectProfileID(ref); err != nil {
		return false, err
	}
	root, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return false, err
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return false, err
	}
	manifest, _, err := store.ResolveUsable()
	if err != nil {
		if errors.Is(err, index.ErrNoUsableRevision) {
			return true, nil
		}
		return false, err
	}
	assets, err := workspace.FileAssetSource{}.Load(ctx, root.CanonicalPath)
	if err != nil {
		return false, err
	}
	if len(assets) != len(manifest.Files) {
		return true, nil
	}
	for _, asset := range assets {
		entry, ok := manifest.Files[asset.RelPath]
		if !ok || entry.ContentHash != asset.BlobName {
			return true, nil
		}
	}
	return false, nil
}

// Close 实现 engine.Lifecycle：关闭全部索引句柄，拒绝后续请求。
func (e *Engine) Close(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	var firstErr error
	for revision, handle := range e.handles {
		handle.retired = true
		if handle.refs == 0 {
			if err := handle.lex.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			delete(e.handles, revision)
		}
	}
	return firstErr
}
