package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 2 的 Rust 项分类器（treesitter.go 的语言分派目标）。
// 声明级语义：use/extern crate 序言按匿名 span 相邻合并；fn 独立成
// chunk；struct/enum/union/type/const/static/macro_rules 为带符号声明
// （小声明允许相邻合并，与批次 1 TS interface/type_alias 同款）；
// impl/trait/inline mod 按容器处理（预算内单 span，超预算拆 header +
// 成员）。成员符号风格与批次 1 的 Class.method 对应：impl 内 fn 归属
// 实现类型（Type::name，Rust 路径语义），trait 内 fn 为 Trait::name，
// mod 内项为 mod::name；trait impl 容器符号为 "Trait for Type"。
// 属性（#[derive] 等）在 grammar 中是声明的兄弟节点（attribute_item/
// inner_attribute_item），按注释附着规则并入后继声明的 span。节点形状
// 依 tree-sitter-rust（languages.lock rust@77a3747266f4）实测。

// rustNamedItems 是携带 name field 的非容器顶层项：符号可检索，小项
// 允许相邻合并（不设 isFunc）。
var rustNamedItems = map[string]bool{
	"struct_item":      true,
	"enum_item":        true,
	"union_item":       true,
	"type_item":        true,
	"const_item":       true,
	"static_item":      true,
	"macro_definition": true,
}

// rustTopLevelSpans 对 source_file 的单个顶层节点分类并产出 span。
func (p Profile) rustTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	switch {
	case typ == "line_comment" || typ == "block_comment" || typ == "attribute_item" || typ == "inner_attribute_item":
		// 属性与注释同语义：紧邻声明起始行的连续块并入该声明。
		return nil, tsKindComment
	case typ == "function_item" || typ == "function_signature_item":
		return []declSpan{{start: start, end: end, symbol: tsNodeName(node, lang, src), isFunc: true}}, tsKindDecl
	case rustNamedItems[typ]:
		return []declSpan{{start: start, end: end, symbol: tsNodeName(node, lang, src)}}, tsKindDecl
	case typ == "trait_item":
		name := tsNodeName(node, lang, src)
		return p.rustContainerSpans(node, lang, src, name, name, start, end, maxLine), tsKindDecl
	case typ == "impl_item":
		wholeSymbol, memberPrefix := rustImplSymbols(node, lang, src)
		return p.rustContainerSpans(node, lang, src, wholeSymbol, memberPrefix, start, end, maxLine), tsKindDecl
	case typ == "mod_item":
		name := tsNodeName(node, lang, src)
		if node.ChildByFieldName("body", lang) == nil {
			// `mod name;` 声明无 body：小声明合并语义。
			return []declSpan{{start: start, end: end, symbol: name}}, tsKindDecl
		}
		return p.rustContainerSpans(node, lang, src, name, name, start, end, maxLine), tsKindDecl
	}
	// use_declaration/extern_crate_declaration/macro_invocation/
	// foreign_mod_item/expression_statement 等：匿名可合并 span。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// rustContainerSpans 按容器处理 impl/trait/inline mod：预算内保持单
// span；超预算拆 header + 成员（assembleContainer 语义与批次 1 一致）。
func (p Profile) rustContainerSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, wholeSymbol string, memberPrefix string, start, end, maxLine int) []declSpan {
	whole := classStandalone(declSpan{start: start, end: end, symbol: wholeSymbol})
	if int(node.EndByte()-node.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	body := node.ChildByFieldName("body", lang)
	if body == nil {
		return []declSpan{whole}
	}
	members := p.walkSiblings(body, lang, src, maxLine, func(child *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return p.rustMemberSpans(child, lang, src, memberPrefix, mStart, mEnd)
	})
	return assembleContainer(whole, start, end, members)
}

// rustNestedTypeItems 是容器内按嵌套类型对待的成员：整体单 span（不
// 递归拆分，批次 1 嵌套类同款），符号带 prefix:: 归属。
var rustNestedTypeItems = map[string]bool{
	"struct_item":      true,
	"enum_item":        true,
	"union_item":       true,
	"trait_item":       true,
	"macro_definition": true,
	"mod_item":         true,
}

// rustMemberSpans 对容器 body（declaration_list）的单个成员分类：fn 带
// prefix::name 符号且独立成 chunk；嵌套类型项整体单 span（不递归拆分，
// 批次 1 嵌套类同款）；嵌套 impl 不递归，取自身符号（实现类型已自我
// 描述，不叠加 mod 前缀）；关联常量/关联类型/use 等为匿名可合并 span
// （与 Java 字段/静态块同语义，前置者由 assembleContainer 并入 header）。
func (p Profile) rustMemberSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, prefix string, start, end int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	switch {
	case typ == "line_comment" || typ == "block_comment" || typ == "attribute_item" || typ == "inner_attribute_item":
		return nil, tsKindComment
	case typ == "function_item" || typ == "function_signature_item":
		return []declSpan{{start: start, end: end, symbol: rustQualify(prefix, tsNodeName(node, lang, src)), isFunc: true}}, tsKindDecl
	case rustNestedTypeItems[typ]:
		return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: rustQualify(prefix, tsNodeName(node, lang, src))})}, tsKindDecl
	case typ == "impl_item":
		wholeSymbol, _ := rustImplSymbols(node, lang, src)
		return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: wholeSymbol})}, tsKindDecl
	}
	// const_item/static_item/type_item（关联项）/use_declaration 等：匿名合并。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// rustImplSymbols 提取 impl 块的容器符号与成员前缀：inherent impl 为
