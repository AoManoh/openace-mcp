package chunk

// 健壮性批次回归测试（诊断报告 2026-08-03 §5 M2/M3/M4）：
// M2 splitGo 超长单行守卫与 splitOversized 单行字节细分；
// M3 //line 指令下行号映射取物理行；
// M4 tree-sitter 兄弟遍历复杂度与 Tree 释放后的池化复用安全。

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// —— M2：MaxChunkBytes 必须对 Go 路径也是硬上限 ——

// bindata 风格巨型单行（合法 Go）必须整文件降级字节窗口：与
// treesitter/行窗口路径的 hasOversizedLine 守卫同款口径，capability
// 如实上报 fallback，不得产出 MiB 级单 chunk 且仍报 ast。
func TestGoOversizedSingleLineDegradesToByteWindow(t *testing.T) {
	profile := DefaultProfile()
	var payload strings.Builder
	for i := 0; payload.Len() < 200*1024; i++ {
		fmt.Fprintf(&payload, "\\x%02x", i%256)
	}
	src := "package blob\n\n// bindata 生成物\nvar blobData = \"" + payload.String() + "\"\n"
	chunks, capability := profile.Split(File{RelPath: "bindata.go", Content: src})
	if capability != CapabilityFallback {
		t.Fatalf("巨型单行 Go 文件应整文件降级字节窗口，capability 不得仍报 %v", capability)
	}
	if len(chunks) == 0 {
		t.Fatal("降级后应有产出")
	}
	for _, c := range chunks {
		if len(c.Content) > profile.MaxChunkBytes*2 {
			t.Fatalf("MaxChunkBytes 失守: chunk %d bytes > %d (%s:%d-%d)",
				len(c.Content), profile.MaxChunkBytes*2, c.RelPath, c.StartLine, c.EndLine)
		}
		if c.Capability != CapabilityFallback {
			t.Fatalf("降级 chunk 必须如实上报 fallback，got %v", c.Capability)
		}
	}
}

