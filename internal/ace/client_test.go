package ace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/auth"
)

type staticSessionLoader struct {
	session auth.Session
}

func (l staticSessionLoader) Load(ctx context.Context) (auth.Session, error) {
	if err := ctx.Err(); err != nil {
		return auth.Session{}, err
	}
	return l.session, nil
}

func TestBlobsPayloadOmitsEmptyCheckpointAndUsesArrays(t *testing.T) {
	payload := blobsPayload("", []string{"b", "a"}, nil)

	if _, ok := payload["checkpoint_id"]; ok {
		t.Fatalf("checkpoint_id should be omitted when empty")
	}
	if got := payload["added_blobs"]; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("added_blobs = %#v", got)
	}
	if got := payload["deleted_blobs"]; !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("deleted_blobs = %#v", got)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"deleted_blobs":null`)) {
		t.Fatalf("deleted_blobs encoded as null: %s", data)
	}
}

func TestBlobsPayloadKeepsCheckpoint(t *testing.T) {
	payload := blobsPayload("cp-1", nil, []string{"z", "x"})

	if got := payload["checkpoint_id"]; got != "cp-1" {
		t.Fatalf("checkpoint_id = %#v", got)
	}
	if got := payload["added_blobs"]; !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("added_blobs = %#v", got)
	}
	if got := payload["deleted_blobs"]; !reflect.DeepEqual(got, []string{"x", "z"}) {
		t.Fatalf("deleted_blobs = %#v", got)
	}
}

func TestTrimForErrorRedactsSensitiveValues(t *testing.T) {
	t.Setenv("AUGMENT_TOKEN", "fake-token-value-abcdefghijklmnopqrstuvwxyz")
	t.Setenv("AUGMENT_TENANT", "https://tenant.example.invalid/")

	text := trimForError([]byte(`{"accessToken":"fake-token-value-abcdefghijklmnopqrstuvwxyz","url":"https://tenant.example.invalid/","authorization":"Bearer fake-token-value-abcdefghijklmnopqrstuvwxyz"}`))
	if bytes.Contains([]byte(text), []byte("fake-token-value")) {
		t.Fatalf("token was not redacted: %s", text)
	}
	if bytes.Contains([]byte(text), []byte("tenant.example.invalid")) {
		t.Fatalf("tenant was not redacted: %s", text)
	}
	if !bytes.Contains([]byte(text), []byte("[REDACTED]")) {
		t.Fatalf("redacted marker missing: %s", text)
	}
}

func TestRetryableIncludesTransientGatewayStatuses(t *testing.T) {
	for _, status := range []int{499, 429, 500, 502, 503} {
		if !retryable(apiError{endpoint: "agents/codebase-retrieval", status: status}) {
			t.Fatalf("status %d should be retryable", status)
		}
	}
	if retryable(apiError{endpoint: "agents/codebase-retrieval", status: 400}) {
		t.Fatal("status 400 should not be retryable")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	if got, ok := parseRetryAfter("3", now); !ok || got != 3*time.Second {
		t.Fatalf("seconds retry-after = %v, %v", got, ok)
	}
	when := now.Add(5 * time.Second).Format(http.TimeFormat)
	if got, ok := parseRetryAfter(when, now); !ok || got != 5*time.Second {
		t.Fatalf("date retry-after = %v, %v", got, ok)
	}
	if got, ok := parseRetryAfter("", now); ok || got != 0 {
		t.Fatalf("empty retry-after = %v, %v", got, ok)
	}
	if got, ok := parseRetryAfter("not-a-date", now); ok || got != 0 {
		t.Fatalf("invalid retry-after = %v, %v", got, ok)
	}
}

func TestClientRateLimitWithRetryAfterDoesNotRetryImmediately(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "quota exhausted", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient(staticSessionLoader{session: auth.Session{
		AccessToken: "token",
		TenantURL:   server.URL,
	}})
	_, _, err := client.FindMissing(context.Background(), []string{"blob-a"})
	if err == nil {
		t.Fatal("FindMissing should return rate limit error")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("rate-limited request should not be retried immediately, got %d requests", got)
	}
	health := client.HealthSnapshot()
	if health.Status != "backoff" || health.LastStatusCode != http.StatusTooManyRequests || health.BackoffUntil == nil {
		t.Fatalf("unexpected health snapshot: %+v", health)
	}
}

func TestClientSharedBackoffBlocksFollowupRequest(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "quota exhausted", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"unknown_memory_names":[]}`))
	}))
	defer server.Close()

	client := NewClient(staticSessionLoader{session: auth.Session{
		AccessToken: "token",
		TenantURL:   server.URL,
	}})
	_, _, err := client.FindMissing(context.Background(), []string{"blob-a"})
	if err == nil {
		t.Fatal("first FindMissing should return rate limit error")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err = client.FindMissing(ctx, []string{"blob-b"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("followup request should wait for shared backoff until context deadline, got %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("followup request should not reach upstream during backoff, got %d requests", got)
	}
}

func TestClientSuccessDoesNotClearActiveBackoff(t *testing.T) {
	client := NewClient(staticSessionLoader{session: auth.Session{AccessToken: "token", TenantURL: "https://tenant.example.invalid"}})
	client.recordFailure(apiError{
		endpoint: "find-missing",
		status:   http.StatusTooManyRequests,
		body:     "quota exhausted",
	}, time.Minute)
	client.recordSuccess()

	health := client.HealthSnapshot()
	if health.Status != "backoff" || health.BackoffUntil == nil || health.LastSuccessAt == nil {
		t.Fatalf("active backoff should survive concurrent success: %+v", health)
	}
}

func TestClientServerErrorRecordsDegradedHealth(t *testing.T) {
	client := NewClient(staticSessionLoader{session: auth.Session{AccessToken: "token", TenantURL: "https://tenant.example.invalid"}})
	client.recordFailure(apiError{
		endpoint: "find-missing",
		status:   http.StatusServiceUnavailable,
		body:     "temporarily unavailable",
	}, 2*time.Second)

	health := client.HealthSnapshot()
	if health.Status != "degraded" || health.LastStatusCode != http.StatusServiceUnavailable {
		t.Fatalf("5xx should record degraded health: %+v", health)
	}
	if health.BackoffUntil == nil || health.RetryAfter <= 0 {
		t.Fatalf("5xx should expose retry timing: %+v", health)
	}
}