// 基础类型名（Point<T> → Point）；trait impl 为 "Trait for Type"，成员
// 仍归属实现类型（Type::method，与调用点路径一致）。
func rustImplSymbols(node *gotreesitter.Node, lang *gotreesitter.Language, src string) (string, string) {
	typeName := rustTypeName(node.ChildByFieldName("type", lang), lang, src)
	traitName := rustTypeName(node.ChildByFieldName("trait", lang), lang, src)
	switch {
	case traitName != "" && typeName != "":
		return traitName + " for " + typeName, typeName
	case typeName != "":
		return typeName, typeName
	default:
		return traitName, typeName
	}
}

// rustTypeName 解析类型节点的基础名：剥离泛型实参（generic_type）、路径
// 限定（scoped_type_identifier）、引用/指针与 dyn 包装；未知形状（元组、
// slice 等罕见 impl 目标）返回空符号，不产出噪声。
func rustTypeName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	for node != nil {
		switch node.Type(lang) {
		case "type_identifier", "primitive_type":
			startByte, endByte := int(node.StartByte()), int(node.EndByte())
			if startByte >= 0 && endByte <= len(src) && startByte < endByte {
				return src[startByte:endByte]
			}
			return ""
		case "generic_type", "reference_type", "pointer_type":
			node = node.ChildByFieldName("type", lang)
		case "scoped_type_identifier":
			node = node.ChildByFieldName("name", lang)
		case "dynamic_type":
			node = node.ChildByFieldName("trait", lang)
		default:
			return ""
		}
	}
	return ""
}

// rustQualify 组装成员归属符号（prefix::name）；任一侧为空时取另一侧。
func rustQualify(prefix string, name string) string {
	if prefix != "" && name != "" {
		return prefix + "::" + name
	}
	if name != "" {
		return name
	}
	return prefix
}

