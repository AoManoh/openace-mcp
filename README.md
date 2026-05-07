# openACE MCP

`openACE` 是 **Open Adapter for Context Engine**，用于把用户自己的 Augment / ACE codebase retrieval 能力接入 AI IDE 的 MCP 工具面。

它不是离线语义检索引擎，也不提供自研 embedding / ranking。openACE 做的是本地工程侧编排：扫描 workspace、过滤文件、计算 blob、同步 checkpoint/cache、调用上游 ACE、提供 MCP 工具、管理 daemon 生命周期和任务队列。

## 当前状态

- Go MCP stdio 主流程已跑通。
- 默认 `OPENACE_MODE=auto`，AI IDE 只需要配置一个 MCP server；`openace-mcp` 会自动复用或启动本机 daemon。
- daemon 异步任务使用可配置 worker pool，默认 4 个 worker。
- daemon 任务快照已支持本地持久化，重启后仍可通过 `list_tasks` / `task_status` 找回最近任务。
- 支持可选 `provider_profile_id` 显式选择 ACE provider profile；同一 daemon 只共享控制面，provider 的 ACE client、checkpoint state、inflight、watch 和上游退避状态互相隔离。
- 已在大仓库场景完成 5 并发冷/热缓存压力测试。
- 默认扫描会跳过 workspace 内 `.gitignore` / `.ignore` 命中内容，并排除 `.env*`、session、credentials、私钥、证书等敏感文件；需要索引被 Git 忽略的项目资产时，可用 `.augmentignore` 显式纳入。

## 你需要准备

- Go `>= 1.23`
- 可用的 Augment / ACE 账号凭据
- 一个支持 MCP JSON 配置的 AI IDE

认证方式任选一种：

- `AUGMENT_TOKEN` + `AUGMENT_TENANT`
- `AUGMENT_SESSION_AUTH`
- `OPENACE_SESSION_FILE`
- 已登录插件留下的 `~/.augment/session.json`
- `OPENACE_PROFILES_FILE`，用于需要显式切换多个 ACE provider profile 的高级场景

## 多 Provider Profile

默认情况下 openACE 沿用单账号凭据链。需要在同一个 daemon 里测试或显式使用多个账号时，可以配置一个本地 profile 文件：

```json
{
  "default_profile_id": "primary",
  "profiles": [
    {
      "id": "primary",
      "accessToken": "your-primary-token",
      "tenantURL": "https://<primary-tenant>.api.augmentcode.com/"
    },
    {
      "id": "standby",
      "accessToken": "your-standby-token",
      "tenantURL": "https://<standby-tenant>.api.augmentcode.com/"
    }
  ]
}
```

然后在 MCP 或 daemon 环境里设置：

```bash
export OPENACE_PROFILES_FILE="/absolute/path/to/openace-profiles.json"
```

profile 文件包含上游 token/session，必须当作本地 secret 管理，不要提交到 Git。

`codebase_retrieval`、`multi_codebase_retrieval`、`sync_workspace`、`start_*` 和 `workspace_status` 都支持可选 `provider_profile_id`；省略时使用默认 profile。

profile 在 daemon 启动时加载；修改 profile 文件或从单账号切换到多 profile 后，需要重启已有 daemon，避免 shim 复用旧 daemon。

边界：openACE 不会因为某个 profile 出现 `429`、quota exhausted 或临时不可用而自动切换到另一个 profile。Augment / ACE 的代码索引跟随账号/租户，上游索引状态可能不同；自动接管会把“可用账号”误当成“同等索引状态”。当前实现只提供显式调度和状态隔离，是否切换由调用方或用户决定。

## 推荐模式

| 模式 | 适合场景 | 说明 |
|------|----------|------|
| `auto` | 日常默认、大仓库、多 AI 会话 | 推荐。一个 MCP 配置，自动复用或启动本机 daemon |
| `direct` | 小仓库、第一次 smoke test、排障 fallback | 不启动 daemon，每个 MCP 进程自己扫描和检索 |
| `manual-daemon` | 高级运维、固定服务、远程/团队部署 | 需要用户自己管理 daemon 生命周期 |

`direct` 不等于 Augment Code 插件原生体验。它只是在同步完成后调用同源 ACE retrieval，适合验证链路是否可用；它没有插件的 IDE 预热、watcher 和会话集成，也不能解决 subagent 链式进程膨胀。

