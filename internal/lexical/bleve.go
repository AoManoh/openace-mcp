// Package lexical 封装 Bleve BM25 词法索引：建库、批量写入、查询与
// 句柄生命周期。只使用纯 Go 路径（scorch + 默认 analyzer），不启用
// vector build tag。BM25 参数保持 Bleve 默认，不引入针对单一评测集
// 校准的常数。
package lexical

import (
	"context"
	"errors"
	"fmt"

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

// Search 执行 BM25 检索：查询词命中 content/path/symbol 分词字段，
// 并对 symbol_raw 精确匹配加 boost，保证 exact identifier 强召回。
func (i *Index) Search(ctx context.Context, queryText string, topK int) ([]Hit, error) {
	if i.idx == nil {
		return nil, ErrClosed
	}
	if topK <= 0 {
		topK = 10
	}
	match := bleve.NewMatchQuery(queryText)
	match.SetField("content")
	pathMatch := bleve.NewMatchQuery(queryText)
	pathMatch.SetField("path")
	symbolMatch := bleve.NewMatchQuery(queryText)
	symbolMatch.SetField("symbol")
	exact := bleve.NewTermQuery(queryText)
	exact.SetField("symbol_raw")
	exact.SetBoost(4.0)

	disjunction := bleve.NewDisjunctionQuery(match, pathMatch, symbolMatch, exact)
	request := bleve.NewSearchRequestOptions(query.Query(disjunction), topK, 0, false)
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
