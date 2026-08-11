// Package lexical 封装 Bleve BM25 词法索引：建库、批量写入、查询与
// 句柄生命周期。只使用纯 Go 路径（scorch + 默认 analyzer），不启用
// vector build tag。BM25 参数保持 Bleve 默认，不引入针对单一评测集
// 校准的常数。
package lexical

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
)

// EngineName/EngineVersion 写入 manifest 的 lexical 字段。
const (
	EngineName    = "bleve"
	EngineVersion = "v2.5.7"
)

// writeBatchSize 是单个 Bleve batch 的文档数上限。
const writeBatchSize = 512

// Doc 是进入词法索引的文档；ID 使用 chunk ID。
type Doc struct {
	ID       string
	Path     string
	Symbol   string
	Language string
	Content  string
}

// Hit 是一次查询命中。
type Hit struct {
	ID    string
	Score float64
}

// Weights 是查询期各子句的 boost 权重；<=0 的子句整体省略。
type Weights struct {
	Content     float64
	Path        float64
	Symbol      float64
	SymbolExact float64
	// LatinContent/LatinPath 是 CJK 守卫子句(方案②机制 A)的权重:
	// 查询同时含 ≥2 个 Han 字符与 ≥1 个高区分度 Latin token 时,对
	// latin-only 子串追加 content/path match 子句——CJK 内容仓里包装
	// 措辞的 Han 单字子句会淹没唯一 key/标识符子句(WP-4 实测 nacos
	// zh 包装 R@5 0.800→0.017),追加子句把区分度信号权重拉回。
	// 触发条件外(纯中文/纯英文查询)子句集合与历史逐字节一致。
	LatinContent float64
	LatinPath    float64
	// CompoundKey 是复合 key 查询扩展子句(a' 机制,G-config 诊断
	// 2026-08-04 批准)的权重:查询含 kebab/dot/snake 复合 token
	// (order-by-type / blake3.workspace)时,标准分词把它拆成泛词且
	// by/and 等停用段被删,gold 配置文件被单泛词高频文件淹没。对每个
	// 复合 token 追加 content 字段 AND-match 子句(经索引同款分析器,
	// 停用段两侧一致丢弃),把"同文档共现全部区分段"的信号拉回。
	// 无复合 token 的查询子句集合不变。
	CompoundKey float64
}

// DefaultWeights 返回默认子句权重。Symbol=0.1 是 Stage 5 T10a 定值
// （F1 消解）：profile v3 首次为非 Go 语言填充符号后，symbol 短字段的
// BM25 权重在 NL 查询语料上放大噪声（旧值 1.0 使 CoSQA R@5 0.166→0.092），
// 0.1 让 CoSQA 完全回到无符号基线（0.1660）、django weaksup 保住 +21.8%
// 增益（0.3100→0.3775）、508 条 exact-symbol 探针全权重满分（声明行
// content 命中已足）。SymbolExact=4 对多词查询惰性、对全串精确查询
// 提供 +0.33pp MRR，保留。证据：docs/refactor/2026-07-30-stage5b-tuning.md。
func DefaultWeights() Weights {
	// LatinContent=4/LatinPath=1 是方案② T4 零费用网格的初值(中值),
	// 定值证据落 docs/benchmarks/work/wp4/ 后按证据冻结。
	// CompoundKey=2 是 a' 机制网格定值(work/gconfig/grid-verdict.txt):
	// wd2 在 cargo raw/zh +1.3/+6.0pp、nacos-zh +3.4pp;wd4/6 反伤
	// cargo-raw;触发面同轮收紧为仅 kebab(dot 复合反伤 nacos-raw -5pp)。
	return Weights{Content: 1, Path: 1, Symbol: 0.1, SymbolExact: 4, LatinContent: 4, LatinPath: 1, CompoundKey: 2}
}

