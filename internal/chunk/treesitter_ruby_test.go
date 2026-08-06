package chunk

// 语言批次 4(Ruby)门禁测试:门 2 span 覆盖(assertLineFidelity 无缝隙
// 断言)、门 3 符号抽取(module/class 容器、singleton_method、class << self
// 手工断言)、门 4 回退安全、门 5 确定性、门 6 大文件基准。测试形态复刻
// 批次 2/3(treesitter_java_test.go / treesitter_csharp_test.go)。
// 节点形状经探针实测(gotreesitter v0.47.0 内嵌 ruby grammar):
// - 顶层:comment/call(require 与 do_block DSL)/assignment/module/class/
//   method/singleton_method;module 与 class 的成员在 body_statement 之下;
// - 容器声明行与首成员之间的注释被提升为容器的直接子节点(body_statement
//   之外),分类器须游走容器自身才能完成注释附着;
// - private/public 裸写解析为 identifier,attr_reader 等 DSL 是 call;
// - YAML/散文等宽松文本可被 ruby grammar 无错解析(一切皆表达式),垃圾
//   回退用例必须选实测带 ERROR 的输入(符号杂讯/二进制/截断 def)。

import (
	"fmt"
	"strings"
	"testing"
)

const rubyFixture = `# frozen_string_literal: true

require "json"
require_relative "helpers"

DEFAULT_CURRENCY = "USD"

# Billing 是计费领域的命名空间
module Billing
  # Invoice 表示一张应收账单
  class Invoice < ApplicationRecord
    attr_reader :total

    STATES = %i[draft open paid].freeze

    def initialize(total)
      @total = total
    end

    # total_cents 把金额换算为分
    def total_cents
      (@total * 100).round
    end

    def self.from_json(payload)
      new(JSON.parse(payload)["total"])
    end

    private

    def internal_note
      "secret"
    end
  end

  def self.configure
    yield self
  end
end

# top_helper 是顶层辅助函数
def top_helper(value)
  value * 2
end

def Billing.reset!
  @configured = false
end

Billing.configure do |config|
  config.registry = {}
end
`

