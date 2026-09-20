# Core / Edge 部署说明

同一个 `go-mumble-server` 二进制支持三种运行模式：

- `standalone`：权威 Core、Local Edge、客户端 TCP/TLS 与 UDP、REST 均在同一进程。
- `core`：保留 standalone 的全部能力，并额外监听远端 Edge。
- `edge`：仅运行客户端传输层和 Edge-Core 客户端，不打开数据库，也不创建用户、频道、ACL、封禁或语音路由的权威副本。

## 证书身份

Edge-Core 链路固定使用 TLS 1.3 双向认证。Core 的 `edge_client_ca` 验证 Edge 客户端证书，证书 SAN DNS 名或 CN 必须等于配置的 `edge_id`。Edge 使用 `core_ca_cert` 验证 Core；建议显式设置 `core_server_name`，也可让程序使用 `core_address` 的主机部分。

生产部署不得省略这些证书。配置缺失时网络客户端保持 fail-closed，不会降级为明文，也不会在 Edge 启动本地 Core。

## 自签证书生成

一张自建 CA 同时充当 Core 的 `edge_client_ca` 与 Edge 的 `core_ca_cert`，由它签发 Core 服务端证书和每台 Edge 的客户端证书。生产环境使用独立的私有 CA（根私钥离线保管、叶子证书有效期建议 ≤397 天），不要复用开发或测试 CA；每台 Edge 单独签发一张证书，退役或泄漏时只影响一台。

以下命令贯穿同一个具体案例（替换为实际值即可）：

| 角色 | 实例名 / edge_id | IP | 域名 |
| --- | --- | --- | --- |
| Core | — | `203.0.113.10`（公网） | `mumble-core.fuxi-legion.com`（已备案） |
| Edge 1 | `edge-shanghai-01` | `10.0.8.21`（内网，仅需出站） | 无需域名 |

注意 Edge 不需要域名，也不需要对外开放任何端口：它只向 Core 发起出站连接，网络身份就是证书 CN/SAN 中的 `edge_id`。需要域名、公网 IP 和开放端口（默认 64740/tcp）的只有 Core。

### 1. 生成 CA

在离线运维机上执行，根私钥不部署到任何服务器：

```bash
openssl ecparam -name prime256v1 -genkey -noout -out fuxi-mumble-ca.key
openssl req -x509 -new -key fuxi-mumble-ca.key -sha256 -days 3650 \
  -subj "/CN=fuxi-mumble-ca" -out fuxi-mumble-ca.crt
```

### 2. Core 服务端证书

SAN 必须覆盖 Edge 侧的 `core_server_name`（未设置时为 `core_address` 的主机部分）。本例 SAN 使用 Core 的已备案域名：

```bash
openssl ecparam -name prime256v1 -genkey -noout -out core-edge.key
openssl req -new -key core-edge.key -subj "/CN=mumble-core.fuxi-legion.com" -out core-edge.csr
openssl x509 -req -in core-edge.csr -CA fuxi-mumble-ca.crt -CAkey fuxi-mumble-ca.key -CAcreateserial \
  -days 397 -sha256 -out core-edge.crt \
  -extfile <(printf 'subjectAltName=DNS:mumble-core.fuxi-legion.com\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature,keyEncipherment\n')
```

跨公网部署时注意：部分运营商 DPI 会拦截携带未备案域名 SNI 的 TLS 握手（表现为 TCP 可建但 ClientHello 即被 RST，与端口和 TLS 版本无关）。生产应像本例一样使用已备案域名作为 SAN 与 `core_server_name`。

若 Core 没有域名，可只签 IP SAN：Go 对 IP 形式的 `core_server_name` 不发送 SNI 扩展（顺带规避上述 SNI 拦截），并按 IP SAN 校验证书：

