package reliability

import (
	"crypto/tls"
	"net/http"
)

// NewHTTPClient 构造 provider 调用共用的 HTTP 客户端（F3 修订）。
//
// 显式禁用 HTTP/2：Go 默认对 https 端点协商 h2 后，全部并发请求复用
// 单条 TCP 连接；embedding 响应体大（128×1024 维 float 文本 ≈ 1.6MB），
// 云端 provider 普遍按连接限速，MaxConcurrency 路并发挤兑单连接会把
// 每批延迟放大 N 倍直至整场超时。HTTP/1.1 连接池让每个在途请求独占
// 一条连接，聚合带宽随并发线性扩展；连接数上限即客户端并发槽数，
// 不会失控。超时仍由每次尝试的 context 控制（暗坑 K26）。
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	// 仅清空 TLSNextProto 不够：克隆自 DefaultTransport 的 TLS 配置可能
	// 已带 h2 ALPN，服务端一旦选中 h2 而本端只会说 HTTP/1.1，连接将以
	// "malformed HTTP response（h2 SETTINGS 帧）"损坏。显式把 ALPN 钉死
	// 在 http/1.1 才能保证协商结果与传输实现一致。
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	// 64:空闲连接池上限须覆盖最高常用嵌入并发(默认 16,自部署高吞吐
	// 可调到 64),否则超出部分每请求重建 TCP+TLS,高并发下握手开销
	// 侵蚀吞吐。仅影响连接复用效率,不构成并发上限。
	transport.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: transport}
}
