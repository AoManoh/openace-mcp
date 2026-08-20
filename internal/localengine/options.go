package localengine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/reliability"
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
	// EnvFreshnessWindow 是查询期新鲜度窗口（Stage 6 前置，T11 证据：
	// 查询延迟由 no-op sync 全量扫描支配）：上次成功同步距今小于窗口
	// 时跳过内联扫描，新鲜度上界=窗口时长；空/0=每查询扫描（现状）。
	// 后台 reconciler/显式 sync 不受窗口约束。
	EnvFreshnessWindow = "OPENACE_FRESHNESS_WINDOW"
	// EnvQualityStrict 是质量严格档（方案④,2026-08-02）:on 时语义链路
	// 任一缺口（覆盖<100%/查询嵌入失败/配置了 rerank 但未生效/任何
	// 降级 reason）都显式报错而非降级放行;默认 off=现状。要求 embedding
	// provider 已配置,否则构造期报错。
	EnvQualityStrict = "OPENACE_QUALITY_STRICT"
	// EnvQueryBuildWait 是查询等待在建索引的时长上界(P1 有界化):
	// 超时后构建继续后台推进,查询按「有旧 revision → allow/deny 降级
	// (原因 index-building)、无 revision → 可行动错误(带构建进度)」
	// 返回;空/0 = 现状(等到构建完成)。显式 sync 与后台任务不受约束。
	EnvQueryBuildWait = "OPENACE_QUERY_BUILD_WAIT"
)

// defaultQueryBuildWait 是查询有界等待的默认上界(P2,灰度反馈
// 2026-08-06;2026-08-11 收紧):必须小于**最严格主流 MCP 客户端**的
// 请求超时,不只我们自家 wrapper(110s)——Cursor 约 60s 即发 -32001
// 杀请求(外部灰度 F:/BA-Dev 实报),90s 预算下引擎的可行动错误(带
// 构建进度与 env 名)永远送不到调用方。40s 给大仓的每请求新鲜度
// 检查(实测 Windows 50K 文件 ~27s)留出余量后仍先于客户端超时返回。
const defaultQueryBuildWait = 40 * time.Second

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
	// FreshnessWindow 是查询期新鲜度窗口；0 = 每查询内联扫描（现状）。
	FreshnessWindow time.Duration
	// QualityStrict 开启质量严格档(方案④);要求 Embedding.Enabled。
	QualityStrict bool
	// QueryBuildWait 是查询等待在建索引的上界;0 = 无界(现状)。
	QueryBuildWait time.Duration
	// DisableLexicalFirst 关闭冷仓 lexical-first 中间发布;仅测试/
	// 诊断程序化覆盖,无环境变量,生产默认 false(即启用)。
	DisableLexicalFirst bool
	// FragmentGate 开启碎片密度门 spike(候选 (l));仅测试/bench
	// 程序化覆盖,无环境变量,默认 false。生产引入须等真实碎片语料
	// 验证与用户裁决。
	FragmentGate bool
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
	freshness, err := reliability.DurationEnv(EnvFreshnessWindow, 0)
	if err != nil {
		return Options{}, err
	}
	strict, err := parseQualityStrict()
	if err != nil {
		return Options{}, err
	}
	// P2(灰度反馈 2026-08-06;2026-08-11 收紧至 40s,见
	// defaultQueryBuildWait 注释):冷仓首建期间的同步检索在客户端
	// 请求超时之前拿到带进度的可行动错误(errQueryBuildWait 路径),
	// 而非挂到裸传输超时;显式 "0" 保留"等到构建完成"。
	buildWait := defaultQueryBuildWait
	if raw := strings.TrimSpace(os.Getenv(EnvQueryBuildWait)); raw != "" {
		if raw == "0" || raw == "0s" {
			buildWait = 0
		} else {
			parsed, err := reliability.DurationEnv(EnvQueryBuildWait, defaultQueryBuildWait)
			if err != nil {
				return Options{}, err
			}
			buildWait = parsed
		}
	}
	// 模板版本注入(M9②):env 路径与引擎构造双点注入同一常量,保证
	// wrapper/daemon 的 Fingerprint 与引擎侧 ProfileHash 同源。
	embedCfg.TemplateVersion = embedTemplateVersion
	return Options{
		Embedding:        embedCfg,
		Rerank:           rerankCfg,
		RetrievalDegrade: retrievalDegrade,
		RerankDegrade:    rerankDegrade,
		FreshnessWindow:  freshness,
		QualityStrict:    strict,
		QueryBuildWait:   buildWait,
	}, nil
}

func parseQualityStrict() (bool, error) {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(EnvQualityStrict))) {
	case "", "off", "0", "false":
		return false, nil
	case "on", "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("invalid %s %q; use \"on\" or \"off\"", EnvQualityStrict, os.Getenv(EnvQualityStrict))
	}
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
	// M9①(诊断报告 2026-08-03):QualityStrict 改变检索语义(缺口报错
	// vs 降级放行),与 degrade 开关同类,必须入指纹——否则两个 wrapper
	// 以 strict=on/off 连接同一 daemon 会静默复用,语义由先启动者决定。
	// 指纹版本升 v2(与 M9② 模板注入共用同一次失配重启窗)。
	strictComponent := "strict-off"
	if o.QualityStrict {
		strictComponent = "strict-on"
	}
	// T8:离线批车道是构建行为模式(daemon 执行构建),wrapper 与 daemon
	// 必须同模式——不入指纹会静默按先启动者的模式执行,违反"配置意图
	// 变更显式生效"的既有语义(与 degrade/strict 同类)。批车道不改向量
	// 身份(同模型同维度,批与实时 API 实测余弦=1.000000),ProfileHash
	// 与索引子树不变。指纹版本升 v3。
	bulkComponent := "bulk-off"
	if o.Embedding.BatchAPIMode != "" {
		bulkComponent = "bulk-" + o.Embedding.BatchAPIMode
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"engine-profile-v3",
		embedComponent,
		rerankComponent,
		string(normalizeDegrade(o.RetrievalDegrade)),
		string(normalizeDegrade(o.RerankDegrade)),
		strictComponent,
		bulkComponent,
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}
