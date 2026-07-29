package chunk

import (
	"os"
	"strings"
	"testing"
)

func TestChunkIDDeterministic(t *testing.T) {
	profile := DefaultProfile()
	file := File{RelPath: "pkg/demo.go", Content: "package demo\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n"}
	first, capability := profile.Split(file)
	second, _ := profile.Split(file)
	if capability != CapabilityAST {
		t.Fatalf("go 文件应走 AST，got %s", capability)
	}
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("重复切分数量不一致: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("chunk ID 不可复现: %s vs %s", first[i].ID, second[i].ID)
		}
	}
	changed := Profile{ID: "default", Version: "2", MaxChunkBytes: profile.MaxChunkBytes, WindowLines: profile.WindowLines, OverlapLines: profile.OverlapLines, DocWindowLines: profile.DocWindowLines}
	third, _ := changed.Split(file)
	if third[0].ID == first[0].ID {
		t.Fatal("profile 版本变化后 chunk ID 应变化")
	}
}

func TestContentHashExcludesLineRange(t *testing.T) {
	profile := DefaultProfile()
	// 同一内容出现在不同文件位置：ID 不同（含行号），ContentHash 相同（供缓存复用）。
	a, _ := profile.Split(File{RelPath: "a.py", Content: "def f():\n    return 1\n"})
	b, _ := profile.Split(File{RelPath: "a.py", Content: "# header\ndef f():\n    return 1\n"})
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("切分结果为空")
	}
	if a[0].ID == b[0].ID {
		t.Fatal("行号平移后 chunk ID 应变化")
	}
}

func TestGoChunkerRealSyncerBoundaries(t *testing.T) {
	raw, err := os.ReadFile("../workspace/syncer.go")
	if err != nil {
		t.Fatalf("读取真实 syncer.go: %v", err)
	}
	content := string(raw)
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "internal/workspace/syncer.go", Content: content})
	if capability != CapabilityAST {
		t.Fatalf("真实 Go 文件应走 AST，got %s", capability)
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	wantSymbols := []string{"Syncer.syncSingleflight", "Syncer.Search", "Syncer.Sync", "Syncer.WorkspaceStatus", "Syncer.WorkspaceChanged"}
	for _, symbol := range wantSymbols {
		found := false
		for _, chunk := range chunks {
			if chunk.SymbolHint != symbol {
				continue
			}
			found = true
			method := symbol[strings.LastIndex(symbol, ".")+1:]
			declared := false
			for i := chunk.StartLine; i <= chunk.EndLine && i <= len(lines); i++ {
				if strings.Contains(lines[i-1], "func (s *Syncer) "+method+"(") {
					declared = true
					break
				}
			}
			if !declared {
				t.Fatalf("chunk %s [%d-%d] 行区间内找不到 %s 的声明", symbol, chunk.StartLine, chunk.EndLine, method)
			}
			break
		}
		if !found {
			t.Fatalf("未找到 SymbolHint=%s 的 chunk", symbol)
		}
	}
	for _, chunk := range chunks {
		if chunk.StartLine < 1 || chunk.EndLine < chunk.StartLine || chunk.EndLine > len(lines) {
			t.Fatalf("非法行区间: %+v", chunk)
		}
	}
}

func TestGoChunkerMalformedFallsBack(t *testing.T) {
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "broken.go", Content: "package broken\nfunc oops( {\n"})
	if capability != CapabilityFallback {
		t.Fatalf("malformed go 应回退行窗口，got %s", capability)
	}
	if len(chunks) == 0 {
		t.Fatal("回退切分不应为空")
	}
}

func TestGoChunkerOversizedFunctionBounded(t *testing.T) {
	var body strings.Builder
	body.WriteString("package big\n\nfunc Huge() {\n")
	for i := 0; i < 500; i++ {
		body.WriteString("\t_ = \"padding line for oversized declaration test\"\n")
	}
	body.WriteString("}\n")
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "big.go", Content: body.String()})
	if capability != CapabilityAST {
		t.Fatalf("应保持 AST capability，got %s", capability)
	}
	if len(chunks) < 2 {
		t.Fatalf("超大函数应被细分，got %d chunks", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk.Content) > profile.MaxChunkBytes*2 {
			t.Fatalf("chunk 超出预算过多: %d bytes", len(chunk.Content))
		}
		if chunk.SymbolHint != "Huge" {
			t.Fatalf("细分块应保留符号名，got %q", chunk.SymbolHint)
		}
	}
}
