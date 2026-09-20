# go-mumble-server Core/Edge 第一阶段实施方案

## 文档状态

- 状态：可执行
- 评估基线：提交 `7ced3af` 与当前工作区
- 评估日期：2026-09-19
- 本阶段目标：在不改变单机行为的前提下，把 authoritative Core 与本地客户端传输解耦，并用 Fake Edge 证明同一 Core 能同时管理本地、远端逻辑会话
- 本阶段不包含：真实 Edge 进程、Edge-Core 网络监听器和线上协议兼容承诺
- 协议所有权约束：`docs/architecture/core-edge-protocol-ownership.md`

## 一、结论

当前代码适合渐进式改造，不需要重写服务端，但原方案中的抽象粒度和语音批次模型需要调整后才能执行。

已经具备的 Core 能力包括：

- `user.Manager` 管理全局用户、会话 ID 和 `SessionGeneration`。
- `channel.Manager`、`acl.Evaluator`、`ban.Manager` 和 identity authority 已集中在 `internal/mumble.Server`。
- 普通语音已经在 `internal/audio.Router` 中完成频道、链接频道、Listener、Speak 权限、mute/deaf 等接收者决策。
- `mumble.User.CryptoMode` 已存在，认证同步时也会写入；只是频道聚合逻辑仍错误地回读本地 `Conn.Crypt`。
- `audio.EncodingCache` 已能按 wire mode 和交付上下文复用编码结果，可继续用于 Local Edge。

当前真正的边界问题是：

1. `internal/mumble.Server.conns` 固定为 `map[uint32]*connection.Conn`，控制消息、连接生命周期、UDP、语音目标和管理操作都直接查本地连接。
2. `protocol.MessageHandler` 的上下文是 `interface{}`，各 handler 再断言为 `*connection.Conn`，不存在可检查的逻辑 Peer 契约。
3. 普通语音由 Router 逐接收者调用 `SendAudio`；VoiceTarget 则绕过 Router，在 `voicetarget.go` 内自己解析并直接发送。只改 Router 会漏掉 whisper 路径。
4. VoiceTarget 用连接指针固定 owner 和显式目标，以避免 session ID 复用把旧目标转给新连接。改成逻辑会话后必须使用不可复用的会话身份（`SessionID + SessionGeneration`），不能只保存 session ID。
5. UDP 入站识别、`CryptState`、UDP 地址、wire mode 和 UDP/TCP fallback 都是本地传输状态，目前分散在 `internal/mumble` 与 `internal/connection`，第一阶段不宜一次搬迁。

因此本阶段采用“先建立窄接口和注册表，再统一语音决策/交付，最后验证 Fake Edge”的顺序；不先设计网络协议，也不把 `connection.Conn` 大拆。

## 二、目标架构与明确边界

部署模型保持为：

```text
standalone（默认）:
Client → Core + Local Edge

core（本阶段只完成内部能力）:
Local Client → Core + Local Edge
Fake Remote Edge ─────────→ Core

未来 edge:
Client → Edge → Core
```

Core 决定：

- 身份认证、封禁、容量限制和 Session ID 分配
- User/Channel/ACL/VoiceTarget/Listener 状态
- 谁可以发送、谁应当接收、每个接收者的语音上下文和音量
- Session 属于哪个 Edge
- 控制消息的全局广播语义

Edge 决定：

- TCP/TLS/UDP socket 生命周期
- 客户端证书和远端地址的采集
- `CryptState`、UDP endpoint 和 NAT rebinding
- 客户端 audio wire mode
- 按客户端能力编码、加密，并选择 UDP 或 TCP tunnel
- 本 Edge 内的实际 fanout

本阶段的 Local Edge 是现有本地发送路径的适配器，不搬动 TCP accept loop 和 UDP read loop。等 Fake Edge 验证边界稳定后，再单独迁移物理代码位置。

## 三、对原方案的关键修正

### 1. 不采用 `Packet []byte + Recipients []uint32` 的批次

当前 `mumbleaudio.Delivery` 包含 `Context`、`VolumeAdjustment`、位置音频标记和原始 `Frame`；最终编码还依赖接收客户端的 `AudioWireMode`。因此同一个 Edge 上的多个接收者不一定能共享同一段已编码 packet。

本阶段使用 canonical delivery batch：

```go
type EdgeID string

const LocalEdgeID EdgeID = "local"

type SessionRef struct {
    SessionID  uint32
    Generation uint64
}

type VoiceRecipient struct {
    Session  SessionRef
    Context  audio.Context
    Volume   float32
    Position bool
}

type VoiceBatch struct {
    Sender     SessionRef
    Frame      audio.Frame
    Recipients []VoiceRecipient
}
```

字段名可在实现时微调，但语义必须保留。Core 每个 Edge 只调用一次 `DeliverVoice(edgeID, batch)`；Local Edge 再利用 `EncodingCache` 按 wire mode/上下文编码并发送。未来 Remote Edge 也据此在边缘编码和加密。

