# Mumble 1.5 Protobuf UDP Audio 完整修复计划

## 文档状态

- 状态：实现与 1.5.0 版本宣告完成，真实客户端互操作待验收
- 计划基线：当前 `main`
- 评估日期：2026-09-19
- 对照实现：Mumble/Murmur `v1.5.915`
- 对应 Issue：#22、#23
- 前置约束：完成全部协议、路由和互操作验收前，控制通道不得宣告 1.5.x

实施结果和剩余验收项见[音频实施与验收记录](0013-mumbleudp-audio-validation.md)。

## 一、结论

本修复不是单纯增加一个 Protobuf 结构体。完整支持 `MumbleUDP.Audio` 必须同时完成以下工作：

1. 实现符合官方字段号和 wire type 的 Protobuf Audio 编解码。
2. 将 Legacy 与 Protobuf 音频都转换为同一个协议无关模型。
3. 根据每个接收者的协商版本重新编码，支持新旧客户端在同一频道共存。
4. 在路由结果中保留 `NORMAL`、`SHOUT`、`WHISPER`、`LISTEN` 上下文，以及 Listener 音量和位置音频策略。
5. 统一 UDP 与 TCP `UDPTunnel` 的解析和发送规则。
6. 完成官方客户端互操作后，最后处理 #23 的 1.5.x 版本宣告。

纯编解码工作量中等；生产可用的完整接入难度为中高。单人熟悉代码的情况下预计 8～12 个工作日，官方客户端互操作和缺陷收口另预留 2～3 个工作日。

## 二、目标与非目标

### 目标

- 官方 Mumble 1.5.915 能与服务器协商并双向使用 Protobuf UDP Audio。
- 旧客户端继续使用 Legacy UDP，不因服务器升级而失去语音能力。
- Legacy 发送者与 Protobuf 接收者、Protobuf 发送者与 Legacy 接收者能够在服务端完成封包转换。
- UDP 与 TCP `UDPTunnel` 使用相同的连接级音频协议模式。
- 普通语音、Linked Channel、VoiceTarget、Listener 和 Loopback 均携带正确的接收上下文。
- Listener 音量通过现代 Audio 的 `volume_adjustment` 下发。
- 客户端不能伪造 `sender_session`、`context` 或接收者音量。
- 音频热路径继续只转发 Opus 负载，不解码、不重采样、不转码。

### 非目标

- 本修复不改变 UDP 加密算法；Protobuf UDP 与 OCB2/AES-GCM 是不同层次。
- 本修复不引入 `google.golang.org/protobuf` 运行时依赖，继续使用项目现有手写 wire 编码策略。
- 本修复不扩展 VoiceTarget ID 范围；沿用官方 Murmur 接受的 1～30，0 为普通语音，31 为服务器 Loopback。
- 本修复不实现音频内容转码。现代 Protobuf Audio 仅接受 Opus。
- 本修复不顺带处理与音频协议无关的 UserList、ACL 编辑器、UserStats 或多虚拟服务器问题。

## 三、协议事实

`MumbleUDP.Audio` 的官方字段定义如下：

| 字段 | 编号 | Wire type | 方向和语义 |
| --- | ---: | ---: | --- |
| `target` | 1 | varint | 客户端到服务端；0、1～30、31 |
| `context` | 2 | varint | 服务端到客户端；NORMAL/SHOUT/WHISPER/LISTEN |
| `sender_session` | 3 | varint | 服务端发送时必须设置；客户端值不可信 |
| `frame_number` | 4 | varint | 当前包中第一帧在语音流中的编号 |
| `opus_data` | 5 | length-delimited | Opus 负载，不允许为空 |
| `positional_data` | 6 | packed fixed32 | 可选，存在时必须恰好为 X/Y/Z 三个 float32 |
| `volume_adjustment` | 7 | fixed32 | 服务端按接收者设置；零值表示未设置 |
| `is_terminator` | 16 | varint | 当前语音流结束标记 |

