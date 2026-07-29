package chunk

import "strings"

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
// 保证 StartLine/EndLine 仍指向真实位置。
func (p Profile) splitBytes(relPath string, language string, content string) []Chunk {
	var chunks []Chunk
	line := 1
	for offset := 0; offset < len(content); offset += p.MaxChunkBytes {
		end := offset + p.MaxChunkBytes
		if end > len(content) {
			end = len(content)
		}
		part := content[offset:end]
		startLine := line
		line += strings.Count(part, "\n")
		if strings.TrimSpace(part) != "" {
			chunks = append(chunks, p.build(relPath, language, CapabilityFallback, startLine, line, part, ""))
		}
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
