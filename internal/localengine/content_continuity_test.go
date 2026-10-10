package localengine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 正文连续性：索引只记录片段的行范围，不记录原文件字节区间。两块只有在
// 共同的行按行位置逐行相同时才合并；行号相邻但没有共同的行时分别展示，
// 因为相邻行号不能证明两段字节连续。

// renderedBodies 按块头 "## " 切开渲染正文，返回每块围栏内的正文。
func renderedBodies(t *testing.T, text string) []string {
	t.Helper()
	var bodies []string
	for _, block := range strings.Split(text, "## ")[1:] {
		parts := strings.SplitN(block, "\n```", 3)
		if len(parts) < 3 {
			continue
		}
		body := parts[1]
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			body = body[i+1:]
		}
		bodies = append(bodies, strings.TrimSuffix(body, "\n"))
	}
	return bodies
}

// assertBodiesContiguous 检查每块正文都是原文里的一段连续字节。
func assertBodiesContiguous(t *testing.T, text, original string, want int) {
	t.Helper()
	bodies := renderedBodies(t, text)
	if len(bodies) != want {
		t.Fatalf("应有 %d 块正文，实际 %d:\n%s", want, len(bodies), text)
	}
	for _, body := range bodies {
		if !strings.Contains(original, body) {
			t.Fatalf("正文不是原文的一段连续字节（%d 字节，开头 %q）", len(body), body[:min(40, len(body))])
		}
	}
}

func hitRanges(hits []engine.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, fmt.Sprintf("%s:%d-%d", h.Path, h.StartLine, h.EndLine))
	}
	return out
}

// TestMergeBlocksRequiresSharedLines：没有共同的行时不合并，包括行号严格相邻、
// 内容相同的情况；有共同的行时按行位置逐行比较，空行与原文内部重复按位置
// 处理，尾随空白不同视为不同。
func TestMergeBlocksRequiresSharedLines(t *testing.T) {
	block := func(start, end int, content string, rank int) renderBlock {
		return renderBlock{record: chunkRecord{RelPath: "f.txt", StartLine: start, EndLine: end, Content: content}, score: 1, rank: rank}
	}
	cases := []struct {
		name   string
		blocks []renderBlock
		want   []string
	}{
		{"两个半行行号相邻", []renderBlock{block(1, 1, "needlex prefix xxxx", 0), block(2, 2, "zzzz needlex suffix yyyy", 1)}, []string{"1-1:needlex prefix xxxx", "2-2:zzzz needlex suffix yyyy"}},
		{"半行加整行", []renderBlock{block(1, 1, "first half of a long line", 0), block(2, 2, "complete second line", 1)}, []string{"1-1:first half of a long line", "2-2:complete second line"}},
		{"相邻完整声明", []renderBlock{block(1, 3, "func A() {\n\treturn\n}", 0), block(4, 6, "func B() {\n\treturn\n}", 1)}, []string{"1-3:func A() {\n\treturn\n}", "4-6:func B() {\n\treturn\n}"}},
		{"相邻且内容相同", []renderBlock{block(1, 1, "same", 0), block(2, 2, "same", 1)}, []string{"1-1:same", "2-2:same"}},
		{"共同行相同", []renderBlock{block(1, 3, "l1\nl2\nl3", 0), block(3, 5, "l3\nl4\nl5", 1)}, []string{"1-5:l1\nl2\nl3\nl4\nl5"}},
		{"共同行是空行", []renderBlock{block(1, 3, "a\n\n", 0), block(2, 4, "\n\nb", 1)}, []string{"1-4:a\n\n\nb"}},
		{"尾随空白不同", []renderBlock{block(1, 2, "x\ny ", 0), block(2, 3, "y\nz", 1)}, []string{"1-2:x\ny ", "2-3:y\nz"}},
		{"原文内部重复", []renderBlock{block(1, 3, "same\nsame\ntail", 0), block(2, 4, "same\ntail\nsame", 1)}, []string{"1-4:same\nsame\ntail\nsame"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, b := range mergeBlocks(tc.blocks) {
				got = append(got, fmt.Sprintf("%d-%d:%s", b.record.StartLine, b.record.EndLine, b.record.Content))
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("合并结果不符:\nwant=%q\n got=%q", tc.want, got)
			}
		})
	}
}

// partialFixture 生成两行文本：第一行 6000 字节，以 needlex prefix
// 开头；第二行先是 300 个 z，再是 needlex suffix 和 2500 个 y。查询 needlex 只
// 命中第 1 与第 4 个 2048 字节窗口，它们行号分别是 1 与 2，字节不连续。
func partialFixture() string {
	line1 := "needlex prefix " + strings.Repeat("x", 6000-len("needlex prefix "))
	line2 := strings.Repeat("z", 300) + " needlex suffix " + strings.Repeat("y", 2500)
	return line1 + "\n" + line2
}

// TestPartialByteFragmentsStaySeparate：两个不连续的半行片段分别展示，每块
// 正文都是原文的一段连续字节；full 与 paths 的块数和顺序一致。
func TestPartialByteFragmentsStaySeparate(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	original := partialFixture()
	writeFixture(t, root, "partial.txt", original)
	req := searchRequest(root, "needlex")
	full, err := e.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitRanges(full.Hits); strings.Join(got, ",") != "partial.txt:1-1,partial.txt:2-2" || full.Display == nil || full.Display.CandidateBlocks != 2 {
		t.Fatalf("两个片段应分别展示为 1-1 与 2-2: hits=%v display=%+v", got, full.Display)
	}
	assertBodiesContiguous(t, full.Text, original, 2)
	req.Detail = "paths"
	paths, err := e.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitRanges(paths.Hits); strings.Join(got, ",") != "partial.txt:1-1,partial.txt:2-2" || strings.Contains(paths.Text, "xxxx") {
		t.Fatalf("paths 应列出同样两块且不带正文: hits=%v", got)
	}
}

