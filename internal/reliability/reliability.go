package reliability

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Class 是 provider 调用失败的类别（决策 11 可行动错误的分类基础，暗坑 K33）。
type Class string

const (
	// ClassRateLimit 上游 429 限流。
	ClassRateLimit Class = "rate-limit"
	// ClassAuth 401/403 认证失败（key 无效或权限不足）。
	ClassAuth Class = "auth"
	// ClassQuota 402 或欠费类失败。
	ClassQuota Class = "quota"
	// ClassTransient 5xx/超时/连接错误，可重试。
	ClassTransient Class = "transient"
	// ClassPermanent 4xx 等不可重试失败（请求本身不被接受）。
	ClassPermanent Class = "permanent"
	// ClassBatchTooLarge 批规模超上游限制，应拆批而非重试（暗坑 K23）。
	ClassBatchTooLarge Class = "batch-too-large"
	// ClassBackoff circuit 处于退避期，调用未发出（D10 no-op 判定依据）。
	ClassBackoff Class = "backoff"
)

// CallError 是分类后的 provider 调用错误；Message 已脱敏（不含 key，
// 单行且长度封顶，暗坑 K21）。
type CallError struct {
	Class      Class
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

// Error 实现 error。
func (e *CallError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("provider %s (HTTP %d): %s", e.Class, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("provider %s: %s", e.Class, e.Message)
}

// Retryable 报告该错误是否值得同一调用内重试。
func (e *CallError) Retryable() bool {
	return e.Class == ClassTransient || e.Class == ClassRateLimit
}

// SanitizeMessage 把任意文本收敛为单行、长度封顶的错误消息。
func SanitizeMessage(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	const maxLen = 512
	if len(text) > maxLen {
		text = text[:maxLen] + "…"
	}
	return text
}

// ParseRetryAfter 解析 Retry-After 头（秒数或 HTTP-date）。
func ParseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}

// 退避参数（受测常数，§14 原则：无真实运维需求不开 env）。
const (
	// maxBackoff 封顶所有退避（含上游 Retry-After 声明）。
	maxBackoff = 5 * time.Minute
	// authBackoff 是认证/欠费失败的长退避：重试无意义，需用户行动（K33）。
	authBackoff = 15 * time.Minute
	// defaultRateLimitBackoff 是无 Retry-After 头时的 429 冷却（对齐 ace client）。
	defaultRateLimitBackoff = 30 * time.Second
	// transientBackoffBase 是暂态失败的 circuit 指数退避基数。
	transientBackoffBase = 30 * time.Second
)

// Circuit 是 daemon 级共享的 provider 健康门（§15）：
// healthy →（失败）backoff(until) →（到期）candidate →（一次成功）healthy。
// 全部构建/查询共享同一实例，不按任务实例化（暗坑 K36）。
type Circuit struct {
	mu                  sync.Mutex
	consecutiveFailures int
	backoffUntil        time.Time
	lastError           string
	lastFailureAt       time.Time
	lastSuccessAt       time.Time
	now                 func() time.Time
}

// NewCircuit 创建健康门。
func NewCircuit() *Circuit {
	return &Circuit{now: time.Now}
}

// Gate 在退避期返回 ClassBackoff 错误（调用不发出），否则放行。
// candidate 状态（退避到期但未证明恢复）放行探测请求。
func (c *Circuit) Gate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if now.Before(c.backoffUntil) {
		return &CallError{
			Class:      ClassBackoff,
			RetryAfter: c.backoffUntil.Sub(now),
			Message:    SanitizeMessage(fmt.Sprintf("provider in backoff until %s after: %s", c.backoffUntil.UTC().Format(time.RFC3339), c.lastError)),
		}
	}
	return nil
}

// RecordSuccess 记录一次成功请求：回到 healthy。
func (c *Circuit) RecordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveFailures = 0
	c.backoffUntil = time.Time{}
	c.lastError = ""
	c.lastSuccessAt = c.now()
}

