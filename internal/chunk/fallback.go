package chunk

import (
	"strings"
	"unicode/utf8"
)

// maxLineBytes 是单行长度阈值；超过即视为 minified/生成物，
// 按字节窗口降级切分，避免行窗口失效（暗坑 K7）。
const maxLineBytes = 4096

// paragraphCutDivisor 控制段落边界成为切点的比例：空行之后下一段首行的
// hash 能被它整除时在该空行后切开，约每 paragraphCutDivisor 个段落一个切点。
const paragraphCutDivisor = 7

// lineSpan 是行下标区间 [start, end)。
type lineSpan struct {
	start int
	end   int
}

// splitLines 对非 Go 文本切分（profile v9 起边界由内容决定）。
//
// v8 以前按固定行窗口（窗口 window 行、步进 window-overlap 行）切分，
// 超过 MaxChunkBytes 的窗口再按行数细分。窗口起点只由行号决定：在大文件
// 中间插入一行，其后每个窗口的内容都变了，整个后半个文件重新送审嵌入。
// 2026-10-26 实测一份被频繁编辑的大 markdown 每改一次重嵌 88–100 个 chunk。
//
// v9 先把文件切成互不重叠的“核心段”，切点由行内容决定（restic、borg 等
// 使用的内容定义切分的做法）：非空行的 hash 能被核心段行数上限整除时在其后
// 切开；空行后面下一段首行的 hash 能被 paragraphCutDivisor 整除时在该空行
// 后切开。核心段超过行数上限（window-overlap）或字节上限（MaxChunkBytes）
// 时强制切开。切点不依赖行号，插入或删除只改变附近的核心段，强制切开引起
// 的错位到下一个内容切点即恢复对齐。每个 chunk 是核心段前面再借上一核心段
// 末尾至多 overlap 行（合计不超过 MaxChunkBytes），保留跨边界的上下文。
//
// 整个文件不超过一个窗口且不超过 MaxChunkBytes 时，产出与 v8 完全相同的
// 单个 chunk：内容与行号不变，升版本后这类文件的向量按内容键直接复用。
// 含超长行（minified/生成物）的文件仍按字节窗口切分，与 v8 相同。
func (p Profile) splitLines(file File, language string) []Chunk {
	content := normalizeNewlines(file.Content)
	if strings.TrimSpace(content) == "" {
		return nil
	}
	if hasOversizedLine(content) {
		return p.splitBytes(file.RelPath, language, content)
	}
	window := p.WindowLines
	if isDocLanguage(language) {
		window = p.DocWindowLines
	}
	if window < 1 {
		window = 1
	}
	overlap := p.OverlapLines
	if overlap >= window {
		overlap = window / 2
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= window && len(content) <= p.MaxChunkBytes {
		return []Chunk{p.build(file.RelPath, language, CapabilityFallback, 1, len(lines), content, "")}
	}
	// 多 chunk 文件末尾的换行不算一行：Split 在末尾换行后多出一个空串，
	// 计入的话最后一个 chunk 的 EndLine 会指向不存在的行。多 chunk 文件升版本
	// 时本来就重嵌，这里去掉它不增加送审量；单 chunk 文件保持 v8 原样。
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	cores := p.stableCores(lines, window-overlap)
	var chunks []Chunk
	for k, core := range cores {
		start := core.start
		if k > 0 && overlap > 0 {
			start = p.borrowOverlap(lines, cores[k-1], core, overlap)
		}
		text := strings.Join(lines[start:core.end], "\n")
		if strings.TrimSpace(text) == "" {
			continue
		}
		chunks = append(chunks, p.splitOversized(file.RelPath, language, CapabilityFallback, start+1, core.end, text, "")...)
	}
	return chunks
}

// stableCores 把行序列切成互不重叠、首尾相接的核心段（见 splitLines）。
// maxLines 是核心段行数上限，同时作为非空行 hash 切点的除数。
func (p Profile) stableCores(lines []string, maxLines int) []lineSpan {
	if maxLines < 1 {
		maxLines = 1
	}
	var cores []lineSpan
	start, size := 0, 0
	for i, line := range lines {
		lineBytes := len(line) + 1
		if i > start && (size+lineBytes > p.MaxChunkBytes || i-start >= maxLines) {
			cores = append(cores, lineSpan{start: start, end: i})
			start, size = i, 0
		}
		size += lineBytes
		if contentCutAfter(lines, i, maxLines) {
			cores = append(cores, lineSpan{start: start, end: i + 1})
			start, size = i+1, 0
		}
	}
	if start < len(lines) {
		cores = append(cores, lineSpan{start: start, end: len(lines)})
	}
	return cores
}

// contentCutAfter 判断第 i 行之后是不是内容切点：只看行内容，不看行号与
// 当前核心段已累计的长度。空行只在紧接下一段首行时判断，连续空行只在最后
// 一个空行后切。
func contentCutAfter(lines []string, i int, lineDivisor int) bool {
	if strings.TrimSpace(lines[i]) == "" {
		return i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" &&
			lineHash(lines[i+1])%paragraphCutDivisor == 0
	}
	return lineHash(lines[i])%uint32(lineDivisor) == 0
}

// borrowOverlap 返回 chunk 的起始行下标：从上一核心段末尾借至多 overlap 行，
// 借来的行与本核心段合计不超过 MaxChunkBytes；核心段本身已超限时不借。
func (p Profile) borrowOverlap(lines []string, prev lineSpan, core lineSpan, overlap int) int {
	size := 0
	for i := core.start; i < core.end; i++ {
		size += len(lines[i]) + 1
	}
	start := core.start
	for start > prev.start && core.start-start < overlap {
		lineBytes := len(lines[start-1]) + 1
		if size+lineBytes > p.MaxChunkBytes {
			break
		}
		size += lineBytes
		start--
	}
	return start
}

// lineHash 是行内容的 FNV-1a 32 位 hash，用来决定内容切点。
func lineHash(line string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(line); i++ {
		h ^= uint32(line[i])
		h *= 16777619
	}
	return h
}

// splitBytes 对含超长行的内容按字节窗口切分；行号按窗口起点所在行标注，
// 保证 StartLine/EndLine 仍指向真实位置。窗口边界回退到 rune 起点
// （review S16）：切断多字节 UTF-8 会产出非法内容并使落盘 JSONL 与
// ContentHash 脱钩（GATE-ENCODING 精神）。
func (p Profile) splitBytes(relPath string, language string, content string) []Chunk {
	var chunks []Chunk
	line := 1
	offset := 0
	for offset < len(content) {
		end := offset + p.MaxChunkBytes
		if end >= len(content) {
			end = len(content)
		} else {
			for end > offset+1 && !utf8.RuneStart(content[end]) {
				end--
			}
		}
		part := content[offset:end]
		startLine := line
		newlines := strings.Count(part, "\n")
		endLine := startLine + newlines
		// 以换行结尾的窗口不引入新行（review S16：EndLine 多算修正）。
		if strings.HasSuffix(part, "\n") && endLine > startLine {
			endLine--
		}
		if strings.TrimSpace(part) != "" {
			chunks = append(chunks, p.build(relPath, language, CapabilityFallback, startLine, endLine, part, ""))
		}
		line += newlines
		offset = end
	}
	return chunks
}

// hasOversizedLine 判断内容是否包含超过阈值的单行。
func hasOversizedLine(content string) bool {
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			if i-start > maxLineBytes {
				return true
			}
			start = i + 1
		}
	}
	return len(content)-start > maxLineBytes
}
