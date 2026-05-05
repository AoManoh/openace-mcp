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
