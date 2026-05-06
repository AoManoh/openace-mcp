# openACE MCP

`openACE` 是 **Open Adapter for Context Engine**。

本仓库计划提供一个稳定的 MCP adapter 和本地索引编排层，首期主线是复用/提取 Augment Code 插件已稳定调用 ACE 的过程，并把它 MCP 化、daemon 化。

这是在 `aug2api` 讨论过程中产生的新独立项目需求，不是 `aug2api` 子模块、拆分项目或代码迁移目标。首期路线涉及复用插件 ACE 调用过程，但仓库可见性不阻塞当前推进，先跑通 MCP 主流程。

当前阶段：最小 Go MCP 主流程、daemon + shim、异步任务 API、任务诊断和大仓库压力测试已跑通。MCP stdio 可以完成初始化、工具列表、workspace 扫描、blob/checkpoint 同步，并通过 `codebase_retrieval` 返回 ACE 检索结果。大仓库建议默认使用 daemon 异步工具，避免 MCP 客户端同步等待超时。

## 边界

- `openACE` 负责本地扫描、blob/checkpoint 缓存、增量同步、daemon 生命周期、MCP 工具、有界并发、取消和诊断。
- 上游 Context Engine 账号负责语义索引、embedding、ranking、格式化检索、quota 和 tenant 授权。
- 用户必须提供自己的上游凭据，例如 `AUGMENT_SESSION_AUTH`、`AUGMENT_TOKEN + AUGMENT_TENANT` 或显式 session 文件。
- 默认实现路径是复用/提取 Augment Code 插件已稳定调用 ACE 的过程，不是重新实现或包装 `auggie --mcp`。
- `auggie --mcp` 只作为对照样本和 fallback adapter 参考。已观察到的 blob/checkpoint/upload 链路需要以插件主流程验证为准。
- 当前 MCP 主流程已经跑通并完成大仓库并发压力验收，后续优化不阻塞当前使用。

## 当前入口

恢复上下文时先读取：

1. `AGENTS.md`
2. `docs/requirements/2026-05-05-project-kickoff.md`
3. `docs/references/2026-05-05-openace-context-engine.md`

文档记录不阻塞编码；当前以真实运行结果推进下一阶段。

## 安装

本地开发验证：

```bash
go test ./...
go test -race ./internal/daemon ./internal/mcp
go vet ./...
go build ./cmd/openace-mcp ./cmd/openace-daemon
```

在线安装可使用 Go module；网络较慢时加 GOPROXY 镜像：

```bash
GOPROXY=https://goproxy.cn,direct go install github.com/AoManoh/openace-mcp/cmd/openace-mcp@latest
GOPROXY=https://goproxy.cn,direct go install github.com/AoManoh/openace-mcp/cmd/openace-daemon@latest
```

## MCP 配置示例

直接 stdio 模式适合小仓库或 smoke test：

```json
{
  "mcpServers": {
    "openace": {
      "command": "go",
      "args": ["run", "/home/oh/projects/openace-mcp/cmd/openace-mcp"]
    }
  }
}
```

## AI IDE 快速测试

推荐用 daemon + MCP shim 测试，尤其是大仓库。`.env` 不会被程序自动加载；如果使用 `.env`，需要先在启动 daemon 的 shell 中执行 `set -a && source .env && set +a`。

1. 安装二进制：

```bash
GOPROXY=https://goproxy.cn,direct go install github.com/AoManoh/openace-mcp/cmd/openace-mcp@latest
GOPROXY=https://goproxy.cn,direct go install github.com/AoManoh/openace-mcp/cmd/openace-daemon@latest
```

2. 启动 daemon：

```bash
export AUGMENT_TOKEN="..."
export AUGMENT_TENANT="..."
export OPENACE_CACHE_DIR="$HOME/.cache/openace-mcp"
export OPENACE_CACHE_NAMESPACE="default"
export OPENACE_DAEMON_LISTEN_ADDR=127.0.0.1:8765
export OPENACE_DAEMON_TOKEN="$(openssl rand -hex 16)"
openace-daemon
```

如果不用 `OPENACE_DAEMON_TOKEN`，可以省略该变量；如果启用 token，AI IDE 的 MCP 配置里也必须设置同一个值。

3. 在 AI IDE 中配置 MCP shim：

```json
{
  "mcpServers": {
    "openace": {
      "command": "openace-mcp",
      "env": {
        "OPENACE_DAEMON_ADDR": "127.0.0.1:8765",
        "OPENACE_DAEMON_TOKEN": "填入 daemon 启动时的同一个 token"
      }
    }
  }
}
```

如果 `go install` 后 AI IDE 找不到 `openace-mcp`，把 `command` 改成绝对路径，例如 `$HOME/go/bin/openace-mcp`。