// latinGuardTokens 提取查询里的高区分度 Latin token(len≥4,含字母开头,
// 保留内部 . _ -,剔除首尾标点);与 hanRuneCount 一起构成机制 A 触发判定。
func latinGuardTokens(query string) []string {
	var tokens []string
	start := -1
	isLatin := func(r byte) bool {
		return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-'
	}
	flush := func(end int) {
		if start < 0 {
			return
		}
		tok := query[start:end]
		start = -1
		tok = strings.Trim(tok, "._-")
		if len(tok) >= 4 && (tok[0] >= 'A' && tok[0] <= 'Z' || tok[0] >= 'a' && tok[0] <= 'z') {
			tokens = append(tokens, tok)
		}
	}
	for i := 0; i < len(query); i++ {
		if isLatin(query[i]) {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(query))
	return tokens
}

// compoundKeyTokens 提取查询中的复合 key token(a' 机制触发面):
// 仅 kebab 形状(≥2 个以 - 连接的字母数字段,总长 ≥6)。网格证据
// (work/gconfig/grid-verdict.txt):dot 复合(nacos.core.auth 类)的
// AND 扩展会把同时含全部命名空间段的代码/import 文件抬过配置 gold
// (nacos config-raw -5pp),而 kebab 键(order-by-type/--kube-api-*)
// 的段共现天然指向配置位。dot/snake 不触发。上限 3 个防子句爆炸。
func compoundKeyTokens(query string) []string {
	var out []string
	for _, token := range strings.Fields(query) {
		token = strings.Trim(token, "\"'`()[]{}<>,;:!?")
		if len(token) < 6 || len(out) >= 3 {
			continue
		}
		if !strings.Contains(token, "-") {
			continue
		}
		segs := strings.FieldsFunc(token, func(r rune) bool { return r == '-' })
		if len(segs) < 2 {
			continue
		}
		valid, hasAlphaSeg := true, false
		for _, seg := range segs {
			if seg == "" {
				valid = false
				break
			}
			segAlpha := false
			for _, r := range seg {
				if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
					segAlpha = true
				} else if r < '0' || r > '9' {
					valid = false
					break
				}
			}
			if segAlpha {
				hasAlphaSeg = true
			}
		}
		// 触发口径:全部段都必须含字母(blake3 类字母数字混合段算含
		// 字母)——纯数字段的版本号形状(v1.2.3 的 2/3 段)不触发,
		// 防把版本串当配置 key 扩展。
		alphaSegs := 0
		for _, seg := range segs {
			for _, r := range seg {
				if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
					alphaSegs++
					break
				}
			}
		}
		if !valid || !hasAlphaSeg || alphaSegs < len(segs) {
			continue
		}
		out = append(out, token)
	}
	return out
}

// hanRuneCount 统计 Han(中日汉字区)rune 数。
func hanRuneCount(query string) int {
	n := 0
	for _, r := range query {
		if unicode.Is(unicode.Han, r) {
			n++
		}
	}
	return n
}

// blevemapping 构建索引 mapping：
// content/path/symbol 走默认 analyzer（分词检索），
// path_raw/symbol_raw/language 走 keyword analyzer（精确匹配与过滤）。
func blevemapping() mapping.IndexMapping {
	m := bleve.NewIndexMapping()
	doc := bleve.NewDocumentMapping()

	text := bleve.NewTextFieldMapping()
	text.Store = false
	text.IncludeInAll = false

	kw := bleve.NewTextFieldMapping()
	kw.Analyzer = keyword.Name
	kw.Store = false
	kw.IncludeInAll = false

	doc.AddFieldMappingsAt("content", text)
	doc.AddFieldMappingsAt("path", text)
	doc.AddFieldMappingsAt("symbol", text)
	doc.AddFieldMappingsAt("path_raw", kw)
	doc.AddFieldMappingsAt("symbol_raw", kw)
	doc.AddFieldMappingsAt("language", kw)

	m.DefaultMapping = doc
	m.DefaultAnalyzer = "standard"
	return m
}

// indexFields 是写入 Bleve 的文档形状。
type indexFields struct {
	Content   string `json:"content"`
	Path      string `json:"path"`
	Symbol    string `json:"symbol,omitempty"`
	PathRaw   string `json:"path_raw"`
	SymbolRaw string `json:"symbol_raw,omitempty"`
	Language  string `json:"language,omitempty"`
}

