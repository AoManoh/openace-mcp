package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// InputType 区分文档与查询嵌入（voyage 语义；openai 形状忽略该维度）。
type InputType string

const (
	// InputDocument 是索引期的 chunk 嵌入。
	InputDocument InputType = "document"
	// InputQuery 是查询期的 query 嵌入。
	InputQuery InputType = "query"
)

// maxResponseBytes 限制响应体读取（1000 条 × 2048 维的极限批约 30MB，留足余量）。
const maxResponseBytes = 256 << 20

// Client 是 embedding provider 的 HTTP 客户端。熔断/限流/并发状态挂在
// 实例上，实例必须为 daemon 级单例共享（暗坑 K36）。
//
// 车道分离(T6):索引车道(document 批)与查询车道(query)各持独立熔断——
// 变更前共用一个熔断器,索引 429 风暴会把交互查询一起打进词法降级
// (架构分析 2026-08-20 事实 F5)。索引车道另受吞吐治理器准入(429 学
// 速率、延迟学窗口);查询车道直通治理器与并发槽:交互请求一次一条,
// 不该排在几十个索引批后面。逃生门 OPENACE_THROUGHPUT_GOVERNOR=off
// 回到共用单熔断+无治理器的旧行为。
type Client struct {
	cfg          Config
	httpClient   *http.Client
	circuitIndex *reliability.Circuit
	circuitQuery *reliability.Circuit
	governor     *reliability.Governor
	limiter      *reliability.RateLimiter
	sem          chan struct{}
	retry        reliability.RetryPolicy
}

// NewClient 创建客户端；未启用的配置直接报错（调用方应先判 Enabled）。
func NewClient(cfg Config) (*Client, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("embedding provider is not enabled: %s", cfg.DisabledReason)
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	client := &Client{
		cfg: cfg,
		// 超时由每次尝试的 context 控制（取消需即时关闭请求，暗坑 K26）；
		// 传输层禁 h2 走 HTTP/1.1 连接池（F3：单连接复用会挤兑超时）。
		// MaxConcurrency 已在上方钳位:0 会使 sem 退化为无缓冲 channel,
		// 全部调用悬挂到超时(L6)。
		httpClient:   reliability.NewHTTPClient(),
		circuitIndex: reliability.NewCircuit(),
		limiter:      reliability.NewRateLimiter(cfg.RPMBudget, cfg.TPMBudget),
		sem:          make(chan struct{}, cfg.MaxConcurrency),
		retry:        reliability.DefaultRetryPolicy(cfg.MaxRetries),
	}
	if cfg.GovernorDisabled {
		// 逃生门:单熔断共用、无治理器——与治理器引入前逐字节同行为。
		client.circuitQuery = client.circuitIndex
	} else {
		client.circuitQuery = reliability.NewCircuit()
		client.governor = reliability.NewGovernor(cfg.MaxConcurrency)
	}
	return client, nil
}

// Config 返回创建时的配置（不含可变状态）。
func (c *Client) Config() Config {
	return c.cfg
}

// CircuitSnapshot 返回索引车道健康视图（构建路径的状态上报口径:
// 构建 no-op 判定与语义补齐都看索引车道;查询车道故障经每次查询的
// 显式降级原因外露,不进此视图）。
func (c *Client) CircuitSnapshot() reliability.CircuitSnapshot {
	return c.circuitIndex.Snapshot()
}

// GovernorSnapshot 返回吞吐治理状态（可观测;逃生门 off 时为零值）。
func (c *Client) GovernorSnapshot() reliability.GovernorSnapshot {
	return c.governor.Snapshot()
}

// laneCircuit 按输入类型选车道熔断。
func (c *Client) laneCircuit(inputType InputType) *reliability.Circuit {
	if inputType == InputQuery {
		return c.circuitQuery
	}
	return c.circuitIndex
}

