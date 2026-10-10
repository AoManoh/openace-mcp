package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// zeroVectorServer 对每条输入返回同一个向量 vec。
func zeroVectorServer(t *testing.T, vec []float32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, 0, len(req.Input))
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": vec})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestQueryEmbeddingRejectsZeroNormVector：查询向量维度正确但范数为零时，
// 客户端在计入成功之前判为无效响应：返回 permanent 类别的 CallError，查询
// 熔断器记一次失败，不再显示 healthy。
func TestQueryEmbeddingRejectsZeroNormVector(t *testing.T) {
	ts := zeroVectorServer(t, make([]float32, 4))
	client, err := NewClient(testConfig(ts.URL, 4))
	if err != nil {
		t.Fatal(err)
	}
	fastRetry(client)
	vec, err := client.EmbedQuery(context.Background(), "query text")
	callErr := asCallError(err)
	if vec != nil || callErr == nil || callErr.Class != reliability.ClassPermanent || !strings.Contains(callErr.Message, "zero norm") {
		t.Fatalf("零范数查询向量应按 permanent 无效响应返回: vec=%v err=%v", vec, err)
	}
	snapshot := client.QueryCircuitSnapshot()
	if snapshot.State == "healthy" || snapshot.ConsecutiveFailures != 1 || !strings.Contains(snapshot.LastError, "zero norm") {
		t.Fatalf("查询熔断器应记一次失败: %+v", snapshot)
	}
	if client.CircuitSnapshot().State != "healthy" {
		t.Fatalf("查询失败不得影响索引熔断器: %+v", client.CircuitSnapshot())
	}
}

// TestDocumentZeroVectorStillDelivered：文档零向量仍原样交给引擎，由引擎按
// 持久化拒绝集处理；客户端不改变这一语义。
func TestDocumentZeroVectorStillDelivered(t *testing.T) {
	ts := zeroVectorServer(t, make([]float32, 4))
	client, err := NewClient(testConfig(ts.URL, 4))
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := client.EmbedBatch(context.Background(), []string{"doc a", "doc b"}, InputDocument)
	if err != nil || len(vectors) != 2 {
		t.Fatalf("文档零向量应照常返回: %v err=%v", vectors, err)
	}
	if client.CircuitSnapshot().State != "healthy" {
		t.Fatalf("文档零向量不计 provider 失败: %+v", client.CircuitSnapshot())
	}
}

// TestDimensionProbeNotValidatedAsQueryVector：维度未知时的探测请求不做数值
// 校验，启动期维度探测的既有合同不变。
func TestDimensionProbeNotValidatedAsQueryVector(t *testing.T) {
	ts := zeroVectorServer(t, make([]float32, 6))
	client, err := NewClient(testConfig(ts.URL, 0))
	if err != nil {
		t.Fatal(err)
	}
	vec, err := client.EmbedQuery(context.Background(), "probe")
	if err != nil || len(vec) != 6 {
		t.Fatalf("维度探测应返回原始长度: len=%d err=%v", len(vec), err)
	}
}

// TestQueryVectorProblem：非有限分量与零范数都判为无效，正常向量通过。
func TestQueryVectorProblem(t *testing.T) {
	cases := map[string]struct {
		vec  []float32
		want string
	}{
		"正常":  {[]float32{0, 0.5, -1}, ""},
		"零范数": {[]float32{0, 0, 0}, "zero norm"},
		"NaN": {[]float32{1, float32(math.NaN()), 0}, "non-finite value at position 1"},
		"Inf": {[]float32{float32(math.Inf(-1)), 1, 0}, "non-finite value at position 0"},
	}
	for name, tc := range cases {
		got := queryVectorProblem(tc.vec)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Fatalf("%s: queryVectorProblem=%q want %q", name, got, tc.want)
		}
	}
}

// TestQueryEmbeddingCancellationNotProviderFailure：调用方取消查询嵌入时返回
// 取消错误，查询熔断器保持 healthy。
func TestQueryEmbeddingCancellationNotProviderFailure(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)
	client, err := NewClient(testConfig(ts.URL, 4))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = client.EmbedQuery(ctx, "query text")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回调用方的取消错误: %v", err)
	}
	if snapshot := client.QueryCircuitSnapshot(); snapshot.State != "healthy" {
		t.Fatalf("取消不计 provider 失败: %+v", snapshot)
	}
}
