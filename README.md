# openACE MCP

`openACE` 是 **Open Adapter for Context Engine**。

本仓库计划提供一个稳定的 MCP adapter 和本地索引编排层，首期主线是复用/提取 Augment Code 插件已稳定调用 ACE 的过程，并把它 MCP 化、daemon 化。

这是在 `aug2api` 讨论过程中产生的新独立项目需求，不是 `aug2api` 子模块、拆分项目或代码迁移目标。首期路线涉及复用插件 ACE 调用过程，但仓库可见性不阻塞当前推进，先跑通 MCP 主流程。

当前阶段：最小 Go MCP 主流程、daemon + shim、异步任务 API 和大仓库压力测试已跑通。MCP stdio 可以完成初始化、工具列表、workspace 扫描、blob/checkpoint 同步，并通过 `codebase_retrieval` 返回 ACE 检索结果。

## 边界

- `openACE` 负责本地扫描、blob/checkpoint 缓存、增量同步、daemon 生命周期、MCP 工具、有界并发、取消和诊断。
- 上游 Context Engine 账号负责语义索引、embedding、ranking、格式化检索、quota 和 tenant 授权。
- 用户必须提供自己的上游凭据，例如 `AUGMENT_SESSION_AUTH`、`AUGMENT_TOKEN + AUGMENT_TENANT` 或显式 session 文件。
- 默认实现路径是复用/提取 Augment Code 插件已稳定调用 ACE 的过程，不是重新实现或包装 `auggie --mcp`。
- `auggie --mcp` 只作为对照样本和 fallback adapter 参考。已观察到的 blob/checkpoint/upload 链路需要以插件主流程验证为准。
- 最小业务主流程已经跑通，下一步优化 daemon、缓存、并发上限、取消、超时和发布形态。

## 当前入口

恢复上下文时先读取：

1. `AGENTS.md`
2. `docs/requirements/2026-05-05-project-kickoff.md`
3. `docs/references/2026-05-05-openace-context-engine.md`

文档记录不阻塞编码；当前以真实运行结果推进下一阶段。

## 本地验证

```bash
go test ./...
go test -race ./internal/daemon ./internal/mcp
go vet ./...
go build ./cmd/openace-mcp ./cmd/openace-daemon
```

## MCP 配置示例

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

## Daemon 模式

启动常驻 daemon：

```bash
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

当前 daemon 暴露：

- `GET /healthz`
- `GET /readyz`
- `POST /v1/sync`
- `POST /v1/retrieve`
- `POST /v1/tasks`
- `GET /v1/tasks/{id}`
- `POST /v1/tasks/{id}/cancel`

可选认证方式：

- 默认读取 `~/.augment/session.json`。
- 或设置 `OPENACE_SESSION_FILE` 指向本地 session 文件。
- 或设置 `AUGMENT_TOKEN` 与 `AUGMENT_TENANT`。

可选缓存设置：

- `OPENACE_CACHE_DIR`：指定 workspace checkpoint/cache 目录。建议不同账号、tenant 或验证批次使用独立缓存目录，避免复用旧 checkpoint 影响验证结论。

当前工具：

- `codebase_retrieval`: 扫描并同步 workspace 后，调用 ACE 检索代码。
- `sync_workspace`: 只执行扫描、缺失 blob 上传和 checkpoint。
- `start_codebase_retrieval`: daemon 模式下提交异步代码检索任务。
- `start_sync_workspace`: daemon 模式下提交异步 workspace 同步任务。
- `task_status`: daemon 模式下查询任务状态和结果。
- `cancel_task`: daemon 模式下取消 queued/running 任务。

## 压力测试基线

已用 `/home/oh/projects/mailing` 做真实 ACE 压力验证：

- 仓库体量约 5.0GB，`rg --files` 可见 5937 个文件，openACE 当前文本扫描后参与索引 2667 个文件。
- 冷缓存 5 并发 MCP shim 宽泛检索全部 completed，首个任务完成 workspace 同步并上传 129 个 blob，后续任务复用 checkpoint/cache。
- 暖缓存 daemon 重启后 5 并发全部 completed，总耗时约 30 秒，全部 `uploaded=0 added=0 deleted=0`。
- daemon 压测期间保持存活，RSS 约 270-290MB。
- 当前并发模型是并发提交任务、daemon 串行执行同步/检索；这是稳定性优先的 MVP，不是最终并行吞吐模型。

## 许可证

本项目采用 MIT License。Copyright (c) 2026 aomanoh.
