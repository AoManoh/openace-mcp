package chunk

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件回归 2026-08-13 R03b 导入撞出的 P1 缺陷:chunk 身份必须是
// (content, profile) 的纯函数,不得受壁钟/负载影响。旧实现对单文件解析
// 设 2s 壁钟超时(parseTimeoutMicros),真实语料(aspnetcore
// HttpConnectionDispatcherTests.cs,191KB,空载解析 ≈1.9s)贴线:负载抖动
// 即跨扫描在 AST/行窗口两种切分间漂移,内容哈希键漂移导致导入对账
// UnknownKey(R03b 实录 509+59)与单进程内两次扫描不一致。
// 修复=去壁钟超时(SetTimeoutMicros(0)),病理输入由库内确定性节点/内存
// 预算兜底(ParseStrict 对 budget 早停同样报错→确定性回退)。

// synthSlowCSharp 生成 GLR 歧义密集的合法 C#(深嵌套泛型 new+lambda+
// 对象初始化器,模仿肇事文件形态)。实测标度(gotreesitter v0.47.0):
// 200 方法/137KB≈0.7s、400 方法/275KB≈2.8s——400 方法在旧 2s 壁钟下
// 空载即确定性超时,是本回归的红态输入;修复后解析完成,测试耗时有界。
func synthSlowCSharp(methods int) string {
	var b strings.Builder
	b.WriteString("using System;\nusing System.Collections.Generic;\nusing System.Threading.Tasks;\n\nnamespace Probe;\n\npublic class BigFixture\n{\n")
	for i := 0; i < methods; i++ {
		fmt.Fprintf(&b, `    public async Task Case%d()
    {
        var data = new Dictionary<string, List<KeyValuePair<int, Func<Task<List<string>>>>>>
        {
            ["k%d"] = new List<KeyValuePair<int, Func<Task<List<string>>>>>
            {
                new KeyValuePair<int, Func<Task<List<string>>>>(%d, async () => await Task.FromResult(new List<string> { "a", "b" })),
            },
        };
        var query = data["k%d"].ConvertAll(p => new { Key = p.Key, Fn = p.Value });
        foreach (var q in query) { await q.Fn(); }
        Assert(new Wrapper<List<Dictionary<int, string>>>(new List<Dictionary<int, string>> { new Dictionary<int, string> { [%d] = "v" } }) != null);
    }

`, i, i, i, i, i)
	}
	b.WriteString("    private static void Assert(bool ok) { if (!ok) throw new Exception(); }\n    private sealed class Wrapper<T> { public Wrapper(T value) { } }\n}\n")
	return b.String()
}

// TestSplitSlowParseFileIsDeterministicAST 断言慢解析合法源稳定走 AST:
// 旧实现在空载即超时回退(CapabilityFallback)→红;修复后 AST 完成且
// 两次切分身份逐一相等(身份=内容纯函数)。
func TestSplitSlowParseFileIsDeterministicAST(t *testing.T) {
	if testing.Short() {
		t.Skip("慢解析回归(约 3-6s),-short 跳过")
	}
	profile := DefaultProfile()
	file := File{RelPath: "src/Probe/BigFixture.cs", Content: synthSlowCSharp(400)}
	first, capability := profile.Split(file)
	if capability != CapabilityAST {
		t.Fatalf("慢解析合法 C# 必须走 AST(身份不得依赖壁钟),实际 capability=%v chunks=%d", capability, len(first))
	}
	second, capability2 := profile.Split(file)
	if capability2 != CapabilityAST {
		t.Fatalf("第二次切分 capability=%v,身份跨扫描漂移", capability2)
	}
	if len(first) != len(second) {
		t.Fatalf("chunk 数漂移: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("chunk[%d] 身份漂移: %s vs %s", i, first[i].ID, second[i].ID)
		}
	}
}

// TestPathologicalInputStillBounded 断言无壁钟后病理输入仍确定性有界:
// 库内节点/内存预算触发 ParseStrict 早停错误→整文件回退行窗口,两次
// 结果一致(确定性回退,非负载依赖)。输入=超深右嵌套泛型(单表达式
// 指数歧义,远超预算)。
func TestPathologicalInputStillBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("病理输入预算回归,-short 跳过")
	}
	var b strings.Builder
	b.WriteString("public class P { public void M() { var x = ")
	depth := 4000
	for i := 0; i < depth; i++ {
		b.WriteString("new Wrapper<")
	}
	b.WriteString("int")
	for i := 0; i < depth; i++ {
		b.WriteString(">(null)")
		if i < depth-1 {
			b.WriteString(" ?? ")
		}
	}
	b.WriteString("; } }\n")
	profile := DefaultProfile()
	file := File{RelPath: "src/Probe/Pathological.cs", Content: b.String()}
	first, capability := profile.Split(file)
	second, capability2 := profile.Split(file)
	if capability != capability2 || len(first) != len(second) {
		t.Fatalf("病理输入切分不确定: cap %v/%v chunks %d/%d", capability, capability2, len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("病理输入 chunk[%d] 身份漂移", i)
		}
	}
}
