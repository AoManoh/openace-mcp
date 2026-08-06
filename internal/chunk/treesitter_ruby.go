package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 4 的 Ruby 声明分类器(treesitter.go 的语言分派目标)。
// 声明级语义与 Java/C# 容器语言对齐:module/class 按容器处理(预算内
// 单 span,超预算拆 header + 成员,符号 javaQualify 点连接,嵌套归属如
// Billing.Invoice.total_cents);method/singleton_method 是函数级声明
// (isFunc,独立 chunk);require/DSL 调用/常量赋值等其余顶层语句按匿名
// 可合并 span。ruby 走严格语言语义(splitTreeSitter errorTolerant=false,
// 树带错即整文件回退),分类器可假设树干净,无需 C 族坏节点容错。
// 节点形状经探针实测(gotreesitter v0.47.0 内嵌 ruby grammar):
// - program 顶层:comment/call(require 与带 do_block 的 DSL 调用同为
//   call)/assignment/module/class/method/singleton_method/if/begin 等;
// - module/class 带 name field;成员包裹在 body_statement 子节点之下;
//   但"声明行与首成员之间"的注释被 grammar 提升为容器的直接子节点
//   (body_statement 之外),故成员展开游走容器自身而非 body_statement,
//   否则首成员的 doc 注释会漏出附着序列;
// - method/singleton_method 的 name field 对 operator 方法(==、[])同样
//   有效;singleton_method 另有 object field(self 或常量);
// - private/public 裸写解析为 identifier,attr_reader/private :sym 是
//   call,常量赋值是 assignment——全部匿名处理;
// - class << self 是 singleton_class 节点,无 name field,body 仍是
//   body_statement。

// rubyTopLevelSpans 对 program 的单个顶层节点分类并产出 span
// (collectTopLevelSpans 的分派目标);顶层无归属前缀。
func (p Profile) rubyTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	return p.rubyNodeSpans(node, lang, src, "", start, end, maxLine)
}

// rubyNodeSpans 是顶层与容器成员共用的节点分类:Ruby 的顶层语句集与
// 容器 body 语句集完全同构(module 里可再写 module/class/def/DSL 调用),
// 仅归属前缀不同,合并实现避免两套分支漂移。
func (p Profile) rubyNodeSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	switch node.Type(lang) {
	case "comment":
		// 行注释与 =begin/=end 块注释同为 comment 节点:进附着序列,
		// 紧邻声明时并入其 span(doc 注释语义)。
		return nil, tsKindComment
	case "module", "class":
		return p.rubyContainerSpans(node, lang, src, prefix, start, end, maxLine), tsKindDecl
	case "method":
		// def x/def ==(other):name field 对 identifier 与 operator 都
		// 有效;isFunc 保证方法符号独立成 chunk(exact-symbol 承诺)。
		return []declSpan{{start: start, end: end, symbol: javaQualify(prefix, tsNodeName(node, lang, src)), isFunc: true}}, tsKindDecl
	case "singleton_method":
		return []declSpan{{start: start, end: end, symbol: rubySingletonSymbol(node, lang, src, prefix), isFunc: true}}, tsKindDecl
	case "singleton_class":
		return p.rubySingletonClassSpans(node, lang, src, prefix, start, end, maxLine)
	}
	// call(require/attr_reader/带 do_block 的 DSL)/assignment(常量)/
	// identifier(裸 private/public)/if/begin/alias 等:匿名可合并 span。
	// do_block 内的 def 不展开:块内方法定义的宿主由运行期决定(如
	// Struct.new、class_eval),静态归属会产出误导符号,整块匿名更诚实。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// rubyContainerSpans 按容器处理 module/class(csharpTypeSpans 同构):
