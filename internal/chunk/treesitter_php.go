package chunk

import (
	gotreesitter "github.com/odvcencio/gotreesitter"
)

// 本文件是批次 4 的 PHP 声明分类器(treesitter.go 的语言分派目标)。
// 与 C# 高度同构:命名容器(class/interface/trait/enum)带 name field 与
// body(declaration_list/enum_declaration_list),预算内单 span、超预算
// 拆 header + 成员(assembleContainer);语句形 `namespace X;` 无 body、
// 后续声明是其兄弟——按匿名序言处理(C# file-scoped namespace 同款);
// braced 形 namespace 的 body 是 compound_statement(实测,非 C# 的
// declaration_list),展开为成员序列且不作符号前缀。PHP 按严格语言处理
// (树带错即整文件回退,见 splitTreeSitter),分类器可假设树干净。
// 节点形状经独立探针对 gotreesitter v0.47.0 内嵌 php grammar 实测:
// - 顶层:php_tag/declare_statement/namespace_use_declaration/
//   const_declaration 为匿名序言;类型声明与 function_definition 的
//   name field 全部有效;attribute_list(#[...])位于声明节点内部,
//   span 天然覆盖,doc 注释走 walkSiblings 附着。
// - HTML 混排:非 PHP 内容是顶层 text/text_interpolation 节点,grammar
//   永不报错——纯 HTML/纯垃圾输入解析为覆盖全文件字节的单个 text 节点
//   且 HasError=false,须由分类器显式识别为"零 PHP 结构"并回退。

// phpTypeDecls 是 PHP 的命名容器声明节点集合(name+body 实测均有效)。
var phpTypeDecls = map[string]bool{
	"class_declaration":     true,
	"interface_declaration": true,
	"trait_declaration":     true,
	"enum_declaration":      true,
}

// phpTopLevelSpans 对 program(或 braced namespace body)的单个节点分类。
func (p Profile) phpTopLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	switch {
	case typ == "comment":
		return nil, tsKindComment
	case typ == "text" && node.StartByte() == 0 && int(node.EndByte()) >= len(src):
		// 覆盖全文件字节的单个 text 节点 = 零 PHP 结构(无 php_tag 的纯
		// HTML、非 PHP 垃圾)。PHP 把标签外内容当字面输出,grammar 对其
		// 永不报错,严格语言的带错回退拦不住;此时 AST 不携带任何边界
		// 信息,产出"AST" chunk 是伪能力标注——skip 使 span 集为空,
		// splitTreeSitter 回退行窗口(与 .html 文件的处理一致,重叠窗口
		// 对纯文本检索也更友好)。混排文件的 text 节点不满足全文件字节
		// 覆盖(php_tag 至少占位),不受影响。
		return nil, tsKindSkip
	case phpTypeDecls[typ]:
		return p.phpTypeSpans(node, lang, src, start, end, maxLine), tsKindDecl
	case typ == "function_definition":
		return []declSpan{{start: start, end: end, symbol: tsNodeName(node, lang, src), isFunc: true}}, tsKindDecl
	case typ == "namespace_definition":
		// 两形态实测:语句形 `namespace X;` 无 body field,声明是其后续
		// 兄弟——匿名序言合并;braced 形 body=compound_statement,展开为
		// 成员序列(成员可独立检索)。namespace 名不作符号前缀:与 C#
		// namespace/Java package 同语义,PHP 的 use 别名机制也使前缀对
		// exact-symbol 检索价值有限。
		body := node.ChildByFieldName("body", lang)
		if body == nil {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		inner := p.walkSiblings(body, lang, src, maxLine, func(child *gotreesitter.Node, cStart, cEnd int) ([]declSpan, tsNodeKind) {
			return p.phpTopLevelSpans(child, lang, src, cStart, cEnd, maxLine)
		})
		if len(inner) == 0 {
			return []declSpan{{start: start, end: end}}, tsKindAnon
		}
		// 头行(namespace X {)并入首成员、右大括号并入尾成员,防覆盖缝隙。
		return cCoverRange(inner, start, end), tsKindDecl
	}
	// php_tag/declare_statement/namespace_use_declaration/const_declaration
	// 序言,text/text_interpolation HTML 片段,echo/赋值等脚本语句:一律
	// 匿名可合并 span——PHP 文件可以是纯脚本,匿名 span 保证行覆盖无缝
	// 隙,零声明脚本文件按语句边界产出匿名 AST chunk(树真实成立)。
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// phpTypeSpans 按容器处理类型声明(csharpTypeSpans 同构):预算内整容器
// 单 span(classStandalone:类型与函数同为 exact-symbol 检索的独立单元,
// 不与邻居合并);超预算拆 header + 成员。
func (p Profile) phpTypeSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) []declSpan {
	symbol := tsNodeName(node, lang, src)
	whole := classStandalone(declSpan{start: start, end: end, symbol: symbol})
	if int(node.EndByte()-node.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	// body field 对四种容器统一有效:class/interface/trait 是
	// declaration_list,enum 是 enum_declaration_list。
	body := node.ChildByFieldName("body", lang)
	if body == nil {
		return []declSpan{whole}
	}
	members := p.walkSiblings(body, lang, src, maxLine, func(member *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return p.phpMemberSpans(member, lang, src, symbol, mStart, mEnd)
	})
	return assembleContainer(whole, start, end, members)
}

// phpMemberSpans 对容器 body 的单个成员分类:方法带 Outer.name 符号且
// 独立成 chunk(name field 实测对 __construct/普通/静态方法均有效);
// 其余成员匿名可合并——property_declaration/const_declaration/
// use_declaration(trait 引入)/enum_case 是容器声明的解释性上下文,
// 前置者由 assembleContainer 并入 header,独立成块无检索价值(enum_case
// 虽有 name field,但 case 行单薄,合并进 header 比一行一符号更利检索)。
func (p Profile) phpMemberSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, outer string, start, end int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	switch {
	case typ == "comment":
		return nil, tsKindComment
	case typ == "method_declaration":
		return []declSpan{{start: start, end: end, symbol: javaQualify(outer, tsNodeName(node, lang, src)), isFunc: true}}, tsKindDecl
	case phpTypeDecls[typ]:
		// 防御分支:PHP 语法不允许嵌套类型声明,但 grammar 若容忍解析出
		// 该形态,按嵌套容器整体单 span(不递归拆分,超预算由
		// splitOversized 兜底),与批次 2/3 嵌套类型语义一致。
		return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: javaQualify(outer, tsNodeName(node, lang, src))})}, tsKindDecl
	}
	return []declSpan{{start: start, end: end}}, tsKindAnon
}
