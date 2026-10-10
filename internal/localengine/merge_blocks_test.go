package localengine

import (
	"fmt"
	"strings"
	"testing"
)

// TestMergeBlocksRequiresIdenticalOverlap：按行位置对应的重叠正文完全相同才
// 合并；同一行号范围内容不同时保留两块；合并块继承最早名次。
func TestMergeBlocksRequiresIdenticalOverlap(t *testing.T) {
	block := func(start, end int, content string, rank int) renderBlock {
		return renderBlock{record: chunkRecord{RelPath: "f.txt", StartLine: start, EndLine: end, Content: content}, score: 1, rank: rank}
	}
	cases := []struct {
		name   string
		blocks []renderBlock
		want   []string
	}{
		{"重叠行相同", []renderBlock{block(1, 3, "l1\nl2\nl3", 2), block(2, 4, "l2\nl3\nl4", 0)}, []string{"1-4:l1\nl2\nl3\nl4"}},
		{"被包含且相同", []renderBlock{block(1, 3, "x\ny\nz", 1), block(2, 2, "y", 0)}, []string{"1-3:x\ny\nz"}},
		// 行号相邻、没有共同行的两块不合并：相邻行号不能证明字节连续（见 overlapTail）。
		{"相邻不合并", []renderBlock{block(1, 2, "a\nb", 0), block(3, 3, "c", 1)}, []string{"1-2:a\nb", "3-3:c"}},
		{"同一行内容不同", []renderBlock{block(1, 1, "part-one", 0), block(1, 1, "part-two", 1)}, []string{"1-1:part-one", "1-1:part-two"}},
		{"相交行内容不同", []renderBlock{block(1, 2, "tail-of-a\nhead-of-b", 0), block(2, 3, "rest-of-b\nnext", 1)}, []string{"1-2:tail-of-a\nhead-of-b", "2-3:rest-of-b\nnext"}},
		{"被包含但不同", []renderBlock{block(1, 3, "x\ny\nz", 0), block(2, 2, "Y", 1)}, []string{"1-3:x\ny\nz", "2-2:Y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeBlocks(tc.blocks)
			var got []string
			for _, b := range merged {
				got = append(got, fmt.Sprintf("%d-%d:%s", b.record.StartLine, b.record.EndLine, b.record.Content))
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("合并结果不符:\nwant=%q\n got=%q", tc.want, got)
			}
			if tc.name == "重叠行相同" && merged[0].rank != 0 {
				t.Fatalf("合并块应继承最早名次 0，实际 %d", merged[0].rank)
			}
		})
	}
}
