package chunk

import (
	"strings"
	"testing"
)

// 批次 3 C++ 分类器测试(节点形状经 zz_astprobe 实测:namespace 有
// name+body(declaration_list);类外定义符号是 function_declarator 内
// qualified_identifier 全文;template_declaration 包装内层声明;
// linkage_specification 的 body 是内层声明)。

// P1:namespace 展开——成员符号可检索,namespace 不作前缀(与 Java
// package 语义对齐);嵌套 namespace 递归展开。
func TestCppNamespaceUnwrap(t *testing.T) {
	src := `namespace redis {
namespace detail {

class ConnPool {
public:
    bool acquire(int timeout_ms);
};

bool helper() { return true; }

}  // namespace detail
}  // namespace redis
`
	chunks := splitFor(t, cProfile(), "src/pool.cpp", src)
	assertAST(t, chunks)
	if chunkBySymbol(chunks, "ConnPool") == nil {
		t.Fatalf("缺 ConnPool: %+v", symbols(chunks))
	}
	if chunkBySymbol(chunks, "helper") == nil {
		t.Fatalf("缺 helper: %+v", symbols(chunks))
	}
}

// P2:类外方法定义/构造/析构——符号取限定名全文(ConnPool::acquire 等)。
func TestCppQualifiedDefinitions(t *testing.T) {
	src := `class ConnPool {
public:
    ConnPool(int cap);
    ~ConnPool();
    bool acquire(int timeout_ms);
private:
    int cap_;
};

ConnPool::ConnPool(int cap) : cap_(cap) {}
ConnPool::~ConnPool() {}
bool ConnPool::acquire(int timeout_ms) { return true; }
`
	chunks := splitFor(t, cProfile(), "src/pool.cpp", src)
	assertAST(t, chunks)
	for _, want := range []string{"ConnPool", "ConnPool::ConnPool", "ConnPool::~ConnPool", "ConnPool::acquire"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("缺 %s: %+v", want, symbols(chunks))
		}
	}
}

// P3:模板包装解包——模板类/模板函数符号取内层名,span 覆盖 template 头。
func TestCppTemplates(t *testing.T) {
	src := `template <typename K, typename V>
struct Cache {
    V lookup(const K& key);
};

template <class T>
void swap_all(T& a, T& b) { }
`
	chunks := splitFor(t, cProfile(), "src/cache.cpp", src)
	assertAST(t, chunks)
	cache := chunkBySymbol(chunks, "Cache")
	if cache == nil {
		t.Fatalf("缺 Cache: %+v", symbols(chunks))
	}
	if !strings.Contains(cache.Content, "template <typename K") {
		t.Fatalf("模板头应覆盖进 span: %q", cache.Content)
	}
	if chunkBySymbol(chunks, "swap_all") == nil {
		t.Fatalf("缺 swap_all: %+v", symbols(chunks))
	}
}

// P4:超预算类拆 header+方法(Class.method 符号,批次 1/2 容器语义)。
func TestCppOversizedClassSplit(t *testing.T) {
	var b strings.Builder
	b.WriteString("class Big {\npublic:\n")
	for i := 0; i < 40; i++ {
		b.WriteString("    // 方法说明,填充预算用的注释行,足够长足够长足够长足够长\n")
		b.WriteString("    int method")
		b.WriteString(string(rune('A' + i%26)))
		b.WriteString(string(rune('0' + i/26)))
		b.WriteString("(int a, int b) { return a + b + ")
		b.WriteString(strings.Repeat("1", 60))
		b.WriteString("; }\n")
	}
	b.WriteString("};\n")
	chunks := splitFor(t, cProfile(), "src/big.cpp", b.String())
	assertAST(t, chunks)
	found := false
	for _, c := range chunks {
		if strings.HasPrefix(c.SymbolHint, "Big.method") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("超预算类应拆出 Big.method* 成员: %+v", symbols(chunks))
	}
}

// P5:operator 重载与 extern "C" 解包。
func TestCppOperatorAndLinkage(t *testing.T) {
	src := `class V { public: int x; };

inline bool operator==(const V& a, const V& b) { return a.x == b.x; }

extern "C" void c_bridge(void) { }
`
	chunks := splitFor(t, cProfile(), "src/ops.cpp", src)
	assertAST(t, chunks)
	joined := strings.Join(symbols(chunks), ",")
	if !strings.Contains(joined, "operator==") {
		t.Fatalf("缺 operator== 符号: %+v", symbols(chunks))
	}
	if !strings.Contains(joined, "c_bridge") {
		t.Fatalf("缺 c_bridge 符号: %+v", symbols(chunks))
	}
}
