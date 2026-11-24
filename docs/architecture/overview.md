---
status: active
doc_type: architecture
owner: engineering
last_reviewed: 2026-11-24
source_of_truth:
  - go.mod
  - cmd/go-mumble-server/main.go
  - internal/app/app.go
  - internal/server/server.go
  - internal/edge/ingress.go
  - internal/mumble/remoteedge.go
  - frontend/package.json
---

# 架构总览

## 范围

介绍 go-mumble-server 当前的运行边界和主要数据流。包职责见[模块地图](module-map.md)，启动顺序见[运行与启动](runtime-and-startup.md)。旧版[技术总览](../reference/technical-overview.md)用于查阅历史背景，不作为当前行为的依据。

## 当前实现

go-mumble-server 是用 Go 实现的 Mumble 语音服务器，带 REST 管理 API 和可随二进制发布的 Vue 管理界面。Mumble 客户端通过 TCP/TLS 发送控制消息，通过 UDP 或 TCP `UDPTunnel` 收发语音；浏览器使用 HTTP API，不直接参与 Mumble 客户端协议。服务器通过 GORM 使用 SQLite 持久化配置和管理数据。

- **客户端接入（Edge）**：负责客户端连接、TLS、UDP 端点与加密状态、编解码以及最终发送。传入控制消息由共享的 `ClientIngressRouter` 按协议所有权分流；语音在接入侧转换为规范化帧。
- **权威状态（Core）**：决定 Mumble 登录、会话和频道状态、ACL、封禁及语音接收者。控制消息按会话投递；语音按接收者所在 Edge 分批，由接入侧编码和发送。
- **管理入口（REST）**：查询或修改持久化及在线状态，由 `server.Server` 接线到实时 Core；路由见 [API 入口](../api/route-index.md)，Web 角色与 Mumble ACL 的区别见[认证与权限](auth-and-permissions.md)。

默认部署 `standalone` 将 Core 和本地 Edge 放在同一进程。`core` 保留本地接入和管理平面，并可启用远程 Edge 接入；`edge` 只运行客户端接入及远程 Core 客户端。当前代码已有 Core/Edge 网络实现，不只是进程内拆分；[协议所有权](core-edge-protocol-ownership.md)记录两侧的处理边界。

## 关键入口

- [进程入口](../../cmd/go-mumble-server/main.go) → [模式装配](../../internal/app/app.go) → [本地运行图](../../internal/server/server.go)或[远程 Edge 运行图](../../internal/app/edge.go)。
- [客户端消息分流](../../internal/edge/ingress.go)、[远程会话入口](../../internal/mumble/remoteedge.go)和 [REST 路由](../../internal/rest/router.go)。

## 不变量

- Core 是会话与业务授权的权威；Edge 不拥有频道或 ACL，不会在 Core 不可用时自行认证。
- 公开协议库 `pkg/mumble` 不依赖服务器业务实现；[协议编码](protocol-encoding.md)和[数据与契约](data-and-contracts.md)分别说明 wire 编码与跨边界的数据约束。
- 当前实时 Mumble 服务固定使用虚拟服务器 ID `1`；数据库和 REST 中存在其它虚拟服务器记录，不代表进程会为其启动 Mumble 实例。