// 门 2+3:Ruby 顶层声明切分——module 容器、顶层 def、def Constant.name
// 符号齐备,doc 注释附着,序言(魔法注释/require/顶层常量)合并为匿名
// chunk,带 do_block 的 DSL 调用保持匿名整块。
func TestRubyASTSplit(t *testing.T) {
	profile := DefaultProfile()
	chunks, capability := profile.Split(File{RelPath: "lib/billing.rb", Content: rubyFixture})
	if capability != CapabilityAST {
		t.Fatalf("ruby 应走 AST: %v", capability)
	}
	symbolSet := map[string]Chunk{}
	for _, c := range chunks {
		if c.Capability != CapabilityAST || c.Language != "ruby" {
			t.Fatalf("chunk 元数据错误: %+v", c)
		}
		if c.SymbolHint != "" {
			symbolSet[c.SymbolHint] = c
		}
	}
	// 断言 R1:module 容器与顶层方法符号。def Billing.reset! 的归属对象
	// 是常量,符号取 Billing.reset!(与 Ruby 调用形态一致)。
	for _, want := range []string{"Billing", "top_helper", "Billing.reset!"} {
		if _, ok := symbolSet[want]; !ok {
			t.Fatalf("缺少符号 %q(已得 %v)", want, keys(symbolSet))
		}
	}
	// 断言 R2:doc 注释附着——module 的前导注释并入 Billing chunk,顶层
	// def 的前导注释并入 top_helper chunk(walkSiblings 附着语义)。
	billing := symbolSet["Billing"]
	if !strings.Contains(billing.Content, "# Billing 是计费领域的命名空间") || !strings.Contains(billing.Content, "module Billing") {
		t.Fatalf("module doc 注释未附着: %q", billing.Content)
	}
	helper := symbolSet["top_helper"]
	if !strings.Contains(helper.Content, "# top_helper 是顶层辅助函数") || !strings.Contains(helper.Content, "def top_helper(value)") {
		t.Fatalf("顶层 def doc 注释未附着: %q", helper.Content)
	}
	// 断言 R3:isFunc 语义——顶层 def 是独立 chunk,不吞相邻 DSL 调用。
	if strings.Contains(helper.Content, "Billing.configure do") {
		t.Fatalf("函数 chunk 不应与相邻语句合并: %q", helper.Content)
	}
	// 断言 R4:预算内 module 保持单 span——不产出成员符号,且整容器
	// (含嵌套 class 与 def self.configure)都在 Billing chunk 内。
	for _, forbidden := range []string{"Billing.Invoice", "Billing.configure", "Billing.Invoice.total_cents"} {
		if _, ok := symbolSet[forbidden]; ok {
			t.Fatalf("预算内容器不应拆成员: %v", keys(symbolSet))
		}
	}
	if !strings.Contains(billing.Content, "class Invoice < ApplicationRecord") || !strings.Contains(billing.Content, "def self.configure") {
		t.Fatalf("预算内 module 应整体单 chunk: %q", billing.Content)
	}
	// 断言 R5:序言合并——魔法注释与 require 序言合并进匿名 chunk。
	preambleCovered := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "# frozen_string_literal: true") && strings.Contains(c.Content, `require "json"`) && strings.Contains(c.Content, "DEFAULT_CURRENCY") {
			preambleCovered = true
		}
	}
	if !preambleCovered {
		t.Fatalf("魔法注释/require/顶层常量序言未合并覆盖")
	}
	// 断言 R6:带 do_block 的 DSL 调用是匿名整块(不提取 configure 符号,
	// 内容完整覆盖)。
	dslCovered := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "Billing.configure do |config|") {
			dslCovered = true
			if c.SymbolHint != "" && c.SymbolHint != "Billing.reset!" {
				t.Fatalf("DSL 调用不应携带符号: %+v", c)
			}
		}
	}
	if !dslCovered {
		t.Fatalf("do_block DSL 调用未被覆盖")
	}
	assertLineFidelity(t, rubyFixture, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// buildOversizedRubyModule 生成超预算 module:嵌套 class(自身也超预算,
// 验证容器递归展开)含 attr_reader/常量/initialize/实例方法/operator 方法/
// self. 类方法/private 区,module 级 singleton_method 与 class << self 块,
// 覆盖成员语义全部分支。
func buildOversizedRubyModule() string {
	var b strings.Builder
	b.WriteString("# frozen_string_literal: true\n\n")
	b.WriteString("# Big 模块的说明\nmodule Big\n")
	b.WriteString("  # Wide 类的说明\n  class Wide\n")
	b.WriteString("    attr_reader :seed\n\n")
	b.WriteString("    STATES = %i[open closed].freeze\n\n")
	b.WriteString("    def initialize(seed)\n      @seed = seed\n    end\n\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "    # m_%d 的说明\n    def m_%d(value)\n      value * 31 + %d\n    end\n\n", i, i, i)
	}
	b.WriteString("    def self.from_json(payload)\n      new(payload)\n    end\n\n")
	b.WriteString("    def ==(other)\n      seed == other.seed\n    end\n\n")
	b.WriteString("    private\n\n")
	b.WriteString("    def hidden_note\n      \"secret\"\n    end\n  end\n\n")
	b.WriteString("  def self.configure\n    yield self\n  end\n\n")
	b.WriteString("  class << self\n    def registry\n      @registry ||= {}\n    end\n  end\nend\n")
	return b.String()
}

// 门 3:超预算 module 按容器语义递归展开——module header 承载 Big,嵌套
// class 自身超预算继续拆(三段符号 Big.Wide.m_i),singleton_method 与
// class << self 内方法都归属 Big. 前缀,attr_reader/常量并入 class header。
func TestRubyOversizedModuleSplitsPerMember(t *testing.T) {
	profile := DefaultProfile()
	src := buildOversizedRubyModule()
	if len(src) <= profile.MaxChunkBytes {
		t.Fatalf("fixture 未超预算: %d bytes", len(src))
	}
	chunks, capability := profile.Split(File{RelPath: "lib/big.rb", Content: src})
	if capability != CapabilityAST {
		t.Fatalf("应走 AST: %v", capability)
	}
	symbolSet := map[string]Chunk{}
	methodSymbols := 0
	for _, c := range chunks {
		if c.SymbolHint != "" {
			symbolSet[c.SymbolHint] = c
		}
		if strings.HasPrefix(c.SymbolHint, "Big.Wide.m_") {
			methodSymbols++
		}
	}
	// 断言 R7:module header 持有 Big 符号且含 doc 注释与 module 行。
	header, ok := symbolSet["Big"]
	if !ok || !strings.Contains(header.Content, "module Big") || !strings.Contains(header.Content, "# Big 模块的说明") {
		t.Fatalf("module header 缺失或不完整: %+v", header)
	}
	// 断言 R8:嵌套 class 超预算递归展开——class header 承载 Big.Wide,
	// 前置 attr_reader 与常量并入 header(匿名前置成员语义),grammar 把
	// 声明行后的注释提升为容器直接子节点,附着由容器游走完成。
	wideHeader, ok := symbolSet["Big.Wide"]
	if !ok || !strings.Contains(wideHeader.Content, "class Wide") {
		t.Fatalf("嵌套 class header 缺失: %v", keys(symbolSet))
	}
	if !strings.Contains(wideHeader.Content, "# Wide 类的说明") {
		t.Fatalf("容器直下注释未附着 class header: %q", wideHeader.Content)
	}
	if !strings.Contains(wideHeader.Content, "attr_reader :seed") || !strings.Contains(wideHeader.Content, "STATES = %i[open closed].freeze") {
		t.Fatalf("attr_reader/常量未并入 class header: %q", wideHeader.Content)
	}
	// 断言 R9:三段符号 Big.Wide.name——initialize/实例方法/operator 方法/
	// self. 类方法/private 后的方法全部产出。
	for _, want := range []string{"Big.Wide.initialize", "Big.Wide.m_0", "Big.Wide.from_json", "Big.Wide.==", "Big.Wide.hidden_note"} {
		if _, ok := symbolSet[want]; !ok {
			t.Fatalf("缺少成员符号 %q(已得 %v)", want, keys(symbolSet))
		}
	}
	// 断言 R10:方法级符号覆盖大部分方法(小方法允许相邻合并)。
	if methodSymbols < 10 {
		t.Fatalf("方法级符号过少: %d", methodSymbols)
	}
	// 断言 R11:isFunc 语义——单个方法 chunk 不吞相邻方法。
	m0 := symbolSet["Big.Wide.m_0"]
	if !strings.Contains(m0.Content, "def m_0(value)") || strings.Contains(m0.Content, "def m_1(value)") {
		t.Fatalf("方法 chunk 边界错误: %q", m0.Content)
	}
	if !strings.Contains(m0.Content, "# m_0 的说明") {
		t.Fatalf("成员 doc 注释未附着: %q", m0.Content)
	}
	// 断言 R12:module 级 singleton_method 归属 Big。
	configure, ok := symbolSet["Big.configure"]
	if !ok || !strings.Contains(configure.Content, "def self.configure") {
		t.Fatalf("module singleton_method 符号缺失: %v", keys(symbolSet))
	}
	// 断言 R13:class << self 块展开——方法按外层符号限定(Ruby 语义上
	// 即类方法 Big.registry),单例类首行并入首成员防覆盖缝隙。
	registry, ok := symbolSet["Big.registry"]
	if !ok || !strings.Contains(registry.Content, "def registry") {
		t.Fatalf("class << self 方法符号缺失: %v", keys(symbolSet))
	}
	if !strings.Contains(registry.Content, "class << self") {
		t.Fatalf("class << self 首行未并入首成员: %q", registry.Content)
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// 门 2:heredoc 的行几何不破坏切分(方法 span 覆盖 heredoc 内容行)。
func TestRubyHeredocGeometry(t *testing.T) {
	src := "class Mailer\n  def body_text\n    <<~TEXT\n      line one\n      line two\n    TEXT\n  end\nend\n"
	chunks := splitFor(t, DefaultProfile(), "lib/mailer.rb", src)
	assertAST(t, chunks)
	if chunkBySymbol(chunks, "Mailer") == nil {
		t.Fatalf("缺 Mailer 符号: %v", symbols(chunks))
	}
	assertLineFidelity(t, src, chunks)
}

// 门 4:Ruby 病理输入不 panic、语言级回退如实上报。ruby 是严格语言
// (splitTreeSitter errorTolerant=false):任一顶层子树带错即整文件回退。
// 用例全部经探针实测确认会产生 ERROR 节点(ruby 语法宽松,YAML/散文
// 反而能无错解析,不能作垃圾用例)。
func TestRubyMalformedInputsFallBack(t *testing.T) {
	profile := DefaultProfile()
	cases := map[string]File{
		"符号杂讯":      {RelPath: "a.rb", Content: "%%% ??? ;;; not ruby at all\n*** &&&\n"},
		"截断 def":    {RelPath: "b.rb", Content: "def broken(\nclass {"},
		"二进制垃圾":     {RelPath: "c.rb", Content: string([]byte{0x00, 0xFF, 0xFE, 0x89, 0x50, 0x4E, 0x47, 0x0A, 0x30})},
		"java 语法误入": {RelPath: "d.rb", Content: "public static void main(String[] a) { return; }"},
	}
	for name, file := range cases {
		chunks, capability := profile.Split(file)
		if capability != CapabilityFallback {
			t.Fatalf("%s: 应整文件回退, got %v (%d chunks)", name, capability, len(chunks))
		}
	}
	// 超长单行(生成物/minified)不进 AST,走字节窗口降级。
	minified := "class M; def f; " + strings.Repeat("x = 1; ", 2000) + "end; end\n"
	chunks, capability := profile.Split(File{RelPath: "m.rb", Content: minified})
	if capability != CapabilityFallback || len(chunks) == 0 {
		t.Fatalf("超长单行应降级字节窗口: cap=%v n=%d", capability, len(chunks))
	}
}

// 门 5:同文件双跑逐字段一致(确定性)。
func TestRubyDeterminism(t *testing.T) {
	profile := DefaultProfile()
	first, _ := profile.Split(File{RelPath: "billing.rb", Content: rubyFixture})
	oversized := buildOversizedRubyModule()
	firstBig, _ := profile.Split(File{RelPath: "big.rb", Content: oversized})
	for i := 0; i < 3; i++ {
		again, _ := profile.Split(File{RelPath: "billing.rb", Content: rubyFixture})
		if len(first) != len(again) {
			t.Fatalf("切分数漂移: %d vs %d", len(first), len(again))
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("chunk %d 不确定: %+v vs %+v", j, first[j], again[j])
			}
		}
		againBig, _ := profile.Split(File{RelPath: "big.rb", Content: oversized})
		if len(firstBig) != len(againBig) {
			t.Fatalf("超预算 module 切分数漂移: %d vs %d", len(firstBig), len(againBig))
		}
		for j := range firstBig {
			if firstBig[j] != againBig[j] {
				t.Fatalf("超预算 module chunk %d 不确定", j)
			}
		}
	}
}

// buildLargeRubySource 生成 ≥5K 行合成 Ruby module(robustness 基准形态)。
func buildLargeRubySource(methods int) string {
	var b strings.Builder
	b.WriteString("# frozen_string_literal: true\n\nmodule Generated\n  class Worker\n")
	for i := 0; i < methods; i++ {
		fmt.Fprintf(&b, "    # handler_%d 的生成说明\n    def handler_%d(input)\n      acc = input + %d\n      acc = acc * 31 + %d\n      acc\n    end\n\n", i, i, i, i)
	}
	b.WriteString("  end\nend\n")
	return b.String()
}

// 门 6:合成 ≥5K 行 Ruby 大文件切分基准(与 BenchmarkJavaLargeFileSplit
// 同形态)。
func BenchmarkRubyLargeFileSplit(b *testing.B) {
	profile := DefaultProfile()
	src := buildLargeRubySource(750) // 7 行/方法 ≈ 5254 行
	if lines := strings.Count(src, "\n"); lines < 5000 {
		b.Fatalf("基准文件行数不足: %d", lines)
	}
	file := File{RelPath: "generated.rb", Content: src}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, capability := profile.Split(file)
		if capability != CapabilityAST || len(chunks) == 0 {
			b.Fatalf("大文件切分异常: cap=%v n=%d", capability, len(chunks))
		}
	}
}
