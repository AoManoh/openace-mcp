# openACE MCP

`openACE` 是 **Open Agent Context Engine**——纯 Go、单二进制、无 CGO、无 sidecar 的本地代码检索引擎,经 MCP 工具接入 AI IDE / agent。

- **检索内核自有**:AST 声明级切分(十三语言内嵌 Tree-sitter,纯 Go 运行时)、BM25 词法索引、本地向量索引、加权 RRF 融合、可选精排编排、immutable 增量索引、显式降级。
- **模型自备**:embedding/rerank 模型不自研也不默认推介任何厂商——由你自备模型服务经可替换 provider 接入(OpenAI-compatible 自部署/托管端点,或 voyage/tei 形状端点)。
- **两条硬承诺**:不配置任何模型服务时,**词法检索零凭据完整可用**;任何语义链路故障都**显式标记,绝不静默降级**。

AI agent 复用本机索引与常驻 daemon 检索本地仓库,而不是为每个会话、每个 subagent 反复启动重型进程、重复扫描 workspace。

## 快速开始(推荐:先装二进制,再贴配置)

### 第 1 步:安装

需要 Go `>= 1.23`。

Linux / macOS / WSL:

```bash
go install -tags "grammar_subset,grammar_subset_python,grammar_subset_typescript,grammar_subset_tsx,grammar_subset_javascript,grammar_subset_java,grammar_subset_rust,grammar_subset_c,grammar_subset_cpp,grammar_subset_c_sharp,grammar_subset_kotlin,grammar_subset_ruby,grammar_subset_php" \
  github.com/AoManoh/openace-mcp/cmd/openace-mcp@main
```

Windows PowerShell:

```powershell
go install -tags "grammar_subset,grammar_subset_python,grammar_subset_typescript,grammar_subset_tsx,grammar_subset_javascript,grammar_subset_java,grammar_subset_rust,grammar_subset_c,grammar_subset_cpp,grammar_subset_c_sharp,grammar_subset_kotlin,grammar_subset_ruby,grammar_subset_php" github.com/AoManoh/openace-mcp/cmd/openace-mcp@main
```

网络受限时在命令前加 `GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn`(PowerShell 用 `$env:GOPROXY=...` 形式)。

> `-tags` 选择内嵌的 Tree-sitter 语法子集(当前 AST 支持的十三种语言,二进制约 30MB)。省略 `-tags` 功能完全一致,但内嵌全部 206 种语法(约 49MB)——切分行为不变,只是体积更大。
> **版本优先钉精确 commit**:把 `@main` 换成 `@<commit>`。`@main` 经 Go module proxy(尤其镜像代理)可能解析到缓存的旧提交而非远端最新——装完用 `openace-mcp version` 核对实际构建。升级就一步:重跑安装命令。Unix 上旧 daemon 会被下一个新会话自动接管,开着的 IDE 会话也会在下次调用时自己跟上,不用动任何东西;Windows 需要手动收尾——`pkill -f 'openace-mcp daemon'` 停掉旧 daemon,再重启 MCP 会话。

### 第 2 步:把配置贴进你的 MCP 客户端

二进制默认装到 `$(go env GOPATH)/bin`(通常 `~/go/bin/openace-mcp`,Windows 为 `%USERPROFILE%\go\bin\openace-mcp.exe`)。**客户端找不到命令时,把 `command` 写成上述绝对路径**(IDE 启动子进程通常不经过 shell,`$HOME`/`%USERPROFILE%` 不会展开)。

