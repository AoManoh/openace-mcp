package localengine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// DegradeMode 支配语义路/精排失败时的行为（决策 11：由用户 env 决定，
// 禁止静默降级）。
type DegradeMode string

const (
	// DegradeAllow 放行降级结果并强制 [DEGRADED] 首行横幅（默认）。
	DegradeAllow DegradeMode = "allow"
	// DegradeDeny 显式报错，不出结果。
	DegradeDeny DegradeMode = "deny"
)

// 环境变量名（阶段计划 §4/D8 定稿）。
const (
	EnvRetrievalDegrade = "OPENACE_RETRIEVAL_DEGRADE"
	EnvRerankDegrade    = "OPENACE_RERANK_DEGRADE"
)

// Options 是 local-hybrid 引擎的完整构造配置；零值 = Stage 2 行为
// （semantic off、rerank off、降级 allow）。
type Options struct {
	Embedding        embedding.Config
	Rerank           rerank.Config
	RetrievalDegrade DegradeMode
	RerankDegrade    DegradeMode
	// LexicalWeights 覆盖词法子句权重；nil = lexical.DefaultWeights()。
	// 仅评测 harness（openace-bench 权重扫描）使用，无对应环境变量；
	// 定值结果冻结进 DefaultWeights 而不是长期依赖本覆盖。
	LexicalWeights *lexical.Weights
	// FusionParams 覆盖 RRF 融合参数；nil = fusion.DefaultParams()。
	// 同上仅评测 harness 使用；T10b 定值经呈批后冻结进 DefaultParams。
	FusionParams *fusion.Params
}

// OptionsFromEnv 解析 local-hybrid 的 provider 与降级配置；
// 仅在 OPENACE_ENGINE=local-hybrid 时由命令入口调用（§4 边界）。
func OptionsFromEnv() (Options, error) {
	embedCfg, err := embedding.ConfigFromEnv()
	if err != nil {
		return Options{}, err
	}
	rerankCfg, err := rerank.ConfigFromEnv()
	if err != nil {
		return Options{}, err
	}
	retrievalDegrade, err := parseDegrade(EnvRetrievalDegrade)
	if err != nil {
		return Options{}, err
	}
	rerankDegrade, err := parseDegrade(EnvRerankDegrade)
	if err != nil {
		return Options{}, err
	}
	return Options{
		Embedding:        embedCfg,
		Rerank:           rerankCfg,
		RetrievalDegrade: retrievalDegrade,
		RerankDegrade:    rerankDegrade,
	}, nil
}

func parseDegrade(name string) (DegradeMode, error) {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(name))) {
	case "", string(DegradeAllow):
		return DegradeAllow, nil
	case string(DegradeDeny):
		return DegradeDeny, nil
	default:
		return "", fmt.Errorf("invalid %s %q; use %q or %q", name, os.Getenv(name), DegradeAllow, DegradeDeny)
	}
}

// normalizeDegrade 把零值归一为默认 allow。
func normalizeDegrade(mode DegradeMode) DegradeMode {
	if mode == DegradeDeny {
		return DegradeDeny
	}
	return DegradeAllow
}

// Fingerprint 计算引擎配置指纹（暗坑 K29）：覆盖 embedding profile、
// rerank 身份与降级开关；不含凭据（暗坑 K21）与运维参数（batch/并发/
// 超时/预算——它们不改变检索语义）。零值 Options 与"配置了 provider 但
// 缺 key"得到相同指纹（二者语义行为一致：semantic off）。
func (o Options) Fingerprint() string {
	embedComponent := "off"
	if o.Embedding.Enabled {
		embedComponent = o.Embedding.ProfileHash()
	}
	rerankComponent := "off"
	if o.Rerank.Enabled {
		rerankComponent = o.Rerank.Identity()
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"engine-profile-v1",
		embedComponent,
		rerankComponent,
		string(normalizeDegrade(o.RetrievalDegrade)),
		string(normalizeDegrade(o.RerankDegrade)),
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}
