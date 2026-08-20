package embedding

// 离线批车道——voyage Batch API 适配(任务 T8,首个 adapter;架构分析
// 2026-08-20 §"批不是 voyage 特例"——OpenAI 等其他配额型托管的 adapter
// 后续按同一接口补,自部署无异步批概念、走治理后的同步车道即最优)。
//
// 业务形态:大额 document 嵌入(缺失量 ≥ 阈值)改走服务端批作业,费用
// -33%,完成窗 12h。生命周期 = 上传 JSONL(每行一条输入,custom_id=
// embedKey)→ 创建作业 → 轮询 → 下载结果 → 三重校验(键/维度/计数)。
// custom_id 用 embedKey 是从评测工具链搬来的教训:早期驱动按"文件内
// 全局偏移"对齐结果,一次维度全拒事故后固化为"键随行走,杜绝偏移
// 映射"(2026-08-13 台账)。
//
// 错误纪律(用户裁决 2026-08-20):批失败/超窗显式外抛,绝不静默回落
// 同步 API 双付;结果文件里的未知键/坏维度行显式拒绝并计数上报。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

const (
	// BulkJobMaxInputs 是单作业输入上限(voyage 官方 ≤100K;超量由调用
	// 方切分为多作业)。
	BulkJobMaxInputs = 100_000
	// bulkDefaultPollInterval 是作业轮询间隔缺省。轮询免费且服务端窗口
	// 是小时级,30s 足够灵敏。
	bulkDefaultPollInterval = 30 * time.Second
	// bulkMaxResultBytes 限制结果文件读取(100K 行 × 1024 维 float 文本
	// ≈ 1.5GB 上界;流式逐行解析,此处只防超尺寸异常文件)。
	bulkMaxResultBytes = 8 << 30
)

// BulkSupported 报告当前配置是否启用批车道。
func (c *Client) BulkSupported() bool {
	return c.cfg.Enabled && c.cfg.BatchAPIMode == ProviderVoyage
}

// BulkMinChunks 返回批车道触发阈值。
func (c *Client) BulkMinChunks() int { return c.cfg.BatchMinChunks }

