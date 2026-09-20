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
	"sync/atomic"
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

// Client 在工作区之间共享请求预算、索引治理状态，以及各自独立的索引与查询熔断器。
// 索引重试逐次准入，查询不占索引窗口，两个路径都登记显式 RPM/TPM 预算。
type Client struct {
	cfg          Config
	httpClient   *http.Client
	circuitIndex *reliability.Circuit
	circuitQuery *reliability.Circuit
	governor     *reliability.Governor
	limiter      *reliability.RateLimiter
	retry        reliability.RetryPolicy
}

// NewClient 创建客户端；未启用的配置直接报错（调用方应先判 Enabled）。
func NewClient(cfg Config) (*Client, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("embedding provider is not enabled: %s", cfg.DisabledReason)
	}
	if cfg.InitialConcurrency < 1 {
		cfg.InitialConcurrency = defaultConcurrency
	}
	client := &Client{
		cfg:          cfg,
		httpClient:   reliability.NewHTTPClient(),
		circuitIndex: reliability.NewCircuit(),
		circuitQuery: reliability.NewCircuit(),
		governor:     reliability.NewGovernor(cfg.InitialConcurrency),
		limiter:      reliability.NewRateLimiter(cfg.RPMBudget, cfg.TPMBudget),
		retry:        reliability.DefaultRetryPolicy(cfg.MaxRetries),
	}
	return client, nil
}

// Config 返回创建时的配置（不含可变状态）。
func (c *Client) Config() Config {
	return c.cfg
}

// CircuitSnapshot 返回索引嵌入的健康状态，构建据此决定是否等待退避或
// 补齐向量。查询嵌入的失败由 QueryCircuitSnapshot 和检索降级原因报告。
func (c *Client) CircuitSnapshot() reliability.CircuitSnapshot {
	return c.circuitIndex.Snapshot()
}

// GovernorSnapshot 返回当前索引窗口、暂停和资源等待原因。
func (c *Client) GovernorSnapshot() reliability.GovernorSnapshot {
	return c.governor.Snapshot()
}

// QueryCircuitSnapshot 返回查询嵌入独立的熔断状态。
func (c *Client) QueryCircuitSnapshot() reliability.CircuitSnapshot {
	return c.circuitQuery.Snapshot()
}

// laneCircuit 按输入类型选择各自的熔断器，防止索引失败暂停查询请求。
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
	return c.EmbedBatchObserved(ctx, texts, inputType, nil)
}

// EmbedBatchObserved 在重试睡眠前后通知投放者，睡眠中的批次仍保留逻辑状态，
// 但不阻止其他批参与准入。回调只属于本次调用，不修改共享重试策略。
func (c *Client) EmbedBatchObserved(ctx context.Context, texts []string, inputType InputType, retryWaiting func(bool)) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if inputType != InputDocument {
		return c.embedBatch(ctx, texts, inputType, retryWaiting)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if batch, ok := c.TryIndexBatch(texts); ok {
			return batch.Embed(ctx, retryWaiting)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// IndexBatch 持有启动逻辑批次前的内存预留。Embed 最多执行一次；未执行时
// Close 归还预留，已开始执行时由 Embed 退出负责归还。
type IndexBatch struct {
	client  *Client
	texts   []string
	release func()
	state   atomic.Uint32
}

func (b *IndexBatch) Close() {
	if b.state.CompareAndSwap(0, 2) {
		b.release()
	}
}
func (b *IndexBatch) Embed(ctx context.Context, retryWaiting func(bool)) ([][]float32, error) {
	if !b.state.CompareAndSwap(0, 1) {
		return nil, fmt.Errorf("index batch already used or closed")
	}
	defer func() { b.state.Store(2); b.release() }()
	return b.client.embedBatch(ctx, b.texts, InputDocument, retryWaiting)
}
func (c *Client) TryIndexBatch(texts []string) (*IndexBatch, bool) {
	release, ok := c.governor.TryStartOperation(c.requestBytes(texts))
	if !ok {
		return nil, false
	}
	return &IndexBatch{client: c, texts: texts, release: release}, true
}
func (c *Client) IndexWindowChanges() <-chan struct{} { return c.governor.Changes() }

func (c *Client) requestBytes(texts []string) int64 {
	const maximum = int64(^uint64(0) >> 1)
	tokens, n, dim := int64(estimateTokens(texts)), int64(len(texts)), int64(c.cfg.Dimension)
	if tokens < 0 || dim < 0 || tokens > maximum/4 || n > maximum/16/max(int64(1), dim) {
		return maximum
	}
	vectors := n * dim * 16
	if tokens*4 > maximum-vectors {
		return maximum
	}
	return tokens*4 + vectors
}

func (c *Client) embedBatch(ctx context.Context, texts []string, inputType InputType, retryWaiting func(bool)) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	circuit := c.laneCircuit(inputType)
	if err := circuit.Gate(); err != nil {
		return nil, err
	}
	vectors, err := c.embedOnce(ctx, texts, inputType, retryWaiting)
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
		left, err := c.embedBatch(ctx, texts[:mid], inputType, retryWaiting)
		if err != nil {
			return nil, err
		}
		right, err := c.embedBatch(ctx, texts[mid:], inputType, retryWaiting)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}
	if inputType == InputDocument && c.governor != nil &&
		callErr.Class == reliability.ClassRateLimit && !c.governor.AtRateFloor() {
		// 429 先由治理器降速与暂停；错误仍上报，后续同步补齐覆盖缺口。
		return nil, callErr
	}
	circuit.RecordFailure(callErr)
	return nil, callErr
}

