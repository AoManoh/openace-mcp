package engine

import (
	"strings"
	"testing"
)

func TestRetrievalOptionValuesAreIndependentCopies(t *testing.T) {
	detail := DetailValues()
	kind := ArtifactKindValues()
	detail[0] = "mutated"
	kind[0] = "mutated"
	if got := strings.Join(DetailValues(), ","); got != "full,paths" {
		t.Fatalf("DetailValues must return an independent copy: %s", got)
	}
	if got := strings.Join(ArtifactKindValues(), ","); got != "any,code,tests,docs" {
		t.Fatalf("ArtifactKindValues must return an independent copy: %s", got)
	}
	for _, value := range DetailValues() {
		if got, err := NormalizeDetail(value); err != nil || got != value {
			t.Fatalf("listed detail %q must be accepted as itself: %q %v", value, got, err)
		}
	}
	for _, value := range ArtifactKindValues() {
		if got, err := NormalizeArtifactKind(value); err != nil || got != value {
			t.Fatalf("listed artifact_kind %q must be accepted as itself: %q %v", value, got, err)
		}
	}
	if got, err := NormalizeDetail(" \t"); err != nil || got != DetailFull {
		t.Fatalf("blank detail must default to full: %q %v", got, err)
	}
	if got, err := NormalizeArtifactKind(""); err != nil || got != ArtifactAny {
		t.Fatalf("empty artifact_kind must default to any: %q %v", got, err)
	}
	for _, raw := range []string{"mutated", "Paths", "TESTS"} {
		if _, err := NormalizeDetail(raw); err == nil || !IsInvalidRequest(err) {
			t.Fatalf("detail %q must stay invalid: %v", raw, err)
		}
		if _, err := NormalizeArtifactKind(raw); err == nil || !IsInvalidRequest(err) {
			t.Fatalf("artifact_kind %q must stay invalid: %v", raw, err)
		}
	}
}
