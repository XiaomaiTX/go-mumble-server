---
status: active
doc_type: standard
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - go.mod
  - internal/app/
  - internal/mumble/
  - internal/edge/
  - pkg/mumble/
---

# 依赖分层

- `cmd/` 负责可执行入口；`internal/app` 组装运行模式和生命周期。不要在入口或 HTTP 层复制协议处理与 ACL 逻辑。
- `internal/` 可以使用 `pkg/mumble`；反向依赖禁止。`pkg/mumble` 是独立的 framing、wire、audio、crypto 和公共协议类型，不依赖数据库、HTTP 路由或运行模式。
- Core 管理权威会话、频道、权限与交付决策；Edge 管理连接、编解码和数据传输。跨边界消息遵循[协议所有权](../architecture/core-edge-protocol-ownership.md)，避免直接读写另一侧运行时状态。
- Vue 页面通过现有 API 封装访问 REST；服务端中间件和 handler 必须执行访问限制，不能依赖前端隐藏控件。数据库读写走服务端已有存取边界。

新增包或跨层调用时，先对照[模块地图](../architecture/module-map.md)，说明新增依赖的方向、生命周期以及如何测试。历史模式中的示意代码不自动成为新架构约束。
