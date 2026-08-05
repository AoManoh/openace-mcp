package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 3 的 C# 声明分类器:与 Java 高度同构(zz_astprobe 实测:
// 类型声明均带 name+body(declaration_list);attribute_list 是声明节点的
// 子节点,天然包含在 span 内)。差异:file-scoped namespace(namespace X;)
// 无 body、声明是其兄弟——按匿名序言处理;块式 namespace 展开为成员
// 序列且不作符号前缀(Java package 语义);positional record 可无 body。

// csharpTypeDecls 是 C# 的类型声明节点集合。
var csharpTypeDecls = map[string]bool{
	"class_declaration":     true,
	"interface_declaration": true,
	"struct_declaration":    true,
	"enum_declaration":      true,
	"record_declaration":    true,
}

// csharpTopLevelSpans 对 compilation_unit/namespace body 的单个节点分类。
func (p Profile) csharpTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	if cNodeBroken(node) && !csharpTypeDecls[typ] && typ != "namespace_declaration" {
		// 非容器坏节点按匿名兜底;容器(类型/namespace)继续走容器路径
		// ——name field 通常完好,坏成员由成员级判据隔离(grammar 的
		// 嵌套泛型 `>>` 误报,见 splitTreeSitter 注)。
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	switch {
	case typ == "comment":
		return nil, tsKindComment
	case csharpTypeDecls[typ]:
		return p.csharpTypeSpans(node, lang, src, "", start, end, maxLine), tsKindDecl
	case typ == "namespace_declaration":
		body := node.ChildByFieldName("body", lang)
		if body == nil {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		inner := p.walkSiblings(body, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
			return p.csharpTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
		})
		if len(inner) == 0 {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		return cCoverRange(inner, start, end), tsKindDecl
	case typ == "delegate_declaration":
		return []declSpan{{start: start, end: end, symbol: tsNodeName(node, lang, src)}}, tsKindDecl
	}
	// using_directive/file_scoped_namespace_declaration/global_statement
	// 等:匿名序言合并。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// csharpTypeSpans 按容器处理类型声明(javaTypeSpans 同构)。带错容器
// 即使在预算内也强制成员展开:把 grammar 误报的毒点隔离在单个成员
// span 内,类名与干净成员符号全部保留。
func (p Profile) csharpTypeSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end, maxLine int) []declSpan {
	symbol := javaQualify(prefix, csharpTypeName(node, lang, src))
	whole := classStandalone(declSpan{start: start, end: end, symbol: symbol})
	body := node.ChildByFieldName("body", lang)
	fieldsLost := false
	if body == nil {
		// 坏子树的 field 映射会丢失(实测:含 `>>` 误报的类,name/body
		// field 全部无标注,而 HasError 传播还不可靠)——field 丢失本身
		// 即坏标记,按节点类型兜底定位 body 并强制成员展开隔离毒点。
		body = firstChildOfType(node, lang, "declaration_list", "enum_member_declaration_list")
		fieldsLost = body != nil
	}
	broken := cNodeBroken(node) || fieldsLost
	if !broken && int(node.EndByte()-node.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	if body == nil {
		return []declSpan{whole}
	}
	members := p.walkSiblings(body, lang, src, maxLine, func(member *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return p.csharpMemberSpans(member, lang, src, symbol, mStart, mEnd, maxLine)
	})
	return assembleContainer(whole, start, end, members)
}

// csharpMemberSpans 对类型 body 的单个成员分类:方法/构造/析构/操作符
// 带 Class.name 且独立;属性/索引器/事件带符号可合并;嵌套类型整体单
// span;字段/常量为匿名可合并。
func (p Profile) csharpMemberSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	if cNodeBroken(node) && !csharpTypeDecls[typ] {
		// 坏成员匿名兜底(毒点隔离);嵌套类型继续容器路径。
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	switch {
	case typ == "comment":
		return nil, tsKindComment
	case typ == "method_declaration" || typ == "constructor_declaration" ||
		typ == "destructor_declaration" || typ == "operator_declaration" ||
		typ == "conversion_operator_declaration":
		return []declSpan{{start: start, end: end, symbol: javaQualify(outer, csharpMemberName(node, lang, src)), isFunc: true}}, tsKindDecl
	case typ == "property_declaration" || typ == "indexer_declaration" || typ == "event_declaration" || typ == "event_field_declaration":
		return []declSpan{{start: start, end: end, symbol: javaQualify(outer, tsNodeName(node, lang, src))}}, tsKindDecl
	case csharpTypeDecls[typ]:
		return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: javaQualify(outer, tsNodeName(node, lang, src))})}, tsKindDecl
	}
	// field_declaration 等:匿名合并。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// csharpTypeName 取类型声明名:优先 name field;带错节点 field 映射
// 丢失时兜底取首个 identifier 子节点原文。
func csharpTypeName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	if name := tsNodeName(node, lang, src); name != "" {
		return name
	}
	for _, child := range namedChildren(node) {
		if child.Type(lang) == "identifier" {
			startByte, endByte := int(child.StartByte()), int(child.EndByte())
			if startByte >= 0 && endByte <= len(src) && startByte < endByte {
				return src[startByte:endByte]
			}
		}
	}
	return ""
}

// csharpMemberName 取成员声明名:优先 name field;带错子树 field 映射
// 丢失时兜底取 parameter_list 前最后一个 identifier(方法名总是紧邻
// 参数表,返回类型即使是 identifier 也会被其后的方法名覆盖)。
func csharpMemberName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	if name := tsNodeName(node, lang, src); name != "" {
		return name
	}
	last := ""
	for _, child := range namedChildren(node) {
		typ := child.Type(lang)
		if typ == "parameter_list" {
			break
		}
		if typ == "identifier" {
			startByte, endByte := int(child.StartByte()), int(child.EndByte())
			if startByte >= 0 && endByte <= len(src) && startByte < endByte {
				last = src[startByte:endByte]
			}
		}
	}
	return last
}
