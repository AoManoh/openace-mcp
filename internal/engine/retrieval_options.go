package engine

import (
	"fmt"
	"strings"
)

// 检索请求 detail 与 artifact_kind 的合法取值只在这里列出。MCP 工具 schema
// 的 enum、MCP 与 daemon 的接收校验、Engine.Search 与 SearchRoutes 都使用这里
// 的值与 NormalizeRetrievalOptions，不另写一份。
const (
	// DetailFull：前 FullResults 个候选块带正文，其余只给头行（默认）。
	DetailFull = "full"
	// DetailPaths：全部候选只给头行。
	DetailPaths = "paths"

	// ArtifactAny：不按产物类型分组（默认）。
	ArtifactAny = "any"
	// ArtifactCode、ArtifactTests、ArtifactDocs：把该类型的候选按原相对顺序
	// 排到最前。
	ArtifactCode  = "code"
	ArtifactTests = "tests"
	ArtifactDocs  = "docs"
)

var (
	detailValues       = [...]string{DetailFull, DetailPaths}
	artifactKindValues = [...]string{ArtifactAny, ArtifactCode, ArtifactTests, ArtifactDocs}
)

// DetailValues 与 ArtifactKindValues 返回合法取值清单的新副本，供工具 schema
// 公开；调用方修改返回的切片不影响其他调用方与校验。
func DetailValues() []string { return append([]string(nil), detailValues[:]...) }

func ArtifactKindValues() []string { return append([]string(nil), artifactKindValues[:]...) }

// NormalizeDetail 校验并规范化 detail：去掉首尾空白后为空或 full 返回 full，
// paths 返回 paths。其余取值（包括大小写不同的写法）返回请求类错误：daemon
// 以 HTTP 400 返回，MCP 返回工具错误。
func NormalizeDetail(raw string) (string, error) {
	if value, ok := listedValue(raw, DetailFull, detailValues[:]); ok {
		return value, nil
	}
	return "", AsInvalidRequest(fmt.Errorf("invalid detail %q; use %s", raw, valueChoices(detailValues[:])))
}

// NormalizeArtifactKind 校验并规范化 artifact_kind：去掉首尾空白后为空或 any
// 返回 any，code、tests、docs 原样返回。其余取值（包括大小写不同的写法）
// 返回请求类错误。
func NormalizeArtifactKind(raw string) (string, error) {
	if value, ok := listedValue(raw, ArtifactAny, artifactKindValues[:]); ok {
		return value, nil
	}
	return "", AsInvalidRequest(fmt.Errorf("invalid artifact_kind %q; use %s", raw, valueChoices(artifactKindValues[:])))
}

func listedValue(raw string, fallback string, values []string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback, true
	}
	for _, listed := range values {
		if value == listed {
			return listed, true
		}
	}
	return "", false
}

func valueChoices(values []string) string {
	if len(values) < 2 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}

// NormalizeRetrievalOptions 依次校验 detail 与 artifact_kind，返回规范化后的
// 值。检索请求的各个入口在同步、建索引、生成任务或任何模型请求之前调用它；
// 同步请求没有这两个参数的语义，不调用。
func NormalizeRetrievalOptions(detail, artifactKind string) (string, string, error) {
	normalizedDetail, err := NormalizeDetail(detail)
	if err != nil {
		return "", "", err
	}
	normalizedKind, err := NormalizeArtifactKind(artifactKind)
	if err != nil {
		return "", "", err
	}
	return normalizedDetail, normalizedKind, nil
}
