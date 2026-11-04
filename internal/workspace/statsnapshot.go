package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// StatSnapshotFile 是 stat 快照在索引子树目录里的文件名,与 active.json 同目录。
// 快照只用来让重启后的第一次扫描跳过未变文件的重新哈希;丢失或损坏只多一次
// 重新哈希,不影响正确性,也不属于索引格式。
const StatSnapshotFile = "stat-snapshot.json"

// ScanRulesVersion 是扫描规则的实现版本:内容门禁(大小上限、二进制判定)、忽略规则
// 与 blobName 的计算方式。这些实现一变,同一文件的收录与否或 blobName 就可能不同,
// 旧二进制保存的快照必须整份作废。改动 syncer.go 的收录判定、忽略规则或 blobName 时
// 把它加一。
const ScanRulesVersion = "1"

// statSnapshotFormat 是快照文件结构的版本,字段含义变化时加一。
const statSnapshotFormat = 1

// StatSnapshotMeta 是快照的归属:工作区规范路径、索引子树名与切分版本都与当前一致才加载。
// 子树名包含 chunk profile、词法评分模型与 embedding 身份;切分版本另列,便于状态说明原因。
type StatSnapshotMeta struct {
	Workspace      string
	Subtree        string
	ChunkerVersion string
}

// StatSnapshotSave 是一次保存的结果:写入的条目数与保存时缓存的变化计数。
type StatSnapshotSave struct {
	Entries    int
	Generation uint64
}

// StatSnapshotLoad 是一次加载的结果。Cache 为 nil 时没有可用快照:Ignored 为空表示文件
// 不存在,不为空时是整份作废的原因。Dropped 是内容哈希没有通过调用方检查而丢弃的条目数。
type StatSnapshotLoad struct {
	Cache   *StatCache
	Loaded  int
	Dropped int
	Ignored string
}

type statSnapshotFile struct {
	Format           int                 `json:"format"`
	Workspace        string              `json:"workspace"`
	Subtree          string              `json:"subtree"`
	ScanRulesVersion string              `json:"scan_rules_version"`
	ChunkerVersion   string              `json:"chunker_version"`
	SavedAt          time.Time           `json:"saved_at"`
	Entries          []statSnapshotEntry `json:"entries"`
}

type statSnapshotEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns,omitempty"`
	Ino     uint64 `json:"ino,omitempty"`
	Dev     uint64 `json:"dev,omitempty"`
	Blob    string `json:"blob"`
}

// SaveSnapshot 把当前条目写成 JSON 快照。缓存只收录已离开 racyWindow 的哈希结果,快照里
// 的每条也都满足这个条件。写临时文件、fsync、rename 原子替换,再 fsync 目录,进程在任何
// 时刻被杀,磁盘上要么是旧快照,要么是完整的新快照。
func (c *StatCache) SaveSnapshot(path string, meta StatSnapshotMeta, now time.Time) (StatSnapshotSave, error) {
	if c == nil {
		return StatSnapshotSave{}, errors.New("stat cache is nil")
	}
	c.mu.Lock()
	gen := c.gen
	entries := make([]statSnapshotEntry, 0, len(c.entries))
	for rel, e := range c.entries {
		entries = append(entries, statSnapshotEntry{Path: rel, Size: e.key.size, MtimeNs: e.key.mtimeNs,
			CtimeNs: e.key.ctimeNs, Ino: e.key.ino, Dev: e.key.dev, Blob: e.blobName})
	}
	c.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	data, err := json.Marshal(statSnapshotFile{Format: statSnapshotFormat, Workspace: meta.Workspace, Subtree: meta.Subtree,
		ScanRulesVersion: ScanRulesVersion, ChunkerVersion: meta.ChunkerVersion, SavedAt: now.UTC(), Entries: entries})
	if err != nil {
		return StatSnapshotSave{}, err
	}
	if err := writeFileAtomic(path, data); err != nil {
		return StatSnapshotSave{}, err
	}
	return StatSnapshotSave{Entries: len(entries), Generation: gen}, nil
}

// LoadStatSnapshot 读取快照并返回装好条目的缓存。归属字段任一与 meta 不同、格式或扫描
// 规则版本不同、文件读不出或解码失败,都整份作废并写明原因。keep 对每条记录做内容核对
// (调用方用 active manifest 的文件哈希),返回 false 的记录只丢这一条,下次扫描时对应
// 文件重新哈希。
func LoadStatSnapshot(path string, meta StatSnapshotMeta, keep func(rel, blob string) bool) StatSnapshotLoad {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return StatSnapshotLoad{}
	}
	if err != nil {
		return StatSnapshotLoad{Ignored: "read failed: " + err.Error()}
	}
	var file statSnapshotFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return StatSnapshotLoad{Ignored: "decode failed: " + err.Error()}
	}
	switch {
	case file.Format != statSnapshotFormat:
		return StatSnapshotLoad{Ignored: fmt.Sprintf("format %d differs from %d", file.Format, statSnapshotFormat)}
	case file.Workspace != meta.Workspace:
		return StatSnapshotLoad{Ignored: fmt.Sprintf("workspace %q differs from %q", file.Workspace, meta.Workspace)}
	case file.Subtree != meta.Subtree:
		return StatSnapshotLoad{Ignored: fmt.Sprintf("subtree %q differs from %q", file.Subtree, meta.Subtree)}
	case file.ScanRulesVersion != ScanRulesVersion:
		return StatSnapshotLoad{Ignored: fmt.Sprintf("scan_rules_version %q differs from %q", file.ScanRulesVersion, ScanRulesVersion)}
	case file.ChunkerVersion != meta.ChunkerVersion:
		return StatSnapshotLoad{Ignored: fmt.Sprintf("chunker version %q differs from %q", file.ChunkerVersion, meta.ChunkerVersion)}
	}
	cache := NewStatCache()
	result := StatSnapshotLoad{Cache: cache}
	for _, e := range file.Entries {
		if e.Path == "" || e.Blob == "" || (keep != nil && !keep(e.Path, e.Blob)) {
			result.Dropped++
			continue
		}
		cache.entries[e.Path] = statEntry{key: statKey{size: e.Size, mtimeNs: e.MtimeNs, ctimeNs: e.CtimeNs, ino: e.Ino, dev: e.Dev},
			blobName: e.Blob}
		result.Loaded++
	}
	return result
}

// writeFileAtomic 在目标目录写临时文件、fsync 后 rename 覆盖目标,最后 fsync 目录让
// rename 持久化。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
