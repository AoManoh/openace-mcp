package chunk

// 语言批次 2（Rust）门禁测试：门 2 span 覆盖、门 3 符号抽取（impl/trait/
// 泛型/宏/mod 手工断言）、门 4 回退安全、门 5 确定性、门 6 大文件基准。
// 测试形态复刻批次 1（treesitter_test.go）。

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const rustFixture = `//! crate 级文档
use std::collections::HashMap;
use std::fmt;

pub fn top_level(v: i64) -> i64 {
    v + 1
}

/// Point 的文档
#[derive(Debug, Clone)]
pub struct Point<T> {
    x: T,
    y: T,
}

pub async fn fetch_all() -> usize {
    0
}

pub enum Shape {
    Circle(f64),
    Rect { w: f64, h: f64 },
}

fn separator_a() {}

pub union Bits {
    i: i32,
    f: f32,
}

fn separator_b() {}

macro_rules! table {
    ($k:expr) => {
        $k
    };
}

fn separator_c() {}

pub type Registry = HashMap<String, i64>;

pub trait Draw {
    fn draw(&self) -> String;
    fn area(&self) -> f64 {
        0.0
    }
}

impl<T: fmt::Debug> Point<T> {
    pub fn new(x: T, y: T) -> Self {
        Point { x, y }
    }
}

impl Draw for Shape {
    fn draw(&self) -> String {
        String::from("shape")
    }
}

pub mod util {
    pub fn helper() -> i32 {
        7
    }
}

mod tests;
`

// 门 2+3：Rust 顶层项切分——fn/struct/enum/union/trait/impl/macro/mod
// 符号齐备，doc 注释与 #[derive] 属性附着，use 序言合并。
func TestRustASTSplit(t *testing.T) {
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "src/lib.rs", Content: rustFixture})
	if capability != CapabilityAST {
		t.Fatalf("rust 应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.Capability != CapabilityAST || c.Language != "rust" {
			t.Fatalf("chunk 元数据错误: %+v", c)
		}
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 R1-R8：顶层项符号（fn/async fn/struct/enum/union/macro/trait/type）。
	for _, want := range []string{"top_level", "fetch_all", "Point", "Shape", "Bits", "table", "Draw", "Registry"} {
		if _, ok := symbols[want]; !ok {
			t.Fatalf("缺少符号 %q（已得 %v）", want, keys(symbols))
		}
	}
	// 断言 R9：doc 注释与 #[derive] 属性附着到 struct chunk（struct 与
	// inherent impl 符号同为 Point，按内容定位 struct 声明 chunk）。
	var pointStruct *Chunk
	for i := range chunks {
		if strings.Contains(chunks[i].Content, "pub struct Point<T> {") {
			pointStruct = &chunks[i]
			break
		}
	}
	if pointStruct == nil || pointStruct.SymbolHint != "Point" {
		t.Fatalf("struct Point chunk 缺失或符号错误: %+v", pointStruct)
	}
	if !strings.Contains(pointStruct.Content, "Point 的文档") || !strings.Contains(pointStruct.Content, "#[derive(Debug, Clone)]") {
		t.Fatalf("doc/属性未附着: %q", pointStruct.Content)
	}
	// 断言 R10：预算内 impl 块保持单 span——inherent impl 符号为基础类型名。
	if _, ok := symbols["Point::new"]; ok {
		t.Fatalf("预算内 impl 不应拆成员: %v", keys(symbols))
	}
	// 断言 R11：trait impl 块符号 "Draw for Shape"（简洁可检索）。
	implChunk, ok := symbols["Draw for Shape"]
	if !ok || !strings.Contains(implChunk.Content, "impl Draw for Shape") {
		t.Fatalf("trait impl 符号缺失: %v", keys(symbols))
	}
	// 断言 R12：use 序言（含 crate 文档注释）合并覆盖且无符号独占。
	preambleCovered := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "use std::collections::HashMap;") && strings.Contains(c.Content, "use std::fmt;") {
			preambleCovered = true
		}
	}
	if !preambleCovered {
		t.Fatalf("use 序言未合并覆盖")
	}
	// 断言 R13：inline mod 预算内单 span，符号为模块名。
	util, ok := symbols["util"]
	if !ok || !strings.Contains(util.Content, "pub fn helper()") {
		t.Fatalf("inline mod 符号缺失: %v", keys(symbols))
	}
	assertLineFidelity(t, rustFixture, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// buildOversizedRustImpl 生成超预算 inherent impl（含泛型），驱动
// Type::method 成员拆分。
func buildOversizedRustImpl() string {
	var b strings.Builder
	b.WriteString("use std::fmt;\n\npub struct Engine<T> {\n    inner: T,\n}\n\n")
	b.WriteString("impl<T: fmt::Debug> Engine<T> {\n")
	b.WriteString("    const SLOTS: usize = 8;\n\n")
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "    /// step_%d 的文档\n    pub fn step_%d(&self, v: i64) -> i64 {\n        v + %d\n    }\n\n", i, i, i)
	}
	b.WriteString("}\n")
	return b.String()
}

