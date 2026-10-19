package engine

import (
	"strings"
	"testing"
)

// TestNormalizeDetail：去掉首尾空白后为空或 full 是 full，paths 是 paths；
// 其余取值（包括大小写不同的写法）按请求类错误拒绝。
func TestNormalizeDetail(t *testing.T) {
	for raw, want := range map[string]string{"": DetailFull, "  ": DetailFull, "full": DetailFull, " full ": DetailFull, "paths": DetailPaths, "\tpaths\n": DetailPaths} {
		if got, err := NormalizeDetail(raw); err != nil || got != want {
			t.Fatalf("%q: want %q, got %q err=%v", raw, want, got, err)
		}
	}
	for _, raw := range []string{"PATHS", "Full", "summary", "unsupported", "full,paths"} {
		_, err := NormalizeDetail(raw)
		if err == nil || !IsInvalidRequest(err) || !strings.Contains(err.Error(), "use full or paths") {
			t.Fatalf("%q: 应按请求类错误拒绝: %v", raw, err)
		}
	}
}

// TestNormalizeArtifactKind：去掉首尾空白后为空或 any 是 any；code、tests、
// docs 原样；其余取值按请求类错误拒绝，不做大小写折叠。
func TestNormalizeArtifactKind(t *testing.T) {
	for raw, want := range map[string]string{"": ArtifactAny, "any": ArtifactAny, " any ": ArtifactAny, "code": ArtifactCode, "tests": ArtifactTests, " docs ": ArtifactDocs} {
		if got, err := NormalizeArtifactKind(raw); err != nil || got != want {
			t.Fatalf("%q: want %q, got %q err=%v", raw, want, got, err)
		}
	}
	for _, raw := range []string{"test", "documentation", "CODE", "*"} {
		_, err := NormalizeArtifactKind(raw)
		if err == nil || !IsInvalidRequest(err) || !strings.Contains(err.Error(), "use any, code, tests or docs") {
			t.Fatalf("%q: 应按请求类错误拒绝: %v", raw, err)
		}
	}
}

// TestNormalizeRetrievalOptions：两个参数一起校验，任一非法即返回请求类错误。
func TestNormalizeRetrievalOptions(t *testing.T) {
	if detail, kind, err := NormalizeRetrievalOptions(" paths ", " code "); err != nil || detail != DetailPaths || kind != ArtifactCode {
		t.Fatalf("合法值应规范化: %q %q %v", detail, kind, err)
	}
	if _, _, err := NormalizeRetrievalOptions("unsupported", "code"); err == nil || !strings.Contains(err.Error(), "invalid detail") {
		t.Fatalf("非法 detail 应报错: %v", err)
	}
	if _, _, err := NormalizeRetrievalOptions("full", "CODE"); err == nil || !strings.Contains(err.Error(), "invalid artifact_kind") {
		t.Fatalf("非法 artifact_kind 应报错: %v", err)
	}
	if len(DetailValues()) != 2 || len(ArtifactKindValues()) != 4 {
		t.Fatalf("合法取值清单不符: %v %v", DetailValues(), ArtifactKindValues())
	}
}
