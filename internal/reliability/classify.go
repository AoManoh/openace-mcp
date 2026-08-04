package reliability

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ClassifyTransportError 分类传输层错误：调用方取消原样返回（不计 provider
// 失败，暗坑 K26）；单次尝试超时与连接失败为 transient，并附可行动提示（K33）。
func ClassifyTransportError(callerCtx context.Context, attemptCtx context.Context, timeout time.Duration, err error) error {
	if callerCtx.Err() != nil {
		return callerCtx.Err()
	}
	if attemptCtx.Err() != nil {
		return &CallError{
			Class:   ClassTransient,
			Message: fmt.Sprintf("request timed out after %s (raise %s for slow endpoints)", timeout, EnvProviderTimeout),
		}
	}
	message := SanitizeMessage(err.Error())
	// 证书验证类错误重试无意义(L8,诊断 2026-08-03):判 permanent 让
	// 调用方立刻拿到可行动错误,而非 5 次重试+退避后才浮现。类型匹配
	// 优先,substring 兜底(经 url.Error 包装且类型信息丢失的场景)。
	if isCertificateError(err) || strings.Contains(message, "x509:") {
		return &CallError{Class: ClassPermanent, Message: message + " (certificate verification failed: check the endpoint TLS certificate or local trust store)"}
	}
	if strings.Contains(message, "connection refused") || strings.Contains(message, "no such host") {
		message += " (endpoint unreachable: is the server running and the base URL correct?)"
	}
	return &CallError{Class: ClassTransient, Message: message}
}

// isCertificateError 识别 x509 验证失败家族(errors.As 穿透包装)。
func isCertificateError(err error) bool {
	var (
		unknownAuthority x509.UnknownAuthorityError
		certInvalid      x509.CertificateInvalidError
		hostname         x509.HostnameError
		systemRoots      x509.SystemRootsError
	)
	return errors.As(err, &unknownAuthority) || errors.As(err, &certInvalid) ||
		errors.As(err, &hostname) || errors.As(err, &systemRoots)
}

// ClassifyHTTPResponse 把非 200 响应分类为可行动错误（暗坑 K33）；
// 读取 ≤2KB 响应体作为脱敏消息。
func ClassifyHTTPResponse(resp *http.Response) *CallError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	body := SanitizeMessage(string(raw))
	lower := strings.ToLower(body)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &CallError{
			Class:      ClassRateLimit,
			StatusCode: resp.StatusCode,
			RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After")),
			Message:    SanitizeMessage("rate limited by provider: " + body),
		}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if containsAny(lower, "balance", "billing", "payment", "quota") {
			return quotaError(resp.StatusCode, body)
		}
		return &CallError{
			Class:      ClassAuth,
			StatusCode: resp.StatusCode,
			Message:    SanitizeMessage("authentication failed (check the configured API key): " + body),
		}
	case resp.StatusCode == http.StatusPaymentRequired:
		return quotaError(resp.StatusCode, body)
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return &CallError{Class: ClassBatchTooLarge, StatusCode: resp.StatusCode, Message: body}
	case resp.StatusCode == http.StatusBadRequest && strings.Contains(lower, "token") && containsAny(lower, "max", "limit", "exceed", "too"):
		return &CallError{Class: ClassBatchTooLarge, StatusCode: resp.StatusCode, Message: body}
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		return &CallError{Class: ClassTransient, StatusCode: resp.StatusCode, Message: body}
	default:
		return &CallError{Class: ClassPermanent, StatusCode: resp.StatusCode, Message: body}
	}
}

func quotaError(status int, body string) *CallError {
	return &CallError{
		Class:      ClassQuota,
		StatusCode: status,
		Message:    SanitizeMessage("provider quota/billing failure (top up or adjust provider dashboard budget): " + body),
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
