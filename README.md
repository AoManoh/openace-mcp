# openACE MCP

`openACE` 是 **Open Adapter for Context Engine**。

它把你自己的 Augment / ACE codebase retrieval 能力接到 AI IDE 的 MCP 工具里，让 AI agent 可以检索本地仓库，而不是为每个会话、每个 subagent 反复启动重型进程、重复扫描 workspace。

openACE 做的是本地编排：扫描 workspace、过滤文件、同步 checkpoint/cache、调用上游 ACE、提供 MCP 工具、复用本机 daemon。它不是离线语义检索引擎，不提供自研 embedding / ranking，也不会绕过 Augment / ACE 的认证、quota、tenant 或 rate limit。

当前日常推荐路径是 **单租户 + `OPENACE_MODE=auto`**。多 Provider Profile 已提供，但仍属于实验性/高级能力，不是自动高可用系统。

## 快速开始：直接在 AI IDE 里配置

多数用户只需要把下面配置放进 AI IDE 的 `mcpServers`。这段配置不要求你提前安装 `openace-mcp`，AI IDE 会通过 `go run` 启动最新代码。

```json
{
  "mcpServers": {
    "openace-mcp": {
      "command": "go",
      "args": [
        "run",
        "github.com/AoManoh/openace-mcp/cmd/openace-mcp@main"
      ],
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

如果你的 AI IDE 只要求填写单个 server 对象，就只复制 `openace-mcp` 里面那层：

```json
"openace-mcp": {
  "command": "go",
  "args": [
    "run",
    "github.com/AoManoh/openace-mcp/cmd/openace-mcp@main"
  ],
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

`@main` 表示跟随最新代码。想要固定版本时，把 `@main` 换成 `@<release-tag-or-commit>`。

如果你不想马上体验最新版本，可以先固定到上一条单租户稳定基线：

```text
github.com/AoManoh/openace-mcp/cmd/openace-mcp@14af3f1
```

`14af3f1` 是引入多 Provider Profile 前、此前远端长期可用的单租户版本。后续如果项目发布正式 tag，也可以把它替换成对应 tag。

Windows 下如果 AI IDE 找不到 `go`，把 `command` 改成 `go.exe` 的绝对路径。

## 你需要准备什么

- Go `>= 1.23`
- 一个可用的 Augment / ACE 账号
- 支持 MCP JSON 配置的 AI IDE

认证方式任选一种：

- `AUGMENT_TOKEN` + `AUGMENT_TENANT`
- `AUGMENT_SESSION_AUTH`
- `OPENACE_SESSION_FILE`
- 已登录插件留下的 `~/.augment/session.json`

最简单的是配置 `AUGMENT_TOKEN` 和 `AUGMENT_TENANT`。如果你只有一个账号或租户，不要配置 `OPENACE_PROFILES_FILE`，也不需要理解 `provider_profile_id`。

## 默认推荐：单租户

单租户是当前最稳定、最清晰的日常路径。

在这个模式下，AI IDE 只启动一个 `openace-mcp`。`OPENACE_MODE=auto` 会让它优先复用本机已有 daemon；如果没有 daemon，就自动托管一个内部 daemon。多个 AI 会话可以共享本机状态，避免重复扫描、重复上传和重复维护 checkpoint。

单租户模式下：

- 不需要 profile 文件。
- 不需要给 MCP 工具传 `provider_profile_id`。
- 不会引入多个账号之间的 checkpoint、quota、索引状态差异。

## 可选：先安装到本机

如果你不想让 AI IDE 每次通过 `go run` 拉取和编译，可以先安装二进制。

Linux / macOS / WSL:

```bash
GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn \
go install github.com/AoManoh/openace-mcp/cmd/openace-mcp@main
```

Windows PowerShell:

```powershell
$env:GOPROXY="https://goproxy.cn,direct"
$env:GOSUMDB="sum.golang.google.cn"
go install github.com/AoManoh/openace-mcp/cmd/openace-mcp@main
```

安装后，MCP 配置可以改成直接调用本地命令：

```json
{
  "mcpServers": {
    "openace-mcp": {
      "command": "openace-mcp",
      "args": [],
      "disabled": false,
      "env": {
        "AUGMENT_TOKEN": "your-augment-token",
        "AUGMENT_TENANT": "https://<your-tenant>.api.augmentcode.com/",
        "OPENACE_MODE": "auto",
        "OPENACE_CACHE_NAMESPACE": "default"
      }
    }
  }
}
```

如果 AI IDE 找不到 `openace-mcp`，把 `command` 改成二进制绝对路径，例如 `/home/you/go/bin/openace-mcp` 或 `C:\\Users\\you\\go\\bin\\openace-mcp.exe`。

后续想更新到最新版本时，重新执行同一条 `go install ...@main` 命令，然后重启 AI IDE 的 MCP 会话。已经运行中的 `openace-mcp` / daemon 进程不会自动热更新，必须重启后才会使用新二进制。

如果你想固定稳定版本，本地安装也可以使用 commit：

```bash
GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn \
go install github.com/AoManoh/openace-mcp/cmd/openace-mcp@14af3f1
```

## 实验性能力：多 Provider Profile

多 Provider Profile 用来让同一个 daemon 识别多个 ACE 账号或租户，适合显式测试、排障、手动切换账号。它不是自动 failover，也不是自动高可用。

只有在你确实需要多个账号时，才创建本地 profile 文件，例如 `/absolute/path/to/openace-profiles.json`：

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

然后在 MCP 配置里直接指定 `OPENACE_PROFILES_FILE`：

```json
{
  "mcpServers": {
    "openace-mcp": {
      "command": "go",
      "args": [
        "run",
        "github.com/AoManoh/openace-mcp/cmd/openace-mcp@main"
      ],
      "disabled": false,
      "env": {
        "GOPROXY": "https://goproxy.cn,direct",
        "GOSUMDB": "sum.golang.google.cn",
        "OPENACE_MODE": "auto",
        "OPENACE_CACHE_NAMESPACE": "default",
        "OPENACE_PROFILES_FILE": "/absolute/path/to/openace-profiles.json"
      }
    }
  }
}
```

profile 文件包含 token/session，必须当作本地 secret 管理，不要提交到 Git。

多 profile 的关键边界：

- openACE 不会因为 `primary` 被限流、封禁、quota 用完或临时不可用，就自动切到 `standby`。
- ACE 的代码索引跟随账号、租户和 checkpoint；另一个账号有 token，不代表它已经同步过同一个 workspace。
- 当前实现只提供显式选择、状态隔离和状态可见性。
- 如果要让备用账号可用，需要先用备用 profile 对同一 workspace 独立同步并确认 ready。

在工具调用里显式选择 profile：

```json
{
  "directory_path": "/absolute/path/to/workspace",
  "information_request": "Find the daemon startup flow",
  "provider_profile_id": "standby"
}
```

备用 profile 还没同步过 workspace 时，先调用 `sync_workspace` 或 `start_sync_workspace`：

```json
{
  "directory_path": "/absolute/path/to/workspace",
  "provider_profile_id": "standby"
}
```

修改 profile 文件后，需要重启已有 daemon，否则 MCP shim 可能仍在复用旧 daemon。

## MCP 工具

常用工具：

| 工具 | 用途 |
|------|------|
| `codebase_retrieval` | 同步当前 workspace，然后调用 ACE 检索代码 |
| `multi_codebase_retrieval` | 显式传入多个 workspace，分仓检索并返回结果 |
| `sync_workspace` | 只同步 workspace，不做检索 |
| `start_codebase_retrieval` | daemon 模式下提交异步检索任务，适合大仓库 |
| `start_multi_codebase_retrieval` | daemon 模式下提交异步跨仓检索任务 |
| `start_sync_workspace` | daemon 模式下提交异步同步任务 |
| `task_status` | 查询异步任务状态和结果 |
| `list_tasks` | 找回最近任务，列表不返回完整检索正文 |
| `workspace_status` | 查看 workspace checkpoint、同步阶段、最近错误和上游退避摘要 |
| `daemon_status` | 查看 MCP wrapper 与 daemon 的 build、pid、cache namespace、capability |

小仓库可以直接用 `codebase_retrieval`。大仓库或跨仓问题优先用 `start_*` 提交异步任务，再用 `task_status` 查询。

## 运行模式

| 模式 | 适合场景 | 说明 |
|------|----------|------|
| `auto` | 日常默认、大仓库、多 AI 会话 | 推荐。自动复用或托管本机 daemon |
| `direct` | 小仓库 smoke test、排障 fallback | 不启动 daemon，每个 MCP 进程自己扫描和检索 |
| `manual-daemon` | 高级运维、固定服务、远程部署 | 用户自己管理 daemon 生命周期 |

`direct` 只是排障和小仓库 fallback，不是插件原生体验。日常使用优先保持 `OPENACE_MODE=auto`。

`auto` 只会复用暴露运行时身份能力且 build revision 兼容的 daemon。若本机 `127.0.0.1:8765` 上已有旧 daemon、缺少 `runtime_identity` capability，或能确认与当前 MCP wrapper revision 不一致，MCP shim 会明确失败并要求重启/升级，而不是静默复用一个未知版本的长驻进程。daemon 返回的 sync、retrieve、workspace status 和 task 响应也会带 `served_by`，用于排查 WSL/Windows、多 IDE、多 cache namespace 混用时到底是哪一个 daemon 在响应。

## 索引范围与安全边界

openACE 默认尊重 `.gitignore` / `.ignore`，并跳过 `.env*`、session、credentials、私钥、证书和 keystore 等敏感文件。

如果你的项目把本地知识资产放在 Git ignore 里，但希望 AI 检索时能看到，可以用 `.augmentignore` 显式纳入。`.augmentignore` 只影响 openACE 本地扫描和 ACE 同步，不改变 Git 跟踪状态，也不能绕过 hard safety denylist。

示例：

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

真实 `.augmentignore` 推荐作为本地配置管理，不要误提交私有索引策略。

## 常用环境变量

| 变量 | 说明 |
|------|------|
| `AUGMENT_TOKEN` | 上游 ACE access token |
| `AUGMENT_TENANT` | 上游 ACE tenant/base URL |
| `AUGMENT_SESSION_AUTH` | 完整 session JSON，优先级最高 |
| `OPENACE_SESSION_FILE` | 显式 session 文件路径 |
| `OPENACE_PROFILES_FILE` | 实验性多 profile JSON；设置后替代单账号凭据链 |
| `OPENACE_MODE` | `auto` / `direct` / `manual-daemon`，默认 `auto` |
| `OPENACE_CACHE_NAMESPACE` | cache 命名空间，用于隔离账号、tenant 或测试批次 |
| `OPENACE_DAEMON_ADDR` | MCP shim 连接 daemon 的地址 |
| `OPENACE_DAEMON_LISTEN_ADDR` | daemon 监听地址，默认 `127.0.0.1:8765` |
| `OPENACE_TASK_WORKERS` | daemon 异步任务 worker 数，默认 `4` |
| `OPENACE_TOOL_TIMEOUT` | 同步 MCP 工具调用超时，默认 `110s` |
| `OPENACE_RETRIEVAL_TIMEOUT` | 单次上游 ACE retrieval 超时，默认 `90s` |

daemon 默认只监听 loopback。不要把 daemon 直接暴露到公网。

## 排障提示

- IDE 启动 MCP 子进程时通常不会经过 shell，`$HOME`、`%USERPROFILE%` 这类占位符不一定会展开。配置路径时优先写绝对路径。
- 使用本地安装方式后，升级需要重新 `go install` 并重启 MCP 会话；运行中的进程不会自动换成新版本。
- 切换 `OPENACE_PROFILES_FILE` 或修改 profile 文件后，需要重启 daemon。
- WSL 里如果复用 Windows daemon，可以传 `D:\\project` 或 `/mnt/d/project`；Windows daemon 会把 WSL mount 路径规范化为 Windows drive path。非 WSL 的 POSIX 路径会被拒绝，避免产生 `C:\\mnt\\...` 这类无效 workspace identity。
- 最新版本会在已有 checkpoint 的 `checkpoint-blobs` HTTP 400 上自动尝试一次 fresh checkpoint；如果旧版本仍持续失败，先确认你运行的是哪个 commit/tag，并优先升级到最新 `@main` 或固定到已验证版本复现。

## 本地开发

```bash
go test ./...
go test -race ./internal/daemon ./internal/mcp ./internal/workspace
go vet ./...
go build ./cmd/openace-mcp ./cmd/openace-daemon
```

## License

MIT License. Copyright (c) 2026 aomanoh.
