# openACE MCP

`openACE` 是 **Open Adapter for Context Engine**，用于把用户自己的 Augment / ACE codebase retrieval 能力接入 AI IDE 的 MCP 工具面。

它不是离线语义检索引擎，也不提供自研 embedding / ranking。openACE 做的是本地工程侧编排：扫描 workspace、过滤文件、计算 blob、同步 checkpoint/cache、调用上游 ACE、提供 MCP 工具、管理 daemon 生命周期和任务队列。

## 当前状态

- Go MCP stdio 主流程已跑通。
- daemon + MCP shim 已跑通，适合大仓库和多 AI 会话共享状态。
- 已在约 5GB 大仓库上完成 5 并发冷/热缓存压力测试。
- 默认扫描会跳过 `.gitignore` / `.ignore` 命中内容，并排除 `.env*`、session、credentials、私钥、证书等敏感文件。

## 你需要准备

- Go `>= 1.23`
- 可用的 Augment / ACE 账号凭据
- 一个支持 MCP JSON 配置的 AI IDE

认证方式任选一种：

- `AUGMENT_TOKEN` + `AUGMENT_TENANT`
- `AUGMENT_SESSION_AUTH`
- `OPENACE_SESSION_FILE`
- 已登录插件留下的 `~/.augment/session.json`

## 推荐模式

| 模式 | 适合场景 | 说明 |
|------|----------|------|
| 直连 MCP | 小仓库、第一次 smoke test | AI IDE 每次启动 MCP 时直接扫描和检索 |
| daemon + shim | 大仓库、多 AI 会话、长期使用 | 推荐。daemon 常驻，AI IDE 只启动轻量 shim |

大仓库建议优先用 daemon + shim，并调用 `start_codebase_retrieval` 异步检索。

## AI IDE 配置：直连 MCP

这是最简单的通用 MCP JSON 片段。多数 AI IDE 需要把下面对象放到自己的 `mcpServers` 里。

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
    "OPENACE_CACHE_DIR": "$HOME/.cache/openace-mcp",
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
        "OPENACE_CACHE_DIR": "$HOME/.cache/openace-mcp",
        "OPENACE_CACHE_NAMESPACE": "default"
      }
    }
  }
}
```

Windows 下如果 IDE 找不到 `go`，把 `command` 改成你的 `go.exe` 绝对路径。

## AI IDE 配置：daemon + shim

第一步，先在终端启动 daemon。上游 ACE 凭据放在 daemon 进程里，不要只放在 AI IDE 的 MCP shim 里。

```bash
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=sum.golang.google.cn
export AUGMENT_TOKEN="your-augment-token"
export AUGMENT_TENANT="https://<your-tenant>.api.augmentcode.com/"
export OPENACE_CACHE_DIR="$HOME/.cache/openace-mcp"
export OPENACE_CACHE_NAMESPACE="default"
export OPENACE_DAEMON_LISTEN_ADDR="127.0.0.1:8765"
export OPENACE_DAEMON_TOKEN="change-me-local-token"

go run github.com/AoManoh/openace-mcp/cmd/openace-daemon@main
```

第二步，在 AI IDE 里配置 shim：

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
    "OPENACE_DAEMON_ADDR": "127.0.0.1:8765",
    "OPENACE_DAEMON_TOKEN": "change-me-local-token"
  }
}
```

如果不设置 `OPENACE_DAEMON_TOKEN`，daemon 和 shim 两边都删掉这个变量即可。默认 daemon 只允许监听 loopback 地址；不要把它直接暴露到公网。

## 本地安装方式

如果不想让 AI IDE 每次 `go run` 在线拉取，可以先安装二进制：

```bash
GOPROXY=https://goproxy.cn,direct go install github.com/AoManoh/openace-mcp/cmd/openace-mcp@main
GOPROXY=https://goproxy.cn,direct go install github.com/AoManoh/openace-mcp/cmd/openace-daemon@main
```

然后把 MCP 配置改成：

```json
"openace-mcp": {
  "command": "openace-mcp",
  "args": [],
  "disabled": false,
  "env": {
    "OPENACE_DAEMON_ADDR": "127.0.0.1:8765",
    "OPENACE_DAEMON_TOKEN": "change-me-local-token"
  }
}
```

如果 AI IDE 找不到命令，把 `command` 改成绝对路径，例如 `$HOME/go/bin/openace-mcp`。

## MCP 工具

| 工具 | 用途 |
|------|------|
| `codebase_retrieval` | 同步扫描 workspace，然后调用 ACE 检索代码 |
| `sync_workspace` | 只同步 workspace，不做检索 |
| `start_codebase_retrieval` | daemon 模式下提交异步检索任务，适合大仓库 |
| `start_sync_workspace` | daemon 模式下提交异步同步任务 |
| `task_status` | 查询任务状态和结果 |
| `list_tasks` | 找回最近任务，列表不返回完整检索正文 |
| `cancel_task` | 取消 queued / running 任务 |

建议：

- 小仓库先用 `codebase_retrieval`。
- 大仓库优先用 `start_codebase_retrieval`，再用 `task_status` 查询。
- 忘记 task id 时用 `list_tasks`。

## 环境变量

| 变量 | 说明 |
|------|------|
| `AUGMENT_TOKEN` | 上游 ACE access token |
| `AUGMENT_TENANT` | 上游 ACE tenant/base URL |
| `AUGMENT_SESSION_AUTH` | 完整 session JSON，优先级最高 |
| `OPENACE_SESSION_FILE` | 显式 session 文件路径 |
| `OPENACE_CACHE_DIR` | workspace checkpoint/cache 目录 |
| `OPENACE_CACHE_NAMESPACE` | cache 命名空间，用于隔离账号、tenant 或测试批次 |
| `OPENACE_DAEMON_ADDR` | MCP shim 连接 daemon 的地址 |
| `OPENACE_DAEMON_LISTEN_ADDR` | daemon 监听地址，默认 `127.0.0.1:8765` |
| `OPENACE_DAEMON_TOKEN` | 可选本地 bearer token |
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

## 安全与边界

- openACE 不绕过 Augment / ACE 的认证、quota、tenant 或 rate limit。
- 检索质量取决于你的上游 ACE 账号和服务状态。
- 默认只索引 UTF-8 文本文件，并跳过常见敏感文件。
- daemon 默认只允许 loopback 监听；远程暴露前必须自行加网络访问控制。

## 压力测试基线

匿名化验证对象：约 5GB 私有大仓库。

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
