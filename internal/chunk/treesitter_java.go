package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 2 的 Java 声明分类器（treesitter.go 的语言分派目标）。
// 声明级语义：package/import 序言按匿名 span 相邻合并；class/interface/
// enum/record/annotation 五种类型声明按容器处理（预算内单 span，超预算
// 拆 header + 成员，成员符号带 Outer.name 前缀——批次 1 类成员语义）；
// 注解与 javadoc：注解在 grammar 中位于声明节点内部（modifiers 子节点），
// 无需解包；javadoc（block_comment/line_comment）走 walkSiblings 的注释
// 附着规则。节点形状依 tree-sitter-java（languages.lock java@e10607b45ff7）
// 实测：program 顶层为 package_declaration/import_declaration/类型声明，
// 类型声明均带 name 与 body field。

// javaTypeDecls 是 Java 的五种顶层/嵌套类型声明节点。
var javaTypeDecls = map[string]bool{
	"class_declaration":           true,
	"interface_declaration":       true,
	"enum_declaration":            true,
	"record_declaration":          true,
	"annotation_type_declaration": true,
}

// javaTopLevelSpans 对 program 的单个顶层节点分类并产出 span。
func (p Profile) javaTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	if typ == "line_comment" || typ == "block_comment" {
		return nil, tsKindComment
	}
	if javaTypeDecls[typ] {
		return p.javaTypeSpans(node, lang, src, "", start, end, maxLine), tsKindDecl
	}
	// package_declaration/import_declaration/module_declaration 等：匿名
	// 可合并 span（序言合并语义，mergeSmallSpans 按预算归并）。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// javaTypeSpans 按容器处理类型声明：预算内保持单 span；超预算时拆
// header + 成员（assembleContainer 语义与批次 1 classSpans 一致）。
// prefix 是嵌套归属前缀（顶层为空），符号形如 Outer.Inner.method。
func (p Profile) javaTypeSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end, maxLine int) []declSpan {
	symbol := javaQualify(prefix, tsNodeName(node, lang, src))
	whole := classStandalone(declSpan{start: start, end: end, symbol: symbol})
	if int(node.EndByte()-node.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	body := node.ChildByFieldName("body", lang)
	if body == nil {
		return []declSpan{whole}
	}
	members := p.javaMemberWalk(body, lang, src, symbol, maxLine)
	return assembleContainer(whole, start, end, members)
}

// javaMemberWalk 游走类型 body（class_body/interface_body/enum_body/
// annotation_type_body）产出成员 span 序列。
func (p Profile) javaMemberWalk(body *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string, maxLine int) []declSpan {
	return p.walkSiblings(body, lang, src, maxLine, func(node *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return p.javaMemberSpans(node, lang, src, outer, mStart, mEnd, maxLine)
	})
}

// javaMemberSpans 对类型 body 的单个成员分类：方法/构造器带 Outer.name
// 符号且独立成 chunk；嵌套类型整体单 span（不递归拆分，批次 1 嵌套类
// 同款，超预算由 splitOversized 兜底）；字段/静态块/枚举常量为匿名可
// 合并 span（前置者由 assembleContainer 并入 header）。
func (p Profile) javaMemberSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	switch {
	case typ == "line_comment" || typ == "block_comment":
		return nil, tsKindComment
	case typ == "method_declaration" || typ == "constructor_declaration" || typ == "compact_constructor_declaration":
		return []declSpan{{start: start, end: end, symbol: javaQualify(outer, tsNodeName(node, lang, src)), isFunc: true}}, tsKindDecl
	case javaTypeDecls[typ]:
		return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: javaQualify(outer, tsNodeName(node, lang, src))})}, tsKindDecl
	case typ == "enum_body_declarations":
		// 枚举常量后的成员区（`;` 起始的包装节点）：展开为成员序列。
		// 包装自身的前导/尾随行（分号行等）补为匿名 span，防覆盖缝隙。
		inner := p.javaMemberWalk(node, lang, src, outer, maxLine)
		if len(inner) == 0 {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		if inner[0].start > start {
			inner = append([]declSpan{{start: start, end: inner[0].start - 1}}, inner...)
		}
		if last := &inner[len(inner)-1]; last.end < end {
			last.end = end
		}
		return inner, tsKindDecl
	}
	// field_declaration/static_initializer/enum_constant/; 等：匿名合并。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// javaQualify 组装嵌套归属符号（Outer.name）；任一侧为空时取另一侧。
func javaQualify(outer string, name string) string {
	if outer != "" && name != "" {
		return outer + "." + name
	}
	if name != "" {
		return name
	}
	return outer
}
