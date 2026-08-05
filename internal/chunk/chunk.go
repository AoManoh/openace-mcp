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

// DefaultProfile 是当前唯一启用的 profile。
// 参数依据 docs/references/2026-07-29-chunking-provider-benchmark-expansion.md §A5。
// Version 2（Stage 3）：字节窗口改为 rune 边界切分并修正尾换行 EndLine
// （review S16）——切分行为变化必须升版本以保证 chunk ID 跨版本不混用（K5）。
// Version 3（Stage 5，决策 25/D10 首批）：Python、TypeScript/TSX、JavaScript
// 由行窗口升级为 Tree-sitter AST 声明级切分（treesitter.go），解析失败
// 语言级回退行窗口；未变语言按纯内容 hash 复用向量零重嵌（K61）。
// Version 4（健壮性批次 M2/M3）：splitGo 增加超长单行守卫（整文件降级
// 字节窗口）、splitOversized 单行超预算按字节窗口细分（MaxChunkBytes
// 成为硬上限）、//line 指令改取物理行号——受影响文件的切分产出变化，
// 依 K5 升版本；未受影响 chunk 内容不变，向量按 ContentHash 复用（K61）。
// Version 5（语言批次 2）：Java、Rust 由行窗口升级为 Tree-sitter AST
// 声明级切分（treesitter_java.go / treesitter_rust.go）——与 v3 批次 1
// 同口径：切分行为变化依 K5 升版本，未变语言按纯内容 hash 复用向量
// 零重嵌（K61），解析失败语言级回退保留。
// Version 6（语言批次 3）：C、C++、C# 由行窗口升级为 Tree-sitter AST
// 声明级切分（treesitter_c.go / treesitter_cpp.go / treesitter_csharp.go），
// 并引入 C 族容错语义(坏顶层节点匿名兜底/预处理与 ERROR 容器展开/
// field 映射丢失按坏标记处理/零符号整文件回退守卫,treesitter.go
// errorTolerant)——依 K5 升版本;子树切换的向量迁移经跨子树复用工具
// (bench -dump-vectors-from)零重付,仅真实变化 chunk 重嵌。
func DefaultProfile() Profile {
	return Profile{
		ID:             "default",
		Version:        "6",
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

// Split 对单个文件执行切分：Go 走标准库 AST，Python/TypeScript/TSX/
// JavaScript（v3 批次 1）与 Java/Rust（v5 批次 2）走 Tree-sitter AST，
// 失败一律回退行窗口。返回的 capability 为该文件实际使用的切分方式，
// 禁止谎报。
func (p Profile) Split(file File) ([]Chunk, Capability) {
	language := DetectLanguage(file.RelPath)
	if language == "go" {
		if chunks, ok := p.splitGo(file, language); ok {
			return chunks, CapabilityAST
		}
	}
	if grammarName := treesitterGrammar(language, file.RelPath); grammarName != "" {
		if chunks, ok := p.splitTreeSitter(file, language, grammarName); ok {
			return chunks, CapabilityAST
		}
	}
	return p.splitLines(file, language), CapabilityFallback
}

// normalizeNewlines 统一 CRLF 为 LF，保证跨平台切分与 hash 一致。
func normalizeNewlines(content string) string {
	return strings.ReplaceAll(content, "\r\n", "\n")
}