// RecordFailure 记录一次最终失败（重试耗尽后），按类别设置退避窗口。
// ClassBackoff（未发出的调用）与 ctx 取消不得进入本方法。
func (c *Circuit) RecordFailure(err *CallError) {
	if err == nil || err.Class == ClassBackoff {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveFailures++
	c.lastError = err.Message
	c.lastFailureAt = c.now()

	var wait time.Duration
	switch err.Class {
	case ClassAuth, ClassQuota:
		// 认证/欠费重试无意义，需用户行动：长退避不受通用封顶约束（K33）。
		wait = authBackoff
	case ClassRateLimit:
		wait = err.RetryAfter
		if wait <= 0 {
			wait = defaultRateLimitBackoff
		}
		if wait > maxBackoff {
			wait = maxBackoff
		}
	default:
		// 指数退避：30s、60s、120s……封顶 maxBackoff。
		shift := c.consecutiveFailures - 1
		if shift > 4 {
			shift = 4
		}
		wait = transientBackoffBase << shift
		if wait > maxBackoff {
			wait = maxBackoff
		}
	}
	c.backoffUntil = c.now().Add(wait)
}

// CircuitSnapshot 是对外状态视图（进入 workspace/daemon status）。
type CircuitSnapshot struct {
	// State 是 healthy / backoff / candidate（§15 判定）。
	State               string
	BackoffUntil        time.Time
	LastError           string
	ConsecutiveFailures int
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
}

// Snapshot 生成当前健康视图。
func (c *Circuit) Snapshot() CircuitSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := CircuitSnapshot{
		LastError:           c.lastError,
		ConsecutiveFailures: c.consecutiveFailures,
		LastSuccessAt:       c.lastSuccessAt,
		LastFailureAt:       c.lastFailureAt,
	}
	switch {
	case c.consecutiveFailures == 0:
		snapshot.State = "healthy"
	case c.now().Before(c.backoffUntil):
		snapshot.State = "backoff"
		snapshot.BackoffUntil = c.backoffUntil
	default:
		snapshot.State = "candidate"
	}
	return snapshot
}

// RetryPolicy 是单次调用内的重试策略：只重试 Retryable 类别，
// 429 优先尊重 Retry-After，其余指数退避 + jitter。
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	// Sleep 可注入以便测试；nil 时使用 ctx 感知的计时器。
	Sleep func(context.Context, time.Duration) error
	// Jitter 可注入；nil 时为 ±20% 随机。
	Jitter func(time.Duration) time.Duration
}

// DefaultRetryPolicy 返回默认策略（MaxRetries 来自配置）。
func DefaultRetryPolicy(maxRetries int) RetryPolicy {
	return RetryPolicy{MaxRetries: maxRetries, BaseDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second}
}

// Do 执行 attempt，按策略重试。ctx 错误原样返回（不计为 provider 失败）；
// 非 *CallError 的错误不重试。
func (p RetryPolicy) Do(ctx context.Context, attempt func(context.Context) error) error {
	sleep := p.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	jitter := p.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}
	var lastErr error
	for try := 0; ; try++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = attempt(ctx)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			// 取消优先：不把取消误判为 provider 失败（暗坑 K26）。
			return ctx.Err()
		}
		callErr, ok := lastErr.(*CallError)
		if !ok || !callErr.Retryable() || try >= p.MaxRetries {
			return lastErr
		}
		wait := callErr.RetryAfter
		if wait <= 0 {
			wait = p.BaseDelay << uint(min(try, 10))
			if p.MaxDelay > 0 && wait > p.MaxDelay {
				wait = p.MaxDelay
			}
			wait = jitter(wait)
		} else if wait > maxBackoff {
			wait = maxBackoff
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// ±20%：0.8d + [0, 0.4d)。
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// RateLimiter 是可选的客户端 RPM/TPM 固定分钟窗预算（决策 14：默认不设，
// 仅用户显式配置时启用）。nil 接收者安全（= 不限）。
type RateLimiter struct {
	mu          sync.Mutex
	rpm, tpm    int
	windowStart time.Time
	usedReqs    int
	usedTokens  int
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

// NewRateLimiter 创建 limiter；rpm/tpm 均为 0 时返回 nil（不限）。
func NewRateLimiter(rpm, tpm int) *RateLimiter {
	if rpm <= 0 && tpm <= 0 {
		return nil
	}
	return &RateLimiter{rpm: rpm, tpm: tpm, now: time.Now, sleep: sleepContext}
}

// Acquire 在当前分钟窗内登记 requests/tokens 用量，超预算时阻塞到下一窗口。
// 单次用量本身超过预算时在空窗直接放行（交由上游 429 裁决），避免死锁。
func (l *RateLimiter) Acquire(ctx context.Context, requests, tokens int) error {
	if l == nil {
		return nil
	}
	for {
		l.mu.Lock()
		now := l.now()
		if now.Sub(l.windowStart) >= time.Minute {
			l.windowStart = now
			l.usedReqs = 0
			l.usedTokens = 0
		}
		fitsRPM := l.rpm <= 0 || l.usedReqs+requests <= l.rpm
		fitsTPM := l.tpm <= 0 || l.usedTokens+tokens <= l.tpm
		emptyWindow := l.usedReqs == 0 && l.usedTokens == 0
		if (fitsRPM && fitsTPM) || emptyWindow {
			l.usedReqs += requests
			l.usedTokens += tokens
			l.mu.Unlock()
			return nil
		}
		wait := l.windowStart.Add(time.Minute).Sub(now)
		l.mu.Unlock()
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}
