# Core/Edge 协议所有权

## 文档状态

- 状态：第一阶段约束草案
- 适用方案：`docs/spec/draft/0014-mumble-core-edge-plan.md`
- 目的：明确客户端 Mumble 协议消息由 Core、Edge 或双方中的哪一方解释、决策和发送，防止传输状态重新渗入 Core

## 一、所有权定义

### Core-owned

Core 解释消息的业务语义并产生 authoritative 状态变更。Edge 只按 session FIFO 转发入站消息或发送 Core 生成的出站消息，不自行作权限或全局状态决策。

主要包括：

- `Authenticate` 的身份、密码、ban、容量、用户名唯一性和 Session ID 决策
- `ServerSync` 及初始 Channel/User/ACL/Codec 状态的内容与顺序
- `UserState`、`UserRemove`、`ChannelState`、`ChannelRemove`
- `TextMessage`、`VoiceTarget`
- `BanList`、`ACL`、`PermissionQuery`
- `RequestBlob`、`UserStats`、`QueryUsers`、`UserList`
- `ContextActionModify`、`ContextAction`、`PluginDataTransmission`
- `Reject`、kick/ban 的原因和关闭决策

### Edge-owned

Edge 解释并终止与具体客户端 socket、加密状态或 wire encoding 有关的协议，不把原始传输状态交给 Core。

包括：

- TLS handshake、客户端证书提取和远端地址采集
- `CryptSetup` 的密钥下发、nonce 更新与 resync
- UDP 加密、解密、计数器和重放/迟到统计
- UDP ping 的解析与回复
- UDP endpoint 识别、NAT binding 和 rebinding
- 客户端 audio wire mode 协商、入站解码和出站编码
- UDP/TCP `UDPTunnel` fallback 的最终选择

### Split-owned

双方各自拥有明确的一部分语义，通过类型化事件或命令交换结果；不得共享 `CryptState`、socket 或 UDP 地址。

| 消息/流程 | Edge 所有权 | Core 所有权 |
| --- | --- | --- |
| `Version` | 解析客户端 wire 能力，选择 audio wire mode | 保存业务需要的客户端版本/能力元数据 |
| `Authenticate` | 收集 TLS/证书/远端 IP，按 FIFO 转发认证请求 | 完成认证、分配 session、决定初始状态或 Reject |
| TCP `Ping` | 终止请求并回复；填充本地 UDP crypto 统计 | 接收客户端上报的 TCP ping 等业务指标并更新 User 快照 |
| `UDPTunnel` 入站 | 按客户端 wire mode 解码成 canonical frame | 校验发送资格并计算接收者 |
| 语音下行 | 编码、加密、UDP/TCP 选择和本地 fanout | 产生 canonical frame 与逐接收者 delivery metadata，并按 Edge 分组 |
| 初始同步 | 保证同一 session 的发送 FIFO并报告发送失败 | 生成完整、有序的同步命令序列 |
| Reject/kick/ban | 按序发送最后消息，flush 后关闭 socket | 决定消息内容、目标和“发送后关闭”动作 |

## 二、Ping 处理模型

### UDP ping

UDP ping 完全在 Edge 终止：

1. Edge 使用本地 `CryptState` 解密并识别 session。
2. Edge 按该客户端的 audio wire mode 解析 ping。
3. Edge 在本地构造回复、加密并发回当前 UDP endpoint。
4. NAT rebinding、mixed-crypto 下强制 TCP 等本地策略由 Edge 执行。
5. Core 不接收每个 UDP ping，不保存 nonce、计数器或 endpoint。

### TCP `Ping`

TCP `Ping` 是 split-owned，但由 Edge 在客户端协议层终止，避免 Core 为生成回复读取 `CryptState`：

1. Edge 按 per-session FIFO 读取 `Ping`。
2. Edge 保留或生成 timestamp，并读取本地 `Good/Late/Lost/Resync` 快照构造响应。
3. Edge 将响应排入同一 session 的控制发送队列。
4. 若请求携带 `TCPPingAvg` 等 Core 业务需要的字段，Edge 发送类型化 `SessionMetricsUpdated` 事件；该事件不携带 `CryptState`。
5. Core 更新 `mumble.User.Ping` 等展示/管理状态，但不参与单次 ping 往返。

第一阶段 Local Edge 可以复用现有实现，但必须把读取 `c.Crypt` 的部分封装在 edge-local handler 中。Fake Edge 测试必须证明 Core Peer 无需 crypto 方法也能处理指标更新。

## 三、控制传输顺序与关闭语义

### Per-session FIFO

Control transport 必须对同一 `SessionRef` 保证 FIFO：

- Core 提交的控制命令按提交顺序到达该客户端。
- 不同 session 之间不要求全局顺序，避免慢客户端阻塞其他用户。
- initial sync 是同一 FIFO 上的连续有序序列；认证完成前不得插入该 session 的普通广播。
- transport 重连或重试不得把旧 generation 的命令送给新 session。
- transport API 返回成功只表示命令被可靠接管；需要关闭的命令还必须等待远端 Edge 的写入/flush 结果或超时。

### `CloseAfterFlush`

`CloseAfterFlush(session, deadline)` 的语义是：原子地封闭该 session 的发送队列，不再接受新消息；等待调用前已经接管的消息完成客户端写入；随后关闭 socket。超时后允许强制关闭，并返回可观测错误。

### `SendThenClose`

`SendThenClose(session, message, deadline)` 是 Reject、kick、ban 等终止流程的唯一组合操作：

1. 在 session FIFO 尾部原子追加最后一条消息并封闭队列。
2. 禁止任何后续消息插入最后消息之后。
3. 等待最后消息写入客户端；Remote Edge 必须回报完成或失败。
4. 完成或超时后关闭 socket并产生一次断线事件。

不得用独立的 `Send` 加异步 `Close` 模拟该语义，否则网络传输和调度竞争会导致 Reject/UserRemove 丢失或乱序。

## 四、演进标识

第一阶段的 `SessionRef` 使用 `SessionID + SessionGeneration`。下一阶段 wire identity 必须预留 `CoreEpoch`，用于 Core 重启或主从切换后拒绝旧 Core 产生的迟到命令。

Edge 注册身份下一阶段扩展为：

```text
EdgeRef = EdgeID + EdgeInstanceGeneration
```

同名 Edge 每次进程启动或成功重新注册都获得新的 instance generation。Registry 的 Edge 注册、session bind、投递、断开清理和 ACK 都必须校验该 generation，避免旧连接清理或确认新实例的数据。

本阶段不实现 Core epoch 分配和远端 Edge generation 协议，但类型、接口和测试 fixture 不得假设 `EdgeID` 或 `SessionID` 单独即可永久唯一。
