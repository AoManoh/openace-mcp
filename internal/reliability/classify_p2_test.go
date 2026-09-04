package reliability

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 本文件固定两处缺陷修复后的行为：证书验证失败归为 ClassPermanent。
// SanitizeMessage 截断时不切断多字节字符。

// TestClassifyCertificateErrorsPermanent 断言三种 x509 错误类型、经
// url.Error 包装的 x509 错误，以及只含 "x509:" 文本的错误都归为
// ClassPermanent。修复前它们一律归为 ClassTransient，用户要等完默认
// 5 次重试和退避（重试之间的等待）才看到证书问题。普通连接失败仍归为
// ClassTransient，作为对照。
func TestClassifyCertificateErrorsPermanent(t *testing.T) {
	ctx := context.Background()
	attempt, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cases := []error{
		x509.UnknownAuthorityError{},
		x509.CertificateInvalidError{Reason: x509.Expired},
		x509.HostnameError{Certificate: &x509.Certificate{}, Host: "example.com"},
		&url.Error{Op: "Post", URL: "https://x", Err: x509.UnknownAuthorityError{}},
		fmt.Errorf("Post \"https://x\": tls: failed to verify certificate: x509: certificate signed by unknown authority"),
	}
	for _, err := range cases {
		got := ClassifyTransportError(ctx, attempt, time.Minute, err)
		callErr := &CallError{}
		if !errors.As(got, &callErr) {
			t.Fatalf("应返回 CallError: %v", got)
		}
		if callErr.Class != ClassPermanent {
			t.Fatalf("证书类错误应归为 ClassPermanent,得到 %v(输入 %v)", callErr.Class, err)
		}
	}
	// 对照：普通连接失败仍归为 ClassTransient。
	got := ClassifyTransportError(ctx, attempt, time.Minute, errors.New("dial tcp: connection refused"))
	callErr := &CallError{}
	if !errors.As(got, &callErr) || callErr.Class != ClassTransient {
		t.Fatalf("连接失败应仍归为 ClassTransient: %v", got)
	}
}

// TestSanitizeMessageRuneBoundary 断言 900 字节的纯中文输入截断后仍是
// 合法 UTF-8、以 "…" 结尾，且总长不超过 512 字节加 "…" 的长度。修复前
// 按字节硬切会把一个三字节字符切开，产物不是合法 UTF-8。
func TestSanitizeMessageRuneBoundary(t *testing.T) {
	long := strings.Repeat("配", 300) // 每字 3 字节，共 900 字节
	got := SanitizeMessage(long)
	if !utf8.ValidString(got) {
		t.Fatalf("截断产物必须是合法 UTF-8: %q…", got[:24])
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长消息应带截断标记: %q", got[len(got)-8:])
	}
	if len(got) > 512+len("…") {
		t.Fatalf("截断后长度超过 512 字节加省略号: %d", len(got))
	}
}