这保证“同一 Edge 一次下行调用”，但不错误承诺“同一 Edge 永远只有一份客户端明文 packet”。

### 2. Peer 只是 transitional adapter

`Peer` 只是把现有 handler 从 `*connection.Conn` 解开的过渡适配器，不是长期 Core/Edge wire contract，也不是新的万能 session 对象。它只允许容纳当前控制 handler 迁移所必需的逻辑会话元数据和控制发送操作。

严禁继续向 `Peer` 添加 `CryptState`、UDP endpoint、UDP 加解密、NAT binding、`AudioWireMode`、音频编解码或底层 `net.Conn` 方法。若某个 handler 需要这些能力，应将该 handler 判定为 edge-local 或拆成类型化 Core/Edge 事件，而不是扩大 `Peer`。

`connection.Conn` 先直接实现 `Peer`，保证行为不变；Remote Edge 不要求伪造一个拥有本地传输能力的 Peer。真实网络阶段应由 `ControlTransport` 和类型化 session metadata 逐步替代 Peer，最终删除该适配器。

`protocol.MessageHandler` 本阶段改为显式接收该接口，或在 `internal/mumble` 建立 typed handler adapter；不得继续让新增代码扩散 `interface{}` 和类型断言。

### 3. Session Location 必须带 generation

仅维护 `SessionID → EdgeID` 不能抵御会话 ID 复用。注册表至少保存：

```text
SessionRef(SessionID, Generation) → EdgeID
```

绑定、解绑和投递都校验 generation。`user.Manager` 已维护 `SessionGeneration`，直接复用，不再引入另一套 epoch。断线清理必须只删除与旧 generation 匹配的绑定，避免迟到的旧 Edge 事件删除新会话。

下一阶段的跨进程 `SessionRef` 预留 `CoreEpoch`，Edge 身份预留 `EdgeInstanceGeneration`。本阶段不生成这两个值，但 registry API、测试 fixture 和序列化设计不得假定 `SessionID`、`EdgeID` 在进程重启后仍全局唯一；详细约束见 Protocol Ownership 文档。

### 4. `CryptoMode` 直接以 User 状态为准

`sendSync` 已把协商结果写入 `mumble.User.CryptoMode`。第一阶段只需让 `UpdateChannelCrypto` 从 `user.Manager` 快照聚合该字段，不再读取 `s.conns[sid].Crypt.Mode()`。协商算法仍留在 Local Edge 路径，未来 Remote Edge 通过受信任事件提交协商结果。

### 5. 建立 Protocol Ownership 清单

`docs/architecture/core-edge-protocol-ownership.md` 是实施时的边界检查表，将 Mumble 消息和流程分为 Core-owned、Edge-owned 和 split-owned。以下能力必须明确为 edge-local：

- `CryptSetup`，包括 key/nonce 下发和 nonce resync
- UDP crypto、计数器和重放状态
- UDP ping 的解析与回复
- 客户端 audio wire decoding/encoding
- UDP endpoint、NAT binding/rebinding

TCP `Ping` 采用 split-owned 模型：Edge 直接回复 timestamp 和本地 crypto 统计；只把 `TCPPingAvg` 等业务指标作为类型化事件上报 Core。Core 不再为了回复 Ping 读取 `CryptState`。

### 6. Session lifecycle 与 initial sync 是同一个提交边界

第一阶段必须引入显式生命周期：

```text
Authenticating → Syncing → Active → Closing → Closed
```

最小实现不新增 `ClientConnRef`。状态归属按认证边界划分：

- 尚无 `SessionRef` 时，Local Edge 的临时 Peer 保持 `Authenticating`；认证判定仍由 Core 完成。该对象不在 session registry、用户集合、Broadcast 或 Voice 路径中。
- `users.Add` 分配 `SessionID + SessionGeneration` 后，Core 在 session registry 创建 `Syncing` entry，并绑定 `LocalEdgeID`。从此以后 registry entry 是生命周期事实来源，`connection.Conn.State()` 不再作为 Core 业务可见性的依据。
- `Syncing → Active`、`Active/Syncing → Closing` 和 `Closing → Closed` 只能由 Core session coordinator 执行。Edge 只能报告写入完成、失败或断线，不能自行把逻辑 session 标成 Active。

各状态语义：

| 状态 | 用户/绑定 | 控制面 | 语音面 |
| --- | --- | --- | --- |
| Authenticating | 未分配 `SessionRef`，不在 `user.Manager` | 只允许 Version/Authenticate 和 edge-local 握手 | 不参与 |
| Syncing | User、SessionRef、Edge binding 已存在 | initial lane 独占；普通 Broadcast 只进入 deferred queue；普通业务 handler 拒绝 | 不作为发送者或接收者 |
| Active | 完整存在并已对外宣布 | 正常 FIFO 控制消息和业务 handler | 可作为发送者与接收者 |
| Closing | entry 已封闭，开始 generation-aware cleanup | 只允许既有 flush 或 final message | 不再发送或接收 |
| Closed | User、binding、临时状态均已清理；entry 删除或保留短期 tombstone | 拒绝迟到事件 | 拒绝迟到投递 |

