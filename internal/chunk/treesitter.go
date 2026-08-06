package chunk

import (
	"strings"
	"sync"

	gotreesitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// 本文件实现 Tree-sitter AST 切分（决策 25 / D10）：批次 1（profile v3，
// Python、TypeScript/TSX、JavaScript）与批次 2（profile v5，Java、Rust，
// 语言级细则见 treesitter_java.go / treesitter_rust.go）按顶层声明切分，
// 语义对齐 splitGo（doc 注释附着、小声明相邻合并、超预算细分、函数符号
// 独立 chunk）。Go 保留标准库 go/ast 切分器；任何解析失败都回退行窗口
// 并如实上报 capability=fallback（§9.2 门禁 3/4 的失败语义）。

// parseTimeoutMicros 是单文件解析超时（暗坑 K60）。workspace 内容门禁
// 上限 1MiB，实测 500KB 合成源解析约 200ms，2s 覆盖病理输入后仍有界；
// 超时经 ParseStrict 显式报错，整文件回退行窗口。
const parseTimeoutMicros = 2_000_000

// DrainParserPools 释放 tree-sitter 运行时的包级 arena 池（M4）。池内
// arena 是强 Go 引用，GC 不会自行回收；上游要求批量扫描结束后排水。
// 调用点归属批量构建的收尾（如 localengine 构建循环结束处），本包只
// 提供封装避免调用方直接依赖 gotreesitter。幂等；与并发中的 Split
// 互不破坏（在途 arena 已检出、不在空闲池内，下次解析重新分配）。
func DrainParserPools() {
	gotreesitter.DrainArenaPools()
}

// treesitterGrammar 返回语言在已启用批次内对应的 grammar 名；不在批次
// 内返回空串。.tsx 是 TypeScript 的语法超集（JSX），上游以独立 grammar
// 发布，须按扩展名细分；.jsx 由 javascript grammar 原生覆盖。批次 2
// 加入 java/rust（grammar 均为 tree-sitter 官方仓，languages.lock pin：
// java@e10607b45ff7、rust@77a3747266f4）。
func treesitterGrammar(language string, relPath string) string {
	switch language {
	case "python":
		return "python"
	case "typescript":
		if strings.HasSuffix(strings.ToLower(relPath), ".tsx") {
			return "tsx"
		}
		return "typescript"
	case "javascript":
		return "javascript"
	case "java":
		return "java"
	case "rust":
		return "rust"
	case "c":
		return "c"
	case "cpp":
		return "cpp"
	case "csharp":
		return "c_sharp"
	// 批次 4(D5):kotlin/ruby/php(grammar 均内嵌于 gotreesitter
	// v0.47.0,subset tags 存在;5A 报告语言清单核查收尾 2026-08-07)。
	case "kotlin":
		return "kotlin"
	case "ruby":
		return "ruby"
	case "php":
		return "php"
	}
	return ""
}

// grammarCache 缓存按名加载的 grammar；存 nil 表示当前二进制未内嵌
// 该 grammar（如缺 grammar_subset_* 构建 tag），该语言按 fallback 处理。
var grammarCache sync.Map

// loadGrammar 从注册表按名加载 grammar；库在 blob 缺失或损坏时会
// panic，这里隔离为语言级 fallback（暗坑 K60），并缓存结果避免重复探测。
func loadGrammar(name string) (lang *gotreesitter.Language) {
	if cached, ok := grammarCache.Load(name); ok {
		lang, _ = cached.(*gotreesitter.Language)
		return lang
	}
	defer func() {
		if recover() != nil {
			lang = nil
		}
		grammarCache.Store(name, lang)
	}()
	if entry := grammars.DetectLanguageByName(name); entry != nil && entry.Language != nil {
		lang = entry.Language()
	}
	return lang
}

// splitTreeSitter 按 AST 顶层声明切分；失败（grammar 缺失、语法错误、
// 超时、解析器 panic）返回 ok=false，由调用方回退行窗口。语法错误时
// 整文件回退而非局部容忍，与 splitGo 的失败语义一致：不产出半坏边界。
func (p Profile) splitTreeSitter(file File, language string, grammarName string) (chunks []Chunk, ok bool) {
	defer func() {
		if recover() != nil {
			chunks, ok = nil, false
		}
	}()
	content := normalizeNewlines(file.Content)
	if strings.TrimSpace(content) == "" {
		return nil, false
	}
	// minified/生成物走字节窗口降级（暗坑 K7 同款门禁）：单行超长时
	// AST 声明边界无行粒度意义，且行覆盖式取文无法细分单行。
	if hasOversizedLine(content) {
		return nil, false
	}
	lang := loadGrammar(grammarName)
	if lang == nil {
		return nil, false
	}
	parser := gotreesitter.NewParser(lang)
	parser.SetTimeoutMicros(parseTimeoutMicros)
	// parseBytes 默认即原文；rust 走两段等长规范化：frontmatter 掩码
	// （cargo-script RFC 3503，pinned grammar 不识别）与 primitive! 宏名
	// 改写（rustNormalizeMacroBangs）。变换严格保长，树的行/字节几何与
	// 原文一一对应；span 取行区间、内容与符号一律切原文，规范化字节不
	// 进入任何产物。被掩码的 frontmatter 行区间以匿名序言 span 补入，
	// 保证行覆盖无缝隙（门 2）。
	parseBytes := []byte(content)
	frontmatterEnd := 0
	if grammarName == "rust" {
		parseBytes, frontmatterEnd = rustMaskFrontmatter(parseBytes)
		parseBytes = rustNormalizeMacroBangs(parseBytes)
	}
	// ParseStrict 把超时/预算等提前停止当作错误返回，避免拿到静默的
	// 部分树（Parse 在超时时返回 tree + nil error）。
	tree, err := parser.ParseStrict(parseBytes)
	// 成功与全部早退路径统一释放（M4）：ParseStrict 超时/预算中止时仍
	// 可能返回非 nil 部分树（err!=nil），弃置即放弃 arena 池化。Release
	// 对 nil 与重复调用安全；chunk 产物只持有 content 的字符串切片，
	// 不引用 arena 内节点，函数退出后释放无悬垂面。
	defer tree.Release()
	if err != nil || tree == nil {
		return nil, false
	}
	root := tree.RootNode()
	if root == nil {
		return nil, false
	}
	// 错误语义分派(批次 3 修订):
	// - 严格语言(批次 1/2 与 C#):任一顶层子树带错即整文件回退——这些
	//   grammar 对合法源解析近乎完美,带错即真语法错误,不产半坏边界;
	// - 容错语言(C/C++):预处理宏使局部解析错误成为常态(redis 实测
	//   sds.c 93 个顶层节点仅 2 个带错,整文件回退会把 AST 覆盖率压到
	//   52%),带错顶层节点由分类器一律按匿名 span 兜底(内容原样保留、
	//   不提取符号,防半坏边界),干净节点照常声明级切分;通篇皆错的
	//   垃圾文件由"零符号产出即回退"守卫拦截(collectTopLevelSpans 后)。
	// c_sharp 的坏因不同但同样需要容错:pinned grammar 对嵌套泛型
	// `new List<X<T>>()` 的 `>>` 误报 ERROR(合法 C#,FluentValidation
	// 实测 11/219 文件命中)——分类器按容器级展开隔离毒点(类名与干净
	// 成员符号保留,坏成员匿名)。
	// kotlin 同族(批次 4,okio 实测 33/313 文件):grammar 把软关键字
	// `yield` 作参数名误报 ERROR(合法 Kotlin),坏顶层节点由分类器
	// 匿名兜底,干净声明照常。
	errorTolerant := grammarName == "c" || grammarName == "cpp" || grammarName == "c_sharp" || grammarName == "kotlin"
	if !errorTolerant {
		if root.HasError() {
			return nil, false
		}
		// 该 runtime 的错误标记不上传到根（实测顶层子节点 err=true 时根
		// 仍为 false），逐个核查顶层子节点：任一子树带错即整文件回退。
		for _, child := range namedChildren(root) {
			if child.HasError() || child.IsError() || child.IsMissing() {
				return nil, false
			}
		}
	}
	lines := strings.Split(content, "\n")
	spans := p.collectTopLevelSpans(root, lang, content, len(lines), grammarName)
	if errorTolerant && !spansHaveSymbol(spans) {
		// 容错语言的整文件垃圾守卫:一个符号都提不出来说明解析面目全非
		// (真语法错误/非本语言内容),回退行窗口比伪 AST 边界诚实。
		return nil, false
	}
	// frontmatter 掩码区对 parser 是空白、不产节点，此处以匿名 span 补
	// 回（与 use 序言同为可合并声明前导；重叠由 coalesceOverlaps 归并）。
	if frontmatterEnd > 0 && frontmatterEnd <= len(lines) {
		spans = append([]declSpan{{start: 1, end: frontmatterEnd}}, spans...)
	}
	if len(spans) == 0 {
		return nil, false
	}
	merged := mergeSmallSpans(coalesceOverlaps(spans), lines, p.MaxChunkBytes)
	for _, span := range merged {
		text := joinLines(lines, span.start, span.end)
		if strings.TrimSpace(text) == "" {
			continue
		}
		chunks = append(chunks, p.splitOversized(file.RelPath, language, CapabilityAST, span.start, span.end, text, span.symbol)...)
	}
	if len(chunks) == 0 {
		return nil, false
	}
	return chunks, true
}

// namedChildren 一次性物化全部子节点并顺序过滤出命名节点。禁止用
// NamedChild(i) 循环遍历兄弟：该 runtime 的 NamedChild 每次都从下标 0
// 起线性扫描（v0.47.0 tree.go:1581），循环整体 O(n²)，数万顶层语句的
// 扁平生成文件会耗秒到分钟级且不可取消（M4）；Children 单次物化为
// O(n)，命名判定与 NamedChild 同一 flag，序列语义等价。返回的切片是
// 节点内部存储，只读遍历、不得修改。
func namedChildren(node *gotreesitter.Node) []*gotreesitter.Node {
	if node == nil {
		return nil
	}
	all := node.Children()
	named := make([]*gotreesitter.Node, 0, len(all))
	for _, child := range all {
		if child != nil && child.IsNamed() {
			named = append(named, child)
		}
	}
	return named
}

// tsNodeKind 是兄弟序列游走时的节点分类。
type tsNodeKind int

const (
	tsKindSkip    tsNodeKind = iota // 无效行区间，忽略
	tsKindComment                   // 注释：进入待附着序列
	tsKindDecl                      // 声明：注释可附着，函数/类不与邻居合并
	tsKindAnon                      // 其它语句：匿名 span，可相邻合并
)

// collectTopLevelSpans 游走根节点的顶层命名子节点，产出待合并 span 序列。
// 节点分类按 grammar 分派：批次 1（python/ts/tsx/js）共享 topLevelSpans，
// 批次 2 的 java/rust 节点类型集不同，各自持有分类器。
func (p Profile) collectTopLevelSpans(root *gotreesitter.Node, lang *gotreesitter.Language, src string, maxLine int, grammarName string) []declSpan {
	classify := p.topLevelSpans
	switch grammarName {
	case "java":
		classify = p.javaTopLevelSpans
	case "rust":
		classify = p.rustTopLevelSpans
	case "c":
		classify = p.cTopLevelSpans
	case "cpp":
		classify = p.cppTopLevelSpans
	case "c_sharp":
		classify = p.csharpTopLevelSpans
	case "kotlin":
		classify = p.kotlinTopLevelSpans
	case "ruby":
		classify = p.rubyTopLevelSpans
	case "php":
		classify = p.phpTopLevelSpans
	}
	return p.walkSiblings(root, lang, src, maxLine, func(node *gotreesitter.Node, start, end int) ([]declSpan, tsNodeKind) {
		return classify(node, lang, src, start, end, maxLine)
	})
}

// walkSiblings 按源码顺序游走 parent 的命名子节点，统一实现注释附着
// 规则（与 go/ast 的 Doc 语义对齐）：与声明起始行紧邻的连续注释块并入
// 该声明的 span；未附着的注释成为可合并的匿名 span。
func (p Profile) walkSiblings(parent *gotreesitter.Node, lang *gotreesitter.Language, src string, maxLine int, classify func(node *gotreesitter.Node, start, end int) ([]declSpan, tsNodeKind)) []declSpan {
	var spans []declSpan
	var commentRun []declSpan
	flushComments := func() {
		spans = append(spans, commentRun...)
		commentRun = commentRun[:0]
	}
	for _, node := range namedChildren(parent) {
		start, end, valid := nodeLines(node, maxLine)
		if !valid {
			continue
		}
		nodeSpans, kind := classify(node, start, end)
		switch kind {
		case tsKindSkip:
			continue
		case tsKindComment:
			// 注释块内部必须行连续，断开即先落盘旧块。
			if len(commentRun) > 0 && commentRun[len(commentRun)-1].end+1 != start {
				flushComments()
			}
			commentRun = append(commentRun, declSpan{start: start, end: end})
		case tsKindDecl:
			if len(nodeSpans) > 0 && len(commentRun) > 0 && commentRun[len(commentRun)-1].end+1 == nodeSpans[0].start {
				nodeSpans[0].start = commentRun[0].start
				commentRun = commentRun[:0]
			} else {
				flushComments()
			}
			spans = append(spans, nodeSpans...)
		case tsKindAnon:
			flushComments()
			spans = append(spans, nodeSpans...)
		}
	}
	flushComments()
	return spans
}

// topLevelSpans 对单个顶层节点分类并产出 span。export/decorated 等包装
// 节点解包内层声明取符号，span 仍覆盖包装本身（export 关键字、装饰器）。
func (p Profile) topLevelSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	if typ == "comment" {
		return nil, tsKindComment
	}
	inner := unwrapDecl(node, lang)
	switch inner.Type(lang) {
	case "function_definition", "function_declaration", "generator_function_declaration", "function_signature":
		return []declSpan{{start: start, end: end, symbol: tsNodeName(inner, lang, src), isFunc: true}}, tsKindDecl
	case "class_definition", "class_declaration", "abstract_class_declaration":
		return p.classSpans(node, inner, lang, src, start, end, maxLine), tsKindDecl
	case "interface_declaration", "type_alias_declaration", "enum_declaration", "module", "internal_module":
		return []declSpan{{start: start, end: end, symbol: tsNodeName(inner, lang, src)}}, tsKindDecl
	case "lexical_declaration", "variable_declaration":
		symbol, isFunc := declaratorInfo(inner, lang, src)
		return []declSpan{{start: start, end: end, symbol: symbol, isFunc: isFunc}}, tsKindDecl
	}
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// classStandalone 标记类 span 不与相邻声明合并：类与函数同为符号检索
// 的独立单元（exact-symbol 承诺），借用 declSpan.isFunc 的"永不合并"语义。
func classStandalone(span declSpan) declSpan {
	span.isFunc = true
	return span
}

// unwrapDecl 逐层解开包装节点（export_statement、decorated_definition、
// ambient_declaration），返回携带符号语义的内层声明；无内层时返回原节点。
func unwrapDecl(node *gotreesitter.Node, lang *gotreesitter.Language) *gotreesitter.Node {
	for {
		switch node.Type(lang) {
		case "decorated_definition":
			if def := node.ChildByFieldName("definition", lang); def != nil {
				node = def
				continue
			}
		case "export_statement", "ambient_declaration":
			if decl := firstDeclChild(node, lang); decl != nil {
				node = decl
				continue
			}
		}
		return node
	}
}

// declTypes 是可作为符号来源的声明节点类型集合。
var declTypes = map[string]bool{
	"function_definition": true, "function_declaration": true,
	"generator_function_declaration": true, "function_signature": true,
	"class_definition": true, "class_declaration": true, "abstract_class_declaration": true,
	"interface_declaration": true, "type_alias_declaration": true, "enum_declaration": true,
	"module": true, "internal_module": true,
	"lexical_declaration": true, "variable_declaration": true,
	"decorated_definition": true,
}

// firstDeclChild 返回节点的首个声明类命名子节点。
func firstDeclChild(node *gotreesitter.Node, lang *gotreesitter.Language) *gotreesitter.Node {
	for _, child := range namedChildren(node) {
		if declTypes[child.Type(lang)] {
			return child
		}
	}
	return nil
}

// classSpans 处理类声明：预算内保持单 span；超预算时拆为 header（类
// 声明与前置成员）+ 每个方法独立 span（symbol=Class.method，嵌套归属）。
// outer 覆盖 export/装饰器包装，class 是解包后的类节点。
func (p Profile) classSpans(outer *gotreesitter.Node, class *gotreesitter.Node, lang *gotreesitter.Language, src string, start, end, maxLine int) []declSpan {
	className := tsNodeName(class, lang, src)
	whole := classStandalone(declSpan{start: start, end: end, symbol: className})
	if int(outer.EndByte()-outer.StartByte()) <= p.MaxChunkBytes {
		return []declSpan{whole}
	}
	body := firstChildOfType(class, lang, "block", "class_body")
	if body == nil {
		return []declSpan{whole}
	}
	members := p.walkSiblings(body, lang, src, maxLine, func(node *gotreesitter.Node, mStart, mEnd int) ([]declSpan, tsNodeKind) {
		return classMemberSpans(node, lang, src, className, mStart, mEnd)
	})
	return assembleContainer(whole, start, end, members)
}

// assembleContainer 把容器声明（类/impl/trait/mod）组装为 header + 成员
// span 序列：header 从容器起点（含包装/附着注释由调用方走 walkSiblings
// 补齐）到首个成员前一行，前置匿名成员（docstring、字段、关联常量）并
// 入 header——它们是容器声明的解释性上下文，独立成块无检索价值；尾行
// （右括号等）并入最后一个成员，避免孤立缝隙 span。成员为空或成员前无
// 独立 header 行（同行内联 body）时退化为整容器单 span。
func assembleContainer(whole declSpan, start int, end int, members []declSpan) []declSpan {
	if len(members) == 0 {
		return []declSpan{whole}
	}
	headerEnd := members[0].start - 1
	if headerEnd < start {
		return []declSpan{whole}
	}
	header := classStandalone(declSpan{start: start, end: headerEnd, symbol: whole.symbol})
	idx := 0
	for idx < len(members) && members[idx].symbol == "" && !members[idx].isFunc {
		header.end = members[idx].end
		idx++
	}
	spans := append([]declSpan{header}, members[idx:]...)
	if last := &spans[len(spans)-1]; last.end < end {
		last.end = end
	}
	return spans
}

// classMemberSpans 对类 body 的单个成员分类：方法/嵌套类带 Class.name
// 符号；其余（docstring、字段、注释外语句）为匿名可合并 span。
func classMemberSpans(node *gotreesitter.Node, lang *gotreesitter.Language, src string, className string, start, end int) ([]declSpan, tsNodeKind) {
	typ := node.Type(lang)
	if typ == "comment" {
		return nil, tsKindComment
	}
	inner := unwrapDecl(node, lang)
	symbol := func(name string) string {
		if className != "" && name != "" {
			return className + "." + name
		}
		if name != "" {
			return name
		}
		return className
	}
	switch inner.Type(lang) {
	case "function_definition", "method_definition":
		return []declSpan{{start: start, end: end, symbol: symbol(tsNodeName(inner, lang, src)), isFunc: true}}, tsKindDecl
	case "class_definition", "class_declaration":
		// 嵌套类不再递归拆分：整体一个 span，超预算由 splitOversized 兜底。
		return []declSpan{classStandalone(declSpan{start: start, end: end, symbol: symbol(tsNodeName(inner, lang, src))})}, tsKindDecl
	case "public_field_definition", "field_definition":
		name := tsNodeName(inner, lang, src)
		if name == "" {
			// 边界检查与 tsNodeName 对齐(L15):越界虽被顶层 recover 兜为
			// 语言级 fallback,但防御应在切片处而非依赖 panic 恢复。
			if f := inner.ChildByFieldName("property", lang); f != nil {
				startByte, endByte := int(f.StartByte()), int(f.EndByte())
				if startByte >= 0 && endByte <= len(src) && startByte < endByte {
					name = src[startByte:endByte]
				}
			}
		}
		if value := inner.ChildByFieldName("value", lang); value != nil && funcValueTypes[value.Type(lang)] {
			return []declSpan{{start: start, end: end, symbol: symbol(name), isFunc: true}}, tsKindDecl
		}
		return []declSpan{{start: start, end: end}}, tsKindAnon
	}
	return []declSpan{{start: start, end: end}}, tsKindAnon
}

// funcValueTypes 是"值为函数"的节点类型（const fn = () => {} 等），
// 命中即按函数级声明对待：符号独立、不与邻居合并。
var funcValueTypes = map[string]bool{
	"arrow_function": true, "function_expression": true,
	"function": true, "generator_function": true,
}

// declaratorInfo 提取 lexical/variable declaration 的首个声明名，并判断
// 其值是否为函数（决定 isFunc 与合并策略）。
func declaratorInfo(node *gotreesitter.Node, lang *gotreesitter.Language, src string) (string, bool) {
	for _, child := range namedChildren(node) {
		if child.Type(lang) != "variable_declarator" {
			continue
		}
		name := tsNodeName(child, lang, src)
		isFunc := false
		if value := child.ChildByFieldName("value", lang); value != nil {
			isFunc = funcValueTypes[value.Type(lang)]
		}
		return name, isFunc
	}
	return "", false
}

// tsNodeName 读取节点 name field 的原文；无 name field 返回空。
func tsNodeName(node *gotreesitter.Node, lang *gotreesitter.Language, src string) string {
	if f := node.ChildByFieldName("name", lang); f != nil {
		startByte, endByte := int(f.StartByte()), int(f.EndByte())
		if startByte >= 0 && endByte <= len(src) && startByte < endByte {
			return src[startByte:endByte]
		}
	}
	return ""
}

// firstChildOfType 返回首个匹配类型的命名子节点。
func firstChildOfType(node *gotreesitter.Node, lang *gotreesitter.Language, types ...string) *gotreesitter.Node {
	for _, child := range namedChildren(node) {
		childType := child.Type(lang)
		for _, t := range types {
			if childType == t {
				return child
			}
		}
	}
	return nil
}

// nodeLines 把 tree-sitter 的 0-based Point 转换为 1-based 闭区间行号。
// 节点以换行收尾时 EndPoint 落在下一行 0 列，须回退一行避免多算。
func nodeLines(node *gotreesitter.Node, maxLine int) (int, int, bool) {
	sp, ep := node.StartPoint(), node.EndPoint()
	start := int(sp.Row) + 1
	end := int(ep.Row) + 1
	if ep.Column == 0 && end > start {
		end--
	}
	if start < 1 || start > maxLine {
		return 0, 0, false
	}
	if end > maxLine {
		end = maxLine
	}
	if end < start {
		end = start
	}
	return start, end, true
}

// coalesceOverlaps 合并行区间重叠的相邻 span（如单行多声明）：行覆盖
// 式取文下重叠区间会产出内容重复、ID 相同的 chunk，必须先归并。
func coalesceOverlaps(spans []declSpan) []declSpan {
	if len(spans) < 2 {
		return spans
	}
	out := spans[:1]
	for _, next := range spans[1:] {
		last := &out[len(out)-1]
		if next.start <= last.end {
			if next.end > last.end {
				last.end = next.end
			}
			if last.symbol == "" {
				last.symbol = next.symbol
			}
			last.isFunc = last.isFunc || next.isFunc
			continue
		}
		out = append(out, next)
	}
	return out
}

// spansHaveSymbol 判断 span 序列是否至少提取到一个符号(容错语言的
// 整文件垃圾守卫判据)。
func spansHaveSymbol(spans []declSpan) bool {
	for _, span := range spans {
		if span.symbol != "" {
			return true
		}
	}
	return false
}
