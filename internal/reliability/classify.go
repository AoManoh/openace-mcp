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

// ClassifyTransportError 把一次 HTTP 传输失败（请求没有拿到响应）归类，
// 供重试和熔断（连续失败后暂停向 provider 发请求）逻辑决定下一步：
//
//   - 调用方的 callerCtx 已结束（调用方取消，或调用方自己的截止时间
//     到了）：原样返回 callerCtx.Err()，不包装成 CallError。原因：
//     取消不是 provider 故障。如果算作一次失败，熔断器会把一个健康的
//     provider 判成故障并暂停向它发请求，同一路径上的其他请求跟着被
//     拒绝。
//   - 单次尝试超时（attemptCtx 已结束）：归为 ClassTransient（可重试）。
//     错误文本写明等待了多久，并提示端点慢时调大 EnvProviderTimeout。
//   - 证书验证失败（isCertificateError 识别的 x509 错误，或错误文本含
//     "x509:"）：归为 ClassPermanent。错误文本提示检查端点 TLS 证书或
//     本机信任库。原因：证书错误重试结果不变。此前归为 ClassTransient
//     时，用户要等完默认 5 次重试和退避（重试之间的等待）才看到真正
//     原因。
//   - 连接被拒绝、主机名不存在：归为 ClassTransient。错误文本附上
//     "服务是否在运行、base URL 是否正确"的提示。
//   - 其他传输错误：归为 ClassTransient，错误文本为底层错误的原始消息。
//
// 取自底层错误的文本先经 SanitizeMessage 压成单行并限长（正文最长
// 512 字节），再进入 CallError.Message。
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
	// 先按错误类型识别证书错误，再检查错误文本是否含 "x509:"。文本检查
	// 覆盖错误链已被转成纯文本、类型信息丢失的情况，此时 errors.As 找
	// 不到 x509 类型，只剩消息可判。
	if isCertificateError(err) || strings.Contains(message, "x509:") {
		return &CallError{Class: ClassPermanent, Message: message + " (certificate verification failed: check the endpoint TLS certificate or local trust store)"}
	}
	if strings.Contains(message, "connection refused") || strings.Contains(message, "no such host") {
		message += " (endpoint unreachable: is the server running and the base URL correct?)"
	}
	return &CallError{Class: ClassTransient, Message: message}
}

// isCertificateError 报告 err 的错误链里是否有 crypto/x509 的四类证书
// 验证失败：签发机构未知、证书本身无效（含过期）、主机名与证书不符、
// 系统根证书读取失败。用 errors.As 沿错误链查找，所以 url.Error 等
// 包装层不影响识别。
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

// ClassifyHTTPResponse 把状态码不是 200 的 provider 响应归类为 CallError，
// 让重试、熔断和给用户的错误文本都按类别处理。响应体只读前 2048 字节，
// 经 SanitizeMessage 压成单行、限长后作为错误文本。只读开头的原因：
// 错误正文只用来给人看，读全量没有用处，还会让异常大的响应占内存。
// 分类按状态码判定：
//
//   - 429：ClassRateLimit。RetryAfter 取自 Retry-After 头（缺失为 0），
//     调用方重试和熔断都按它等待。
//   - 401 或 403：正文（转小写）含 balance、billing、payment、quota
//     任一词时归为 ClassQuota，否则归为 ClassAuth。原因：账户余额耗尽
//     时若归为 ClassAuth，错误文本会提示"检查 API key"，把用户引向
//     错误方向。ClassAuth 的文本提示检查配置的 key。
//   - 402：ClassQuota，错误文本提示充值或调整 provider 控制台预算。
//   - 413：ClassBatchTooLarge。调用方收到该类别后把批次对半拆分重发，
//     不按失败重试。
//   - 400 且正文提到 token 并含 max、limit、exceed、too 任一词：同样归为
//     ClassBatchTooLarge。Voyage 对单批 token 数超上限的请求返回的是
//     400 而不是 413，正文说明 token 超限。
//   - 408 或任何 5xx：ClassTransient，可重试。
//   - 其余状态码：ClassPermanent，请求本身不被接受，重试结果不变。
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
