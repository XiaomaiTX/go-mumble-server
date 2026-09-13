# Linked Channel 修复计划

## 文档状态

- 状态：Implemented
- 实现基线：当前工作区（待提交）
- 评估日期：2026-09-13
- 对照实现：Mumble/Murmur `v1.5.915`
- 对应 Issue：#6、#7、#14

## 实施结果

- `ConnectedChannelIDs` 以无向图遍历返回完整、稳定排序的 linked-channel 连通分量；普通语音、VoiceTarget `links=true` 和 mixed-crypto 判断已统一使用该查询。
- 普通语音将发送者状态门禁与逐频道 `Speak` 权限检查拆开。当前频道无权时整包丢弃；连通分量中的单个目标频道无权时，仅跳过该频道的占用者与 Listener。
- `UpdateLinks` 在单次数据库事务和频道写锁内写入双向链接。ChannelState 的新增链接要求两端均有 `LinkChannel`；删除链接要求至少一端有该权限。
- 新增回归测试覆盖单向历史链接的无向多跳遍历、双向持久化、目标频道 Speak deny，以及两端 LinkChannel 授权。

## 一、结论

Linked Channel 并非完全没有实现。当前代码已经能够保存频道链接，并把普通语音转发给发送者所在频道及其直接链接频道；VoiceTarget 在 `links=true` 时也会自行遍历链接；Linked Channel Listener 已有集成测试。

目前缺少的是完整且一致的语义：

1. #7 是安全缺陷。普通语音只检查发送者在当前频道的 `Speak` 权限，之后直接向链接频道转发，没有逐一检查发送者在每个目标频道的 `Speak` 权限。因此用户可能从允许发言的频道，经链接向明确拒绝其 `Speak` 的频道发送音频。
2. #6 是连通性缺陷。`LinkedChannelIDs` 只返回当前频道和一跳链接，普通语音无法覆盖 A—B—C 这类完整连通分量；VoiceTarget 则有一套独立 BFS，两个路径的行为可能分叉。
3. #14 是链接管理缺陷。当前 `ChannelState` 修改链接主要检查目标频道的 `Write`，尚未按官方 `LinkChannel` 规则校验链接两端，也没有在同一原子操作中保证双向内存与数据库状态一致。

所以，“Linked Channel 没实现”不是因为 #7；更准确的说法是：基础链接和一跳转发已实现，但 #7 使其不满足安全要求，#6 使其不满足完整连通语义，#14 使链接管理本身仍不完整。

## 二、修复范围与顺序

本轮按“先关闭音频越权，再统一语义，最后收紧管理入口”的顺序实施。

### 阶段 1：统一链接图读取

在 `internal/channel` 提供唯一的 connected-component 查询，例如 `ConnectedChannelIDs(channelID)`：

- 从起始频道执行 BFS 或 DFS，返回包含起点的完整链接连通分量。
- 将有效链接按无向边解释，使读取侧不会因历史数据只保存单边链接而产生方向相关的路由结果。
- 忽略不存在的频道、自链接和重复边，并使用稳定顺序返回，避免测试和日志随机抖动。
- 整次遍历在一次读锁内完成，不在音频热路径中反复获取频道锁。

`LinkedChannelIDs` 不再作为音频路由的事实来源；可保留为兼容包装，也可在调用方迁移完成后删除。普通语音、VoiceTarget `links=true`、Listener 和 mixed-crypto 判断统一调用 connected-component 查询。

### 阶段 2：修复 #7 的逐频道 Speak 检查

重构普通语音路由的权限回调，将当前的 `CanSenderSpeak(sessionID)` 拆为两个职责：

- 发送者状态门禁：连接存在，且没有 `Mute`、`Suppress` 或 `SelfMute`。
- 频道权限门禁：`CanSenderSpeakInChannel(sessionID, channelID)` 使用同一个 `acl.Subject`，对实际扫描的每个频道检查 `PermissionSpeak`。

路由规则如下：

1. 发送者状态门禁失败，整包丢弃。
2. 发送者当前频道的 `Speak` 失败，整包丢弃。
3. 当前频道通过后，遍历完整链接连通分量；某个链接频道的 `Speak` 失败时，只跳过该频道的占用者和监听者，不影响其他已授权频道。
4. 接收者仍统一执行 deaf/self-deaf、连接状态和去重过滤。
5. ACL 查询失败、发送者会话失效或频道在路由期间被删除时失败关闭，不发送到对应频道。

