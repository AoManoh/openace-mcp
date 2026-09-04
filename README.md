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
  github.com/AoManoh/openace-mcp/cmd/openace-mcp@latest
```

Windows PowerShell:

```powershell
go install -tags "grammar_subset,grammar_subset_python,grammar_subset_typescript,grammar_subset_tsx,grammar_subset_javascript,grammar_subset_java,grammar_subset_rust,grammar_subset_c,grammar_subset_cpp,grammar_subset_c_sharp,grammar_subset_kotlin,grammar_subset_ruby,grammar_subset_php" github.com/AoManoh/openace-mcp/cmd/openace-mcp@latest
```

网络受限时在命令前加 `GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn`(PowerShell 用 `$env:GOPROXY=...` 形式)。

> `-tags` 选择内嵌的 Tree-sitter 语法子集(当前 AST 支持的十三种语言,二进制约 30MB)。省略 `-tags` 功能完全一致,但内嵌全部 206 种语法(约 49MB)——切分行为不变,只是体积更大。

#### 选版本:`@latest`、`@main` 还是 `@<commit>`

三种写法装出来的二进制用法完全相同,区别只在"拿到哪一份代码"和"什么时候会变":

| 写法 | 解析到 | 版本号形态(`openace-mcp version` / `daemon_status`) | 什么时候变化 | 适合 |
|---|---|---|---|---|
| `@latest` | 最新正式发布 tag(GitHub Releases 有对应发布说明) | `v0.2.0` | 只在打出新 tag 时 | 日常使用;想知道"这版改了什么"时看发布说明 |
| `@main` | 主分支最新提交,含尚未发布的修复与变更 | `v0.2.1-0.20260901120000-<12位提交hash>`,中段是提交时间(UTC) | 每次重跑安装都可能变 | 跟进最新修复、参与灰度反馈 |
| `@<commit>` | 指定提交 | 同上形态,时间与 hash 为该提交 | 从不 | 复现问题、锁定构建 |

`@main` 经 Go module proxy(尤其镜像代理)可能解析到代理缓存的稍旧提交而非远端最新,装完用 `openace-mcp version` 核对。升级就一步:重跑同一条安装命令。Unix 上旧 daemon 会被下一个新会话自动接管,开着的 IDE 会话也会在下次调用时自己跟上;Windows 需要手动收尾——停掉旧的 `openace-mcp daemon` 进程,再重启 MCP 会话。

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

### 启动方式:预装二进制,还是每次启动时解析(`go run`)

MCP 客户端每次启动 agent 会话都会重新拉起 `command` 指定的进程。`command` 写成预装的二进制,还是写成 `go run …@latest` / `go run …@main`,决定了每一次启动时发生什么:

| `command` 写法 | 每次启动 MCP 时发生什么 | 版本什么时候变 | 代价 |
|---|---|---|---|
| `openace-mcp`(预装二进制,上文第 1 步) | 直接执行本机二进制,不访问网络 | 只在你重跑 `go install` 时 | 无;升级需要你主动执行一次安装命令 |
| `go run …@latest` | 先向 Go module proxy 查询当前最新 tag(本机实测 0.65 秒,该版本已编译过时),有新 tag 后的第一次启动现场下载并编译(本机实测 13–38 秒) | 每次有新正式发布,下一次启动自动换上 | 每次启动都需要网络与 Go 工具链;新版本首次启动慢,可能撞上客户端的 MCP 启动超时 |
| `go run …@main` | 同上,但查询的是主分支最新提交;主分支每前进一次,下一次启动就重新编译 | 每次主分支有新提交 | 同上且更频繁;会拿到尚未发布的变更 |

`go run` 形态的配置(把 `@latest` 换成 `@main` 即跟主分支):

```json
{
  "mcpServers": {
    "openace": {
      "command": "go",
      "args": ["run", "github.com/AoManoh/openace-mcp/cmd/openace-mcp@latest"],
      "env": {
        "GOPROXY": "https://goproxy.cn,direct",
        "GOSUMDB": "sum.golang.google.cn",
        "OPENACE_MODE": "auto"
      }
    }
  }
}
```

两点须知:`args` 里不加 `-tags` 时编译的是全语法体积(约 49MB),加上第 1 步那串 `-tags` 参数可缩小体积、缩短编译;`go run` 模式下会话中途的版本跟随不生效——新版本只在下一次启动 MCP 时换上,连接时对旧 daemon 的自动接管照常工作。日常使用推荐预装二进制;Windows 下客户端找不到 `go` 时,把 `command` 写成 `go.exe` 绝对路径。

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
- **成本边界**:embedding/rerank 的调用与计费发生在你自己的模型服务上。索引期按变更内容付费——未变更 chunk 跨 revision 复用向量,不重复付费;查询期每次消耗一次 query embedding(启用 rerank 时另加一次精排调用)。预算护栏建议设在你的服务/账户侧。大额首建(万级 chunk 以上)可开 `OPENACE_EMBEDDING_BATCH_API=voyage` 走批作业:同样的向量,费用少三分之一,代价是服务端排队(上限 12 小时,实测通常快得多);排队期间词法检索照常可用,daemon 重启会接着等同一个作业,不会重复交钱。
- **数据边界**:索引在本机 cache 目录保存被索引文件的明文片段副本(权限仅当前用户);启用 embedding 时 chunk 内容会发送到你配置的模型服务。使用任何**托管**服务前请自行核实其数据保留与训练条款(多数托管服务默认可将数据用于训练,需显式退出);自部署端点无此顾虑。`.openaceignore` 与内置敏感文件 denylist 先于一切生效。
- **向量身份隔离**:模型/维度/端点、内置嵌入模板版本或切分器版本(如新增 AST 语言批次)任一变化,创建平行索引子树并全量重建(一次 corpus 全量嵌入费用,旧子树保留可回退);换 key 不触发重建。
- **增量索引**:首建后编辑只重建变更文件,删除/重命名立即从结果消失;嵌入费用有界于变更量。delta 链自动本地合并(compaction,零模型调用);索引只保留最近两个 revision,内容按需读取不常驻内存。
- **索引不挤兑查询,限流不停摆**:交互查询与索引批各持独立熔断——大仓索引撞 429 风暴时,你正在敲的检索照常 hybrid。风暴本身由治理器消化:先按 Retry-After 降速续跑(而不是熔断停摆),速率压到地板仍被拒才全停保护费用。自部署服务没有 429,治理器改看延迟:短窗延迟明显偏离基线就收并发窗口,恢复平稳再逐步放开。
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
| `repo_map` | 只读的仓库地图(按目录/文件聚合的索引概览,带预算截断与 `focus` 子树参数);冷仓返回 index_not_ready,不触发索引 |
| `cancel_task` | 取消一个排队或运行中的异步任务 |
| `list_workspaces` | 列出 daemon 已知的工作区及其状态摘要 |

小仓库直接 `codebase_retrieval`;大仓库预热或跨仓问题开完整面后用 `start_*` + `task_status`(进度携带速率与 ETA 估算)。

**检索结果没有字节预算,也没有 `max_output_length` 参数。**默认(`detail=full`)回复的形状是:按排名前 N 个候选带源码正文,其后的每个候选一行 `## 路径:起止行 符号`,中间用一行 `-- remaining results listed as paths only; Read a file to see its content --` 隔开;精排窗口(前 50 个候选)之外的候选前另有一行 `-- results below were not reranked (fused order) --`。任何候选都不会被丢掉,AI 看标题决定是否用自己的 Read 工具展开。N 由**你**在 MCP 配置里设置,不是 AI 的调用参数:`OPENACE_FULL_RESULTS`,默认 20;AI 反馈"结果太长被客户端截断"就调小,反馈"总要多 Read 一轮"就调大;设 0 则全部只给标题行(内容与 `detail=paths` 相同,只多首行一条"以下只列路径"的分隔文字)。`detail=paths` 仍可由 AI 按需选择,只回标题行。本机实测(一次检索 79 个候选):默认 N=20 约 30 KB,N=5 约 13 KB,N=0 约 4 KB。

