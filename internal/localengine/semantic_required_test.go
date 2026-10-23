package localengine

import (
	"context"
	"strings"
	"testing"
)

// TestEmptySemanticIndexErrorWithoutBuildInProgress：provider 全部失败后发布的
// 零向量 revision 被查询时返回错误，写明没有构建在进行并带出索引嵌入熔断器
// 状态，不写全为 0 的构建进度；查询嵌入在判定之前不发出。
func TestEmptySemanticIndexErrorWithoutBuildInProgress(t *testing.T) {
	server := newEmbedServer(t, 8)
	server.setFailWhen(func([]string) bool { return true })
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err == nil || !strings.Contains(err.Error(), semanticIndexEmptyReason) {
		t.Fatalf("前置：零向量 revision 的同步应报告没有向量: %v", err)
	}
	before := server.callCount()
	_, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err == nil {
		t.Fatal("没有向量的 revision 不得以纯词法结果作答")
	}
	message := err.Error()
	for _, want := range []string{semanticIndexEmptyReason, "no build in progress", "embedding provider state=backoff"} {
		if !strings.Contains(message, want) {
			t.Fatalf("错误应包含 %q: %v", want, err)
		}
	}
	if strings.Contains(message, "build progress:") {
		t.Fatalf("没有构建在进行时不应写构建进度: %v", err)
	}
	if n := len(queryTexts(server, before)); n != 0 {
		t.Fatalf("没有向量时不应发出查询嵌入，实际 %d 次", n)
	}
}
