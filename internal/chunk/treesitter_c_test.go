package chunk

import (
	"strings"
	"testing"
)

// 批次 3 C 语言分类器测试(失败测试先行;节点形状经 zz_astprobe 实测:
// function_definition 的符号藏在 declarator 链内,typedef 的符号在
// declarator field,struct/enum/union 顶层名在 name field)。

func cProfile() Profile { return DefaultProfile() }

func splitFor(t *testing.T, p Profile, rel string, src string) []Chunk {
	t.Helper()
	chunks, _ := p.Split(File{RelPath: rel, Content: src})
	if len(chunks) == 0 {
		t.Fatalf("应有 chunk 产出")
	}
	return chunks
}

func chunkBySymbol(chunks []Chunk, symbol string) *Chunk {
	for i := range chunks {
		if chunks[i].SymbolHint == symbol {
			return &chunks[i]
		}
	}
	return nil
}

func assertAST(t *testing.T, chunks []Chunk) {
	t.Helper()
	for _, c := range chunks {
		if c.Capability != CapabilityAST {
			t.Fatalf("期望 AST capability,得到 %s(chunk %d-%d)", c.Capability, c.StartLine, c.EndLine)
		}
	}
}

// C1:函数定义符号提取——普通函数、static 指针返回函数(declarator 链
// 穿透 pointer_declarator)、main。
func TestCFunctionSymbols(t *testing.T) {
	src := `#include <stdio.h>

static struct dict *dict_find(struct dict *d, const char *key) {
    return d;
}

int main(int argc, char **argv) {
    return 0;
}
`
	chunks := splitFor(t, cProfile(), "src/dict.c", src)
	assertAST(t, chunks)
	if c := chunkBySymbol(chunks, "dict_find"); c == nil {
		t.Fatalf("缺 dict_find 符号: %+v", symbols(chunks))
	} else if !strings.Contains(c.Content, "dict_find(") {
		t.Fatalf("dict_find chunk 内容不符: %q", c.Content)
	}
	if chunkBySymbol(chunks, "main") == nil {
		t.Fatalf("缺 main 符号: %+v", symbols(chunks))
	}
}

// C2:typedef struct 取 typedef 名;裸 struct/enum/union 取标签名;
// 函数指针 typedef 穿透 parenthesized/pointer declarator。
func TestCTypeSymbols(t *testing.T) {
	src := `typedef struct dictEntry {
    void *key;
    struct dictEntry *next;
} dictEntry;

struct config {
    int port;
};

enum log_level { LOG_DEBUG, LOG_INFO };

union value { int i; float f; };

typedef int (*cmp_fn)(const void *a, const void *b);
`
	chunks := splitFor(t, cProfile(), "src/types.c", src)
	assertAST(t, chunks)
	for _, want := range []string{"dictEntry", "config", "log_level", "value", "cmp_fn"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("缺 %s 符号: %+v", want, symbols(chunks))
		}
	}
}

// C3:注释附着——函数前紧邻注释并入函数 chunk(splitGo 对齐语义)。
func TestCCommentAttachment(t *testing.T) {
	src := `#include <stdio.h>

/* 建立连接。
 * 失败返回 NULL。 */
static int conn_open(int fd) {
    return fd;
}
`
	chunks := splitFor(t, cProfile(), "src/conn.c", src)
	c := chunkBySymbol(chunks, "conn_open")
	if c == nil {
		t.Fatalf("缺 conn_open: %+v", symbols(chunks))
	}
	if !strings.Contains(c.Content, "建立连接") {
		t.Fatalf("紧邻注释应附着到函数 chunk: %q", c.Content)
	}
}

// C4:头文件原型带符号且可与相邻小声明合并(不因 isFunc 阻断,防
// 原型海头文件 chunk 爆炸);带参宏取宏名。
func TestCHeaderPrototypes(t *testing.T) {
	src := `#ifndef DICT_H
#define DICT_H

#define DICT_OK 0
#define DICT_NOTUSED(V) ((void) V)

struct dict *dict_create(void);
int dict_add(struct dict *d, void *key, void *val);

#endif
`
	chunks := splitFor(t, cProfile(), "src/dict.h", src)
	assertAST(t, chunks)
	joined := strings.Join(symbols(chunks), ",")
	if !strings.Contains(joined, "dict_create") && !strings.Contains(joined, "dict_add") &&
		!strings.Contains(joined, "DICT_NOTUSED") {
		t.Fatalf("原型/宏符号应至少保留一个可检索符号(合并后任一即可): %+v", symbols(chunks))
	}
	// 行覆盖完整性:首尾行都在某个 chunk 内。
	last := 0
	for _, c := range chunks {
		if c.EndLine > last {
			last = c.EndLine
		}
	}
	if last < 10 {
		t.Fatalf("尾行未被覆盖: last=%d", last)
	}
}

// C5:语法错误整文件回退行窗口(capability=fallback)。
func TestCSyntaxErrorFallsBack(t *testing.T) {
	src := "int broken(struct {{{\n"
	chunks, _ := cProfile().Split(File{RelPath: "bad.c", Content: src})
	for _, c := range chunks {
		if c.Capability == CapabilityAST {
			t.Fatalf("语法错误应整文件回退: %+v", c)
		}
	}
}

func symbols(chunks []Chunk) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.SymbolHint)
	}
	return out
}
