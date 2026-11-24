---
status: active
doc_type: guide
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - docs/ai/repo-rules.md
  - cmd/go-mumble-server/main.go
  - internal/server/server.go
  - internal/app/edge.go
  - internal/rest/router.go
  - internal/mumble/handlers.go
  - frontend/vite.config.js
---

# 调试指南

## 先定位失败的边界

记录提交或二进制版本、运行模式、触发步骤、期望与实际结果，保留最小且脱敏的日志。区分启动、HTTP（含管理 UI）、Mumble 客户端和 Core–Edge 链路；它们的成功判据不同。

| 症状 | 第一轮排查 |
| --- | --- |
| 启动即退出、端口未监听 | 在[主程序](../../cmd/go-mumble-server/main.go)对照 `config load failed`、`runtime composition failed` 或 `server exit`；检查模式、实际监听地址、SQLite 路径及证书文件。Standalone/Core 的[装配代码](../../internal/server/server.go)会读取 SQLite 中的持久化设置，不要仅根据 TOML 判断生效值。 |
| 浏览器白屏或管理 API 失败 | 先直接探测 Go 后端的 `/health`，再检查 Vite 代理或嵌入构建，核对网络面板的状态和 Content-Type；用[路由索引](../api/route-index.md)区分路径与 JWT/角色问题。不要把 `GET /api/v1/auth/status` 返回匿名状态当作 JWT 已通过验证。 |
| 客户端能连但没有语音 | 分开检查连接/认证、声道和权限、UDP 与 TCP tunnel。在线用户返回的 `voice_transport`、`crypto_mode` 可辅助定位。Standalone/Core 的活动服务器可在[服务器配置 API](../api/route-index.md)中临时启用 `voice_debug`，在 [Mumble handler](../../internal/mumble/handlers.go)的 `[VOICE-DEBUG]` 日志中定位 UDP 会话识别等步骤；复现后恢复该设置。 |
| Edge 启动后不能服务客户端 | [Edge 进程](../../internal/app/edge.go)没有 REST 或本地权威数据库；不能用其 `/health` 判断 Core 连通性。按 [Core–Edge 部署指南](core-edge-deployment.md)核对 Core 地址、证书身份及链路故障行为。 |
| 外部身份认证或在线重验异常 | 先区分 Mumble 登录与内部重验回调，按[外部身份接入](fuxi-seat-external-identity.md)检查 provider 可达性、配置及独立令牌用途。 |

## 收敛到可复验的结果

以最近的一层复现（见[测试指南](testing-guide.md)），再检查受影响的真实客户端或调用方。规范与运行结果冲突时，按[仓库规则](../ai/repo-rules.md)对照实现、配置和测试，记录观察到的差异；不要用扩大日志或变更权限替代定位。共享日志前移除凭据、Bearer token 和不必要的客户端地址等数据。
