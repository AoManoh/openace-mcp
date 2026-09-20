package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dimensionCacheFile 是探测结果的缓存文件名（放在 cache 根目录，不按 namespace 隔离：
// 维度是 provider+模型的属性，与工作区无关）。
const dimensionCacheFile = "embedding-dimensions.json"

// probeText 是维度探测用的最短文本；探测走 InputQuery 车道，一次请求、一条文本。
const probeText = "openACE embedding dimension probe"

// ResolveDimension 把 DimensionAuto 的配置补成带真实维度的配置：先查缓存
// （key = adapter|地址|模型），没有则向 provider 发一次单文本嵌入请求取向量长度，
// 成功后写缓存。显式配置了维度的配置原样返回。
//   - 探测失败返回错误（含变量名提示）：用户可设 OPENACE_EMBEDDING_DIMENSION 跳过探测。
//   - cacheDir 为空时不读写缓存，只探测。
func ResolveDimension(ctx context.Context, cfg Config, cacheDir string) (Config, error) {
	if !cfg.Enabled || !cfg.DimensionAuto || cfg.Dimension > 0 {
		return cfg, nil
	}
	key := strings.Join([]string{cfg.ProviderType, cfg.BaseURL, cfg.Model}, "|")
	if dim, ok := readDimensionCache(cacheDir, key); ok {
		cfg.Dimension = dim
		return cfg, nil
	}
	dim, err := ProbeDimension(ctx, cfg)
	if err != nil {
		return Config{}, fmt.Errorf("embedding dimension probe against %s (%s) failed: %w; set %s to skip the probe", cfg.BaseURL, cfg.Model, err, EnvDimension)
	}
	cfg.Dimension = dim
	if writeErr := writeDimensionCache(cacheDir, key, dim); writeErr != nil {
		// 缓存写失败只影响下次是否再探测一次，不影响本次启用；不静默：交给调用方记日志。
		return cfg, &CacheWriteWarning{Err: writeErr}
	}
	return cfg, nil
}

// CacheWriteWarning 表示探测成功但缓存写入失败；调用方应记日志并继续使用返回的配置。
type CacheWriteWarning struct{ Err error }

func (w *CacheWriteWarning) Error() string {
	return "embedding dimension cache write failed: " + w.Err.Error()
}
func (w *CacheWriteWarning) Unwrap() error { return w.Err }

// ProbeDimension 发送一次单文本嵌入请求并返回向量长度。用 Dimension=0 的临时配置构造
// 客户端，此时客户端跳过维度校验（见 doRequest）。
func ProbeDimension(ctx context.Context, cfg Config) (int, error) {
	probeCfg := cfg
	probeCfg.Dimension = 0
	probeCfg.BatchAPIMode = ""
	client, err := NewClient(probeCfg)
	if err != nil {
		return 0, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	vectors, err := client.EmbedBatch(probeCtx, []string{probeText}, InputQuery)
	if err != nil {
		return 0, err
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return 0, errors.New("probe returned no vector")
	}
	return len(vectors[0]), nil
}

func readDimensionCache(cacheDir, key string) (int, bool) {
	if cacheDir == "" {
		return 0, false
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, dimensionCacheFile))
	if err != nil {
		return 0, false
	}
	var table map[string]int
	if json.Unmarshal(raw, &table) != nil {
		return 0, false
	}
	dim, ok := table[key]
	return dim, ok && dim > 0
}

func writeDimensionCache(cacheDir, key string, dim int) error {
	if cacheDir == "" {
		return nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(cacheDir, dimensionCacheFile)
	table := map[string]int{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &table)
	}
	table[key] = dim
	raw, err := json.MarshalIndent(table, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
