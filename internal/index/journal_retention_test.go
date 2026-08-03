package index

import "testing"

// M8 回归(诊断报告 2026-08-03):压实保留集必须同时受条数与字节上限
// 约束——字节超限时保留环要能实际收缩,防"触发压实却一条不删、每次
// 打开全量重写"。
func TestJournalRetentionByteCap(t *testing.T) {
	order := []string{"a", "b", "c", "d"} // 旧→新
	sizes := map[string]int64{"a": 100, "b": 100, "c": 100, "d": 100}

	// 条数上限生效:保留最新 2 条。
	keep := journalRetention(order, sizes, 2, 1<<30)
	if len(keep) != 2 || !keep["d"] || !keep["c"] {
		t.Fatalf("条数上限应保留最新 2 条: %v", keep)
	}
	// 字节上限生效:250 字节只装得下最新 2 条。
	keep = journalRetention(order, sizes, 10, 250)
	if len(keep) != 2 || !keep["d"] || !keep["c"] {
		t.Fatalf("字节上限应保留最新 2 条: %v", keep)
	}
	// 单条即超限:保留空集(而非死循环/全保留)。
	keep = journalRetention(order, sizes, 10, 50)
	if len(keep) != 0 {
		t.Fatalf("单条超限应得空保留集: %v", keep)
	}
}
