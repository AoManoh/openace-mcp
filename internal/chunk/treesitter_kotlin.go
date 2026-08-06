package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 4 的 Kotlin 声明分类器(astprobe4 实测,2026-08-07):
// class/data class/interface/enum/annotation class 统一是
// class_declaration(body 为 class_body 或 enum_class_body),object 单例
// 是 object_declaration,与 Java/C# 的容器形态同构。关键差异:该 grammar
// 的 name/body field 映射全部为空——名字取 type_identifier(类型)或
// simple_identifier(函数/属性)子节点原文,body 按节点类型定位。Kotlin
// 按严格语义处理:树带错即整文件回退,分类器可假设树干净。

// kotlinContainerDecls 是 Kotlin 的容器声明节点集合。companion_object
// 只出现在成员位,顶层集合不含。
var kotlinContainerDecls = map[string]bool{
	"class_declaration":  true,
	"object_declaration": true,
}

// kotlinTopLevelSpans 对 source_file 的单个顶层节点分类。kotlin 走容错
// 语义(grammar 对软关键字 `yield` 作参数名等合法源误报 ERROR):坏的
// 非容器节点匿名兜底(内容原样保留、不提符号,防半坏边界);容器继续
// 容器路径,坏成员由成员级判据隔离。
func (p Profile) kotlinTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	if cNodeBroken(node) && !kotlinContainerDecls[typ] {
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	switch {
	case typ == "line_comment" || typ == "multiline_comment":
		return nil, tsKindComment
	case kotlinContainerDecls[typ]:
		return p.kotlinTypeSpans(node, lang, src, "", start, end, maxLine), tsKindDecl
	case typ == "function_declaration":
		return []declSpan{{start: start, end: end, symbol: kotlinDeclName(node, lang, src), isFunc: true}}, tsKindDecl
	case typ == "type_alias":
		return []declSpan{{start: start, end: end, symbol: kotlinDeclName(node, lang, src)}}, tsKindDecl
	}
	// package_header/import_list/property_declaration(顶层 val/const)/
	// 脚本语句等:匿名序言合并。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// kotlinTypeSpans 按容器处理类型声明(csharpTypeSpans 同构):整体在
// 预算内单 span,超预算按 body 成员展开,类名与成员符号全部保留。
func (p Profile) kotlinTypeSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end, maxLine int) []declSpan {
	symbol := javaQualify(prefix, kotlinTypeName(node, lang, src))
	whole := classStandalone(declSpan{start: start, end: end, symbol: symbol})
	if int(node.EndByte()-node.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	// name/body field 映射为空(实测),按节点类型定位 body。
	body := firstChildOfType(node, lang, "class_body", "enum_class_body")
	if body == nil {
		return []declSpan{whole}
	}
	members := p.walkSiblings(body, lang, src, maxLine, func(member *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return p.kotlinMemberSpans(member, lang, src, symbol, mStart, mEnd, maxLine)
	})
	return assembleContainer(whole, start, end, members)
}

// kotlinMemberSpans 对容器 body 的单个成员分类:方法/次级构造器带
// Class.name 且独立;companion 与嵌套类型按容器递归;属性/init 块/
// enum entry 为匿名可合并。
func (p Profile) kotlinMemberSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	if cNodeBroken(node) && !kotlinContainerDecls[typ] && typ != "companion_object" {
		// 坏成员匿名兜底(毒点隔离);容器继续容器路径。
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	switch {
	case typ == "line_comment" || typ == "multiline_comment":
		return nil, tsKindComment
	case typ == "function_declaration":
		return []declSpan{{start: start, end: end, symbol: javaQualify(outer, kotlinDeclName(node, lang, src)), isFunc: true}}, tsKindDecl
	case typ == "secondary_constructor":
		return []declSpan{{start: start, end: end, symbol: javaQualify(outer, "constructor"), isFunc: true}}, tsKindDecl
	case typ == "companion_object":
		// 未命名 companion 以惯用名 Companion 入符号(Kotlin 语言语义:
		// 引用形态即 Outer.Companion)。
		name := kotlinTypeName(node, lang, src)
		if name == "" {
			name = "Companion"
		}
		return p.kotlinNestedSpans(node, lang, src, javaQualify(outer, name), start, end, maxLine), tsKindDecl
	case kotlinContainerDecls[typ]:
		return p.kotlinNestedSpans(node, lang, src, javaQualify(outer, kotlinTypeName(node, lang, src)), start, end, maxLine), tsKindDecl
	}
	// property_declaration/anonymous_initializer/enum_entry 等:匿名合并。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// kotlinNestedSpans 处理成员位的嵌套容器:预算内整体单 span(独立不与
// 邻居合并),超预算继续按成员展开(companion 内工厂方法是高频检索面,
// 与 C# 嵌套类型的"整体单 span"相比多走一层,成本只在超预算路径)。
func (p Profile) kotlinNestedSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, symbol string, start, end, maxLine int) []declSpan {
	whole := classStandalone(declSpan{start: start, end: end, symbol: symbol})
	if int(node.EndByte()-node.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	body := firstChildOfType(node, lang, "class_body", "enum_class_body")
	if body == nil {
		return []declSpan{whole}
	}
	members := p.walkSiblings(body, lang, src, maxLine, func(member *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return p.kotlinMemberSpans(member, lang, src, symbol, mStart, mEnd, maxLine)
	})
	return assembleContainer(whole, start, end, members)
}

// kotlinTypeName 取类型/object/companion 名:首个 type_identifier 子节点
// 原文(name field 映射为空,实测)。
func kotlinTypeName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	if name := tsNodeName(node, lang, src); name != "" {
		return name
	}
	return kotlinChildText(node, lang, src, "type_identifier")
}

// kotlinDeclName 取函数/typealias/属性名:优先 type_identifier(typealias),
// 回退 simple_identifier(函数名;扩展函数的 receiver_type 不含
// simple_identifier 直接子节点,首个 simple_identifier 即函数名)。
func kotlinDeclName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	if name := tsNodeName(node, lang, src); name != "" {
		return name
	}
	if node.Type(lang) == "type_alias" {
		if name := kotlinChildText(node, lang, src, "type_identifier"); name != "" {
			return name
		}
	}
	return kotlinChildText(node, lang, src, "simple_identifier")
}

// kotlinChildText 取首个指定类型直接子节点的原文。
func kotlinChildText(node *gotreesitter.Node, lang *gotreesitter.Language, src string, childType string) string {
	for _, child := range namedChildren(node) {
		if child.Type(lang) != childType {
			continue
		}
		startByte, endByte := int(child.StartByte()), int(child.EndByte())
		if startByte >= 0 && endByte <= len(src) && startByte < endByte {
			return src[startByte:endByte]
		}
	}
	return ""
}
