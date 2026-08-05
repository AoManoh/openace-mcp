package chunk

import (
	"strings"
	"testing"
)

// 批次 3 C# 分类器测试(节点形状经 zz_astprobe 实测:类型声明均带
// name+body(declaration_list),Java 同构;file-scoped namespace 无 body,
// 声明是其兄弟;positional record 可无 body)。

// S1:类型家族符号——class/interface/struct/enum/record 全带符号。
func TestCsharpTypeSymbols(t *testing.T) {
	src := `using System;

namespace Redis.Client;

public class ConnPool : IDisposable
{
    public void Dispose() { }
}

public interface ICache<T>
{
    T Lookup(string key);
}

public struct Point
{
    public int X;
}

public enum LogLevel { Debug, Info }

public record Entry(string Key, long Value);
`
	chunks := splitFor(t, cProfile(), "src/Pool.cs", src)
	assertAST(t, chunks)
	for _, want := range []string{"ConnPool", "ICache", "Point", "LogLevel", "Entry"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("缺 %s: %+v", want, symbols(chunks))
		}
	}
}

// S2:块式 namespace 展开(成员可检索,不作前缀)+ XML 文档注释附着。
func TestCsharpBlockNamespaceAndDocs(t *testing.T) {
	src := `namespace Redis.Client
{
    /// <summary>连接池。</summary>
    public class ConnPool
    {
        public bool Acquire(int timeoutMs) { return true; }
    }
}
`
	chunks := splitFor(t, cProfile(), "src/Pool.cs", src)
	assertAST(t, chunks)
	pool := chunkBySymbol(chunks, "ConnPool")
	if pool == nil {
		t.Fatalf("缺 ConnPool: %+v", symbols(chunks))
	}
	if !strings.Contains(pool.Content, "连接池") {
		t.Fatalf("XML 文档注释应附着: %q", pool.Content)
	}
}

// S3:超预算类拆成员——方法/属性/构造器符号 Class.name;attribute 随
// 声明并入成员 span。
func TestCsharpOversizedClassMembers(t *testing.T) {
	var b strings.Builder
	b.WriteString("public class Handler\n{\n")
	for i := 0; i < 40; i++ {
		b.WriteString("    [Obsolete(\"占位特性,验证 attribute 并入成员\")]\n")
		b.WriteString("    public int Handle")
		b.WriteString(string(rune('A' + i%26)))
		b.WriteString(string(rune('0' + i/26)))
		b.WriteString("(int a) { return a + ")
		b.WriteString(strings.Repeat("9", 80))
		b.WriteString("; }\n")
	}
	b.WriteString("}\n")
	chunks := splitFor(t, cProfile(), "src/Handler.cs", b.String())
	assertAST(t, chunks)
	var member *Chunk
	for i := range chunks {
		if strings.HasPrefix(chunks[i].SymbolHint, "Handler.Handle") {
			member = &chunks[i]
			break
		}
	}
	if member == nil {
		t.Fatalf("超预算类应拆出 Handler.Handle* 成员: %+v", symbols(chunks))
	}
	if !strings.Contains(member.Content, "[Obsolete") {
		t.Fatalf("attribute 应并入成员 span: %q", member.Content)
	}
}

// S4:容错语义双面——零符号垃圾整文件回退;带错但类名完好的文件保留
// AST(毒点隔离,grammar 嵌套泛型误报场景,splitTreeSitter 注)。
func TestCsharpErrorTolerance(t *testing.T) {
	garbage := "%%% ??? ;;; garbage not csharp at all\n*** &&&\n"
	chunks, _ := cProfile().Split(File{RelPath: "Bad.cs", Content: garbage})
	for _, c := range chunks {
		if c.Capability == CapabilityAST {
			t.Fatalf("零符号垃圾应整文件回退: %+v", c)
		}
	}

	// 嵌套泛型 `>>` 是 pinned grammar 的已知误报面:类与干净方法符号
	// 必须保留,毒点被隔离在匿名成员 span 内。
	poisoned := `public class RuleBuilder
{
    public void Poisoned()
    {
        var xs = new List<IRule<T>>();
    }

    public int Clean(int a)
    {
        return a;
    }
}
`
	chunks = splitFor(t, cProfile(), "src/RuleBuilder.cs", poisoned)
	assertAST(t, chunks)
	if chunkBySymbol(chunks, "RuleBuilder") == nil {
		t.Fatalf("坏容器的类名应保留: %+v", symbols(chunks))
	}
	if chunkBySymbol(chunks, "RuleBuilder.Clean") == nil {
		t.Fatalf("干净成员符号应保留: %+v", symbols(chunks))
	}
}
