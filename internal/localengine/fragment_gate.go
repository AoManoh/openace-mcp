package localengine

import (
	"strings"
	"unicode"
)

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
	// 版本号/端口号等单行数字标识(`1.26.5`)是有效配置内容,不得按
	// 日期误杀。日期碎片须有年月日单位,或呈多行拆散形态(≥3 行)。
	return hasDigit && (hasDateUnit || nonEmptyLines >= 3)
}

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
