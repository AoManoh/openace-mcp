package lexical

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// 方案②机制 A 失败测试:CJK 包装措辞淹没高区分度 key(WP-4 nacos 面,
// raw key R@5 0.800 → zh 包装 0.017 的最小复现)。
//
// 语料形态:1 个含 key 的配置 chunk + 多个中文文档 chunk(包装词
// "配置/项/在/哪/里/定/义" 的 Han 单字在中文文档里高频出现)。
func buildCJKIndex(t *testing.T) *Index {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cjk.bleve")
	docs := []Doc{
		{ID: "gold", Path: "conf/application.properties", Language: "properties",
			Content: "nacos.core.auth.enabled=true\nnacos.core.auth.system.type=nacos"},
	}
	// 中文文档:每篇都密集含包装用字(模拟 nacos 中文手册/注释语域)。
	zhBody := "本配置项说明:在这里定义了服务发现的配置。配置项在哪里定义?在配置中心里定义。" +
		"每个配置项都有定义位置,定义在配置文件里。哪里能改配置?在控制台里改配置项。"
	for i := 0; i < 8; i++ {
		docs = append(docs, Doc{
			ID: fmt.Sprintf("zhdoc%d", i), Path: fmt.Sprintf("docs/manual-%d.md", i),
			Language: "markdown", Content: zhBody,
		})
	}
	if err := Build(context.Background(), dir, docs); err != nil {
		t.Fatal(err)
	}
	idx, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func rankOf(hits []Hit, id string) int {
	for i, h := range hits {
		if h.ID == id {
			return i + 1
		}
	}
	return -1
}

// TestCJKWrapperDoesNotDrownKey:zh 包装查询下,含 key 的 gold 必须仍在
// 首位(机制 A:查询含 CJK 且含高区分度 Latin token 时,追加 latin 子句)。
func TestCJKWrapperDoesNotDrownKey(t *testing.T) {
	idx := buildCJKIndex(t)
	ctx := context.Background()

	// 前置自证:裸 key 查询 gold 居首(现状已成立的面,不得回退)。
	raw, err := idx.SearchWeighted(ctx, "nacos.core.auth.enabled", 10, DefaultWeights())
	if err != nil {
		t.Fatal(err)
	}
	if rankOf(raw, "gold") != 1 {
		t.Fatalf("裸 key 查询 gold 应居首: %+v", raw)
	}

	// 失败面:zh 包装查询。修复前 gold 被中文文档淹没(不在首位),
	// 修复后必须回到首位。
	wrapped, err := idx.SearchWeighted(ctx, "nacos.core.auth.enabled 配置项在哪里定义", 10, DefaultWeights())
	if err != nil {
		t.Fatal(err)
	}
	if got := rankOf(wrapped, "gold"); got != 1 {
		t.Fatalf("zh 包装查询 gold 应居首(机制 A),实际第 %d 位: %+v", got, wrapped)
	}
}

// TestCJKGuardTriggerBoundary:触发边界——纯中文查询与纯英文查询的行为
// 必须与现状逐位一致(机制 A 不触发)。
func TestCJKGuardTriggerBoundary(t *testing.T) {
	idx := buildCJKIndex(t)
	ctx := context.Background()

	// 纯中文查询(G-zh 面):中文文档为正当答案,不得因机制 A 变化。
	zhOnly, err := idx.SearchWeighted(ctx, "配置项在哪里定义", 10, DefaultWeights())
	if err != nil {
		t.Fatal(err)
	}
	if len(zhOnly) == 0 || zhOnly[0].ID == "gold" {
		t.Fatalf("纯中文查询应由中文文档主导(Han 匹配是信号): %+v", zhOnly)
	}

	// 纯英文查询:无 CJK → 不触发,与默认权重结果逐位一致由子句构造保证;
	// 此处以行为断言兜底(gold 居首)。
	enOnly, err := idx.SearchWeighted(ctx, "nacos auth enabled config", 10, DefaultWeights())
	if err != nil {
		t.Fatal(err)
	}
	if rankOf(enOnly, "gold") != 1 {
		t.Fatalf("纯英文查询 gold 应居首: %+v", enOnly)
	}
}
