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
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/auth"
)

type SessionLoader interface {
	Load(context.Context) (auth.Session, error)
}

type Client struct {
	loader SessionLoader
	http   *http.Client
	mu     sync.Mutex
	health HealthSnapshot
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
	endpoint           string
	status             int
	body               string
	requestShape       string
	retryAfterDuration time.Duration
	receivedAt         time.Time
}

func (e apiError) Error() string {
	if e.requestShape != "" {
		return fmt.Sprintf("%s returned HTTP %d: %s (%s)", e.endpoint, e.status, e.body, e.requestShape)
	}
	return fmt.Sprintf("%s returned HTTP %d: %s", e.endpoint, e.status, e.body)
}

// HealthSnapshot summarizes the latest upstream ACE availability signal.
type HealthSnapshot struct {
	Status         string
	LastStatusCode int
	LastError      string
	RetryAfter     time.Duration
	BackoffUntil   *time.Time
	LastFailureAt  *time.Time
	LastSuccessAt  *time.Time
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
		if err := c.waitForBackoff(ctx); err != nil {
			return err
		}
		err := c.postOnce(ctx, session, endpoint, payload, out)
		if err == nil {
			c.recordSuccess()
			return nil
		}
		lastErr = err
		if !retryable(err) || attempt == 2 {
			delay := time.Duration(0)
			if retryable(err) {
				delay = retryDelay(err, attempt)
			}
			c.recordFailure(err, delay)
			return err
		}
		delay := retryDelay(err, attempt)
		c.recordFailure(err, delay)
		if isRateLimited(err) {
			return err
		}
		if err := sleep(ctx, delay); err != nil {
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
		retryAfter := resp.Header.Get("Retry-After")
		delay, _ := parseRetryAfter(retryAfter, time.Now().UTC())
		return apiError{
			endpoint:           endpoint,
			status:             resp.StatusCode,
			body:               trimForError(data),
			requestShape:       requestPayloadShape(endpoint, payload),
			retryAfterDuration: delay,
			receivedAt:         time.Now().UTC(),
		}
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
		return api.status == 499 || api.status == http.StatusTooManyRequests || api.status >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "tls handshake timeout") || strings.Contains(text, "temporary")
}

func retryDelay(err error, attempt int) time.Duration {
	var api apiError
	if errors.As(err, &api) && api.retryAfterDuration > 0 {
		return api.retryAfterDuration
	}
	return time.Duration(attempt+1) * 500 * time.Millisecond
}

func isRateLimited(err error) bool {
	var api apiError
	return errors.As(err, &api) && api.status == http.StatusTooManyRequests
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		return 0, true
	}
	return delay, true
}

