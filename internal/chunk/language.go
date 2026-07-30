package chunk

import (
	"path/filepath"
	"strings"
)

// extLanguage 是扩展名到语言标识的映射；语言标识进入索引字段与
// capability 上报，保持小写稳定值。
var extLanguage = map[string]string{
	".go":    "go",
	".ts":    "typescript",
	".tsx":   "typescript",
	".js":    "javascript",
	".jsx":   "javascript",
	".mjs":   "javascript",
	".cjs":   "javascript",
	".py":    "python",
	".java":  "java",
	".kt":    "kotlin",
	".cs":    "csharp",
	".c":     "c",
	".h":     "c",
	".cpp":   "cpp",
	".cc":    "cpp",
	".hpp":   "cpp",
	".rs":    "rust",
	".rb":    "ruby",
	".php":   "php",
	".swift": "swift",
	".scala": "scala",
	".lua":   "lua",
	".sh":    "shell",
	".bash":  "shell",
	".zsh":   "shell",
	".sql":   "sql",
	".proto": "proto",
	".md":    "markdown",
	".mdx":   "markdown",
	".rst":   "restructuredtext",
	".txt":   "text",
	".json":  "json",
	".yaml":  "yaml",
	".yml":   "yaml",
	".toml":  "toml",
	".xml":   "xml",
	".html":  "html",
	".css":   "css",
	// 注：不映射 .env——workspace 敏感文件 denylist 先于切分生效，
	// 该扩展名在本层不可达（Stage 2 review S13，Stage 5 收编清理）。
}

// docLanguages 使用独立窗口参数的知识/配置类语言。
var docLanguages = map[string]bool{
	"markdown":         true,
	"restructuredtext": true,
	"text":             true,
	"json":             true,
	"yaml":             true,
	"toml":             true,
	"xml":              true,
}

// DetectLanguage 按扩展名识别语言；未知扩展返回 "text"。
func DetectLanguage(relPath string) string {
	ext := strings.ToLower(filepath.Ext(relPath))
	if language, ok := extLanguage[ext]; ok {
		return language
	}
	return "text"
}

func isDocLanguage(language string) bool {
	return docLanguages[language]
}