`auto` / daemon 模式也不是“复刻插件 UI”。它提供的是面向 AI Agent 的 openACE 原生 MCP 体验：复用用户 ACE 能力，同时增加本地状态共享、任务管理、资源收敛和可观测性。

## AI IDE 配置：默认 auto 模式

多数 AI IDE 需要把下面对象放到自己的 `mcpServers` 里。日常只需要这一段配置，不需要手动启动 `openace-daemon`。

```json
"openace-mcp": {
  "args": [
    "run",
    "github.com/AoManoh/openace-mcp/cmd/openace-mcp@main"
  ],
  "command": "go",
  "disabled": false,
  "env": {
    "GOPROXY": "https://goproxy.cn,direct",
    "GOSUMDB": "sum.golang.google.cn",
    "AUGMENT_TOKEN": "your-augment-token",
    "AUGMENT_TENANT": "https://<your-tenant>.api.augmentcode.com/",
    "OPENACE_MODE": "auto",
    "OPENACE_CACHE_NAMESPACE": "default"
  }
}
```

如果你的 IDE 要求完整配置：

```json
{
  "mcpServers": {
    "openace-mcp": {
      "args": [
        "run",
        "github.com/AoManoh/openace-mcp/cmd/openace-mcp@main"
      ],
      "command": "go",
      "disabled": false,
      "env": {
        "GOPROXY": "https://goproxy.cn,direct",
        "GOSUMDB": "sum.golang.google.cn",
        "AUGMENT_TOKEN": "your-augment-token",
        "AUGMENT_TENANT": "https://<your-tenant>.api.augmentcode.com/",
        "OPENACE_MODE": "auto",
        "OPENACE_CACHE_NAMESPACE": "default"
      }
    }
  }
}
```

> **注意**：AI IDE 启动 MCP 子进程时不会经过 shell，`$HOME`、`%USERPROFILE%` 这类占位符**不会自动展开**。openACE 已在内部兼容这些写法，但仍建议在 IDE 配置里写**绝对路径**或直接省略 `OPENACE_CACHE_DIR`（默认走 `os.UserCacheDir()`，Windows 自动落到 `%LocalAppData%\openace-mcp`，macOS 落到 `~/Library/Caches/openace-mcp`，Linux 落到 `~/.cache/openace-mcp`）。

Windows 下如果 IDE 找不到 `go`，把 `command` 改成你的 `go.exe` 绝对路径。

auto 模式会先连接 `OPENACE_DAEMON_ADDR`，其次使用 `OPENACE_DAEMON_LISTEN_ADDR`，默认是 `127.0.0.1:8765`；如果没有现成 daemon，就自动启动当前 `openace-mcp` 二进制的内部 daemon 子进程。daemon 默认只允许 loopback 监听，不要把它直接暴露到公网。

## 本地安装方式

如果不想让 AI IDE 每次 `go run` 在线拉取，可以先安装二进制：

```bash
GOPROXY=https://goproxy.cn,direct go install github.com/AoManoh/openace-mcp/cmd/openace-mcp@main
```

然后把 MCP 配置改成：

```json
"openace-mcp": {
  "command": "openace-mcp",
  "args": [],
  "disabled": false,
  "env": {
    "AUGMENT_TOKEN": "your-augment-token",
    "AUGMENT_TENANT": "https://<your-tenant>.api.augmentcode.com/",
    "OPENACE_MODE": "auto"
  }
}
```

如果 AI IDE 找不到命令，把 `command` 改成绝对路径。例如 Linux/macOS 用 `/home/you/go/bin/openace-mcp`，Windows 用 `C:\Users\you\go\bin\openace-mcp.exe`。AI IDE 不会自动展开 `$HOME` 或 `%USERPROFILE%`。

## 高级模式

### direct

只想做 smoke test 或排查 daemon 问题时，可以设置：

```json
"OPENACE_MODE": "direct"
```

此时只会暴露同步工具，不会暴露 `start_*`、`task_status`、`list_tasks`、`list_workspaces` 等 daemon 工具。

### manual-daemon

如果你要自己用 systemd、终端、远程机器或固定端口管理 daemon，可以先启动：