// TestHalfLineThenWholeLineStaySeparate：第一行 4095 字节加换行，第二个窗口恰好
// 在换行处结束，第三个窗口从第二行行首开始。只命中第一与第三个窗口时，
// 第一行的后半段没有被召回，两块行号相邻但不连续，分别展示。
func TestHalfLineThenWholeLineStaySeparate(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	line1 := "halfneedle " + strings.Repeat("h", 4095-len("halfneedle "))
	line2 := "halfneedle on a complete second line"
	original := line1 + "\n" + line2 + "\n"
	writeFixture(t, root, "half.txt", original)
	res, err := e.Search(context.Background(), searchRequest(root, "halfneedle"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hitRanges(res.Hits); strings.Join(got, ",") != "half.txt:1-1,half.txt:2-2" {
		t.Fatalf("半行与整行应分别展示: hits=%v", got)
	}
	assertBodiesContiguous(t, res.Text, original, 2)
}

// TestWhitespaceBoundaryFragmentsStaySeparate：字节窗口边界落在一长串空格中，
// 两个命中片段行号相邻，分别展示，正文保留原样的空白。
func TestWhitespaceBoundaryFragmentsStaySeparate(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	line1 := "wsneedle" + strings.Repeat(" ", 5992)
	line2 := strings.Repeat(" ", 300) + "wsneedle tail " + strings.Repeat("w", 2500)
	original := line1 + "\n" + line2
	writeFixture(t, root, "spaces.txt", original)
	res, err := e.Search(context.Background(), searchRequest(root, "wsneedle"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("空白边界的两个片段应分别展示: hits=%v", hitRanges(res.Hits))
	}
	assertBodiesContiguous(t, res.Text, original, 2)
}

// TestAdjacentDeclarationsStaySeparate：两个相邻的完整 Go 声明各自成块，行号
// 相邻但没有共同的行，分别展示，避免在缺少字节区间时推断连续性。
func TestAdjacentDeclarationsStaySeparate(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	original := "package fixture\n\nfunc DeclA() { declneedle() }\nfunc DeclB() { declneedle() }\n"
	writeFixture(t, root, "decl.go", original)
	res, err := e.Search(context.Background(), searchRequest(root, "declneedle"))
	if err != nil {
		t.Fatal(err)
	}
	got := hitRanges(res.Hits)
	if len(got) != 2 || strings.Count(res.Text, "## decl.go:") != 2 {
		t.Fatalf("相邻声明应分别展示: hits=%v\n%s", got, res.Text)
	}
	assertBodiesContiguous(t, res.Text, original, 2)
}

// TestOverlappingWindowsStillMerge：CRLF 文本按 LF 切分，40 行窗口重叠 10 行；
// 重叠行相同，三个窗口合并成一块，每行只出现一次，正文不含回车。原文里
// 完全相同的行按位置各自保留，不被当作重复删除。
func TestOverlappingWindowsStillMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		line func(n int) string
	}{
		{"CRLF", func(n int) string { return fmt.Sprintf("line %03d overlapneedle", n) }},
		{"原文内部重复", func(int) string { return "overlapneedle same text" }},
		{"重叠区为空行", func(n int) string {
			if n >= 31 && n <= 40 {
				return ""
			}
			return fmt.Sprintf("line %03d overlapneedle", n)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t)
			root := t.TempDir()
			lines := make([]string, 0, 100)
			for n := 1; n <= 100; n++ {
				lines = append(lines, tc.line(n))
			}
			sep := "\n"
			if tc.name == "CRLF" {
				sep = "\r\n"
			}
			// 末尾不加换行：带末尾换行时切分把其后的空串计为第 101 行。
			writeFixture(t, root, "doc.txt", strings.Join(lines, sep))
			res, err := e.Search(context.Background(), searchRequest(root, "overlapneedle"))
			if err != nil {
				t.Fatal(err)
			}
			if got := hitRanges(res.Hits); strings.Join(got, ",") != "doc.txt:1-100" {
				t.Fatalf("重叠窗口应合并为 1-100 一块: %v", got)
			}
			bodies := renderedBodies(t, res.Text)
			if len(bodies) != 1 || bodies[0] != strings.Join(lines, "\n") || strings.Contains(res.Text, "\r") {
				t.Fatalf("合并块应逐行等于原文（LF）且不含回车: blocks=%d", len(bodies))
			}
		})
	}
}

// TestRerankedPartialFragmentsKeepOrder：精排把第二个片段排到前面，两块仍分别
// 展示，顺序跟随精排结果。
func TestRerankedPartialFragmentsKeepOrder(t *testing.T) {
	ts := newRerankServer(t, func(doc string) float64 {
		if strings.Contains(doc, "needlex suffix") {
			return 0.9
		}
		return 0.1
	}, nil)
	e := newTestEngineWith(t, Options{Rerank: rerankOptions(ts.URL)})
	root := t.TempDir()
	original := partialFixture()
	writeFixture(t, root, "partial.txt", original)
	res, err := e.Search(context.Background(), searchRequest(root, "needlex"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hitRanges(res.Hits); !strings.Contains(res.RetrievalMode, "rerank") || strings.Join(got, ",") != "partial.txt:2-2,partial.txt:1-1" {
		t.Fatalf("精排后应先列 2-2 再列 1-1: mode=%q hits=%v", res.RetrievalMode, got)
	}
	assertBodiesContiguous(t, res.Text, original, 2)
}