`user.Manager` 继续负责身份唯一性、容量、Session ID 分配和用户数据，因此 Syncing 用户会暂时存在于其内部；但“正常在线/可路由用户集合”必须由 `user.Manager` 快照与 session registry 的 `Active` 状态相交得到。不得继续把 `users.Exists`、`SnapshotAll` 或 `connection.StateActive` 单独当作在线资格。

## 四、initial sync barrier 与认证提交时序

### 1. 为什么不能直接跳过 Syncing 期间的 Broadcast

当前 Channel、User、Listener 等状态由多个 Manager 分别提供快照，没有一个覆盖整个 initial sync 的全局一致性事务。如果仅跳过 Syncing session 的普通 Broadcast，某项状态在其 snapshot 已发送后发生变化，该客户端将永久遗漏变化。

因此本阶段选择 session-local deferred queue，不引入分布式事务或全局状态锁。

### 2. Barrier 接口与队列语义

ControlTransport 增加概念接口：

```text
BeginSync(SessionRef)
SendInitial(SessionRef, ControlMessage)
Send(SessionRef, ControlMessage)
CommitSync(SessionRef, deadline)
AbortSync(SessionRef, cause)
```

语义如下：

1. `BeginSync` 建立该 session 的 initial lane 和有界 deferred queue。生命周期必须已经是 `Syncing`。
2. `SendInitial` 只追加 initial snapshot 消息；只有认证协程可以调用。
3. 普通 `Broadcast` 查询 registry：Active session 调用 `Send`；Syncing session 不立即发送，而是按产生顺序追加 deferred queue；Authenticating/Closing/Closed 不发送也不暂存。
4. initial lane 由单 writer 按顺序写入。队列末尾放置 write barrier，确认最后一条 initial 消息已经写给本地 socket，而不只是进入 Go channel。
5. `CommitSync` 等待 write barrier；成功后在 session-local transition lock 内先把 deferred queue 原序接到正常 FIFO，再把 lifecycle 提交为 `Active`，最后开放后续 `Send`。这样 buffered update 必定位于 initial sync 之后、新广播之前。
6. deferred queue 必须有容量/字节上限；溢出视为 sync failure，进入 Closing 并统一回滚，不能静默丢消息。
7. `AbortSync` 原子封闭两条队列，拒绝新消息，并触发 generation-aware cleanup。

不要求全局 FIFO。每个 session 有独立队列和 transition lock；任何 registry/global manager 锁都不得跨 socket 写入或等待 write receipt。

### 3. 保持当前 Mumble initial sync 顺序

当前代码实际顺序是：服务端 `Version` 在 accept 后发送；认证成功后 `sendSync` 发送 `CryptSetup`、`CodecVersion`、ChannelState 列表、自己的 UserState、其他用户 roster、`ServerSync`、`ServerConfig`。其中 `CryptSetup` 还会在其余同步前注册本地连接，以便客户端紧随其后发送 UDP ping/voice。

迁移后保持客户端可见顺序：

```text
Local Edge: Version
authentication accepted
Local Edge: CryptSetup，建立本地 crypto/UDP readiness
Core initial lane:
    CodecVersion
    ChannelState...
    self UserState
    Active users roster（不含其他 Syncing session）
    ServerSync
    ServerConfig
write barrier confirmed
deferred control updates...
Active commit
向其他 Active session 广播新用户 join
```

`ServerSync` 仍保持在 `ServerConfig` 之前，不为追求形式上的“最后一条”而改变现有 wire 顺序。Active commit 以整个现有 initial sequence 的最后一条 `ServerConfig` 写入成功为准。Local Edge 可以在 Syncing 期间处理 edge-local UDP ping，但 Core 丢弃该 session 的上行 voice，voice dispatcher 也不向其投递普通音频。

### 4. Active 的唯一 commit point

唯一 commit point 是 session coordinator 的 `CommitActive(ref)`：它只能由成功的 `CommitSync` write receipt 触发，并在 session-local transition lock 下完成以下不可分割的逻辑顺序：

1. 再次校验 `SessionRef`、state=`Syncing`、Edge binding 和 transport 均仍有效。
2. 将 deferred queue 原序接到正常 FIFO，但暂不开放新的普通发送。
3. 将 registry lifecycle 改为 `Active`，随后开放新的普通发送；新消息只能排在 deferred queue 之后。
4. 将“已对外宣布”标志置位，并向其他 Active session 入队 join `UserState`。

