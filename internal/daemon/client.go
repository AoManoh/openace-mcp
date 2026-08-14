package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/runtimeinfo"
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
	// tokenErr 记录默认档 token 读取失败的原因(P12,review 二批:此前
	// return "" 吞错,所有请求以无解释 401 呈现);随 401 错误外显。
	tokenErr error

	capMu                     sync.Mutex
	providerProfilesSupported bool
	// recover 是连接拒绝时的恢复回调(SetRecoverHook;managed 重拉)。
	recover func(context.Context) error

	// expected* 是 managed Connect 成功时绑定的运行身份。后续每个
	// daemon-backed 响应利用自带 served_by 零额外请求复验,防会话
	// 中途 daemon 换血后旧 wrapper 静默读取新 wire 并剥未知字段。
	identityExpected bool
	expectedBuild    buildinfo.Info
	expectedEngine   string
	expectedProfile  string
}

type healthResponse struct {
	Status       string          `json:"status"`
	Service      string          `json:"service"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

func NewClient(addr string) *Client {
	token, tokenErr := clientToken(addr)
	return &Client{
		baseURL: baseURL(addr),
		http: &http.Client{
			Timeout: 30 * time.Minute,
		},
		token:    token,
		tokenErr: tokenErr,
	}
}

// clientToken 解析客户端凭据(M5,与服务端 resolveAuthToken 对偶):
// env=off → 空;env 非空 → 显式值;env 未设 → 默认档走与服务端相同的
// load-or-create——文件缺失时由先到方原子创建,wrapper/daemon 无论谁先
// 启动都收敛到同一 token。历史实现在文件缺失时缓存空 token,全新机器
// 首启(wrapper 先构造 client、daemon 随后自建 token)健康探测永远 401
// (review -15 灰度前置缺陷)。对 off 档或历史无认证 daemon,多带
// Authorization 头无害(服务端零认证路径不校验)。读取失败不再吞错
// (P12):记录原因,请求撞 401 时随错误外显。
func clientToken(addr string) (string, error) {
	env := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_TOKEN"))
	if strings.EqualFold(env, tokenModeOff) {
		return "", nil
	}
	if env != "" {
		return env, nil
	}
	_, token, err := LoadOrCreateToken(addr)
	if err != nil {
		return "", err
	}
	return token, nil
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
		Detail:             req.Detail,
		PathPrefix:         req.PathPrefix,
	}, &result)
	return result, err
}

// RepoMap 实现 engine.RepoMapper(经 daemon 透传;旧 daemon 无该端点
// 时 404 原文上抛,提示升级)。
func (c *Client) RepoMap(ctx context.Context, req engine.RepoMapRequest) (engine.Result, error) {
	var result engine.Result
	err := c.post(ctx, "/v1/repo-map", repoMapRequest{
		DirectoryPath:   req.Workspace.DirectoryPath,
		MaxOutputLength: req.MaxOutputLen,
		Focus:           req.Focus,
	}, &result)
	return result, err
}

func (c *Client) ListWorkspaceStatuses(ctx context.Context) ([]engine.WorkspaceStatus, error) {
	var result struct {
		Workspaces []engine.WorkspaceStatus `json:"workspaces"`
	}
	err := c.get(ctx, "/v1/workspaces", &result)
	if err == nil {
		for i := range result.Workspaces {
			if err = c.validateServedBy(result.Workspaces[i].ServedBy); err != nil {
				break
			}
		}
	}
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
	if err == nil {
		for i := range result.Tasks {
			if err = c.validateServedBy(result.Tasks[i].ServedBy); err != nil {
				break
			}
		}
	}
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

// SetExpectedIdentity 绑定 managed Connect 已复验的 build/engine/profile。
// manual-daemon 不调用本方法,生命周期与兼容责任仍归用户。
func (c *Client) SetExpectedIdentity(build buildinfo.Info, engineID string, profile string) {
	c.expectedBuild = build
	c.expectedEngine = engineID
	c.expectedProfile = profile
	c.identityExpected = true
}

// validateServedBy 对响应自带身份做零额外请求复验。served_by 缺失保持
// 兼容(旧 daemon/测试 fixture);一旦明确携带且冲突即 fail-closed。
func (c *Client) validateServedBy(served *runtimeinfo.ServedBy) error {
	if !c.identityExpected || served == nil || served.Service == "" {
		return nil
	}
	mismatch := ""
	if served.Service != "openace-daemon" {
		mismatch = fmt.Sprintf("service %q", served.Service)
	} else if c.expectedBuild.VCSRevision != "" && served.Build.VCSRevision != "" && c.expectedBuild.VCSRevision != served.Build.VCSRevision {
		mismatch = fmt.Sprintf("wrapper revision %s != daemon revision %s", c.expectedBuild.VCSRevision, served.Build.VCSRevision)
	} else if c.expectedBuild.Version != "" && served.Build.Version != "" && c.expectedBuild.Version != "(devel)" && served.Build.Version != "(devel)" && c.expectedBuild.Version != served.Build.Version {
		mismatch = fmt.Sprintf("wrapper version %s != daemon version %s", c.expectedBuild.Version, served.Build.Version)
	} else if c.expectedEngine != "" && served.Engine != "" && c.expectedEngine != served.Engine {
		mismatch = fmt.Sprintf("wrapper engine %q != daemon engine %q", c.expectedEngine, served.Engine)
	} else if c.expectedProfile != "" && served.EngineProfile != "" && c.expectedProfile != served.EngineProfile {
		mismatch = fmt.Sprintf("wrapper engine profile %s != daemon engine profile %s", c.expectedProfile, served.EngineProfile)
	}
	if mismatch == "" {
		return nil
	}
	return fmt.Errorf("%s: %s; restart the MCP session or restore the expected daemon", engine.DaemonIdentityChangedMarker, mismatch)
}

func (c *Client) validateDecodedIdentity(out any) error {
	switch value := out.(type) {
	case *engine.Result:
		return c.validateServedBy(value.ServedBy)
	case *engine.WorkspaceStatus:
		return c.validateServedBy(value.ServedBy)
	case *TaskSnapshot:
		return c.validateServedBy(value.ServedBy)
	case *Status:
		return c.validateServedBy(&value.ServedBy)
	}
	return nil
}

// SetRecoverHook 注册连接拒绝时的恢复回调(managed 模式接线:daemon 死亡
// 后重拉)。回调成功即对同一请求重试一次;nil 或回调失败保持原错误。
// 修复灰度/自食共同暴露的可靠性缺口:daemon 崩溃后 wrapper 内所有调用
// 永远 connection refused,用户只能重启 IDE 会话。
func (c *Client) SetRecoverHook(fn func(context.Context) error) {
	c.recover = fn
}

func (c *Client) post(ctx context.Context, path string, reqBody any, out any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	send := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("user-agent", "openace-mcp-shim/0.1")
		c.authorize(req)
		return c.do(req, path, out)
	}
	return c.withRecover(ctx, send)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	send := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("user-agent", "openace-mcp-shim/0.1")
		c.authorize(req)
		return c.do(req, path, out)
	}
	return c.withRecover(ctx, send)
}

// withRecover 执行请求;连接拒绝且注册了恢复回调时,恢复成功后重试一次。
func (c *Client) withRecover(ctx context.Context, send func() error) error {
	err := send()
	if err == nil || c.recover == nil || ctx.Err() != nil || !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	if recoverErr := c.recover(ctx); recoverErr != nil {
		return fmt.Errorf("%w(daemon 重拉失败: %v)", err, recoverErr)
	}
	return send()
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("authorization", "Bearer "+c.token)
	}
}

// maxResponseBodyBytes 是客户端读取 daemon 响应体的上限(本地 DoS 面
// 收敛;与服务端请求体上限同量级)。
const maxResponseBodyBytes = 4 << 20

func (c *Client) do(req *http.Request, path string, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 多读 1 字节以区分"恰好等于上限"与"被截断"(P6:此前满额截断后
	// decode 报裸 unexpected end of JSON input,无端点、无恢复提示)。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if err != nil {
		return err
	}
	truncated := len(data) > maxResponseBodyBytes
	if truncated {
		data = data[:maxResponseBodyBytes]
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("daemon %s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
		// P12:默认 token 档读取失败此前被静默吞掉,401 无从解释;
		// 把失败原因与恢复路径附在错误上。
		if resp.StatusCode == http.StatusUnauthorized && c.tokenErr != nil {
			message += fmt.Sprintf("(wrapper 侧 daemon token 读取失败: %v;可显式设 OPENACE_DAEMON_TOKEN 或修复权限)", c.tokenErr)
		}
		return errors.New(message)
	}
	if truncated {
		return fmt.Errorf("daemon %s response exceeded 4MiB and was truncated; retry with a smaller limit or narrower request", path)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("daemon %s: decode response: %w", path, err)
	}
	if err := c.validateDecodedIdentity(out); err != nil {
		return err
	}
	return nil
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
