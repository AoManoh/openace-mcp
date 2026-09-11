package localengine

import (
	"strings"
	"unicode"
)

// isFragmentNoise 判断一个 chunk 是否是没有检索价值的碎片块。这类块来自
// 行窗口切分（Capability 为 fallback）的文档：一个日期被拆成一行一个字、
// 一段分隔符、一段空白。它们会占用结果位置却回答不了任何问题。判定
// 只在 fragmentGate 开启时使用，默认关闭。
//   - AST 切分产出的 chunk 一律不算碎片：声明级切分出的短内容（如一个
//     短函数）是有效代码。
//   - 去掉首尾空白后为空、或只剩日期成分（见 isDateOnlyFragment）、或
//     没有任何字母与数字：算碎片。
//   - 其余情况要同时满足四个条件才算碎片：至少 4 个含字母或数字的行、
//     字母数字总数不超过 32、每行字母数字数不超过 4、没有任何长度达到
//     3 的连续 ASCII 字母、数字或下划线串（一个英文单词或标识符）。单独
//     按字符数设阈值会把正常的短文档段与一行 TODO 误判为碎片，四个条件
//     合起来只命中多行、每行一两个字、没有单词的形态。
func isFragmentNoise(record chunkRecord) bool {
	if record.Capability != "fallback" {
		return false
	}
	content := strings.TrimSpace(record.Content)
	if content == "" {
		return true
	}
	if isDateOnlyFragment(content) {
		return true
	}

	nonEmptyLines := 0
	maxEffectivePerLine := 0
	totalEffective := 0
	hasASCIIWord := false
	for _, line := range strings.Split(content, "\n") {
		effective := 0
		asciiRun := 0
		for _, r := range line {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				effective++
				totalEffective++
			}
			if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
				asciiRun++
				if asciiRun >= 3 {
					hasASCIIWord = true
				}
			} else {
				asciiRun = 0
			}
		}
		if effective > 0 {
			nonEmptyLines++
			if effective > maxEffectivePerLine {
				maxEffectivePerLine = effective
			}
		}
	}
	if totalEffective == 0 {
		return true
	}
	return nonEmptyLines >= 4 && totalEffective <= 32 && maxEffectivePerLine <= 4 && !hasASCIIWord
}

// isDateOnlyFragment 判断内容是否只是一个日期：除空白、标点与符号外只有
// 数字和中文日期单位（年月日号时分秒），出现任何其他字符即不是。此外
// 必须含数字，并且要么含日期单位，要么拆成了至少 3 个非空行。
func isDateOnlyFragment(content string) bool {
	hasDigit := false
	hasDateUnit := false
	nonEmptyLines := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) != "" {
			nonEmptyLines++
		}
	}
	for _, r := range content {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			continue
		case strings.ContainsRune("年月日号时分秒", r):
			hasDateUnit = true
		default:
			return false
		}
	}
	// 单行的纯数字标识（版本号 1.26.5、端口号 8765）是有效的配置内容，
	// 只有数字不算日期：还要含年月日单位，或者被拆成至少 3 个非空行
	// （日期碎片的典型形态是一行一个字）。
	return hasDigit && (hasDateUnit || nonEmptyLines >= 3)
}

// filterFragmentNoise 从已排序的候选中去掉碎片块，返回过滤后的候选与去掉
// 的数量。fragmentGate 关闭（默认）或候选为空时原样返回，行为与没有本
// 功能时一致。调用点在精排（rerank）之后、渲染之前，不改变召回与精排的
// 候选池。读取 chunk 记录失败时返回错误，不静默放行。
func (e *Engine) filterFragmentNoise(handle *revisionHandle, ordered []rankedHit) ([]rankedHit, int, error) {
	if !e.fragmentGate || len(ordered) == 0 {
		return ordered, 0, nil
	}
	filtered := make([]rankedHit, 0, len(ordered))
	removed := 0
	for _, hit := range ordered {
		record, err := handle.record(hit.id)
		if err != nil {
			return nil, 0, err
		}
		if isFragmentNoise(record) {
			removed++
			continue
		}
		filtered = append(filtered, hit)
	}
	return filtered, removed, nil
}