// embedOnce 在每次 HTTP 尝试前检查显式预算和索引名额。重试同样计数，
// 每次尝试结束后先释放名额再退避，避免没有 HTTP 在途时仍挡住其他批。
// 查询共用显式预算，但不占索引名额。等待预算的时间不作为 provider 延迟。
func (c *Client) embedOnce(ctx context.Context, texts []string, inputType InputType, retryWaiting func(bool)) ([][]float32, error) {
	tokens := estimateTokens(texts)
	indexLane := inputType == InputDocument
	var vectors [][]float32
	retry := c.retry
	if retryWaiting != nil {
		retry.Sleep = func(ctx context.Context, d time.Duration) error {
			retryWaiting(true)
			defer retryWaiting(false)
			if c.retry.Sleep != nil {
				return c.retry.Sleep(ctx, d)
			}
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	err := retry.Do(ctx, func(ctx context.Context) error {
		var permit reliability.IndexPermit
		if indexLane {
			// 同时保留请求文本、JSON 响应与解码向量的投影，使用 int64 防乘法溢出。
			bytes := c.requestBytes(texts)
			p, err := c.governor.AcquireIndexWithBudget(ctx, tokens, bytes, c.limiter)
			if err != nil {
				return err
			}
			permit = p
		} else if err := c.limiter.Acquire(ctx, 1, tokens); err != nil {
			return err
		}
		start := time.Now()
		outcome := reliability.OutcomeOther
		var latency, retryAfter time.Duration
		if indexLane {
			defer func() { c.governor.Observe(permit, outcome, latency, retryAfter) }()
		}
		got, err := c.doRequest(ctx, texts, inputType)
		outcome, latency, retryAfter = governorOutcome(err), time.Since(start), retryAfterOf(err)
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

	// 服务商差异只在这里：voyage 形状带 input_type 与 output_dimension（维度未知时不传，
	// 由服务端按模型默认维度返回，供探测）；OpenAI 兼容形状只有 input 与 model。
	body := map[string]any{"input": texts, "model": c.cfg.Model}
	if c.cfg.ProviderType == ProviderVoyage {
		body["input_type"] = string(inputType)
		if c.cfg.Dimension > 0 {
			body["output_dimension"] = c.cfg.Dimension
		}
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
		// Dimension==0 只出现在维度探测请求（ResolveDimension）：此时不校验，由调用方读取长度。
		if c.cfg.Dimension > 0 && len(item.Embedding) != c.cfg.Dimension {
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
