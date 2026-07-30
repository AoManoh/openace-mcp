package reliability

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// fakeClock 提供可推进的时间源。
type fakeClock struct{ at time.Time }

func (f *fakeClock) now() time.Time            { return f.at }
func (f *fakeClock) advance(d time.Duration)   { f.at = f.at.Add(d) }
func newFakeClock() *fakeClock                 { return &fakeClock{at: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)} }
func newTestCircuit(clock *fakeClock) *Circuit { c := NewCircuit(); c.now = clock.now; return c }

func TestCircuitLifecycle(t *testing.T) {
	clock := newFakeClock()
	c := newTestCircuit(clock)
	if err := c.Gate(); err != nil {
		t.Fatalf("初始应放行: %v", err)
	}
	if s := c.Snapshot(); s.State != "healthy" {
		t.Fatalf("初始应 healthy: %+v", s)
	}

	c.RecordFailure(&CallError{Class: ClassTransient, Message: "503"})
	gateErr := c.Gate()
	callErr := &CallError{}
	if !errors.As(gateErr, &callErr) || callErr.Class != ClassBackoff || callErr.RetryAfter <= 0 {
		t.Fatalf("退避期应返回 ClassBackoff: %v", gateErr)
	}
	if s := c.Snapshot(); s.State != "backoff" || s.BackoffUntil.IsZero() {
		t.Fatalf("应为 backoff 态: %+v", s)
	}

	// 到期只回到 candidate（§15），一次成功才 healthy。
	clock.advance(31 * time.Second)
	if err := c.Gate(); err != nil {
		t.Fatalf("candidate 应放行探测: %v", err)
	}
	if s := c.Snapshot(); s.State != "candidate" {
		t.Fatalf("到期应为 candidate: %+v", s)
	}
	c.RecordSuccess()
	if s := c.Snapshot(); s.State != "healthy" || s.LastError != "" {
		t.Fatalf("成功后应 healthy: %+v", s)
	}
}