**按产物类型分组:`artifact_kind`(可选,`any` / `code` / `tests` / `docs`)。**调用 AI 只在使用者明确要某一类文件时设置它:精排完成后,该类型的候选保持原相对顺序排到最前,其余候选按原序跟在后面,任何候选都不丢;每条结果带 `kind` 字段,分组依据可见。省略或 `any` 就是普通排名顺序,一个字节都不变。类型按路径机械规则判定,不猜意图:目录段 `test/`、`tests/`、`spec/`、`__tests__/`、`testdata/`,或文件名含 `_test.`、`.test.`、`.spec.`、以 `test_` 开头、以 `Test`/`Tests` 结尾 → `tests`;目录段 `doc/`、`docs/`、`documentation/`,或扩展名 `.md/.mdx/.rst/.adoc/.txt`,或文件名 `README*`/`CHANGELOG*` → `docs`;其余 → `code`。已知边界:框架自身的 `testing/` 目录算代码,代码目录里的 `.md` 算文档,文件名以 `test` 结尾的非测试文件(如 `latest.go`)会被归为测试。`kind` 只出现在结构化结果 `hits[]` 里,正文标题行不带它;客户端不向 AI 展示结构化字段时,AI 看不到分类结果。分组把未精排的候选提到精排候选之前时,正文不再插入"以下未经 rerank 打分"的分界行,每条结果的 `reranked` 字段仍如实给出。依据:2026-09-03 在 django 快照上的 400 条"找实现"查询,精排把测试/文档排在实现之上,前五命中率因此低 8.75 个百分点;只在输出层把代码排前即可拿回(复算 +8.25 个百分点)。

