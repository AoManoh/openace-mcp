package chunk

import (
	"strings"
	"testing"
)

// 批次 4 Kotlin 分类器测试(节点形状经 astprobe4 实测:class/data class/
// interface/enum 统一为 class_declaration,object 为 object_declaration;
// name field 全部为空,名字取 type_identifier/simple_identifier 子节点)。

// K1:类型家族符号——class/data/interface/object/enum/typealias 全带符号,
// 顶层函数与扩展函数带符号且 isFunc。
func TestKotlinTypeSymbols(t *testing.T) {
	src := `package com.example.app

import java.util.UUID

const val MAX_RETRIES = 3

class SessionService(private val repo: Repo) {
    fun establish(userId: String): Session = repo.create(userId)
}

data class Session(val id: UUID, val userId: String)

interface Service {
    fun establish(userId: String): Session
}

object Singleton {
    fun ping() = "pong"
}

enum class State { ACTIVE, REVOKED }

typealias Handler = (String) -> Unit

fun topLevelHelper(x: Int): Int = x * 2

fun String.shout(): String = uppercase()
`
	chunks := splitFor(t, cProfile(), "app/Session.kt", src)
	assertAST(t, chunks)
	for _, want := range []string{"SessionService", "Session", "Service", "Singleton", "State", "Handler", "topLevelHelper", "shout"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("缺 %s: %+v", want, symbols(chunks))
		}
	}
}

// K2:KDoc 文档注释附着到声明。
func TestKotlinDocAttachment(t *testing.T) {
	src := `package app

/**
 * 用户会话服务。
 */
class SessionService {
    fun ping() = "pong"
}
`
	chunks := splitFor(t, cProfile(), "app/S.kt", src)
	assertAST(t, chunks)
	svc := chunkBySymbol(chunks, "SessionService")
	if svc == nil {
		t.Fatalf("缺 SessionService: %+v", symbols(chunks))
	}
	if !strings.Contains(svc.Content, "用户会话服务") {
		t.Fatalf("KDoc 应附着: %q", svc.Content)
	}
}

// K3:超预算类拆成员——方法符号 Class.name、companion 与嵌套类型可检索,
// init 块与属性匿名并入。
func TestKotlinOversizedClassMembers(t *testing.T) {
	var b strings.Builder
	b.WriteString("class Handler(private val repo: Repo) {\n")
	b.WriteString("    val cache = mutableMapOf<String, Int>()\n\n")
	b.WriteString("    init { cache.clear() }\n\n")
	for i := 0; i < 30; i++ {
		b.WriteString("    fun handle")
		b.WriteString(string(rune('A' + i%26)))
		b.WriteString(string(rune('0' + i/26)))
		b.WriteString("(a: Int): Int { return a + ")
		b.WriteString(strings.Repeat("9", 80))
		b.WriteString(" }\n")
	}
	b.WriteString("\n    constructor(x: Int) : this(Repo(x))\n")
	b.WriteString("\n    companion object {\n        fun default() = Handler(Repo(0))\n    }\n")
	b.WriteString("\n    class Nested { fun inner() = 1 }\n")
	b.WriteString("}\n")
	chunks := splitFor(t, cProfile(), "app/Handler.kt", b.String())
	assertAST(t, chunks)
	for _, want := range []string{"Handler", "Handler.handleA0", "Handler.constructor", "Handler.Companion", "Handler.Nested"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("缺 %s: %+v", want, symbols(chunks))
		}
	}
}

// K4:enum 带方法——entry 匿名并入,方法带符号。
func TestKotlinEnumWithMembers(t *testing.T) {
	var b strings.Builder
	b.WriteString("enum class State(val code: Int) {\n")
	b.WriteString("    ACTIVE(1), REVOKED(2);\n\n")
	for i := 0; i < 25; i++ {
		b.WriteString("    fun describe")
		b.WriteString(string(rune('A' + i)))
		b.WriteString("(): String { return \"state-\" + ")
		b.WriteString(strings.Repeat("8", 80))
		b.WriteString(" }\n")
	}
	b.WriteString("}\n")
	chunks := splitFor(t, cProfile(), "app/State.kt", b.String())
	assertAST(t, chunks)
	if chunkBySymbol(chunks, "State") == nil || chunkBySymbol(chunks, "State.describeA") == nil {
		t.Fatalf("enum 容器与方法符号缺失: %+v", symbols(chunks))
	}
}

// K5:病理输入诚实回退(容错语义下由零符号守卫兜底:垃圾文件提不出
// 任何符号即回退行窗口)。
func TestKotlinMalformedFallsBack(t *testing.T) {
	for name, src := range map[string]string{
		"纯垃圾":   "%%% ??? ((( not kotlin\n",
		"截断到一半": "class Session(val id: UUI",
	} {
		chunks, capability := cProfile().Split(File{RelPath: "bad.kt", Content: src})
		if capability == CapabilityAST {
			t.Fatalf("%s: 应回退行窗口(%d chunks)", name, len(chunks))
		}
	}
}

// K6:容错语义——grammar 把软关键字 yield 作参数名误报 ERROR(合法
// Kotlin,okio Utf8.kt 实测形态):坏顶层节点匿名兜底(内容保留),
// 干净声明符号照常提取,文件整体仍是 AST。
func TestKotlinToleratesYieldParamMisparse(t *testing.T) {
	src := `package okio

internal inline fun ByteArray.processUtf8CodePoints(
  beginIndex: Int,
  endIndex: Int,
  yield: (Int) -> Unit,
) {
  var index = beginIndex
  while (index < endIndex) { index++ }
}

class CleanService {
    fun ping(): String = "pong"
}

fun cleanHelper(x: Int): Int = x * 2
`
	chunks := splitFor(t, cProfile(), "okio/Utf8.kt", src)
	assertAST(t, chunks)
	for _, want := range []string{"CleanService", "cleanHelper"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("干净声明符号应保留 %s: %+v", want, symbols(chunks))
		}
	}
	// 坏节点内容不得丢失(行覆盖):processUtf8CodePoints 文本仍可检索。
	joined := ""
	for _, c := range chunks {
		joined += c.Content + "\n"
	}
	if !strings.Contains(joined, "processUtf8CodePoints") {
		t.Fatalf("坏节点内容应以匿名 span 保留: %+v", symbols(chunks))
	}
}
