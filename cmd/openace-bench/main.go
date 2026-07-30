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
	"github.com/AoManoh/openace-mcp/internal/localengine"
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
	)
	flag.Parse()
	if *workspace == "" || *queries == "" || *qrels == "" || *out == "" {
		return fmt.Errorf("必填参数: -workspace -queries -qrels -out")
	}

	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		return err
	}
	eng, err := localengine.New(opts)
	if err != nil {
		return err
	}
	defer eng.Close(context.Background())

	queryList, err := bench.LoadQueries(*queries)
	if err != nil {
		return err
	}
	qrelSet, err := bench.LoadQrels(*qrels)
	if err != nil {
		return err
	}
	pathToDoc := map[string]string{}
	if *docmap != "" {
		if err := loadDocmap(*docmap, pathToDoc); err != nil {
			return err
		}
	}
	// 只评有 qrels 的查询（协议 §3：缺判定剔除并计数）。
	evaluable := make([]bench.Query, 0, len(queryList))
	for _, query := range queryList {
		if len(qrelSet[query.ID]) > 0 {
			evaluable = append(evaluable, query)
		}
	}
	sort.Slice(evaluable, func(i, j int) bool { return evaluable[i].ID < evaluable[j].ID })
	if *limit > 0 && len(evaluable) > *limit {
		evaluable = evaluable[:*limit]
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	resultsFile, err := os.Create(filepath.Join(*out, "results.jsonl"))
	if err != nil {
		return err
	}
	defer resultsFile.Close()
	writer := bufio.NewWriter(resultsFile)

	manifest := runManifest{
		Label: *label, StartedAt: time.Now().UTC(),
		GitRevision: buildinfo.Current().VCSRevision, Workspace: *workspace,
		QueriesPath: *queries, QrelsPath: *qrels, TopK: *topK,
		EngineFlavor: engineFlavor(opts),
		InputSHA: map[string]string{
			"queries": fileSHA(*queries), "qrels": fileSHA(*qrels),
		},
	}

	ctx := context.Background()
	runResults := bench.Run{}
	ref := engine.WorkspaceRef{DirectoryPath: *workspace}
	// 预热一次 sync（首建），之后每查询的 sync 为 no-op 快路径。
	if _, err := eng.Sync(ctx, engine.SyncRequest{Workspace: ref}); err != nil {
		return fmt.Errorf("workspace 首建: %w", err)
	}
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

func engineFlavor(opts localengine.Options) string {
	if opts.Embedding.Enabled {
		return "hybrid"
	}
	return "lexical-only"
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
