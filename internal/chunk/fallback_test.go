package chunk

import (
	"strings"
	"testing"
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