完整 UDP 明文包由一个消息类型字节和 Protobuf body 组成：

```text
0x00 | protobuf(MumbleUDP.Audio)
```

实现必须特别注意：

- `target` 与 `context` 是 `oneof`，方向由连接角色决定。
- `is_terminator` 的字段号是 16，不是当前文档中错误记录的 8。
- proto3 的 repeated 数值字段默认使用 packed 编码；解码器同时接受 packed 与 unpacked，以兼容合法 Protobuf 编码器。
- 未知字段必须可跳过，保证向前兼容。
- 官方最大 UDP 明文包大小为 1024 字节；编码和解码都要执行上限检查。
- 客户端发来的 `sender_session`、`context` 和 `volume_adjustment` 不参与授权或路由决策。

## 四、实施前缺口（已落实，互操作除外）

### 编解码层

- `pkg/mumble/audio/packet.go` 只解析客户端到服务端的 Legacy Opus 包。
- 没有 Legacy 服务端方向编码器，也没有 Protobuf Audio 编解码器。
- Legacy 解析结果没有完整保留位置音频数据。
- 当前文档错误声明现代 UDP 已实现，并把 `is_terminator` 写成字段 8。

### 接收路径

- `HandleUDP` 解密后直接按 Legacy header 的高三位判断消息类型。
- `handleUDPTunnel` 同样把首字节当作 Legacy codec/target。
- 已认证连接的现代 Ping 与 Audio 尚未根据连接版本解析。
- 入站音频没有统一的格式、尺寸和 Opus 校验入口。

### 路由路径

- `internal/audio.Router` 接收和转发原始 `[]byte`，无法理解帧号、位置、终止标记等协议中立属性。
- 路由结果只有接收者 Session，没有接收上下文、Listener 音量和位置音频资格。
- VoiceTarget 的显式用户、频道目标和频道 Listener 没有在输出包中区分 `WHISPER`、`SHOUT`、`LISTEN`。

### 发送路径

- `rewriteAudioPacket` 只能给 Legacy 包插入 Session ID。
- `SendAudio` 对所有接收者复用同一明文包，无法按接收者版本编码。
- TCP 回退直接发送调用方给出的原始包，不能保证与接收连接的协议版本一致。
- 当前没有按协议版本、context、音量和位置数据资格进行编码分组。

### 协商层

- 连接已经保存客户端 `VersionV1/VersionV2`，但没有明确保存音频 wire mode。
- 1.5.0 版本宣告已启用；连接模式根据客户端版本分流，旧客户端继续使用 Legacy。

## 五、目标架构

### 5.1 协议无关音频模型

在 `pkg/mumble/audio` 增加统一模型：

```go
type WireMode uint8

const (
    WireLegacy WireMode = iota
    WireProtobuf
)

type Context uint8

const (
    ContextNormal Context = iota
    ContextShout
    ContextWhisper
    ContextListen
)

type Frame struct {
    Codec         Codec
    Target        uint32
    SenderSession uint32
    FrameNumber   uint64
    OpusData      []byte
    Position      [3]float32
    HasPosition   bool
    IsTerminator  bool
}

type Delivery struct {
    Frame
    Context          Context
    VolumeAdjustment float32
}
```

`Frame` 表示发送者提交的音频事实；`Delivery` 表示服务端为某个接收者生成的交付语义。客户端输入不能直接决定 `Delivery.Context`、`Delivery.VolumeAdjustment` 或最终 `SenderSession`。

### 5.2 编解码接口

提供面向方向的接口，避免调用方混淆 `target` 与 `context`：

```go
func DecodeClientPacket(mode WireMode, data []byte) (Frame, error)
func EncodeServerPacket(mode WireMode, delivery Delivery) ([]byte, error)
```

必要时保留更底层的测试接口，但生产代码只调用方向明确的 API。

### 5.3 连接级协议模式

由控制通道 Version 协商得到唯一模式：

