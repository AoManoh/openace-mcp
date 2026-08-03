// openace-bench 是 Stage 5 评测 harness 的执行器：在物化 workspace 上
// 驱动 local-hybrid 检索并按 preregistration 协议产出 run 记录
// （manifest.json / results.jsonl / metrics.json）。
// 工具本身不含任何数据集内容；数据资产按 D5 存放本地私有目录。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/bench"
	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/localengine"
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "openace-bench:", err)
		os.Exit(1)
	}
}

type runManifest struct {
	Label        string            `json:"label"`
	StartedAt    time.Time         `json:"started_at"`
	FinishedAt   time.Time         `json:"finished_at"`
	GitRevision  string            `json:"git_revision"`
	EngineFlavor string            `json:"engine_flavor"`
	Workspace    string            `json:"workspace"`
	QueriesPath  string            `json:"queries_path"`
	QrelsPath    string            `json:"qrels_path"`
	QueryCount   int               `json:"query_count"`
	TopK         int               `json:"top_k"`
	InputSHA     map[string]string `json:"input_sha256"`
	// LexWeights 记录本 run 生效的词法子句权重（run 可复现的 config
	// 指纹组成部分；nil 字段缺省 = 引擎默认）。
	LexWeights map[string]float64 `json:"lex_weights,omitempty"`
	// FusionParams 记录本 run 生效的 RRF 融合参数（同上；nil = 默认）。
	FusionParams map[string]float64 `json:"fusion_params,omitempty"`
}

type resultLine struct {
	QueryID string   `json:"qid"`
	Group   string   `json:"group,omitempty"`
	Docs    []string `json:"docs"`
	HitRank int      `json:"hit_rank"` // 0 = 未命中
	Elapsed int64    `json:"elapsed_ms"`
}

