package localengine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AoManoh/openace-mcp/internal/index"
)

// locatedMeta 是一条 chunk 记录的 ID 与常驻元数据。segIdx 由组装存活集的
// revision 决定：分段缓存里一律为 0，组装时按该分段在 manifest.Segments 中的
// 位置填入。
type locatedMeta struct {
	id   string
	meta chunkMeta
}

// decodeSegmentChunksHook 在每次逐行解码一个分段的 chunks.jsonl 时调用，
// 参数为分段目录。只供测试统计解码次数，生产路径为 nil。
var decodeSegmentChunksHook func(dir string)

// segmentMetaEntry 是 Engine 级分段元数据缓存的一个条目：ready 在解码结束时
// 关闭，byFile 与 err 只在 ready 关闭后可读；dir 用于按索引子树清理。
type segmentMetaEntry struct {
	dir    string
	ready  chan struct{}
	byFile map[string][]locatedMeta
	err    error
}

// decodeSegmentMetas 逐行解码一个分段的 chunks.jsonl，按相对路径分组返回元数据与
// 各记录在文件内的偏移；内容本体不保留。读取中途出错时按已读部分结束，与改动前
// loadLiveChunkMetas 的行为一致；JSON 解码失败报错。
func decodeSegmentMetas(dir string) (map[string][]locatedMeta, error) {
	if hook := decodeSegmentChunksHook; hook != nil {
		hook(dir)
	}
	file, err := os.Open(filepath.Join(dir, index.ChunksFileName))
	if err != nil {
		return nil, fmt.Errorf("读取 segment chunk 数据: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	var offset int64
	byFile := make(map[string][]locatedMeta)
	for {
		line, err := reader.ReadBytes('\n')
		lineLen := len(line)
		trimmed := bytes.TrimRight(line, "\n")
		if len(trimmed) > 0 {
			var record chunkRecord
			if unmarshalErr := json.Unmarshal(trimmed, &record); unmarshalErr != nil {
				return nil, fmt.Errorf("chunk 数据损坏: %w", unmarshalErr)
			}
			byFile[record.RelPath] = append(byFile[record.RelPath], locatedMeta{
				id: record.ID,
				meta: chunkMeta{
					RelPath: record.RelPath, Language: record.Language,
					StartLine: record.StartLine, EndLine: record.EndLine,
					Symbol: record.Symbol, ContentHash: record.ContentHash,
					offset: offset, length: len(trimmed),
				},
			})
		}
		offset += int64(lineLen)
		if err != nil {
			break
		}
	}
	return byFile, nil
}

// assembleLiveMetas 由各分段（按 manifest.Segments 顺序）的元数据组装存活集：
// 同一相对路径出现在多个分段时，后面（更新）的分段整体覆盖前面的记录；路径
// 不在 manifest.Files 里（文件已删除或改名）的记录全部丢弃。返回的元数据是
// 拷贝，segIdx 填为该记录所在分段的位置，不改动缓存里的条目。
func assembleLiveMetas(manifest *index.Manifest, perSegment []map[string][]locatedMeta) map[string]chunkMeta {
	type newestGroup struct {
		segIdx int
		group  []locatedMeta
	}
	byFile := make(map[string]newestGroup, len(manifest.Files))
	for segIdx, segFiles := range perSegment {
		for path, group := range segFiles {
			byFile[path] = newestGroup{segIdx: segIdx, group: group}
		}
	}
	metas := make(map[string]chunkMeta)
	for path, newest := range byFile {
		if _, live := manifest.Files[path]; !live {
			continue
		}
		for _, item := range newest.group {
			meta := item.meta
			meta.segIdx = newest.segIdx
			metas[item.id] = meta
		}
	}
	return metas
}

// segmentMetas 返回一个分段按文件分组的元数据，同一分段（目录与 chunks 校验和
// 都相同）在本进程内只解码一次。并发请求同一分段时，后到者等待先到者解码完成
// 并共用结果；解码失败时删除占位，下一次重新解码。
func (e *Engine) segmentMetas(dir string, checksum string) (map[string][]locatedMeta, error) {
	key := dir + "\x00" + checksum
	e.segmentMetaMu.Lock()
	if e.segmentMetaCache == nil {
		e.segmentMetaCache = make(map[string]*segmentMetaEntry)
	}
	if entry, ok := e.segmentMetaCache[key]; ok {
		e.segmentMetaMu.Unlock()
		<-entry.ready
		return entry.byFile, entry.err
	}
	entry := &segmentMetaEntry{dir: dir, ready: make(chan struct{})}
	e.segmentMetaCache[key] = entry
	e.segmentMetaMu.Unlock()

	entry.byFile, entry.err = decodeSegmentMetas(dir)
	close(entry.ready)
	if entry.err != nil {
		e.segmentMetaMu.Lock()
		if e.segmentMetaCache[key] == entry {
			delete(e.segmentMetaCache, key)
		}
		e.segmentMetaMu.Unlock()
	}
	return entry.byFile, entry.err
}

// liveChunkMetas 是打开 revision 句柄时组装存活 chunk 元数据的入口，结果与
// loadLiveChunkMetas 相同。分段不可变，各分段的解码结果按分段缓存：delta 发布的
// 新 revision 与上一 revision 共享前面的分段，打开新句柄只解码新增分段。改动前
// 每次都逐行解码全部分段，一个 32 万 chunk 的工作区每个新 revision 要多花约 3 秒、
// 临时分配约 1.3 GiB。segmentDirs 必须与 manifest.Segments 一一对应。
func (e *Engine) liveChunkMetas(manifest *index.Manifest, segmentDirs []string) (map[string]chunkMeta, error) {
	if len(manifest.Segments) != len(segmentDirs) {
		return nil, fmt.Errorf("revision %s 的分段数 %d 与分段目录数 %d 不一致", manifest.Revision, len(manifest.Segments), len(segmentDirs))
	}
	perSegment := make([]map[string][]locatedMeta, len(segmentDirs))
	for i, dir := range segmentDirs {
		byFile, err := e.segmentMetas(dir, manifest.Segments[i].ChunksChecksum)
		if err != nil {
			return nil, err
		}
		perSegment[i] = byFile
	}
	e.pruneSegmentMetas(segmentDirs)
	return assembleLiveMetas(manifest, perSegment), nil
}

// pruneSegmentMetas 删除同一索引子树里已不属于当前 revision 的分段条目，缓存
// 只保留最近一次打开的 revision 的分段。条目只在组装存活集时使用，已打开的
// 句柄持有的是组装出的拷贝，删除条目不影响它们；回退到更早的 revision 时会
// 重新解码。
func (e *Engine) pruneSegmentMetas(segmentDirs []string) {
	if len(segmentDirs) == 0 {
		return
	}
	parent := filepath.Dir(segmentDirs[0])
	current := make(map[string]bool, len(segmentDirs))
	for _, dir := range segmentDirs {
		current[dir] = true
	}
	e.segmentMetaMu.Lock()
	for key, entry := range e.segmentMetaCache {
		if filepath.Dir(entry.dir) == parent && !current[entry.dir] {
			delete(e.segmentMetaCache, key)
		}
	}
	e.segmentMetaMu.Unlock()
}