```text
客户端版本未知或 < 1.5.0  -> WireLegacy
客户端版本 >= 1.5.0       -> WireProtobuf
```

- UDP 与 TCP `UDPTunnel` 共用该模式。
- 认证后音频包不得通过内容猜测模式。
- 未认证服务器列表 Ping 继续保留现有双格式探测，因为此时确实没有连接版本。

### 5.4 按接收者生成 Delivery

路由器输出接收者及其交付元数据：

| 路由来源 | Context |
| --- | --- |
| 普通当前频道语音 | `NORMAL` |
| 普通语音经 Linked Channel 到达 | `NORMAL` |
| VoiceTarget 显式用户 | `WHISPER` |
| VoiceTarget 频道、子频道或链接频道目标 | `SHOUT` |
| 通过 Channel Listener 接收 | `LISTEN` |
| 服务器 Loopback | `NORMAL` |

同一接收者经多条路径命中时，按 Murmur 规则：

- 只保留一份交付。
- Context 取数值较小者。
- Listener 音量取 factor 较大者。
- 只有发送者和接收者的 plugin context 相等时才保留位置音频；否则移除位置字段。

### 5.5 按兼容组编码

发送前按以下维度分组：

```text
WireMode × Context × VolumeAdjustment × HasPosition
```

每组编码一次，再分别使用每个接收者的 CryptState 加密并发送。这样避免为同组每个接收者重复执行 Protobuf 编码。

## 六、实施阶段

### 阶段 0：规格锁定与文档纠偏

- 将官方 `MumbleUDP.proto` 的固定版本副本放入测试数据目录，仅作字段校验，不参与代码生成。
- 增加字段号、wire type 和消息类型常量测试。
- 修正 `docs/protocol/voice-data.md` 与 `docs/patterns/audio-pipeline-pattern.md`：
  - 状态改为未实现或实施中。
  - `is_terminator` 改为字段 16。
  - 明确 packet type 前缀、oneof 和 packed positional data。
- 建立来自官方定义的固定十六进制 golden vectors。

完成条件：测试能够在字段号、wire type 或前缀发生漂移时失败。

### 阶段 1：实现 Protobuf Audio 纯编解码

新增建议文件：

- `pkg/mumble/audio/frame.go`
- `pkg/mumble/audio/protobuf.go`
- `pkg/mumble/audio/protobuf_test.go`

编码器必须：

- 写入 `0x00` Audio 前缀。
- 客户端方向只写 target；服务端方向只写 context 和真实 sender session。
- 使用 packed fixed32 编码三个位置坐标。
- 只在非默认时写 volume adjustment 和 terminator。
- 生成包超过 1024 字节时返回错误。

解码器必须：

- 校验前缀、tag、wire type、长度和 varint 溢出。
- 接受 packed/unpacked positional data。
- 对 singular 重复字段采用最后值，按 Protobuf oneof 的最后字段语义处理 target/context。
- 跳过所有合法未知字段。
- 拒绝空 Opus、位置数量非 0/3、截断字段和超长包。
- 返回独立拥有的 Opus 字节，或明确约束输入缓冲区生命周期；不得引用 UDP 读循环下一次会覆盖的缓冲区。

完成条件：单元测试、错误用例和 fuzz 测试通过，无 panic、越界或无限循环。

### 阶段 2：重构 Legacy 编解码

- 将现有 `ParsePacket` 迁移到统一 `Frame`。
- 增加 Legacy 客户端方向解码和服务端方向编码。
- 完整处理 Session、帧号、Opus 长度、terminator 和三个 float32 位置坐标。
- 对尾随字节执行严格校验；只接受无位置数据或恰好 12 字节位置数据。
- 保留旧公开 API 的兼容包装，待内部调用全部迁移后再决定是否删除。

完成条件：Legacy client/server 两个方向都能 round-trip，并能与现有测试向量兼容。

### 阶段 3：接入连接协商与双入口解析