func run() error {
	var (
		workspace = flag.String("workspace", "", "物化语料 workspace 目录")
		queries   = flag.String("queries", "", "queries.jsonl")
		qrels     = flag.String("qrels", "", "qrels.tsv")
		docmap    = flag.String("docmap", "", "docmap.tsv（relpath\\tdocid；空则 docid=relpath，适配 weaksup/sealed 的文件级 qrels）")
		out       = flag.String("out", "", "run 输出目录")
		label     = flag.String("label", "run", "run 标签")
		topK      = flag.Int("topk", 10, "每查询保留候选 doc 数")
		limit     = flag.Int("limit", 0, "查询数上限（0=全部）")
		// 词法子句权重扫描（T10a）：负值 = 引擎默认；权重只影响查询期
		// 计分，索引可跨扫描配置复用。
		symbolWeight = flag.Float64("lex-symbol-weight", -1, "symbol 分词子句权重（<0=默认）")
		exactBoost   = flag.Float64("lex-symbol-exact-boost", -1, "symbol_raw 精确子句 boost（<0=默认）")
		pathWeight   = flag.Float64("lex-path-weight", -1, "path 子句权重（<0=默认）")
		// 双路候选导出（T10b）：dump 融合前 lex/dense 各 route-depth 条
		// 候选到 routes.jsonl，供离线融合参数扫描（零新增嵌入费用）。
		dumpRoutes = flag.Bool("dump-routes", false, "导出融合前双路候选而非评分")
		routeDepth = flag.Int("route-depth", 200, "dump-routes 每路候选深度")
		// rerank head 定值（T10b-4）：融合序头部 pool 送 rerank 打分并
		// 导出原始分，head≤pool 的配置全部离线派生（送审一次付费一次）。
		dumpRerank = flag.Bool("dump-rerank-scores", false, "导出融合头部的 rerank 原始分")
		rerankPool = flag.Int("rerank-pool", 100, "dump-rerank-scores 送审池大小")
		// chunk 全量导出（E3）：存活集逐条记录到 chunks.jsonl，供离线
		// 模板重组与直连 provider 重嵌（不经引擎嵌入路径）。
		dumpChunks = flag.Bool("dump-chunks", false, "导出存活 chunk 全量记录而非评分")
		// 融合参数覆盖（T10b 定值验证）：负值 = 引擎默认（k=60 等权）。
		fusionK      = flag.Int("fusion-k", -1, "RRF k（<0=默认）")
		fusionLexW   = flag.Float64("fusion-lex-weight", -1, "词法路权重（<0=默认）")
		fusionDenseW = flag.Float64("fusion-dense-weight", -1, "dense 路权重（<0=默认）")
	)
	flag.Parse()
	if *dumpChunks {
		if *workspace == "" || *out == "" {
			return fmt.Errorf("-dump-chunks 必填参数: -workspace -out")
		}
	} else if *workspace == "" || *queries == "" || *qrels == "" || *out == "" {
		return fmt.Errorf("必填参数: -workspace -queries -qrels -out")
	}

	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		return err
	}
	var lexWeights map[string]float64
	if *symbolWeight >= 0 || *exactBoost >= 0 || *pathWeight >= 0 {
		weights := lexical.DefaultWeights()
		if *symbolWeight >= 0 {
			weights.Symbol = *symbolWeight
		}
		if *exactBoost >= 0 {
			weights.SymbolExact = *exactBoost
		}
		if *pathWeight >= 0 {
			weights.Path = *pathWeight
		}
		opts.LexicalWeights = &weights
		lexWeights = map[string]float64{
			"content": weights.Content, "path": weights.Path,
			"symbol": weights.Symbol, "symbol_exact": weights.SymbolExact,
		}
	}
	var fusionParams map[string]float64
	if *fusionK >= 0 || *fusionLexW >= 0 || *fusionDenseW >= 0 {
		params := fusion.DefaultParams()
		if *fusionK >= 0 {
			params.K = *fusionK
		}
		if *fusionLexW >= 0 {
			params.LexWeight = *fusionLexW
		}
		if *fusionDenseW >= 0 {
			params.DenseWeight = *fusionDenseW
		}
		opts.FusionParams = &params
		fusionParams = map[string]float64{
			"k": float64(params.K), "lex_weight": params.LexWeight, "dense_weight": params.DenseWeight,
		}
	}
	eng, err := localengine.New(opts)
	if err != nil {
		return err
	}
	defer eng.Close(context.Background())

	var evaluable []bench.Query
	var qrelSet bench.Qrels
	pathToDoc := map[string]string{}
	if !*dumpChunks {
		queryList, err := bench.LoadQueries(*queries)
		if err != nil {
			return err
		}
		qrelSet, err = bench.LoadQrels(*qrels)
		if err != nil {
			return err
		}
		if *docmap != "" {
			if err := loadDocmap(*docmap, pathToDoc); err != nil {
				return err
			}
		}
		// 只评有 qrels 的查询（协议 §3：缺判定剔除并计数）。
		evaluable = make([]bench.Query, 0, len(queryList))
		for _, query := range queryList {
			if len(qrelSet[query.ID]) > 0 {
				evaluable = append(evaluable, query)
			}
		}
		sort.Slice(evaluable, func(i, j int) bool { return evaluable[i].ID < evaluable[j].ID })
		if *limit > 0 && len(evaluable) > *limit {
			evaluable = evaluable[:*limit]
		}
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	manifest := runManifest{
		Label: *label, StartedAt: time.Now().UTC(),
		GitRevision: buildinfo.Current().VCSRevision, Workspace: *workspace,
		QueriesPath: *queries, QrelsPath: *qrels, TopK: *topK,
		EngineFlavor: engineFlavor(opts),
		InputSHA: map[string]string{
			"queries": fileSHA(*queries), "qrels": fileSHA(*qrels),
		},
		LexWeights:   lexWeights,
		FusionParams: fusionParams,
	}

	ctx := context.Background()
	runResults := bench.Run{}
	ref := engine.WorkspaceRef{DirectoryPath: *workspace}
	// 预热一次 sync（首建），之后每查询的 sync 为 no-op 快路径。
	if _, err := eng.Sync(ctx, engine.SyncRequest{Workspace: ref}); err != nil {
		return fmt.Errorf("workspace 首建: %w", err)
	}
	// 语义配置下如实上报构建期覆盖与 provider 健康（T10b 教训：部分
	// 覆盖 + circuit 退避会让整个 run 的语义路静默瘫痪，必须前置可见）。
	if opts.Embedding.Enabled {
		if status, err := eng.WorkspaceStatus(ctx, ref); err == nil && status.Semantic != nil {
			sem := status.Semantic
			fmt.Fprintf(os.Stderr, "workspace: files=%d coverage=%s (%d/%d) rejected=%d journal=%d provider=%s last_error=%q\n",
				status.FileCount, sem.Coverage, sem.CoveredChunks,
				sem.TotalChunks, sem.RejectedChunks, sem.JournalEntries,
				sem.ProviderState, sem.LastError)
		}
	}
	if *dumpChunks {
		count, err := dumpChunkRecords(ctx, eng, ref, *out, manifest)
		if err != nil {
			return err
		}
		fmt.Printf("chunks dumped: %d → %s\n", count, *out)
		return nil
	}
	if *dumpRoutes {
		if err := dumpRouteCandidates(ctx, eng, ref, evaluable, pathToDoc, *out, *routeDepth, manifest); err != nil {
			return err
		}
		fmt.Printf("routes dumped: %d queries depth=%d → %s\n", len(evaluable), *routeDepth, *out)
		return nil
	}
	if *dumpRerank {
		if !opts.Rerank.Enabled {
			return fmt.Errorf("-dump-rerank-scores 需要 rerank provider 配置（当前未启用）")
		}
		rr, err := rerank.NewClient(opts.Rerank)
		if err != nil {
			return err
		}
		params := fusion.DefaultParams()
		if opts.FusionParams != nil {
			params = *opts.FusionParams
		}
		if err := dumpRerankScores(ctx, eng, rr, ref, evaluable, pathToDoc, *out, *rerankPool, *routeDepth, params, manifest); err != nil {
			return err
		}
		fmt.Printf("rerank scores dumped: %d queries pool=%d → %s\n", len(evaluable), *rerankPool, *out)
		return nil
	}
	resultsFile, err := os.Create(filepath.Join(*out, "results.jsonl"))
	if err != nil {
		return err
	}
	defer resultsFile.Close()
	writer := bufio.NewWriter(resultsFile)
	for i, query := range evaluable {
		start := time.Now()
		candidates, err := eng.SearchCandidates(ctx, engine.SearchRequest{Workspace: ref, Query: query.Text})
		if err != nil {
			return fmt.Errorf("query %s: %w", query.ID, err)
		}
		docs := make([]string, 0, *topK)
		seen := map[string]bool{}
		for _, candidate := range candidates {
			docID := pathToDoc[candidate.RelPath]
			if docID == "" {
				// 无 docmap 时 docid=完整相对路径（weaksup/sealed 的
				// qrels 契约）；去扩展名取 basename 会跨目录撞名。
				docID = candidate.RelPath
			}
			if seen[docID] {
				continue
			}
			seen[docID] = true
			docs = append(docs, docID)
			if len(docs) >= *topK {
				break
			}
		}
		runResults[query.ID] = docs
		hitRank := 0
		for rank, docID := range docs {
			if qrelSet[query.ID][docID] > 0 {
				hitRank = rank + 1
				break
			}
		}
		line, _ := json.Marshal(resultLine{
			QueryID: query.ID, Group: query.Group, Docs: docs,
			HitRank: hitRank, Elapsed: time.Since(start).Milliseconds(),
		})
		writer.Write(line)
		writer.WriteByte('\n')
		if (i+1)%100 == 0 {
			fmt.Fprintf(os.Stderr, "progress: %d/%d\n", i+1, len(evaluable))
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	scores := bench.Evaluate(evaluable, qrelSet, runResults)
	metricsJSON, _ := json.MarshalIndent(scores, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "metrics.json"), metricsJSON, 0o644); err != nil {
		return err
	}
	manifest.FinishedAt = time.Now().UTC()
	manifest.QueryCount = len(evaluable)
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), manifestJSON, 0o644); err != nil {
		return err
	}
	for _, score := range scores {
		fmt.Printf("%-10s %-12s mean=%.4f n=%d\n", score.Metric, score.Group, score.Mean, score.Queries)
	}
	return nil
}

