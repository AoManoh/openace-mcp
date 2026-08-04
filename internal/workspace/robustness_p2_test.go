package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 本文件锁定健壮性诊断 P2 批次 L1-L4 的修复
// (docs/code-review/2026-08-03-robustness-review.md §6)。

// TestBlobNameDomainSeparation(L1):blobName 的摘要输入必须是
// (rel, content) 的单射编码。裸拼接 sha256(rel+content) 下
// ("a","bc") 与 ("ab","c") 输入同一字节串,产生歧义碰撞。
func TestBlobNameDomainSeparation(t *testing.T) {
	if got, want := blobName("a", []byte("bc")), blobName("ab", []byte("c")); got == want {
		t.Fatalf("歧义碰撞: (%q,%q) 与 (%q,%q) 产生同一 blobName %s", "a", "bc", "ab", "c", got)
	}
	// 域移位边界:内容整体挪进路径/路径整体挪进内容。
	if blobName("x", nil) == blobName("", []byte("x")) {
		t.Fatal("歧义碰撞: (\"x\",\"\") 与 (\"\",\"x\") 产生同一 blobName")
	}
	// 同输入确定性(增量 diff 与 statcache 复用的根基)。
	if blobName("f.go", []byte("body")) != blobName("f.go", []byte("body")) {
		t.Fatal("同输入必须产生确定性 blobName")
	}
}

// toctouCtx 在第二次 ctx.Err() 检查点触发一次文件替换,精确模拟
// readIndexableContent 中 os.Stat 之后、读文件之前的 TOCTOU 窗口
// (白盒:依赖该函数"stat 前、读前、读后"各查一次 ctx 的既有结构)。
type toctouCtx struct {
	context.Context
	calls   int
	swapped bool
	swap    func()
}

func (c *toctouCtx) Err() error {
	c.calls++
	if c.calls >= 2 && !c.swapped {
		c.swapped = true
		c.swap()
	}
	return c.Context.Err()
}