// Build 在 dir 新建索引并写入全部文档，完成后关闭句柄。
// 构建属于 staging 阶段：任何错误直接返回，由调用方丢弃 staging。
func Build(ctx context.Context, dir string, docs []Doc) error {
	idx, err := bleve.New(dir, blevemapping())
	if err != nil {
		return fmt.Errorf("创建 lexical 索引: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			idx.Close()
		}
	}()
	batch := idx.NewBatch()
	flush := func() error {
		if batch.Size() == 0 {
			return nil
		}
		if err := idx.Batch(batch); err != nil {
			return err
		}
		batch.Reset()
		return nil
	}
	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return err
		}
		fields := indexFields{
			Content:   doc.Content,
			Path:      doc.Path,
			Symbol:    doc.Symbol,
			PathRaw:   doc.Path,
			SymbolRaw: doc.Symbol,
			Language:  doc.Language,
		}
		if err := batch.Index(doc.ID, fields); err != nil {
			return err
		}
		if batch.Size() >= writeBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	closed = true
	return idx.Close()
}

// Index 是打开的只读词法索引句柄；多 segment 时经 IndexAlias 聚合检索
// （Stage 4 D2）。已知限制：alias 下 BM25 语料统计按各 segment 局部计算，
// 跨段分数存在偏差——候选按 rank 进 RRF 且有 rerank 精排缓冲（暗坑 K38，
// Stage 5 benchmark 量化）。
//
// 并发契约（Stage 2 review S8）：Search 可并发；Close 与 Search 的互斥
// 由上层 revisionHandle 的引用计数保证（refs==0 才关闭），本类型自身
// 不做加锁。禁止绕过句柄直接持有 Index 跨发布使用。
type Index struct {
	idx     bleve.Index
	members []bleve.Index
}

// Open 以只读方式打开已发布 segment 中的索引。
func Open(dir string) (*Index, error) {
	return OpenMulti([]string{dir})
}

