package chunk

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// changedChunks 返回 after 里内容在 before 中不存在的 chunk 数，即升版本后
// 或改动后需要重新送审嵌入的 chunk 数。
func changedChunks(before, after []Chunk) int {
	seen := make(map[string]bool, len(before))
	for _, chunk := range before {
		seen[chunk.ContentHash] = true
	}
	changed := 0
	for _, chunk := range after {
		if !seen[chunk.ContentHash] {
			changed++
		}
	}
	return changed
}

func markdownDoc(paragraphs int, lineBytes int, insertAt int, inserted string) string {
	var b strings.Builder
	b.WriteString("# 项目状态\n\n")
	for i := 0; i < paragraphs; i++ {
		if i == insertAt {
			b.WriteString(inserted + "\n\n")
		}
		line := fmt.Sprintf("- 2026-10-%02d 条目 %d：", i%28+1, i)
		for len(line) < lineBytes {
			line += fmt.Sprintf("第 %d 段的说明文字，记录一次改动与验证结果；", i)
		}
		b.WriteString(line + "\n\n")
	}
	return b.String()
}

// TestFallbackEditOnlyChangesNearbyChunks：大文档中间插入一段，只有插入点
// 附近的 chunk 内容改变，其余 chunk 的内容键不变、不需要重新送审。覆盖三种
// 形态：短段落 markdown、长段落 markdown（字节上限强制切开）、没有空行的
// 配置文本。
func TestFallbackEditOnlyChangesNearbyChunks(t *testing.T) {
	profile := DefaultProfile()
	cases := []struct {
		name   string
		path   string
		before string
		after  string
	}{
		{"短段落 markdown", "docs/state.md", markdownDoc(300, 60, -1, ""), markdownDoc(300, 60, 150, "- 新插入的一条记录")},
		{"长段落 markdown", "docs/state.md", markdownDoc(200, 900, -1, ""), markdownDoc(200, 900, 100, "- 新插入的一条较长记录，说明本次改动")},
		{"无空行配置文本", "config/app.yaml", yamlDoc(600, -1), yamlDoc(600, 300)},
	}
	for _, tc := range cases {
		before, _ := profile.Split(File{RelPath: tc.path, Content: tc.before})
		after, _ := profile.Split(File{RelPath: tc.path, Content: tc.after})
		changed := changedChunks(before, after)
		if len(before) < 20 {
			t.Fatalf("%s: 样本应切出足够多的 chunk，实际 %d", tc.name, len(before))
		}
		if changed > 4 {
			t.Fatalf("%s: 插入一段后 %d/%d 个 chunk 内容改变，应只改变插入点附近的 chunk", tc.name, changed, len(after))
		}
	}
}

// longLineDoc 模拟频繁编辑的中文项目状态文档：大部分是几百字节的普通段落，
// 每 40 段里有一段写成不换行的长段落，长度 6–14 KB，超过 maxLineBytes
// （4096 字节）。
func longLineDoc(paragraphs int, insertAt int, inserted string) string {
	var b strings.Builder
	b.WriteString("# 项目状态\n\n")
	for i := 0; i < paragraphs; i++ {
		if i == insertAt {
			b.WriteString(inserted + "\n\n")
		}
		target := 300
		if i%40 == 7 {
			target = 6000 + (i%3)*4000
		}
		line := fmt.Sprintf("- 2026-10-%02d 条目 %d：", i%28+1, i)
		for len(line) < target {
			line += fmt.Sprintf("第 %d 段的说明文字，记录一次改动与验证结果；", i)
		}
		b.WriteString(line + "\n\n")
	}
	return b.String()
}

// TestFallbackLongLineDocEditOnlyChangesNearbyChunks：文档里有超过 maxLineBytes
// 的行时，插入一段只改变插入点附近的 chunk，长行内部改字只改变该行的 chunk。
// v8 与 v9 首版遇到这样的行就把整个文件从头按 2,048 字节窗口切分，插入点之后
// 的窗口全部错位；2026-10-26 实测一份 205 KB、3 行超过 4,096 字节的项目状态
// 文档开头插入一段，101 个 chunk 全部要重新嵌入。
func TestFallbackLongLineDocEditOnlyChangesNearbyChunks(t *testing.T) {
	profile := DefaultProfile()
	const path = "docs/decisions/project-state.md"
	before := longLineDoc(200, -1, "")
	if !hasOversizedLine(before) {
		t.Fatal("样本应含超过 maxLineBytes 的行")
	}
	beforeChunks, _ := profile.Split(File{RelPath: path, Content: before})
	if len(beforeChunks) < 20 {
		t.Fatalf("样本应切出足够多的 chunk，实际 %d", len(beforeChunks))
	}
	for _, tc := range []struct {
		name  string
		after string
	}{
		{"开头插入一段", longLineDoc(200, 1, "- 新插入的一条记录")},
		{"中间插入一段", longLineDoc(200, 100, "- 新插入的一条较长记录，说明本次改动")},
	} {
		afterChunks, _ := profile.Split(File{RelPath: path, Content: tc.after})
		if changed := changedChunks(beforeChunks, afterChunks); changed > 4 {
			t.Fatalf("%s: %d/%d 个 chunk 内容改变，应只改变插入点附近的 chunk", tc.name, changed, len(afterChunks))
		}
	}

	lines := strings.Split(before, "\n")
	longest := 0
	for i, line := range lines {
		if len(line) > len(lines[longest]) {
			longest = i
		}
	}
	mid := len(lines[longest]) / 2
	for !utf8.RuneStart(lines[longest][mid]) {
		mid--
	}
	edited := append([]string{}, lines...)
	edited[longest] = lines[longest][:mid] + "（补充几个字）" + lines[longest][mid:]
	afterChunks, _ := profile.Split(File{RelPath: path, Content: strings.Join(edited, "\n")})
	seen := make(map[string]bool, len(beforeChunks))
	for _, chunk := range beforeChunks {
		seen[chunk.ContentHash] = true
	}
	for _, chunk := range afterChunks {
		if seen[chunk.ContentHash] {
			continue
		}
		if chunk.StartLine != longest+1 || chunk.EndLine != longest+1 {
			t.Fatalf("长行内部改字只应改变第 %d 行的 chunk，实际改变 [%d-%d]", longest+1, chunk.StartLine, chunk.EndLine)
		}
	}
}