- 在 `connection.Conn` 中增加只读的音频 wire mode，或提供基于协商版本的稳定查询函数。
- `handleVersion` 计算并保存模式；未知版本保持 Legacy。
- `HandleUDP` 完成发送者识别和解密后，使用发送者连接模式解析 Ping 或 Audio。
- `handleUDPTunnel` 使用 TCP 连接的同一模式解析 Audio。
- 现代连接的已认证 Ping 使用 Protobuf Ping；Legacy 连接保持 Legacy Ping。
- 删除音频路径对首字节 Legacy bit-field 的无条件判断。

安全要求：无论客户端 Audio 是否携带 `sender_session`，均覆盖为当前已认证 Session。

完成条件：相同客户端通过 UDP 和 TCP 隧道提交同一 Frame 时，路由输入完全一致。

### 阶段 4：路由器改为 Frame/Delivery

- 将 `internal/audio.Router.Route` 的原始包参数替换为 `Frame`。
- 将 Sender 接口改为接收 `Delivery`，不再接收调用方预编码的 `[]byte`。
- 普通频道、Linked Channel、VoiceTarget、Listener、Loopback 分别设置正确 Context。
- 重构 VoiceTarget 解析结果，使显式用户、频道占用者和 Listener 带有各自交付上下文。
- 将 ListenerManager 中已保存的音量 factor 加入 Delivery。
- 合并重复接收者时实现稳定的 Context 和音量优先级。
- 根据发送者与接收者 plugin context 决定是否保留位置数据。

完成条件：路由测试不依赖 wire bytes，只断言接收者、Context、音量和位置资格。

### 阶段 5：按接收者模式编码和发送

- `sendAudioTo` 根据接收连接的 WireMode 编码 Delivery。
- UDP 路径先编码，再使用接收者 CryptState 加密。
- TCP 回退将同一编码结果作为 `UDPTunnel` payload，不改变 wire mode。
- 保留 mixed-crypto 频道强制 TCP 行为，但不得因此回退为 Legacy wire format。
- 引入本次路由调用内的兼容组缓存，避免重复编码。
- 删除 `rewriteAudioPacket`。

必须覆盖四种转换：

| 入站 | 出站 | 预期 |
| --- | --- | --- |
| Legacy | Legacy | 重新编码 Legacy |
| Legacy | Protobuf | 转为 Protobuf Audio |
| Protobuf | Legacy | 转为 Legacy Audio |
| Protobuf | Protobuf | 重写 sender/context 后编码 Protobuf |

完成条件：新旧客户端可在同一频道、Linked Channel 和 VoiceTarget 中互相收听。

### 阶段 6：版本宣告与能力启用

该阶段已执行：

- 在自动测试完成后，将控制通道 Version 提升到 1.5.x；真实客户端互操作证据仍需补录。
- 同时发送正确的 `VersionV1` 和 `VersionV2`。
- 将当前“必须处于 1.4.x”的保护测试替换为 1.5.x 能力一致性测试。
- 保留旧客户端按自身版本使用 Legacy 的路径。
- 不以自定义 `CryptoModes` 代替官方版本协商；加密模式与音频 wire mode 独立处理。

完成条件：服务器宣告的 1.5 音频能力已有实现；真实客户端互操作证据作为发布验收项继续跟踪。

### 阶段 7：文档和 Issue 收口

- 更新 README、产品概览、技术概览、Voice Data 和 Audio Pipeline 文档。
- 删除“现代 UDP 已实现”与实际状态不一致的旧描述。
- 在 #22 记录协议测试、互操作版本和抓包证据后关闭。
- #22 关闭后再更新并关闭 #23；当前代码已完成 #23 的版本宣告实现。
- 记录对旧客户端、Mumla/Plumble 和自定义 secure/lite 客户端的兼容结果。

## 七、测试计划

### 7.1 编解码单元测试