// EmbedQuery 嵌入单条查询文本。
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.EmbedBatch(ctx, []string{text}, InputQuery)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// EmbedBatch 嵌入一批文本，结果与输入逐位对齐；单调用 all-or-nothing
// （数量/维度校验失败整批拒绝，暗坑 K22）。circuit 退避期返回 ClassBackoff
// 错误且不发请求（D10 no-op 判定依据）；批规模超上游限制时自适应对半
// 拆分（暗坑 K23，拆分不计 circuit 失败）。
//
// 注记：拆分路径的子批 all-or-nothing 仍成立——右半失败时左半结果一并
// 丢弃（调用方按"整批未完成"处理）；默认批 128 条使该窗口足够小。
func (c *Client) EmbedBatch(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	circuit := c.laneCircuit(inputType)
	if err := circuit.Gate(); err != nil {
		return nil, err
	}
	vectors, err := c.embedOnce(ctx, texts, inputType)
	if err == nil {
		circuit.RecordSuccess()
		return vectors, nil
	}
	// 取消优先：不把调用方取消计为 provider 失败（暗坑 K26）。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	callErr := asCallError(err)
	if callErr == nil {
		return nil, err
	}
	if callErr.Class == reliability.ClassBatchTooLarge {
		if len(texts) == 1 {
			// 当前 chunk profile（≤4KB/chunk）不可能触发；防御性明确报错。
			return nil, &reliability.CallError{
				Class:   reliability.ClassPermanent,
				Message: reliability.SanitizeMessage("single text exceeds provider batch limit: " + callErr.Message),
			}
		}
		mid := len(texts) / 2
		left, err := c.EmbedBatch(ctx, texts[:mid], inputType)
		if err != nil {
			return nil, err
		}
		right, err := c.EmbedBatch(ctx, texts[mid:], inputType)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}
	if inputType == InputDocument && c.governor != nil &&
		callErr.Class == reliability.ClassRateLimit && !c.governor.AtRateFloor() {
		// T6 层次:治理器是油门,熔断是保险丝。429 已由治理器降速+暂停,
		// 未到速率地板就不熔断——变更前这里直接进退避,索引风暴期吞吐
		// 归零且(共用熔断时代)殃及查询。本批失败如实上抛,覆盖缺口由
		// 既有部分成功语义与下次 semantic-fill 收敛,不静默。
		return nil, callErr
	}
	circuit.RecordFailure(callErr)
	return nil, callErr
}

