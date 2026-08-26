//go:build linux

package vector

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMmapColdQueryProbe 是环境门控的 mmap 冷查询探针(默认 skip,零
// provider):量化"向量页被内核回收后,全量扫描查询的磁盘重读代价"——
// A&Q 待补数据。用 madvise(DONTNEED) 对映射区间逼冷(file-backed 干净
// 页直接丢弃,下次访问重新读盘,等效于内存压力回收;fadvise 对
// mapcount>0 的页无效,已实测排除),对照冷/热两态的查询延迟分布。
//
//	OPENACE_VECTOR_COLD_PROBE_ROWS=400000 go test ./internal/vector/ \
//	  -run TestMmapColdQueryProbe -v -timeout 30m
func TestMmapColdQueryProbe(t *testing.T) {
	rowsEnv := os.Getenv("OPENACE_VECTOR_COLD_PROBE_ROWS")
	if rowsEnv == "" {
		t.Skip("OPENACE_VECTOR_COLD_PROBE_ROWS 未设置")
	}
	const dim = 1024
	for _, part := range strings.Split(rowsEnv, ",") {
		rows, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			t.Fatal(err)
		}
		coldProbeOnce(t, rows, dim)
	}
}

func coldProbeOnce(t *testing.T, rows, dim int) {
	t.Helper()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(20260826))
	entries := make([]Entry, rows)
	vectors := make([][]float32, rows)
	backing := make([]float32, rows*dim)
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
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	entries, vectors, backing = nil, nil, nil
	_ = backing
	ix, err := load(dir, dim, dataSum, idxSum, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if !ix.Mapped() {
		t.Fatal("冷查询探针要求 mmap 驻留形态")
	}
	query := make([]float32, dim)
	for j := range query {
		query[j] = rng.Float32() - 0.5
	}
	if err := Normalize(query); err != nil {
		t.Fatal(err)
	}

	evict := func() {
		// 只读映射的 file-backed 页全为干净页,DONTNEED 直接丢弃,
		// 下次触碰按需重读盘——与内核在内存压力下回收再重读同形态。
		if err := unix.Madvise(ix.mapped, unix.MADV_DONTNEED); err != nil {
			t.Fatalf("madvise: %v", err)
		}
	}
	runOne := func() time.Duration {
		start := time.Now()
		if _, err := ix.Search(context.Background(), query, 60); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	stats := func(samples []time.Duration) (p50, max time.Duration) {
		sorted := append([]time.Duration(nil), samples...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return sorted[len(sorted)/2], sorted[len(sorted)-1]
	}

	// 热态基线:预热一遍后测 10 次。
	runOne()
	warm := make([]time.Duration, 0, 10)
	for i := 0; i < 10; i++ {
		warm = append(warm, runOne())
	}
	// 冷态:每次查询前 DONTNEED 全量驱逐(最坏情形=每查询整文件重读)。
	cold := make([]time.Duration, 0, 10)
	for i := 0; i < 10; i++ {
		evict()
		cold = append(cold, runOne())
	}
	warmP50, warmMax := stats(warm)
	coldP50, coldMax := stats(cold)
	// 边界诚实:madvise 只保证本进程映射页丢弃;宿主/虚拟化层(如 WSL2
	// VHDX 的 Windows 缓存)可能仍热,读数是"进程页冷"而非"存储介质冷"。
	// 真实介质冷读上界按顺序读带宽推算(如 1.5GiB/3GB·s⁻¹≈0.5s)。
	t.Logf("rows=%d dim=%d dat=%.1fMiB warm_p50=%s warm_max=%s cold_p50=%s cold_max=%s cold/warm=%.1fx (n=10 each, topK=60, madvise-DONTNEED evict; host-cache may stay warm)",
		rows, dim, float64(int64(rows)*int64(dim)*4)/(1<<20), warmP50, warmMax, coldP50, coldMax,
		float64(coldP50)/float64(warmP50))
}
