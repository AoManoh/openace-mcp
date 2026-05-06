package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL).Health(context.Background()); err != nil {
		t.Fatalf("health should accept openace daemon: %v", err)
	}
}

func TestClientHealthRejectsUnexpectedService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"other"}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL).Health(context.Background()); err == nil {
		t.Fatal("health should reject non-openace service")
	}
}
