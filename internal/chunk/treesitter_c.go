package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 3 的 C 声明分类器(treesitter.go 的语言分派目标;C++ 在
// treesitter_cpp.go 复用本文件的 declarator 链穿透)。节点形状依
// tree-sitter-c 实测(zz_astprobe):function_definition/declaration 的
// 符号藏在 declarator 链(pointer/parenthesized/function/init/array
// declarator 逐层包裹)内;typedef 符号在 type_definition 的 declarator
// field;struct/enum/union 顶层名在 name field,форward 声明无 body。
// 头文件原型海(redis dict.h 类)是 C 的常态:原型带符号但**不设
// isFunc**——保持可合并,防一行一 chunk 的碎片爆炸;宏(preproc_
// function_def)同理。

// cNodeBroken 是 C/C++ 容错语义的节点级判据:带错节点不提取符号、不进
// 声明语义,由调用方按匿名 span 兜底(splitTreeSitter 的 errorTolerant
// 分派;严格语言在进入分类器前已整文件回退,不会走到这里)。
func cNodeBroken(node *gotreesitter.Node) bool {
	return node.HasError() || node.IsError() || node.IsMissing()
}

// cTopLevelSpans 对 translation_unit 的单个顶层节点分类并产出 span。
func (p Profile) cTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	// 预处理容器(include guard/条件编译)先于坏节点判据展开:HasError
	// 会从任意后代上传到包装节点(redis 实测 include-guard 头文件整体
	// 带错),不展开会把全文件匿名化;展开后由递归对真正的坏子节点逐个
	// 匿名兜底,干净子节点照常声明级切分。
	switch node.Type(lang) {
	case "preproc_ifdef", "preproc_if", "preproc_else":
		inner := p.walkSiblings(node, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
			return p.cTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
		})
		if len(inner) == 0 {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		return cCoverRange(inner, start, end), tsKindDecl
	case "linkage_specification":
		// C 头文件惯用 `#ifdef __cplusplus extern "C" {`:全部声明都在
		// linkage body 里,同样先于坏节点判据展开(HasError 常由结尾
		// 大括号/#endif 交错上传)。
		if body := node.ChildByFieldName("body", lang); body != nil && body.Type(lang) == "declaration_list" {
			inner := p.walkSiblings(body, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
				return p.cTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
			})
			if len(inner) > 0 {
				return cCoverRange(inner, start, end), tsKindDecl
			}
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	if node.IsError() {
		// ERROR 节点内部仍有已解析的子声明(C 头文件惯用的
		// `#ifdef __cplusplus extern "C" {` 不配平大括号即此形态,
		// redis geohash.h 实测):展开子节点逐个分类,可识别的声明照常
		// 提取,残渣保持匿名;整体空产出时按匿名 span 兜底。
		inner := p.walkSiblings(node, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
			return p.cTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
		})
		if len(inner) == 0 {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		return cCoverRange(inner, start, end), tsKindDecl
	}
	if cNodeBroken(node) {
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	switch node.Type(lang) {
	case "comment":
		return nil, tsKindComment
	case "function_definition":
		return []declSpan{{start: start, end: end, symbol: cDeclaratorName(node, lang, src), isFunc: true}}, tsKindDecl
	case "type_definition":
		return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: cDeclaratorName(node, lang, src)})}, tsKindDecl
	case "struct_specifier", "enum_specifier", "union_specifier":
		// 带 body 的具名类型是独立声明;前向声明(无 body)按匿名合并。
		if node.ChildByFieldName("body", lang) != nil {
			return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: tsNodeName(node, lang, src)})}, tsKindDecl
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	case "declaration":
		// 函数原型:declarator 链内含 function_declarator。带符号但可
		// 合并(见文件头);其余(全局变量/extern 数据)匿名。
		if cHasFunctionDeclarator(node, lang) {
			return []declSpan{{start: start, end: end, symbol: cDeclaratorName(node, lang, src)}}, tsKindDecl
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	case "preproc_function_def":
		return []declSpan{{start: start, end: end, symbol: tsNodeName(node, lang, src)}}, tsKindDecl
	}
	// preproc_include/preproc_def/expression_statement/; 等:匿名合并。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// cCoverRange 把展开的内部 span 序列补齐到 [start,end] 全覆盖(首部
// 守卫行并入首 span,尾部 #endif 行并入尾 span),防覆盖缝隙。
func cCoverRange(spans []declSpan, start int, end int) []declSpan {
	if spans[0].start > start {
		spans[0].start = start
	}
	if last := &spans[len(spans)-1]; last.end < end {
		last.end = end
	}
	return spans
}

// cDeclaratorName 沿 declarator field 链穿透包装,返回最内层标识符原文;
// qualified_identifier(C++ 类外定义 ConnPool::acquire)、operator_name、
// destructor_name 取整段原文。
func cDeclaratorName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	for node != nil {
		switch node.Type(lang) {
		case "identifier", "type_identifier", "field_identifier",
			"qualified_identifier", "operator_name", "destructor_name":
			startByte, endByte := int(node.StartByte()), int(node.EndByte())
			if startByte >= 0 && endByte <= len(src) && startByte < endByte {
				return src[startByte:endByte]
			}
			return ""
		case "parenthesized_declarator":
			// (*cmp_fn) 形态:field 缺失,取首个命名子节点继续穿透。
			children := namedChildren(node)
			if len(children) == 0 {
				return ""
			}
			node = children[0]
		default:
			node = node.ChildByFieldName("declarator", lang)
		}
	}
	return ""
}

// cHasFunctionDeclarator 判断 declaration 的 declarator 链内是否有
// function_declarator(原型判定)。
func cHasFunctionDeclarator(node *gotreesitter.Node, lang *gotreesitter.Language) bool {
	seen := 0
	for node != nil && seen < 8 {
		if node.Type(lang) == "function_declarator" {
			return true
		}
		next := node.ChildByFieldName("declarator", lang)
		if next == nil && node.Type(lang) == "parenthesized_declarator" {
			children := namedChildren(node)
			if len(children) > 0 {
				next = children[0]
			}
		}
		node = next
		seen++
	}
	return false
}
