package chunk

// 语言批次 2（Java）门禁测试：门 2 span 覆盖（assertLineFidelity 无缝隙
// 断言）、门 3 符号抽取（嵌套类型/构造器/泛型手工断言）、门 4 回退安全、
// 门 5 确定性、门 6 大文件基准。测试形态复刻批次 1（treesitter_test.go）。

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const javaFixture = `package com.example.core;

import java.util.List;
import java.util.Map;

/** Repo 的 javadoc */
@Service
public class Repo<T> implements Store<T> {
    private static final int LIMIT = 64;

    public Repo(Map<String, T> seed) { }

    /** find 的说明 */
    @Override
    public List<T> find(String id) { return List.of(); }
}

interface Store<T> {
    void put(String key, T value);
    default int size() { return 0; }
}

enum Level {
    LOW, HIGH;
    public Level flip() { return this == LOW ? HIGH : LOW; }
}

record Pair(int left, int right) {
    public int sum() { return left + right; }
}

@interface Audited {
    String value() default "";
}
`

// 门 2+3：Java 顶层声明切分——class/interface/enum/record/annotation 符号
// 齐备，javadoc 与注解附着，序言（package/import）合并为匿名 chunk。
func TestJavaASTSplit(t *testing.T) {
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "src/Repo.java", Content: javaFixture})
	if capability != CapabilityAST {
		t.Fatalf("java 应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.Capability != CapabilityAST || c.Language != "java" {
			t.Fatalf("chunk 元数据错误: %+v", c)
		}
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 J1-J5：五种顶层类型声明各自持有符号。
	for _, want := range []string{"Repo", "Store", "Level", "Pair", "Audited"} {
		if _, ok := symbols[want]; !ok {
			t.Fatalf("缺少符号 %q（已得 %v）", want, keys(symbols))
		}
	}
	// 断言 J6：javadoc 与 @Service 注解附着在 Repo chunk 内。
	repo := symbols["Repo"]
	if !strings.Contains(repo.Content, "Repo 的 javadoc") || !strings.Contains(repo.Content, "@Service") {
		t.Fatalf("javadoc/注解未附着: %q", repo.Content)
	}
	// 断言 J7：package/import 序言合并进匿名 chunk（首 chunk 无符号且含两者）。
	first := chunks[0]
	if first.SymbolHint != "" && first.SymbolHint != "Repo" {
		t.Fatalf("首 chunk 意外符号: %+v", first)
	}
	preambleCovered := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "package com.example.core;") && strings.Contains(c.Content, "import java.util.Map;") {
			preambleCovered = true
		}
	}
	if !preambleCovered {
		t.Fatalf("package/import 序言未合并覆盖")
	}
	// 断言 J8：类未超预算时保持单 span——不产出 Repo.find 成员符号。
	if _, ok := symbols["Repo.find"]; ok {
		t.Fatalf("预算内类不应拆成员: %v", keys(symbols))
	}
	assertLineFidelity(t, javaFixture, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// buildOversizedJavaClass 生成超预算类：字段/静态块 + 构造器 + 常规与
// 泛型方法 + 嵌套类/嵌套枚举，覆盖批次 1 类成员语义的全部分支。
func buildOversizedJavaClass() string {
	var b strings.Builder
	b.WriteString("package com.example.big;\n\n")
	b.WriteString("/** Big 的 javadoc */\npublic class Big {\n")
	b.WriteString("    private static final int LIMIT = 64;\n\n")
	b.WriteString("    static {\n        System.out.println(\"init\");\n    }\n\n")
	b.WriteString("    public Big(int seed) {\n        // 构造器\n    }\n\n")
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "    /** method_%d 的说明 */\n    public int method_%d(int value) {\n        return value + %d;\n    }\n\n", i, i, i)
	}
	b.WriteString("    public <R> R map(R seed) {\n        return seed;\n    }\n\n")
	b.WriteString("    public static class Builder {\n        public Big build() { return new Big(1); }\n    }\n\n")
	b.WriteString("    private enum State { OPEN, CLOSED }\n}\n")
	return b.String()
}

