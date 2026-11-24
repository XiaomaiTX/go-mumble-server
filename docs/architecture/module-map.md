---
status: active
doc_type: architecture
owner: engineering
last_reviewed: 2026-11-24
source_of_truth:
  - internal/app/app.go
  - internal/server/server.go
  - internal/mumble/handlers.go
  - internal/edge/clientruntime.go
  - internal/rest/router.go
  - frontend/src/main.js
---

# 模块地图

## 范围

按职责定位代码与依赖边界；不复述[启动顺序](runtime-and-startup.md)或 [REST 路由清单](../api/route-index.md)。

## 当前实现

装配层把客户端接入、Core 业务和 HTTP 适配器组合起来；公开协议库只提供数据类型、编码和传输所需原语。定位改动时，先区分它改变的是客户端协议、服务器决策，还是管理 API 的呈现和调用方式。

## 关键入口

| 模块 | 职责与代码入口 |
| --- | --- |
| 进程与装配 | [main.go](../../cmd/go-mumble-server/main.go) 处理参数、信号和生命周期；[app](../../internal/app/app.go) 选择模式；[server](../../internal/server/server.go) 装配进程内 Core、Local Edge 和 HTTP 管理平面。 |
| 对外协议库 | [控制帧](../../pkg/mumble/protocol/framing.go)、[语音包与规范化帧](../../pkg/mumble/audio/frame.go)、[UDP 加密](../../pkg/mumble/crypto/cryptstate.go)和[权限位](../../pkg/mumble/permission.go)是与服务端存储无关的可复用构件。 |
| 接入与传输 | [ClientRuntime](../../internal/edge/clientruntime.go)管理共享 TCP/TLS 和 UDP 接入；[LocalEdge](../../internal/mumble/localedge.go)接到进程内 Core；远程模式用 [EdgeRuntime](../../internal/edge/edgeruntime.go) 和 [RemoteCoreClient](../../internal/edge/remotecore.go)。底层连接与监听在 [connection](../../internal/connection/) 和 [transport](../../internal/transport/)。 |
| Core 业务 | [消息处理器](../../internal/mumble/handlers.go)执行业务检查和状态变更；[频道](../../internal/channel/manager.go)、[在线用户](../../internal/user/)、[ACL](../../internal/acl/evaluator.go)、[封禁](../../internal/ban/)管理各自状态；[语音路由](../../internal/audio/)按规则计算接收者。 |
| 会话与跨进程投递 | [Registry](../../internal/cluster/registry.go)绑定会话与 Edge；[ControlTransport](../../internal/cluster/control.go)处理每会话控制队列；[Dispatcher](../../internal/cluster/dispatcher.go)按 Edge 分组语音；[RemoteEdgeServer](../../internal/cluster/remoteedgeserver.go)接入远程 Edge。 |
| 身份与管理 | [identity](../../internal/identity/authority.go)定义 Mumble 身份提供方边界；[REST](../../internal/rest/router.go)装配中间件、处理器及在线状态适配器；[handler](../../internal/handler/)处理 HTTP 请求，其中管理账号业务使用 [UserService](../../internal/service/user.go)，并非所有处理器都经由 service 包。 |
| 配置与存储 | [启动配置](../../internal/config/config.go)读取 TOML 与环境变量；[持久化配置](../../internal/config/dbconfig.go)提供加载与合成；[数据库](../../internal/database/db.go)打开 SQLite 并迁移[模型](../../internal/database/models/)。 |
| Web 与 API 契约 | [前端入口](../../frontend/src/main.js)初始化 Vue 应用；[请求封装](../../frontend/src/utils/api.js)调用管理 API；[OpenAPI](../../api/openapi.yaml)描述对外 HTTP 契约。 |

## 不变量

- `cmd` 负责进程边界，`app` 负责按模式装配；业务处理器不另行选择运行模式。
- `pkg/mumble` 不反向依赖 `internal`。客户端 socket 和会话业务以接入路由、`Peer` 和类型化投递对象为边界；不要让 Core 控制处理器用物理连接作为业务身份。
- HTTP 处理器和 Mumble 消息处理器不是同一个入口；维护共享状态时须检查[持久化及实时同步边界](data-and-contracts.md)，不能假设写库即已通知在线客户端。