- 所有字段单独出现和组合出现。
- target/context oneof 的两种顺序。
- sender session、frame number 的边界值。
- 1、127、128、最大合法 varint 等长度边界。
- packed 与 unpacked positional data。
- 正负数、零值和普通值的 float32 bit 保真。
- terminator 字段 16 的两字节 tag。
- 未知 varint、fixed32、fixed64、length-delimited 字段。
- 重复字段、空 Opus、截断字段、非法 wire type、超长长度、超过 1024 字节。
- 解码后输入缓冲区被覆盖时，Frame 数据仍然有效。

### 7.2 Legacy/Protobuf 转换测试

- Legacy -> Frame -> Legacy。
- Protobuf -> Frame -> Protobuf。
- Legacy -> Frame -> Protobuf。
- Protobuf -> Frame -> Legacy。
- 帧号、Opus、位置和 terminator 在可表达范围内保持一致。
- Protobuf 的 Context/音量不会错误写入客户端到服务端的包。

### 7.3 路由语义测试

- 普通频道接收者为 NORMAL。
- Linked Channel 接收者为 NORMAL，并继续执行逐频道 Speak 检查。
- VoiceTarget 显式用户为 WHISPER。
- VoiceTarget 频道目标为 SHOUT。
- Listener 为 LISTEN，并携带正确音量。
- 同一接收者多路径命中时只发送一次，Context/音量合并正确。
- Deaf/SelfDeaf、Mute/Suppress/SelfMute、ACL 与 session 复用保护保持有效。
- Plugin context 相同才保留位置音频。

### 7.4 传输与协商测试

- Legacy UDP、Protobuf UDP、Legacy UDPTunnel、Protobuf UDPTunnel。
- UDP 探测成功后发送；UDP 不可用时 TCP 回退。
- mixed-crypto 频道强制 TCP 后仍使用接收者协商的音频 wire mode。
- NAT 重绑定后重新识别发送者并按其模式解析。
- 客户端伪造 sender session 不影响最终发送者身份。
- 1.4 与 1.5 客户端同时在线时互相收听。

### 7.5 质量测试

- 对 Protobuf 和 Legacy 解码器执行 Go fuzz。
- 对音频路由和连接断开/session 复用执行 `go test -race`。
- 全量 `go test ./...`。
- 在实际部署网络中执行持续说话、快速按键、丢包、乱序和 TCP/UDP 切换测试。

### 7.6 官方客户端互操作矩阵

至少验证：

- Mumble Desktop 1.5.915。
- 一个仍使用 Legacy UDP 的旧版官方客户端。
- Mumla/Plumble 或项目实际支持的 Android 客户端。
- 项目自定义 secure/lite 客户端（若仍作为公开能力保留）。

场景包括普通语音、Loopback、VoiceTarget、Linked Channel、Listener、位置音频、停止说话、断线重连、UDP 和强制 TCP。

## 八、验收标准

以下条件必须全部满足：

- [ ] Audio 字段号、wire type、前缀和最大长度与官方 1.5.915 一致。
- [ ] `is_terminator` 使用字段 16。
- [ ] 解码器支持合法 unknown field、packed/unpacked position，拒绝所有截断和越界输入。
- [ ] 入站 sender session 始终由认证连接覆盖。
- [ ] UDP 与 `UDPTunnel` 都按连接版本解析和编码。
- [ ] 四种 Legacy/Protobuf 新旧组合全部通过自动测试。
- [ ] NORMAL/SHOUT/WHISPER/LISTEN context 正确。
- [ ] Listener volume adjustment 和位置音频过滤正确。
- [ ] mixed-crypto TCP 回退不改变音频 wire mode。
- [ ] 官方 Mumble 1.5.915 使用 Protobuf UDP 完成双向语音。
- [ ] 旧客户端仍能通过 Legacy UDP/TCP 正常语音。
- [ ] 全量测试、race 测试和 fuzz 冒烟通过。
- [ ] 完成上述验收后才宣告服务端 1.5.x。

## 九、提交拆分

建议按以下顺序提交，保持每一步可审查、可回滚：