第 4 步的入队与关闭迁移由同一个 session-local transition lock 串行化，但不得在锁内等待网络写入。若 disconnect 先取得该锁，commit 失败且不广播 join；若 commit 先完成，disconnect 随后按 join → remove 的顺序向其他 session 的 FIFO 入队。cleanup 只有在“已对外宣布”为真时才广播 `UserRemove`。

### 5. 普通业务门禁

- 控制 handler 的通用前置检查使用 registry lifecycle，而不是 `connection.StateActive`。
- Syncing session 只允许完成 initial sync 所需的 edge-local 行为；UserState、TextMessage、VoiceTarget、ACL、Channel、Ban 等普通业务请求失败关闭或忽略，具体沿用现有协议的安全行为。
- Broadcast 的 recipient selection 仅以 Active 为立即发送对象；Syncing 仅由 barrier 暂存消息，不算已收到 Broadcast。
- Voice Router 入口要求发送者 Active；canonical recipients 在分组前再次按 `SessionRef + Active` 过滤，防止状态在计算与投递之间变化。
- Local Edge 在真正加密/发送前最后校验 generation 和 Active，形成并发关闭下的最后一道防线。

## 五、运行模式与配置策略

新增类型化的 `standalone` 和 `core` 运行模式。配置来源为 `[distributed].mode`、`MUMBLE_MODE` 和 `--mode`，优先级沿用现有规则：默认值 < TOML < 环境变量 < CLI。`--mode` 必须使用“未传入则不覆盖”的处理方式，不能用 flag 默认值覆盖文件或环境配置。

`mode` 属于进程部署配置，不写入 `server_configs`，也不受 `ConfigForServer` 的数据库覆盖影响。非法值在打开数据库和监听端口之前失败。

本阶段：

- `standalone` 为默认值，remote edge capability disabled。
- `core` 启动相同的 Mumble TCP/UDP、REST、DB、mDNS 和 Local Edge，并启用 Fake Remote Edge capability，但尚不开放 Edge listener。
- 两种模式的本地用户必须走完全相同的 Core Runtime、session lifecycle、registry、ControlTransport、voice dispatcher 和 Local Edge transport；禁止保留旧 standalone 快速路径。
- 不提前接受 `edge` 后再返回运行期错误；真正实现 Edge 进程时再加入该枚举。
- `edge_listen`、region、权重等尚未被代码消费的配置不进入示例配置，避免形成虚假接口。

## 六、实施步骤

### 阶段 0：建立基线

执行并记录：

```text
go test ./...
go test -race ./internal/audio ./internal/mumble ./internal/user
```

若基线失败，先记录为既有问题，不把无关修复混入本方案。当前计划文档是未跟踪文件，实施时保留工作区其他用户改动。

### 阶段 1：加入运行模式，不改变网络行为

修改 `internal/config/config.go`、配置测试、主程序和示例配置，增加 TOML/env/CLI 解析、校验及优先级测试。两种模式启动相同服务；默认命令、现有配置和部署文件不需要修改。

### 阶段 2：引入 session/edge registry

新增 `internal/cluster`，只放置 `EdgeID`、`LocalEdgeID`、`SessionRef`、并发安全的 `Registry`，以及最小的 Edge transport 注册和查询能力。

约束：

- 不存 IP、UDP 地址或连接指针。
- 重复绑定必须显式返回错误；相同绑定可幂等。
- 移除 Edge 时返回或清理其会话集合，供未来 Core 统一断线。
- 所有 map 返回快照，不把内部可变对象暴露给调用方。
- Edge 注册记录的内部结构为下一阶段 `EdgeID + EdgeInstanceGeneration` 留出扩展点；旧实例的 unbind、ACK 或断开事件不得影响同名新实例。

本地认证成功并获得 `stored.SessionGeneration` 后绑定到 `LocalEdgeID`；断线、kick、ban 和 revalidation eviction 的所有清理入口统一调用 generation-aware unbind。绑定失败必须回滚已加入的用户，不能留下半认证会话。

### 阶段 2A：实现 lifecycle coordinator 与统一 cleanup

在 registry entry 增加 `Syncing/Active/Closing` 状态、已宣布标志和 session-local transition lock；`Authenticating` 由尚未取得 `SessionRef` 的临时 Local Edge Peer 表示，`Closed` 是 cleanup 完成后的终态或有界短期 tombstone。

认证路径调整为：

```text
authoritative authentication accepted
users.Add
construct SessionRef
registry Bind(LocalEdgeID, state=Syncing)
ControlTransport BeginSync
Local Edge CryptSetup / readiness
send initial sync
CommitSync write receipt
CommitActive
broadcast join
```

任一步失败都调用唯一的 `CloseSession(ref, cause)`；不得在多个 handler 中继续手写 `users.Remove + UnregisterConn + listener cleanup` 的不同排列。

统一 cleanup 先在 transition lock 下执行 `Syncing/Active → Closing` 并封闭控制/语音入口，然后在不持有该锁等待网络的前提下完成：

