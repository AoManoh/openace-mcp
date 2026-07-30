package chunk

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// —— 切分语义（§9.1 对齐：声明级、注释附着、嵌套归属、符号独立） ——

const pyFixture = `import os

# 模块常量说明
DEFAULT = 1

# loader 的说明注释
@decorator
def load(path):
    """docstring"""
    return os.path.exists(path)

class Loader:
    """类 docstring"""
    limit = 10

    def read(self, p):
        return p

async def fetch(url):
    return url
`

func TestPythonASTSplit(t *testing.T) {
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "pkg/loader.py", Content: pyFixture})
	if capability != CapabilityAST {
		t.Fatalf("python 应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.Capability != CapabilityAST || c.Language != "python" {
			t.Fatalf("chunk 元数据错误: %+v", c)
		}
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	for _, want := range []string{"load", "Loader", "fetch"} {
		if _, ok := symbols[want]; !ok {
			t.Fatalf("缺少符号 %q（已得 %v）", want, keys(symbols))
		}
	}
	// 装饰器与前导注释附着：load 的 chunk 应从 "# loader 的说明注释" 行开始。
	load := symbols["load"]
	if !strings.HasPrefix(load.Content, "# loader 的说明注释") {
		t.Fatalf("装饰器/注释未附着: %q (start=%d)", load.Content, load.StartLine)
	}
	if !strings.Contains(load.Content, "@decorator") || !strings.Contains(load.Content, "docstring") {
		t.Fatalf("load chunk 应包含装饰器与 docstring: %q", load.Content)
	}
	// 函数符号声明独立成 chunk：fetch 不与其它声明合并。
	fetch := symbols["fetch"]
	if strings.Contains(fetch.Content, "class Loader") {
		t.Fatalf("函数 chunk 不应吞并类声明: %q", fetch.Content)
	}
	assertLineFidelity(t, pyFixture, chunks)
	assertNoDuplicateIDs(t, chunks)
}

const tsFixture = `import { x } from "./x";

// Foo 接口说明
export interface Foo { id: number }

/** Service 的 JSDoc */
export class Service {
  private total = 0;
  charge(amount: number): number { return this.total += amount; }
}

export const handler = (req: string) => req.length;

export default function main(): void {}

type Alias = Foo | null;

export function guard(f: Foo) { return f.id > 0; }
`

func TestTypeScriptASTSplit(t *testing.T) {
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "src/service.ts", Content: tsFixture})
	if capability != CapabilityAST {
		t.Fatalf("typescript 应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	for _, want := range []string{"Foo", "Service", "handler", "main", "Alias", "guard"} {
		if _, ok := symbols[want]; !ok {
			t.Fatalf("缺少符号 %q（已得 %v）", want, keys(symbols))
		}
	}
	// export 包装解包取符号，chunk 内容仍覆盖 export 关键字与 JSDoc。
	service := symbols["Service"]
	if !strings.Contains(service.Content, "export class Service") || !strings.Contains(service.Content, "JSDoc") {
		t.Fatalf("Service chunk 应含 export 与 JSDoc: %q", service.Content)
	}
	// 箭头函数常量按函数级对待：handler 拥有独立 chunk 且不并入相邻声明。
	handler := symbols["handler"]
	if strings.Contains(handler.Content, "function main") {
		t.Fatalf("箭头函数不应与相邻声明合并: %q", handler.Content)
	}
	assertLineFidelity(t, tsFixture, chunks)
	assertNoDuplicateIDs(t, chunks)
}

func TestTSXAndJSXSplit(t *testing.T) {
	profile := DefaultProfile()
	tsx := "export function App() {\n  return <div className=\"x\">hi</div>;\n}\n"
	chunks, capability := profile.Split(File{RelPath: "web/App.tsx", Content: tsx})
	if capability != CapabilityAST {
		t.Fatalf(".tsx 应经 tsx grammar 走 AST: %v", capability)
	}
	if len(chunks) != 1 || chunks[0].SymbolHint != "App" {
		t.Fatalf("App 组件切分异常: %+v", chunks)
	}
	jsx := "const App = () => <div>hi</div>;\n\nexport default App;\n"
	chunks, capability = profile.Split(File{RelPath: "web/App.jsx", Content: jsx})
	if capability != CapabilityAST {
		t.Fatalf(".jsx 应经 javascript grammar 走 AST: %v", capability)
	}
	found := false
	for _, c := range chunks {
		if c.SymbolHint == "App" {
			found = true
		}
	}
	if !found {
		t.Fatalf("jsx 箭头组件符号缺失: %+v", chunks)
	}
}

