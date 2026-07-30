package chunk

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestGoldenRepoCorpus 是 §9.2 门禁 3/4 的仓库级验证：在
// OPENACE_GOLDEN_DIR 指向的真实仓库树上，对全部批次语言文件执行
// v3 AST 切分与行窗口对照，断言（a）无文件遗漏——凡行窗口有产出的
// 文件 Split 必有产出；（b）AST 产出行区间与源码逐字对应、无重复 ID；
// （c）全程无 panic。同时输出 coherence 统计供选型报告（门禁 5 佐证）。
// 未设置环境变量时跳过（评测资产不入 CI）。
func TestGoldenRepoCorpus(t *testing.T) {
	root := os.Getenv("OPENACE_GOLDEN_DIR")
	if root == "" {
		t.Skip("OPENACE_GOLDEN_DIR 未设置，跳过仓库级 golden 验证")
	}
	profile := DefaultProfile()
	type langStat struct {
		files, astFiles, fallbackFiles int
		emptyFiles                     int
		astChunks, symbolChunks        int
		funcChunks                     int
		totalSpanLines                 int
		astNanos, windowNanos          int64
		fallbackList                   []string
	}
	stats := map[string]*langStat{}
	var walked int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".tox" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		language, batch := map[string]string{
			".py": "python", ".ts": "typescript", ".tsx": "typescript",
			".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
		}[ext], true
		if language == "" {
			batch = false
		}
		if !batch {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.Size() > 1<<20 {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil || !utf8.Valid(raw) || strings.ContainsRune(string(raw), 0) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		file := File{RelPath: rel, Content: string(raw)}
		st := stats[language]
		if st == nil {
			st = &langStat{}
			stats[language] = st
		}
		st.files++
		walked++
		if strings.TrimSpace(file.Content) == "" {
			st.emptyFiles++
			return nil
		}

		beginWindow := time.Now()
		windowChunks := profile.splitLines(file, language)
		st.windowNanos += time.Since(beginWindow).Nanoseconds()

		beginAST := time.Now()
		chunks, capability := profile.Split(file)
		st.astNanos += time.Since(beginAST).Nanoseconds()

		// 门禁 4：无文件遗漏——行窗口有产出而 Split 无产出即为丢文件。
		if len(windowChunks) > 0 && len(chunks) == 0 {
			t.Errorf("文件遗漏: %s（行窗口 %d chunks，Split 0）", rel, len(windowChunks))
			return nil
		}
		if capability != CapabilityAST {
			st.fallbackFiles++
			if len(st.fallbackList) < 40 {
				st.fallbackList = append(st.fallbackList, rel)
			}
			return nil
		}
		st.astFiles++
		lines := strings.Split(normalizeNewlines(file.Content), "\n")
		seen := map[string]bool{}
		for _, c := range chunks {
			st.astChunks++
			st.totalSpanLines += c.EndLine - c.StartLine + 1
			if c.SymbolHint != "" {
				st.symbolChunks++
			}
			if c.Capability != CapabilityAST {
				t.Errorf("%s: AST 文件出现非 AST chunk", rel)
			}
			if seen[c.ID] {
				t.Errorf("%s: 重复 chunk ID %s (%d-%d)", rel, c.ID, c.StartLine, c.EndLine)
			}
			seen[c.ID] = true
			if c.StartLine < 1 || c.EndLine > len(lines) || c.EndLine < c.StartLine {
				t.Errorf("%s: 行区间非法 %d-%d（共 %d 行）", rel, c.StartLine, c.EndLine, len(lines))
				continue
			}
			if want := strings.Join(lines[c.StartLine-1:c.EndLine], "\n"); want != c.Content {
				t.Errorf("%s:%d-%d 内容与源码行不符", rel, c.StartLine, c.EndLine)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if walked == 0 {
		t.Skipf("%s 下无批次语言文件", root)
	}
	languages := make([]string, 0, len(stats))
	for language := range stats {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	var report strings.Builder
	fmt.Fprintf(&report, "golden corpus %s: %d files\n", root, walked)
	for _, language := range languages {
		st := stats[language]
		astRatio := 0.0
		if nonEmpty := st.files - st.emptyFiles; nonEmpty > 0 {
			astRatio = float64(st.astFiles) / float64(nonEmpty) * 100
		}
		symbolRatio, avgSpan := 0.0, 0.0
		if st.astChunks > 0 {
			symbolRatio = float64(st.symbolChunks) / float64(st.astChunks) * 100
			avgSpan = float64(st.totalSpanLines) / float64(st.astChunks)
		}
		fmt.Fprintf(&report, "  %-11s files=%d ast=%d(%.1f%%) fallback=%d empty=%d chunks=%d symbol=%.1f%% avgSpan=%.1f lines ast_ms=%d window_ms=%d\n",
			language, st.files, st.astFiles, astRatio, st.fallbackFiles, st.emptyFiles, st.astChunks, symbolRatio, avgSpan,
			st.astNanos/1e6, st.windowNanos/1e6)
		for _, rel := range st.fallbackList {
			fmt.Fprintf(&report, "    fallback: %s\n", rel)
		}
	}
	t.Log(report.String())
	if out := os.Getenv("OPENACE_GOLDEN_REPORT"); out != "" {
		if writeErr := os.WriteFile(out, []byte(report.String()), 0o644); writeErr != nil {
			t.Errorf("写报告失败: %v", writeErr)
		}
	}
}
