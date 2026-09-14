package reliability

import (
	"context"
	"testing"
	"time"
)

func TestGovernorSupplyDropDoesNotRejectLargerWindow(t *testing.T) {
	g, clock := governed(16)
	g.fine = true
	completeGroup(t, g, clock, time.Second)
	verifiedRate := g.verifiedRate
	long := mustAcquire(t, g, 100)
	short := acquireGroup(t, g, 17)
	_ = clock.sleep(context.Background(), time.Second)
	for _, p := range short {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	// 服务没有变慢；后续只有四个输入，空闲的请求名额不能用于推断服务容量。
	for i := 0; i < 2; i++ {
		short = acquireGroup(t, g, 4)
		_ = clock.sleep(context.Background(), time.Second)
		for _, p := range short {
			g.Observe(p, OutcomeSuccess, time.Second, 0)
		}
	}
	g.Observe(long, OutcomeSuccess, 3*time.Second, 0)
	if s := g.Snapshot(); s.Window != 18 || s.LastAdjustment != "window-underutilized" || g.verifiedRate != verifiedRate || g.verifiedWindow != 16 {
		t.Fatalf("输入减少改变了既有容量判断: snapshot=%+v verifiedWindow=%d verifiedRate=%f", s, g.verifiedWindow, g.verifiedRate)
	}
	assertGovernorDrained(t, g)
	// 供给恢复后仍应继续探索，不能让被排除的样本把后续扩张永久关闭。
	completeGroup(t, g, clock, time.Second)
	if s := g.Snapshot(); s.Window != 20 || s.LastAdjustment != "throughput-probe" {
		t.Fatalf("供给恢复后没有继续比较成功吞吐: %+v", s)
	}
	assertGovernorDrained(t, g)
}

func TestGovernorBriefRefillGapStillMeasuresThroughput(t *testing.T) {
	g, clock := governed(16)
	g.fine = true
	completeGroup(t, g, clock, time.Second)
	ps := acquireGroup(t, g, 18)
	_ = clock.sleep(context.Background(), time.Second)
	for _, p := range ps[:17] {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	// 末批多用10ms，不应把整秒的持续占用丢弃；这种时序仍有实际吞吐增益。
	_ = clock.sleep(context.Background(), 10*time.Millisecond)
	g.Observe(ps[17], OutcomeSuccess, 1010*time.Millisecond, 0)
	if s := g.Snapshot(); s.Window != 20 || s.LastAdjustment != "throughput-probe" {
		t.Fatalf("短暂补充间隔使有效样本被排除: %+v", s)
	}
	assertGovernorDrained(t, g)
}

func TestGovernorSupplyDropKeepsMeasuredThroughputGain(t *testing.T) {
	g, clock := governed(16)
	g.fine = true
	completeGroup(t, g, clock, 2*time.Second)
	long := mustAcquire(t, g, 100)
	short := acquireGroup(t, g, 17)
	_ = clock.sleep(context.Background(), time.Second)
	for _, p := range short {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	for i := 0; i < 2; i++ {
		short = acquireGroup(t, g, 4)
		_ = clock.sleep(context.Background(), time.Second)
		for _, p := range short {
			g.Observe(p, OutcomeSuccess, time.Second, 0)
		}
	}
	g.Observe(long, OutcomeSuccess, 3*time.Second, 0)
	// 实际完成速率从16/2提高到26/3，即使后半段输入减少，增益已经发生。
	// 排除低吞吐的反证，不能同时丢弃这组已完成请求给出的正向证据。
	if s := g.Snapshot(); s.Window != 20 || s.LastAdjustment != "throughput-probe" {
		t.Fatalf("输入减少使已测得的吞吐增益被丢弃: %+v", s)
	}
	assertGovernorDrained(t, g)
}

func TestGovernorSupplyDropDoesNotSuppressOverload(t *testing.T) {
	g, clock := governed(16)
	g.fine = true
	completeGroup(t, g, clock, time.Second)
	ps := acquireGroup(t, g, 18)
	_ = clock.sleep(context.Background(), time.Second)
	for _, p := range ps[:10] {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	_ = clock.sleep(context.Background(), 3*time.Second)
	g.Observe(ps[10], OutcomeOverload, 4*time.Second, 0)
	g.Observe(ps[11], OutcomeOverload, 4*time.Second, 0)
	if s := g.Snapshot(); s.Window != 9 || s.LastAdjustment != "overload" {
		t.Fatalf("占用不足不能隐藏实际过载: %+v", s)
	}
	for _, p := range ps[12:] {
		g.Observe(p, OutcomeOther, 0, 0)
	}
	assertGovernorDrained(t, g)
}

func pendingUnderfilledSample(t *testing.T) (*Governor, *governorClock, []IndexPermit) {
	t.Helper()
	g, clock := governed(16)
	g.fine = true
	completeGroup(t, g, clock, time.Second)
	ps := acquireGroup(t, g, 18)
	_ = clock.sleep(context.Background(), 100*time.Millisecond)
	for _, p := range ps[:3] {
		g.Observe(p, OutcomeSuccess, 100*time.Millisecond, 0)
	}
	_ = clock.sleep(context.Background(), 800*time.Millisecond)
	pending := acquireGroup(t, g, 3)
	_ = clock.sleep(context.Background(), 300*time.Millisecond)
	for _, p := range ps[3:] {
		g.Observe(p, OutcomeSuccess, 1200*time.Millisecond, 0)
	}
	if s := g.Snapshot(); s.Window != 18 || s.LastAdjustment != "throughput-inconclusive" {
		t.Fatalf("欠用期间已有的组外请求仍可能改变判断，应先结算: %+v", s)
	}
	return g, clock, pending
}

func TestGovernorUnderfilledSettlementKeepsLateSuccess(t *testing.T) {
	g, _, pending := pendingUnderfilledSample(t)
	for _, p := range pending {
		g.Observe(p, OutcomeSuccess, 300*time.Millisecond, 0)
	}
	if s := g.Snapshot(); s.Window != 20 || s.LastAdjustment != "throughput-probe" {
		t.Fatalf("欠用样本丢失了迟到成功带来的实际吞吐增益: %+v", s)
	}
	assertGovernorDrained(t, g)
}

func TestGovernorUnderfilledSettlementKeepsLateOverload(t *testing.T) {
	g, _, pending := pendingUnderfilledSample(t)
	for _, p := range pending {
		g.Observe(p, OutcomeOverload, 300*time.Millisecond, 0)
	}
	if s := g.Snapshot(); s.Window != 9 || s.LastAdjustment != "overload" {
		t.Fatalf("欠用样本提前清空，导致连续组外过载没有缩窗: %+v", s)
	}
	assertGovernorDrained(t, g)
}
