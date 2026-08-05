package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 3 的 C++ 声明分类器:C 分类器的超集(declarator 链穿透
// 复用 treesitter_c.go)。新增(zz_astprobe 实测形状):
// - namespace_definition:name+body(declaration_list)——展开为成员序列,
//   namespace 不作符号前缀(与 Java package 语义对齐);类外定义的限定名
//   (ConnPool::acquire)已天然携带归属;
// - class/struct/union_specifier 带 body:容器语义(预算内单 span,超
//   预算拆 header+成员,成员符号 Class.name);
// - template_declaration:包装节点,解包内层声明取符号,span 覆盖模板头;
// - linkage_specification(extern "C"):解包 body 内层声明;
// - alias_declaration(using X = ...):name field,带符号可合并。

// cppTopLevelSpans 对 translation_unit/namespace body 的单个节点分类。
func (p Profile) cppTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	if node.IsError() {
		// ERROR 节点展开子声明(C 同款语义;fmt 实测:C++20 `module;`
		// 等 grammar 未知语法把文件头卷进巨型 ERROR,内部函数/类仍被
		// 解析为子节点)。
		inner := p.walkSiblings(node, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
			return p.cppTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
		})
		if len(inner) == 0 {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		return cCoverRange(inner, start, end), tsKindDecl
	}
	if typ := node.Type(lang); cNodeBroken(node) &&
		typ != "namespace_definition" && typ != "class_specifier" &&
		typ != "struct_specifier" && typ != "union_specifier" &&
		typ != "template_declaration" && typ != "linkage_specification" &&
		typ != "preproc_ifdef" && typ != "preproc_if" && typ != "preproc_else" {
		// 非容器坏节点匿名兜底;容器类节点继续走容器/展开路径(HasError
		// 由后代上传是常态,成员级判据负责隔离毒点)。
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	switch node.Type(lang) {
	case "namespace_definition":
		body := node.ChildByFieldName("body", lang)
		if body == nil {
			body = firstChildOfType(node, lang, "declaration_list")
		}
		if body == nil {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		inner := p.walkSiblings(body, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
			return p.cppTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
		})
		if len(inner) == 0 {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		return cCoverRange(inner, start, end), tsKindDecl
	case "class_specifier", "struct_specifier", "union_specifier":
		if node.ChildByFieldName("body", lang) != nil {
			return p.cppTypeSpans(node, node, lang, src, "", start, end, maxLine), tsKindDecl
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	case "template_declaration":
		if inner := cppTemplateInner(node, lang); inner != nil {
			switch inner.Type(lang) {
			case "class_specifier", "struct_specifier", "union_specifier":
				if inner.ChildByFieldName("body", lang) != nil {
					// outer=模板节点(span 覆盖模板头),类型名取内层。
					return p.cppTypeSpans(node, inner, lang, src, "", start, end, maxLine), tsKindDecl
				}
			case "function_definition":
				return []declSpan{{start: start, end: end, symbol: cDeclaratorName(inner, lang, src), isFunc: true}}, tsKindDecl
			case "declaration":
				if cHasFunctionDeclarator(inner, lang) {
					return []declSpan{{start: start, end: end, symbol: cDeclaratorName(inner, lang, src)}}, tsKindDecl
				}
			case "alias_declaration":
				return []declSpan{{start: start, end: end, symbol: tsNodeName(inner, lang, src)}}, tsKindDecl
			}
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	case "linkage_specification":
		// extern "C" { ... } 或单声明:解包 body 分类。
		if body := node.ChildByFieldName("body", lang); body != nil {
			if body.Type(lang) == "declaration_list" {
				inner := p.walkSiblings(body, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
					return p.cppTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
				})
				if len(inner) == 0 {
					return []declSpan{{start: start, end: end}}, tsKindAnon
				}
				return cCoverRange(inner, start, end), tsKindDecl
			}
			spans, kind := p.cppTopLevelSpans(body, lang, src, start, end, maxLine)
			if kind == tsKindDecl {
				return spans, kind
			}
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	case "alias_declaration":
		return []declSpan{{start: start, end: end, symbol: tsNodeName(node, lang, src)}}, tsKindDecl
	}
	// 其余节点(function_definition/declaration/type_definition/enum/
	// preproc/comment 等)与 C 同构,复用 C 分类器。
	return p.cTopLevelSpans(node, lang, src, start, end, maxLine)
}

// cppTemplateInner 取 template_declaration 的内层声明节点(参数表之后
// 的首个声明类命名子节点)。
func cppTemplateInner(node *gotreesitter.Node, lang *gotreesitter.Language) *gotreesitter.Node {
	for _, child := range namedChildren(node) {
		switch child.Type(lang) {
		case "class_specifier", "struct_specifier", "union_specifier",
			"function_definition", "declaration", "alias_declaration":
			return child
		}
	}
	return nil
}

// cppTypeSpans 按容器处理具名类型:预算内单 span,超预算拆 header+成员
// (Java javaTypeSpans 同构;outer 覆盖模板包装,typeNode 是类型节点)。
func (p Profile) cppTypeSpans(outer *gotreesitter.Node, typeNode *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end, maxLine int) []declSpan {
	symbol := javaQualify(prefix, cppTypeName(typeNode, lang, src))
	whole := classStandalone(declSpan{start: start, end: end, symbol: symbol})
	body := typeNode.ChildByFieldName("body", lang)
	fieldsLost := false
	if body == nil {
		// 坏子树 field 映射丢失(C# 同款实测):按类型兜底定位并强制
		// 成员展开隔离毒点。
		body = firstChildOfType(typeNode, lang, "field_declaration_list")
		fieldsLost = body != nil
	}
	broken := cNodeBroken(typeNode) || fieldsLost
	if !broken && int(outer.EndByte()-outer.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	if body == nil {
		return []declSpan{whole}
	}
	members := p.walkSiblings(body, lang, src, maxLine, func(node *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return p.cppMemberSpans(node, lang, src, symbol, mStart, mEnd, maxLine)
	})
	return assembleContainer(whole, start, end, members)
}

// cppMemberSpans 对类型 body 的单个成员分类:方法定义带 Class.name 且
// 独立;方法原型(含构造/析构原型)带符号可合并;嵌套类型整体单 span;
// access_specifier(public: 行)/字段/友元为匿名可合并。
func (p Profile) cppMemberSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	if cNodeBroken(node) {
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	switch node.Type(lang) {
	case "comment":
		return nil, tsKindComment
	case "function_definition":
		return []declSpan{{start: start, end: end, symbol: javaQualify(outer, cDeclaratorName(node, lang, src)), isFunc: true}}, tsKindDecl
	case "declaration", "field_declaration":
		if cHasFunctionDeclarator(node, lang) {
			return []declSpan{{start: start, end: end, symbol: javaQualify(outer, cDeclaratorName(node, lang, src))}}, tsKindDecl
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	case "class_specifier", "struct_specifier", "union_specifier", "enum_specifier":
		if node.ChildByFieldName("body", lang) != nil {
			return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: javaQualify(outer, tsNodeName(node, lang, src))})}, tsKindDecl
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	case "template_declaration":
		if inner := cppTemplateInner(node, lang); inner != nil && inner.Type(lang) == "function_definition" {
			return []declSpan{{start: start, end: end, symbol: javaQualify(outer, cDeclaratorName(inner, lang, src)), isFunc: true}}, tsKindDecl
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// cppTypeName 取类型名:优先 name field;坏子树 field 丢失时兜底取首个
// type_identifier 子节点原文。
func cppTypeName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	if name := tsNodeName(node, lang, src); name != "" {
		return name
	}
	for _, child := range namedChildren(node) {
		if child.Type(lang) == "type_identifier" {
			startByte, endByte := int(child.StartByte()), int(child.EndByte())
			if startByte >= 0 && endByte <= len(src) && startByte < endByte {
				return src[startByte:endByte]
			}
		}
	}
	return ""
}