// TestReadIndexableContentBoundsPostStatRead(L2):stat 校验 size 之后
// 文件被替换为超限大文件时,读取必须有界(不整读入内存),并按既有
// oversize 跳过口径处理(ok=false, err=nil)。
func TestReadIndexableContentBoundsPostStatRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "grow.txt")
	small := []byte("package small\n")
	if err := os.WriteFile(path, small, 0o600); err != nil {
		t.Fatal(err)
	}
	const maxBytes = int64(1024)

	// 对照组:无替换时小文件正常可读,证明用例不因门禁空转而假绿。
	content, ok, err := readIndexableContent(context.Background(), path, maxBytes)
	if err != nil || !ok || !bytes.Equal(content, small) {
		t.Fatalf("对照组应读到原始内容: content=%q ok=%v err=%v", content, ok, err)
	}

	giant := bytes.Repeat([]byte{'a'}, 512*1024) // 超限 512 倍
	ctx := &toctouCtx{Context: context.Background(), swap: func() {
		if err := os.WriteFile(path, giant, 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	content, ok, err = readIndexableContent(ctx, path, maxBytes)
	if err != nil {
		t.Fatalf("TOCTOU 替换不应报错(按跳过处理): %v", err)
	}
	if !ctx.swapped {
		t.Fatal("测试脚手架失效:文件替换未发生,窗口未被覆盖")
	}
	if got := int64(len(content)); got > maxBytes {
		t.Fatalf("TOCTOU 窗口内发生无界读: 读入 %d 字节 > maxBytes=%d", got, maxBytes)
	}
	if ok {
		t.Fatal("读取期超限文件必须按 oversize 跳过口径处理(ok=false)")
	}
}

// TestIgnoreDirOnlyRuleMatrix(L3):目录规则(尾随 / 的 dirOnly)只匹配
// 目录本身;同名普通文件不受影响(gitignore 语义)。目录被忽略后其
// 内容经包含语义(非最终段命中 / 前缀包含)仍被忽略。
func TestIgnoreDirOnlyRuleMatrix(t *testing.T) {
	rules := parseIgnoreRulesWithBase("build/\n/tools/\nfoo/bar/\n", "", ignoreLayerGit)
	cases := []struct {
		rel   string
		isDir bool
		want  bool
		why   string
	}{
		{"build", false, false, "非锚定目录规则不得误伤同名普通文件"},
		{"build", true, true, "非锚定目录规则匹配目录本身"},
		{"x/build", false, false, "嵌套位置的同名普通文件同样不受目录规则误伤"},
		{"x/build", true, true, "嵌套位置的同名目录被匹配"},
		{"build/gen.js", false, true, "被忽略目录内的文件按包含语义忽略"},
		{"tools", false, false, "锚定目录规则不得误伤同名普通文件"},
		{"tools", true, true, "锚定目录规则匹配目录本身"},
		{"foo/bar", false, false, "含路径目录规则不得误伤同名普通文件"},
		{"foo/bar", true, true, "含路径目录规则匹配目录本身"},
		{"foo/bar/x.txt", false, true, "含路径目录规则的目录内容按前缀包含忽略"},
	}
	for _, tc := range cases {
		if got := rules.Match(tc.rel, tc.isDir); got != tc.want {
			t.Errorf("Match(%q, isDir=%v)=%v, want %v: %s", tc.rel, tc.isDir, got, tc.want, tc.why)
		}
	}
}

// TestScanDirOnlyRuleDoesNotIgnoreSameNamedFile(L3 端到端):扫描层面
// 验证同名普通文件被收录,真目录仍被跳过——同时覆盖内置默认规则层
// (build/、dist/)与 .gitignore 层。
func TestScanDirOnlyRuleDoesNotIgnoreSameNamedFile(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".gitignore":       "/tools/\ncache/\nfoo/bar/\n",
		"build":            "#!/bin/sh\necho build\n", // 内置默认层 build/ 的同名文件
		"tools":            "tools manifest\n",
		"sub/cache":        "cache file\n",
		"foo/bar":          "path rule file\n",
		"dist/lib.js":      "ignored\n", // 内置默认层 dist/ 真目录
		"sub2/cache/x.txt": "ignored\n", // .gitignore cache/ 真目录
		"keep.go":          "package keep\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	got := scannedRelPaths(scanned)
	want := ".gitignore,build,foo/bar,keep.go,sub/cache,tools"
	if got != want {
		t.Fatalf("目录规则误伤同名文件:\n got: %s\nwant: %s", got, want)
	}
}

// TestLooksBinaryScansFullContent(L4):内容已整读进内存,NUL 探测必须
// 全量——NUL 是合法 UTF-8(U+0000),utf8.Valid 拦不住,只查前 8000
// 字节会放伪文本进索引。
func TestLooksBinaryScansFullContent(t *testing.T) {
	late := append(bytes.Repeat([]byte{'a'}, 8192), 0x00)
	if !looksBinary(late) {
		t.Fatal("NUL 位于 8000 字节之后的内容必须判为二进制")
	}
	if looksBinary(bytes.Repeat([]byte{'a'}, 16384)) {
		t.Fatal("无 NUL 的纯文本不得误判为二进制")
	}
	if !looksBinary([]byte{0x00}) {
		t.Fatal("NUL 开头的内容必须判为二进制(既有行为)")
	}
	if looksBinary(nil) {
		t.Fatal("空内容不得判为二进制(既有行为)")
	}
}

// TestScanSkipsLateNulBinaryFile(L4 端到端):NUL 靠后的伪文本文件被
// 扫描跳过,常规文本文件不受影响。
func TestScanSkipsLateNulBinaryFile(t *testing.T) {
	root := t.TempDir()
	fake := append(bytes.Repeat([]byte{'a'}, 8192), 0x00, 'b', 'c')
	if err := os.WriteFile(filepath.Join(root, "fake.txt"), fake, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	scanned, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := scannedRelPaths(scanned); got != "real.txt" {
		t.Fatalf("NUL 靠后的伪文本必须被跳过: got=%s want=real.txt", got)
	}
}
