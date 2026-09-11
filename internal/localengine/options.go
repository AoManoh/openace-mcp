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

// DegradeMode 决定语义路（向量检索）或精排（rerank）失败时的行为。取值
// 由用户经环境变量决定；两种取值下失败都会被看到，没有静默回退到词法
// 结果的形态。
type DegradeMode string

const (
	// DegradeAllow 放行降级结果，结果首行带 [DEGRADED] 横幅说明原因（默认）。
	DegradeAllow DegradeMode = "allow"
	// DegradeDeny 直接报错，不返回结果。
	DegradeDeny DegradeMode = "deny"
)

// 本包读取的环境变量名。
const (
	EnvRetrievalDegrade = "OPENACE_RETRIEVAL_DEGRADE"
	EnvRerankDegrade    = "OPENACE_RERANK_DEGRADE"
	// EnvFreshnessWindow 是查询期的新鲜度窗口：上次成功同步距今不足该
	// 时长的查询跳过内联扫描，直接用现有 revision，索引最多比工作区落后
	// 一个窗口；空或 0 表示每次查询都扫描（默认）。大仓里查询延迟主要花
	// 在内容未变时的全量扫描上，窗口把这部分省掉。后台同步与显式 sync
	// 不受窗口约束。
	EnvFreshnessWindow = "OPENACE_FRESHNESS_WINDOW"
	// EnvQualityStrict 是质量严格档：on 时语义链路的任一缺口（覆盖率不足
	// 100%、查询嵌入失败、配置了 rerank 但未生效、任何降级原因）都报错，
	// 不降级放行；默认 off。要求已配置 embedding provider，否则引擎构造
	// 期报错。
	EnvQualityStrict = "OPENACE_QUALITY_STRICT"
	// EnvQueryBuildWait 是查询等待在建索引的时长上界。超时后构建继续在
	// 后台推进，查询按有无旧 revision 返回：有则按 allow/deny 降级（原因
	// index-building），没有则返回带构建进度与处置提示的错误。未设置时
	// 默认 defaultQueryBuildWait；显式设为 0 表示一直等到构建完成。显式
	// sync 与后台任务不受约束。
	EnvQueryBuildWait = "OPENACE_QUERY_BUILD_WAIT"
	// EnvVectorMemoryBudget 是可寻址向量数据的字节预算，用户自愿开启；空
	// 或 0 表示不限（默认）。默认不限的原因：早先写死的 40 万行上限让
	// 大仓失去语义检索，能力不该默认受限，资源受限的环境自行配置。约束
	// 对象是单个 revision 的向量数据（行数×维度×4 字节）、构建期复用装载
	// 的向量与 journal（已付费但尚未进入 revision 的向量暂存文件）字节数，
	// 不是引擎全局总量。超限行为都可见：查询路降级并记
	// vector-envelope-exceeded，构建路在调用 provider 付费前拦截。预算约束
	// 的是向量字节数而不是进程 RSS：默认 mmap 驻留形态（OPENACE_VECTOR_MMAP）
	// 下向量页是内核可回收的文件后备页，实际常驻通常远低于预算值；
	// OPENACE_VECTOR_MMAP=off 的堆加载形态下同样字节数是不可回收的匿名
	// 内存。1024 维 float32 的每百万行向量数据为 4,096,000,000 字节。
	EnvVectorMemoryBudget = "OPENACE_VECTOR_MEMORY_BUDGET"
)

// defaultQueryBuildWait 是查询有界等待的默认上界。它必须小于最严格的主流
// MCP 客户端的请求超时，而不只是本项目 wrapper 的 110 秒：Cursor 约 60 秒
// 就发送 -32001 取消请求（外部试用工作区实测），预算若为 90 秒，引擎带构建
// 进度与环境变量名的错误会在客户端已经放弃之后才产生，调用方收不到。
// 40 秒给大仓每次请求的新鲜度检查（Windows 上 5 万文件实测约 27 秒）留出
// 余量后，仍先于客户端超时返回。
const defaultQueryBuildWait = 40 * time.Second

