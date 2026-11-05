package reliability

import (
	"context"
	"sync"
)

// UsageKind 区分送审用途：文档嵌入、查询嵌入与精排。
type UsageKind string

const (
	UsageDocument UsageKind = "document"
	UsageQuery    UsageKind = "query"
	UsageRerank   UsageKind = "rerank"
)

// UsageCounts 累计服务商在成功响应里报告的 usage.total_tokens 与请求数，
// 文档嵌入、查询嵌入与精排分列。服务商按这个数计费；成功响应里没有用量
// 字段（部分自部署服务）时只计入 UnreportedRequests，不估算。
//
// *AbandonedRequests 与 *AbandonedBytes 是单次尝试到了超时、调用方仍在等待
// 而被放弃的请求数与请求体字节数（发出的 JSON 字节数）。这些请求已经发出，
// 服务商可能照常处理并计费，openACE 随后按重试规则重发同一批内容；服务商
// 没有返回用量，所以只记字节数、不估算 tokens。
type UsageCounts struct {
	DocumentTokens     int64
	DocumentRequests   int64
	QueryTokens        int64
	QueryRequests      int64
	RerankTokens       int64
	RerankRequests     int64
	UnreportedRequests int64

	DocumentAbandonedRequests int64
	DocumentAbandonedBytes    int64
	QueryAbandonedRequests    int64
	QueryAbandonedBytes       int64
	RerankAbandonedRequests   int64
	RerankAbandonedBytes      int64
}

// Plus 返回两组计数之和。
func (u UsageCounts) Plus(other UsageCounts) UsageCounts {
	return UsageCounts{
		DocumentTokens:     u.DocumentTokens + other.DocumentTokens,
		DocumentRequests:   u.DocumentRequests + other.DocumentRequests,
		QueryTokens:        u.QueryTokens + other.QueryTokens,
		QueryRequests:      u.QueryRequests + other.QueryRequests,
		RerankTokens:       u.RerankTokens + other.RerankTokens,
		RerankRequests:     u.RerankRequests + other.RerankRequests,
		UnreportedRequests: u.UnreportedRequests + other.UnreportedRequests,

		DocumentAbandonedRequests: u.DocumentAbandonedRequests + other.DocumentAbandonedRequests,
		DocumentAbandonedBytes:    u.DocumentAbandonedBytes + other.DocumentAbandonedBytes,
		QueryAbandonedRequests:    u.QueryAbandonedRequests + other.QueryAbandonedRequests,
		QueryAbandonedBytes:       u.QueryAbandonedBytes + other.QueryAbandonedBytes,
		RerankAbandonedRequests:   u.RerankAbandonedRequests + other.RerankAbandonedRequests,
		RerankAbandonedBytes:      u.RerankAbandonedBytes + other.RerankAbandonedBytes,
	}
}

func (u *UsageCounts) add(kind UsageKind, tokens int64, reported bool) {
	switch kind {
	case UsageDocument:
		u.DocumentTokens += tokens
		u.DocumentRequests++
	case UsageQuery:
		u.QueryTokens += tokens
		u.QueryRequests++
	case UsageRerank:
		u.RerankTokens += tokens
		u.RerankRequests++
	}
	if !reported {
		u.UnreportedRequests++
	}
}

func (u *UsageCounts) addAbandoned(kind UsageKind, bytes int64) {
	switch kind {
	case UsageDocument:
		u.DocumentAbandonedRequests++
		u.DocumentAbandonedBytes += bytes
	case UsageQuery:
		u.QueryAbandonedRequests++
		u.QueryAbandonedBytes += bytes
	case UsageRerank:
		u.RerankAbandonedRequests++
		u.RerankAbandonedBytes += bytes
	}
}

// UsageMeter 并发安全地累计 UsageCounts。零值可用。
//
// 设立原因：实时嵌入路径过去丢弃响应里的用量，共享 daemon 在后台为各工作区
// 送审了多少 token 从状态里看不到，产生计费后只能从索引文件与文件修改时间
// 事后反推消耗来源。
type UsageMeter struct {
	mu     sync.Mutex
	counts UsageCounts
}

// Record 记一次成功响应的用量。
func (m *UsageMeter) Record(kind UsageKind, tokens int64, reported bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.counts.add(kind, tokens, reported)
	m.mu.Unlock()
}

// RecordAbandoned 记一次因单次尝试超时被放弃的请求与它的请求体字节数。
func (m *UsageMeter) RecordAbandoned(kind UsageKind, bytes int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.counts.addAbandoned(kind, bytes)
	m.mu.Unlock()
}

// Snapshot 返回当前累计值。
func (m *UsageMeter) Snapshot() UsageCounts {
	if m == nil {
		return UsageCounts{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts
}

type usageMeterKey struct{}

// WithUsageMeter 让 ctx 携带一个计量器：经该 ctx 发出的成功请求，其用量除了
// 记入客户端自己的累计，还记入这个计量器。构建与检索用它把用量归到具体
// 工作区；共享客户端被几个工作区同时使用时也不会混记。
func WithUsageMeter(ctx context.Context, meter *UsageMeter) context.Context {
	return context.WithValue(ctx, usageMeterKey{}, meter)
}

// RecordUsage 把一次成功响应的用量记入 ctx 携带的计量器（没有则不记）。
func RecordUsage(ctx context.Context, kind UsageKind, tokens int64, reported bool) {
	if meter, ok := ctx.Value(usageMeterKey{}).(*UsageMeter); ok {
		meter.Record(kind, tokens, reported)
	}
}

// RecordAbandoned 把一次被放弃的请求记入 ctx 携带的计量器（没有则不记）。
func RecordAbandoned(ctx context.Context, kind UsageKind, bytes int64) {
	if meter, ok := ctx.Value(usageMeterKey{}).(*UsageMeter); ok {
		meter.RecordAbandoned(kind, bytes)
	}
}
