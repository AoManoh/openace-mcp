package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	scored, err := parseResponse(c.cfg.ProviderType, raw)
	if err != nil {
		return nil, err
	}
	// 响应校验（暗坑 K28）：index 在界且唯一；允许 provider 只返回头部。
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
	// 按分数降序稳定排序；同分保持送审（RRF）顺序，保证确定性（暗坑 K27）。
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	return hits, nil
}

// scoredIndex 是三种响应形状归一后的条目。
type scoredIndex struct {
	Index int
	Score float64
}

// parseResponse 解析 voyage（data）/ Cohere/Jina（results）/ TEI（顶层数组）
// 三种响应形状。
func parseResponse(providerType string, raw []byte) ([]scoredIndex, error) {
	malformed := func(err error) *reliability.CallError {
		return &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage("malformed rerank response: " + err.Error())}
	}
	if providerType == ProviderTEI {
		var items []struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, malformed(err)
		}
		out := make([]scoredIndex, 0, len(items))
		for _, item := range items {
			out = append(out, scoredIndex{Index: item.Index, Score: item.Score})
		}
		return out, nil
	}
	var decoded struct {
		Data []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"data"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, malformed(err)
	}
	source := decoded.Data
	if len(source) == 0 && len(decoded.Results) > 0 {
		// Cohere/Jina 兼容端点用 results 键；语义一致。
		source = decoded.Results
	}
	out := make([]scoredIndex, 0, len(source))
	for _, item := range source {
		out = append(out, scoredIndex{Index: item.Index, Score: item.RelevanceScore})
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
