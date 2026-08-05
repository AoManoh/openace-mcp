package chunk

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 批次 3 六门禁的门 4/门 5 断言(C/C++/C#;门 1/2/3 证据=golden 输出与
// 语言单测,门 6=golden 计时采样,见执行日志 §13)。

// 门 4:病理输入不崩溃、诚实降级(容错语言零符号守卫兜底)。
func TestBatch3MalformedInputs(t *testing.T) {
	profile := DefaultProfile()
	cases := map[string]File{
		"c 纯垃圾":    {RelPath: "a.c", Content: "%%% ??? ((( \x01\x02 not c\n"},
		"c 二进制":    {RelPath: "b.c", Content: string([]byte{0x00, 0xFF, 0xFE, 0x89, 0x50, 0x4E})},
		"cpp 纯垃圾":  {RelPath: "a.cpp", Content: ";;; ]]] }}} ***\n@@@\n"},
		"cs 纯垃圾":   {RelPath: "a.cs", Content: "%%% ??? ;;; garbage\n*** &&&\n"},
		"c 截断到一半":  {RelPath: "t.c", Content: "int f(void) { retu"},
		"cpp 截断模板": {RelPath: "t.cpp", Content: "template <typena"},
	}
	for name, file := range cases {
		chunks, capability := profile.Split(file)
		if capability == CapabilityAST {
			// 容错语义下允许 AST 仅当提取到真符号;垃圾输入不该有。
			t.Fatalf("%s: 垃圾输入不应产出 AST(%d chunks)", name, len(chunks))
		}
	}
	// 深嵌套(C 表达式)有界完成不崩。
	deep := "int x = " + strings.Repeat("(", 3000) + "1" + strings.Repeat(")", 3000) + ";\n"
	if chunks, _ := profile.Split(File{RelPath: "deep.c", Content: deep}); len(chunks) == 0 {
		t.Fatal("深嵌套 C 输入应有产出(AST 或回退)")
	}
}

// 门 5:切分确定性——同输入 3 轮逐字段一致(三语言代表性源)。
func TestBatch3Determinism(t *testing.T) {
	profile := DefaultProfile()
	files := []File{
		{RelPath: "d.c", Content: `#include <stdio.h>

/* 说明 */
typedef struct kv { int k; } kv;

static int get(kv *m, int k) { return m->k + k; }

int put(kv *m, int k) { return k; }
`},
		{RelPath: "d.cpp", Content: `namespace ns {
class Box {
public:
    Box(int v);
    int get() const;
private:
    int v_;
};
Box::Box(int v) : v_(v) {}
int Box::get() const { return v_; }
}
`},
		{RelPath: "d.cs", Content: `namespace Ns;

public class Box
{
    public Box(int v) { V = v; }
    public int V { get; }
    public int Get() => V;
}
`},
	}
	for _, file := range files {
		var first string
		for round := 0; round < 3; round++ {
			chunks, capability := profile.Split(file)
			if capability != CapabilityAST {
				t.Fatalf("%s: 应走 AST,得到 %v", file.RelPath, capability)
			}
			rendered := renderChunks(chunks)
			if round == 0 {
				first = rendered
				continue
			}
			if rendered != first {
				t.Fatalf("%s: 第 %d 轮切分与首轮不一致:\n%s\n---\n%s", file.RelPath, round+1, first, rendered)
			}
		}
	}
}

func renderChunks(chunks []Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&b, "%s|%d-%d|%s|%s|%v|%s\n",
			c.ID, c.StartLine, c.EndLine, c.SymbolHint, c.Language, c.Capability, c.ContentHash)
	}
	return b.String()
}

// 门 5 补充:reflect 级等价(防 renderChunks 字段遗漏)。
func TestBatch3DeterminismDeep(t *testing.T) {
	profile := DefaultProfile()
	file := File{RelPath: "e.c", Content: "int a(void){return 1;}\n\nint b(void){return 2;}\n"}
	first, cap1 := profile.Split(file)
	second, cap2 := profile.Split(file)
	if cap1 != cap2 || !reflect.DeepEqual(first, second) {
		t.Fatalf("两轮 Split 不等价")
	}
}