**其他两个可选参数与结构化字段。**`path_prefix`(索引内相对路径前缀,如 `internal/localengine`):融合之后、精排之前只保留该子树的候选,只在使用者明确点名子树、或上一轮检索已经确认目标在该子树时使用;不要从截断的仓库地图推断前缀。`detail`:`full`(默认)/`paths`。每条结果在结构化字段 `hits[]` 里带 `path`、`start_line`、`end_line`、`symbol`、`rank`、`shown`(恒为 true)、`reranked`(是否经过精排)、`rerank_score`(精排相关度,仅 reranked 时有值)、`source`(`lexical`/`dense`/`both`,来自哪一路召回)、`kind`(`code`/`tests`/`docs`)。这些字段只在结构化结果里,正文不含;不透传结构化字段的客户端里 AI 看不到它们。

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
| `OPENACE_EMBEDDING_BATCH_SIZE` `_MAX_CONCURRENCY` `_RPM_BUDGET` `_TPM_BUDGET` | 索引期调用参数(默认 128 / 16 / 不限 / 不限)。`_MAX_CONCURRENCY` 是并发**上限**:治理器在其内自动寻优,知道自己服务底细就把上限设准(自部署 32-64,免费档 4);`_RPM_BUDGET`/`_TPM_BUDGET` 是治理器不可逾越的硬顶 |
| `OPENACE_THROUGHPUT_GOVERNOR` | 吞吐治理器与车道分离的逃生门:默认 `on`——索引 429 自动降速续跑(尊重 Retry-After,乘性减/加性增),自部署按延迟梯度自动收放并发,交互查询与索引各持独立熔断互不拖累;`off` 回到固定并发+共用熔断的旧行为 |
| `OPENACE_EMBEDDING_BATCH_API` | 离线批车道:`voyage` = 大额嵌入改走 voyage Batch API(费用 -33%,服务端 12h 完成窗,崩溃后续作业不重复付费);默认 `off`。要求 provider 就是 voyage,其他组合启动即报错 |
| `OPENACE_EMBEDDING_BATCH_MIN_CHUNKS` | 批车道触发阈值(默认 `2000`):缺失量低于此走同步车道——几百个 chunk 分钟级就完了,不值得排 12h 窗 |
| `OPENACE_RERANK_PROVIDER` | 精排(质量至上默认档):`tei` / `voyage` / `off`;默认 `voyage`,key 缺省回退 `VOYAGE_API_KEY`。配置即启用;语义已配而精排缺配置时结果携带 `rerank-unconfigured` 提示(`OPENACE_QUALITY_STRICT=on` 下升级为报错),显式 `off` 视为确认放弃。`_BASE_URL`/`_API_KEY`/`_MODEL` 语义同上;`OPENACE_RERANK_MAX_TOKENS` 是单次精排请求送审文本的估算 token 上限(默认 `200000`),超出部分的候选不送审、按融合顺序跟在精排结果之后 |
| `OPENACE_RETRIEVAL_DEGRADE` / `OPENACE_RERANK_DEGRADE` | 语义路/精排失败策略:`allow`(默认,放行并标 `[DEGRADED]`)/ `deny`(返回可行动错误) |
| `OPENACE_QUALITY_STRICT` | `on` = 质量严格档:语义链路任一缺口(覆盖 <100%、查询嵌入失败、已配置的 rerank 未生效等)直接报错;要求已配置 embedding。默认 `off`。结构化结果携带 `rerank_sent`/`query_embed_failed`/`embedding_profile` |
| `OPENACE_QUERY_BUILD_WAIT` | 查询等待在建索引的上界,**默认 `40s`**(先于主流 MCP 客户端的请求超时,冷仓首建期间的同步检索返回带构建进度的可行动错误,而非裸超时):超时后有旧索引按 allow/deny 降级,无旧索引返回带进度的错误;显式 `0` = 等到构建完成 |
| `OPENACE_MCP_TOOLS` | MCP 工具面:未设 = 只暴露 `codebase_retrieval`;`all` = 完整能力面;或逗号清单指定 |
| `OPENACE_RENDER_LINE_NUMBERS` | `1` = 检索结果围栏内逐行携带真实文件行号(`cat -n` 形状,Read-parity 试验面);默认关闭 |
| `OPENACE_FULL_RESULTS` | `detail=full` 时带正文返回的候选块数(按排名取前 N 个,其余只给标题行),默认 `20`;`0`=全部只给标题行。这是使用者侧配置,不是 AI 的调用参数;改动后重启 MCP 会话生效,不需要重启 daemon |
| `OPENACE_FRESHNESS_WINDOW` | 查询前复用最近一次扫描结果的时长(如 `30s`),期间不重扫工作区;留空(默认)每次查询都扫描;不接受 `0` |
| `OPENACE_VECTOR_MEMORY_BUDGET` | 常驻向量的内存上限(字节数);留空(默认)不限 |
| `OPENACE_MAX_FILE_BYTES` / `OPENACE_MAX_TEXT_FILE_BYTES` | 单文件索引上限:一般文件默认 1 MiB,纯文本文件默认 4 MiB,超过整篇不索引 |
| `OPENACE_CACHE_DIR` | 索引缓存根目录;默认用户缓存目录下的 `openace-mcp` |
| `OPENACE_TOOL_TIMEOUT` | 单次 MCP 工具调用的处理超时,默认 `110s` |
| `OPENACE_WATCH_*` / `OPENACE_RECONCILE_CONCURRENCY` | daemon 对已服务过的工作区做变更监测:`OPENACE_WATCH_MODE` `seen`(默认)/`off`;间隔默认 30s、去抖 2s、单次同步超时 5m、探测出错后退避 5s–2m、最多 64 个工作区;并行 reconcile worker 默认 2 |
| `OPENACE_TASK_QUEUE_SIZE` / `OPENACE_TASK_HISTORY_LIMIT` | 异步任务队列长度(默认 256,最大 4096)与保留的已完成任务数(默认 1024) |
| `OPENACE_GRAY_FEEDBACK` | `1` = instructions 追加灰度反馈协议:调用 AI 在任务收尾交付答案时,一次性汇总本次全部 openACE 调用的诊断(事实/效果/体验/耗时/bug 复现),不在每次调用后打断任务。默认关闭 |
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

