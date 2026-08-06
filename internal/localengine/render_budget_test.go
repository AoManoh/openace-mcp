package localengine

import (
	"context"
	"strings"
	"testing"
)

// F3(review 2026-08-06,实测):首块超预算被 i>0 豁免——请求
// MaxOutputLen=200 可返回数 KB 且无截断标记,agent 的上下文预算管理
// 被静默打爆。修复后:首块超预算硬截断到预算并打标。
func TestRenderFirstBlockRespectsBudget(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	root := t.TempDir()
	var b strings.Builder
	b.WriteString("def giant_handler(x):\n")
	for i := 0; i < 300; i++ {
		b.WriteString("    x = x + 1  # padding line to inflate the single block far beyond budget\n")
	}
	b.WriteString("    return x\n")
	writeFixture(t, root, "giant.py", b.String())
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	req := searchRequest(root, "giant_handler")
	req.MaxOutputLen = 200
	result, err := e.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Text) > 200+len(truncationMarker)+64 {
		t.Fatalf("首块未按预算截断: 请求 200,返回 %d 字节", len(result.Text))
	}
	if !strings.Contains(result.Text, "[output truncated by max_output_length]") {
		t.Fatalf("截断无标记: %q", result.Text[:min(len(result.Text), 120)])
	}
}
