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

// P2 修复回归(诊断 2026-08-03 §6):L8 证书类永久错误分流、L10 截断
// rune 边界安全。

// TestClassifyCertificateErrorsPermanent:x509 验证失败重试无意义,必须
// 判 permanent(旧行为:一律 transient → 5 次重试+退避才见可行动错误)。
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
			t.Fatalf("证书类错误应 permanent,got %v for %v", callErr.Class, err)
		}
	}
	// 普通连接错误保持 transient。
	got := ClassifyTransportError(ctx, attempt, time.Minute, errors.New("dial tcp: connection refused"))
	callErr := &CallError{}
	if !errors.As(got, &callErr) || callErr.Class != ClassTransient {
		t.Fatalf("连接失败应保持 transient: %v", got)
	}
}

// TestSanitizeMessageRuneBoundary:512 字节截断不得切断多字节字符。
func TestSanitizeMessageRuneBoundary(t *testing.T) {
	long := strings.Repeat("配", 300) // 3 字节/字 → 900 字节
	got := SanitizeMessage(long)
	if !utf8.ValidString(got) {
		t.Fatalf("截断产物必须是合法 UTF-8: %q…", got[:24])
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长消息应带截断标记: %q", got[len(got)-8:])
	}
	if len(got) > 512+len("…") {
		t.Fatalf("截断长度失守: %d", len(got))
	}
}