```bash
export AUGMENT_TOKEN="your-augment-token"
export AUGMENT_TENANT="https://<your-tenant>.api.augmentcode.com/"
export OPENACE_DAEMON_LISTEN_ADDR="127.0.0.1:8765"
openace-mcp daemon
```

然后在 MCP 配置里设置：

```json
"OPENACE_MODE": "manual-daemon",
"OPENACE_DAEMON_ADDR": "127.0.0.1:8765"
```

## MCP 工具

| 工具 | 用途 |
|------|------|
| `codebase_retrieval` | 同步扫描 workspace，然后调用 ACE 检索代码 |
| `multi_codebase_retrieval` | 显式传入多个 workspace，分别检索并按 workspace 分段返回结果 |
| `sync_workspace` | 只同步 workspace，不做检索 |
| `start_codebase_retrieval` | daemon 模式下提交异步检索任务，适合大仓库 |
| `start_multi_codebase_retrieval` | daemon 模式下提交异步跨仓检索任务 |
| `start_sync_workspace` | daemon 模式下提交异步同步任务 |
| `task_status` | 查询任务状态和结果 |
| `list_tasks` | 找回最近任务，列表不返回完整检索正文 |
| `cancel_task` | 取消 queued / running 任务 |
| `list_workspaces` | daemon 模式下列出已见 workspace 状态 |
| `workspace_status` | daemon 模式下查询指定 workspace 的 checkpoint、文件数、同步中状态、最近错误和上游退避摘要 |

建议：

- 小仓库先用 `codebase_retrieval`。
- 跨仓问题用 `multi_codebase_retrieval`，显式传入 `directory_paths`；大仓库跨仓场景优先用 `start_multi_codebase_retrieval`。
- 大仓库优先用 `start_codebase_retrieval`，再用 `task_status` 查询。
- daemon 重启后可继续用 `list_tasks` 找回最近 completed / failed / cancelled 任务；重启前仍在 queued / running 的任务会标记为 failed，并附带 `abandoned after daemon restart`。
- 忘记 task id 时用 `list_tasks`。
- 多项目或压测排查时用 `list_workspaces` 和 `workspace_status` 观察 daemon 状态。
- 多 provider 压测或排障时显式传 `provider_profile_id`；不要把它当作自动 failover 开关。

## 环境变量

| 变量 | 说明 |
|------|------|
| `AUGMENT_TOKEN` | 上游 ACE access token |
| `AUGMENT_TENANT` | 上游 ACE tenant/base URL |
| `AUGMENT_SESSION_AUTH` | 完整 session JSON，优先级最高 |
| `OPENACE_SESSION_FILE` | 显式 session 文件路径 |
| `OPENACE_PROFILES_FILE` | 多 provider profile JSON；设置后替代单账号凭据链，工具通过 `provider_profile_id` 显式选择 |
| `OPENACE_MODE` | `auto` / `direct` / `manual-daemon`，默认 `auto` |
| `OPENACE_CACHE_DIR` | workspace checkpoint/cache 目录，省略时走 `os.UserCacheDir()`；支持 `~/`、`$HOME`、`${HOME}`、`%USERPROFILE%` 占位符，但仍推荐绝对路径 |
| `OPENACE_CACHE_NAMESPACE` | cache 命名空间，用于隔离账号、tenant 或测试批次 |
| `OPENACE_DAEMON_ADDR` | MCP shim 连接 daemon 的地址 |
| `OPENACE_DAEMON_LISTEN_ADDR` | daemon 监听地址，默认 `127.0.0.1:8765` |
| `OPENACE_DAEMON_TOKEN` | 可选本地 bearer token |
| `OPENACE_DAEMON_START_TIMEOUT` | auto 模式等待托管 daemon ready 的时间，默认 `10s` |
| `OPENACE_TASK_WORKERS` | daemon 异步任务 worker 数，默认 `4`，最大 `32` |
| `OPENACE_TASK_QUEUE_SIZE` | daemon 异步任务队列容量，默认 `256`，最大 `4096` |
| `OPENACE_TASK_HISTORY_LIMIT` | daemon 保留最近任务快照数量，默认 `1024`，最大 `8192` |
| `OPENACE_TASK_STORE_DIR` | daemon 任务快照目录；默认位于 `OPENACE_CACHE_DIR/tasks/<namespace>` 或用户 cache 目录 |
| `OPENACE_WATCH_MODE` | daemon 已见 workspace 后台增量检查模式，默认 `seen`；设为 `off` 可关闭 |
| `OPENACE_WATCH_INTERVAL` | 后台检查无变化 workspace 的间隔，默认 `30s` |
| `OPENACE_WATCH_DEBOUNCE` | workspace 被显式访问后首次后台检查的防抖时间，默认 `2s` |
| `OPENACE_WATCH_TIMEOUT` | 单次后台检查和同步的超时，默认 `5m` |
| `OPENACE_WATCH_BACKOFF_MIN` | 后台探测失败后的最小重试退避，默认 `5s` |
| `OPENACE_WATCH_BACKOFF_MAX` | 后台探测失败后的最大重试退避，默认 `2m` |
| `OPENACE_WATCH_MAX_WORKSPACES` | 单个 daemon 维护的已见 workspace 上限，默认 `64` |
| `OPENACE_TOOL_TIMEOUT` | 同步 MCP 工具调用超时，默认 `110s`；大仓库长任务优先使用 `start_*` 异步工具 |
| `OPENACE_RETRIEVAL_TIMEOUT` | 单次上游 ACE retrieval 超时，默认 `90s` |
| `OPENACE_ALLOW_REMOTE_DAEMON` | 显式允许监听非 loopback 地址 |
| `OPENACE_UPLOAD_BATCH_BYTES` | batch-upload 估算上限，默认 `1048576` |
| `OPENACE_FIND_MISSING_BATCH_SIZE` | find-missing 分批 blob 数，默认 `1000` |
| `OPENACE_MAX_FILE_BYTES` | 单文件索引上限，默认 `1048576` |