不得只在收集完接收者后按接收者当前频道过滤，因为 Listener 可以位于其他频道；权限判断必须绑定“本次音频代表的目标频道”。

### 阶段 3：统一 VoiceTarget 与普通语音

删除 `voicetarget.go` 内部独立的链接 BFS，改用相同的 connected-component 查询。保留协议权限差异：

- 普通语音 target 0 对每个频道检查 `Speak`。
- VoiceTarget/Whisper 对每个目标频道检查 `Whisper`，不改成 `Speak`。
- `children=true` 在扩展子树后仍按每个实际目标频道检查权限。
- Group selector 在实际目标频道上求值；Listener 与频道占用者执行相同的组过滤。

这样共享“图遍历和目标频道集合”，但不错误合并 `Speak` 与 `Whisper` 的权限语义。

### 阶段 4：修复 #14 的链接变更

在音频读取修复完成后，单独收紧 `ChannelState.links`、`links_add` 和 `links_remove`：

- 新增链接要求操作者在链接两端都具有 `LinkChannel`。
- 删除链接要求操作者至少在其中一端具有 `LinkChannel`。
- 拒绝不存在频道、自链接和重复输入；root 频道按相同规则处理。
- 在 Manager 的单个写锁和数据库事务内更新链接两端，提交失败时不得留下内存与数据库分叉。
- 广播双方最终状态；客户端提交的单边变更不得形成永久单向链接。
- 对已有单向、悬空或重复数据提供启动时规范化，或提供一次性迁移；实施时根据数据库模型选择其中一种并记录兼容影响。

## 三、测试计划

### Channel Manager 单元测试

- A—B—C 返回同一完整连通分量，从 A、B、C 查询结果一致。
- 环、自链接、重复边和悬空频道不会死循环或重复返回。
- 历史单向 A→B 数据从两端查询均得到 A、B。
- 返回顺序稳定，调用方修改返回切片不会修改内部状态。

### 普通语音集成测试

- A 与 B 直连且两端允许 `Speak` 时，B 的占用者和 Listener 都能收到音频。
- A—B—C 多跳且全部允许时，C 能收到音频。
- 发送者在 A 允许、B 拒绝、C 允许时，A 和 C 的接收者收到，B 的占用者和 Listener 均收不到。
- 发送者在当前频道 A 被拒绝时，任何链接频道都收不到。
- 同一用户同时占用或监听多个目标频道时只收到一次。
- mute、suppress、self-mute、deaf、self-deaf、断线及 session 复用行为保持失败关闭。
- ACL 或链接在并发变更时无数据竞争，且不会因旧缓存继续向已拒绝频道发送。

### VoiceTarget 与链接管理测试

- `links=true` 覆盖多跳连通分量，并逐频道执行 `Whisper`。
- `links=false` 只命中显式频道；`children=true` 与链接组合不重复发送。
- 新增链接缺少任一端 `LinkChannel` 时拒绝；删除链接在两端都无权限时拒绝。
- 成功新增或删除后，内存、数据库、双方 `ChannelState` 广播和重启恢复结果一致。
- 数据库事务失败时回滚，不产生半条链接。

## 四、实施切分

本次按以下三个可独立审查的变更实施：

1. `fix(channel): 统一 linked-channel 连通分量遍历`：实现图查询，迁移普通语音、VoiceTarget、Listener 和 mixed-crypto 调用方，关闭 #6。
2. `fix(audio): 对 linked-channel 逐频道检查 Speak`：重构路由门禁并补齐安全回归测试，关闭 #7。
3. `fix(channel): 按 LinkChannel 权限原子更新双向链接`：修复管理入口和持久化一致性，关闭 #14。

若第一个变更会暂时扩大普通语音可达范围，则第一、第二个变更必须在同一个发布版本中交付；不得只上线完整连通图而保留一处 `Speak` 检查。

## 五、验收标准

- 普通语音与 VoiceTarget 共用唯一的链接连通分量实现。
- 任一目标频道拒绝发送者对应语音权限时，该频道的占用者和 Listener 都不会收到音频。
- 多跳链接从任一频道进入都得到一致结果，且每个接收者最多收到一次。
- 链接增删符合官方 `LinkChannel` 两端权限规则，内存和数据库保持双向一致。
- `go test ./...`、`go vet ./...` 和格式检查通过；Linux CI 的 race 测试通过。
- 使用官方 Mumble 客户端完成 A—B—C 多跳、目标频道 Speak deny、Whisper、Listener、链接增删和重连黑盒验证。
