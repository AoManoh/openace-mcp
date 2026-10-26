package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// maxResponseBytes 限制响应体读取（rerank 响应只含分数与 index，1MB 余量充足）。
const maxResponseBytes = 1 << 20

// Document 是送审文档；Text 为已拼好 `path:start-end symbol` 头的完整
// 送审文本（阶段计划 D6：rerank 无缓存语义，逐查询构造）。
type Document struct {
	ID   string
	Text string
}

// Hit 是精排结果（按相关性降序）。
type Hit struct {
	ID    string
	Score float64
}

// Client 是 rerank provider 的 HTTP 客户端；circuit 挂在实例上，
// 实例必须为 daemon 级单例共享（暗坑 K36）。查询路天然低频，
// 不设客户端 RPM/TPM limiter（预算 env 为 embedding 专属，§4）。
type Client struct {
	cfg        Config
	httpClient *http.Client
	circuit    *reliability.Circuit
	retry      reliability.RetryPolicy
	// usage 累计成功响应里服务商报告的精排用量（进程内各工作区共享）；
	// 按工作区的用量经 ctx 携带的计量器另记。
	usage reliability.UsageMeter
}

// UsageSnapshot 返回客户端创建以来成功响应里服务商报告的精排用量累计。
func (c *Client) UsageSnapshot() reliability.UsageCounts {
	return c.usage.Snapshot()
}

// reportedUsage 读取响应里的 usage.total_tokens（voyage 形状）；TEI 形状的
// 响应是数组、没有用量字段，返回 reported=false。
func reportedUsage(raw []byte) (tokens int64, reported bool) {
	var decoded struct {
		Usage *struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Usage == nil {
		return 0, false
	}
	return decoded.Usage.TotalTokens, true
}

// NewClient 创建客户端；未启用的配置直接报错。
func NewClient(cfg Config) (*Client, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("rerank provider is not enabled: %s", cfg.DisabledReason)
	}
	return &Client{
		cfg: cfg,
		// 传输层禁 h2 走 HTTP/1.1 连接池（F3，与 embedding 客户端一致）。
		httpClient: reliability.NewHTTPClient(),
		circuit:    reliability.NewCircuit(),
		retry:      reliability.DefaultRetryPolicy(cfg.MaxRetries),
	}, nil
}

// Config 返回创建时的配置。
func (c *Client) Config() Config {
	return c.cfg
}

// CircuitSnapshot 返回 provider 健康视图（进入状态上报）。
func (c *Client) CircuitSnapshot() reliability.CircuitSnapshot {
	return c.circuit.Snapshot()
}

// Rerank 精排 docs 头部（受 MaxTokens 估算截断，暗坑 K28）并返回按
// 相关性降序的命中与实际送审数 sent；docs[sent:] 由调用方按原序跟随。
// 成功时 hits 与送审集一一对应（条数不足按 malformed 整体拒绝，H1）；
// circuit 退避期返回 ClassBackoff；任何失败下调用方候选集不受影响
// （P3-T04 业务验收：rerank 失败绝不丢候选）。
func (c *Client) Rerank(ctx context.Context, query string, docs []Document) (hits []Hit, sent int, err error) {
	if len(docs) == 0 {
		return nil, 0, nil
	}
	sent = c.tokenCappedCount(query, docs)
	if sent == 0 {
		// 预算不足以送审任何文档：如实返回空重排（调用方保持 RRF 序）。
		return nil, 0, nil
	}
	if err := c.circuit.Gate(); err != nil {
		return nil, 0, err
	}
	var out []Hit
	retryErr := c.retry.Do(ctx, func(ctx context.Context) error {
		got, err := c.doRequest(ctx, query, docs[:sent])
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if retryErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// 取消优先：不计为 provider 失败（暗坑 K26）。
			return nil, 0, ctxErr
		}
		if callErr := asCallError(retryErr); callErr != nil {
			c.circuit.RecordFailure(callErr)
		}
		return nil, 0, retryErr
	}
	c.circuit.RecordSuccess()
	return out, sent, nil
}

// tokenCappedCount 按保守估算（字节/4）决定送审文档数。
func (c *Client) tokenCappedCount(query string, docs []Document) int {
	queryTokens := len(query)/4 + 1
	total := 0
	for i, doc := range docs {
		docTokens := len(doc.Text)/4 + 1
		// rerank 计量语义：query 按文档数重复计入 + 全部文档 tokens（调研 B2）。
		if total+docTokens+queryTokens*(i+1) > c.cfg.MaxTokens {
			return i
		}
		total += docTokens
	}
	return len(docs)
}

