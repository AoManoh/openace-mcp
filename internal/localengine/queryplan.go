package localengine

import (
	"strings"
	"unicode"
)

// 本文件实现查询规划：对含结构 token（标识符、路径、camelCase 单词）的
// 自然语言查询，词法路（BM25）只用这些结构 token 检索，dense 路与 rerank
// 路仍用原查询。
//
// 原因：英文自然语言查询里的包装词（where、is、config、key、defined）与
// 语料里的高频英文词大量匹配，词法路因此失去焦点，查询里的配置键或符号
// 不再排在词法首位。中文查询没有这个问题：CJK 包装词与 ASCII 词元互不
// 匹配。实测同一组 111 条配置类查询，英文版比中文版 R@5 低 26.1 个百分
// 点，差距来自词法路。只改词法路、保留原查询给 dense 与 rerank，可以让
// 任何语言的查询同时得到"词法聚焦"和"语义完整"。
//
// 原查询在请求中不被改写：dense/rerank 路照常使用，结果的 QueryPlan
// 字段记录本次词法路实际使用的查询串供审计。结构变体在词法路零命中时，
// 调用方回退到原查询再检索一次（见 search.go 的 lexicalRoute）。
//
// 触发条件三条，全部满足才触发；不触发时检索行为与没有本规划完全一致：
//  1. 查询至少 4 个空白分隔 token。只含一个键名或精确符号的短查询本来
//     就命中准确，改写没有收益。
//  2. 至少含 1 个结构 token（判定见 isStructuralToken）。
//  3. 不含 CJK 字符。CJK 包装词不与 ASCII 词元匹配，词法路已经聚焦
//     （实测 14 条这类查询中 12 条的键名已是词法首位），触发只会引入
//     变化而没有收益。

// queryPlan 是一次查询规划的结果。零值表示不触发，词法路使用原查询。
type queryPlan struct {
	// Triggered 为 true 时词法路使用 LexicalQuery，否则使用原查询。
	Triggered bool
	// LexicalQuery 是原查询中的结构 token 按原顺序用单个空格连接得到的
	// 词法路查询串。
	LexicalQuery string
}

// planLexicalQuery 按文件头的三条触发条件对原查询做规划。纯函数，同一
// 输入恒定输出；每次查询只调用一次。
func planLexicalQuery(query string) queryPlan {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" || containsCJK(trimmed) {
		return queryPlan{}
	}
	tokens := strings.Fields(trimmed)
	if len(tokens) < 4 {
		return queryPlan{}
	}
	var structural []string
	for _, tok := range tokens {
		if isStructuralToken(tok) {
			structural = append(structural, tok)
		}
	}
	if len(structural) == 0 {
		return queryPlan{}
	}
	return queryPlan{Triggered: true, LexicalQuery: strings.Join(structural, " ")}
}

// isStructuralToken 判定一个 token 是否具有代码结构形态。规则只看字符，
// 不读配置、不访问网络：
//   - 含 `.`、`-`、`_`、`/` 之一的多段标识符算（serde.workspace、
//     license-files、hash_password、internal/lexical/bleve.go）；只有数字
//     的分段词与版本号（1.2、v1.2.3）不算；
//   - camelCase / mixedCase 单词算（maxOutputLength、buildDelta）。
//
// 全大写缩写（HTTP、GET）与普通英文单词不算：它们在自然语言里也常见，
// 不指向具体的代码位置。
func isStructuralToken(tok string) bool {
	tok = strings.Trim(tok, ".,;:!?()[]{}\"'`")
	if tok == "" {
		return false
	}
	hasSeparator := false
	hasLower := false
	hasUpper := false
	hasLetter := false
	upperAfterLower := false
	prevLower := false
	for _, r := range tok {
		switch {
		case r == '.' || r == '-' || r == '_' || r == '/':
			hasSeparator = true
			prevLower = false
			continue
		case unicode.IsLower(r):
			hasLower = true
			hasLetter = true
			prevLower = true
			continue
		case unicode.IsUpper(r):
			hasUpper = true
			hasLetter = true
			if prevLower {
				upperAfterLower = true
			}
			prevLower = false
			continue
		case unicode.IsDigit(r):
			prevLower = false
			continue
		default:
			// 其他字符（含 CJK）不算分隔符也不算字母，只打断 camelCase 的
			// "小写后接大写"判定。
			prevLower = false
		}
	}
	if hasSeparator {
		// 含分隔符的 token 还必须含字母，否则是 1.2、2024-01-31 这类只有
		// 数字的分段词。字母只剩单个 v/V 的（v1.2.3）是版本号，同样排除。
		if !hasLetter {
			return false
		}
		letters := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) {
				return r
			}
			return -1
		}, tok)
		if len(letters) == 1 && (letters == "v" || letters == "V") {
			return false
		}
		return true
	}
	// 无分隔符时只认 camelCase：某个小写字母后面直接跟大写字母，且大小写
	// 同时存在。全大写缩写没有"小写后接大写"，因此不算。
	return upperAfterLower && hasLower && hasUpper
}

// containsCJK 判定字符串是否含汉字、日文假名或韩文音节；含任一即视为
// CJK 查询，不做查询规划。
func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}
