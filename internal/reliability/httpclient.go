package reliability

import (
	"crypto/tls"
	"net/http"
)

// NewHTTPClient 构造 provider 调用用的 HTTP 客户端，embedding 与 rerank
// 客户端各持一个。它不设整体超时：超时由每次尝试的 context 控制，调用方
// 取消时请求立即关闭。传输层强制使用 HTTP/1.1，原因如下。
//
// Go 默认对 https 端点协商 HTTP/2，之后全部并发请求复用同一条 TCP 连接。
// embedding 响应体大（128 条 × 1024 维 float 的文本约 1.6MB），云端
// provider 按连接限速：实测 Voyage 一批 128 条在单连接上要 27s，分到两条
// 连接上各 12-15s。多个请求共用一条连接时，每批延迟随
// 并发数成倍增长，直到整场构建超时。改用 HTTP/1.1 连接池后，每个在途
// 请求独占一条连接，总带宽随并发数增长。同时打开的连接数等于同时在途
// 的请求数，索引尝试在发送前受动态窗口与资源准入约束。
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	// 只关 ForceAttemptHTTP2 并清空 TLSNextProto 还不够：从 DefaultTransport
	// 克隆的 TLS 配置可能已经在 ALPN（TLS 握手时协商应用层协议的扩展）里
	// 声明了 h2。服务端选中 h2 而本端只按 HTTP/1.1 解析时，响应以
	// "malformed HTTP response" 失败，实际收到的是 h2 的 SETTINGS 帧。把
	// NextProtos 固定为 http/1.1，协商结果才与传输实现一致。
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	// 空闲连接最多保留 64 条，避免长时间保留大量描述符。这只影响复用，
	// 不限制在途请求；超过此数量的重复突发可能增加连接与 TLS 开销。
	transport.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: transport}
}
