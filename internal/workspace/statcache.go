package workspace

import (
	"sync"
	"time"
)

// StatCache 是扫描期的 (size, mtime) → blobName 短路缓存(T11 分档
// 证据驱动:no-op sync 每轮全量读内容重哈希,95K 文件档把查询期
// 新鲜度检查钉在 ~1.7s;stat 短路后降至目录遍历量级)。
//
// 正确性边界(与 git index racy 语义同向,保守化):
//   - 命中条件 = size 与 mtimeNs 均一致;
//   - mtime 落在当前时刻 racyWindow 内的文件永远重哈希——防"同秒内
//     改内容且尺寸不变"漏检(文件系统 mtime 粒度);
//   - 主动回拨 mtime 且保持尺寸不变的改动不可检测,属接受的边界
//     (与 git 相同),内容门禁语义不受影响(缓存只收录可索引文件)。
//
// 并发:Engine 每 workspace 一个实例,构建持写锁串行,仍加锁自卫。
type StatCache struct {
	mu      sync.Mutex
	entries map[string]statEntry
}

type statEntry struct {
	size     int64
	mtimeNs  int64
	blobName string
}

// racyWindow 内的 mtime 视为"可能仍在写入",不走短路。
const racyWindow = 2 * time.Second

// NewStatCache 构造空缓存。
func NewStatCache() *StatCache {
	return &StatCache{entries: make(map[string]statEntry)}
}

// lookup 返回命中的 blobName;mtime 落在 racy 窗口内一律 miss。
func (c *StatCache) lookup(rel string, size int64, mtime time.Time, now time.Time) (string, bool) {
	if c == nil {
		return "", false
	}
	if now.Sub(mtime) < racyWindow {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[rel]
	if !ok || entry.size != size || entry.mtimeNs != mtime.UnixNano() {
		return "", false
	}
	return entry.blobName, true
}

// store 记录一次真实哈希结果。
func (c *StatCache) store(rel string, size int64, mtime time.Time, blobName string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries[rel] = statEntry{size: size, mtimeNs: mtime.UnixNano(), blobName: blobName}
	c.mu.Unlock()
}

// prune 丢弃本轮未见路径(删除/改名/变为不可索引的文件),防陈条目
// 无界增长与"复活"误命中。
func (c *StatCache) prune(seen map[string]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for rel := range c.entries {
		if !seen[rel] {
			delete(c.entries, rel)
		}
	}
	c.mu.Unlock()
}