`.env` 不会被 openACE 自动加载。如果你使用 `.env`，需要在启动 daemon 或 MCP 前手动加载：

```bash
set -a
source .env
set +a
```

## 索引范围与项目资产

openACE 默认把 `.gitignore` / `.ignore` 当作索引排除规则，这能避免上传依赖目录、构建产物和多数不应进入代码检索的本地文件。它也支持 `.augmentignore` 作为索引策略覆盖层：`.augmentignore` 只影响 openACE 本地扫描和 ACE 同步，不会改变 Git 跟踪状态，也不会把私有文档写入公共 Git 历史。

Git ignore 和 openACE index ignore 是两条不同边界：`docs/`、`skills/` 可以继续保持 Git 忽略、不推送，但只要你显式选择，它们仍可作为本机 AI 知识资产进入 openACE 索引。推荐把真实 `.augmentignore` 当成本地配置，加入项目 `.gitignore` 或 `.git/info/exclude`，不要把私有索引策略误提交给公共仓库。

如果你的项目把 `docs/`、`skills/`、`AGENTS.md`、`CLAUDE.md`、`.augment-guidelines`、`.augment/rules/`、本地 runbook 或设计文档放在 `.gitignore` 里，但希望 AI 检索时能看到这些知识资产，可以在 workspace root 放置 `.augmentignore`：

```gitignore
!AGENTS.md
!CLAUDE.md
!.augment-guidelines
!.augment/
!.augment/rules/
!.augment/rules/**/
!.augment/rules/*.md
!.augment/rules/**/*.md
!docs/
!docs/**/
!docs/*.md
!docs/**/*.md
!skills/
!skills/**/
!skills/**/SKILL.md
!skills/**/SPEC.md
```

目录被 `.gitignore` 排除时，需要先 re-include 目录本身，再 re-include 需要的子路径。上面的示例会让 openACE 索引 `AGENTS.md`、`CLAUDE.md`、`.augment-guidelines`、`.augment/rules/`、`docs/` 下 Markdown 资产和 `skills/` 下的 `SKILL.md` / `SPEC.md` 知识文件，但不会改变这些文件是否被 Git 提交。

`.augmentignore` 不能绕过 hard safety denylist：`.env*`、session/token/credentials、私钥、证书和 keystore 类文件仍会被跳过。若你不希望 `.augmentignore` 本身进入 Git，可以把它加入项目 `.gitignore` 或 `.git/info/exclude`；openACE 仍会读取它作为本地索引策略。