// 门 3：超预算类按批次 1 类成员语义拆分——header 承载类符号，方法/
// 构造器/嵌套类型带 Big. 前缀，字段与静态块并入匿名区（header）。
func TestJavaOversizedClassSplitsPerMember(t *testing.T) {
	profile := DefaultProfile()
	src := buildOversizedJavaClass()
	chunks, capability := profile.Split(File{RelPath: "Big.java", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	methodSymbols := 0
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
		if strings.HasPrefix(c.SymbolHint, "Big.method_") {
			methodSymbols++
		}
	}
	// 断言 J9：header 持有类符号且含 javadoc 与类声明行。
	header, ok := symbols["Big"]
	if !ok || !strings.Contains(header.Content, "public class Big {") || !strings.Contains(header.Content, "Big 的 javadoc") {
		t.Fatalf("类 header 缺失或不完整: %+v", header)
	}
	// 断言 J10：字段与静态块并入 header（匿名前置成员语义）。
	if !strings.Contains(header.Content, "LIMIT = 64") || !strings.Contains(header.Content, "static {") {
		t.Fatalf("字段/静态块未并入 header: %q", header.Content)
	}
	// 断言 J11：构造器符号 Big.Big。
	ctor, ok := symbols["Big.Big"]
	if !ok || !strings.Contains(ctor.Content, "public Big(int seed)") {
		t.Fatalf("构造器符号缺失: %v", keys(symbols))
	}
	// 断言 J12：方法级符号覆盖大部分方法（小方法允许相邻合并）。
	if methodSymbols < 10 {
		t.Fatalf("方法级符号过少: %d", methodSymbols)
	}
	// 断言 J13：泛型方法 Big.map。
	if _, ok := symbols["Big.map"]; !ok {
		t.Fatalf("泛型方法符号缺失: %v", keys(symbols))
	}
	// 断言 J14：嵌套类 Big.Builder 整体单 span（不递归拆分）。
	builder, ok := symbols["Big.Builder"]
	if !ok || !strings.Contains(builder.Content, "public Big build()") {
		t.Fatalf("嵌套类符号缺失或被拆散: %v", keys(symbols))
	}
	// 断言 J15：嵌套枚举 Big.State。
	if _, ok := symbols["Big.State"]; !ok {
		t.Fatalf("嵌套枚举符号缺失: %v", keys(symbols))
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 3：超预算 enum 的 enum_body_declarations 展开（常量区并入 header，
// 方法带 Enum. 前缀）；enum 常量分号独立成行不留缝隙（门 2 边界）。
func TestJavaOversizedEnumSplitsMethods(t *testing.T) {
	profile := DefaultProfile()
	var b strings.Builder
	b.WriteString("enum Op {\n    ADD,\n    SUB,\n    ;\n\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "    /** apply_%d 说明 */\n    public int apply_%d(int v) {\n        return v + %d;\n    }\n\n", i, i, i)
	}
	b.WriteString("}\n")
	src := b.String()
	chunks, capability := profile.Split(File{RelPath: "Op.java", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 J16：enum header 含常量区。
	header, ok := symbols["Op"]
	if !ok || !strings.Contains(header.Content, "ADD,") {
		t.Fatalf("enum header 缺失: %v", keys(symbols))
	}
	// 断言 J17：enum 方法符号 Op.apply_0。
	if _, ok := symbols["Op.apply_0"]; !ok {
		t.Fatalf("enum 方法符号缺失: %v", keys(symbols))
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 3：超预算 record 的紧凑构造器（compact_constructor_declaration）
// 与方法按类成员语义拆分。
func TestJavaOversizedRecordSplitsMembers(t *testing.T) {
	profile := DefaultProfile()
	var b strings.Builder
	b.WriteString("record Wide(int lo, int hi) {\n")
	b.WriteString("    Wide {\n        if (lo > hi) throw new IllegalArgumentException();\n    }\n\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "    /** slot_%d 说明 */\n    public int slot_%d() {\n        return lo + %d;\n    }\n\n", i, i, i)
	}
	b.WriteString("}\n")
	src := b.String()
	chunks, capability := profile.Split(File{RelPath: "Wide.java", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 J18：紧凑构造器符号 Wide.Wide。
	if _, ok := symbols["Wide.Wide"]; !ok {
		t.Fatalf("紧凑构造器符号缺失: %v", keys(symbols))
	}
	// 断言 J19：record 方法符号 Wide.slot_0。
	if _, ok := symbols["Wide.slot_0"]; !ok {
		t.Fatalf("record 方法符号缺失: %v", keys(symbols))
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 3：超预算 interface 的 default 方法按成员拆分。
func TestJavaOversizedInterfaceSplitsMethods(t *testing.T) {
	profile := DefaultProfile()
	var b strings.Builder
	b.WriteString("public interface Handler {\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "    /** on_%d 说明 */\n    default int on_%d(int v) {\n        return v * %d;\n    }\n\n", i, i, i)
	}
	b.WriteString("}\n")
	src := b.String()
	chunks, capability := profile.Split(File{RelPath: "Handler.java", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbols := map[string]Chunk{}
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbols[c.SymbolHint] = c
		}
	}
	// 断言 J20：interface 成员符号 Handler.on_0。
	if _, ok := symbols["Handler.on_0"]; !ok {
		t.Fatalf("interface 方法符号缺失: %v", keys(symbols))
	}
	assertLineFidelity(t, src, chunks)
}

// 门 4：Java 病理输入不 panic、语言级回退如实上报。
func TestJavaMalformedInputsFallBack(t *testing.T) {
	profile := DefaultProfile()
	cases := map[string]File{
		"截断 class":  {RelPath: "A.java", Content: "public class {"},
		"括号错配":      {RelPath: "B.java", Content: "class B { void f( { } }"},
		"二进制垃圾":     {RelPath: "C.java", Content: string([]byte{0x00, 0xFF, 0xFE, 0x89, 0x50, 0x4E, 0x47, 0x0A, 0x30})},
		"rust 语法误入": {RelPath: "D.java", Content: "pub fn broken(x: i32) -> i32 { x }"},
	}
	for name, file := range cases {
		chunks, capability := profile.Split(file)
		if capability != CapabilityFallback {
			t.Fatalf("%s: 应整文件回退, got %v (%d chunks)", name, capability, len(chunks))
		}
	}
	// 超长单行（生成物）不进 AST，走字节窗口降级。
	minified := "class M { void f() {" + strings.Repeat(" g();", 2000) + "} }\n"
	chunks, capability := profile.Split(File{RelPath: "M.java", Content: minified})
	if capability != CapabilityFallback || len(chunks) == 0 {
		t.Fatalf("超长单行应降级字节窗口: cap=%v n=%d", capability, len(chunks))
	}
}

// 门 5：同文件双跑逐字段一致（确定性）。
func TestJavaDeterminism(t *testing.T) {
	profile := DefaultProfile()
	first, _ := profile.Split(File{RelPath: "R.java", Content: javaFixture})
	oversized := buildOversizedJavaClass()
	firstBig, _ := profile.Split(File{RelPath: "Big.java", Content: oversized})
	for i := 0; i < 3; i++ {
		again, _ := profile.Split(File{RelPath: "R.java", Content: javaFixture})
		if len(first) != len(again) {
			t.Fatalf("切分数漂移: %d vs %d", len(first), len(again))
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("chunk %d 不确定: %+v vs %+v", j, first[j], again[j])
			}
		}
		againBig, _ := profile.Split(File{RelPath: "Big.java", Content: oversized})
		if len(firstBig) != len(againBig) {
			t.Fatalf("超预算类切分数漂移: %d vs %d", len(firstBig), len(againBig))
		}
		for j := range firstBig {
			if firstBig[j] != againBig[j] {
				t.Fatalf("超预算类 chunk %d 不确定", j)
			}
		}
	}
}

// buildLargeJavaSource 生成 ≥5K 行合成 Java 类（robustness 基准形态），
// 供门 6 大文件耗时基准与真实 fixture 互证。
func buildLargeJavaSource(methods int) string {
	var b strings.Builder
	b.WriteString("package com.example.gen;\n\npublic class Generated {\n")
	for i := 0; i < methods; i++ {
		fmt.Fprintf(&b, "    /** handler_%d 的生成说明 */\n    public long handler_%d(long input) {\n        long acc = input + %d;\n        acc = acc * 31 + %d;\n        return acc;\n    }\n\n", i, i, i, i)
	}
	b.WriteString("}\n")
	return b.String()
}

// 门 6：合成 ≥5K 行 Java 大文件切分基准（与 BenchmarkFlatPythonSplit 同形态）。
func BenchmarkJavaLargeFileSplit(b *testing.B) {
	profile := DefaultProfile()
	src := buildLargeJavaSource(750) // 7 行/方法 ≈ 5254 行
	if lines := strings.Count(src, "\n"); lines < 5000 {
		b.Fatalf("基准文件行数不足: %d", lines)
	}
	file := File{RelPath: "Generated.java", Content: src}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, capability := profile.Split(file)
		if capability != CapabilityAST || len(chunks) == 0 {
			b.Fatalf("大文件切分异常: cap=%v n=%d", capability, len(chunks))
		}
	}
}

// 门 6（真实文件）：fixtures 最大 Java 文件（nacos 2798 行）切分基准；
// fixture 缺失时跳过（评测资产不入 CI）。
func BenchmarkJavaRealFileSplit(b *testing.B) {
	raw, err := os.ReadFile("../../docs/benchmarks/fixtures/nacos/ai/src/test/java/com/alibaba/nacos/ai/service/skills/SkillOperationServiceImplTest.java")
	if err != nil {
		b.Skipf("fixture 缺失: %v", err)
	}
	profile := DefaultProfile()
	file := File{RelPath: "SkillOperationServiceImplTest.java", Content: string(raw)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, capability := profile.Split(file)
		if capability != CapabilityAST || len(chunks) == 0 {
			b.Fatalf("真实大文件切分异常: cap=%v n=%d", capability, len(chunks))
		}
	}
}