- **build 过期**:升级后的 wrapper 在连接时自动接管旧 daemon——SIGTERM 请求优雅停机,等它退出,再启动新的,全程实测 0.1 秒;嵌入进度有断点日志,付过费的向量一条不丢。接管前先判断谁新谁旧:源码构建比较 vcs 提交时间,`go install` 装的模块构建比较伪版本里的提交时间或正式 tag 的版本序;两边判断不出先后(例如 `v0.2.0` 之前装的 `@main` 二进制,版本号以 `v0.0.0-` 开头,对上 tag 版本的新 wrapper)就不接管,错误文本给出旧 daemon 的 pid,手动停一次即可。Windows 没有对应的信号语义,保持显式报错,错误文本里同样带 pid 和一条可复制的修复命令。
- **provider/降级 env 变了**:这是你改了配置意图,不是版本过期,wrapper 不会替你猜。它按配置指纹拒绝复用并明确报错,按提示重启 daemon 即生效。

**升级不打断 IDE 会话**(Unix)。升级后,开着的 MCP 会话在下一次调用时发现 daemon 已换代,wrapper 就原地 exec 磁盘上的新版自身:进程号不变,标准流不断,触发的那条请求被保存下来由新进程重放,管线里排队的请求也一并带过去。你看到的只是一次正常应答。自愈失败时——比如磁盘二进制反而旧于 daemon——按原样返回可行动硬错,30 秒冷却防止 exec 打转。Windows 无 exec 语义,保持"重启 MCP 会话"提示。

## 按场景选配置

环境变量的说明见上表与 `.env.example`,这里只给三组常用组合。

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
