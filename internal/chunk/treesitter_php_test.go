package chunk

// 语言批次 4(PHP)分类器测试(失败测试先行)。节点形状经独立探针对
// gotreesitter v0.47.0 内嵌 php grammar 实测:类型声明(class/interface/
// trait/enum)均带 name field,body 为 declaration_list(enum 为
// enum_declaration_list);method_declaration 的 name field 有效(含
// __construct 与静态方法);语句形 namespace 无 body field,braced 形
// body=compound_statement;HTML 混排是顶层 text/text_interpolation 节点;
// 纯 HTML/纯垃圾输入被解析为覆盖全文件字节的单个 text 节点且
// HasError=false(grammar 对非 PHP 内容永不报错)。

import (
	"fmt"
	"strings"
	"testing"
)

const phpFixture = `<?php

declare(strict_types=1);

namespace App\Billing;

use App\Contracts\Invoiceable;
use App\Support\Money as Cash;

const TAX_RATE = 0.13;

/**
 * Invoice 表示一张发票。
 */
final class Invoice implements Invoiceable
{
    use SoftDeletes;

    public const STATUS_DRAFT = 'draft';

    private array $items = [];

    public function __construct(
        private readonly string $number,
        private Cash $total,
    ) {}

    public function addItem(string $sku, Cash $price): void
    {
        $this->items[] = [$sku, $price];
    }

    public static function fromArray(array $data): self
    {
        return new self($data['number'], new Cash(0));
    }
}

interface Invoiceable
{
    public function addItem(string $sku, Cash $price): void;
}

trait SoftDeletes
{
    protected ?string $deletedAt = null;

    public function softDelete(): void
    {
        $this->deletedAt = date('c');
    }
}

enum InvoiceStatus: string
{
    case Draft = 'draft';
    case Sent = 'sent';

    public function label(): string
    {
        return ucfirst($this->value);
    }
}

function format_money(Cash $amount): string
{
    return (string) $amount;
}

abstract class Report
{
    abstract public function render(): string;
}
`

// P1:代表性源码——php_tag/declare/语句形 namespace/use/const 合并为
// 匿名序言;class(final,promoted constructor)/interface/trait/backed
// enum/顶层函数/abstract class 符号齐备;doc 注释附着;预算内容器保持
// 单 span 不拆成员。
func TestPhpTopLevelSymbols(t *testing.T) {
	chunks := splitFor(t, DefaultProfile(), "src/Invoice.php", phpFixture)
	assertAST(t, chunks)
	for _, c := range chunks {
		if c.Language != "php" {
			t.Fatalf("语言标注错误: %+v", c)
		}
	}
	for _, want := range []string{"Invoice", "Invoiceable", "SoftDeletes", "InvoiceStatus", "format_money", "Report"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("缺 %s 符号: %+v", want, symbols(chunks))
		}
	}
	// 序言(php_tag/declare/namespace 语句形/use/const)合并为匿名 chunk。
	preamble := chunks[0]
	if preamble.SymbolHint != "" {
		t.Fatalf("序言 chunk 不应有符号: %+v", preamble)
	}
	for _, want := range []string{"declare(strict_types=1);", "namespace App\\Billing;", "use App\\Support\\Money as Cash;", "const TAX_RATE"} {
		if !strings.Contains(preamble.Content, want) {
			t.Fatalf("序言未合并 %q: %q", want, preamble.Content)
		}
	}
	// doc 注释附着到类 chunk;classStandalone(isFunc 语义)使相邻类型
	// 声明不互相合并。
	invoice := chunkBySymbol(chunks, "Invoice")
	if !strings.Contains(invoice.Content, "Invoice 表示一张发票") {
		t.Fatalf("doc 注释未附着: %q", invoice.Content)
	}
	if strings.Contains(invoice.Content, "interface Invoiceable") {
		t.Fatalf("类型声明不应与邻居合并: %q", invoice.Content)
	}
	// 顶层函数 isFunc:独立 chunk,不与相邻 abstract class 合并。
	fn := chunkBySymbol(chunks, "format_money")
	if strings.Contains(fn.Content, "abstract class Report") {
		t.Fatalf("函数不应与邻居合并: %q", fn.Content)
	}
	// 预算内容器保持单 span——不产出成员符号。
	for _, member := range []string{"Invoice.addItem", "Invoice.__construct", "InvoiceStatus.label"} {
		if chunkBySymbol(chunks, member) != nil {
			t.Fatalf("预算内容器不应拆成员: %+v", symbols(chunks))
		}
	}
	assertLineFidelity(t, phpFixture, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// buildOversizedPhpClass 生成超预算类:use/const/property 前置成员 +
// promoted constructor + 带 doc 注释与 attribute 的方法 + 静态方法。
func buildOversizedPhpClass() string {
	var b strings.Builder
	b.WriteString("<?php\n\nnamespace App\\Big;\n\n/**\n * Big 服务类。\n */\nclass Big\n{\n")
	b.WriteString("    use SoftDeletes;\n\n")
	b.WriteString("    public const LIMIT = 64;\n\n")
	b.WriteString("    private array $cache = [];\n\n")
	b.WriteString("    public function __construct(private int $seed) {}\n\n")
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "    /** handle_%d 的说明 */\n    #[Route('/h%d')]\n    public function handle_%d(int $value): int\n    {\n        return $value + %d;\n    }\n\n", i, i, i, i)
	}
	b.WriteString("    public static function make(): self\n    {\n        return new self(1);\n    }\n}\n")
	return b.String()
}