1. 若需要 final message，执行 `SendThenClose`；否则执行 `CloseAfterFlush` 或直接关闭失败 transport。
2. 只有 entry 的 announced 标志为真时才向其他 Active session 广播 `UserRemove`。
3. 以完整 `SessionRef` 清理 VoiceTarget、Listener、UDP/local transport 索引和限流器。
4. 以 generation-aware API 解绑 Edge；旧 generation 的 cleanup 不得删除新绑定。
5. 调用 `users.RemoveIfGeneration(sessionID, generation)`，回收 Session ID；现有只按 session ID 的 `Remove` 不足以支撑并发复用，必须增加条件删除。
6. 标记 Closed，删除 entry 或留下有界短期 tombstone 以拒绝迟到事件。

具体失败规则：

- `users.Add` 后 bind 失败：立即条件删除 User 并回收 ID；清理可能创建的 Listener/VoiceTarget 临时状态，不产生 join/remove 广播。
- initial sync 任一入队、写入、barrier 或 deferred queue 失败：不得 Active，转 Closing 并统一 cleanup。
- Syncing 期间断线：`CommitActive` 因 state/generation 校验失败，不广播 join；所有绑定和临时状态被清理。
- Active commit 与 disconnect 并发：session-local transition lock 决定唯一顺序；cleanup 和所有回调携带 generation。测试必须覆盖两个胜出顺序并通过 race detector。

### 阶段 3：抽象控制面 Peer

在 `internal/mumble` 定义最小且明确标注 deprecated/transitional 的 `Peer`，让 `connection.Conn` 编译期声明实现。按 handler 分批把 `ctx.(*connection.Conn)` 改为 typed Peer。

需要保留本地专属能力的路径：

- crypto negotiation、`CryptSetup` 和 nonce resync 读取 TLS/crypto 状态，是 Local Edge 逻辑，通过本地 capability helper 完成。
- 音频解码和 UDP 入站继续使用本地连接，不强塞进 Core Peer。
- 需要 `net.Conn` 的代码不得进入通用 handler。
- `Peer` 代码注释和接口测试应建立禁止项：不得加入 `CryptState`、UDP、`AudioWireMode`、音频 codec 或 socket accessor。

把 `conns` 逐步改名/改型为逻辑 peers registry 后，Broadcast、单播控制消息、kick/ban、认证同步和控制 handler 只能依赖 Peer。以 `rg "ctx\.\(\*connection\.Conn\)" internal/mumble` 的结果为审计清单；允许残留的必须仅是明确的本地传输入口并有注释。

### 阶段 3A：建立有序 ControlTransport

在把 Remote Edge 接入控制面之前定义 `ControlTransport`。最低语义要求：

- 对同一 `SessionRef` 保证 FIFO；不同 session 独立排队，不提供也不依赖全局 FIFO。
- initial sync 作为同一 session 队列上的有序序列发送。`CryptSetup` 由 Edge 本地完成后，Core 生成的 ChannelState/UserState/ServerSync 等不得乱序，也不得在同步完成前插入该 session 的普通广播。
- transport 接管命令前复制 payload 所有权；调用方返回后修改原切片不得改变已排队消息。
- generation 不匹配、session 已封闭或 Edge instance 已过期时失败关闭。
- 慢 session 只触发自己的背压/断开，不能在持有 Core 全局锁时阻塞其他 session。

增加两个终止原语：

```text
CloseAfterFlush(session, deadline)
SendThenClose(session, finalMessage, deadline)
```

`CloseAfterFlush` 原子封闭队列，拒绝新消息，排空已接管消息后关闭；`SendThenClose` 原子地把最后消息追加到 FIFO 尾部并封闭队列，确认写入或超时后关闭。Reject、kick、ban 必须统一调用 `SendThenClose`，不得继续使用 `WriteMessage` 后另起 goroutine 调用 `CloseAfterFlush` 的竞态组合。Local Edge 和 Fake Edge 使用同一组契约测试。

在此阶段一并实现 `BeginSync/SendInitial/CommitSync/AbortSync` barrier。现有 `sendSync` 改为返回错误，不得再忽略每次 `WriteMessage` 的结果；它只负责构造 Core-owned initial messages，`CryptSetup` 和 UDP readiness 从中移到 Local Edge。

### 阶段 4：消除 Core 对本地 crypto 的业务依赖

- `UpdateChannelCrypto` 从用户快照中的 `CryptoMode` 聚合。
- 用户加入、换频道、断线以及协商完成后的刷新顺序保持一致。
- 增加 mixed/legacy/lite/secure 与断线后的回归测试。
- REST 的 `ChannelCryptoModes` 和连接用户 `CryptoMode` 输出保持不变。

UDP 是否可用、加密计数器和 nonce 仍只属于 Local Edge；不能复制到 `mumble.User`。