// BulkJob 是一个已提交批作业的持久化身份(engine 落盘到作业状态文件,
// 崩溃/重启后据此续轮询,绝不重复提交付费)。
type BulkJob struct {
	ID          string    `json:"id"`
	InputFileID string    `json:"input_file_id"`
	Keys        []string  `json:"keys"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// BulkJobStatus 是轮询到的作业状态。
type BulkJobStatus struct {
	ID           string
	Status       string // in_progress / completed / failed / expired / cancelled
	OutputFileID string
	ErrorFileID  string
	Done         int
	Total        int
}

// Terminal 报告作业是否到达终态。
func (s BulkJobStatus) Terminal() bool {
	switch s.Status {
	case "completed", "partially_completed", "failed", "expired", "cancelled":
		return true
	}
	return false
}

// BulkPollInterval 返回轮询间隔(测试可经 Config 覆盖)。
func (c *Client) BulkPollInterval() time.Duration {
	if c.cfg.BulkPollInterval > 0 {
		return c.cfg.BulkPollInterval
	}
	return bulkDefaultPollInterval
}

// BulkSubmit 上传一段输入并创建批作业。keys/texts 逐位对齐且长度 ≤
// BulkJobMaxInputs(调用方负责切分);返回含 keys 的作业身份供持久化。
// 提交路径不经吞吐治理器(两次控制面请求,不是数据面吞吐)。
func (c *Client) BulkSubmit(ctx context.Context, keys []string, texts []string) (BulkJob, error) {
	if len(keys) != len(texts) || len(keys) == 0 {
		return BulkJob{}, fmt.Errorf("bulk submit: keys/texts 不对齐或为空 (%d/%d)", len(keys), len(texts))
	}
	if len(keys) > BulkJobMaxInputs {
		return BulkJob{}, fmt.Errorf("bulk submit: %d inputs 超过单作业上限 %d(调用方必须切分)", len(keys), BulkJobMaxInputs)
	}
	// 1) 组装 JSONL:每行一条输入,custom_id=embedKey。
	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	for i, key := range keys {
		line := map[string]any{
			"custom_id": key,
			"body":      map[string]any{"input": []string{texts[i]}},
		}
		if err := enc.Encode(line); err != nil {
			return BulkJob{}, fmt.Errorf("bulk submit: encode line: %w", err)
		}
	}
	// 2) 上传文件(multipart, purpose=batch)。
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("purpose", "batch"); err != nil {
		return BulkJob{}, err
	}
	part, err := writer.CreateFormFile("file", "openace-bulk.jsonl")
	if err != nil {
		return BulkJob{}, err
	}
	if _, err := part.Write(payload.Bytes()); err != nil {
		return BulkJob{}, err
	}
	if err := writer.Close(); err != nil {
		return BulkJob{}, err
	}
	var uploaded struct {
		ID string `json:"id"`
	}
	if err := c.bulkCall(ctx, http.MethodPost, "/files", writer.FormDataContentType(), &form, &uploaded); err != nil {
		return BulkJob{}, fmt.Errorf("bulk submit: upload input file: %w", err)
	}
	if uploaded.ID == "" {
		return BulkJob{}, fmt.Errorf("bulk submit: provider 未返回文件 id")
	}
	// 3) 创建作业。
	// voyage 真实形状(2026-08-20 真实 422 实测+119M 实跑驱动器口径):
	// 模型/输入类型/维度在**作业级** request_params,不在 JSONL 行内。
	createBody, err := json.Marshal(map[string]any{
		"input_file_id":     uploaded.ID,
		"endpoint":          "/v1/embeddings",
		"completion_window": "12h",
		"request_params": map[string]any{
			"model":            c.cfg.Model,
			"input_type":       string(InputDocument),
			"output_dimension": c.cfg.Dimension,
		},
		"metadata": map[string]string{"tool": "openace-bulk-lane"},
	})
	if err != nil {
		return BulkJob{}, err
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := c.bulkCall(ctx, http.MethodPost, "/batches", "application/json", bytes.NewReader(createBody), &created); err != nil {
		return BulkJob{}, fmt.Errorf("bulk submit: create batch: %w", err)
	}
	if created.ID == "" {
		return BulkJob{}, fmt.Errorf("bulk submit: provider 未返回作业 id")
	}
	return BulkJob{ID: created.ID, InputFileID: uploaded.ID, Keys: keys, SubmittedAt: time.Now().UTC()}, nil
}

// BulkPoll 查询作业状态(只读,免费)。
func (c *Client) BulkPoll(ctx context.Context, jobID string) (BulkJobStatus, error) {
	var decoded struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		OutputFileID  string `json:"output_file_id"`
		ErrorFileID   string `json:"error_file_id"`
		RequestCounts struct {
			Total     int `json:"total"`
			Completed int `json:"completed"`
		} `json:"request_counts"`
	}
	if err := c.bulkCall(ctx, http.MethodGet, "/batches/"+jobID, "", nil, &decoded); err != nil {
		return BulkJobStatus{}, fmt.Errorf("bulk poll %s: %w", jobID, err)
	}
	return BulkJobStatus{
		ID: decoded.ID, Status: decoded.Status,
		OutputFileID: decoded.OutputFileID, ErrorFileID: decoded.ErrorFileID,
		Done: decoded.RequestCounts.Completed, Total: decoded.RequestCounts.Total,
	}, nil
}

// BulkResult 是结果回收产物:键→向量、实耗 token(结果行 usage 求和;
// 0=provider 未回报,调用方按保守估算入账)、被显式拒绝的行数明细。
type BulkResult struct {
	Vectors      map[string][]float32
	UsageTokens  int
	RejectedKeys []string
	RejectReason map[string]string
}

// BulkFetchResults 下载并校验作业结果。expected 是该作业提交的键集
// (来自持久化的 BulkJob.Keys):未知键、维度不符、非 200 行、缺失键都
// 显式记入拒绝明细——绝不静默丢行。
func (c *Client) BulkFetchResults(ctx context.Context, status BulkJobStatus, expected []string) (BulkResult, error) {
	if status.Status != "completed" && status.Status != "partially_completed" {
		return BulkResult{}, fmt.Errorf("bulk fetch: 作业 %s 状态 %s 不可回收", status.ID, status.Status)
	}
	if status.OutputFileID == "" {
		return BulkResult{}, fmt.Errorf("bulk fetch: 作业 %s 无输出文件", status.ID)
	}
	expectSet := make(map[string]bool, len(expected))
	for _, key := range expected {
		expectSet[key] = true
	}
	result := BulkResult{
		Vectors:      make(map[string][]float32, len(expected)),
		RejectReason: map[string]string{},
	}
	consume := func(fileID string) error {
		body, err := c.bulkDownload(ctx, fileID)
		if err != nil {
			return err
		}
		defer body.Close()
		scanner := bufio.NewScanner(io.LimitReader(body, bulkMaxResultBytes))
		scanner.Buffer(make([]byte, 1<<20), 64<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			// 行形状为实跑驱动器核实的 voyage 口径:状态码在 response 层,
			// 数据在 response.body 层;单输入行使 data 恰 1 条,天然规避
			// "data[].index 是服务端全局偏移"的历史陷阱(2026-08-05 实测)。
			var row struct {
				CustomID string `json:"custom_id"`
				Response struct {
					StatusCode int `json:"status_code"`
					Body       struct {
						Data []struct {
							Embedding []float32 `json:"embedding"`
						} `json:"data"`
						Usage struct {
							TotalTokens int `json:"total_tokens"`
						} `json:"usage"`
					} `json:"body"`
				} `json:"response"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("bulk fetch: 结果行解析失败: %w", err)
			}
			if !expectSet[row.CustomID] {
				// 未知键:R03b 导入教训的产品化——归属不明的向量绝不入库。
				result.RejectedKeys = append(result.RejectedKeys, row.CustomID)
				result.RejectReason[row.CustomID] = "unknown custom_id"
				continue
			}
			if row.Response.StatusCode != 0 && row.Response.StatusCode != http.StatusOK {
				result.RejectedKeys = append(result.RejectedKeys, row.CustomID)
				result.RejectReason[row.CustomID] = fmt.Sprintf("status_code=%d", row.Response.StatusCode)
				continue
			}
			data := row.Response.Body.Data
			if len(data) != 1 || len(data[0].Embedding) != c.cfg.Dimension {
				dim := 0
				if len(data) > 0 {
					dim = len(data[0].Embedding)
				}
				result.RejectedKeys = append(result.RejectedKeys, row.CustomID)
				result.RejectReason[row.CustomID] = fmt.Sprintf("bad shape: data=%d dim=%d want=%d", len(data), dim, c.cfg.Dimension)
				continue
			}
			result.Vectors[row.CustomID] = data[0].Embedding
			result.UsageTokens += row.Response.Body.Usage.TotalTokens
		}
		return scanner.Err()
	}
	if err := consume(status.OutputFileID); err != nil {
		return BulkResult{}, err
	}
	if status.ErrorFileID != "" {
		// 错误文件里的行同样显式登记(键已知但服务端失败)。
		if err := consume(status.ErrorFileID); err != nil {
			return BulkResult{}, err
		}
	}
	for _, key := range expected {
		if _, ok := result.Vectors[key]; !ok {
			if _, rejected := result.RejectReason[key]; !rejected {
				result.RejectedKeys = append(result.RejectedKeys, key)
				result.RejectReason[key] = "missing from result files"
			}
		}
	}
	return result, nil
}

