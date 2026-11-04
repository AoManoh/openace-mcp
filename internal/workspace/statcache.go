package workspace

import (
	"os"
	"sync"
	"time"
)

// StatCache 是扫描期的文件 stat → blobName 短路缓存(T11 分档
// 证据驱动:no-op sync 每轮全量读内容重哈希,95K 文件档把查询期
// 新鲜度检查钉在 ~1.7s;stat 短路后降至目录遍历量级)。
//
// 正确性边界(与 git index racy 语义同向,保守化):
//   - 命中条件 = statKey 各字段全部一致:大小、修改时间,Linux 与 macOS 上
//     另加 ctime、inode 与设备号。tar -x、rsync -a、cp -p 这类保留修改时间的
//     等长改写不改变大小与修改时间,只比较这两项会一直命中改写前的哈希;
//     ctime 由内核在内容或元数据变化时更新,普通工具改不了,rename 覆盖会换
//     inode,两者都能发现这类改写。其他平台的 ctime 与 inode 记为 0,命中
//     条件退回大小与修改时间;
//   - 修改时间或 ctime 落在当前时刻 racyWindow 内的文件永远重哈希——防"同秒内
//     改内容且尺寸不变"漏检(文件系统时间戳粒度);
//   - 哈希时修改时间或 ctime 距扫描开始不足 racyWindow 的结果不存入:哈希之后
//     同一时间刻内的等长改写不改变 stat,存入的记录在窗口过去后会被当作有效
//     命中,这次改写要等文件再次变化才被发现。git 对 mtime 不早于 index 写入
//     时刻的条目同样不信任 stat 信息(racy-git 文档);
//   - 内容门禁语义不受影响(缓存只收录可索引文件)。
//
// 并发:Engine 每 workspace 一个实例,构建持写锁串行,仍加锁自卫。
type StatCache struct {
	mu      sync.Mutex
	entries map[string]statEntry
	// gen 在条目新增、改变或删除时加一;保存 stat 快照的一方比较它决定是否需要重写。
	gen uint64
}

// Generation 返回条目变化的计数,两次读数相同说明期间条目没有变化。
func (c *StatCache) Generation() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

type statEntry struct {
	key      statKey
	blobName string
}

// statKey 是 stat 缓存比较的文件身份。ctimeNs、ino、dev 只在 Linux 与 macOS 上
// 取值(statcache_linux.go、statcache_darwin.go),其他平台为 0。
type statKey struct {
	size    int64
	mtimeNs int64
	ctimeNs int64
	ino     uint64
	dev     uint64
}

// statKeyOf 从扫描时的 os.FileInfo 取出比较字段。
func statKeyOf(info os.FileInfo) statKey {
	key := statKey{size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
	fillPlatformStat(&key, info)
	return key
}

// changedAt 返回修改时间与 ctime 中较晚的一个,racy 窗口按它判断:保留修改时间
// 的改写只推进 ctime,同一时间刻内的第二次等长改写同样要防。
func (k statKey) changedAt() time.Time {
	if k.ctimeNs > k.mtimeNs {
		return time.Unix(0, k.ctimeNs)
	}
	return time.Unix(0, k.mtimeNs)
}

// racyWindow 内的 mtime 视为"可能仍在写入",不走短路。
const racyWindow = 2 * time.Second

// NewStatCache 构造空缓存。
func NewStatCache() *StatCache {
	return &StatCache{entries: make(map[string]statEntry)}
}

// lookup 返回命中的 blobName;修改时间或 ctime 落在 racy 窗口内一律 miss。
func (c *StatCache) lookup(rel string, key statKey, now time.Time) (string, bool) {
	if c == nil {
		return "", false
	}
	if now.Sub(key.changedAt()) < racyWindow {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[rel]
	if !ok || entry.key != key {
		return "", false
	}
	return entry.blobName, true
}

// store 记录一次真实哈希结果。now 是本轮扫描开始时刻;修改时间或 ctime 距 now
// 不足 racyWindow 时不存入,并删掉该路径的旧记录,下次扫描重新读内容。
func (c *StatCache) store(rel string, key statKey, blobName string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, existed := c.entries[rel]
	if now.Sub(key.changedAt()) < racyWindow {
		if existed {
			delete(c.entries, rel)
			c.gen++
		}
		return
	}
	entry := statEntry{key: key, blobName: blobName}
	if !existed || old != entry {
		c.entries[rel] = entry
		c.gen++
	}
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
			c.gen++
		}
	}
	c.mu.Unlock()
}
