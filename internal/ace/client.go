package ace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/auth"
)

type SessionLoader interface {
	Load(context.Context) (auth.Session, error)
}

type Client struct {
	loader SessionLoader
	http   *http.Client
}

func NewClient(loader SessionLoader) *Client {
	return &Client{
		loader: loader,
		http: &http.Client{
			Timeout: 3 * time.Minute,
		},
	}
}

type BlobUpload struct {
	BlobName string
	Path     string
	Content  string
}

type RetrievalOptions struct {
	CheckpointID string
	AddedBlobs   []string
	DeletedBlobs []string
	MaxOutputLen int
}

type apiError struct {
	endpoint string
	status   int
	body     string
}

func (e apiError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d: %s", e.endpoint, e.status, e.body)
}

func (c *Client) FindMissing(ctx context.Context, blobNames []string) ([]string, []string, error) {
	var resp struct {
		UnknownMemoryNames []string `json:"unknown_memory_names"`
		UnknownBlobNames   []string `json:"unknownBlobNames"`
		NonindexedNames    []string `json:"nonindexed_blob_names"`
		NonindexedBlobs    []string `json:"nonindexedBlobNames"`
	}
	if err := c.post(ctx, "find-missing", map[string]any{
		"mem_object_names": blobNames,
	}, &resp); err != nil {
		return nil, nil, err
	}
	unknown := append(resp.UnknownMemoryNames, resp.UnknownBlobNames...)
	nonindexed := append(resp.NonindexedNames, resp.NonindexedBlobs...)
	return unique(unknown), unique(nonindexed), nil
}

func (c *Client) BatchUpload(ctx context.Context, blobs []BlobUpload) error {
	payload := make([]map[string]string, 0, len(blobs))
	for _, blob := range blobs {
		payload = append(payload, map[string]string{
			"blob_name": blob.BlobName,
			"path":      blob.Path,
			"content":   blob.Content,
		})
	}
	var resp any
	return c.post(ctx, "batch-upload", map[string]any{"blobs": payload}, &resp)
}

func (c *Client) CheckpointBlobs(ctx context.Context, checkpointID string, added []string, deleted []string) (string, error) {
	var resp struct {
		NewCheckpointID string `json:"new_checkpoint_id"`
		NewCheckpoint   string `json:"newCheckpointId"`
	}
	if err := c.post(ctx, "checkpoint-blobs", map[string]any{
		"blobs": blobsPayload(checkpointID, added, deleted),
	}, &resp); err != nil {
		return "", err
	}
	if resp.NewCheckpointID != "" {
		return resp.NewCheckpointID, nil
	}
	return resp.NewCheckpoint, nil
}

func (c *Client) CodebaseRetrieval(ctx context.Context, informationRequest string, opts RetrievalOptions) (string, error) {
	var resp struct {
		FormattedRetrieval string `json:"formatted_retrieval"`
		FormattedCamel     string `json:"formattedRetrieval"`
	}
	if err := c.post(ctx, "agents/codebase-retrieval", map[string]any{
		"information_request":           informationRequest,
		"blobs":                         blobsPayload(opts.CheckpointID, opts.AddedBlobs, opts.DeletedBlobs),
		"dialog":                        []any{},
		"max_output_length":             opts.MaxOutputLen,
		"disable_codebase_retrieval":    false,
		"enable_commit_retrieval":       false,
		"enable_conversation_retrieval": false,
	}, &resp); err != nil {
		return "", err
	}
	if resp.FormattedRetrieval != "" {
		return resp.FormattedRetrieval, nil
	}
	return resp.FormattedCamel, nil
}

func (c *Client) post(ctx context.Context, endpoint string, body any, out any) error {
	session, err := c.loader.Load(ctx)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		err := c.postOnce(ctx, session, endpoint, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) || attempt == 2 {
			return err
		}
		if err := sleep(ctx, time.Duration(attempt+1)*500*time.Millisecond); err != nil {
			return err
		}
	}
	return lastErr
}

func (c *Client) postOnce(ctx context.Context, session auth.Session, endpoint string, payload []byte, out any) error {
	url := strings.TrimRight(session.TenantURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+session.AccessToken)
	req.Header.Set("user-agent", "openace-mcp/0.1")
	req.Header.Set("x-request-id", randomID())
	req.Header.Set("x-request-session-id", randomID())

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
		return apiError{endpoint: endpoint, status: resp.StatusCode, body: trimForError(data)}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	return nil
}

func retryable(err error) bool {
	var api apiError
	if errors.As(err, &api) {
		return api.status == http.StatusTooManyRequests || api.status >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "tls handshake timeout") || strings.Contains(text, "temporary")
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func jsonStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func blobsPayload(checkpointID string, added []string, deleted []string) map[string]any {
	payload := map[string]any{
		"added_blobs":   jsonStrings(added),
		"deleted_blobs": jsonStrings(deleted),
	}
	if checkpointID != "" {
		payload["checkpoint_id"] = checkpointID
	}
	return payload
}

func trimForError(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) > 500 {
		return text[:500]
	}
	return text
}