// TestFallbackLongLineSplitWithinLine：超过 MaxChunkBytes 的行单独切成字节窗口，
// 窗口不跨行、不超过字节上限、不切断多字节字符，同一行的窗口按顺序拼接即为
// 该行原文；其余行照常按内容边界切分，全文每一行都被某个 chunk 覆盖。
func TestFallbackLongLineSplitWithinLine(t *testing.T) {
	profile := DefaultProfile()
	content := longLineDoc(120, -1, "")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	chunks, capability := profile.Split(File{RelPath: "docs/state.md", Content: content})
	if capability != CapabilityFallback {
		t.Fatalf("markdown 应走行窗口切分: %s", capability)
	}
	covered := make([]bool, len(lines))
	pieces := make(map[int]string)
	for _, chunk := range chunks {
		if len(chunk.Content) > profile.MaxChunkBytes {
			t.Fatalf("chunk 超过字节上限: %d", len(chunk.Content))
		}
		if !utf8.ValidString(chunk.Content) {
			t.Fatalf("[%d-%d] 含非法 UTF-8", chunk.StartLine, chunk.EndLine)
		}
		for line := chunk.StartLine; line <= chunk.EndLine; line++ {
			covered[line-1] = true
		}
		if len(lines[chunk.StartLine-1]) > profile.MaxChunkBytes {
			if chunk.EndLine != chunk.StartLine {
				t.Fatalf("超长行的窗口不应跨行: [%d-%d]", chunk.StartLine, chunk.EndLine)
			}
			pieces[chunk.StartLine] += chunk.Content
		}
	}
	for i, line := range lines {
		if strings.TrimSpace(line) != "" && !covered[i] {
			t.Fatalf("第 %d 行没有被任何 chunk 覆盖", i+1)
		}
		if len(line) > profile.MaxChunkBytes && pieces[i+1] != line {
			t.Fatalf("第 %d 行的字节窗口拼接后与原文不同", i+1)
		}
	}
}

func yamlDoc(lines int, insertAt int) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		if i == insertAt {
			b.WriteString("  inserted_key: inserted-value\n")
		}
		fmt.Fprintf(&b, "  key_%04d: value-%d-%d\n", i, i*7%13, i%5)
	}
	return b.String()
}

// TestFallbackSmallFileUnchangedFromV8：不超过一个窗口与 MaxChunkBytes 的文件
// 仍是一个覆盖全文的 chunk，内容与行号同 v8，升版本后向量按内容键复用。
func TestFallbackSmallFileUnchangedFromV8(t *testing.T) {
	profile := DefaultProfile()
	content := "# 标题\n\n第一段。\n\n第二段。\n"
	chunks, capability := profile.Split(File{RelPath: "README.md", Content: content})
	if capability != CapabilityFallback || len(chunks) != 1 {
		t.Fatalf("小文件应为一个行窗口 chunk: %s %d", capability, len(chunks))
	}
	if chunks[0].Content != content || chunks[0].StartLine != 1 || chunks[0].EndLine != len(strings.Split(content, "\n")) {
		t.Fatalf("小文件 chunk 应与 v8 相同: [%d-%d] %q", chunks[0].StartLine, chunks[0].EndLine, chunks[0].Content)
	}
}

// TestFallbackStableChunksBounded：内容决定的切分仍满足窗口与字节上限，且同一
// 输入两次切分结果相同。
func TestFallbackStableChunksBounded(t *testing.T) {
	profile := DefaultProfile()
	content := markdownDoc(200, 900, -1, "")
	first, _ := profile.Split(File{RelPath: "docs/state.md", Content: content})
	second, _ := profile.Split(File{RelPath: "docs/state.md", Content: content})
	if len(first) != len(second) {
		t.Fatalf("同一输入切分结果不同: %d vs %d", len(first), len(second))
	}
	for i, chunk := range first {
		if chunk.ID != second[i].ID {
			t.Fatalf("第 %d 个 chunk 身份不同", i)
		}
		if len(chunk.Content) > profile.MaxChunkBytes {
			t.Fatalf("chunk 超过字节上限: %d", len(chunk.Content))
		}
		if chunk.EndLine-chunk.StartLine+1 > profile.DocWindowLines {
			t.Fatalf("chunk 超过文档窗口: [%d-%d]", chunk.StartLine, chunk.EndLine)
		}
	}
}
