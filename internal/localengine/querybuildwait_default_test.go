package localengine

import (
	"os"
	"testing"
	"time"
)

// P2(灰度反馈):查询有界等待默认开启——冷仓首建期间的同步检索在
// 工具超时(110s)之前拿到带进度的可行动错误,而非裸传输超时。
// 默认 90s;显式 "0" 保留"等到构建完成"的历史行为。
func TestQueryBuildWaitDefaultBounded(t *testing.T) {
	os.Unsetenv("OPENACE_QUERY_BUILD_WAIT")
	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if opts.QueryBuildWait != 90*time.Second {
		t.Fatalf("默认 QueryBuildWait 应为 90s(P2 灰度修复),得到 %v", opts.QueryBuildWait)
	}
	t.Setenv("OPENACE_QUERY_BUILD_WAIT", "0")
	opts, err = OptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if opts.QueryBuildWait != 0 {
		t.Fatalf("显式 0 应保留无界等待,得到 %v", opts.QueryBuildWait)
	}
	t.Setenv("OPENACE_QUERY_BUILD_WAIT", "30s")
	opts, err = OptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if opts.QueryBuildWait != 30*time.Second {
		t.Fatalf("显式值应生效,得到 %v", opts.QueryBuildWait)
	}
}
