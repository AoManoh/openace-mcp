package reliability

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Class 是 provider 调用失败的类别。重试策略（RetryPolicy）据此决定是否
// 再试，熔断器（Circuit，连续失败后暂停向 provider 发请求）据此决定
// 暂停多久，错误文本据此告诉用户下一步该做什么，而不是只报"请求失败"。
type Class string

const (
	// ClassRateLimit：上游返回 429，单位时间内请求数或 token 数超过配额。
	// 可重试，等待时长优先取上游的 Retry-After。
	ClassRateLimit Class = "rate-limit"
	// ClassAuth：上游返回 401 或 403，key 无效或权限不足。重试结果不变，
	// 需要用户更换 key。
	ClassAuth Class = "auth"
	// ClassQuota：上游返回 402，或 401/403 的正文写明余额、账单问题。
	// 重试结果不变，需要用户充值或调整 provider 侧预算。
	ClassQuota Class = "quota"
	// ClassTransient：5xx、408、单次尝试超时、连接失败等短暂故障。可重试。
	ClassTransient Class = "transient"
	// ClassPermanent：请求本身不被接受，例如其余 4xx、证书验证失败、
	// 响应内容通不过校验。重试结果不变，也不拆批。
	ClassPermanent Class = "permanent"
	// ClassBatchTooLarge：一批文本超过上游单批上限（413，或 Voyage 对
	// token 超限返回的 400）。正确处理是把批次对半拆开重发，原样重试
	// 仍会超限。
	ClassBatchTooLarge Class = "batch-too-large"
	// ClassBackoff：熔断器处于退避期（连续失败后暂停发请求的时段），请求
	// 没有发出，本次没有消耗 provider 配额。构建流程收到该类别后停止投放
	// 后续批，等退避到期再补齐缺失的向量。
	ClassBackoff Class = "backoff"
)

