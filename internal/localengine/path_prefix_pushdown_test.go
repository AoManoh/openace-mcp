package localengine

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// P-gray-02(2026-08-11 外部灰度,F:\KE):path_prefix 是融合后的后置过滤,
// 目标子树被海量三方目录(extern/)挤出深度受限候选池(纯词法 top20/
// hybrid 每路 top60)时,prefix 过滤后为空——同一查询全仓可命中,加前缀
// 反而 miss。修复=前缀下推到检索路由:prefix 存在时词法/语义路只在
// 子树内选 topK,空结果语义变为"子树内确无匹配"。
func TestSearchPathPrefixSurvivesDominatedSubtree(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	// 40 个 extern 干扰文件:符号即 WinMain(symbol/symbol_raw/content 全命中);
	// trunk 目标符号不同(仅 content 弱命中一词),得分严格最低——
	// 词法 defaultTopK=20 下旧实现必然把 trunk 挤出候选池,prefix 后置过滤为空。
	for i := 0; i < 40; i++ {
		writeFixture(t, root, fmt.Sprintf("extern/lib%02d/winmain.c", i),
			"// WinMain application entry point. WinMain application entry point.\n"+
				"// application entry point wrapper around WinMain entry.\nint WinMain() { return 0; }\n")
	}
	writeFixture(t, root, "trunk/client/main.c",
		"// game client startup lives here (WinMain)\nint GameClientMain() { return 42; }\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	req := searchRequest(root, "WinMain application entry point")
	req.PathPrefix = "trunk"
	res, err := e.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 || !strings.Contains(res.Text, "trunk/client/main.c") {
		t.Fatalf("被淹没子树的 prefix 检索必须命中子树内容: hits=%d text=%q", len(res.Hits), res.Text)
	}
	for _, hit := range res.Hits {
		if !strings.HasPrefix(hit.Path, "trunk/") {
			t.Fatalf("hit 越出前缀: %+v", hit)
		}
	}
}
