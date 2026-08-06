package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// R2(review 2026-08-06):dump 首行携带嵌入身份头,与 import 输入行
// 同构解析——身份头可被 importResultLine 误读为空 custom_id 的向量行,
// 因此 import 端必须先识别并消费它;跨身份(profile/dim 不符)拒绝。
func TestVectorsDumpHeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vectors.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"identity": map[string]any{
		"embedding_profile_hash": "abc123",
		"embedding_model":        "model-a",
		"embedding_dimension":    8,
	}})
	f.Write(header)
	f.WriteString("\n")
	row, _ := json.Marshal(importResultLine{CustomID: "k1", Embedding: make([]float32, 8)})
	f.Write(row)
	f.WriteString("\n")
	f.Close()

	// 读回:首行应解析为身份头(Identity 非空),第二行为向量行。
	rf, _ := os.Open(path)
	defer rf.Close()
	scanner := bufio.NewScanner(rf)
	scanner.Scan()
	var h vectorsIdentityHeader
	if err := json.Unmarshal(scanner.Bytes(), &h); err != nil || h.Identity == nil {
		t.Fatalf("首行应为身份头: %v %s", err, scanner.Text())
	}
	if h.Identity.ProfileHash != "abc123" || h.Identity.Dimension != 8 {
		t.Fatalf("身份头字段不符: %+v", h.Identity)
	}
	scanner.Scan()
	var line importResultLine
	if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line.CustomID != "k1" {
		t.Fatalf("第二行应为向量行: %v", err)
	}
	// 向量行不得被误判为身份头(Identity 必为空)。
	var asHeader vectorsIdentityHeader
	_ = json.Unmarshal(scanner.Bytes(), &asHeader)
	if asHeader.Identity != nil {
		t.Fatal("向量行被误判为身份头")
	}
}