```bash
openssl ecparam -name prime256v1 -genkey -noout -out core-edge.key
openssl req -new -key core-edge.key -subj "/CN=203.0.113.10" -out core-edge.csr
openssl x509 -req -in core-edge.csr -CA fuxi-mumble-ca.crt -CAkey fuxi-mumble-ca.key \
  -CAcreateserial -days 397 -sha256 -out core-edge.crt \
  -extfile <(printf 'subjectAltName=IP:203.0.113.10\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature,keyEncipherment\n')
```

对应 Edge 侧 `core_address = "203.0.113.10:64740"`、`core_server_name = "203.0.113.10"`（不设置时自动取 `core_address` 的主机部分）。代价是更换服务器 IP 时必须重签证书，域名方案换机器只需改解析。

### 3. Edge 客户端证书（每台 Edge 独立签发）

证书的 CN 或 SAN DNS 名必须等于该 Edge 配置的 `edge_id`（本例 `edge-shanghai-01`），否则 Core 在 Hello 阶段直接关闭连接。SAN 写 edge_id 而非域名：

```bash
openssl ecparam -name prime256v1 -genkey -noout -out edge-shanghai-01.key
openssl req -new -key edge-shanghai-01.key -subj "/CN=edge-shanghai-01" -out edge-shanghai-01.csr
openssl x509 -req -in edge-shanghai-01.csr -CA fuxi-mumble-ca.crt -CAkey fuxi-mumble-ca.key -CAcreateserial \
  -days 397 -sha256 -out edge-shanghai-01.crt \
  -extfile <(printf 'subjectAltName=DNS:edge-shanghai-01\nextendedKeyUsage=clientAuth\nkeyUsage=digitalSignature\n')
```

### 4. 验证与分发

```bash
openssl verify -CAfile fuxi-mumble-ca.crt core-edge.crt edge-shanghai-01.crt
```

- Core（203.0.113.10）：`core-edge.crt` / `core-edge.key` + `fuxi-mumble-ca.crt`（作 `edge_client_ca`）。
- Edge（10.0.8.21，edge-shanghai-01）：`fuxi-mumble-ca.crt`（作 `core_ca_cert`）+ 自己的 `edge-shanghai-01.crt` / `edge-shanghai-01.key`。
- Docker 部署时容器内以 uid 999 运行，root 生成的私钥默认 600 会导致启动报 `permission denied`：挂载目录内 `chown 999:999` 或 `chmod 644`（生产用前者）。
- 轮换 CA 时可先将新旧 CA 证书的 PEM 拼接为过渡信任文件（一个文件多个 PEM 块），两端滚动替换后再切换为仅新 CA，最后重签叶子证书。当前实现不校验 CRL，吊销依赖短有效期与 CA 轮换。

## Core 示例

```toml
[distributed]
mode = "core"
edge_listen = "0.0.0.0:64740"
edge_tls_cert = "/etc/go-mumble-server/core-edge.crt"
edge_tls_key = "/etc/go-mumble-server/core-edge.key"
edge_client_ca = "/etc/go-mumble-server/fuxi-mumble-ca.crt"
heartbeat_interval_seconds = 5
heartbeat_timeout_seconds = 15
queue_size = 256
```

Core 仍在 `[network]` 指定的端口接受普通 Mumble 客户端，并继续提供 REST 管理面。

## Edge 示例

对应上表中的 `edge-shanghai-01`（10.0.8.21）。`core_address` 指向 Core 的域名与 edge 监听端口；无域名场景改为 `203.0.113.10:64740` 并把 `core_server_name` 设为同一 IP：

```toml
[distributed]
mode = "edge"
edge_id = "edge-shanghai-01"
core_address = "mumble-core.fuxi-legion.com:64740"
core_ca_cert = "/etc/go-mumble-server/fuxi-mumble-ca.crt"
client_cert = "/etc/go-mumble-server/edge-shanghai-01.crt"
client_key = "/etc/go-mumble-server/edge-shanghai-01.key"
core_server_name = "mumble-core.fuxi-legion.com"
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
