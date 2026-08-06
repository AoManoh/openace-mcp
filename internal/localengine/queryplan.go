package localengine

import (
	"strings"
	"unicode"
)

// 路由分立查询规划(方案 docs/development/2026-08-06-route-split-query-proposal.md,
// §7 -13/-14 批准):对含结构 token 的自然语言查询,词法路(BM25)改用
// 结构 token 变体,dense/rerank 路保持原查询——把 zh 包装在英文语料上
// 免费获得的"词法聚焦 + 语义完整"双路最优(headroom verdict §3)显式
// 提供给任何语言的查询。原查询在调用侧永不丢失(调研护栏 7/8)。
//
// 触发门三条(全部满足才触发;不触发 = 检索行为与历史逐字节一致):
//  1. 查询 ≥4 个空白分隔 token——纯 key/exact-symbol 查询现行为已最优;
//  2. 含 ≥1 个结构 token(见 isStructuralToken);
//  3. 不含 CJK 字符——CJK 包装词天然不与 ASCII 词元碰撞,词法路已
//     等效聚焦(headroom 实测 12/14 #1 锚),触发只会徒增变数。

// queryPlan 是一次查询规划的结果。零值 = 不触发。
type queryPlan struct {
	// Triggered 为 true 时词法路使用 LexicalQuery,否则用原查询。
	Triggered bool
	// LexicalQuery 是结构 token 按原序空格连接的词法路变体(变体 A)。
	LexicalQuery string
}

// planLexicalQuery 对原查询做路由分立规划。纯函数、确定性、零分配热路径
// 之外(每查询一次)。
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

// isStructuralToken 判定 token 是否携带代码结构形态。规则(全部纯词法,
// 零配置零网络):
//   - 含 `.`/`-`/`_`/`/` 的多段标识符(serde.workspace、license-files、
//     hash_password、internal/lexical/bleve.go),但纯数字/版本号
//     (1.2、v1.2.3)不算;
//   - camelCase / mixedCase 单词(maxOutputLength、buildDelta)。
//
// 纯大写缩写(HTTP、GET)与普通英文词不算——它们是自然语言的常规部件,
// 不构成"定位性"信号。
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
			// 其他字符(含 CJK)不参与结构判定。
			prevLower = false
		}
	}
	if hasSeparator {
		// 必须含字母,排除 1.2 / 2026-08-06 这类纯数字分段;
		// 排除 v1.2.3 形态(去掉分隔符后仅剩 v+数字)。
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
	// camelCase:小写后跟大写,且同时存在大小写(排除全大写缩写)。
	return upperAfterLower && hasLower && hasUpper
}

// containsCJK 判定字符串是否含 CJK 统一表意/日文假名/韩文音节。
func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}