4. 测试建议：

- 小仓库可直接调用 `codebase_retrieval`。
- 大仓库优先调用 `start_codebase_retrieval`，再用 `task_status` 查询结果。
- 忘记 task id 时调用 `list_tasks` 找回最近任务。

## Daemon 模式

启动常驻 daemon。上游 ACE 凭据必须给 `openace-daemon` 进程，而不是只给 MCP shim：

```bash
export AUGMENT_TOKEN="..."
export AUGMENT_TENANT="..."
export OPENACE_DAEMON_LISTEN_ADDR=127.0.0.1:8765
go run ./cmd/openace-daemon
```

让 MCP shim 代理到 daemon：

```json
{
  "mcpServers": {
    "openace": {
      "command": "go",
      "args": ["run", "/home/oh/projects/openace-mcp/cmd/openace-mcp"],
      "env": {
        "OPENACE_DAEMON_ADDR": "127.0.0.1:8765"
      }
    }
  }
}
```

如果需要给本机 daemon 加一层本地 bearer token，daemon 和 shim 同时设置同一个 `OPENACE_DAEMON_TOKEN`。

默认只允许 daemon 监听 loopback 地址，例如 `127.0.0.1:8765` 或 `localhost:8765`。如需绑定 `0.0.0.0`，必须显式设置 `OPENACE_ALLOW_REMOTE_DAEMON=1`，并自行配置网络访问控制；这不是默认推荐形态。

当前 daemon 暴露：

- `GET /healthz`
- `GET /readyz`
- `POST /v1/sync`
- `POST /v1/retrieve`
- `POST /v1/tasks`
- `GET /v1/tasks`
- `GET /v1/tasks/{id}`
- `POST /v1/tasks/{id}/cancel`

可选认证方式：

- 默认读取 `~/.augment/session.json`。
- 或设置 `OPENACE_SESSION_FILE` 指向本地 session 文件。
- 或设置 `AUGMENT_TOKEN` 与 `AUGMENT_TENANT`。

可选缓存设置：

- `OPENACE_CACHE_DIR`：指定 workspace checkpoint/cache 目录。建议不同账号、tenant 或验证批次使用独立缓存目录，避免复用旧 checkpoint 影响验证结论。
- `OPENACE_CACHE_NAMESPACE`：在同一个 cache 目录下按账号、tenant 或压测批次隔离 workspace state，默认 `default`。

大仓库调参：

- `OPENACE_UPLOAD_BATCH_BYTES`：单次 batch-upload 估算上限，默认 `1048576`。
- `OPENACE_FIND_MISSING_BATCH_SIZE`：单次 find-missing blob 数量，默认 `1000`。
- `OPENACE_MAX_FILE_BYTES`：单文件索引上限，默认 `1048576`。

扫描安全：

- 默认跳过 `.gitignore` / `.ignore` 命中的文件和目录。
- 默认跳过 `.env*`、`.npmrc`、`.netrc`、`session.json`、`credentials.json`、私钥和证书类文件。
- 只索引非空、未超过上限、非二进制且 UTF-8 合法的文本文件。

当前工具：

- `codebase_retrieval`: 扫描并同步 workspace 后，调用 ACE 检索代码。
- `sync_workspace`: 只执行扫描、缺失 blob 上传和 checkpoint。
- `start_codebase_retrieval`: daemon 模式下提交异步代码检索任务。
- `start_sync_workspace`: daemon 模式下提交异步 workspace 同步任务。
- `task_status`: daemon 模式下查询任务状态和结果。
- `list_tasks`: daemon 模式下列出最近任务，列表不返回完整检索文本，避免诊断接口放大内存和输出。
- `cancel_task`: daemon 模式下取消 queued/running 任务。

## 压力测试基线

已用 `/home/oh/projects/mailing` 做真实 ACE 压力验证：

- 仓库体量约 5.0GB，`rg --files` 可见 5937 个文件，openACE 当前过滤后参与索引 2557 个文本文件。
- 冷缓存 5 并发 MCP shim 宽泛检索全部 completed，耗时 55.1 秒，daemon RSS 高峰约 20.9MB。
- 暖缓存 daemon 重启后 5 并发全部 completed，耗时 30.0 秒，全部 `uploaded=0 added=0 deleted=0`，daemon RSS 高峰约 20.8MB。
- daemon 压测期间保持存活；扫描与上传支持取消，任务列表可通过 `list_tasks` 找回。
- 当前并发模型是并发提交任务、daemon 串行执行同步/检索；这是稳定性优先的 MVP，不是最终并行吞吐模型。

## 许可证

本项目采用 MIT License。Copyright (c) 2026 aomanoh.
