package localengine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		embedding.EnvProvider, embedding.EnvBaseURL, embedding.EnvAPIKey, embedding.EnvVoyageAPIKey,
		embedding.EnvModel, embedding.EnvDimension, embedding.EnvBatchSize, embedding.EnvMaxConcurrency,
		embedding.EnvRPMBudget, embedding.EnvTPMBudget,
		rerank.EnvProvider, rerank.EnvBaseURL, rerank.EnvAPIKey, rerank.EnvModel, rerank.EnvMaxTokens,
		EnvRetrievalDegrade, EnvRerankDegrade,
		"OPENACE_PROVIDER_TIMEOUT", "OPENACE_PROVIDER_MAX_RETRIES",
	} {
		t.Setenv(name, "")
	}
}

func TestOptionsFromEnvDefaults(t *testing.T) {
	clearProviderEnv(t)
	opts, err := OptionsFromEnv()
	if err != nil {
		t.Fatalf("OptionsFromEnv: %v", err)
	}
	if opts.Embedding.Enabled || opts.Rerank.Enabled {
		t.Fatalf("无 key 时两个 provider 均不启用: %+v", opts)
	}
	if opts.RetrievalDegrade != DegradeAllow || opts.RerankDegrade != DegradeAllow {
		t.Fatalf("降级默认 allow: %+v", opts)
	}
	// 无 key 的默认 env 与零值 Options 指纹一致（旧 daemon 兼容基线，D11）。
	if opts.Fingerprint() != (Options{}).Fingerprint() {
		t.Fatalf("semantic off 指纹应与零值一致")
	}
}

func TestOptionsFromEnvDegradeParsing(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv(EnvRetrievalDegrade, "deny")
	t.Setenv(EnvRerankDegrade, "ALLOW")
	opts, err := OptionsFromEnv()
	if err != nil || opts.RetrievalDegrade != DegradeDeny || opts.RerankDegrade != DegradeAllow {
		t.Fatalf("降级解析: %+v err=%v", opts, err)
	}
	t.Setenv(EnvRetrievalDegrade, "maybe")
	if _, err := OptionsFromEnv(); err == nil || !strings.Contains(err.Error(), EnvRetrievalDegrade) {
		t.Fatalf("非法降级值应报错: %v", err)
	}
}

// TestFingerprintSensitivity 是暗坑 K29：指纹对语义配置敏感、对 key 与
// 运维参数不敏感。
func TestFingerprintSensitivity(t *testing.T) {
	base := Options{Embedding: embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderVoyage,
		BaseURL: "https://api.voyageai.com/v1", Model: "voyage-code-3", Dimension: 1024,
		APIKey: "key-a", BatchSize: 128, MaxConcurrency: 4, Timeout: time.Minute, MaxRetries: 5,
	}}
	fp := base.Fingerprint()

	insensitive := base
	insensitive.Embedding.APIKey = "key-b"
	insensitive.Embedding.BatchSize = 1
	insensitive.Embedding.Timeout = time.Second
	if insensitive.Fingerprint() != fp {
		t.Fatalf("key/运维参数不得影响指纹（K21/K29）")
	}

	model := base
	model.Embedding.Model = "voyage-4"
	if model.Fingerprint() == fp {
		t.Fatalf("模型变化必须改变指纹")
	}
	withRerank := base
	withRerank.Rerank = rerank.Config{Enabled: true, ProviderType: rerank.ProviderVoyage, Model: "rerank-2.5"}
	if withRerank.Fingerprint() == fp {
		t.Fatalf("rerank 配置必须改变指纹")
	}
	deny := base
	deny.RetrievalDegrade = DegradeDeny
	if deny.Fingerprint() == fp {
		t.Fatalf("降级开关必须改变指纹")
	}
}

// TestCloseCancelsInflightBuild 是 review S6：Close 取消在飞构建并等待
// 退出，不遗留出网调用与 staging。
func TestCloseCancelsInflightBuild(t *testing.T) {
	const dim = 8
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer func() { close(release); ts.Close() }()

	e := newTestEngineWith(t, embedOptions(ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	syncDone := make(chan error, 1)
	go func() {
		_, err := e.Sync(context.Background(), syncRequest(root))
		syncDone <- err
	}()
	<-started

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(closeCtx); err != nil {
		t.Fatalf("Close 应在构建取消后返回: %v", err)
	}
	select {
	case err := <-syncDone:
		if err == nil {
			t.Fatalf("被取消的构建不应成功")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("构建 goroutine 未退出")
	}
	// staging 干净（embedding 阶段在 staging 创建前，此处防御性断言）。
	_, workspaceKey, _ := e.resolveRoot(root)
	store, err := e.storeFor(workspaceKey)
	if err == nil {
		entries, _ := os.ReadDir(filepath.Join(store.Root(), "staging"))
		if len(entries) != 0 {
			t.Fatalf("staging 应为空: %d", len(entries))
		}
	}
}