// 预算内保持单 span(classStandalone:容器与函数同为符号检索的独立
// 单元);超预算拆 header + 成员。与 Java/C# 的"嵌套类型不递归"不同,
// 成员级嵌套容器继续走本函数递归展开:Ruby 惯用 module 纯作命名空间
// (module Foo; class Bar 是 gem 标准形态),文件主体几乎总在嵌套容器
// 内,不递归会把超预算的实体类整体丢给 splitOversized 按行盲切,系统
// 性丢失方法符号。
func (p Profile) rubyContainerSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end, maxLine int) []declSpan {
	symbol := javaQualify(prefix, tsNodeName(node, lang, src))
	whole := classStandalone(declSpan{start: start, end: end, symbol: symbol})
	if int(node.EndByte()-node.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	members := p.rubyMemberWalk(node, lang, src, symbol, maxLine)
	// 空容器/单行内联 body(members 为空或首成员与声明同行)由
	// assembleContainer 退化为整容器单 span,超预算兜底走 splitOversized。
	return assembleContainer(whole, start, end, members)
}

// rubyMemberWalk 游走容器(module/class/singleton_class)自身的命名子
// 节点并展开 body_statement 产出成员 span 序列。不直接游走 body_statement
// 的原因(实测):容器声明行与首成员之间的注释挂在容器直下、位于
// body_statement 之外,只有把它与 body_statement 的展开序列放进同一次
// walkSiblings,注释才能按紧邻规则附着到首成员 span。
func (p Profile) rubyMemberWalk(container *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string, maxLine int) []declSpan {
	return p.walkSiblings(container, lang, src, maxLine, func(node *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		switch node.Type(lang) {
		case "comment":
			return nil, tsKindComment
		case "body_statement":
			// 成员包裹节点:展开为成员序列后作为整体 decl 返回,使容器
			// 直下注释可附着展开序列的首 span。首尾缝隙补齐(Java
			// enum_body_declarations 同款),防行覆盖漏洞(门 2)。
			inner := p.walkSiblings(node, lang, src, maxLine, func(member *gotreesitter.Node, s, e int) ([]declSpan, tsNodeKind) {
				return p.rubyNodeSpans(member, lang, src, outer, s, e, maxLine)
			})
			if len(inner) == 0 {
				return []declSpan{{start: mStart, end: mEnd}}, tsKindAnon
			}
			if inner[0].start > mStart {
				inner = append([]declSpan{{start: mStart, end: inner[0].start - 1}}, inner...)
			}
			if last := &inner[len(inner)-1]; last.end < mEnd {
				last.end = mEnd
			}
			return inner, tsKindDecl
		}
		// constant(容器名)/superclass/self:声明行的组成部分,行区间由
		// assembleContainer 的 header(或 singleton_class 的首行并入)覆盖,
		// 不单独产 span。
		return nil, tsKindSkip
	})
}

// rubySingletonClassSpans 处理 class << self 块:节点无 name field,按
// "无名容器"始终展开成员——块内 def 就是外层容器的类方法(Ruby 语义),
// 符号直接用外层前缀限定(Big 内 def registry → Big.registry,与调用
// 形态一致),符号价值高于块的整体性,故不设预算门槛。首行(class <<
// self)并入首成员而非补匿名 span:它只是成员的语法开场,单独成 span
// 会在两个 isFunc 邻居之间留下不可合并的孤行 chunk。
func (p Profile) rubySingletonClassSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	inner := p.rubyMemberWalk(node, lang, src, prefix, maxLine)
	if len(inner) == 0 {
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	if inner[0].start > start {
		inner[0].start = start
	}
	if last := &inner[len(inner)-1]; last.end < end {
		last.end = end
	}
	return inner, tsKindDecl
}

// rubySingletonSymbol 取 singleton_method(def X.name)的归属符号:
// object 为常量/常量路径时归属该对象(def Money.mint → Money.mint,
// def Foo::Bar.baz → Foo::Bar.baz——定义的是该常量的类方法,与所在
// 容器无关);object 为 self(惯用 def self.x)时归属外层容器。
func rubySingletonSymbol(node *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string) string {
	name := tsNodeName(node, lang, src)
	if obj := node.ChildByFieldName("object", lang); obj != nil {
		switch obj.Type(lang) {
		case "constant", "scope_resolution":
			return javaQualify(rubyNodeText(obj, src), name)
		}
	}
	return javaQualify(outer, name)
}

// rubyNodeText 取节点原文;边界检查与 tsNodeName 对齐(L15):越界虽被
// splitTreeSitter 的 recover 兜为语言级 fallback,防御仍应在切片处。
func rubyNodeText(node *gotreesitter.Node, src string) string {
	startByte, endByte := int(node.StartByte()), int(node.EndByte())
	if startByte >= 0 && endByte <= len(src) && startByte < endByte {
		return src[startByte:endByte]
	}
	return ""
}
