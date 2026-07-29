// Package chunk 提供确定性的代码切分：Go 文件按 AST 声明切分，
// 其它文本按行窗口切分。chunk 身份可跨进程、跨机器复现，是索引
// 增量与后续 embedding 缓存复用的基础。
package chunk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Capability 表示某语言在当前 profile 下的切分能力。
type Capability string

const (
	CapabilityAST      Capability = "ast"
	CapabilityFallback Capability = "fallback"
)

// Chunk 是一个可索引的代码/文本片段。
// StartLine/EndLine 为 1-based 且闭区间，与编辑器行号一致。
type Chunk struct {
	// ID 由 profile、相对路径、内容 hash 与行区间派生（见 Profile.ChunkID），
	// 同输入跨进程复现相同值。
	ID       string
	RelPath  string
	Language string
	// Capability 记录该 chunk 由 AST 还是行窗口产生，禁止谎报。
	Capability Capability
	StartLine  int
	EndLine    int
	Content    string
	// SymbolHint 是 AST 路径下的声明名（函数/类型/方法），行窗口路径为空。
	SymbolHint string
	// ContentHash 是纯内容 hash（不含行号与路径），供后续阶段按内容
	// 复用派生产物（如 embedding 缓存），避免行号平移放大失效。
	ContentHash string
}

// Profile 固定一次切分的全部参数；参数变化即新 profile，
// chunk ID 随之变化，禁止跨 profile 复用切分产物。
type Profile struct {
	// ID 是 profile 名称（如 "default"）。
	ID string
	// Version 随任何切分行为变化而递增。
	Version string
	// MaxChunkBytes 是单 chunk 内容字节上限；AST 声明超限时细分，
	// 行窗口按此上限截断单行超长内容。
	MaxChunkBytes int
	// WindowLines / OverlapLines 是行窗口 fallback 的窗口与重叠行数。
	WindowLines  int
	OverlapLines int
	// DocWindowLines 是 Markdown/JSON/YAML/TOML 等知识与配置文件的独立窗口。
	DocWindowLines int
}

// DefaultProfile 是 Stage 2 唯一启用的 profile。
// 参数依据 docs/references/2026-07-29-chunking-provider-benchmark-expansion.md §A5。
func DefaultProfile() Profile {
	return Profile{
		ID:             "default",
		Version:        "1",
		MaxChunkBytes:  2048,
		WindowLines:    60,
		OverlapLines:   10,
		DocWindowLines: 40,
	}
}

// ChunkID 派生确定性 chunk 身份：
// sha256(profileID/version + relpath + contentHash + lineRange) 的前 16 字节 hex。
func (p Profile) ChunkID(relPath string, contentHash string, startLine int, endLine int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s@%s\x00%s\x00%s\x00%d-%d", p.ID, p.Version, relPath, contentHash, startLine, endLine)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// HashContent 计算纯内容 hash（sha256 前 16 字节 hex）。
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:32]
}

// build 组装 Chunk 并填充派生字段。
func (p Profile) build(relPath string, language string, capability Capability, startLine int, endLine int, content string, symbolHint string) Chunk {
	contentHash := HashContent(content)
	return Chunk{
		ID:          p.ChunkID(relPath, contentHash, startLine, endLine),
		RelPath:     relPath,
		Language:    language,
		Capability:  capability,
		StartLine:   startLine,
		EndLine:     endLine,
		Content:     content,
		SymbolHint:  symbolHint,
		ContentHash: contentHash,
	}
}

// File 描述一个待切分文件。Content 必须已通过调用方的可索引判定
// （文本、UTF-8、大小上限），本包不重复做内容门禁。
type File struct {
	RelPath string
	Content string
}

// Split 对单个文件执行切分：Go 走 AST（失败回退行窗口），其余走行窗口。
// 返回的 capability 为该文件实际使用的切分方式。
func (p Profile) Split(file File) ([]Chunk, Capability) {
	language := DetectLanguage(file.RelPath)
	if language == "go" {
		if chunks, ok := p.splitGo(file, language); ok {
			return chunks, CapabilityAST
		}
	}
	return p.splitLines(file, language), CapabilityFallback
}

// normalizeNewlines 统一 CRLF 为 LF，保证跨平台切分与 hash 一致。
func normalizeNewlines(content string) string {
	return strings.ReplaceAll(content, "\r\n", "\n")
}
