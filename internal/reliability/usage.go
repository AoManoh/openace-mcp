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
type UsageCounts struct {
	DocumentTokens     int64
	DocumentRequests   int64
	QueryTokens        int64
	QueryRequests      int64
	RerankTokens       int64
	RerankRequests     int64
	UnreportedRequests int64
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
