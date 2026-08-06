package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// D6 扫描器规则回验(差分审计):以真实 git check-ignore 为 oracle,
// 逐文件比对 scan 的取舍与 git 语义的奇偶性。矩阵覆盖:通配/否定/
// 锚定/dirOnly/嵌套 gitignore 覆盖/字符类/?/转义 #/尾随 ** 家族/
// 被排除目录内的 re-include。fixture 刻意避开内置默认忽略清单与
// 敏感文件 denylist 的名字,保证差异只来自 gitignore 语义本身。
// git 不可用时跳过(审计工装;语义钉住由 syncer_test 的纯 Go 用例承担)。
func TestGitignoreDifferentialAudit(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git 不可用,跳过差分审计")
	}
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")

	files := map[string]string{
		".gitignore": "*.log\n" +
			"!important.log\n" +
			"/out/\n" +
			"logs/\n" +
			"docs/**\n" +
			"!docs/keep.md\n" +
			"note?.md\n" +
			"[abc].txt\n" +
			"sub/*.tmpx\n" +
			"\\#tag.txt\n",
		"sub/.gitignore":   "local/\n!override.log\n",
		"app.log":          "x",
		"important.log":    "x",
		"out/x.txt":        "x",
		"outpost/x.txt":    "x",
		"logs/a.txt":       "x",
		"logs/keep.log":    "x",
		"docs/readme.md":   "x",
		"docs/keep.md":     "x",
		"docs/deep/in.md":  "x",
		"note1.md":         "x",
		"noteAB.md":        "x",
		"a.txt":            "x",
		"d.txt":            "x",
		"sub/a.tmpx":       "x",
		"deep/sub/b.tmpx":  "x",
		"sub/local/z.go":   "x",
		"sub/override.log": "x",
		"x/y/logs/f.txt":   "x",
		"#tag.txt":         "x",
		"plain.go":         "x",
	}
	var paths []string
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, rel)
	}
	sort.Strings(paths)

	// oracle:git check-ignore --stdin(退出码 1=全不忽略,非错误)。
	cmd := exec.Command(gitPath, "-C", root, "check-ignore", "--stdin")
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			t.Fatalf("git check-ignore: %v\n%s", err, stderr.String())
		}
	}
	gitIgnored := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			gitIgnored[filepath.ToSlash(line)] = true
		}
	}

	// 我方:scan 实际取舍(未入选=被忽略)。
	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ourKept := map[string]bool{}
	for _, file := range scanned {
		ourKept[file.RelPath] = true
	}

	var mismatches []string
	for _, rel := range paths {
		gitSays := gitIgnored[rel]
		weSay := !ourKept[rel]
		if gitSays != weSay {
			mismatches = append(mismatches, rel+
				" git_ignored="+boolWord(gitSays)+" ours_ignored="+boolWord(weSay))
		}
	}
	if len(mismatches) > 0 {
		t.Fatalf("与 git 语义不一致(%d 处):\n  %s", len(mismatches), strings.Join(mismatches, "\n  "))
	}
}

func boolWord(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// 纯 Go 钉住(D6 修复,不依赖 git 在场):尾随 ** 不匹配目录本身——
// `docs/**` 只排除内容,git 层 `!docs/keep.md` 可 re-include(修复前
// docs 目录被误判忽略触发剪枝,keep.md 永无机会)。
func TestScanTrailingDoubleStarAllowsGitLayerReinclude(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":     "docs/**\n!docs/keep.md\n",
		"docs/keep.md":   "kept\n",
		"docs/other.md":  "ignored\n",
		"docs/sub/in.md": "ignored\n",
		"main.go":        "package main\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, file := range scanned {
		rels = append(rels, file.RelPath)
	}
	got := strings.Join(rels, ",")
	if got != ".gitignore,docs/keep.md,main.go" {
		t.Fatalf("尾随 ** 的 re-include 语义错误: %s", got)
	}
}
