package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildIgnoreHeavyTree 构造 F6 病理形态:大量目录各带本地 ignore 文件,
// 规则总数上千,文件分布在深处——旧实现里 walk 对每个文件评估
// "至今累积的全部规则"(兄弟子树规则经 base 检查空转但每次都付匹配成本)。
func buildIgnoreHeavyTree(tb testing.TB, dirs, filesPerDir int) string {
	tb.Helper()
	root := tb.TempDir()
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("pkg%03d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			tb.Fatal(err)
		}
		// 每目录 4 条规则(含通配),模拟真实仓库的分散 .gitignore。
		rules := "*.tmp\nbuild-*/\ncache_[0-9]*\n/vendor.local\n"
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(rules), 0o644); err != nil {
			tb.Fatal(err)
		}
		for f := 0; f < filesPerDir; f++ {
			path := filepath.Join(dir, fmt.Sprintf("file_%03d.go", f))
			if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return root
}

// BenchmarkScanIgnoreHeavyTree 是 F6 扫描器债的性能门:300 目录 × 4 规则
// = 1,200 规则(对齐 sealed-run.log 的 1,088 规则 SIGQUIT 证据量级)。
func BenchmarkScanIgnoreHeavyTree(b *testing.B) {
	root := buildIgnoreHeavyTree(b, 300, 12)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		files, err := scan(context.Background(), root)
		if err != nil {
			b.Fatal(err)
		}
		if len(files) != 300*12+300 { // .go 文件 + 各目录 .gitignore 自身
			b.Fatalf("扫描文件数异常: %d", len(files))
		}
	}
}

// TestScanIgnoreHeavyTreeCorrectness:重构前后语义锚——规则只对所在
// 子树生效(pkg000 的 *.tmp 不误伤 pkg299 的同名文件由 base 保证,
// 且被忽略类文件确实被忽略)。
func TestScanIgnoreHeavyTreeCorrectness(t *testing.T) {
	root := buildIgnoreHeavyTree(t, 5, 3)
	// 命中本目录规则的文件应被忽略。
	if err := os.WriteFile(filepath.Join(root, "pkg002", "junk.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 根层无 *.tmp 规则:根下同名文件不受子目录规则影响。
	if err := os.WriteFile(filepath.Join(root, "root.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.RelPath] = true
	}
	if got["pkg002/junk.tmp"] {
		t.Fatal("子目录规则应忽略本子树内 *.tmp")
	}
	if !got["root.tmp"] {
		t.Fatal("子目录规则不得泄漏到根层文件")
	}
	if !got["pkg004/file_000.go"] {
		t.Fatal("正常文件应在扫描集")
	}
}