// OpenMulti 以只读方式打开一组 segment 索引并聚合为单一检索句柄；
// 任一打开失败即整体失败并回收已打开成员。
func OpenMulti(dirs []string) (*Index, error) {
	if len(dirs) == 0 {
		return nil, errors.New("OpenMulti 需要至少一个索引目录")
	}
	members := make([]bleve.Index, 0, len(dirs))
	for _, dir := range dirs {
		member, err := bleve.OpenUsing(dir, map[string]interface{}{"read_only": true})
		if err != nil {
			for _, opened := range members {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("打开 lexical 索引: %w", err)
		}
		members = append(members, member)
	}
	if len(members) == 1 {
		return &Index{idx: members[0], members: members}, nil
	}
	alias := bleve.NewIndexAlias(members...)
	return &Index{idx: alias, members: members}, nil
}

// ErrClosed 表示句柄已关闭。
var ErrClosed = errors.New("lexical 索引句柄已关闭")

// Search 执行 BM25 检索（默认权重）：查询词命中 content/path/symbol
// 分词字段，并对 symbol_raw 精确匹配加 boost，保证 exact identifier
// 强召回。
func (i *Index) Search(ctx context.Context, queryText string, topK int) ([]Hit, error) {
	return i.SearchWeighted(ctx, queryText, topK, DefaultWeights())
}

// SearchWeighted 按显式子句权重执行 BM25 检索；权重 <=0 的子句省略。
func (i *Index) SearchWeighted(ctx context.Context, queryText string, topK int, w Weights) ([]Hit, error) {
	return i.SearchWeightedPrefix(ctx, queryText, topK, w, "")
}

// SearchWeightedPrefix 同 SearchWeighted,pathPrefix 非空时按 path_raw
// (keyword 字段)把候选域收敛到该子树后再选 topK(P-gray-02 前缀下推:
// 后置过滤在受限深度下会被无关子树淹没为空)。前缀语义与引擎侧一致:
// 等于该路径本身,或以 prefix+"/" 开头。
func (i *Index) SearchWeightedPrefix(ctx context.Context, queryText string, topK int, w Weights, pathPrefix string) ([]Hit, error) {
	if i.idx == nil {
		return nil, ErrClosed
	}
	if topK <= 0 {
		topK = 10
	}
	var clauses []query.Query
	if w.Content > 0 {
		match := bleve.NewMatchQuery(queryText)
		match.SetField("content")
		match.SetBoost(w.Content)
		clauses = append(clauses, match)
	}
	if w.Path > 0 {
		pathMatch := bleve.NewMatchQuery(queryText)
		pathMatch.SetField("path")
		pathMatch.SetBoost(w.Path)
		clauses = append(clauses, pathMatch)
	}
	if w.Symbol > 0 {
		symbolMatch := bleve.NewMatchQuery(queryText)
		symbolMatch.SetField("symbol")
		symbolMatch.SetBoost(w.Symbol)
		clauses = append(clauses, symbolMatch)
	}
	if w.SymbolExact > 0 {
		exact := bleve.NewTermQuery(queryText)
		exact.SetField("symbol_raw")
		exact.SetBoost(w.SymbolExact)
		clauses = append(clauses, exact)
	}
	// CJK 守卫(方案②机制 A):混排查询追加 latin-only 子句,详见 Weights 注释。
	// 触发条件不满足时不追加任何子句——纯中文/纯英文查询行为与历史逐字节一致。
	if (w.LatinContent > 0 || w.LatinPath > 0) && hanRuneCount(queryText) >= 2 {
		if tokens := latinGuardTokens(queryText); len(tokens) > 0 {
			latinOnly := strings.Join(tokens, " ")
			if w.LatinContent > 0 {
				guard := bleve.NewMatchQuery(latinOnly)
				guard.SetField("content")
				guard.SetBoost(w.LatinContent)
				clauses = append(clauses, guard)
			}
			if w.LatinPath > 0 {
				guardPath := bleve.NewMatchQuery(latinOnly)
				guardPath.SetField("path")
				guardPath.SetBoost(w.LatinPath)
				clauses = append(clauses, guardPath)
			}
		}
	}
	// a' 复合 key 查询扩展:AND-match 走索引同款分析器,停用段一致丢弃;
	// 单段剩余时退化为对该区分段的加权 match(仍优于被泛词淹没)。
	if w.CompoundKey > 0 {
		for _, key := range compoundKeyTokens(queryText) {
			expanded := bleve.NewMatchQuery(strings.ReplaceAll(key, "-", " "))
			expanded.SetField("content")
			expanded.SetOperator(query.MatchQueryOperatorAnd)
			expanded.SetBoost(w.CompoundKey)
			clauses = append(clauses, expanded)
		}
	}
	if len(clauses) == 0 {
		return nil, nil
	}

	var q query.Query = bleve.NewDisjunctionQuery(clauses...)
	if pathPrefix != "" {
		exact := bleve.NewTermQuery(pathPrefix)
		exact.SetField("path_raw")
		subtree := bleve.NewPrefixQuery(pathPrefix + "/")
		subtree.SetField("path_raw")
		scope := bleve.NewDisjunctionQuery(exact, subtree)
		q = bleve.NewConjunctionQuery(q, scope)
	}
	request := bleve.NewSearchRequestOptions(q, topK, 0, false)
	result, err := i.idx.SearchInContext(ctx, request)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, Hit{ID: hit.ID, Score: hit.Score})
	}
	return hits, nil
}

// DocCount 返回索引文档数（状态上报用）。
func (i *Index) DocCount() (uint64, error) {
	if i.idx == nil {
		return 0, ErrClosed
	}
	return i.idx.DocCount()
}

// Close 释放句柄（逐成员关闭）；幂等。
func (i *Index) Close() error {
	if i.idx == nil {
		return nil
	}
	var firstErr error
	for _, member := range i.members {
		if err := member.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	i.idx = nil
	i.members = nil
	return firstErr
}