// bulkCall 执行一次批控制面请求(上传/建作业/查状态),统一鉴权、错误
// 分类与响应解码。控制面失败按既有 CallError 分类显式外抛,不重试
// (作业级幂等性由调用方的状态持久化保证,盲重试上传会产生孤儿文件)。
func (c *Client) bulkCall(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, method, c.cfg.BaseURL+path, body)
	if err != nil {
		return &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage(err.Error())}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return reliability.ClassifyTransportError(ctx, attemptCtx, c.cfg.Timeout, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reliability.ClassifyHTTPResponse(resp)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body) }()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return &reliability.CallError{Class: reliability.ClassPermanent, Message: reliability.SanitizeMessage("malformed bulk response: " + err.Error())}
	}
	return nil
}

// bulkDownload 下载结果文件内容。voyage 的 content 端点以 307 重定向到
// GCS 签名 URL(约 15 分钟时效;2026-08-05 实测):Go 默认跟随重定向,
// 且跨域自动剥除 Authorization 头——签名 URL 不需要鉴权,行为恰好正确。
// 已知边界:GCS 单连接限速明显,GB 级结果文件应并行分段下载(评测驱动
// 器已验证该优化);产品侧当前按串行整取,10 万行以内量级足够,更大
// 规模列为后续优化项(任务文档遗留)。
func (c *Client) bulkDownload(ctx context.Context, fileID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/files/"+fileID+"/content", nil)
	if err != nil {
		return nil, err
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, reliability.ClassifyTransportError(ctx, ctx, 0, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, reliability.ClassifyHTTPResponse(resp)
	}
	return resp.Body, nil
}