同时迁移 Ping：UDP ping 完全留在 Local Edge；TCP `Ping` 由 Local Edge 直接回复 timestamp 与 `Good/Late/Lost/Resync`，只将客户端上报的延迟指标作为类型化事件写回 Core 用户状态。完成后 Core handler 不再读取 `c.Crypt`。

### 阶段 5：统一语音解析结果

把 `internal/audio.Router` 从“解析并逐用户发送”改为返回或提交一组 canonical recipients。必须同时迁移普通语音和 VoiceTarget：

- 普通语音继续由 Router 计算频道、linked channels、Listener、Speak、mute/deaf、上下文合并和音量。
- VoiceTarget 保留 Whisper 权限、children/links/group 规则，但输出相同的 recipient 结构，不再直接调用 `sendDeliveryTo`。
- loopback target 31 也经过同一个 dispatcher。
- 位置音频是否保留仍由 Core 根据发送者/接收者 `PluginContext` 决定，结果写入每个 recipient。

VoiceTarget 的 owner 和显式 session 目标由 `SessionRef` 固定；每次路由验证 generation 和 active 状态，以替代当前的连接指针固定语义。

Router 测试继续只验证路由策略，不依赖 socket；Mumble 集成测试继续验证 wire encoding、UDP/TCP fallback、mixed crypto 和 NAT rebinding。

### 阶段 6：加入 Edge-aware voice dispatcher

定义最小 `VoiceTransport.DeliverVoice(context.Context, VoiceBatch) error`。Dispatcher：

1. 根据 generation-aware registry 查找每个 recipient 的 Edge。
2. 丢弃不存在、generation 不匹配或 Edge 未注册的目标。
3. 按 Edge 分组；每个 Edge 每个入站语音帧最多调用一次 `DeliverVoice`。
4. 一个 Edge 失败不阻塞其他 Edge；记录受控日志/指标，不把发送错误返回给客户端协议。
5. 复制必要的 frame/切片所有权，禁止异步 transport 持有调用方会复用的 UDP buffer。

Local Edge adapter 接收 batch 后，再次固定本次使用的本地连接和 UDP 地址，保持现有 session-reuse 并发安全；使用一个 `EncodingCache` 对各 recipient 的 wire mode、context、volume 和 position 组合编码；沿用现有加密、UDP 优先、mixed crypto 强制 TCP 与 `UDPTunnel` fallback。

不得在持有 registry 或 peer map 锁时调用 transport，否则远端网络阻塞会冻结整个 Core。

### 阶段 7：Fake Remote Edge 集成验证

实现仅用于测试的 transport，不实现网络 listener。至少覆盖：

- Local sessions 1、2；Remote Edge A sessions 100、101；Remote Edge B session 200。
- 一帧同时命中三类用户时，Local/A/B 各收到一次 batch。
- A 的 100、101 位于同一 batch，而不是两次 transport 调用。
- 同一 Edge 内不同 context、volume、position 信息不丢失。
- 普通语音、VoiceTarget、Listener 和 loopback 都经过 dispatcher。
- Remote recipient 不要求 `connection.Conn`、`CryptState`、UDP 地址或 Core 本地 socket。
- 旧 generation 的迟到投递不会送给已复用的新会话。
- 一个 Fake Edge 返回错误时 Local Edge 和另一个 Fake Edge 仍完成投递。
- disconnect 与投递并发运行时通过 race 测试。

除语音外，必须增加控制面顺序测试：

- initial sync ordering：同一 session 收到的版本/同步状态序列符合 Core 生成顺序，普通广播不能穿插到同步屏障之前。
- reject + close ordering：Reject 是该 session 最后一条控制消息，Fake Edge 确认写入后才记录 close；超时路径可观测并强制关闭。
- kick + close ordering：被踢客户端先收到最终 `UserRemove`/原因再关闭，其他客户端广播不受其慢队列阻塞。
- ban + close ordering：与 kick 使用相同 `SendThenClose` 契约，并保留 `Ban=true`。
- 队列封闭后提交的新消息被拒绝，不得出现在 final message 之后。
- 两个 session 并发时各自 FIFO，慢 session 不阻塞另一个 session。
- Syncing eligibility：Syncing session 不立即接收普通 Broadcast、不接收 Voice、不作为普通业务 target；相关控制更新只进入有界 deferred queue。
- Active commit：write barrier 成功前 state 始终为 Syncing；成功后 deferred update 先于新 Broadcast，普通 Broadcast/Voice 才可抵达。
- sync failure rollback：分别模拟 initial message、write barrier 和 deferred queue 失败；session 不 Active、不 announced，User、SessionRef、Edge binding、Listener/VoiceTarget 均清理，Session ID 可由更高 generation 安全复用。
- disconnect race：在 write receipt 与 `CommitActive` 之间、commit 与 cleanup 争用 transition lock 时触发断线，验证不产生幽灵 join/remove、旧 generation 不清理新 session，并通过 `go test -race`。