func TestCircuitBackoffByClass(t *testing.T) {
	cases := []struct {
		name string
		err  *CallError
		want time.Duration
	}{
		{"429 带 Retry-After", &CallError{Class: ClassRateLimit, RetryAfter: 42 * time.Second}, 42 * time.Second},
		{"429 无头默认 30s", &CallError{Class: ClassRateLimit}, 30 * time.Second},
		{"429 Retry-After 封顶 5min", &CallError{Class: ClassRateLimit, RetryAfter: time.Hour}, 5 * time.Minute},
		{"auth 长退避", &CallError{Class: ClassAuth}, 15 * time.Minute},
		{"quota 长退避", &CallError{Class: ClassQuota}, 15 * time.Minute},
	}
	for _, tc := range cases {
		clock := newFakeClock()
		c := newTestCircuit(clock)
		c.RecordFailure(tc.err)
		if got := c.Snapshot().BackoffUntil.Sub(clock.at); got != tc.want {
			t.Fatalf("%s: 退避 %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestCircuitTransientExponential(t *testing.T) {
	clock := newFakeClock()
	c := newTestCircuit(clock)
	wants := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, want := range wants {
		c.RecordFailure(&CallError{Class: ClassTransient, Message: "boom"})
		got := c.Snapshot().BackoffUntil.Sub(clock.at)
		if got != want {
			t.Fatalf("第 %d 次失败退避 %v，期望 %v", i+1, got, want)
		}
		clock.advance(got + time.Second)
	}
}

func TestCircuitIgnoresBackoffClassAndNil(t *testing.T) {
	c := newTestCircuit(newFakeClock())
	c.RecordFailure(nil)
	c.RecordFailure(&CallError{Class: ClassBackoff})
	if s := c.Snapshot(); s.State != "healthy" {
		t.Fatalf("gate 拒绝不应计为失败: %+v", s)
	}
}

func TestRetryPolicyRetriesTransientAndHonorsRetryAfter(t *testing.T) {
	var slept []time.Duration
	policy := RetryPolicy{
		MaxRetries: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: time.Second,
		Sleep:  func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		Jitter: func(d time.Duration) time.Duration { return d },
	}
	attempts := 0
	err := policy.Do(context.Background(), func(context.Context) error {
		attempts++
		if attempts == 1 {
			return &CallError{Class: ClassRateLimit, RetryAfter: 42 * time.Millisecond}
		}
		if attempts == 2 {
			return &CallError{Class: ClassTransient}
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("应第三次成功: attempts=%d err=%v", attempts, err)
	}
	if len(slept) != 2 || slept[0] != 42*time.Millisecond || slept[1] != 20*time.Millisecond {
		t.Fatalf("等待序列不符（Retry-After 优先，其后指数）: %v", slept)
	}
}

func TestRetryPolicyStopsOnNonRetryable(t *testing.T) {
	policy := DefaultRetryPolicy(5)
	policy.Sleep = func(context.Context, time.Duration) error { return nil }
	for _, class := range []Class{ClassAuth, ClassQuota, ClassPermanent, ClassBatchTooLarge} {
		attempts := 0
		err := policy.Do(context.Background(), func(context.Context) error {
			attempts++
			return &CallError{Class: class}
		})
		callErr := &CallError{}
		if !errors.As(err, &callErr) || callErr.Class != class || attempts != 1 {
			t.Fatalf("%s 不应重试: attempts=%d err=%v", class, attempts, err)
		}
	}
	// 非 CallError 同样不重试。
	attempts := 0
	plain := errors.New("plain")
	if err := policy.Do(context.Background(), func(context.Context) error { attempts++; return plain }); err != plain || attempts != 1 {
		t.Fatalf("普通错误不应重试: attempts=%d err=%v", attempts, err)
	}
}

func TestRetryPolicyExhaustsAndReturnsLastError(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond,
		Sleep: func(context.Context, time.Duration) error { return nil }}
	attempts := 0
	err := policy.Do(context.Background(), func(context.Context) error {
		attempts++
		return &CallError{Class: ClassTransient, Message: "still down"}
	})
	callErr := &CallError{}
	if !errors.As(err, &callErr) || attempts != 3 {
		t.Fatalf("2 次重试 = 3 次尝试: attempts=%d err=%v", attempts, err)
	}
}

func TestRetryPolicyReturnsContextErrorOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	policy := RetryPolicy{MaxRetries: 5, BaseDelay: time.Millisecond,
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	attempts := 0
	err := policy.Do(ctx, func(context.Context) error {
		attempts++
		cancel()
		return &CallError{Class: ClassTransient}
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("取消应原样返回 ctx 错误（不误判 provider 失败，K26）: attempts=%d err=%v", attempts, err)
	}
}

func TestRateLimiterNilSafe(t *testing.T) {
	var l *RateLimiter
	if err := l.Acquire(context.Background(), 1, 1000); err != nil {
		t.Fatalf("nil limiter 应不限: %v", err)
	}
	if NewRateLimiter(0, 0) != nil {
		t.Fatalf("双 0 预算应返回 nil")
	}
}

func TestRateLimiterBlocksOverBudget(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(2, 0)
	l.now = clock.now
	slept := 0
	l.sleep = func(ctx context.Context, d time.Duration) error {
		slept++
		clock.advance(d) // 模拟等到窗口结束
		return ctx.Err()
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := l.Acquire(ctx, 1, 100); err != nil {
			t.Fatalf("窗口内应放行: %v", err)
		}
	}
	if err := l.Acquire(ctx, 1, 100); err != nil || slept != 1 {
		t.Fatalf("第三次应等待下一窗口后放行: slept=%d err=%v", slept, err)
	}
}

func TestRateLimiterOversizedBurstAllowedInEmptyWindow(t *testing.T) {
	clock := newFakeClock()
	l := NewRateLimiter(0, 1000)
	l.now = clock.now
	l.sleep = func(ctx context.Context, d time.Duration) error { clock.advance(d); return ctx.Err() }
	// 单次超预算：空窗直接放行（交由上游 429 裁决），避免死锁。
	if err := l.Acquire(context.Background(), 1, 5000); err != nil {
		t.Fatalf("空窗超额单次应放行: %v", err)
	}
	// 非空窗则等待。
	if err := l.Acquire(context.Background(), 1, 5000); err != nil {
		t.Fatalf("等待新窗口后应放行: %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := ParseRetryAfter("17"); d != 17*time.Second {
		t.Fatalf("秒数解析: %v", d)
	}
	if d := ParseRetryAfter(""); d != 0 {
		t.Fatalf("空头应为 0: %v", d)
	}
	if d := ParseRetryAfter("-3"); d != 0 {
		t.Fatalf("负数应为 0: %v", d)
	}
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if d := ParseRetryAfter(future); d < 25*time.Second || d > 31*time.Second {
		t.Fatalf("HTTP-date 解析: %v", d)
	}
}

func TestSanitizeMessage(t *testing.T) {
	multi := "line1\nline2\t  spaced"
	if got := SanitizeMessage(multi); got != "line1 line2 spaced" {
		t.Fatalf("应收敛为单行: %q", got)
	}
	long := make([]byte, 2000)
	for i := range long {
		long[i] = 'a'
	}
	if got := SanitizeMessage(string(long)); len(got) > 520 {
		t.Fatalf("长度应封顶: %d", len(got))
	}
}