// P2:超预算类拆 header + 成员——header 承载类符号并吞并前置匿名成员
// (use/const/property),方法符号 Big.name(含 __construct 与静态方法),
// 成员的 doc 注释与 attribute 并入成员 span。
func TestPhpOversizedClassSplitsMembers(t *testing.T) {
	src := buildOversizedPhpClass()
	if len(src) <= DefaultProfile().MaxChunkBytes {
		t.Fatalf("fixture 未超预算: %d bytes", len(src))
	}
	chunks := splitFor(t, DefaultProfile(), "src/Big.php", src)
	assertAST(t, chunks)
	header := chunkBySymbol(chunks, "Big")
	if header == nil {
		t.Fatalf("缺类 header 符号: %+v", symbols(chunks))
	}
	for _, want := range []string{"Big 服务类", "class Big", "use SoftDeletes;", "LIMIT = 64", "$cache"} {
		if !strings.Contains(header.Content, want) {
			t.Fatalf("header 缺 %q: %q", want, header.Content)
		}
	}
	if chunkBySymbol(chunks, "Big.__construct") == nil {
		t.Fatalf("缺构造器符号: %+v", symbols(chunks))
	}
	if chunkBySymbol(chunks, "Big.make") == nil {
		t.Fatalf("缺静态方法符号: %+v", symbols(chunks))
	}
	methodSymbols := 0
	for _, c := range chunks {
		if strings.HasPrefix(c.SymbolHint, "Big.handle_") {
			methodSymbols++
		}
	}
	if methodSymbols < 10 {
		t.Fatalf("方法级符号过少: %d (%+v)", methodSymbols, symbols(chunks))
	}
	h0 := chunkBySymbol(chunks, "Big.handle_0")
	if h0 == nil || !strings.Contains(h0.Content, "handle_0 的说明") || !strings.Contains(h0.Content, "#[Route('/h0')]") {
		t.Fatalf("成员 doc 注释/attribute 未并入成员 span: %+v", h0)
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// P3:超预算 backed enum 拆成员——enum_case 是匿名前置成员并入 header,
// 方法符号 Op.name。
func TestPhpOversizedEnumSplitsMethods(t *testing.T) {
	var b strings.Builder
	b.WriteString("<?php\nenum Op: string\n{\n    case Add = 'add';\n    case Sub = 'sub';\n\n")
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "    /** apply_%d 说明 */\n    public function apply_%d(int $v): int\n    {\n        return $v + %d;\n    }\n\n", i, i, i)
	}
	b.WriteString("}\n")
	src := b.String()
	chunks := splitFor(t, DefaultProfile(), "src/Op.php", src)
	assertAST(t, chunks)
	header := chunkBySymbol(chunks, "Op")
	if header == nil || !strings.Contains(header.Content, "case Add = 'add';") {
		t.Fatalf("enum header 缺失或未吞并 case 区: %+v", header)
	}
	if chunkBySymbol(chunks, "Op.apply_0") == nil {
		t.Fatalf("缺 enum 方法符号: %+v", symbols(chunks))
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// P4:braced namespace 展开为成员序列(成员可检索、不作符号前缀,C#
// 块式 namespace 同语义);namespace 头行与右大括号并入首尾成员不留
// 覆盖缝隙;doc 注释在 namespace body 内照常附着。
func TestPhpBracedNamespaceExpands(t *testing.T) {
	src := `<?php
namespace App\Models {
    /** User 模型。 */
    class User
    {
        public function name(): string { return 'u'; }
    }

    function helper(): int { return 1; }
}

namespace App\Other {
    class Role {}
}
`
	chunks := splitFor(t, DefaultProfile(), "src/Models.php", src)
	assertAST(t, chunks)
	for _, want := range []string{"User", "helper", "Role"} {
		if chunkBySymbol(chunks, want) == nil {
			t.Fatalf("缺 %s 符号: %+v", want, symbols(chunks))
		}
	}
	user := chunkBySymbol(chunks, "User")
	if !strings.Contains(user.Content, "User 模型") {
		t.Fatalf("namespace body 内 doc 注释未附着: %q", user.Content)
	}
	if !strings.Contains(user.Content, "namespace App\\Models {") {
		t.Fatalf("namespace 头行未并入首成员: %q", user.Content)
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// P5:诚实降级——纯垃圾/无 php_tag 的纯 HTML 解析为覆盖全文件的单个
// text 节点(AST 零结构信息),整文件回退行窗口而非伪 AST;真语法错误
// (树带错)按严格语言语义整文件回退。
func TestPhpNonCodeAndErrorsFallBack(t *testing.T) {
	cases := map[string]string{
		"纯垃圾":       "%%% ??? ;;; not php at all\n*** &&&\n",
		"纯 HTML":    "<html>\n<head><title>Static</title></head>\n<body><p>No PHP here.</p></body>\n</html>\n",
		"截断 class":  "<?php\nclass Broken {\n    public function f(\n",
		"括号错配":      "<?php\nfunction f() { if (true) { }\n",
		"rust 语法误入": "<?php\npub fn broken(x: i32) -> i32 { x }\n",
	}
	for name, src := range cases {
		chunks, capability := DefaultProfile().Split(File{RelPath: "bad.php", Content: src})
		if capability != CapabilityFallback || len(chunks) == 0 {
			t.Fatalf("%s: 应整文件回退行窗口, got cap=%v n=%d", name, capability, len(chunks))
		}
		for _, c := range chunks {
			if c.Capability == CapabilityAST {
				t.Fatalf("%s: 不应产出 AST chunk: %+v", name, c)
			}
		}
	}
}

// P6:纯脚本文件(php_tag + 顶层语句,零声明)——AST 真实成立,按语句
// 边界产出匿名 chunk(capability=ast、零符号),行覆盖完整。
func TestPhpPureScriptStaysAnonymousAST(t *testing.T) {
	src := `<?php
$config = ['debug' => true];
echo "hello";
if ($config['debug']) {
    error_log('on');
}
require __DIR__ . '/vendor/autoload.php';
`
	chunks := splitFor(t, DefaultProfile(), "scripts/bootstrap.php", src)
	assertAST(t, chunks)
	for _, c := range chunks {
		if c.SymbolHint != "" {
			t.Fatalf("纯脚本不应伪造符号: %+v", c)
		}
	}
	assertLineFidelity(t, src, chunks)
}

// P7:HTML 混排——顶层 text/text_interpolation 是匿名 span 保证行覆盖,
// 混排中的 PHP 声明符号照常提取(单行声明与同行 HTML 片段重叠时由
// coalesceOverlaps 归并,符号保留)。
func TestPhpHTMLMixedKeepsSymbols(t *testing.T) {
	src := `<html>
<body>
<?php foreach ($users as $u): ?>
  <li><?= htmlspecialchars($u) ?></li>
<?php endforeach; ?>
<?php function render_footer(): string { return '<footer/>'; } ?>
</body>
</html>
`
	chunks := splitFor(t, DefaultProfile(), "views/list.php", src)
	assertAST(t, chunks)
	if chunkBySymbol(chunks, "render_footer") == nil {
		t.Fatalf("混排中的函数符号丢失: %+v", symbols(chunks))
	}
	assertLineFidelity(t, src, chunks)
	assertNoDuplicateIDs(t, chunks)
}

// P8:heredoc 在方法体内解析无异常,类保持单 span、内容含 heredoc 原文。
func TestPhpHeredocInsideMethod(t *testing.T) {
	src := `<?php
class Mailer
{
    public function template(): string
    {
        $name = 'x';
        return <<<HTML
<div>
  Hello {$name}
</div>
HTML;
    }
}
`
	chunks := splitFor(t, DefaultProfile(), "src/Mailer.php", src)
	assertAST(t, chunks)
	mailer := chunkBySymbol(chunks, "Mailer")
	if mailer == nil {
		t.Fatalf("缺 Mailer 符号: %+v", symbols(chunks))
	}
	if !strings.Contains(mailer.Content, "<<<HTML") || !strings.Contains(mailer.Content, "Hello {$name}") {
		t.Fatalf("heredoc 原文未保留: %q", mailer.Content)
	}
	assertLineFidelity(t, src, chunks)
}
