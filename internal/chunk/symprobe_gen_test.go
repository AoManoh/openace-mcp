package chunk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestGenerateSymbolProbe 是 T10a 的调优资产生成器（env 门控，CI 跳过）：
// 从 OPENACE_SYMPROBE_DIR 仓库树用当前 profile 切分出符号，取"裸名在
// 全仓唯一声明"的符号做 exact-symbol 探针（query=裸符号名，gold=声明
// 文件），确定性采样并写入 OPENACE_SYMPROBE_OUT/{queries.jsonl,qrels.tsv}。
// 该资产仅用于词法权重定值（调优层），按 D8 与 sealed 集物理隔离——
// 派生方式（chunker 符号枚举）与 sealed G-exact（_mined 池人工链路）不同源。
func TestGenerateSymbolProbe(t *testing.T) {
	root := os.Getenv("OPENACE_SYMPROBE_DIR")
	outDir := os.Getenv("OPENACE_SYMPROBE_OUT")
	if root == "" || outDir == "" {
		t.Skip("OPENACE_SYMPROBE_DIR/OPENACE_SYMPROBE_OUT 未设置，跳过探针生成")
	}
	sampleSize := 150
	profile := DefaultProfile()
	// bare 符号名 → 声明文件集合（多文件声明即含糊，剔除）。
	declFiles := map[string]map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".go",
			".java", ".rs", ".c", ".h", ".cpp", ".cc", ".hpp", ".cs":
		default:
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.Size() > 1<<20 {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil || !utf8.Valid(raw) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		chunks, capability := profile.Split(File{RelPath: rel, Content: string(raw)})
		if capability != CapabilityAST {
			return nil
		}
		for _, c := range chunks {
			if c.SymbolHint == "" {
				continue
			}
			bare := c.SymbolHint
			if idx := strings.LastIndex(bare, "."); idx >= 0 {
				bare = bare[idx+1:]
			}
			if len(bare) < 4 {
				continue
			}
			set := declFiles[bare]
			if set == nil {
				set = map[string]bool{}
				declFiles[bare] = set
			}
			set[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	type probe struct{ symbol, file string }
	var candidates []probe
	for symbol, files := range declFiles {
		if len(files) != 1 {
			continue
		}
		for file := range files {
			candidates = append(candidates, probe{symbol: symbol, file: file})
		}
	}
	if len(candidates) == 0 {
		t.Skipf("%s 无唯一声明符号", root)
	}
	sort.Slice(candidates, func(a, b int) bool { return candidates[a].symbol < candidates[b].symbol })
	step := len(candidates) / sampleSize
	if step < 1 {
		step = 1
	}
	var picked []probe
	for i := 0; i < len(candidates) && len(picked) < sampleSize; i += step {
		picked = append(picked, candidates[i])
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Base(strings.TrimRight(root, "/\\"))
	var queries, qrels strings.Builder
	for i, p := range picked {
		qid := fmt.Sprintf("symprobe-%s-%03d", repo, i)
		line, _ := json.Marshal(map[string]string{"id": qid, "text": p.symbol, "group": "symprobe"})
		queries.Write(line)
		queries.WriteByte('\n')
		fmt.Fprintf(&qrels, "%s\t%s\t1\n", qid, p.file)
	}
	if err := os.WriteFile(filepath.Join(outDir, "queries.jsonl"), []byte(queries.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "qrels.tsv"), []byte(qrels.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("symprobe %s: 唯一符号 %d，采样 %d → %s", repo, len(candidates), len(picked), outDir)
}
