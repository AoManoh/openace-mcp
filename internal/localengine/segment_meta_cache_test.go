package localengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
)

// TestHandleOpenDecodesOnlyNewSegments：delta 发布的新 revision 与上一 revision
// 共享基段，打开新句柄时只解码新增分段的 chunks.jsonl。改动前每次打开句柄都
// 逐行解码全部分段，一个 32 万 chunk 的工作区每发布一个新 revision 要多花约
// 3 秒、临时分配约 1.3 GiB。
func TestHandleOpenDecodesOnlyNewSegments(t *testing.T) {
	var mu sync.Mutex
	var decoded []string
	decodeSegmentChunksHook = func(dir string) {
		mu.Lock()
		decoded = append(decoded, filepath.Base(dir))
		mu.Unlock()
	}
	t.Cleanup(func() { decodeSegmentChunksHook = nil })

	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	e.releaseHandle(first)

	writeFixture(t, root, "new.go", "package app\n\nfunc NewThing() {}\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	before := len(decoded)
	mu.Unlock()
	second, err := e.acquireHandle(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer e.releaseHandle(second)
	if got := len(second.manifest.Segments); got != 2 {
		t.Fatalf("前置：delta 后应有 2 个分段，实际 %d", got)
	}
	mu.Lock()
	opened := append([]string(nil), decoded[before:]...)
	mu.Unlock()
	if len(opened) != 1 || opened[0] != second.manifest.NewestSegment().ID {
		t.Fatalf("打开新句柄只应解码新增分段 %s，实际解码 %v", second.manifest.NewestSegment().ID, opened)
	}
}

// referenceLiveChunkMetas 是改动前 loadLiveChunkMetas 的算法：逐个分段完整解码，
// 同一路径以后面的分段为准，路径不在 manifest.Files 里的记录丢弃。只用作对照。
func referenceLiveChunkMetas(t *testing.T, manifest *index.Manifest, segmentDirs []string) map[string]chunkMeta {
	t.Helper()
	type located struct {
		id   string
		meta chunkMeta
	}
	byFile := make(map[string][]located)
	for segIdx, dir := range segmentDirs {
		data, err := os.ReadFile(filepath.Join(dir, index.ChunksFileName))
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(bytes.NewReader(data))
		var offset int64
		segByFile := make(map[string][]located)
		for {
			line, err := reader.ReadBytes('\n')
			trimmed := bytes.TrimRight(line, "\n")
			if len(trimmed) > 0 {
				var record chunkRecord
				if err := json.Unmarshal(trimmed, &record); err != nil {
					t.Fatal(err)
				}
				segByFile[record.RelPath] = append(segByFile[record.RelPath], located{id: record.ID, meta: chunkMeta{
					RelPath: record.RelPath, Language: record.Language, StartLine: record.StartLine, EndLine: record.EndLine,
					Symbol: record.Symbol, ContentHash: record.ContentHash, segIdx: segIdx, offset: offset, length: len(trimmed),
				}})
			}
			offset += int64(len(line))
			if err != nil {
				break
			}
		}
		for path, group := range segByFile {
			byFile[path] = group
		}
	}
	metas := make(map[string]chunkMeta)
	for path, group := range byFile {
		if _, live := manifest.Files[path]; !live {
			continue
		}
		for _, item := range group {
			metas[item.id] = item.meta
		}
	}
	return metas
}

// TestSegmentMetaCacheMatchesReference：经分段缓存组装的存活集与改动前的算法
// 逐条相同（ID、元数据、segIdx、偏移与长度）。随机生成 200 组分段序列：文件在
// 后面的分段里被改写、在 manifest 中被删除；每组按分段前缀依次组装，模拟
// delta 逐个发布，前面的分段走缓存命中。
func TestSegmentMetaCacheMatchesReference(t *testing.T) {
	e := &Engine{}
	rng := rand.New(rand.NewSource(20261103))
	paths := []string{"a.go", "b.go", "docs/c.md", "d/e.go", "f.txt", "g/h/i.py"}
	base := t.TempDir()
	for c := 0; c < 200; c++ {
		segRoot := filepath.Join(base, fmt.Sprintf("case-%d", c), "segments")
		segmentCount := 1 + rng.Intn(5)
		var segments []index.SegmentRef
		var dirs []string
		for s := 0; s < segmentCount; s++ {
			id := fmt.Sprintf("seg-%d", s)
			dir := filepath.Join(segRoot, id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			var records []chunkRecord
			for _, path := range paths {
				if rng.Intn(2) == 0 {
					continue
				}
				for k := 0; k < 1+rng.Intn(3); k++ {
					records = append(records, chunkRecord{
						ID: fmt.Sprintf("c%d-%s-%s-%d", c, id, path, k), RelPath: path, Language: "go",
						StartLine: k*10 + 1, EndLine: k*10 + 9, Symbol: fmt.Sprintf("S%d", k),
						Content: fmt.Sprintf("content %d %s %d", c, path, rng.Intn(1000)), ContentHash: fmt.Sprintf("h%d", rng.Intn(1e6)),
					})
				}
			}
			chunksPath := filepath.Join(dir, index.ChunksFileName)
			if err := writeChunkRecords(chunksPath, records); err != nil {
				t.Fatal(err)
			}
			sum, err := index.ChecksumFile(chunksPath)
			if err != nil {
				t.Fatal(err)
			}
			segments = append(segments, index.SegmentRef{ID: id, ChunksChecksum: sum})
			dirs = append(dirs, dir)
		}
		files := make(map[string]index.FileEntry)
		for _, path := range paths {
			if rng.Intn(4) != 0 {
				files[path] = index.FileEntry{ContentHash: "x"}
			}
		}
		for k := 1; k <= segmentCount; k++ {
			manifest := &index.Manifest{Files: files, Segments: segments[:k]}
			want := referenceLiveChunkMetas(t, manifest, dirs[:k])
			got, err := e.liveChunkMetas(manifest, dirs[:k])
			if err != nil {
				t.Fatalf("case %d prefix %d: %v", c, k, err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("case %d prefix %d: 缓存组装结果与原算法不同\nwant=%v\ngot=%v", c, k, want, got)
			}
			uncached, err := loadLiveChunkMetas(manifest, dirs[:k])
			if err != nil || !reflect.DeepEqual(want, uncached) {
				t.Fatalf("case %d prefix %d: 不经缓存的组装结果与原算法不同: %v", c, k, err)
			}
		}
	}
}
