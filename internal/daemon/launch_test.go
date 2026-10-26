package daemon

import (
	"context"
	"testing"
	"time"
)

// TestStatusReportsLauncherFromEnv：managed wrapper 写进环境的拉起者信息出现在
// daemon_status 的 launched_by；手动启动（没有该变量）或值无法解析时为空。
func TestStatusReportsLauncherFromEnv(t *testing.T) {
	at := time.Date(2026, 10, 26, 3, 25, 56, 0, time.UTC)
	t.Setenv(EnvLaunchedBy, EncodeLaunchInfo(LaunchInfo{PID: 4242, Cwd: "/work/repo", At: at}))
	status := NewServer(fakeSyncer{}).statusSnapshot(context.Background())
	if status.LaunchedBy == nil || status.LaunchedBy.PID != 4242 || status.LaunchedBy.Cwd != "/work/repo" || !status.LaunchedBy.At.Equal(at) {
		t.Fatalf("launched_by 不对: %+v", status.LaunchedBy)
	}
	for _, value := range []string{"", "not-json", `{"pid":0}`} {
		t.Setenv(EnvLaunchedBy, value)
		if got := NewServer(fakeSyncer{}).statusSnapshot(context.Background()).LaunchedBy; got != nil {
			t.Fatalf("值 %q 不应产生 launched_by: %+v", value, got)
		}
	}
}