### 阶段 8：收口和文档化

- 删除已经无调用的逐 session `RecipientSender` 和重复 VoiceTarget 发送路径。
- 用 `rg` 审计 `*connection.Conn`、`.Crypt`、`addrBySession` 的引用；它们只能存在于 accept/UDP/local transport/crypto negotiation 等本地边界。
- 更新 `docs/technical-overview.md` 和配置示例，说明 `core` 暂不接受真实 Edge。
- 以 `docs/architecture/core-edge-protocol-ownership.md` 审计全部已注册 handler；新增消息必须先标注 ownership 才能接入。
- 不在本阶段定义 wire message structs；先以 Go 接口和 Fake Edge 验证语义，下一阶段再选择 protobuf/QUIC/TLS 等承载方式。

## 七、预计文件变更

新增：

```text
internal/cluster/types.go
internal/cluster/registry.go
internal/cluster/registry_test.go
internal/cluster/session_lifecycle.go
internal/cluster/session_lifecycle_test.go
internal/cluster/voice.go
internal/cluster/dispatcher.go
internal/cluster/dispatcher_test.go
internal/mumble/peer.go
internal/mumble/control_transport.go
internal/mumble/session_cleanup.go
internal/mumble/local_voice_transport.go
docs/architecture/core-edge-protocol-ownership.md
```

重点修改：

```text
cmd/go-mumble-server/main.go
internal/config/config.go
internal/config/config_test.go
internal/mumble/handlers.go
internal/mumble/voicetarget.go
internal/mumble/listeners.go
internal/audio/router.go
internal/audio/router_test.go
internal/server/server.go
configs/mumble-server.toml
.env.example
docs/technical-overview.md
```

具体文件名允许随实现调整，但不得把 `internal/cluster` 扩成调度、发现或高可用框架。

## 八、提交拆分建议

1. `feat(config): add standalone and core runtime modes`
2. `feat(cluster): add generation-aware edge session registry`
3. `feat(cluster): add session lifecycle and generation-safe cleanup`
4. `refactor(mumble): introduce transitional control peer`
5. `feat(mumble): add fifo control transport and sync barrier`
6. `refactor(mumble): commit active sessions after initial sync`
7. `refactor(mumble): move crypto and ping ownership to local edge`
8. `refactor(audio): return canonical voice recipients`
9. `feat(cluster): dispatch voice batches by edge`
10. `test(cluster): verify fake edge lifecycle voice and control ordering`
11. `docs: document protocol ownership and core edge boundary`

每个提交都应通过相关包测试；第 5、6 步不应合并成一个无法定位回归的大提交。

## 九、测试与验收命令

每阶段至少执行：

```text
go test ./internal/config ./internal/cluster ./internal/audio ./internal/mumble ./internal/server
```

最终执行：

```text
go test ./...
go test -race ./internal/cluster ./internal/audio ./internal/mumble ./internal/user
go vet ./...
```

如环境允许，再用当前 Mumble 客户端进行默认模式和 `--mode=core` 下的认证、频道移动、文字消息、普通语音、VoiceTarget、混合加密 fallback、REST 用户列表与频道 crypto mode 冒烟验证。

## 十、完成定义

- 未配置 mode 时仍为 standalone，现有配置和客户端无需改变。
- core mode 仍直接接受普通 Mumble 客户端，且行为与 standalone 相同。
- 所有认证成功的本地会话都以正确 generation 绑定到 `LocalEdgeID`，所有退出路径都安全解绑。
- Session lifecycle 明确经历 Authenticating、Syncing、Active、Closing、Closed；只有 Core coordinator 能提交逻辑状态迁移。
- Syncing session 不在普通 Broadcast/Voice/业务 target 集合中，sync 期间的状态变化由有界 deferred queue 保留。
- `ServerConfig` write barrier 成功是 `Syncing → Active` 的必要条件；失败和断线均统一回滚且不产生幽灵用户。
- Core 控制 handler 不再要求对象一定是 `*connection.Conn`；残留引用仅限明确的 Local Edge 边界。
- `Peer` 被明确标记为 transitional adapter，且不包含 `CryptState`、UDP、`AudioWireMode`、codec 或 socket 能力。
- `UpdateChannelCrypto` 不再读取本地 `CryptState`。
- `CryptSetup`、UDP crypto、UDP ping、audio wire encoding 和 NAT binding 均只存在于 Edge/Local Edge 边界。
- TCP `Ping` 由 Edge 终止，Core 只接收业务指标事件。
- Control transport 对每个 session 保证 FIFO，initial sync 不被普通广播穿插。
- Reject、kick、ban 使用 `SendThenClose`，最后消息写入发生在 close 之前；超时和失败可观测。
- 普通语音、VoiceTarget 和 loopback 共享 canonical recipient → group by Edge → transport 的交付路径。
- 同一 Edge 的多个接收者每帧只触发一次 transport 调用，同时保留逐接收者 context、volume、position 和 wire-mode 正确性。
- Fake Remote Edge 能承载没有 Core 本地 socket/CryptState 的逻辑会话。
- session ID 复用、Edge 故障、断线与投递并发均失败关闭且通过 race 测试。
- 类型设计记录下一阶段 `CoreEpoch` 和 `EdgeInstanceGeneration` 的扩展约束，不把当前 ID 当作跨重启永久唯一标识。
- `go test ./...`、指定 race 测试和 `go vet ./...` 通过。