// (MaxChunkBytes, maxLineBytes] 区间的单行不触发整文件降级，由
// splitOversized 的第二层守卫按字节窗口细分，细分块保留符号与行号。
func TestGoSingleLineOverBudgetSplitsByBytes(t *testing.T) {
	profile := DefaultProfile()
	var payload strings.Builder
	for i := 0; payload.Len() < 3*1024; i++ {
		fmt.Fprintf(&payload, "%04d", i)
	}
	src := "package mid\n\nvar midData = \"" + payload.String() + "\"\n"
	chunks, capability := profile.Split(File{RelPath: "mid.go", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("3KB 单行未超 maxLineBytes，应保持 AST 路径: %v", capability)
	}
	if len(chunks) < 2 {
		t.Fatalf("单行超预算应被字节细分，got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if len(c.Content) > profile.MaxChunkBytes {
			t.Fatalf("单行超预算未按字节细分: %d bytes > %d", len(c.Content), profile.MaxChunkBytes)
		}
		if c.SymbolHint != "midData" {
			t.Fatalf("细分块应保留符号名，got %q", c.SymbolHint)
		}
		if c.StartLine != 3 || c.EndLine != 3 {
			t.Fatalf("单行细分块行号应恒为该行: %d-%d", c.StartLine, c.EndLine)
		}
	}
}

// —— M3：`//line` 指令不得干扰行号映射 ——

// goyacc/cgo 生成文件携带 //line 指令时，chunk 必须按物理行取文：
// 声明内容不得错位（取到指令行/邻居声明的行），虚拟行号超出物理行数
// 时不得静默丢声明。
func TestGoLineDirectiveKeepsPhysicalLines(t *testing.T) {
	profile := DefaultProfile()
	src := `package gen

//line parser.y:1
func Parse() int {
	return 1
}

//line huge.y:9999

func Late() int {
	return 2
}
`
	chunks, capability := profile.Split(File{RelPath: "gen.go", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("//line 指令文件是合法 Go，应走 AST: %v", capability)
	}
	lines := strings.Split(src, "\n")
	want := map[string]string{
		"Parse": "func Parse() int {",
		"Late":  "func Late() int {",
	}
	seen := map[string]bool{}
	for _, c := range chunks {
		if c.StartLine < 1 || c.EndLine > len(lines) || c.EndLine < c.StartLine {
			t.Fatalf("行区间非法: %d-%d（共 %d 行）", c.StartLine, c.EndLine, len(lines))
		}
		if got := strings.Join(lines[c.StartLine-1:c.EndLine], "\n"); got != c.Content {
			t.Fatalf("chunk [%d-%d] 内容与物理源行脱钩:\nwant %q\ngot  %q",
				c.StartLine, c.EndLine, got, c.Content)
		}
		decl, cares := want[c.SymbolHint]
		if !cares {
			continue
		}
		seen[c.SymbolHint] = true
		if !strings.Contains(c.Content, decl) {
			t.Fatalf("符号 %s 的 chunk [%d-%d] 不含真实声明行 %q（//line 指令错位）:\n%s",
				c.SymbolHint, c.StartLine, c.EndLine, decl, c.Content)
		}
	}
	for symbol := range want {
		if !seen[symbol] {
			t.Fatalf("符号 %s 的声明被静默丢弃（//line 虚拟行号超出物理行数）", symbol)
		}
	}
}

// —— M4：扁平文件遍历复杂度 + Tree 释放/池化复用安全 ——

func flatPythonSource(statements int) string {
	var b strings.Builder
	for i := 0; i < statements; i++ {
		fmt.Fprintf(&b, "CONST_%05d = %d\n", i, i)
	}
	return b.String()
}

// 4 万条顶层短语句的扁平生成文件（720KB，workspace 1MiB 上限内的
// 真实形态）必须在秒级内完成：NamedChild(i) 线性扫描回归时本用例
// 实测 13.9s（O(n²)），O(n) 遍历实测约 0.7s（parse 占大头）；阈值
// 10s 只拦复杂度回归、不假设机器速度。
func TestFlatFileSplitBounded(t *testing.T) {
	if raceEnabled {
		t.Skip("race 插桩使 parse 超过 2s 超时预算而回退，计时亦失真；复杂度回归由非 race 运行拦截")
	}
	profile := DefaultProfile()
	src := flatPythonSource(40000)
	begin := time.Now()
	chunks, capability := profile.Split(File{RelPath: "gen/constants.py", Content: src})
	elapsed := time.Since(begin)
	if capability != CapabilityAST || len(chunks) == 0 {
		t.Fatalf("扁平常量表应走 AST: cap=%v n=%d", capability, len(chunks))
	}
	t.Logf("40000 顶层语句切分耗时 %v（%d chunks）", elapsed, len(chunks))
	if elapsed > 10*time.Second {
		t.Fatalf("40000 顶层语句切分耗时 %v，疑似兄弟遍历 O(n²) 回归", elapsed)
	}
}

// BenchmarkFlatPythonSplit 供 M4 优化前后 b.N 对比（5 千顶层语句）。
func BenchmarkFlatPythonSplit(b *testing.B) {
	profile := DefaultProfile()
	file := File{RelPath: "flat.py", Content: flatPythonSource(5000)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, capability := profile.Split(file)
		if capability != CapabilityAST || len(chunks) == 0 {
			b.Fatalf("扁平文件切分异常: cap=%v n=%d", capability, len(chunks))
		}
	}
}

// 成功路径与失败路径（语法错误早退）都释放 Tree 后，池化 arena 被
// 后续解析复用不得产生 use-after-release：交替切分好/坏文件多轮，
// 产出必须与首轮逐字段一致；包级排水 DrainParserPools 幂等可重复调。
func TestTreeReleaseAndDrainSafety(t *testing.T) {
	profile := DefaultProfile()
	good := File{RelPath: "g.py", Content: pyFixture}
	bad := File{RelPath: "b.py", Content: "def broken(:\n    pass\n"}
	first, capability := profile.Split(good)
	if capability != CapabilityAST || len(first) == 0 {
		t.Fatalf("基准切分异常: cap=%v n=%d", capability, len(first))
	}
	for round := 0; round < 5; round++ {
		if _, c := profile.Split(bad); c != CapabilityFallback {
			t.Fatalf("坏文件应回退: %v", c)
		}
		again, c := profile.Split(good)
		if c != CapabilityAST || len(again) != len(first) {
			t.Fatalf("释放后复用产出漂移: cap=%v n=%d（want %d）", c, len(again), len(first))
		}
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("第 %d 轮 chunk %d 不一致（疑似 use-after-release）:\n%+v\n%+v",
					round, i, first[i], again[i])
			}
		}
		DrainParserPools()
	}
	DrainParserPools()
}
