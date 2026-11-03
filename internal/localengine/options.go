package localengine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/reliability"
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// DegradeMode 决定精排（rerank）失败时的行为，取值由用户经
// OPENACE_RERANK_DEGRADE 决定；两种取值下失败都会被看到。语义路（向量检索）
// 没有对应开关：配置了 embedding 后语义路跑不了一律报错，见 policyError。
type DegradeMode string

const (
	// DegradeAllow 放行降级结果，结果首行带 [DEGRADED] 横幅说明原因（默认）。
	DegradeAllow DegradeMode = "allow"
	// DegradeDeny 直接报错，不返回结果。
	DegradeDeny DegradeMode = "deny"
)

// 本包读取的环境变量名。
const (
	// EnvRetrievalDegrade 是已退役的检索降级开关，只用于启动时报错。它的
	// allow 档让查询嵌入失败、revision 没有向量、向量文件不可用时返回纯词法
	// 结果；纯词法的召回只有语义检索的四分之一左右，调用方会把没找到当成
	// 不存在。现在这三种情况在配置了 embedding 时一律报错；部分覆盖与索引
	// 不新鲜照常作答并写明原因，需要它们也报错时用 OPENACE_QUALITY_STRICT=on。
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
	// EnvVectorMemoryBudget 是已移除的向量内存预算变量，只用于启动时报错。
	// 移除原因：预算超出时会关闭整个工作区的语义检索、不再嵌入新内容，
	// 合并与换子树构建也会暂停，检索漏掉的内容会让调用方误以为不存在。
	// 向量数据默认以 mmap 驻留，内存紧张时由内核回收文件页，只会变慢，
	// 结果不变，不需要用降低召回换内存。
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
// embedding 与 rerank，精排降级模式 allow，每次查询都扫描，一直等到构建完成。
type Options struct {
	Embedding     embedding.Config
	Rerank        rerank.Config
	RerankDegrade DegradeMode
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
	// DisableLexicalFirst 关闭首次构建时先发布只有词法索引的中间 revision
	// 的行为；只供测试与诊断在程序内覆盖，没有环境变量，生产默认 false
	// （即启用中间发布）。
	DisableLexicalFirst bool
	// FragmentGate 开启实验性的碎片过滤（见 fragment_gate.go）；只供测试与
	// 评测程序在程序内覆盖，没有环境变量，默认 false。进入生产默认要等
	// 真实碎片密集语料的验证与用户决定。
	FragmentGate bool
}

// OptionsFromEnv 从环境变量解析引擎配置：embedding 与 rerank provider、精排
// 降级开关、新鲜度窗口、质量严格档、查询等待上界。任一变量取值非法即返回
// 错误，指出变量名；设置了已退役的变量同样报错。
func OptionsFromEnv() (Options, error) {
	embedCfg, err := embedding.ConfigFromEnv()
	if err != nil {
		return Options{}, err
	}
	rerankCfg, err := rerank.ConfigFromEnv()
	if err != nil {
		return Options{}, err
	}
	// 任何取值都报错，包括原默认值 allow：allow 原本表示语义路失败时返回
	// 纯词法结果，这一行为已经取消，静默接受会让用户以为它仍然生效。
	if raw := strings.TrimSpace(os.Getenv(EnvRetrievalDegrade)); raw != "" {
		return Options{}, fmt.Errorf("%s is no longer supported (value %q): remove it; when an embedding provider is configured, queries now fail instead of returning lexical-only results whenever the semantic path cannot run (query embedding failed, no vectors yet, vector data unavailable); set %s=on to also reject partial coverage and stale indexes", EnvRetrievalDegrade, raw, EnvQualityStrict)
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
	// 空值与 0 原本就表示不限，与现行为一致，照常放行；其余取值原本会限制
	// 检索，继续运行会让用户以为限制仍然生效，因此在启动时报错。
	if raw := strings.TrimSpace(os.Getenv(EnvVectorMemoryBudget)); raw != "" && raw != "0" {
		return Options{}, fmt.Errorf("%s is no longer supported (value %q): remove it; vectors are memory-mapped and the kernel reclaims their pages under memory pressure, so retrieval stays complete and only gets slower", EnvVectorMemoryBudget, raw)
	}
	// TemplateVersion 参与 ProfileHash，而 ProfileHash 又进入 Fingerprint：
	// 这里与 New 都注入同一常量 embedTemplateVersion，wrapper 只走本函数
	// 算指纹，daemon 还会走 New，两处必须一致才能复用判定。
	embedCfg.TemplateVersion = embedTemplateVersion
	return Options{
		Embedding:       embedCfg,
		Rerank:          rerankCfg,
		RerankDegrade:   rerankDegrade,
		FreshnessWindow: freshness,
		QualityStrict:   strict,
		QueryBuildWait:  buildWait,
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
// 覆盖会改变检索语义的项：词法评分模型、embedding ProfileHash、rerank 身份、
// 精排降级开关、质量严格档、批量嵌入模式。不含凭据（key 不进任何指纹、状态或
// 日志，换 key 也不该判成不同引擎）与运维参数（批大小、并发、超时、
// 预算，它们不改变检索语义）。Embedding.Enabled=false 时嵌入身份记为 off，
// 其他指纹项相同才会得到相同指纹。OpenAI-compatible 服务允许不带 key，
// 是否启用语义路径由 Enabled 决定。
func (o Options) Fingerprint() string {
	// 词法评分模型决定词法路的分数与排序，不由 env 配置，但必须进指纹：
	// 未设置评分模型、实际按 TF-IDF 评分的旧 daemon 与 BM25 的 wrapper 即使
	// env 完全相同也不能互相复用。加入该项时前缀从 v3 升到 v4，纯词法零值
	// 配置的指纹同样随之改变。检索降级开关退役后前缀升到 v5：旧 daemon 在
	// 语义路失败时返回纯词法结果，新 wrapper 不能复用它。前缀在组成项变化
	// 时升版，旧版 daemon 与新版 wrapper 不会算出相同指纹。
	inputs := []string{"engine-profile-v5"}
	for _, part := range o.fingerprintParts() {
		inputs = append(inputs, part.input)
	}
	sum := sha256.Sum256([]byte(strings.Join(inputs, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// fingerprintPart 是引擎配置指纹的一个组成项：input 原样进入指纹哈希；label
// 是写进 daemon 状态与报错的说明，含 input 或能唯一对应 input 的短哈希，
// 两组配置某一项的 label 相同当且仅当该项的 input 相同。
type fingerprintPart struct {
	name  string
	input string
	label string
}

// fingerprintComponentNames 是指纹组成项的名称与顺序；顺序就是哈希输入的顺序，
// 改动会改变全部指纹值。
var fingerprintComponentNames = []string{"lexical", "embedding", "rerank", "rerank_degrade", "quality_strict", "bulk"}

// FingerprintComponentNames 返回指纹组成项的名称，顺序固定，供报错按同一顺序
// 列出不一致的项。
func FingerprintComponentNames() []string {
	return append([]string(nil), fingerprintComponentNames...)
}

// FingerprintComponents 返回指纹各组成项的说明（名称见
// FingerprintComponentNames）。daemon 把它放进状态，wrapper 在指纹不一致时逐项
// 比较，告诉用户具体哪项设置不同；不含 key，URL 只保留协议、主机与路径。
func (o Options) FingerprintComponents() map[string]string {
	components := make(map[string]string, len(fingerprintComponentNames))
	for _, part := range o.fingerprintParts() {
		components[part.name] = part.label
	}
	return components
}

// fingerprintParts 按 fingerprintComponentNames 的顺序给出各组成项。
func (o Options) fingerprintParts() []fingerprintPart {
	lexicalPart := "lexical-" + lexical.ScoringModel
	embed := fingerprintPart{name: "embedding", input: "off", label: "off"}
	if o.Embedding.Enabled {
		// 用只凭 env 可算的配置身份（维度未显式配置时记 auto），不用含探测维度的
		// ProfileHash：wrapper 不做探测，两边才能算出同一指纹。ConfigIdentity 是
		// 哈希，label 另写出 adapter、模型、维度与地址，并带上这个哈希。
		embed.input = o.Embedding.ConfigIdentity()
		dimension := "auto"
		if !o.Embedding.DimensionAuto {
			dimension = fmt.Sprintf("%d", o.Embedding.Dimension)
		}
		embed.label = fmt.Sprintf("%s/%s dim=%s endpoint=%s id=%s",
			o.Embedding.ProviderType, o.Embedding.Model, dimension, displayEndpoint(o.Embedding.BaseURL), embed.input)
	}
	rr := fingerprintPart{name: "rerank", input: "off", label: "off"}
	if o.Rerank.Enabled {
		rr.input = o.Rerank.Identity()
		rr.label = rr.input
		if o.Rerank.Model == "" {
			// 没有模型 id 时 Identity 带原始地址，label 改用去掉用户信息与查询
			// 参数的地址，并附 input 的短哈希保持一一对应。
			sum := sha256.Sum256([]byte(rr.input))
			rr.label = fmt.Sprintf("%s@%s id=%s", o.Rerank.ProviderType, displayEndpoint(o.Rerank.BaseURL), hex.EncodeToString(sum[:])[:8])
		}
	}
	// QualityStrict 改变检索语义（缺口报错还是降级放行），与降级开关同类，
	// 必须进指纹：否则一个 strict=on 与一个 strict=off 的 wrapper 会复用
	// 同一个 daemon，实际语义由先启动的那个决定。
	strict := "strict-off"
	if o.QualityStrict {
		strict = "strict-on"
	}
	// 批量嵌入模式是构建行为模式（构建由 daemon 执行），wrapper 与 daemon
	// 必须一致：不进指纹就会按先启动者的模式执行，与降级、严格档开关的
	// 处理不一致。它不改变向量身份（同模型同维度，批接口与实时接口产出
	// 的向量在三个抽样点上的余弦相似度均为 1.000000），所以不进 ProfileHash，索引子树
	// 不变。
	bulk := "bulk-off"
	if o.Embedding.BatchAPIMode != "" {
		bulk = "bulk-" + o.Embedding.BatchAPIMode
	}
	degrade := string(normalizeDegrade(o.RerankDegrade))
	return []fingerprintPart{
		{name: "lexical", input: lexicalPart, label: lexicalPart},
		embed,
		rr,
		{name: "rerank_degrade", input: degrade, label: degrade},
		{name: "quality_strict", input: strict, label: strict},
		{name: "bulk", input: bulk, label: bulk},
	}
}

// displayEndpoint 返回给人看的服务地址：只保留协议、主机与路径，去掉 URL 里的
// 用户信息、查询参数与片段，这些位置可能带凭据。解析失败时返回 "invalid-url"。
func displayEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}