// 门 3：超预算 inherent impl 拆分——header 承载基础类型符号（泛型剥除），
// 方法带 Type::name 前缀，关联常量并入匿名区。
func TestRustOversizedImplSplitsPerMethod(t *testing.T) {
	profile := DefaultProfile()
	src := buildOversizedRustImpl()
	chunks, capability := profile.Split(File{RelPath: "engine.rs", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	methodSymbols := 0
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
		if strings.HasPrefix(c.SymbolHint, "Engine::step_") {
			methodSymbols++
		}
	}
	// 断言 R14：impl header 符号为基础类型名（Engine<T> → Engine）且含
	// impl 声明行与前置关联常量。
	header, ok := symbols["Engine"]
	if !ok || !strings.Contains(header.Content, "impl<T: fmt::Debug> Engine<T> {") {
		t.Fatalf("impl header 缺失: %v", keys(symbols))
	}
	if !strings.Contains(header.Content, "const SLOTS") {
		t.Fatalf("关联常量未并入 header: %q", header.Content)
	}
	// 断言 R15：方法符号 Engine::step_0（doc 注释附着）。
	step0, ok := symbols["Engine::step_0"]
	if !ok || !strings.Contains(step0.Content, "step_0 的文档") {
		t.Fatalf("impl 方法符号缺失或 doc 未附着: %v", keys(symbols))
	}
	// 断言 R16：方法级符号覆盖大部分方法。
	if methodSymbols < 10 {
		t.Fatalf("方法级符号过少: %d", methodSymbols)
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 3：超预算 trait impl 拆分——方法符号归属实现类型（Type::method）。
func TestRustOversizedTraitImplSplits(t *testing.T) {
	profile := DefaultProfile()
	var b strings.Builder
	b.WriteString("pub struct Renderer;\n\npub trait Paint {\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "    /// brush_%d 的默认实现\n    fn brush_%d(&self) -> i64 {\n        %d\n    }\n\n", i, i, i)
	}
	b.WriteString("}\n\nimpl Paint for Renderer {\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "    fn brush_%d(&self) -> i64 {\n        %d * 2\n    }\n\n", i, i)
	}
	b.WriteString("}\n")
	src := b.String()
	chunks, capability := profile.Split(File{RelPath: "paint.rs", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 R17：超预算 trait 拆分——trait 默认方法符号 Paint::brush_0。
	if _, ok := symbols["Paint::brush_0"]; !ok {
		t.Fatalf("trait 默认方法符号缺失: %v", keys(symbols))
	}
	// 断言 R18：trait 声明 header 符号 Paint。
	if _, ok := symbols["Paint"]; !ok {
		t.Fatalf("trait header 符号缺失: %v", keys(symbols))
	}
	// 断言 R19：trait impl header 符号 "Paint for Renderer"。
	if _, ok := symbols["Paint for Renderer"]; !ok {
		t.Fatalf("trait impl header 符号缺失: %v", keys(symbols))
	}
	// 断言 R20：trait impl 方法符号 Renderer::brush_0（归属实现类型）。
	if _, ok := symbols["Renderer::brush_0"]; !ok {
		t.Fatalf("trait impl 方法符号缺失: %v", keys(symbols))
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 3：超预算 inline mod 按容器拆分——成员 fn/struct 带 mod:: 前缀；
// #[cfg(test)] 属性附着到 mod header。
func TestRustOversizedModSplitsMembers(t *testing.T) {
	profile := DefaultProfile()
	var b strings.Builder
	b.WriteString("pub fn outer() -> i32 {\n    1\n}\n\n#[cfg(test)]\nmod tests {\n    use super::*;\n\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "    /// case_%d 说明\n    #[test]\n    fn case_%d() {\n        assert_eq!(outer(), 1);\n    }\n\n", i, i)
	}
	b.WriteString("    pub struct Probe {\n        pub hits: usize,\n    }\n}\n")
	src := b.String()
	chunks, capability := profile.Split(File{RelPath: "mod_split.rs", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 R21：mod header 符号 tests 且 #[cfg(test)] 属性附着。
	header, ok := symbols["tests"]
	if !ok || !strings.Contains(header.Content, "#[cfg(test)]") {
		t.Fatalf("mod header 缺失或属性未附着: %v", keys(symbols))
	}
	// 断言 R22：mod 内 fn 符号 tests::case_0（#[test] 属性附着到成员）。
	case0, ok := symbols["tests::case_0"]
	if !ok || !strings.Contains(case0.Content, "#[test]") {
		t.Fatalf("mod 成员 fn 符号缺失或属性未附着: %v", keys(symbols))
	}
	// 断言 R23：mod 内 struct 符号 tests::Probe。
	if _, ok := symbols["tests::Probe"]; !ok {
		t.Fatalf("mod 成员 struct 符号缺失: %v", keys(symbols))
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 1 根因修复：pinned grammar 不接受 primitive 名作宏名（str! 等，
// snapbox 断言宏在 cargo 测试套广泛使用），等长规范化后须走 AST 且
// chunk 内容逐字节保持原文（规范化字节不得外泄）。
func TestRustPrimitiveNamedMacroParses(t *testing.T) {
	profile := DefaultProfile()
	src := `pub fn check_version() {
    let cases = &[("1.0", str!["1.0"]), ("2.0", str![[r#"
2.0
"#]])];
    assert_eq!(cases.len(), 2);
}

pub fn keep_literal() -> &'static str {
    "call str!(x) inside string"
}

pub fn not_a_macro(a: usize, b: usize) -> bool {
    a != b
}
`
	chunks, capability := profile.Split(File{RelPath: "snapbox.rs", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("str! 宏文件应经规范化走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 R24：三个 fn 符号齐备。
	for _, want := range []string{"check_version", "keep_literal", "not_a_macro"} {
		if _, ok := symbols[want]; !ok {
			t.Fatalf("缺少符号 %q（已得 %v）", want, keys(symbols))
		}
	}
	// 断言 R25：chunk 内容保持原文——str! 原样、字符串字面量原样。
	if !strings.Contains(symbols["check_version"].Content, `str!["1.0"]`) {
		t.Fatalf("规范化字节泄漏进 chunk 内容: %q", symbols["check_version"].Content)
	}
	if !strings.Contains(symbols["keep_literal"].Content, `"call str!(x) inside string"`) {
		t.Fatalf("字符串字面量被改写: %q", symbols["keep_literal"].Content)
	}
	// 断言 R26：`a != b` 比较不受规范化触碰（内容原样即证）。
	if !strings.Contains(symbols["not_a_macro"].Content, "a != b") {
		t.Fatalf("!= 比较被误改写: %q", symbols["not_a_macro"].Content)
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 1 根因修复：cargo-script frontmatter（RFC 3503）掩码后主体走 AST，
// frontmatter 行以匿名序言 span 补入（门 2 无缝隙）；unclosed 栅栏保持
// honest fallback。
func TestRustFrontmatterScriptParses(t *testing.T) {
	profile := DefaultProfile()
	src := `#!/usr/bin/env -S cargo -Zscript
---
[dependencies]
clap = "4"
---

use std::env;

fn main() {
    println!("script body");
}
`
	chunks, capability := profile.Split(File{RelPath: "tool.rs", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("frontmatter 脚本应经掩码走 AST: %v", capability)
	}
	var mainSeen, frontmatterCovered bool
	for _, c := range chunks {
		if c.SymbolHint == "main" {
			mainSeen = true
		}
		if strings.Contains(c.Content, `clap = "4"`) {
			frontmatterCovered = true
		}
	}
	if !mainSeen {
		t.Fatalf("脚本主体符号缺失: %+v", chunks)
	}
	if !frontmatterCovered {
		t.Fatalf("frontmatter 行未被序言 span 覆盖")
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)

	// unclosed 栅栏（cargo 亦报错）：不掩码、整文件回退。
	broken := "---\n[dependencies]\nclap = \"4\"\n\nfn main() {}\n"
	_, capability = profile.Split(File{RelPath: "broken.rs", Content: broken})
	if capability != CapabilityFallback {
		t.Fatalf("unclosed frontmatter 应回退: %v", capability)
	}
}

// 门 4：Rust 病理输入不 panic、语言级回退如实上报。
func TestRustMalformedInputsFallBack(t *testing.T) {
	profile := DefaultProfile()
	cases := map[string]File{
		"截断 fn":     {RelPath: "a.rs", Content: "pub fn broken(:\n    1"},
		"括号错配":      {RelPath: "b.rs", Content: "impl Foo { fn f( { } }"},
		"二进制垃圾":     {RelPath: "c.rs", Content: string([]byte{0x00, 0xFF, 0xFE, 0x89, 0x50, 0x4E, 0x47, 0x0A, 0x30})},
		"java 语法误入": {RelPath: "d.rs", Content: "public class Broken { void f() {} }"},
	}
	for name, file := range cases {
		chunks, capability := profile.Split(file)
		if capability != CapabilityFallback {
			t.Fatalf("%s: 应整文件回退, got %v (%d chunks)", name, capability, len(chunks))
		}
	}
	// 超长单行（生成物/宏展开）不进 AST，走字节窗口降级。
	minified := "pub fn m() {" + strings.Repeat(" g();", 2000) + "}\n"
	chunks, capability := profile.Split(File{RelPath: "m.rs", Content: minified})
	if capability != CapabilityFallback || len(chunks) == 0 {
		t.Fatalf("超长单行应降级字节窗口: cap=%v n=%d", capability, len(chunks))
	}
	// 深嵌套表达式有界完成（AST 或回退均可，不得 hang/panic）。
	deep := "pub fn deep() -> i64 { " + strings.Repeat("(", 2000) + "1" + strings.Repeat(")", 2000) + " }\n"
	chunks, capability = profile.Split(File{RelPath: "deep.rs", Content: deep})
	if len(chunks) == 0 {
		t.Fatalf("深嵌套应有产出（AST 或回退），got 0 (cap=%v)", capability)
	}
}

// 门 5：同文件双跑逐字段一致（确定性）。
func TestRustDeterminism(t *testing.T) {
	profile := DefaultProfile()
	first, _ := profile.Split(File{RelPath: "lib.rs", Content: rustFixture})
	oversized := buildOversizedRustImpl()
	firstBig, _ := profile.Split(File{RelPath: "engine.rs", Content: oversized})
	for i := 0; i < 3; i++ {
		again, _ := profile.Split(File{RelPath: "lib.rs", Content: rustFixture})
		if len(first) != len(again) {
			t.Fatalf("切分数漂移: %d vs %d", len(first), len(again))
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("chunk %d 不确定: %+v vs %+v", j, first[j], again[j])
			}
		}
		againBig, _ := profile.Split(File{RelPath: "engine.rs", Content: oversized})
		if len(firstBig) != len(againBig) {
			t.Fatalf("超预算 impl 切分数漂移: %d vs %d", len(firstBig), len(againBig))
		}
		for j := range firstBig {
			if firstBig[j] != againBig[j] {
				t.Fatalf("超预算 impl chunk %d 不确定", j)
			}
		}
	}
}

// buildLargeRustSource 生成 ≥5K 行合成 Rust 源（robustness 基准形态）。
func buildLargeRustSource(fns int) string {
	var b strings.Builder
	b.WriteString("//! 生成模块\n\n")
	for i := 0; i < fns; i++ {
		fmt.Fprintf(&b, "/// gen_%d 的生成文档\npub fn gen_%d(input: i64) -> i64 {\n    let acc = input + %d;\n    acc * 31\n}\n\n", i, i, i)
	}
	return b.String()
}

// 门 6：合成 ≥5K 行 Rust 大文件切分基准。
func BenchmarkRustLargeFileSplit(b *testing.B) {
	profile := DefaultProfile()
	src := buildLargeRustSource(850) // 6 行/fn ≈ 5102 行
	if lines := strings.Count(src, "\n"); lines < 5000 {
		b.Fatalf("基准文件行数不足: %d", lines)
	}
	file := File{RelPath: "generated.rs", Content: src}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, capability := profile.Split(file)
		if capability != CapabilityAST || len(chunks) == 0 {
			b.Fatalf("大文件切分异常: cap=%v n=%d", capability, len(chunks))
		}
	}
}

// 门 6（真实文件）：fixtures 最大 Rust 文件（ripgrep defs.rs 8161 行）
// 切分基准；fixture 缺失时跳过（评测资产不入 CI）。
func BenchmarkRustRealFileSplit(b *testing.B) {
	raw, err := os.ReadFile("../../docs/benchmarks/fixtures/ripgrep/crates/core/flags/defs.rs")
	if err != nil {
		b.Skipf("fixture 缺失: %v", err)
	}
	profile := DefaultProfile()
	file := File{RelPath: "defs.rs", Content: string(raw)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, capability := profile.Split(file)
		if capability != CapabilityAST || len(chunks) == 0 {
			b.Fatalf("真实大文件切分异常: cap=%v n=%d", capability, len(chunks))
		}
	}
}