// rustMaskFrontmatter 对 cargo-script frontmatter（RFC 3503：可选 shebang
// 后以 ---infostring 开栅栏、以同 dash 数的 --- 闭栅栏包裹 TOML）做等长
// 空白掩码：pinned grammar 不识别该语法，掩码后 Rust 主体可正常解析。
// 返回（可能拷贝的）解析字节与闭栅栏行号（0=未检测到）。保守规则：
//   - 栅栏必须顶格（缩进即非法，与 cargo 一致，不掩码）；
//   - infostring 只接受空串或 [A-Za-z][A-Za-z0-9._-]*（逗号/前导点等
//     cargo 报错形态不掩码，保持 honest fallback）；
//   - 闭栅栏必须是恰好同 dash 数且无尾随内容的行；找不到即不掩码
//     （unclosed/mismatch 夹具整文件回退）；
//   - 只把栅栏区间内非换行字节改为空格，几何逐字节保持。
func rustMaskFrontmatter(src []byte) ([]byte, int) {
	offset := 0
	line := 1
	nextLine := func() ([]byte, bool) {
		if offset >= len(src) {
			return nil, false
		}
		end := offset
		for end < len(src) && src[end] != '\n' {
			end++
		}
		return src[offset:end], true
	}
	advance := func(lineBytes []byte) {
		offset += len(lineBytes)
		if offset < len(src) && src[offset] == '\n' {
			offset++
		}
		line++
	}
	// 可选 shebang（#! 且非 #![，后者是 inner attribute，grammar 原生支持）。
	if first, ok := nextLine(); ok && len(first) >= 2 && first[0] == '#' && first[1] == '!' && !(len(first) >= 3 && first[2] == '[') {
		advance(first)
	}
	// 可选空白行。
	for {
		blank, ok := nextLine()
		if !ok {
			return src, 0
		}
		if len(bytesTrimSpace(blank)) != 0 {
			break
		}
		advance(blank)
	}
	// 开栅栏：顶格 3+ dash + 合法 infostring。
	fenceStart := offset
	opener, ok := nextLine()
	if !ok || len(opener) < 3 || opener[0] != '-' {
		return src, 0
	}
	dashes := 0
	for dashes < len(opener) && opener[dashes] == '-' {
		dashes++
	}
	if dashes < 3 || !rustValidInfostring(opener[dashes:]) {
		return src, 0
	}
	advance(opener)
	// 扫描闭栅栏：恰好 dashes 个 dash，无尾随非空白内容。
	for {
		candidate, ok := nextLine()
		if !ok {
			return src, 0
		}
		isClose := len(candidate) >= dashes
		for i := 0; isClose && i < dashes; i++ {
			isClose = candidate[i] == '-'
		}
		if isClose && len(bytesTrimSpace(candidate[dashes:])) == 0 && (len(candidate) == dashes || candidate[dashes] != '-') {
			fenceEndLine := line
			advance(candidate)
			out := append([]byte(nil), src...)
			for i := fenceStart; i < offset && i < len(out); i++ {
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
			return out, fenceEndLine
		}
		advance(candidate)
	}
}

// bytesTrimSpace 去除 ASCII 空白（frontmatter 栅栏行判定专用）。
func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

// rustValidInfostring 校验开栅栏 infostring（空或标识符形态）。
func rustValidInfostring(rest []byte) bool {
	rest = bytesTrimSpace(rest)
	if len(rest) == 0 {
		return true
	}
	if !((rest[0] >= 'a' && rest[0] <= 'z') || (rest[0] >= 'A' && rest[0] <= 'Z')) {
		return false
	}
	for _, b := range rest[1:] {
		valid := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '.' || b == '_' || b == '-'
		if !valid {
			return false
		}
	}
	return true
}

// rustPrimitiveMacroNames 是 tree-sitter-rust 词法为 primitive_type 的
// 全部名字。pinned grammar（rust@77a3747266f4）的 macro_invocation 只接受
// identifier/scoped_identifier/reserved identifier，`str!["…"]`（snapbox
// 断言宏，cargo 测试套广泛使用）会整文件解析失败。
var rustPrimitiveMacroNames = map[string]bool{
	"u8": true, "u16": true, "u32": true, "u64": true, "u128": true, "usize": true,
	"i8": true, "i16": true, "i32": true, "i64": true, "i128": true, "isize": true,
	"f32": true, "f64": true, "bool": true, "char": true, "str": true,
}

// rustMaxPrimitiveNameLen 是上表最长名字的字节数（usize/isize/u128/i128=5）。
const rustMaxPrimitiveNameLen = 5

// rustNormalizeMacroBangs 对解析输入做严格等长的宏名规范化：把
// `primitive!` 且后随宏定界符（(/[/{，允许空白）的 primitive 名首字节
// 改写为 'x'（str!→xtr!），使其按普通标识符词法进入 macro_invocation。
// 安全边界：
//   - 等长替换，树的行/字节几何与原文逐字节对齐（chunk 行区间可直接
//     切原文）；产物内容与符号一律取原文，规范化字节不外泄；
//   - 后随 != 等非定界符不改写（str != x 的比较语义不受触碰）；
//   - 字符串/注释内的 `str!(` 形字面量会被误改写，但引号/定界结构不变，
//     token 边界与树几何不受影响（内容仍取原文，无用户可见效应）；
//   - 无命中时零拷贝返回原切片。
func rustNormalizeMacroBangs(src []byte) []byte {
	out := src
	copied := false
	isIdentByte := func(b byte) bool {
		return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	for i := 1; i < len(src); i++ {
		if src[i] != '!' {
			continue
		}
		// `!` 后第一个非空白字节必须是宏定界符。
		j := i + 1
		for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
			j++
		}
		if j >= len(src) || (src[j] != '(' && src[j] != '[' && src[j] != '{') {
			continue
		}
		// 回溯 `!` 前的标识符 token（长度封顶 primitive 名上限）。
		tokenEnd := i
		tokenStart := tokenEnd
		for tokenStart > 0 && tokenEnd-tokenStart < rustMaxPrimitiveNameLen && isIdentByte(src[tokenStart-1]) {
			tokenStart--
		}
		if tokenStart == tokenEnd {
			continue
		}
		// token 左边界必须干净（更长的标识符如 mystr! 不是 primitive）。
		if tokenStart > 0 && isIdentByte(src[tokenStart-1]) {
			continue
		}
		if !rustPrimitiveMacroNames[string(src[tokenStart:tokenEnd])] {
			continue
		}
		if !copied {
			out = append([]byte(nil), src...)
			copied = true
		}
		out[tokenStart] = 'x'
	}
	return out
}
