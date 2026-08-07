package vector

import (
	"os"
	"runtime"
	"testing"
)

// TestLargeVectorLoadMemoryProbe 是环境门控的真实大段诊断(默认 skip):
// OPENACE_VECTOR_PROBE_DIR 指向公开 benchmark segment 时,记录流式 Load
// 的实际 TotalAlloc/RSS 量级。它不进入日常回归,避免 CI 依赖大资产。
func TestLargeVectorLoadMemoryProbe(t *testing.T) {
	dir := os.Getenv("OPENACE_VECTOR_PROBE_DIR")
	if dir == "" {
		t.Skip("OPENACE_VECTOR_PROBE_DIR 未设置")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ix, err := Load(dir, 1024, "", "", DefaultMaxResidentVectors)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	t.Logf("load count=%d total_alloc_delta=%.2fGiB heap_alloc_delta=%.2fGiB heap_sys=%.2fGiB",
		ix.Count(), float64(after.TotalAlloc-before.TotalAlloc)/(1<<30),
		float64(after.HeapAlloc-before.HeapAlloc)/(1<<30), float64(after.HeapSys)/(1<<30))
	if os.Getenv("OPENACE_VECTOR_PROBE_WRITE") != "1" {
		return
	}
	rows := make([][]float32, ix.Count())
	for i := range rows {
		rows[i] = ix.Row(i)
	}
	if _, _, err := Write(t.TempDir(), ix.Dimension(), ix.Entries(), rows); err != nil {
		t.Fatal(err)
	}
	var written runtime.MemStats
	runtime.ReadMemStats(&written)
	t.Logf("load+write total_alloc_delta=%.2fGiB heap_alloc_delta=%.2fGiB heap_sys=%.2fGiB",
		float64(written.TotalAlloc-before.TotalAlloc)/(1<<30),
		float64(written.HeapAlloc-before.HeapAlloc)/(1<<30), float64(written.HeapSys)/(1<<30))
}