// embedOnce 执行一次（含策略内重试的）批调用：limiter → 车道准入 → HTTP。
// 索引车道:并发槽(硬上限)+治理器(速率令牌与动态窗口,每次真实 HTTP
// 尝试都过闸并回报观测——重试也是真实请求,消耗同样的上游配额)。
// 查询车道:直通并发槽与治理器——交互查询一次一条,不排在索引批后面;
// 其上限由查询到达率天然约束。用户 RPM/TPM 预算(limiter)对两车道都是
// 不可逾越的硬顶,语义与引入治理器前一致。
func (c *Client) embedOnce(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	tokens := estimateTokens(texts)
	if err := c.limiter.Acquire(ctx, 1, tokens); err != nil {
		return nil, err
	}
	indexLane := inputType == InputDocument
	if indexLane {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var vectors [][]float32
	err := c.retry.Do(ctx, func(ctx context.Context) error {
		if indexLane && c.governor != nil {
			if err := c.governor.AcquireIndex(ctx, tokens); err != nil {
				return err
			}
		}
		start := time.Now()
		got, err := c.doRequest(ctx, texts, inputType)
		if indexLane && c.governor != nil {
			c.governor.Observe(governorOutcome(err), tokens, time.Since(start), retryAfterOf(err))
		}
		if err != nil {
			return err
		}
		vectors = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return vectors, nil
}

// governorOutcome 把调用结果映射为治理器观测分类:429→速率信号,
// 暂态(5xx/超时/断流)→容量过载信号,其余(认证/额度/永久/拆批)不属于
// 吞吐问题,治理器不动作、由既有分类外抛。
func governorOutcome(err error) reliability.GovernorOutcome {
	if err == nil {
		return reliability.OutcomeSuccess
	}
	callErr := asCallError(err)
	if callErr == nil {
		return reliability.OutcomeOther
	}
	switch callErr.Class {
	case reliability.ClassRateLimit:
		return reliability.OutcomeRateLimited
	case reliability.ClassTransient:
		return reliability.OutcomeOverload
	default:
		return reliability.OutcomeOther
	}
}

// retryAfterOf 提取 429 的 Retry-After(无则 0,治理器用缺省冷却)。
func retryAfterOf(err error) time.Duration {
	if callErr := asCallError(err); callErr != nil {
		return callErr.RetryAfter
	}
	return 0
}

// doRequest 发送单次 HTTP 请求并解析、校验响应。
func (c *Client) doRequest(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	timeout := c.cfg.Timeout
	if inputType == InputQuery && c.cfg.QueryTimeout > 0 {
		// RS3:查询期独立超时,构建期大批调优不放大交互最坏等待。
		timeout = c.cfg.QueryTimeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body := map[string]any{"input": texts, "model": c.cfg.Model}
	if c.cfg.ProviderType == ProviderVoyage {
		body["input_type"] = string(inputType)
		body["output_dimension"] = c.cfg.Dimension
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage("encode request: " + err.Error())}
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.cfg.BaseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	// key 只在此处出现；为空时不发送 Authorization 头（自部署常态，暗坑 K21）。
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, reliability.ClassifyTransportError(ctx, attemptCtx, timeout, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, reliability.ClassifyHTTPResponse(resp)
	}

	var decoded struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	// 成功路径读尽剩余字节让连接归还池(L7,诊断 2026-08-03):json.Decoder
	// 停在 JSON 值末尾,不读尽 body 时 HTTP/1.1 连接无法复用,每批重建
	// TCP+TLS,与 F3 连接池设计意图相悖(纯性能非泄漏)。
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&decoded); err != nil {
		// 响应体读取期间的取消/超时不是 malformed（F3）：误判为 permanent
		// 会计入 circuit 并在大批量构建中把语义路整场熄火。
		if ctx.Err() != nil || attemptCtx.Err() != nil {
			return nil, reliability.ClassifyTransportError(ctx, attemptCtx, timeout, err)
		}
		// 连接中断类解码错误同理（F6，sealed 实跑发现：redis 首建遭
		// "unexpected EOF" 被判 permanent → circuit 退避 → 11% 覆盖入库;
		// sealed v2 k8s 再遭 TCP connection reset 同样误判 → 94% 覆盖,
		// 2026-08-04 补齐 net 层错误家族）：半途断流是传输故障不是 JSON
		// 垃圾，必须 transient 走重试。
		var netErr net.Error
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
			errors.As(err, &netErr) || errors.Is(err, net.ErrClosed) ||
			errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
			return nil, &reliability.CallError{Class: reliability.ClassTransient, Message: reliability.SanitizeMessage("response stream interrupted: " + err.Error())}
		}
		return nil, &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage("malformed embeddings response: " + err.Error())}
	}
	// 结构校验（暗坑 K22）：数量精确、index 唯一且在界、维度与配置一致。
	if len(decoded.Data) != len(texts) {
		return nil, &reliability.CallError{
			Class:   reliability.ClassPermanent,
			Message: fmt.Sprintf("embedding count mismatch: requested %d, got %d (batch rejected)", len(texts), len(decoded.Data)),
		}
	}
	out := make([][]float32, len(texts))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(texts) || out[item.Index] != nil {
			return nil, &reliability.CallError{
				Class:   reliability.ClassPermanent,
				Message: fmt.Sprintf("embedding response index invalid or duplicated: %d (batch rejected)", item.Index),
			}
		}
		if len(item.Embedding) != c.cfg.Dimension {
			return nil, &reliability.CallError{
				Class: reliability.ClassPermanent,
				Message: fmt.Sprintf("embedding dimension mismatch: got %d, want %d (check %s or the served model)",
					len(item.Embedding), c.cfg.Dimension, EnvDimension),
			}
		}
		out[item.Index] = item.Embedding
	}
	return out, nil
}

// estimateTokens 是保守的 token 估算（字节/4，向上取整）。
func estimateTokens(texts []string) int {
	total := 0
	for _, text := range texts {
		total += len(text)/4 + 1
	}
	return total
}

func asCallError(err error) *reliability.CallError {
	callErr := &reliability.CallError{}
	if errors.As(err, &callErr) {
		return callErr
	}
	return nil
}
