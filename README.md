# openACE MCP

`openACE` 是 **Open Adapter for Context Engine**。

本仓库计划提供一个稳定的 MCP adapter 和本地索引编排层，用于调用用户自带账号对应的 codebase Context Engine 能力。它不是独立语义检索后端，不绕过上游认证、quota、tenant 限制或服务可用性边界。

这是在 `aug2api` 讨论过程中产生的新 public 项目需求，不是 `aug2api` 子模块、拆分项目或代码迁移目标。

当前阶段：仅项目启动治理。项目已有 `AGENTS.md`、需求文档、参考资料和工作日志，尚无业务实现。

## 边界

- `openACE` 负责本地扫描、blob/checkpoint 缓存、增量同步、daemon 生命周期、MCP 工具、有界并发、取消和诊断。
- 上游 Context Engine 账号负责语义索引、embedding、ranking、格式化检索、quota 和 tenant 授权。
- 用户必须提供自己的上游凭据，例如 `AUGMENT_SESSION_AUTH`、`AUGMENT_TOKEN + AUGMENT_TENANT` 或显式 session 文件。
- 默认实现路径是以 Augment Code 插件 codebase retrieval 可观察行为为核心参考的 clean adapter，不是重新实现或包装 `auggie --mcp`。
- `auggie --mcp` 只作为官方公开接入路径、认证/session 行为和 fallback adapter 的参考。已观察到的 blob/checkpoint/upload 链路只是技术依据，不代表稳定官方 API。
- MVP 默认不复用插件运行时。VSCode 插件桥接最多作为后续 experimental adapter 讨论。
- 项目不得 vendor、复制或分发 proprietary IDE 插件代码。

## 治理

实现前必须先读取：

1. `AGENTS.md`
2. `docs/requirements/2026-05-05-project-kickoff.md`
3. `docs/references/2026-05-05-openace-context-engine.md`

在用户审核并确认 `AGENTS.md`、项目启动需求文档和被引用的 Context Engine 调研文档可作为当前项目事实之前，不得开始编码。

## 许可证

本项目采用 MIT License。Copyright (c) 2026 aomanoh.