## 十一、本阶段必须实现

- Core session registry 中的 lifecycle、announced 标志和 generation-aware transition。
- Local Edge 与 Fake Edge 共用的 per-session FIFO ControlTransport、initial sync barrier 和 write receipt。
- 认证顺序改为 Syncing 后发送 initial sync、成功后唯一 Active commit、随后 join announce。
- Active-only Broadcast、普通业务 handler 和 Voice 双重门禁。
- `users.RemoveIfGeneration` 与统一 `CloseSession` rollback/cleanup。
- Reject/Kick/Ban 的 `SendThenClose` 以及 Syncing 失败的关闭语义。
- lifecycle、barrier、rollback 和 disconnect race 测试。
- standalone/core 对本地用户使用完全相同的 registry、lifecycle、ControlTransport、voice dispatcher 和 Local Edge transport。

## 十二、有意推迟到 Remote Edge 阶段

```text
ClientConnRef
Remote Edge pre-auth protocol
EdgeSessionReady 网络事件
真实 Edge 二进制与 --mode=edge
Edge-Core listener、握手、心跳和重连
网络协议、版本协商和兼容性承诺
Edge 身份认证、mTLS 与证书轮换
CoreEpoch 的实际生成
EdgeInstanceGeneration 的实际网络生命周期
自动 Edge 选择、GeoIP、RTT、遥测、容量调度
用户迁移、draining、多 Core、HA、Raft、Redis 复制
数据库分布式同步、QUIC multipath、FEC
控制消息按 Edge 批量编码优化
```

下一阶段应以本阶段稳定下来的 `Peer`、`SessionRef`、`Registry`、`VoiceBatch` 和 `VoiceTransport` 为边界，先补安全模型和协议状态机，再实现真实 Remote Edge。若届时仍需大规模修改 ACL、Channel、User 或语音接收者策略，说明本阶段边界尚未完成，不应继续叠加网络实现。

## 十三、开工前决策答复

1. **Session lifecycle 存在哪里？** `Syncing` 起存于 Core session registry entry；未分配 `SessionRef` 的 `Authenticating` 由临时 Local Edge Peer 表示，`Closed` 是 cleanup 完成后的终态或短期 tombstone。`connection.Conn.State` 只保留为本地传输适配状态，不再决定 Core 可见性。
2. **谁拥有状态迁移权？** 只有 Core session coordinator。Edge 只能报告 readiness、write receipt、失败和 disconnect。
3. **`Syncing → Active` 的唯一 commit point 是什么？** `CommitSync` 确认当前 initial sequence 最后一条 `ServerConfig` 已写入后，由 coordinator 在 session-local transition lock 中执行 `CommitActive(ref)`。
4. **Broadcast 如何识别 Syncing session？** 通过 registry 的完整 `SessionRef` 和 lifecycle；Active 立即进入正常 FIFO，Syncing 进入 deferred queue，其他状态拒绝。
5. **Voice dispatcher 如何识别 Syncing session？** Router 入口检查 Active sender，dispatcher 分组前按 `SessionRef + Active` 过滤 recipient，Local Edge 发送前再校验一次。
6. **initial sync barrier 怎么实现？** 每 session 的 initial lane、deferred queue、单 writer 和 write barrier。commit 时先把 deferred queue 接到正常 FIFO，再开放正常发送并标记 Active。
7. **sync 失败怎么 rollback？** 调用唯一 `CloseSession`：转 Closing、封闭队列、按 generation 清理临时状态和 Edge binding、`RemoveIfGeneration` 回收 User/ID，不广播从未 announced 的用户。
8. **disconnect 如何与 state transition 协调？** disconnect 与 `CommitActive` 争用同一个 session-local transition lock，并携带完整 generation；胜者确定顺序，旧 generation cleanup 不能影响复用后的新 session。
9. **standalone 和 core 是否走相同 lifecycle？** 是。两者本地用户走完全相同的 Core Runtime 和 Local Edge 路径；产品级差别只有是否启用 remote edge capability。
10. **是否改变 client-facing Mumble wire semantics？** 不改变。保留现有 `Version → CryptSetup → CodecVersion → ChannelState → UserState roster → ServerSync → ServerConfig` 的客户端可见顺序；变化只在内部状态提交、排队、错误处理和路由资格。
