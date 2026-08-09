package main

import (
	"reflect"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/localengine"
)

func TestCollapseCandidatesKeepsFirstDocumentSpan(t *testing.T) {
	candidates := []localengine.CandidateRef{
		{ID: "c1", RelPath: "a.py", StartLine: 10, EndLine: 20},
		{ID: "c2", RelPath: "a.py", StartLine: 30, EndLine: 40},
		{ID: "c3", RelPath: "b.py", StartLine: 5, EndLine: 8},
	}
	docs, hits := collapseCandidates(candidates, map[string]string{"a.py": "doc-a", "b.py": "doc-b"}, 2)
	if !reflect.DeepEqual(docs, []string{"doc-a", "doc-b"}) {
		t.Fatalf("docs: %v", docs)
	}
	want := []resultHit{
		{DocID: "doc-a", Path: "a.py", StartLine: 10, EndLine: 20},
		{DocID: "doc-b", Path: "b.py", StartLine: 5, EndLine: 8},
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits: %+v", hits)
	}
}
