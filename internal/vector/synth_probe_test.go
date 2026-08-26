package vector

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSyntheticVectorMemoryProbe 是环境门控的合成内存/延迟探针(默认
// skip,零 provider):为"常驻向量默认不限"(2026-08-26 裁决)的 A&Q 文档
// 提供实测数据——给定规模下的磁盘尺寸、Load 墙钟/堆增量/进程 RSS、
// 单查询延迟。用法:
//
//	OPENACE_VECTOR_SYNTH_PROBE_ROWS=100000,400000 go test ./internal/vector/ \
//	  -run TestSyntheticVectorMemoryProbe -v -timeout 30m
func TestSyntheticVectorMemoryProbe(t *testing.T) {
	rowsEnv := os.Getenv("OPENACE_VECTOR_SYNTH_PROBE_ROWS")
	if rowsEnv == "" {
		t.Skip("OPENACE_VECTOR_SYNTH_PROBE_ROWS 未设置")
	}
	const dim = 1024
	for _, part := range strings.Split(rowsEnv, ",") {
		rows, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			t.Fatal(err)
		}
		probeOnce(t, rows, dim)
	}
}

func probeOnce(t *testing.T, rows, dim int) {
	t.Helper()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(20260826))
	entries := make([]Entry, rows)
	vectors := make([][]float32, rows)
	backing := make([]float32, rows*dim) // 单块 backing,贴近引擎真实布局
	for i := 0; i < rows; i++ {
		row := backing[i*dim : (i+1)*dim]
		for j := range row {
			row[j] = rng.Float32() - 0.5
		}
		if err := Normalize(row); err != nil {
			t.Fatal(err)
		}
		entries[i] = Entry{ID: fmt.Sprintf("%032d", i), ContentHash: fmt.Sprintf("%064d", i)}
		vectors[i] = row
	}
	writeStart := time.Now()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	writeWall := time.Since(writeStart)
	stat := func(name string) int64 {
		info, err := os.Stat(dir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	datSize, idxSize := stat(DataFileName), stat(IndexFileName)
	// 释放生成期内存,单独度量 Load。
	entries, vectors, backing = nil, nil, nil
	_ = backing
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	rssBefore := vmRSSMiB(t)
	loadStart := time.Now()
	ix, err := Load(dir, dim, dataSum, idxSum, 0)
	if err != nil {
		t.Fatal(err)
	}
	loadWall := time.Since(loadStart)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	rssAfter := vmRSSMiB(t)

	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32() - 0.5
	}
	if err := Normalize(query); err != nil {
		t.Fatal(err)
	}
	const queries = 20
	latencies := make([]time.Duration, 0, queries)
	for q := 0; q < queries; q++ {
		start := time.Now()
		if _, err := ix.Search(context.Background(), query, 60); err != nil {
			t.Fatal(err)
		}
		latencies = append(latencies, time.Since(start))
	}
	p50, max := latencies[queries/2], latencies[0]
	for _, d := range latencies {
		if d > max {
			max = d
		}
	}
	t.Logf("rows=%d dim=%d dat=%.1fMiB idx=%.1fMiB write=%.1fs load=%.2fs heap_live_delta=%.2fGiB rss_delta=%.2fGiB rss_after=%.2fGiB search_p50=%s search_max=%s (n=%d topK=60)",
		rows, dim, float64(datSize)/(1<<20), float64(idxSize)/(1<<20), writeWall.Seconds(), loadWall.Seconds(),
		float64(after.HeapAlloc-before.HeapAlloc)/(1<<30), (rssAfter-rssBefore)/1024, rssAfter/1024, p50, max, len(latencies))
	runtime.KeepAlive(ix)
}

func vmRSSMiB(t *testing.T) float64 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1 // 非 Linux 平台探针仍可运行,RSS 记 -1
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseFloat(fields[1], 64)
				return kb / 1024
			}
		}
	}
	return -1
}
