package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
)

// TestEmbedBatchLatencyProbe 是 T10b 现场排障探针（env 门控，CI 跳过）：
// 用生产客户端路径连打若干真实批，量化单批耗时来源。
func TestEmbedBatchLatencyProbe(t *testing.T) {
	if os.Getenv("OPENACE_EMBED_PROBE") == "" {
		t.Skip("OPENACE_EMBED_PROBE 未设置")
	}
	cfg, err := embedding.ConfigFromEnv()
	if err != nil || !cfg.Enabled {
		t.Fatalf("provider 未配置: %v %+v", err, cfg)
	}
	client, err := embedding.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 48)
	for i := range texts {
		texts[i] = fmt.Sprintf("def probe_%d(x):\n    return x * %d\n", i, i)
	}
	for round := 0; round < 4; round++ {
		start := time.Now()
		vectors, err := client.EmbedBatch(context.Background(), texts, embedding.InputDocument)
		t.Logf("round %d: %v vectors=%d err=%v", round, time.Since(start).Round(time.Millisecond), len(vectors), err)
	}
}