1. `docs(protocol): 校准 MumbleUDP Audio 官方字段与当前缺口`
2. `feat(audio): 增加协议无关 Frame 与 Protobuf Audio 编解码`
3. `refactor(audio): 用统一 Frame 重写 Legacy 双向编解码`
4. `feat(connection): 保存连接级音频 wire mode`
5. `refactor(audio): 路由器输出带 context 和音量的 Delivery`
6. `feat(audio): 按接收者版本编码 UDP 与 UDPTunnel`
7. `test(audio): 覆盖跨协议路由、回退、fuzz 与 race`
8. `feat(protocol): 完成 1.5 VersionV1/VersionV2 宣告`
9. `docs(protocol): 更新兼容矩阵并关闭 #22/#23`

第 8 个提交前的代码可以合入但保持功能不对官方 1.5 客户端宣告；第 8 个提交是唯一启用协商行为的开关点。

## 十、发布、观测与回滚

### 发布策略

- 先发布到测试服务器，仅允许已知客户端连接。
- 开启现有 `voice_debug`，增加低频统计：入站 wire mode、出站分组数量、解析失败原因、UDP/TCP 选择。
- 日志不得记录 Opus 负载、密钥、nonce 或完整原始包。
- 观察解析失败率、UDP 回退率、CryptState 失败率和每包编码耗时。
- 通过小规模真实频道验证后再扩大部署。

### 回滚策略

- 版本宣告是最后且独立的提交；紧急情况下可只把控制通道版本退回 1.4.x，让官方客户端恢复 Legacy UDP。
- 保留全部 Legacy 编解码和按客户端版本分流代码，不通过删除 Legacy 实现完成升级。
- 回滚版本宣告不需要回滚数据库，也不改变用户、频道、ACL 或证书数据。
- 若仅现代路径异常，可在后续增加临时启动开关强制宣告 1.4.x；开关只控制能力宣告，不允许连接宣告与实际解析模式不一致。

## 十一、主要风险与控制措施

| 风险 | 后果 | 控制措施 |
| --- | --- | --- |
| 提前宣告 1.5.x | 官方客户端切换后语音失效 | 版本提交最后合入，保留保护测试 |
| 继续透传原始包 | 新旧客户端无法共存，身份或 context 错误 | 强制经过 Frame/Delivery 和接收者侧编码 |
| 信任客户端 sender/context | 身份伪造或错误路由显示 | 服务端覆盖 sender，路由器生成 context |
| packed float 处理错误 | 位置音频无法互操作 | packed/unpacked 双解码和官方 golden vectors |
| Listener 元数据丢失 | 1.5 客户端音量或 UI 语义错误 | 路由结果携带 LISTEN 与 volume adjustment |
| 热路径重复编码 | CPU 和延迟上升 | 按兼容组缓存编码结果，增加 benchmark |
| 缓冲区生命周期错误 | 音频数据被下一次 UDP 读取覆盖 | 明确所有权并用测试覆盖缓冲区复用 |
| 新旧路径行为分叉 | ACL、deaf、链接权限回归 | 路由只处理 Frame，不按 wire mode 分叉授权逻辑 |

## 十二、工作量评估

| 工作项 | 预计时间 |
| --- | ---: |
| 规格校准、测试向量、文档纠偏 | 0.5～1 天 |
| Protobuf Audio 编解码与 fuzz | 1～2 天 |
| Legacy 双向编解码与统一 Frame | 1～2 天 |
| 连接协商和 UDP/UDPTunnel 接入 | 1～1.5 天 |
| Router/VoiceTarget/Listener Delivery 重构 | 2～3 天 |
| 按接收者编码、缓存与发送 | 1～2 天 |
| 自动测试、race、性能检查 | 1～2 天 |
| 官方客户端互操作与问题收口 | 2～3 天 |

总计约 10～16 个工作日。编码本身不是最大工作项；接收者上下文、Listener 音量、跨版本转换和互操作测试决定最终工期。
