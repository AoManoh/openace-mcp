package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFallbackLineNumbersMatchContent(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 150; i++ {
		b.WriteString("line-content-")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	profile := DefaultProfile()
	content := b.String()
	chunks, capability := profile.Split(File{RelPath: "src/app.ts", Content: content})
	if capability != CapabilityFallback {
		t.Fatalf("ts 文件应走行窗口，got %s", capability)
	}
	lines := strings.Split(content, "\n")
	for _, chunk := range chunks {
		wantFirst := lines[chunk.StartLine-1]
		gotFirst := strings.SplitN(chunk.Content, "\n", 2)[0]
		if wantFirst != gotFirst {
			t.Fatalf("chunk [%d-%d] 首行不匹配: want %q got %q", chunk.StartLine, chunk.EndLine, wantFirst, gotFirst)
		}
		if chunk.Language != "typescript" {
			t.Fatalf("语言识别错误: %s", chunk.Language)
		}
	}
	if chunks[1].StartLine != chunks[0].StartLine+profile.WindowLines-profile.OverlapLines {
		t.Fatalf("窗口步进错误: %d -> %d", chunks[0].StartLine, chunks[1].StartLine)
	}
}

func TestFallbackDocWindowForMarkdown(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("markdown paragraph line\n")
	}
	profile := DefaultProfile()
	chunks, _ := profile.Split(File{RelPath: "README.md", Content: b.String()})
	if len(chunks) == 0 {
		t.Fatal("markdown 切分为空")
	}
	span := chunks[0].EndLine - chunks[0].StartLine + 1
	if span > profile.DocWindowLines {
		t.Fatalf("markdown 应使用文档窗口 %d，got span %d", profile.DocWindowLines, span)
	}
}

func TestFallbackOversizedSingleLine(t *testing.T) {
	minified := "var a=1;" + strings.Repeat("f(", 4000) + strings.Repeat(")", 4000) + ";"
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "dist/bundle.min.js", Content: minified})
	if capability != CapabilityFallback {
		t.Fatalf("minified 应走 fallback，got %s", capability)
	}
	if len(chunks) == 0 {
		t.Fatal("超长单行应产出有界 chunk 序列")
	}
	for _, chunk := range chunks {
		if len(chunk.Content) > profile.MaxChunkBytes {
			t.Fatalf("字节窗口超出预算: %d", len(chunk.Content))
		}
	}
}

func TestFallbackEmptyFile(t *testing.T) {
	profile := DefaultProfile()
	chunks, _ := profile.Split(File{RelPath: "empty.py", Content: "   \n\n"})
	if len(chunks) != 0 {
		t.Fatalf("空白文件应产出 0 chunk，got %d", len(chunks))
	}
}

// TestFallbackOversizedCJKLineKeepsValidUTF8 是 review S16：含 CJK 的
// minified 内容按字节窗口切分时不得切断多字节 rune——非法 UTF-8 会让
// 落盘 JSONL（U+FFFD 替换）与 ContentHash 脱钩。
func TestFallbackOversizedCJKLineKeepsValidUTF8(t *testing.T) {
	minified := "var 消息=1;" + strings.Repeat("处理数据(", 1500) + strings.Repeat(")", 1500) + ";"
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "dist/app.min.js", Content: minified})
	if capability != CapabilityFallback || len(chunks) < 2 {
		t.Fatalf("CJK minified 应走字节窗口且产出多个 chunk: cap=%s n=%d", capability, len(chunks))
	}
	var rebuilt strings.Builder
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk.Content) {
			t.Fatalf("第 %d 个 chunk 含非法 UTF-8（S16/GATE-ENCODING）", i)
		}
		if len(chunk.Content) > profile.MaxChunkBytes {
			t.Fatalf("字节窗口超出预算: %d", len(chunk.Content))
		}
		rebuilt.WriteString(chunk.Content)
	}
	if rebuilt.String() != minified {
		t.Fatal("字节窗口拼接应无损还原原文")
	}
}

// TestFallbackByteWindowEndLineExact 是 review S16 的 EndLine 修正断言：
// 以换行结尾的窗口不得把行号多算一行。
func TestFallbackByteWindowEndLineExact(t *testing.T) {
	// 第 1 行超长触发字节窗口；构造窗口恰在换行后断开的场景。
	long := strings.Repeat("x", 5000)
	content := long + "\nshort line 2\nshort line 3"
	profile := DefaultProfile()
	chunks, _ := profile.Split(File{RelPath: "one.min.js", Content: content})
	if len(chunks) == 0 {
		t.Fatal("切分为空")
	}
	last := chunks[len(chunks)-1]
	if last.EndLine != 3 {
		t.Fatalf("最后一个 chunk 应止于第 3 行: %d-%d %q", last.StartLine, last.EndLine, last.Content)
	}
	for _, chunk := range chunks {
		if chunk.EndLine < chunk.StartLine {
			t.Fatalf("行区间非法: %d-%d", chunk.StartLine, chunk.EndLine)
		}
		if strings.HasSuffix(chunk.Content, "\n") {
			body := strings.TrimSuffix(chunk.Content, "\n")
			if want := chunk.StartLine + strings.Count(body, "\n"); chunk.EndLine != want {
				t.Fatalf("尾换行窗口 EndLine 多算: got %d want %d", chunk.EndLine, want)
			}
		}
	}
}

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		"a/b.go": "go", "x.tsx": "typescript", "y.py": "python",
		"doc.md": "markdown", "conf.yaml": "yaml", "unknown.xyz": "text",
	}
	for path, want := range cases {
		if got := DetectLanguage(path); got != want {
			t.Fatalf("DetectLanguage(%s) = %s, want %s", path, got, want)
		}
	}
}