// routeLine 是 routes.jsonl 的一行：候选以 [chunkID, docID] 对表示，
// 路内序即召回序（RRF rank 的事实源）。
type routeLine struct {
	QueryID string      `json:"qid"`
	Group   string      `json:"group,omitempty"`
	Lex     [][2]string `json:"lex"`
	Dense   [][2]string `json:"dense,omitempty"`
	Reasons []string    `json:"reasons,omitempty"`
	Elapsed int64       `json:"elapsed_ms"`
}

// dumpRouteCandidates 逐查询导出融合前双路候选（T10b 离线扫描原料）。
func dumpRouteCandidates(ctx context.Context, eng *localengine.Engine, ref engine.WorkspaceRef, evaluable []bench.Query, pathToDoc map[string]string, out string, depth int, manifest runManifest) error {
	routesFile, err := os.Create(filepath.Join(out, "routes.jsonl"))
	if err != nil {
		return err
	}
	defer routesFile.Close()
	writer := bufio.NewWriter(routesFile)
	toDoc := func(relPath string) string {
		if docID := pathToDoc[relPath]; docID != "" {
			return docID
		}
		return relPath
	}
	pairs := func(refs []localengine.CandidateRef) [][2]string {
		out := make([][2]string, 0, len(refs))
		for _, ref := range refs {
			out = append(out, [2]string{ref.ID, toDoc(ref.RelPath)})
		}
		return out
	}
	for i, query := range evaluable {
		start := time.Now()
		routes, err := eng.SearchRoutes(ctx, engine.SearchRequest{Workspace: ref, Query: query.Text}, depth)
		if err != nil {
			return fmt.Errorf("query %s: %w", query.ID, err)
		}
		line, _ := json.Marshal(routeLine{
			QueryID: query.ID, Group: query.Group,
			Lex: pairs(routes.Lex), Dense: pairs(routes.Dense),
			Reasons: routes.Reasons, Elapsed: time.Since(start).Milliseconds(),
		})
		writer.Write(line)
		writer.WriteByte('\n')
		if (i+1)%100 == 0 {
			fmt.Fprintf(os.Stderr, "routes: %d/%d\n", i+1, len(evaluable))
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	manifest.FinishedAt = time.Now().UTC()
	manifest.QueryCount = len(evaluable)
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	return os.WriteFile(filepath.Join(out, "manifest.json"), manifestJSON, 0o644)
}

// dumpChunkRecords 导出存活 chunk 全量记录到 chunks.jsonl（E3 嵌入模板
// A/B）：字段来自引擎 hook，离线侧按模板重组文本后直连 provider 重嵌。
func dumpChunkRecords(ctx context.Context, eng *localengine.Engine, ref engine.WorkspaceRef, out string, manifest runManifest) (int, error) {
	chunksFile, err := os.Create(filepath.Join(out, "chunks.jsonl"))
	if err != nil {
		return 0, err
	}
	defer chunksFile.Close()
	writer := bufio.NewWriter(chunksFile)
	count := 0
	if err := eng.DumpChunkRecords(ctx, ref, func(rec localengine.ChunkDumpRecord) error {
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		writer.Write(line)
		writer.WriteByte('\n')
		count++
		return nil
	}); err != nil {
		return 0, err
	}
	if err := writer.Flush(); err != nil {
		return 0, err
	}
	manifest.FinishedAt = time.Now().UTC()
	manifest.QueryCount = count
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	return count, os.WriteFile(filepath.Join(out, "manifest.json"), manifestJSON, 0o644)
}

// rerankScoreLine 是 -dump-rerank-scores 的单查询记录：pool 为融合序
// 头部候选（chunk,doc 对），Scores 为 rerank 对每个送审 chunk 的原始
// 分——head≤pool 的任意配置可离线派生（送审一次，全部 head 复用）。
type rerankScoreLine struct {
	QueryID string             `json:"qid"`
	Group   string             `json:"group,omitempty"`
	Pool    [][2]string        `json:"pool"`
	Scores  map[string]float64 `json:"scores"`
	Sent    int                `json:"sent"`
	Elapsed int64              `json:"elapsed_ms"`
}

// dumpRerankScores 对每查询取融合序头部 pool 送 rerank 打分并原样导出
// （T10b-4 head 定值）。融合参数沿引擎生效配置（-fusion-* flag 支配）。
func dumpRerankScores(ctx context.Context, eng *localengine.Engine, rr *rerank.Client, ref engine.WorkspaceRef, evaluable []bench.Query, pathToDoc map[string]string, out string, pool int, routeDepth int, params fusion.Params, manifest runManifest) error {
	scoresFile, err := os.Create(filepath.Join(out, "rerank-scores.jsonl"))
	if err != nil {
		return err
	}
	defer scoresFile.Close()
	writer := bufio.NewWriter(scoresFile)
	toDoc := func(relPath string) string {
		if docID := pathToDoc[relPath]; docID != "" {
			return docID
		}
		return relPath
	}
	for i, query := range evaluable {
		start := time.Now()
		routes, err := eng.SearchRoutes(ctx, engine.SearchRequest{Workspace: ref, Query: query.Text}, routeDepth)
		if err != nil {
			return fmt.Errorf("query %s: %w", query.ID, err)
		}
		lexIDs := make([]string, 0, len(routes.Lex))
		relByID := make(map[string]string, len(routes.Lex)+len(routes.Dense))
		for _, ref := range routes.Lex {
			lexIDs = append(lexIDs, ref.ID)
			relByID[ref.ID] = ref.RelPath
		}
		denseIDs := make([]string, 0, len(routes.Dense))
		for _, ref := range routes.Dense {
			denseIDs = append(denseIDs, ref.ID)
			relByID[ref.ID] = ref.RelPath
		}
		fused := fusion.RRFWeighted(lexIDs, denseIDs, params)
		if len(fused) > pool {
			fused = fused[:pool]
		}
		ids := make([]string, 0, len(fused))
		poolPairs := make([][2]string, 0, len(fused))
		for _, f := range fused {
			ids = append(ids, f.ID)
			poolPairs = append(poolPairs, [2]string{f.ID, toDoc(relByID[f.ID])})
		}
		texts, err := eng.ChunkDocTexts(ctx, ref, ids)
		if err != nil {
			return fmt.Errorf("query %s 取文: %w", query.ID, err)
		}
		docs := make([]rerank.Document, 0, len(ids))
		for _, id := range ids {
			if text, ok := texts[id]; ok {
				docs = append(docs, rerank.Document{ID: id, Text: text})
			}
		}
		hits, sent, err := rr.Rerank(ctx, query.Text, docs)
		if err != nil {
			return fmt.Errorf("query %s rerank: %w", query.ID, err)
		}
		scores := make(map[string]float64, len(hits))
		for _, hit := range hits {
			scores[hit.ID] = hit.Score
		}
		line, _ := json.Marshal(rerankScoreLine{
			QueryID: query.ID, Group: query.Group, Pool: poolPairs,
			Scores: scores, Sent: sent, Elapsed: time.Since(start).Milliseconds(),
		})
		writer.Write(line)
		writer.WriteByte('\n')
		if (i+1)%50 == 0 {
			fmt.Fprintf(os.Stderr, "rerank-scores: %d/%d\n", i+1, len(evaluable))
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	manifest.FinishedAt = time.Now().UTC()
	manifest.QueryCount = len(evaluable)
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	return os.WriteFile(filepath.Join(out, "manifest.json"), manifestJSON, 0o644)
}

func engineFlavor(opts localengine.Options) string {
	if !opts.Embedding.Enabled {
		return "lexical-only"
	}
	// rerank 由 key 存在性缺省开启（Stage 3 语义），run 记录必须区分
	// 纯融合与精排后结果（T10b 排障教训：二者指标差一个档位）。
	if opts.Rerank.Enabled {
		return "hybrid+rerank"
	}
	return "hybrid"
}

func loadDocmap(path string, out map[string]string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), "\t", 2)
		if len(fields) == 2 {
			out[fields[0]] = fields[1]
		}
	}
	return scanner.Err()
}

func fileSHA(path string) string {
	sum, err := bench.FileSHA256(path)
	if err != nil {
		return "unavailable"
	}
	return sum
}