func (c *Client) waitForBackoff(ctx context.Context) error {
	for {
		c.mu.Lock()
		until := cloneTime(c.health.BackoffUntil)
		c.mu.Unlock()
		if until == nil {
			return nil
		}
		delay := time.Until(*until)
		if delay <= 0 {
			return nil
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) recordSuccess() {
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health.BackoffUntil != nil && now.Before(*c.health.BackoffUntil) {
		c.health.LastSuccessAt = &now
		return
	}
	c.health.Status = "ok"
	c.health.LastError = ""
	c.health.LastStatusCode = 0
	c.health.RetryAfter = 0
	c.health.BackoffUntil = nil
	c.health.LastSuccessAt = &now
}

func (c *Client) recordFailure(err error, delay time.Duration) {
	now := time.Now().UTC()
	snapshot := HealthSnapshot{
		Status:        "degraded",
		LastError:     redactSensitive(err.Error()),
		RetryAfter:    delay,
		LastFailureAt: &now,
	}
	var api apiError
	if errors.As(err, &api) {
		snapshot.LastStatusCode = api.status
		if api.receivedAt.IsZero() {
			api.receivedAt = now
		}
		snapshot.LastFailureAt = cloneTimeValue(api.receivedAt)
		if api.status == http.StatusTooManyRequests {
			snapshot.Status = "backoff"
		}
	}
	if delay > 0 {
		until := now.Add(delay).UTC()
		snapshot.BackoffUntil = &until
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health.LastSuccessAt != nil {
		snapshot.LastSuccessAt = cloneTime(c.health.LastSuccessAt)
	}
	if snapshot.BackoffUntil == nil && c.health.BackoffUntil != nil && time.Now().Before(*c.health.BackoffUntil) {
		snapshot.BackoffUntil = cloneTime(c.health.BackoffUntil)
		snapshot.RetryAfter = time.Until(*c.health.BackoffUntil)
		snapshot.Status = "backoff"
	}
	c.health = snapshot
}

func (c *Client) HealthSnapshot() HealthSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.health
	if snapshot.BackoffUntil != nil && !time.Now().Before(*snapshot.BackoffUntil) {
		snapshot.BackoffUntil = nil
		snapshot.RetryAfter = 0
		if snapshot.Status == "backoff" {
			snapshot.Status = "degraded"
		}
	}
	snapshot.BackoffUntil = cloneTime(snapshot.BackoffUntil)
	snapshot.LastFailureAt = cloneTime(snapshot.LastFailureAt)
	snapshot.LastSuccessAt = cloneTime(snapshot.LastSuccessAt)
	return snapshot
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}

func cloneTimeValue(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copied := value.UTC()
	return &copied
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
	if len(values) == 0 {
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

func requestPayloadShape(endpoint string, payload []byte) string {
	switch endpoint {
	case "checkpoint-blobs", "agents/codebase-retrieval":
	default:
		return ""
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return ""
	}
	blobs, ok := root["blobs"]
	if !ok {
		return ""
	}
	parts := describeBlobsPayloadShape(blobs)
	if len(parts) == 0 {
		return ""
	}
	return "request_shape=" + strings.Join(parts, " ")
}

func describeBlobsPayloadShape(raw json.RawMessage) []string {
	var blobs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blobs); err != nil {
		return []string{"blobs=" + jsonValueShape(raw)}
	}
	parts := []string{
		"blobs.added_blobs=" + jsonValueShape(blobs["added_blobs"]),
		"blobs.deleted_blobs=" + jsonValueShape(blobs["deleted_blobs"]),
	}
	if _, ok := blobs["checkpoint_id"]; ok {
		parts = append(parts, "blobs.checkpoint_id=present")
	} else {
		parts = append(parts, "blobs.checkpoint_id=absent")
	}
	return parts
}

func jsonValueShape(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "missing"
	}
	switch trimmed[0] {
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return "array(invalid)"
		}
		return fmt.Sprintf("array(len=%d)", len(values))
	case 'n':
		if bytes.Equal(trimmed, []byte("null")) {
			return "null"
		}
	case '{':
		return "object"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	}
	return "number"
}

func trimForError(data []byte) string {
	text := redactSensitive(strings.TrimSpace(string(data)))
	if len(text) > 500 {
		return text[:500]
	}
	return text
}

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization["':=\s]+bearer\s+)[A-Za-z0-9._-]+`),
	regexp.MustCompile(`(?i)(accessToken["'\s:=]+)["']?[A-Za-z0-9._-]{20,}`),
	regexp.MustCompile(`(?i)(token["'\s:=]+)["']?[A-Za-z0-9._-]{20,}`),
	regexp.MustCompile(`(?i)https://d[0-9]+\.api\.augmentcode\.com/?`),
}

func redactSensitive(text string) string {
	for _, value := range []string{
		os.Getenv("AUGMENT_TOKEN"),
		os.Getenv("AUGMENT_TENANT"),
		os.Getenv("SR_TOKEN"),
		os.Getenv("SR_TENANT"),
	} {
		value = strings.TrimSpace(value)
		if value != "" {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	for _, pattern := range sensitivePatterns {
		text = pattern.ReplaceAllString(text, "${1}[REDACTED]")
	}
	return text
}
