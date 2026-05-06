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
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type Client struct {
	baseURL string
	http    *http.Client
	token   string
}

func NewClient(addr string) *Client {
	return &Client{
		baseURL: baseURL(addr),
		http: &http.Client{
			Timeout: 30 * time.Minute,
		},
		token: strings.TrimSpace(os.Getenv("OPENACE_DAEMON_TOKEN")),
	}
}

func (c *Client) Health(ctx context.Context) error {
	var result struct {
		Status  string `json:"status"`
		Service string `json:"service"`
	}
	if err := c.get(ctx, "/healthz", &result); err != nil {
		return err
	}
	if result.Status != "ok" || result.Service != "openace-daemon" {
		return fmt.Errorf("daemon /healthz returned unexpected service %q with status %q", result.Service, result.Status)
	}
	return nil
}

func (c *Client) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	var result workspace.Result
	err := c.post(ctx, "/v1/sync", syncRequest{DirectoryPath: dir}, &result)
	return result, err
}

func (c *Client) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	var result workspace.Result
	err := c.post(ctx, "/v1/retrieve", retrieveRequest{
		DirectoryPath:      dir,
		InformationRequest: query,
		MaxOutputLength:    maxOutputLen,
	}, &result)
	return result, err
}

func (c *Client) ListWorkspaceStatuses(ctx context.Context) ([]workspace.WorkspaceStatus, error) {
	var result struct {
		Workspaces []workspace.WorkspaceStatus `json:"workspaces"`
	}
	err := c.get(ctx, "/v1/workspaces", &result)
	return result.Workspaces, err
}

func (c *Client) WorkspaceStatus(ctx context.Context, dir string) (workspace.WorkspaceStatus, error) {
	var result workspace.WorkspaceStatus
	err := c.post(ctx, "/v1/workspace/status", workspaceStatusRequest{DirectoryPath: dir}, &result)
	return result, err
}

func (c *Client) StartTask(ctx context.Context, req TaskRequest) (TaskSnapshot, error) {
	var result TaskSnapshot
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
