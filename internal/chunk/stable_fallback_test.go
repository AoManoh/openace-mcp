package chunk

import (
	"fmt"
	"strings"
	"testing"
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
