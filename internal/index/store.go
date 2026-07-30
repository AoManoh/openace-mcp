package index

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	activeFileName = "active.json"
	manifestsDir   = "manifests"
	segmentsDir    = "segments"
	stagingDir     = "staging"
	dirPerm        = 0o700
	filePerm       = 0o600
)

// ChunksFileName 是 segment 内 chunk 数据文件名（JSONL，每行一个 chunk）。
const ChunksFileName = "chunks.jsonl"

// LexicalDirName 是 segment 内 Bleve 索引目录名。
const LexicalDirName = "lexical.bleve"

// Store 管理单个 <workspace, profile> 的索引持久化目录。
// daemon 内同一 Store 的 Publish 由上层 singleflight 串行化。
type Store struct {
	root string
}

// NewStore 定位（必要时创建）索引根目录：
// <cacheDir>/<namespace>/engines/local-hybrid/<workspaceKey>/<profileKey>/
func NewStore(cacheDir string, namespace string, workspaceKey string, profileKey string) (*Store, error) {
	if cacheDir == "" || namespace == "" || workspaceKey == "" || profileKey == "" {
		return nil, errors.New("index store 需要非空的 cache/namespace/workspace/profile 标识")
	}
	root := filepath.Join(cacheDir, namespace, "engines", "local-hybrid", workspaceKey, profileKey)
	for _, sub := range []string{manifestsDir, segmentsDir, stagingDir} {
		if err := os.MkdirAll(filepath.Join(root, sub), dirPerm); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

// Root 返回该 Store 的根目录（供状态上报）。
func (s *Store) Root() string { return s.root }

// NewBuildID 生成一次构建的随机标识（同时用作 segment ID 与 revision 后缀）。
func NewBuildID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// BeginStaging 创建并返回一次构建的 staging 目录。
func (s *Store) BeginStaging(buildID string) (string, error) {
	path := filepath.Join(s.root, stagingDir, buildID)
	if err := os.MkdirAll(path, dirPerm); err != nil {
		return "", err
	}
	return path, nil
}

// DiscardStaging 删除一次构建的 staging 目录（取消/失败路径）。
func (s *Store) DiscardStaging(buildID string) error {
	return os.RemoveAll(filepath.Join(s.root, stagingDir, buildID))
}

// CleanupStaging 清空全部残留 staging（启动时调用；暗坑 K16）。
// active/previous revision 不受影响。
func (s *Store) CleanupStaging() error {
	entries, err := os.ReadDir(filepath.Join(s.root, stagingDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(s.root, stagingDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Publish 原子发布一次构建：
//  1. staging 目录整体改名为 segments/<segmentID>（同卷 rename）；
//  2. 写入 manifests/<revision>.json（temp+rename）；
//  3. 更新 active.json 指针（temp+Sync+rename+父目录 fsync）。
//
// 任一步骤中断都不影响当前 active revision 的可用性。
func (s *Store) Publish(manifest *Manifest, stagingPath string) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	segmentPath := filepath.Join(s.root, segmentsDir, manifest.SegmentID)
	if _, err := os.Stat(segmentPath); err == nil {
		return fmt.Errorf("segment %s 已存在，segment 不可覆盖", manifest.SegmentID)
	}
	if err := os.Rename(stagingPath, segmentPath); err != nil {
		return fmt.Errorf("发布 segment: %w", err)
	}
	// 目录项持久化与 active.json 的强度对齐（review S4，K1 原文落实）。
	syncDirBestEffort(filepath.Join(s.root, segmentsDir))
	manifestPath := filepath.Join(s.root, manifestsDir, manifest.Revision+".json")
	if err := writeFileAtomic(manifestPath, mustJSON(manifest)); err != nil {
		return fmt.Errorf("写入 manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(s.root, activeFileName), mustJSON(activePointer{Revision: manifest.Revision})); err != nil {
		return fmt.Errorf("更新 active 指针: %w", err)
	}
	return nil
}

// ActiveRevision 读取 active 指针；不存在时返回 ok=false。
func (s *Store) ActiveRevision() (string, bool, error) {
	var pointer activePointer
	err := decodeJSONFile(filepath.Join(s.root, activeFileName), &pointer)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if pointer.Revision == "" {
		return "", false, errors.New("active.json 缺少 revision")
	}
	return pointer.Revision, true, nil
}

// LoadManifest 读取并校验指定 revision 的 manifest。
func (s *Store) LoadManifest(revision string) (*Manifest, error) {
	var manifest Manifest
	if err := decodeJSONFile(filepath.Join(s.root, manifestsDir, revision+".json"), &manifest); err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// SegmentPath 返回 manifest 对应的 segment 目录。
func (s *Store) SegmentPath(manifest *Manifest) string {
	return filepath.Join(s.root, segmentsDir, manifest.SegmentID)
}

// VerifyManifest 校验 revision 数据完整性：chunks checksum 与 lexical 目录存在性。
// Bleve 索引自身的深度校验由打开动作完成（打开失败视为损坏）。
func (s *Store) VerifyManifest(manifest *Manifest) error {
	segment := s.SegmentPath(manifest)
	sum, err := ChecksumFile(filepath.Join(segment, ChunksFileName))
	if err != nil {
		return fmt.Errorf("读取 chunks 数据: %w", err)
	}
	if sum != manifest.ChunksChecksum {
		return fmt.Errorf("chunks checksum 不匹配: manifest=%s 实际=%s", manifest.ChunksChecksum, sum)
	}
	if info, err := os.Stat(filepath.Join(segment, LexicalDirName)); err != nil || !info.IsDir() {
		return fmt.Errorf("lexical 索引目录缺失: %v", err)
	}
	return nil
}

// ListRevisions 返回全部持久化 manifest 的 revision 标识（无序）。
func (s *Store) ListRevisions() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, manifestsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	revisions := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		revisions = append(revisions, name[:len(name)-len(".json")])
	}
	return revisions, nil
}

// RemoveRevision 删除一个 revision 的 segment 与 manifest。
// 调用方必须保证该 revision 不是 active/previous 且无打开句柄。
// 删除顺序为 segment 目录先、manifest 后：中断残留的孤儿 manifest
// 在校验时会自然失败并被后续 GC 重试。
func (s *Store) RemoveRevision(revision string) error {
	manifest, err := s.LoadManifest(revision)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		// manifest 已不可读：仍尝试移除残留文件。
		return os.Remove(filepath.Join(s.root, manifestsDir, revision+".json"))
	}
	if err := os.RemoveAll(s.SegmentPath(manifest)); err != nil {
		return err
	}
	return os.Remove(filepath.Join(s.root, manifestsDir, revision+".json"))
}

// ErrNoUsableRevision 表示 active 与 previous 均不可用。
var ErrNoUsableRevision = errors.New("没有可用的索引 revision")

// MaxRevisionChain 是 previous 链遍历的深度上限（review S3）：正常保留
// 链长 ≤2（active+previous），上限只在链被外部损坏/成环时兜底。
const MaxRevisionChain = 16

// ResolveUsable 返回首个通过校验的 revision：active 优先，损坏时沿
// previous 链回退（暗坑 K1/K2 的恢复路径）。回退发生时返回的 degradedFrom
// 记录被跳过的损坏 revision，供状态上报。链遍历带环检测与深度上限
// （review S3：被外部编辑成环的 manifest 不得挂起 daemon）。
func (s *Store) ResolveUsable() (*Manifest, []string, error) {
	revision, ok, err := s.ActiveRevision()
	if err != nil || !ok {
		return nil, nil, errOrNoRevision(err)
	}
	var skipped []string
	visited := make(map[string]bool)
	for revision != "" && !visited[revision] && len(visited) < MaxRevisionChain {
		visited[revision] = true
		manifest, err := s.LoadManifest(revision)
		if err != nil {
			skipped = append(skipped, revision)
			break
		}
		if verifyErr := s.VerifyManifest(manifest); verifyErr == nil {
			return manifest, skipped, nil
		}
		skipped = append(skipped, revision)
		revision = manifest.PreviousRevision
	}
	return nil, skipped, ErrNoUsableRevision
}

// CleanupOrphanSegments 删除不被任何 manifest 引用的 segment 目录
// （review S5：Publish 在 rename 之后中断会留下孤儿 segment）。
// 与 CleanupStaging 同时机（store 创建时）调用；单 daemon 所有权下
// 此时不存在在飞发布（跨进程共享 cache 的防护属 Stage 4 议题 S15）。
func (s *Store) CleanupOrphanSegments() error {
	revisions, err := s.ListRevisions()
	if err != nil {
		return err
	}
	referenced := make(map[string]bool, len(revisions))
	for _, revision := range revisions {
		if manifest, err := s.LoadManifest(revision); err == nil {
			referenced[manifest.SegmentID] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.root, segmentsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || referenced[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, segmentsDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func errOrNoRevision(err error) error {
	if err != nil {
		return err
	}
	return ErrNoUsableRevision
}

// writeFileAtomic 以"同目录 temp + Sync + rename + 父目录 fsync"写文件，
// 保证读者要么看到旧内容要么看到完整新内容（暗坑 K1）。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDirBestEffort(dir)
	return nil
}

// syncDirBestEffort 对目录执行 fsync；Windows 与部分挂载文件系统
// 不支持目录 fsync，失败时忽略（文件本身已 Sync，rename 原子性由
// 文件系统保证）。
func syncDirBestEffort(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func mustJSON(value any) []byte {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	return data
}