`AGENTS.md`、`CLAUDE.md`、`.augment-guidelines` 和 `.augment/rules/` 在官方插件中属于规则/指南上下文。openACE 当前作为 MCP retrieval adapter，会在用户显式 `.augmentignore` 授权后把这些文件作为可检索资产同步；完整的规则 prompt 注入、IDE 权限 UI 和编辑器事件通道不是本轮实现范围。

## 后台增量与状态

daemon 模式会记住已经被 `sync_workspace` / `codebase_retrieval` / `multi_codebase_retrieval` 显式访问过的 workspace，并在后台做增量检查。后台检查先在本地重新扫描索引范围并比较 blob 集；只有发现新增、删除或内容变化时，才触发后台 `sync` 进入 ACE 上传与 checkpoint 流程，避免无变化时反复消耗上游 quota。配置多 provider profile 时，已见 workspace、inflight、checkpoint/cache 和后台 watch 状态都会按 provider profile 隔离；一个 profile 的冷索引、错误或退避不会污染另一个 profile。

`workspace_status` 和 `list_workspaces` 会返回可解释状态字段：`stage` 表示当前同步阶段（`scanning` / `reconciling` / `uploading` / `checkpointing` / `ready` / `failed`），`provider_profile_id` / `provider_state` 表示当前 provider 维度的本地状态，`last_sync_reason` 区分 `manual`、`retrieval` 和 `background`，watch 字段会展示后台检查是否启用、下次检查时间、最近检查时间、最近后台同步时间和探测错误。后台探测失败会按 `OPENACE_WATCH_BACKOFF_MIN` / `OPENACE_WATCH_BACKOFF_MAX` 退避重试。上游 ACE 返回 `429` / `5xx` 时，status 还会通过 `upstream_status`、`upstream_last_status_code`、`upstream_retry_after`、`upstream_backoff_until`、`upstream_last_error`、`upstream_last_failure` 和 `upstream_last_success` 暴露当前 provider 的最近退避与错误摘要；这些字段不是单 workspace 指标，而是帮助多 agent 场景判断是否应暂停真实上游压力的上游信号。

## 排障：checkpoint-blobs 400

如果看到 `checkpoint-blobs returned HTTP 400: Json deserialize error: invalid type: null, expected a sequence`，先确认运行的是固定 commit/tag 或本地二进制，而不是不可复现的浮动 `@main`。当前版本会把 `added_blobs` / `deleted_blobs` 显式编码为数组，即使为空也是 `[]`，不会发送 `null`。

较新的错误信息会附带脱敏请求形态，例如 `request_shape=blobs.added_blobs=array(len=12) blobs.deleted_blobs=array(len=0)`；这里只包含字段类型和数量，不包含 blob 名、文件内容、token 或 tenant。若该形态显示 `null`，请升级到包含该保护的版本；若形态已经是 `array(...)` 但上游仍返回 400，请保留 exact commit、`workspace_status` 的 `stage/last_error_stage/last_error/last_added/last_deleted/last_uploaded`，再排查旧 checkpoint、大量删除/重命名、Windows 路径大小写变化或上游临时兼容问题。

## 安全与边界

- openACE 不绕过 Augment / ACE 的认证、quota、tenant 或 rate limit。
- 检索质量取决于你的上游 ACE 账号和服务状态。
- `OPENACE_PROFILES_FILE` 指向的 profile 文件包含凭据，应作为本地 secret 保存，不应进入公共 Git 历史。
- 默认只索引 UTF-8 文本文件，并跳过常见敏感文件；`.augmentignore` 只能覆盖索引排除规则，不能覆盖 hard safety denylist。
- daemon 默认只允许 loopback 监听；远程暴露前必须自行加网络访问控制。

## 压力测试基线

匿名化验证对象：大仓库。

- 原始仓库包含数千个文件。
- openACE 过滤后参与索引约 2500 个文本文件。
- 冷缓存 5 并发宽泛检索全部 completed：55.1 秒，daemon RSS 高峰约 20.9MB。
- 暖缓存 daemon 重启后 5 并发全部 completed：30.0 秒，全部 `uploaded=0 added=0 deleted=0`，daemon RSS 高峰约 20.8MB。

## 本地开发

```bash
go test ./...
go test -race ./internal/daemon ./internal/mcp ./internal/workspace
go vet ./...
go build ./cmd/openace-mcp ./cmd/openace-daemon
```

## License

MIT License. Copyright (c) 2026 aomanoh.
