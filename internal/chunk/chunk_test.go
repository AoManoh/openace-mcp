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
	changed := Profile{ID: "default", Version: "test-bump", MaxChunkBytes: profile.MaxChunkBytes, WindowLines: profile.WindowLines, OverlapLines: profile.OverlapLines, DocWindowLines: profile.DocWindowLines}
	third, _ := changed.Split(file)
	if third[0].ID == first[0].ID {
		t.Fatal("profile 版本变化后 chunk ID 应变化")
	}
}

// TestContentHashExcludesLineRange 是 D4/K5 的直接断言（review S18 补全）：
// 同一声明内容在不同文件位置时 chunk ID 变化（含行号）而 ContentHash
// 不变（Stage 3 embedding 缓存复用的键控依据）。
func TestContentHashExcludesLineRange(t *testing.T) {
	profile := DefaultProfile()
	// Go AST 切分：同一函数在文件头部注释平移后内容逐字节相同、行号不同。
	a, _ := profile.Split(File{RelPath: "a.go", Content: "package a\n\nfunc F() int {\n\treturn 1\n}\n"})
	b, _ := profile.Split(File{RelPath: "a.go", Content: "package a\n\n// shifted by an extra comment block\n\nfunc F() int {\n\treturn 1\n}\n"})
	var chunkA, chunkB *Chunk
	for i := range a {
		if a[i].SymbolHint == "F" {
			chunkA = &a[i]
		}
	}
	for i := range b {
		if b[i].SymbolHint == "F" {
			chunkB = &b[i]
		}
	}
	if chunkA == nil || chunkB == nil {
		t.Fatalf("应各切出函数 F: a=%v b=%v", chunkA, chunkB)
	}
	if chunkA.Content != chunkB.Content {
		t.Fatalf("函数内容应逐字节一致:\n%q\n%q", chunkA.Content, chunkB.Content)
	}
	if chunkA.StartLine == chunkB.StartLine {
		t.Fatal("行号应因平移不同")
	}
	if chunkA.ID == chunkB.ID {
		t.Fatal("行号平移后 chunk ID 应变化")
	}
	if chunkA.ContentHash != chunkB.ContentHash {
		t.Fatalf("ContentHash 必须只依赖内容（K5）: %s vs %s", chunkA.ContentHash, chunkB.ContentHash)
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
	// Stage 7 删除 legacy Syncer 后,锚点换扫描/ignore 家族的常驻方法
	// (本测试以真实 syncer.go 为夹具,符号集随其演化)。
	wantSymbols := []string{"ruleStack.Match", "ruleStack.unwindTo", "ignoreRules.Match", "ScanStats", "ignoreRule.matches"}
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
				line := lines[i-1]
				if strings.Contains(line, method+"(") && strings.Contains(line, "func ") ||
					strings.Contains(line, "type "+method+" ") {
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
