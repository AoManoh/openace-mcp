package chunk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"unicode/utf8"
)

// splitGo 按顶层声明切分 Go 源文件。解析失败返回 ok=false，由调用方回退行窗口。
func (p Profile) splitGo(file File, language string) ([]Chunk, bool) {
	content := normalizeNewlines(file.Content)
	// bindata 风格巨型单行是合法 Go（单行可达 MiB 级），行粒度上无法
	// 细分，MaxChunkBytes 会失守（M2）。与 treesitter/行窗口路径同款
	// 守卫（暗坑 K7）：整文件降级字节窗口，capability 如实上报 fallback。
	if hasOversizedLine(content) {
		return nil, false
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file.RelPath, content, parser.ParseComments)
	if err != nil || parsed == nil {
		return nil, false
	}
	lines := strings.Split(content, "\n")
	var chunks []Chunk
	var pending []declSpan

	flush := func() {
		if len(pending) == 0 {
			return
		}
		merged := mergeSmallSpans(pending, lines, p.MaxChunkBytes)
		for _, span := range merged {
			text := joinLines(lines, span.start, span.end)
			if strings.TrimSpace(text) == "" {
				continue
			}
			chunks = append(chunks, p.splitOversized(file.RelPath, language, CapabilityAST, span.start, span.end, text, span.symbol)...)
		}
		pending = pending[:0]
	}

	for _, decl := range parsed.Decls {
		span, ok := declToSpan(fset, decl, len(lines))
		if !ok {
			continue
		}
		pending = append(pending, span)
	}
	flush()
	if len(chunks) == 0 {
		return nil, false
	}
	return chunks, true
}

// declSpan 是一个顶层声明的行区间（1-based 闭区间）。
type declSpan struct {
	start  int
	end    int
	symbol string
	// isFunc 标记函数/方法声明；函数永不与相邻声明合并，
	// 保证每个函数符号都有独立 chunk（exact-symbol 检索承诺）。
	isFunc bool
}

// declToSpan 提取声明的行区间与符号名；包含其 doc comment。
func declToSpan(fset *token.FileSet, decl ast.Decl, maxLine int) (declSpan, bool) {
	var start, end token.Pos
	var symbol string
	var isFunc bool
	switch d := decl.(type) {
	case *ast.FuncDecl:
		start, end = d.Pos(), d.End()
		if d.Doc != nil {
			start = d.Doc.Pos()
		}
		symbol = d.Name.Name
		if d.Recv != nil && len(d.Recv.List) == 1 {
			symbol = receiverName(d.Recv.List[0].Type) + "." + symbol
		}
		isFunc = true
	case *ast.GenDecl:
		start, end = d.Pos(), d.End()
		if d.Doc != nil {
			start = d.Doc.Pos()
		}
		symbol = genDeclSymbol(d)
	default:
		return declSpan{}, false
	}
	// PositionFor(pos, adjusted=false) 取未经 //line 指令调整的物理位置
	// （M3）：goyacc/cgo 生成文件常携带 //line 指令，adjusted 行号指向
	// 虚拟源（parser.y 等），用它取行区间会错位取文，或使 startLine 超出
	// 物理行数而静默丢声明。本函数消费的是 content 的物理行。
	startLine := fset.PositionFor(start, false).Line
	endLine := fset.PositionFor(end, false).Line
	if startLine < 1 || endLine < startLine || startLine > maxLine {
		return declSpan{}, false
	}
	if endLine > maxLine {
		endLine = maxLine
	}
	return declSpan{start: startLine, end: endLine, symbol: symbol, isFunc: isFunc}, true
}

// receiverName 提取方法接收者的类型名。
func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	default:
		return ""
	}
}

// genDeclSymbol 为 type/const/var/import 声明提取代表性符号名。
func genDeclSymbol(decl *ast.GenDecl) string {
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			return s.Name.Name
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return s.Names[0].Name
			}
		}
	}
	return ""
}

// smallSpanBytes 是"小声明"阈值：低于该字节数的相邻声明（import、
// const/var 组、小类型）才会互相合并。函数级声明必须独立成 chunk，
// 保证 SymbolHint 与源码声明一一对应（exact-symbol 检索承诺）。
const smallSpanBytes = 512

