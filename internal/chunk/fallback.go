package chunk

import (
	"strings"
	"unicode/utf8"
)

// maxLineBytes 是单行长度阈值；超过即视为 minified/生成物，
// 按字节窗口降级切分，避免行窗口失效（暗坑 K7）。
const maxLineBytes = 4096

// splitLines 对非 Go 文本执行确定性行窗口切分。
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
	var chunks []Chunk
	step := window - overlap
	for start := 0; start < len(lines); start += step {
		end := start + window
		if end > len(lines) {
			end = len(lines)
		}
		text := strings.Join(lines[start:end], "\n")
		if strings.TrimSpace(text) != "" {
			chunks = append(chunks, p.splitOversized(file.RelPath, language, CapabilityFallback, start+1, end, text, "")...)
		}
		if end == len(lines) {
			break
		}
	}
	return chunks
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
