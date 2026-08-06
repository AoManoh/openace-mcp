package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// daemon HTTP client 对上层暴露与本地引擎一致的通用 contract。
var (
	_ engine.Service            = (*Client)(nil)
	_ engine.WorkspaceInspector = (*Client)(nil)
)

type Client struct {
	baseURL string
	http    *http.Client
	token   string

	capMu                     sync.Mutex
	providerProfilesSupported bool
}

type healthResponse struct {
	Status       string          `json:"status"`
	Service      string          `json:"service"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

func NewClient(addr string) *Client {
	return &Client{
		baseURL: baseURL(addr),
		http: &http.Client{
			Timeout: 30 * time.Minute,
		},
		token: clientToken(addr),
	}
}

// clientToken 解析客户端凭据(M5,与服务端 resolveAuthToken 对偶):
// env=off → 空;env 非空 → 显式值;env 未设 → 默认档走与服务端相同的
// load-or-create——文件缺失时由先到方原子创建,wrapper/daemon 无论谁先
// 启动都收敛到同一 token。历史实现在文件缺失时缓存空 token,全新机器
// 首启(wrapper 先构造 client、daemon 随后自建 token)健康探测永远 401
// (review -15 灰度前置缺陷)。对 off 档或历史无认证 daemon,多带
// Authorization 头无害(服务端零认证路径不校验)。
func clientToken(addr string) string {
	env := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_TOKEN"))
	if strings.EqualFold(env, tokenModeOff) {
		return ""
	}
	if env != "" {
		return env
	}
	_, token, err := LoadOrCreateToken(addr)
	if err != nil {
		return ""
	}
	return token
}

func (c *Client) Health(ctx context.Context) error {
	_, err := c.health(ctx)
	return err
}

func (c *Client) Endpoint() string {
	return c.baseURL
}

func (c *Client) DaemonStatus(ctx context.Context) (Status, error) {
	var result Status
	if err := c.get(ctx, "/v1/daemon/status", &result); err != nil {
		return Status{}, err
	}
	if result.Status != "ok" || result.Service != "openace-daemon" {
		return Status{}, fmt.Errorf("daemon /v1/daemon/status returned unexpected service %q with status %q", result.Service, result.Status)
	}
	return result, nil
}

func (c *Client) health(ctx context.Context) (healthResponse, error) {
	var result healthResponse
	if err := c.get(ctx, "/healthz", &result); err != nil {
		return healthResponse{}, err
	}
	if result.Status != "ok" || result.Service != "openace-daemon" {
		return healthResponse{}, fmt.Errorf("daemon /healthz returned unexpected service %q with status %q", result.Service, result.Status)
	}
	return result, nil
}

func (c *Client) ensureProviderProfiles(ctx context.Context, providerProfileID string) error {
	if strings.TrimSpace(providerProfileID) == "" {
		return nil
	}
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.providerProfilesSupported {
		return nil
	}
	health, err := c.health(ctx)
	if err != nil {
		return err
	}
	if !health.Capabilities["provider_profiles"] {
		return fmt.Errorf("daemon does not advertise provider profile support; restart the openACE daemon")
	}
	c.providerProfilesSupported = true
	return nil
}

// Sync 实现 engine.Service：把同步请求转发给 daemon HTTP API（wire 格式不变）。
func (c *Client) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	var result engine.Result
	providerProfileID := strings.TrimSpace(req.Workspace.ProviderProfileID)
	if err := c.ensureProviderProfiles(ctx, providerProfileID); err != nil {
		return result, err
	}
	err := c.post(ctx, "/v1/sync", syncRequest{
		DirectoryPath:     req.Workspace.DirectoryPath,
		ProviderProfileID: providerProfileID,
	}, &result)
	return result, err
}

// Search 实现 engine.Service：把检索请求转发给 daemon HTTP API（wire 格式不变）。
func (c *Client) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	var result engine.Result
	providerProfileID := strings.TrimSpace(req.Workspace.ProviderProfileID)
	if err := c.ensureProviderProfiles(ctx, providerProfileID); err != nil {
		return result, err
	}
	err := c.post(ctx, "/v1/retrieve", retrieveRequest{
		DirectoryPath:      req.Workspace.DirectoryPath,
		ProviderProfileID:  providerProfileID,
		InformationRequest: req.Query,
		MaxOutputLength:    req.MaxOutputLen,
	}, &result)
	return result, err
}

func (c *Client) ListWorkspaceStatuses(ctx context.Context) ([]engine.WorkspaceStatus, error) {
	var result struct {
		Workspaces []engine.WorkspaceStatus `json:"workspaces"`
	}
	err := c.get(ctx, "/v1/workspaces", &result)
	return result.Workspaces, err
}

// WorkspaceStatus 实现 engine.WorkspaceInspector。
func (c *Client) WorkspaceStatus(ctx context.Context, ref engine.WorkspaceRef) (engine.WorkspaceStatus, error) {
	var result engine.WorkspaceStatus
	providerProfileID := strings.TrimSpace(ref.ProviderProfileID)
	if err := c.ensureProviderProfiles(ctx, providerProfileID); err != nil {
		return result, err
	}
	err := c.post(ctx, "/v1/workspace/status", workspaceStatusRequest{
		DirectoryPath:     ref.DirectoryPath,
		ProviderProfileID: providerProfileID,
	}, &result)
	return result, err
}

func (c *Client) StartTask(ctx context.Context, req TaskRequest) (TaskSnapshot, error) {
	var result TaskSnapshot
	if err := c.ensureProviderProfiles(ctx, req.ProviderProfileID); err != nil {
		return result, err
	}
	err := c.post(ctx, "/v1/tasks", req, &result)
	return result, err
}

func (c *Client) ListTasks(ctx context.Context, limit int) ([]TaskSnapshot, error) {
	path := "/v1/tasks"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var result struct {
		Tasks []TaskSnapshot `json:"tasks"`
	}
	err := c.get(ctx, path, &result)
	return result.Tasks, err
}

func (c *Client) TaskStatus(ctx context.Context, id string) (TaskSnapshot, error) {
	var result TaskSnapshot
	err := c.get(ctx, "/v1/tasks/"+url.PathEscape(id), &result)
	return result, err
}

func (c *Client) CancelTask(ctx context.Context, id string) (TaskSnapshot, error) {
	var result TaskSnapshot
	err := c.post(ctx, "/v1/tasks/"+url.PathEscape(id)+"/cancel", map[string]any{}, &result)
	return result, err
}

func (c *Client) post(ctx context.Context, path string, reqBody any, out any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "openace-mcp-shim/0.1")
	c.authorize(req)
	return c.do(req, path, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("user-agent", "openace-mcp-shim/0.1")
	c.authorize(req)
	return c.do(req, path, out)
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("authorization", "Bearer "+c.token)
	}
}

func (c *Client) do(req *http.Request, path string, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("daemon %s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func baseURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultAddr
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/")
	}
	return "http://" + strings.TrimRight(addr, "/")
}
