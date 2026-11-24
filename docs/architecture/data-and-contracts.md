---
status: active
doc_type: architecture
owner: engineering
last_reviewed: 2026-11-24
source_of_truth:
  - internal/database/db.go
  - internal/database/models
  - internal/channel/manager.go
  - internal/server/server.go
  - internal/cluster/types.go
  - internal/cluster/control.go
  - internal/cluster/wire.go
  - internal/mumble/remoteedge.go
  - internal/rest/router.go
  - api/openapi.yaml
  - frontend/src/utils/api.js
---

# 数据与契约

## 范围

说明哪些数据须持久化、哪些只在运行期有效，以及变更时需要同步的生产方与消费方。字段和访问级别见 [REST 路由入口](../api/route-index.md)，客户端编码细节见[协议编码](protocol-encoding.md)。

## 当前实现

### 持久化与在线状态

| 状态 | 归属和约定 |
| --- | --- |
| 管理账号与 Mumble 注册身份 | SQLite `users` 保存管理账号、角色和验签所需信息；`registered_users` 是按虚拟服务器范围存储的本地 Mumble 身份。它们与在线 `SessionID` 不是同一命名空间，外部身份也不以本地账号表为权威。详见[认证与权限](auth-and-permissions.md)。 |
| 服务与策略 | `virtual_servers` 保存管理记录，`meta_config` 保存进程配置，`server_config` 保存 per-server 配置；频道、ACL、组和封禁分别有持久化模型。当前 SQLite 入口通过 GORM `AutoMigrate` 维护这些表。 |
| 频道树 | 实时频道管理器从 SQLite 装载并维护内存树；根频道的协议 ID 是 `0`（持久化行无父节点），不要把零值当成频道缺失。频道链接列是 JSON 数组，链接更新经管理器维护两侧，并向在线客户端广播受影响频道。 |
| 在线状态 | Core 内存保有在线用户、会话绑定和控制队列；接入侧保有 socket、UDP 地址和加密状态。在线会话与其传输状态不通过 SQLite 恢复。 |

REST 改动不会因为写入 SQLite 就自动传播到 Mumble。对实时虚拟服务器 `1`，频道操作使用其在线管理器并触发频道广播；ACL 改动使缓存失效并刷新在线发言抑制和进入权限状态；封禁变更重载缓存。配置更新回调只热更新语音诊断和内容策略，不能推断全部持久化字段都会实时生效。

### 跨边界契约

- **HTTP**：用 [Chi 路由](../../internal/rest/router.go)和 [HTTP 处理器](../../internal/handler/)确认服务端实际行为，[OpenAPI](../../api/openapi.yaml)记录对外契约；[前端请求封装](../../frontend/src/utils/api.js)处理 Bearer、`401`、`204` 和响应解码。改变请求、响应或授权时同步核对契约与调用方，不把数据库模型当 HTTP 响应模板。
- **Mumble 客户端**：TCP 控制帧与客户端语音编码各自维护；控制消息的字段存在性是契约的一部分，例如 `UserState` 必须能区分未发送字段和显式发送 `false`、`0`。语音在 Core 内按规范化帧处理，接收者可带上下文、音量等投递元数据。
- **Core/Edge**：内部链路不是客户端 Mumble wire。当前使用 4 字节大端长度前缀和 JSON 信封，帧体上限 4 MiB；握手返回 Core epoch 和带代次的 Edge 实例引用。`SessionRef` 由 `SessionID` 与 `Generation` 组成，不是跨 Core 进程永久 ID。控制消息在单会话 FIFO 内投递；初始同步写入确认后才提交 `Active`，需要关闭的最后控制消息使用有序写入和 flush 语义。远程语音使用规范化帧和每接收者投递元数据，不交换客户端密钥或 socket。

## 关键入口

- [数据库迁移](../../internal/database/db.go)、[存储模型](../../internal/database/models/)、[频道树](../../internal/channel/manager.go)、[在线适配与回调](../../internal/server/server.go)。
- [UserState 编码](../../pkg/mumble/protocol/messages/user.go#L113)、[规范化语音帧](../../pkg/mumble/audio/frame.go)、[内部 wire](../../internal/cluster/wire.go)、[会话和语音类型](../../internal/cluster/types.go)、[控制投递](../../internal/cluster/control.go)。

## 不变量

- 区分持久用户 ID、在线会话引用和 Edge 实例引用；延迟到达的投递或清理不能仅凭可复用的 `SessionID` 命中新会话。
- 改动持久化状态时，检查对应的内存缓存、广播或重启边界；改动 wire 时同步确认编解码和调用两端。