// Options 是 local-hybrid 引擎的完整构造配置。零值是纯词法行为：不配置
// embedding 与 rerank，降级模式 allow，每次查询都扫描，一直等到构建完成。
type Options struct {
	Embedding        embedding.Config
	Rerank           rerank.Config
	RetrievalDegrade DegradeMode
	RerankDegrade    DegradeMode
	// LexicalWeights 覆盖词法各子句的权重，nil 表示 lexical.DefaultWeights()。
	// 只有评测程序 openace-bench 做权重扫描时使用，没有对应环境变量；
	// 定值结果冻结进 DefaultWeights，不长期依赖本覆盖。
	LexicalWeights *lexical.Weights
	// FusionParams 覆盖 RRF 融合参数，nil 表示 fusion.DefaultParams()。
	// 同样只有评测程序使用，定值后冻结进 DefaultParams。
	FusionParams *fusion.Params
	// FreshnessWindow 是查询期的新鲜度窗口，0 表示每次查询都内联扫描。
	FreshnessWindow time.Duration
	// QualityStrict 开启质量严格档，要求 Embedding.Enabled，否则 New 报错。
	QualityStrict bool
	// QueryBuildWait 是查询等待在建索引的上界，0 表示一直等到构建完成。
	// OptionsFromEnv 在环境变量未设置时填入 defaultQueryBuildWait。
	QueryBuildWait time.Duration
	// VectorMemoryBudget 是常驻向量的字节预算，0 表示不限（默认）。运维
	// 参数，不参与 Fingerprint：它只决定资源上限，不改变检索语义。
	VectorMemoryBudget int64
	// DisableLexicalFirst 关闭首次构建时先发布只有词法索引的中间 revision
	// 的行为；只供测试与诊断在程序内覆盖，没有环境变量，生产默认 false
	// （即启用中间发布）。
	DisableLexicalFirst bool
	// FragmentGate 开启实验性的碎片过滤（见 fragment_gate.go）；只供测试与
	// 评测程序在程序内覆盖，没有环境变量，默认 false。进入生产默认要等
	// 真实碎片密集语料的验证与用户决定。
	FragmentGate bool
}

// OptionsFromEnv 从环境变量解析引擎配置：embedding 与 rerank provider、两个
// 降级开关、新鲜度窗口、质量严格档、查询等待上界、向量内存预算。任一
// 变量取值非法即返回错误，指出变量名。
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
	// 未设置时用 defaultQueryBuildWait：首次构建期间的查询在客户端请求
	// 超时之前拿到带进度的错误（errQueryBuildWait 路径），而不是一直挂到
	// 传输层超时。显式设为 0 或 0s 表示一直等到构建完成。
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
	vectorBudget, err := reliability.IntEnv(EnvVectorMemoryBudget, 0, 0)
	if err != nil {
		return Options{}, err
	}
	// TemplateVersion 参与 ProfileHash，而 ProfileHash 又进入 Fingerprint：
	// 这里与 New 都注入同一常量 embedTemplateVersion，wrapper 只走本函数
	// 算指纹，daemon 还会走 New，两处必须一致才能复用判定。
	embedCfg.TemplateVersion = embedTemplateVersion
	return Options{
		Embedding:          embedCfg,
		Rerank:             rerankCfg,
		RetrievalDegrade:   retrievalDegrade,
		RerankDegrade:      rerankDegrade,
		FreshnessWindow:    freshness,
		QualityStrict:      strict,
		QueryBuildWait:     buildWait,
		VectorMemoryBudget: int64(vectorBudget),
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

// normalizeDegrade 把除 deny 之外的取值（含零值）归一为默认的 allow。
func normalizeDegrade(mode DegradeMode) DegradeMode {
	if mode == DegradeDeny {
		return DegradeDeny
	}
	return DegradeAllow
}

// Fingerprint 计算引擎配置指纹，wrapper 与 daemon 各自计算后比对，不一致
// 即拒绝复用 daemon，用户改了配置不会静默继续用旧配置的 daemon。指纹
// 覆盖会改变检索语义的项：embedding ProfileHash、rerank 身份、两个降级
// 开关、质量严格档、批量嵌入模式。不含凭据（key 不进任何指纹、状态或
// 日志，换 key 也不该判成不同引擎）与运维参数（批大小、并发、超时、
// 预算，它们不改变检索语义）。Embedding.Enabled=false 时嵌入身份记为 off，
// 其他指纹项相同才会得到相同指纹。OpenAI-compatible 服务允许不带 key，
// 是否启用语义路径由 Enabled 决定。
func (o Options) Fingerprint() string {
	embedComponent := "off"
	if o.Embedding.Enabled {
		embedComponent = o.Embedding.ProfileHash()
	}
	rerankComponent := "off"
	if o.Rerank.Enabled {
		rerankComponent = o.Rerank.Identity()
	}
	// QualityStrict 改变检索语义（缺口报错还是降级放行），与降级开关同类，
	// 必须进指纹：否则一个 strict=on 与一个 strict=off 的 wrapper 会复用
	// 同一个 daemon，实际语义由先启动的那个决定。
	strictComponent := "strict-off"
	if o.QualityStrict {
		strictComponent = "strict-on"
	}
	// 批量嵌入模式是构建行为模式（构建由 daemon 执行），wrapper 与 daemon
	// 必须一致：不进指纹就会按先启动者的模式执行，与降级、严格档开关的
	// 处理不一致。它不改变向量身份（同模型同维度，批接口与实时接口产出
	// 的向量在三个抽样点上的余弦相似度均为 1.000000），所以不进 ProfileHash，索引子树
	// 不变。前缀 "engine-profile-v3" 在指纹组成项变化时升版，旧版 daemon
	// 与新版 wrapper 不会算出相同指纹。
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