// doRequest 发送单次 rerank 请求并解析、校验响应。
func (c *Client) doRequest(ctx context.Context, query string, docs []Document) ([]Hit, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	texts := make([]string, len(docs))
	for i, doc := range docs {
		texts[i] = doc.Text
	}
	var body map[string]any
	switch c.cfg.ProviderType {
	case ProviderTEI:
		// TEI 形状：{query, texts} → [{index, score}]。
		body = map[string]any{"query": query, "texts": texts}
	default:
		// voyage 形状（Cohere/Jina 兼容）：{query, documents, model, top_k}。
		body = map[string]any{"query": query, "documents": texts, "model": c.cfg.Model, "top_k": len(texts)}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage("encode request: " + err.Error())}
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.cfg.BaseURL+"/rerank", bytes.NewReader(payload))
	if err != nil {
		return nil, &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	// key 只在此处出现；为空时不发送 Authorization 头（暗坑 K21）。
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, reliability.ClassifyTransportError(ctx, attemptCtx, c.cfg.Timeout, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, reliability.ClassifyHTTPResponse(resp)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &reliability.CallError{Class: reliability.ClassTransient, Message: reliability.SanitizeMessage("read response: " + err.Error())}
	}
	// 用量在结构校验之前记账：服务商返回 200 即已计费。
	tokens, reported := reportedUsage(raw)
	c.usage.Record(reliability.UsageRerank, tokens, reported)
	reliability.RecordUsage(ctx, reliability.UsageRerank, tokens, reported)
	scored, err := parseResponse(c.cfg.ProviderType, raw)
	if err != nil {
		return nil, err
	}
	// 响应校验（暗坑 K28）：index 在界且唯一。
	seen := make(map[int]bool, len(scored))
	hits := make([]Hit, 0, len(scored))
	for _, item := range scored {
		if item.Index < 0 || item.Index >= len(docs) || seen[item.Index] {
			return nil, &reliability.CallError{
				Class:   reliability.ClassPermanent,
				Message: fmt.Sprintf("rerank response index invalid or duplicated: %d (response rejected)", item.Index),
			}
		}
		seen[item.Index] = true
		hits = append(hits, Hit{ID: docs[item.Index].ID, Score: item.Score})
	}
	// 结构校验（H1）：条数与送审集精确一致，all-or-nothing——与 embedding
	// 客户端 K22 同语义。空 data、部分返回（网关兜底页、忽略非标准 top_k
	// 且自带默认截断的端点）都按 malformed 整体拒绝，调用方走
	// rerank-skipped 显式降级路，绝不静默丢弃已送审候选（P3-T04 / 决策 11）。
	// 多于送审数的响应必含越界/重复 index，已被上面的检查拒绝。
	if len(scored) != len(docs) {
		return nil, &reliability.CallError{
			Class:   reliability.ClassPermanent,
			Message: fmt.Sprintf("rerank count mismatch: sent %d, got %d (response rejected)", len(docs), len(scored)),
		}
	}
	// 按分数降序稳定排序；同分保留响应中的条目顺序，不推断 provider 的同分规则。
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	return hits, nil
}

// scoredIndex 是三种响应形状归一后的条目。
type scoredIndex struct {
	Index int
	Score float64
}

// parseResponse 解析 voyage（data）/ Cohere/Jina（results）/ TEI（顶层数组）
// 三种响应形状。每条的 index 与分数都必须存在、不为 null，分数必须是有限
// 数：字段解码为指针，缺失或 null 时是 nil，和合法的 index=0、score=0 区分
// 开；否则缺失的 index 会被当成 0，缺失的分数会被当成 0 分，范围、唯一性
// 和条数检查都发现不了。index 的范围、唯一性与条数由 doRequest 随后检查。
func parseResponse(providerType string, raw []byte) ([]scoredIndex, error) {
	malformed := func(err error) *reliability.CallError {
		return &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage("malformed rerank response: " + err.Error())}
	}
	type item struct {
		Index *int     `json:"index"`
		Score *float64 `json:"score"`
		// RelevanceScore 是 data/results 形状的分数字段。
		RelevanceScore *float64 `json:"relevance_score"`
	}
	var source []item
	scoreField := "relevance_score"
	if providerType == ProviderTEI {
		scoreField = "score"
		if err := json.Unmarshal(raw, &source); err != nil {
			return nil, malformed(err)
		}
	} else {
		var decoded struct {
			Data    []item `json:"data"`
			Results []item `json:"results"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, malformed(err)
		}
		source = decoded.Data
		if len(source) == 0 && len(decoded.Results) > 0 {
			// Cohere/Jina 兼容端点用 results 键；语义一致。
			source = decoded.Results
		}
	}
	out := make([]scoredIndex, 0, len(source))
	for position, entry := range source {
		score := entry.RelevanceScore
		if providerType == ProviderTEI {
			score = entry.Score
		}
		switch {
		case entry.Index == nil:
			return nil, malformed(fmt.Errorf("item %d: missing or null index", position))
		case score == nil:
			return nil, malformed(fmt.Errorf("item %d: missing or null %s", position, scoreField))
		case math.IsNaN(*score) || math.IsInf(*score, 0):
			return nil, malformed(fmt.Errorf("item %d: non-finite %s", position, scoreField))
		}
		out = append(out, scoredIndex{Index: *entry.Index, Score: *score})
	}
	return out, nil
}

func asCallError(err error) *reliability.CallError {
	callErr := &reliability.CallError{}
	if errors.As(err, &callErr) {
		return callErr
	}
	return nil
}
