# Core / Edge 部署说明

同一个 `go-mumble-server` 二进制支持三种运行模式：

- `standalone`：权威 Core、Local Edge、客户端 TCP/TLS 与 UDP、REST 均在同一进程。
- `core`：保留 standalone 的全部能力，并额外监听远端 Edge。
- `edge`：仅运行客户端传输层和 Edge-Core 客户端，不打开数据库，也不创建用户、频道、ACL、封禁或语音路由的权威副本。

## 证书身份

Edge-Core 链路固定使用 TLS 1.3 双向认证。Core 的 `edge_client_ca` 验证 Edge 客户端证书，证书 SAN DNS 名或 CN 必须等于配置的 `edge_id`。Edge 使用 `core_ca_cert` 验证 Core；建议显式设置 `core_server_name`，也可让程序使用 `core_address` 的主机部分。

生产部署不得省略这些证书。配置缺失时网络客户端保持 fail-closed，不会降级为明文，也不会在 Edge 启动本地 Core。

## Core 示例

```toml
[distributed]
mode = "core"
edge_listen = "0.0.0.0:64740"
edge_tls_cert = "/etc/go-mumble-server/core-edge.crt"
edge_tls_key = "/etc/go-mumble-server/core-edge.key"
edge_client_ca = "/etc/go-mumble-server/edge-ca.crt"
heartbeat_interval_seconds = 5
heartbeat_timeout_seconds = 15
queue_size = 256
```

Core 仍在 `[network]` 指定的端口接受普通 Mumble 客户端，并继续提供 REST 管理面。

## Edge 示例

```toml
[distributed]
mode = "edge"
edge_id = "edge-01"
core_address = "core.example.internal:64740"
core_ca_cert = "/etc/go-mumble-server/core-ca.crt"
client_cert = "/etc/go-mumble-server/edge-01.crt"
client_key = "/etc/go-mumble-server/edge-01.key"
core_server_name = "core.example.internal"
heartbeat_interval_seconds = 5
heartbeat_timeout_seconds = 15
reconnect_min_ms = 250
reconnect_max_ms = 5000
queue_size = 256
```

## 故障行为

每次 Core 启动都会生成新的 `CoreEpoch`；每次 Edge 连接都会获得新的实例代数。旧 Core 生命周期、旧 Edge 连接以及旧 `SessionRef` 的迟到消息都会因 epoch、实例代数或会话代数不匹配而失效。

Edge 与 Core 断开后采用 fail-closed：关闭现有客户端、拒绝形成新的认证会话并持续指数退避重连。单个 Edge 的控制和语音写队列有独立上限，队列溢出只关闭或丢弃该 Edge 的相关流量，不会阻塞 Local Edge 或其他远端 Edge。

控制面的初始同步和最终关闭使用 Edge 客户端写队列回执：Core 只有在 Edge 确认消息已刷到客户端连接后才提交 Active，Reject、kick 和 ban 的最终消息也保持先刷新再关闭。