// CallError 是归类后的 provider 调用错误。Message 是单行、限长的文本：
// 取自 provider 响应正文或底层错误的部分先经 SanitizeMessage 处理（正文
// 最长 512 字节），其余为固定格式。本包拼接的固定文本不含 API key（key
// 只出现在请求头构造处）；provider 响应正文的截取部分原样进入 Message，
// 本包不检查其中是否回显了凭据。Message 进入日志、workspace_status 和给
// 用户的错误。
type CallError struct {
	Class      Class
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

// Error 返回 "provider <类别> (HTTP <状态码>): <消息>"。没有状态码的
// 错误（传输失败、退避期拒绝）省略括号部分。
func (e *CallError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("provider %s (HTTP %d): %s", e.Class, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("provider %s: %s", e.Class, e.Message)
}

// Retryable 报告该错误是否值得在同一次调用内重试。只有 ClassTransient
// 与 ClassRateLimit 返回真。ClassAuth、ClassQuota、ClassPermanent 重试
// 结果不变。ClassBatchTooLarge 由调用方拆批而不是原样重试。ClassBackoff
// 表示请求没有发出，无从重试。
func (e *CallError) Retryable() bool {
	return e.Class == ClassTransient || e.Class == ClassRateLimit
}

// SanitizeMessage 把任意文本压成一条单行错误消息：连续空白（含换行、
// 制表符）合并为一个空格。正文超过 512 字节时截到 512 字节以内并追加
// "…"。截断点回退到 UTF-8 字符的起始字节。原因：按字节数硬切会把一个
// 多字节字符切成非法序列，消息不再是合法 UTF-8，日志和 workspace_status
// 的 JSON 输出里出现乱码。
func SanitizeMessage(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	const maxLen = 512
	if len(text) > maxLen {
		cut := maxLen
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// ParseRetryAfter 解析 Retry-After 头的两种合法写法：非负整数秒数，或
// HTTP-date 绝对时刻（取从现在到该时刻的剩余时长）。头为空、负数、
// 格式不合法或时刻已过去时都返回 0，调用方按"上游没有声明"处理，改用
// 自己的默认等待时长。
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

// 熔断器的退避时长常数。它们是经测试固定下来的值，没有对应环境变量：
// 项目约定并发、超时、批次这类参数先用测试过的常量，只有出现真实运维
// 需求时才增加环境变量。
const (
	// maxBackoff 是除 ClassAuth、ClassQuota 之外各类退避的上限，上游
	// Retry-After 声明的时长（熔断器退避与 RetryPolicy.Do 的等待）也受它
	// 约束。这样即使上游声明很长的等待，探测请求也最迟 5 分钟后发出
	// 一次，恢复不会被无限推后。
	maxBackoff = 5 * time.Minute
	// authBackoff 是 ClassAuth 与 ClassQuota 失败后的退避时长。这两类
	// 失败在用户换 key 或充值之前重试结果不变，所以退避远长于 maxBackoff，
	// 避免每隔几分钟就向上游发一次注定失败的请求。
	authBackoff = 15 * time.Minute
	// defaultRateLimitBackoff 是 429 没有携带 Retry-After 时的退避时长。
	// 吞吐治理器（Governor）同场景的 governorPauseFallback 取同一值。
	defaultRateLimitBackoff = 30 * time.Second
	// transientBackoffBase 是其他类别失败的指数退避起点：第 1 次连续失败
	// 等 30s，之后每次翻倍，最多到 maxBackoff。
	transientBackoffBase = 30 * time.Second
)

// Circuit 是熔断器：记录一条 provider 调用路径最近连续失败的次数，在
// 退避期内拒绝发出新请求。每个实例都挂在 daemon 级客户端单例上，由全部
// workspace 的构建与查询共用，不按任务新建：embedding 客户端默认为索引批
// 请求和查询请求各持一个熔断器（OPENACE_THROUGHPUT_GOVERNOR=off 时两类
// 请求共用一个），rerank 客户端持一个。按任务各建一个的话，并发任务会
// 同时重试，请求量成倍放大。状态有三种：
//
//   - healthy：没有未恢复的失败，Gate 直接放行。
//   - backoff：最近一次最终失败之后、backoffUntil 之前，Gate 拒绝请求。
//   - candidate：退避到期但还没有一次成功请求证明恢复。Gate 放行请求
//     作为探测。一次成功回到 healthy。再失败则连续失败次数加一，进入
//     更长的退避。
type Circuit struct {
	mu                  sync.Mutex
	consecutiveFailures int
	backoffUntil        time.Time
	lastError           string
	lastFailureAt       time.Time
	lastSuccessAt       time.Time
	now                 func() time.Time
}

// NewCircuit 创建一个处于 healthy 状态、使用系统时钟的熔断器。
func NewCircuit() *Circuit {
	return &Circuit{now: time.Now}
}

// Gate 在请求发出前调用。退避期内返回 ClassBackoff 的 CallError，其
// RetryAfter 为距退避结束的时长，Message 带上次失败原因，调用方据此不发
// 请求。healthy 与 candidate 状态返回 nil 放行。candidate 也放行的原因：
// 熔断器只能靠一次真实请求确认 provider 已恢复，退避到期后放行的第一批
// 请求就是这次探测。
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

// RecordSuccess 记录一次成功请求：清零连续失败计数、退避截止时刻与上次
// 错误文本，状态回到 healthy。
func (c *Circuit) RecordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveFailures = 0
	c.backoffUntil = time.Time{}
	c.lastError = ""
	c.lastSuccessAt = c.now()
}

// RecordFailure 记录一次最终失败（RetryPolicy 的重试已用完，或错误不可
// 重试），连续失败计数加一，并按类别设置退避截止时刻：
//
//   - ClassAuth、ClassQuota：固定等 authBackoff（15 分钟），不受 maxBackoff
//     约束。原因：换 key 或充值之前重试结果不变。
//   - ClassRateLimit：等上游 Retry-After 声明的时长。没有声明时等
//     defaultRateLimitBackoff（30s）。超过 maxBackoff 时按 maxBackoff。
//   - 其他类别：指数退避，第 n 次连续失败等 transientBackoffBase 的
//     2^(n-1) 倍，即 30s、60s、120s、240s，之后按 maxBackoff（5 分钟）。
//
// err 为 nil 或类别为 ClassBackoff 时不记录：ClassBackoff 表示请求没有
// 发出，不是 provider 的一次失败。调用方取消返回的是 ctx 错误而不是
// CallError，调用方不应把它传进来。
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
		// 换 key 或充值之前重试结果不变，所以这里不套 maxBackoff。
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
		// shift 最多取 4：30s 左移 4 位是 480s，已经超过 maxBackoff，会被
		// 下面封顶。不限制位移的话，连续失败几十次后左移会让 Duration
		// 溢出成负数。
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

// CircuitSnapshot 是熔断器某一时刻的只读状态。它进入 workspace_status
// 的 semantic 块，让用户看到 provider 现在是否可用、多久后恢复、上次为
// 什么失败。构建流程也据此判断熔断器是否处于 backoff，处于 backoff 时
// 内容未变的同步不再尝试补齐向量。
type CircuitSnapshot struct {
	// State 是 healthy、backoff、candidate 之一，含义见 Circuit。
	State               string
	BackoffUntil        time.Time
	LastError           string
	ConsecutiveFailures int
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
}

// Snapshot 返回当前状态。State 按顺序判定：连续失败计数为 0 是 healthy。
// 否则当前时刻早于退避截止是 backoff，并填 BackoffUntil。否则是
// candidate。
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

// RetryPolicy 是单次 provider 调用内的重试策略：只对 Retryable 为真的
// CallError（ClassTransient、ClassRateLimit）重试。上游给了 Retry-After
// 就等它声明的时长。否则从 BaseDelay 起每次翻倍、不超过 MaxDelay，再加
// 随机抖动，避免多个 goroutine 同时失败后又在同一时刻重试。
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	// Sleep 是等待函数，测试注入它以免真实等待。nil 时使用 sleepContext：
	// 等待中 ctx 结束即提前返回 ctx 错误。
	Sleep func(context.Context, time.Duration) error
	// Jitter 给等待时长加随机抖动，测试注入恒等函数以得到确定的等待
	// 序列。nil 时使用 defaultJitter（±20%）。
	Jitter func(time.Duration) time.Duration
}

// DefaultRetryPolicy 返回默认策略：BaseDelay 500ms、MaxDelay 30s。
// maxRetries 由调用方从 OPENACE_PROVIDER_MAX_RETRIES 读取后传入。
func DefaultRetryPolicy(maxRetries int) RetryPolicy {
	return RetryPolicy{MaxRetries: maxRetries, BaseDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second}
}

// Do 反复执行 attempt，直到成功、错误不可重试或重试次数用完，失败时返回
// 最后一次的错误。attempt 最多执行 MaxRetries+1 次。各分支：
//
//   - 每次执行前检查 ctx，attempt 失败后再检查一次：ctx 已结束就原样
//     返回 ctx 错误，不返回 attempt 的错误。原因：取消不是 provider
//     故障，返回 CallError 会让调用方把它计入熔断器。
//   - 错误不是 CallError（用 errors.As 判定，调用方以 %w 包装也能识别）、
//     或 Retryable 为假、或已重试 MaxRetries 次：返回该错误。
//   - 否则等待后再试。CallError.RetryAfter 大于 0 时等它，上限
//     maxBackoff，不加抖动。等于 0 时等 BaseDelay 左移 try 位（try 最多
//     按 10 计，防止位移溢出），不超过 MaxDelay，再加抖动。
//   - 等待期间 ctx 结束：返回 ctx 错误。
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
			// attempt 失败时 ctx 已结束，以取消为准：这次失败很可能就是
			// 取消关闭连接造成的，不是 provider 故障。
			return ctx.Err()
		}
		// 用 errors.As 而不是类型断言：调用方若以 %w 包装 CallError，类型
		// 断言会失败，重试被静默关闭且没有任何提示。
		callErr := &CallError{}
		if !errors.As(lastErr, &callErr) || !callErr.Retryable() || try >= p.MaxRetries {
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
	// 结果落在 [0.8d, 1.2d)，即 ±20% 的随机抖动。
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// RateLimiter 按固定的一分钟窗口限制请求数（rpm）与 token 数（tpm）。
// 默认不创建：只有用户显式设置 OPENACE_EMBEDDING_RPM_BUDGET 或
// OPENACE_EMBEDDING_TPM_BUDGET 时 embedding 客户端才持有一个，作为用户
// 给自己定的硬上限，与 provider 实际配额无关。nil 接收者的方法直接放行，
// 调用方不必判 nil。
type RateLimiter struct {
	mu          sync.Mutex
	rpm, tpm    int
	windowStart time.Time
	usedReqs    int
	usedTokens  int
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

// NewRateLimiter 创建限速器。rpm 与 tpm 都不大于 0 时返回 nil，表示不限速。
// 只设其中一项时，另一项不参与判定。
func NewRateLimiter(rpm, tpm int) *RateLimiter {
	if rpm <= 0 && tpm <= 0 {
		return nil
	}
	return &RateLimiter{rpm: rpm, tpm: tpm, now: time.Now, sleep: sleepContext}
}

// Acquire 在当前一分钟窗口内登记 requests 个请求与 tokens 个 token。
// 窗口从重置时刻起满一分钟即清零计数。加上本次后请求数或 token 数超出
// 预算时，阻塞到窗口结束再重新判定，ctx 结束则返回 ctx 错误。例外：
// 窗口内还没有任何用量，而本次单笔就超过预算时直接放行，由上游用 429
// 裁决。原因：单笔需求大于预算时，任何一个空窗口都装不下它，不放行的
// 话该请求会一直等待。
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
