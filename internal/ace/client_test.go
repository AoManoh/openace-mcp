package ace

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

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