**Claude Desktop**(`claude_desktop_config.json`,macOS: `~/Library/Application Support/Claude/`,Windows: `%APPDATA%\Claude\`):

```json
{
  "mcpServers": {
    "openace": {
      "command": "openace-mcp",
      "env": {
        "OPENACE_MODE": "auto"
      }
    }
  }
}
```

**Cursor**(项目内 `.cursor/mcp.json` 或全局 `~/.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "openace": {
      "command": "openace-mcp",
      "env": {
        "OPENACE_MODE": "auto"
      }
    }
  }
}
```

**其他支持 MCP stdio 的客户端**(Windsurf/Cline/Codex 等):同上形状——`command` 指向 `openace-mcp`,按需加 `env`。

到这里就可用了:让 AI 调 `codebase_retrieval` 工具、传入你的项目目录,即得纯词法检索(零凭据、零出网)。要开语义混合检索,继续看下一节。

### 免安装变体(客户端经 `go run` 启动)

不想预装时,`command` 换成 `go`:

```json
{
  "mcpServers": {
    "openace": {
      "command": "go",
      "args": ["run", "github.com/AoManoh/openace-mcp/cmd/openace-mcp@main"],
      "env": {
        "GOPROXY": "https://goproxy.cn,direct",
        "GOSUMDB": "sum.golang.google.cn",
        "OPENACE_MODE": "auto"
      }
    }
  }
}
```

首次启动会现场拉取编译(较慢,且为全语法体积);日常使用推荐预装二进制。Windows 下客户端找不到 `go` 时,把 `command` 写成 `go.exe` 绝对路径。

## 开启语义混合检索(模型自备)

在 MCP 配置的 `env` 里加上你的 embedding 服务(OpenAI-compatible 端点,自部署 vLLM/TEI/Infinity/Ollama 或任何兼容托管服务;亦支持 `voyage` 形状端点):

```jsonc
// 片段:并入上文 MCP 配置的 "env" 对象
"env": {
  "OPENACE_MODE": "auto",
  "OPENACE_EMBEDDING_PROVIDER": "openai",
  "OPENACE_EMBEDDING_BASE_URL": "http://127.0.0.1:8080/v1",
  "OPENACE_EMBEDDING_MODEL": "<your-embedding-model>",
  "OPENACE_EMBEDDING_DIMENSION": "1024",
  "OPENACE_EMBEDDING_API_KEY": "<key-if-required>"
}
```

接入后查询自动升级:BM25 与向量双路召回,RRF 融合,头部候选送精排。精排默认启用——在 12 个真实仓、720 条查询的评测里,它把核心召回率比纯融合抬高了 12.8 个百分点,这是"质量至上"这个默认值的底气。rerank 支持 `tei` 与 `voyage` 形状端点。配置了 embedding 而没配 rerank 时,结果会带一条 `rerank-unconfigured` 提示;补上 `OPENACE_RERANK_API_KEY`(voyage 形状可直接复用 `VOYAGE_API_KEY`),或者显式 `OPENACE_RERANK_PROVIDER=off` 确认放弃,提示就消失。召回质量最终取决于你选的模型,挑面向代码检索、口碑可靠的。

### 行为与边界(如实声明)

- **词法路径永远可用**:无 key、断网、模型服务故障时 BM25 检索继续完整工作;未配置模型服务时与纯词法模式完全一致,不出现降级标记。
- **降级完全显式,由你支配**:语义路/精排失败或索引覆盖不完整时,结果首行出现 `[DEGRADED] <原因>; mode=...; semantic_coverage=...` 横幅,结构化字段同步携带 `retrieval_mode`/`degraded_reason`/`semantic_coverage`;`OPENACE_RETRIEVAL_DEGRADE=deny` / `OPENACE_RERANK_DEGRADE=deny` 可改为直接返回可行动错误(默认 `allow` 放行词法结果)。不存在静默降级。
- **成本边界**:embedding/rerank 的调用与计费发生在你自己的模型服务上。索引期按变更内容付费——未变更 chunk 跨 revision 复用向量,不重复付费;查询期每次消耗一次 query embedding(启用 rerank 时另加一次精排调用)。预算护栏建议设在你的服务/账户侧。
- **数据边界**:索引在本机 cache 目录保存被索引文件的明文片段副本(权限仅当前用户);启用 embedding 时 chunk 内容会发送到你配置的模型服务。使用任何**托管**服务前请自行核实其数据保留与训练条款(多数托管服务默认可将数据用于训练,需显式退出);自部署端点无此顾虑。`.openaceignore` 与内置敏感文件 denylist 先于一切生效。
- **向量身份隔离**:模型/维度/端点、内置嵌入模板版本或切分器版本(如新增 AST 语言批次)任一变化,创建平行索引子树并全量重建(一次 corpus 全量嵌入费用,旧子树保留可回退);换 key 不触发重建。
- **增量索引**:首建后编辑只重建变更文件,删除/重命名立即从结果消失;嵌入费用有界于变更量。delta 链自动本地合并(compaction,零模型调用);索引只保留最近两个 revision,内容按需读取不常驻内存。
- **中断不丢付费进度**:每批嵌入成功即写本地 journal;超时/取消/进程被杀后,下次 sync 复用已付费向量只补缺口。进度经 `workspace_status` 实时可见。
- **崩溃与多进程安全**:任意时刻杀进程,重启自动恢复、无重复付费;同一索引子树写路径跨进程互斥,持锁进程崩溃后自动接管;只读检索无锁。索引 immutable、发布原子切换,数据损坏自动回退上一 revision 并自愈。
- **重启不用重新等**:daemon 重启后,第一次查询直接用磁盘上已发布的索引即刻应答,结果标 `index-refreshing`,后台自动对账收敛。三万文件的真实仓实测 1.6 秒拿到结果,而不是白等一轮 43 秒的全量重扫。
- **首次语义 sync 是分钟级操作**(实测 ~2,400 chunks 在托管服务 1–5 分钟),可能超过默认 `OPENACE_TOOL_TIMEOUT=110s`;首建建议临时调大(如 `600s`),或改用 `start_sync_workspace` 异步提交。

## 语言支持

| 切分方式 | 语言 |
|---|---|
| **AST 声明级**(函数/类/方法独立成块并携带符号) | Go(标准库 parser);Python、TypeScript、TSX、JavaScript、Java、Rust、C、C++、C#、Kotlin、Ruby、PHP(内嵌 Tree-sitter,纯 Go 运行时,无 CGO) |
| 确定性行窗口 | 其余全部文本语言 |

单文件解析失败(语法错误、超时、超长单行)自动回退行窗口;`workspace_status` 如实上报每语言 `ast|fallback|mixed`、语义覆盖率与 provider 健康状态。

## MCP 工具

**默认只暴露 `codebase_retrieval` 一个工具**:多工具面会让部分 AI 客户端选错工具,且每个会话为用不到的 schema 白烧 tokens。检索会自动同步 workspace,冷仓首次检索最迟 40 秒返回带构建进度的可行动提示,通常这一个工具就够了。

需要完整工具面(异步任务、状态诊断)时,在 MCP 配置的 `env` 里加:

```json
"OPENACE_MCP_TOOLS": "all"
```

也可以给逗号分隔的自定义清单(如 `"codebase_retrieval,start_sync_workspace,task_status"`);未列出的工具不再被客户端发现,但按名调用仍会被处理。完整清单:

| 工具 | 用途 |
|------|------|
| `codebase_retrieval` | 同步当前 workspace,然后混合检索(BM25 + 可选语义/精排)——**默认唯一暴露** |
| `multi_codebase_retrieval` | 显式传入多个 workspace,分仓检索 |
| `sync_workspace` | 只同步,不检索 |
| `start_codebase_retrieval` / `start_multi_codebase_retrieval` / `start_sync_workspace` | daemon 模式下提交异步任务,适合大仓库 |
| `task_status` / `list_tasks` | 查询异步任务状态/找回最近任务 |
| `workspace_status` | workspace revision、同步阶段、语义覆盖、provider 健康摘要、顶层目录文件计数(排除面可见) |
| `daemon_status` | wrapper 与 daemon 的 build、pid、cache namespace、capability |

小仓库直接 `codebase_retrieval`;大仓库预热或跨仓问题开完整面后用 `start_*` + `task_status`(进度携带速率与 ETA 估算)。

`max_output_length` 默认 20000 字节且**质量优先**:除非明确要节省 token,不要传小值——小预算会截断结果、凭空拉低检索质量;截断时标记会注明"展示了 N/M 个结果块"与恢复方法。

## 运行模式

| 模式 | 适合场景 | 说明 |
|------|----------|------|
| `auto`(默认,推荐) | 日常、大仓库、多 AI 会话 | 自动复用或托管本机 daemon,多个会话共享索引,避免重复扫描与重复嵌入付费 |
| `direct` | 小仓库 smoke test、排障 | 不启动 daemon,每个 MCP 进程自己扫描检索 |
| `manual-daemon` | 高级运维、固定服务 | 你自己管理 daemon 生命周期 |

`auto` 复用 daemon 有两道门:build 一致,provider 配置指纹一致。build 过期的旧 daemon 不挡路——Unix 上新 wrapper 连接时自动接管替换,实测 0.1 秒。配置指纹不一致则明确报错,绝不静默复用。每个 daemon 响应都带 `served_by`,多 IDE/WSL 混用时一眼看出在跟谁说话。

## 索引范围与安全边界

默认尊重 `.gitignore` / `.ignore`,并跳过 `.env*`、credentials、私钥、证书、keystore 等敏感文件(hard denylist,不可绕过)。

项目知识资产在 Git ignore 里、但希望 AI 能检索时,用 `.openaceignore` 显式纳入(gitignore 语法,支持 `!` re-include;只影响 openACE 扫描,不改变 Git 状态):

```gitignore
!AGENTS.md
!docs/
!docs/**/
!docs/**/*.md
```

规则文件只认 `.openaceignore` 这一个名字;安全硬拒绝名单任何规则都覆盖不了。

## 常用环境变量

| 变量 | 说明 |
|------|------|
| `OPENACE_EMBEDDING_PROVIDER` | 语义路端点类型:`openai`(OpenAI-compatible)/ `voyage` / `off`。默认 `voyage` 且未提供 key 时语义路保持关闭、词法照常——即**不配置就是纯词法** |
| `OPENACE_EMBEDDING_BASE_URL` `_API_KEY` `_MODEL` `_DIMENSION` | 模型服务身份四项(`openai` 类型必填 base_url 与 model);`voyage` 类型 key 为空时回退读 `VOYAGE_API_KEY`;任一身份变化触发平行索引全量重建 |
| `OPENACE_EMBEDDING_BATCH_SIZE` `_MAX_CONCURRENCY` `_RPM_BUDGET` `_TPM_BUDGET` | 索引期调用参数(默认 128 / 16 / 不限 / 不限)。索引吞吐通常由 provider 限速决定:免费档 RPM 低时提并发只会触发 429 退避(无害);付费档与自部署高吞吐模型吞吐随并发近线性——自部署资源充足时把 `_MAX_CONCURRENCY` 调到 32-64 可数倍缩短索引时间,`_RPM_BUDGET`/`_TPM_BUDGET` 可按服务额度设硬顶 |
| `OPENACE_RERANK_PROVIDER` | 精排(质量至上默认档):`tei` / `voyage` / `off`;默认 `voyage`,key 缺省回退 `VOYAGE_API_KEY`。配置即启用;语义已配而精排缺配置时结果携带 `rerank-unconfigured` 提示(`OPENACE_QUALITY_STRICT=on` 下升级为报错),显式 `off` 视为确认放弃。`_BASE_URL`/`_API_KEY`/`_MODEL`/`_MAX_TOKENS` 语义同上 |
| `OPENACE_RETRIEVAL_DEGRADE` / `OPENACE_RERANK_DEGRADE` | 语义路/精排失败策略:`allow`(默认,放行并标 `[DEGRADED]`)/ `deny`(返回可行动错误) |
| `OPENACE_QUALITY_STRICT` | `on` = 质量严格档:语义链路任一缺口(覆盖 <100%、查询嵌入失败、已配置的 rerank 未生效等)直接报错;要求已配置 embedding。默认 `off`。结构化结果携带 `rerank_sent`/`query_embed_failed`/`embedding_profile` |
| `OPENACE_QUERY_BUILD_WAIT` | 查询等待在建索引的上界,**默认 `40s`**(先于主流 MCP 客户端的请求超时,冷仓首建期间的同步检索返回带构建进度的可行动错误,而非裸超时):超时后有旧索引按 allow/deny 降级,无旧索引返回带进度的错误;显式 `0` = 等到构建完成 |
| `OPENACE_MCP_TOOLS` | MCP 工具面:未设 = 只暴露 `codebase_retrieval`;`all` = 完整能力面;或逗号清单指定 |
| `OPENACE_RENDER_LINE_NUMBERS` | `1` = 检索结果围栏内逐行携带真实文件行号(`cat -n` 形状,Read-parity 试验面);默认关闭 |
| `OPENACE_GRAY_FEEDBACK` | `1` = instructions 追加灰度反馈协议:调用 AI 每轮工具调用后输出多维诊断报告(事实/效果/体验/耗时/bug 复现),供测试者汇总回传。默认关闭 |
| `OPENACE_PROVIDER_TIMEOUT` / `OPENACE_PROVIDER_MAX_RETRIES` | provider HTTP 超时(默认 `60s`)与单批重试上限(默认 `5`) |
| `OPENACE_MODE` | `auto` / `direct` / `manual-daemon`(默认 `auto`) |
| `OPENACE_CACHE_NAMESPACE` | cache 命名空间,隔离账号/tenant/测试批次 |
| `OPENACE_DAEMON_ADDR` / `OPENACE_DAEMON_LISTEN_ADDR` | shim 连接地址 / daemon 监听地址(默认 `127.0.0.1:8765`) |
| `OPENACE_DAEMON_TOKEN` | daemon HTTP 凭据。**默认自动生成随机 token**(0600 文件,wrapper 自动读取)——零配置即防多用户机上其他本地用户经回环端口读取你的索引;`off` 显式关闭(自担风险) |
| `OPENACE_RECONCILE_CONCURRENCY` | daemon 后台 workspace 监测并发度(默认 `2`) |
| `OPENACE_TASK_WORKERS` | daemon 异步任务 worker 数(默认 `4`) |
| `OPENACE_TOOL_TIMEOUT` | 同步 MCP 工具超时(默认 `110s`) |

daemon 只监听 loopback,不要直接暴露公网。引擎固定为 local-hybrid,历史 `OPENACE_ENGINE=ace` 已退役,设置会得到明确报错。

wrapper 与 daemon 的一致性分两层,行为刻意不同:

- **build 过期**:升级后的 wrapper 在连接时自动接管旧 daemon——SIGTERM 请求优雅停机,等它退出,再拉起新的,全程实测 0.1 秒;嵌入进度有断点日志,付过费的向量一条不丢。Windows 没有对应的信号语义,保持显式报错,错误文本里带着旧 daemon 的 pid 和一条可复制的修复命令。
- **provider/降级 env 变了**:这是你改了配置意图,不是版本过期,wrapper 不会替你猜。它按配置指纹拒绝复用并明确报错,按提示重启 daemon 即生效。

**升级不打断 IDE 会话**(Unix)。升级后,开着的 MCP 会话在下一次调用时发现 daemon 已换代,wrapper 就原地 exec 磁盘上的新版自身:进程号不变,标准流不断,触发的那条请求被保存下来由新进程重放,管线里排队的请求也一并带过去。你看到的只是一次正常应答。自愈失败时——比如磁盘二进制反而旧于 daemon——按原样返回可行动硬错,30 秒冷却防止 exec 打转。Windows 无 exec 语义,保持"重启 MCP 会话"提示。

## 按场景选配置

环境变量的完整语义都在上表,这里只给三组常用组合。

- **结果完整性优先**——审计、事实核查这类"宁可报错,不要部分结果"的任务。设 `OPENACE_RETRIEVAL_DEGRADE=deny`,任何降级直接变报错;要求更严就 `OPENACE_QUALITY_STRICT=on`,语义链路差一点都不放行。为什么要设:默认档是降级放行,key 失效那天词法结果照样返回,顶部一行 `[DEGRADED]` 横幅是唯一警示——只看文件列表、不看横幅的调用方,会把词法结果当成完整语义检索用。
- **大仓高频查询**——每次查询前有一轮内联重扫,成本随文件数线性涨,接近十万文件的仓库实测 1.5 秒起步。设 `OPENACE_FRESHNESS_WINDOW=30s`,窗口内的查询跳过重扫,同档实测 p50 降到 0.4 秒。代价明码标价:窗口内的磁盘改动最多延迟 30 秒可见,自己权衡。
- **候选被噪声淹没**——运行日志、构建产物、实验残留这类目录一多,宽泛查询的候选位就被它们挤掉。用 `.openaceignore` 逐目录排掉,偶尔要查再用 `!pattern` 精确放行。这些规则决定索引面,比事后在查询里过滤省得多。

## 排障提示

- **某个目录整体检索不到**:文件选择遵循逐目录的 `.gitignore` / `.ignore` / `.openaceignore`,内置敏感文件 denylist 先于一切。最常见的一种:根 `.gitignore` 忽略了 `docs/`,git 惯例把私有或生成内容排除在版本库外,索引跟着跳过了。解法一行:在 `.openaceignore` 里加 `!docs/`。不确定哪个目录被排除?看 `workspace_status` 的 `top_level_file_counts`,预期目录缺失或计数为 0 就是被排除了,不用做对照实验。
- **索引速度慢**:嵌入吞吐通常由 provider 限速决定(免费档 RPM 很低)。`workspace_status`/`task_status` 进度带 `rate/eta`;付费档/自部署高吞吐模型可调大 `OPENACE_EMBEDDING_MAX_CONCURRENCY`(默认 16,自部署可到 64)。
- **客户端找不到命令**:`command` 写绝对路径(`~/go/bin/openace-mcp` 等);IDE 启动子进程不经过 shell,环境变量占位符不展开。
- **升级不生效**:Unix 上重跑 `go install` 就完事,旧 daemon 会被自动接管,开着的会话下次调用自动跟上;`daemon_status` 能核对两边的 build。Windows 仍需手动:停旧 daemon,重启 MCP 会话。
- **改了 provider env 没反应**:确认重启了 MCP 会话;`daemon_status` 可查当前 daemon 的 build 与配置指纹。
- **WSL/Windows 混用**:WSL 里复用 Windows daemon 时传 `D:\project` 或 `/mnt/d/project` 均可(自动规范化);非 WSL 的 POSIX 路径会被拒绝,避免产生无效 workspace 身份。
- **`provider_profile_id` 报错**:该参数属已退役 legacy 引擎,删除即可。

## 本地开发

```bash
go test ./...
go vet ./...
go test -race ./internal/daemon ./internal/mcp ./internal/workspace
# 发布形态构建(语法子集,~30MB;省略 -tags 为全语法 ~49MB)
go build -tags "grammar_subset,grammar_subset_python,grammar_subset_typescript,grammar_subset_tsx,grammar_subset_javascript,grammar_subset_java,grammar_subset_rust,grammar_subset_c,grammar_subset_cpp,grammar_subset_c_sharp,grammar_subset_kotlin,grammar_subset_ruby,grammar_subset_php" ./cmd/openace-mcp ./cmd/openace-daemon
```

## License

MIT License. Copyright (c) 2026 aomanoh.
