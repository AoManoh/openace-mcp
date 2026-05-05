package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(addr string) *Client {
	return &Client{
		baseURL: baseURL(addr),
		http: &http.Client{
			Timeout: 3 * time.Minute,
		},
	}
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

func (c *Client) StartTask(ctx context.Context, req TaskRequest) (TaskSnapshot, error) {
	var result TaskSnapshot
	err := c.post(ctx, "/v1/tasks", req, &result)
	return result, err
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
	return c.do(req, path, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("user-agent", "openace-mcp-shim/0.1")
	return c.do(req, path, out)
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