// mergeSmallSpans 只合并相邻的小声明，直到接近字节预算；
// 合并块的符号取组内字节数最大的成员，避免代表性符号被吞。
func mergeSmallSpans(spans []declSpan, lines []string, maxBytes int) []declSpan {
	if len(spans) == 0 {
		return nil
	}
	var merged []declSpan
	var current declSpan
	currentBytes := 0
	dominantBytes := 0
	active := false

	flush := func() {
		if active {
			merged = append(merged, current)
			active = false
			currentBytes = 0
			dominantBytes = 0
		}
	}

	for _, next := range spans {
		nextBytes := spanBytes(lines, next.start, next.end)
		if next.isFunc || nextBytes >= smallSpanBytes {
			flush()
			merged = append(merged, next)
			continue
		}
		if !active {
			current, currentBytes, dominantBytes, active = next, nextBytes, nextBytes, true
			continue
		}
		if currentBytes+nextBytes > maxBytes {
			flush()
			current, currentBytes, dominantBytes, active = next, nextBytes, nextBytes, true
			continue
		}
		if next.end > current.end {
			current.end = next.end
		}
		if next.symbol != "" && (current.symbol == "" || nextBytes > dominantBytes) {
			current.symbol = next.symbol
			dominantBytes = nextBytes
		}
		currentBytes += nextBytes
	}
	flush()
	return merged
}

// splitOversized 把超出字节预算的声明按行细分为多个同 capability chunk；
// 每段行数按预算与平均行宽估算，保证输出有界。单行超预算时行粒度已
// 无法细分，退化为字节窗口（M2 第二层守卫：整文件级 hasOversizedLine
// 只拦超 maxLineBytes 的行，(MaxChunkBytes, maxLineBytes] 区间靠这里
// 兜底）；行宽分布不均使某段仍超预算时递归细分，MaxChunkBytes 因此
// 对所有路径都是硬上限。
func (p Profile) splitOversized(relPath string, language string, capability Capability, startLine int, endLine int, text string, symbol string) []Chunk {
	if len(text) <= p.MaxChunkBytes {
		return []Chunk{p.build(relPath, language, capability, startLine, endLine, text, symbol)}
	}
	if startLine == endLine {
		return p.splitBytesWithin(relPath, language, capability, startLine, text, symbol)
	}
	lines := strings.Split(text, "\n")
	totalLines := len(lines)
	avg := len(text)/totalLines + 1
	step := p.MaxChunkBytes / avg
	if step < 1 {
		step = 1
	}
	var chunks []Chunk
	for offset := 0; offset < totalLines; offset += step {
		last := offset + step
		if last > totalLines {
			last = totalLines
		}
		part := strings.Join(lines[offset:last], "\n")
		if strings.TrimSpace(part) == "" {
			continue
		}
		// 递归终止有界：len(part)>预算时本段必含多行且 step<totalLines
		// （step≤预算/均宽<totalLines·预算/len(text)<totalLines），层层
		// 递减到单行后走字节窗口分支。
		chunks = append(chunks, p.splitOversized(relPath, language, capability, startLine+offset, startLine+last-1, part, symbol)...)
	}
	return chunks
}

// splitBytesWithin 把单行超预算文本按字节窗口细分；所有窗口都落在
// line 一行内（单行内细分不产生新行号），符号与 capability 沿用声明
// 边界的口径（与多行细分同款语义：边界仍来自 AST/行窗口，细分只是
// 预算兜底）。窗口边界回退 rune 起点，与 splitBytes 一致（review S16）。
func (p Profile) splitBytesWithin(relPath string, language string, capability Capability, line int, text string, symbol string) []Chunk {
	var chunks []Chunk
	offset := 0
	for offset < len(text) {
		end := offset + p.MaxChunkBytes
		if end >= len(text) {
			end = len(text)
		} else {
			for end > offset+1 && !utf8.RuneStart(text[end]) {
				end--
			}
		}
		part := text[offset:end]
		if strings.TrimSpace(part) != "" {
			chunks = append(chunks, p.build(relPath, language, capability, line, line, part, symbol))
		}
		offset = end
	}
	return chunks
}

// spanBytes 计算行区间的字节数（含换行）。
func spanBytes(lines []string, start int, end int) int {
	total := 0
	for i := start; i <= end && i <= len(lines); i++ {
		total += len(lines[i-1]) + 1
	}
	return total
}

// joinLines 取出 1-based 闭区间 [start, end] 的原文。
func joinLines(lines []string, start int, end int) string {
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n")
}