func TestJavaScriptCommonJSSplit(t *testing.T) {
	profile := DefaultProfile()
	js := `const util = require("util");

function plain(a) { return a; }

class Widget {
  render() { return null; }
}

module.exports = { plain, Widget };
`
	chunks, capability := profile.Split(File{RelPath: "lib/widget.js", Content: js})
	if capability != CapabilityAST {
		t.Fatalf("javascript 应走 AST: %v", capability)
	}
	symbols := map[string]bool{}
	for _, c := range chunks {
		symbols[c.SymbolHint] = true
	}
	if !symbols["plain"] || !symbols["Widget"] {
		t.Fatalf("符号缺失: %v", symbols)
	}
	assertLineFidelity(t, js, chunks)
}

// 超预算类拆分：header 承载类名，方法独立 chunk 且符号带 Class. 前缀。
func TestOversizedClassSplitsPerMethod(t *testing.T) {
	profile := DefaultProfile()
	var b strings.Builder
	b.WriteString("class Big:\n    \"\"\"大类\"\"\"\n    limit = 10\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "    def method_%d(self, value):\n        # 方法 %d 的注释说明，占据一定体积\n        return value + %d\n\n", i, i, i)
	}
	src := b.String()
	chunks, capability := profile.Split(File{RelPath: "big.py", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	var headerSeen bool
	methodSymbols := 0
	for _, c := range chunks {
		if c.SymbolHint == "Big" && strings.Contains(c.Content, "大类") {
			headerSeen = true
		}
		if strings.HasPrefix(c.SymbolHint, "Big.method_") {
			methodSymbols++
		}
	}
	if !headerSeen {
		t.Fatalf("类 header chunk 缺失")
	}
	// 小方法允许相邻合并（symbol 取组内最大成员），但方法符号必须保留
	// Class. 前缀且覆盖大部分方法。
	if methodSymbols < 10 {
		t.Fatalf("方法级符号过少: %d", methodSymbols)
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 同行多声明归并为单 span，不产出重复 chunk ID（行覆盖式取文的护栏）。
func TestSameLineDeclarationsCoalesce(t *testing.T) {
	profile := DefaultProfile()
	js := "const a = 1; const b = 2; function f() { return a + b; }\nconst c = 3;\n"
	chunks, capability := profile.Split(File{RelPath: "one.js", Content: js})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	assertNoDuplicateIDs(t, chunks)
	assertLineFidelity(t, js, chunks)
}

// —— 门禁 3：malformed/adversarial 输入不崩溃、如实回退 ——

func TestMalformedInputsFallBack(t *testing.T) {
	profile := DefaultProfile()
	cases := map[string]File{
		"截断 python":   {RelPath: "a.py", Content: "def broken(:\n    pass"},
		"截断 ts":       {RelPath: "a.ts", Content: "export class {"},
		"二进制垃圾":       {RelPath: "b.py", Content: string([]byte{0x00, 0xFF, 0xFE, 0x89, 0x50, 0x4E, 0x47, 0x0A, 0x30})},
		"js 语法错误":     {RelPath: "c.js", Content: "function ( { ] )"},
		"tsx 走 ts 语法": {RelPath: "d.ts", Content: "export function App() { return <div>hi</div>; }\n"},
	}
	for name, file := range cases {
		chunks, capability := profile.Split(file)
		if capability != CapabilityFallback {
			t.Fatalf("%s: 语法错误应整文件回退, got %v (%d chunks)", name, capability, len(chunks))
		}
	}
}

func TestAdversarialDeepNesting(t *testing.T) {
	profile := DefaultProfile()
	deep := "x = " + strings.Repeat("(", 5000) + "1" + strings.Repeat(")", 5000) + "\n"
	chunks, capability := profile.Split(File{RelPath: "deep.py", Content: deep})
	if len(chunks) == 0 {
		t.Fatalf("深嵌套输入应有产出（AST 或回退），got 0 chunks (cap=%v)", capability)
	}
}

// 超长单行（minified/生成物）不进 AST 路径，沿用字节窗口降级（K7）。
func TestOversizedLineSkipsAST(t *testing.T) {
	profile := DefaultProfile()
	minified := "var a=1;" + strings.Repeat("f();", 2000) + "\n"
	chunks, capability := profile.Split(File{RelPath: "m.min.js", Content: minified})
	if capability != CapabilityFallback {
		t.Fatalf("minified 应回退: %v", capability)
	}
	if len(chunks) == 0 {
		t.Fatalf("字节窗口应有产出")
	}
}

// 空文件与空白文件：与行窗口语义一致（零 chunk）。
func TestEmptyAndBlankFiles(t *testing.T) {
	profile := DefaultProfile()
	for _, content := range []string{"", "   \n\n  \n"} {
		chunks, _ := profile.Split(File{RelPath: "e.py", Content: content})
		if len(chunks) != 0 {
			t.Fatalf("空内容应零 chunk: %q → %d", content, len(chunks))
		}
	}
}

// CRLF 归一：与 LF 版本产出相同的 ContentHash 集合（跨平台一致性）。
func TestCRLFNormalization(t *testing.T) {
	profile := DefaultProfile()
	lf := pyFixture
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	lfChunks, _ := profile.Split(File{RelPath: "x.py", Content: lf})
	crlfChunks, _ := profile.Split(File{RelPath: "x.py", Content: crlf})
	if len(lfChunks) != len(crlfChunks) {
		t.Fatalf("CRLF 切分数不一致: %d vs %d", len(lfChunks), len(crlfChunks))
	}
	for i := range lfChunks {
		if lfChunks[i].ContentHash != crlfChunks[i].ContentHash {
			t.Fatalf("CRLF hash 漂移: %d", i)
		}
	}
}

// —— 门禁 4（单元级）：AST 路径无内容遗漏 —— 每个非空源码行都被至少
// 一个 chunk 的行区间覆盖（golden 对比的仓库级验证在六门禁报告脚本）。

func assertLineFidelity(t *testing.T, src string, chunks []Chunk) {
	t.Helper()
	lines := strings.Split(normalizeNewlines(src), "\n")
	covered := make([]bool, len(lines)+2)
	for _, c := range chunks {
		for l := c.StartLine; l <= c.EndLine && l < len(covered); l++ {
			covered[l] = true
		}
		// 行区间与内容一致性：chunk 内容必须与源码行逐字对应。
		want := strings.Join(lines[c.StartLine-1:min(c.EndLine, len(lines))], "\n")
		if want != c.Content {
			t.Fatalf("chunk 内容与源码行不符 %s:%d-%d:\nwant %q\ngot  %q", c.RelPath, c.StartLine, c.EndLine, want, c.Content)
		}
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !covered[i+1] {
			t.Fatalf("第 %d 行未被任何 chunk 覆盖: %q", i+1, line)
		}
	}
}

func assertNoDuplicateIDs(t *testing.T, chunks []Chunk) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range chunks {
		if seen[c.ID] {
			t.Fatalf("重复 chunk ID: %s (%s:%d-%d)", c.ID, c.RelPath, c.StartLine, c.EndLine)
		}
		seen[c.ID] = true
	}
}

// —— 并发安全：多语言并行切分（race detector 用例，S8 同款契约） ——

func TestConcurrentSplit(t *testing.T) {
	profile := DefaultProfile()
	files := []File{
		{RelPath: "a.py", Content: pyFixture},
		{RelPath: "b.ts", Content: tsFixture},
		{RelPath: "c.js", Content: "function f() { return 1; }\n"},
		{RelPath: "d.tsx", Content: "export const C = () => <p>x</p>;\n"},
	}
	var wg sync.WaitGroup
	for round := 0; round < 8; round++ {
		for _, file := range files {
			wg.Add(1)
			go func(f File) {
				defer wg.Done()
				chunks, capability := profile.Split(f)
				if capability != CapabilityAST || len(chunks) == 0 {
					t.Errorf("%s 并发切分异常: cap=%v n=%d", f.RelPath, capability, len(chunks))
				}
			}(file)
		}
	}
	wg.Wait()
}

// —— 确定性：同输入跨多次调用产出逐字段一致 ——

func TestTreeSitterDeterminism(t *testing.T) {
	profile := DefaultProfile()
	first, _ := profile.Split(File{RelPath: "s.py", Content: pyFixture})
	for i := 0; i < 3; i++ {
		again, _ := profile.Split(File{RelPath: "s.py", Content: pyFixture})
		if len(first) != len(again) {
			t.Fatalf("切分数漂移: %d vs %d", len(first), len(again))
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("chunk %d 不确定: %+v vs %+v", j, first[j], again[j])
			}
		}
	}
}

// 批次外语言（rust 等）不受影响，仍走行窗口。
func TestNonBatchLanguagesKeepFallback(t *testing.T) {
	profile := DefaultProfile()
	rust := "pub fn add(a: i32, b: i32) -> i32 { a + b }\n"
	_, capability := profile.Split(File{RelPath: "lib.rs", Content: rust})
	if capability != CapabilityFallback {
		t.Fatalf("rust 应保持行窗口: %v", capability)
	}
}

func keys(m map[string]Chunk) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
