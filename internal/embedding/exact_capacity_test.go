package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEmbedVectorsExactCapacity：同步嵌入返回的每条向量容量等于维度。改动前
// encoding/json 解码时逐个元素扩容，1024 维向量的容量停在 1344，构建期间全部
// 新向量多占约三成内存（第 8 步堆剖析）。
func TestEmbedVectorsExactCapacity(t *testing.T) {
	const dim = 1024
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, dim)
			vec[i%dim] = 1
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer ts.Close()
	client, err := NewClient(testConfig(ts.URL, dim))
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := client.EmbedBatchObserved(context.Background(), []string{"a", "b", "c"}, InputDocument, nil)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vectors) != 3 {
		t.Fatalf("want 3 vectors, got %d", len(vectors))
	}
	for i, vec := range vectors {
		if len(vec) != dim || cap(vec) != dim {
			t.Fatalf("vector %d: len=%d cap=%d, want both %d", i, len(vec), cap(vec), dim)
		}
		if vec[i%dim] != 1 {
			t.Fatalf("vector %d content changed", i)
		}
	}
}

// TestBulkResultVectorsExactCapacity：批作业结果文件逐行解析得到的向量容量等于
// 维度，内容与原文一致。
func TestBulkResultVectorsExactCapacity(t *testing.T) {
	const dim = 1024
	vec := make([]float32, dim)
	vec[7] = 0.5
	line, err := json.Marshal(map[string]any{
		"custom_id": "key-1",
		"response": map[string]any{
			"status_code": 200,
			"body": map[string]any{
				"data":  []map[string]any{{"embedding": vec}},
				"usage": map[string]any{"total_tokens": 3},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := decodeBulkResultRow(line, dim)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.CustomID != "key-1" || len(row.Response.Body.Data) != 1 {
		t.Fatalf("unexpected row: %+v", row)
	}
	got := exactVector(row.Response.Body.Data[0].Embedding)
	if len(got) != dim || cap(got) != dim || got[7] != 0.5 {
		t.Fatalf("len=%d cap=%d value=%v", len(got), cap(got), got[7])
	}
}
