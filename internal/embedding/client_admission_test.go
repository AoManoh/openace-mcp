package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbedRetriesRespectExplicitBudget(t *testing.T) {
	for _, initial := range []int{1, 4} {
		for _, kind := range []InputType{InputDocument, InputQuery} {
			for _, budget := range []string{"rpm", "tpm"} {
				t.Run(fmt.Sprintf("initial=%d/%s/%s", initial, kind, budget), func(t *testing.T) {
					var calls atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if calls.Add(1) == 1 {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						respondEmbeddings(w, []string{"a"}, 2)
					}))
					defer srv.Close()
					cfg := testConfig(srv.URL, 2)
					cfg.InitialConcurrency = initial
					cfg.MaxRetries = 1
					if budget == "rpm" {
						cfg.RPMBudget = 1
					} else {
						cfg.TPMBudget = estimateTokens([]string{"a"})
					}
					client, err := NewClient(cfg)
					if err != nil {
						t.Fatal(err)
					}
					fastRetry(client)
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					_, err = client.EmbedBatch(ctx, []string{"a"}, kind)
					if calls.Load() != 1 || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("每次实际尝试都应计入预算: HTTP=%d err=%v", calls.Load(), err)
					}
					if client.GovernorSnapshot().InFlight != 0 {
						t.Fatal("预算等待取消后仍占用并发名额")
					}
					if client.CircuitSnapshot().ConsecutiveFailures != 0 {
						t.Fatal("调用方预算等待取消不应计为 provider 故障")
					}
				})
			}
		}
	}
}

func TestEmbedRetryBackoffReleasesConcurrency(t *testing.T) {
	for _, initial := range []int{1, 4} {
		t.Run(fmt.Sprintf("initial=%d", initial), func(t *testing.T) {
			var aCalls atomic.Int32
			bEntered := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req embedRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, "invalid test request", http.StatusBadRequest)
					return
				}
				if req.Input[0] == "A" && aCalls.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if req.Input[0] == "B" {
					bEntered <- struct{}{}
				}
				respondEmbeddings(w, req.Input, 2)
			}))
			defer srv.Close()
			cfg := testConfig(srv.URL, 2)
			cfg.InitialConcurrency, cfg.MaxRetries = initial, 1
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			retrying, releaseRetry := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(releaseRetry) }) }
			defer release()
			client.retry.Sleep = func(ctx context.Context, _ time.Duration) error {
				close(retrying)
				select {
				case <-releaseRetry:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			aDone, bDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := client.EmbedBatch(ctx, []string{"A"}, InputDocument); aDone <- err }()
			select {
			case <-retrying:
			case <-ctx.Done():
				t.Fatal("第一批未进入退避")
			}
			occupied := client.GovernorSnapshot().InFlight
			go func() { _, err := client.EmbedBatch(ctx, []string{"B"}, InputDocument); bDone <- err }()
			blocked := false
			select {
			case <-bEntered:
			case <-time.After(100 * time.Millisecond):
				blocked = true
			}
			release()
			if err := <-aDone; err != nil {
				t.Fatal(err)
			}
			if err := <-bDone; err != nil {
				t.Fatal(err)
			}
			if occupied != 0 || blocked {
				t.Fatalf("退避期间应允许其他批执行: occupied=%d blocked=%t", occupied, blocked)
			}
			if client.GovernorSnapshot().InFlight != 0 {
				t.Fatal("全部尝试结束后仍占用并发名额")
			}
		})
	}
}

func TestEmbedBudgetWaitAllowsSmallerIndexRequest(t *testing.T) {
	for _, initial := range []int{1, 4} {
		t.Run(fmt.Sprintf("initial=%d", initial), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req embedRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, "invalid test request", http.StatusBadRequest)
					return
				}
				respondEmbeddings(w, req.Input, 2)
			}))
			defer srv.Close()
			cfg := testConfig(srv.URL, 2)
			cfg.InitialConcurrency, cfg.TPMBudget = initial, 5
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			large := []string{"123456789012"}
			if _, err := client.EmbedBatch(context.Background(), large, InputDocument); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.EmbedBatch(ctx, large, InputDocument); done <- err }()
			// 留出调度时间让大请求进入预算等待；小请求仍应使用剩余一个 token。
			time.Sleep(20 * time.Millisecond)
			smallCtx, smallCancel := context.WithTimeout(context.Background(), time.Second)
			defer smallCancel()
			_, smallErr := client.EmbedBatch(smallCtx, []string{"a"}, InputDocument)
			cancel()
			largeErr := <-done
			if smallErr != nil || !errors.Is(largeErr, context.Canceled) || calls.Load() != 2 {
				t.Fatalf("预算等待阻塞其他可执行请求: small=%v large=%v calls=%d", smallErr, largeErr, calls.Load())
			}
			if client.GovernorSnapshot().InFlight != 0 {
				t.Fatal("预算等待结束后名额未归还")
			}
		})
	}
}

func TestEmbedHTTPTimeoutRetryReleasesAndCountsAdmission(t *testing.T) {
	for _, initial := range []int{1, 4} {
		t.Run(fmt.Sprintf("initial=%d", initial), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req embedRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, "invalid test request", http.StatusBadRequest)
					return
				}
				if calls.Add(1) == 1 {
					<-r.Context().Done()
					return
				}
				respondEmbeddings(w, req.Input, 2)
			}))
			defer srv.Close()
			cfg := testConfig(srv.URL, 2)
			cfg.InitialConcurrency, cfg.MaxRetries, cfg.RPMBudget = initial, 1, 2
			cfg.Timeout = 30 * time.Millisecond
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			fastRetry(client)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := client.EmbedBatch(ctx, []string{"a"}, InputDocument); err != nil {
				t.Fatal(err)
			}
			lastCtx, lastCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer lastCancel()
			_, err = client.EmbedBatch(lastCtx, []string{"b"}, InputDocument)
			if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 2 {
				t.Fatalf("超时重试未消耗两个预算或多发请求: calls=%d err=%v", calls.Load(), err)
			}
			if client.GovernorSnapshot().InFlight != 0 {
				t.Fatal("超时或预算取消后仍占名额")
			}
		})
	}
}
