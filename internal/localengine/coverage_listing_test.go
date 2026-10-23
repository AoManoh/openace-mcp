package localengine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
)

// syntheticCoverageManifest 构造一个 total 个文件的 manifest：前 uncovered 个
// 文件各缺 1 个向量，前 rejected 个文件各有 1 个被拒绝的 chunk（与缺向量的
// 文件错开），其余文件全覆盖。每个文件 4 个 chunk。
func syntheticCoverageManifest(total, uncovered, rejected int) *index.Manifest {
	files := make(map[string]index.FileEntry, total)
	vectors, rejectedCount := 0, 0
	for i := 0; i < total; i++ {
		entry := index.FileEntry{ContentHash: fmt.Sprintf("h%d", i), ChunkCount: 4, CoveredChunks: 4}
		switch {
		case i < uncovered:
			entry.CoveredChunks = 3
		case i < uncovered+rejected:
			entry.CoveredChunks, entry.RejectedChunks = 3, 1
		}
		vectors += entry.CoveredChunks
		rejectedCount += entry.RejectedChunks
		files[fmt.Sprintf("dir%03d/file%05d.go", i%100, i)] = entry
	}
	return &index.Manifest{
		Revision: "rev-synthetic", Files: files,
		Counts:      index.Counts{Files: total, Chunks: total * 4},
		VectorCount: vectors, RejectedCount: rejectedCount,
	}
}

// TestCoverageListingLimit：未覆盖文件恰好 50 个时全部列出、不报错；51 个时
// 返回 semantic-coverage-insufficient 缺口且不返回清单。含被拒绝 chunk 的
// 文件最多列 50 个，总数另行返回；被拒绝的 chunk 不让文件算作未覆盖。
func TestCoverageListingLimit(t *testing.T) {
	m := syntheticCoverageManifest(200, uncoveredFilesLimit, 70)
	uncovered, rejected, rejectedTotal, gap := coverageListing(m)
	if gap != nil || len(uncovered) != uncoveredFilesLimit {
		t.Fatalf("恰好 %d 个未覆盖文件应全部列出: listed=%d gap=%v", uncoveredFilesLimit, len(uncovered), gap)
	}
	if len(rejected) != uncoveredFilesLimit || rejectedTotal != 70 {
		t.Fatalf("被拒绝文件应列 %d 个、总数 70: listed=%d total=%d", uncoveredFilesLimit, len(rejected), rejectedTotal)
	}
	for _, path := range rejected {
		if containsString(uncovered, path) {
			t.Fatalf("只含被拒绝 chunk 的文件不算未覆盖: %s", path)
		}
	}
	note := coverageNote(uncovered, rejected, rejectedTotal)
	if !strings.Contains(note, "these 50 files yet") || !strings.Contains(note, "(+20 more)") {
		t.Fatalf("正文提示应写明数量与未列出的部分: %q", note)
	}

	m = syntheticCoverageManifest(200, uncoveredFilesLimit+1, 0)
	uncovered, _, _, gap = coverageListing(m)
	if gap == nil || gap.reason != coverageInsufficientReason || uncovered != nil {
		t.Fatalf("51 个未覆盖文件应返回缺口且不列清单: uncovered=%d gap=%v", len(uncovered), gap)
	}
	if gapSwitch(gap.reason) != gapSemanticUnavailable || !strings.Contains(gap.detail.Error(), "51 files") {
		t.Fatalf("缺口应归入语义路不可用并写明文件数: %v", gap.detail)
	}
}

// BenchmarkCoverageListing 测 1.5 万个文件（与本机最大的真实工作区同量级）
// 的 manifest 上列出覆盖缺口的耗时；只在覆盖不完整或有被拒绝内容时执行。
func BenchmarkCoverageListing(b *testing.B) {
	m := syntheticCoverageManifest(15000, 30, 30)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		coverageListing(m)
	}
}
