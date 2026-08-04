package daemon

import (
	"testing"
	"time"
)

// L13 回归(诊断 2026-08-03):shutdown/abandoned 终态任务的剪枝保护有
// 7 天时效——此前永久保护导致 tasks.json 跨重启缓慢累积。
func TestPrunableTaskAgeCap(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	recent := now.Add(-time.Hour)

	cases := []struct {
		name string
		snap TaskSnapshot
		want bool
	}{
		{"普通终态任务立即可剪", TaskSnapshot{State: TaskStateCompleted, SubmittedAt: recent}, true},
		{"shutdown 新鲜期受保护", TaskSnapshot{State: TaskStateFailed, Error: "shutdown", SubmittedAt: recent, CompletedAt: &recent}, false},
		{"shutdown 过 7 天可剪", TaskSnapshot{State: TaskStateFailed, Error: "shutdown", SubmittedAt: old, CompletedAt: &old}, true},
		{"abandoned 过 7 天可剪", TaskSnapshot{State: TaskStateFailed, Error: "abandoned after daemon restart", SubmittedAt: old}, true},
		{"运行中永不剪", TaskSnapshot{State: TaskStateRunning, SubmittedAt: old}, false},
	}
	for _, tc := range cases {
		if got := isPrunableTask(tc.snap); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
