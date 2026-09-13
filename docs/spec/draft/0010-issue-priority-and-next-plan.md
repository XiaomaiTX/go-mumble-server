# 下一阶段修复优先级与执行计划

## 文档状态

- 状态：Draft
- 计划基线：`97eb8ce`
- 评估日期：2026-09-13
- 对照实现：Mumble/Murmur `v1.5.915`
- 当前验证：`go test ./...` 已通过

本文档根据 GitHub Issue 与当前代码重新评估下一阶段工作顺序。Issue 标题中的旧优先级不再直接作为执行依据；实现状态、实际安全影响、官方协议兼容性和项目产品定位共同决定优先级。

## 一、当前结论

GitHub 当前有 26 个开放 Issue，另有 [#27](https://github.com/XiaomaiTX/go-mumble-server/issues/27) 已关闭并标记为 completed。开放 Issue 当前主要通过标题记录优先级，实际 GitHub Labels 和讨论较少，因此需要持续用代码和测试结果校准状态。

当前最重要的结论是：

1. ACL、Group、linked-channel 音频权限、BanList 查询和 Tree TextMessage 仍然属于公开生产前的安全门槛。
2. VoiceTarget session 复用问题、QueryUsers 的 session-ID 问题、外部身份接口缺失问题已经被近期提交部分或主要解决，原 Issue 标题已过期。
3. Channel Listener 已基本实现，但仍存在注册用户持久化和现代 UDP 音量字段等收口工作。
4. 当前控制连接只宣告 Mumble `1.4.0`，这是为了避免客户端协商尚未实现的 Protobuf UDP；不能在实现 #22 之前单独把协议版本提高到 `1.5.x`。
5. 如果项目定位是 EVE 的受控语音服务器，Legacy UDP 和原生管理协议可以后置；如果定位是官方 Mumble 1.5.x 的替代服务器，则 #22/#23 必须升级为发布阻断项。

## 二、重新排序后的 Issue 矩阵

### P0：公开生产前必须完成

| Issue | 主题 | 当前判断 | 执行要求 |
| --- | --- | --- | --- |
| [#3](https://github.com/XiaomaiTX/go-mumble-server/issues/3) | ACL Traverse、Write、root-only 语义 | 仍有效，可能造成权限扩大或权限判断错误 | 重构 Murmur 风格的权限计算状态机，并增加根权限与子频道测试 |
| [#4](https://github.com/XiaomaiTX/go-mumble-server/issues/4) | Group selector、Token、证书语义 | 仍有效，可能让错误用户匹配到 ACL Group | 补齐官方 selector、Token 大小写、`sub` 参数和临时成员语义 |
| [#5](https://github.com/XiaomaiTX/go-mumble-server/issues/5) | Group 继承优先级 | 仍有效，子频道 remove 可能被父级 add 覆盖 | 按 root 到目标频道的顺序重算，并覆盖 `inherit/inheritable` 组合 |
| [#7](https://github.com/XiaomaiTX/go-mumble-server/issues/7) | Linked Channel 目标频道 Speak 检查 | 仍是音频权限绕过风险 | 对每个目标频道单独检查发送者 Speak 权限 |
| [#8](https://github.com/XiaomaiTX/go-mumble-server/issues/8) | BanList 查询权限 | 仍可能向普通用户泄露 IP、证书 hash 和封禁原因 | 查询和更新入口统一检查 root Ban 权限 |
| [#10](https://github.com/XiaomaiTX/go-mumble-server/issues/10) | Tree TextMessage 子频道权限 | 仍可能绕过 descendant channel 的 TextMessage deny | 按每个实际目标频道独立检查权限 |

P0 项应先于新协议功能和管理界面功能处理。#3～#5 应作为同一轮 ACL/Group 重构，而不是继续进行局部补丁。

#3～#5 的实现、兼容影响和验收结果见 [0011 ACL 与 Group 联合修复说明](0011-acl-group-fix.md)。

### P1：下一批完成

| Issue | 主题 | 当前判断 | 执行要求 |
| --- | --- | --- | --- |
| [#2](https://github.com/XiaomaiTX/go-mumble-server/issues/2) | VoiceTarget 发言者、接收者和 Whisper 权限 | 核心缺口已修，原 P0 标题过期 | 补齐与 Murmur 的回归测试；确认后关闭或改为兼容性验证项 |
| [#6](https://github.com/XiaomaiTX/go-mumble-server/issues/6) | Linked Channel 完整连通图 | 普通语音仍主要只走一跳；VoiceTarget `links=true` 已自行 BFS | 统一正常语音和 Whisper 的 connected-component 遍历 |
| [#9](https://github.com/XiaomaiTX/go-mumble-server/issues/9) | 空 BanList 清空全部封禁 | 仍有效 | `query=false` 时始终执行 replace，包括空数组 |
| [#11](https://github.com/XiaomaiTX/go-mumble-server/issues/11) | Temporary Channel 生命周期 | 对官方客户端兼容属于 P1；受控 EVE 部署可降为 P2 | 临时频道不持久化，最后一个用户离开后按官方规则清理，重启不恢复 |
| [#14](https://github.com/XiaomaiTX/go-mumble-server/issues/14) | LinkChannel 权限 | 当前主要检查 Write，存在链接权限扩大风险 | 按官方 LinkChannel 规则分别检查链接两侧并同步内存与数据库 |
| [#22](https://github.com/XiaomaiTX/go-mumble-server/issues/22) | Mumble 1.5 Protobuf UDP Audio | 以官方 1.5.x 兼容为目标时属于 P1；Legacy-only 部署可降为 P2 | 完成 Audio 编解码、模式选择、sender session、target/context 和转发测试 |
| [#23](https://github.com/XiaomaiTX/go-mumble-server/issues/23) | VersionV1/VersionV2 协商 | 必须依赖 #22，不能单独提前修复 | #22 完成并通过互操作测试后，再宣告对应协议能力 |

### P2：协议与管理能力补齐

| Issue | 主题 | 当前判断 |
| --- | --- | --- |
| [#12](https://github.com/XiaomaiTX/go-mumble-server/issues/12) | ChannelState proto2 field presence | 仍有效，影响显式清空和设置零值 |
| [#13](https://github.com/XiaomaiTX/go-mumble-server/issues/13) | Channel reparent/move | 仍未实现，属于频道管理兼容性 |
| [#15](https://github.com/XiaomaiTX/go-mumble-server/issues/15) | 原生 ACL groups/acls 消息 | REST 可用，但原生 Mumble ACL 编辑器不完整 |
| [#16](https://github.com/XiaomaiTX/go-mumble-server/issues/16) | 原生 UserList | 当前 handler 仍是 no-op |
| [#18](https://github.com/XiaomaiTX/go-mumble-server/issues/18) | 完整 UserStats | 当前只回显很少的请求字段，属于管理和可观测性缺口 |
| [#19](https://github.com/XiaomaiTX/go-mumble-server/issues/19) | Channel Listener | 核心功能已实现；剩余注册用户持久化和现代 UDP 音量语义 |
| [#25](https://github.com/XiaomaiTX/go-mumble-server/issues/25) | 外部身份权威接口 | HTTP authority、Authenticator、Resolve、失败关闭和重新鉴权已实现；原 Issue 应关闭或重写为行为差异清单 |
| [#26](https://github.com/XiaomaiTX/go-mumble-server/issues/26) | SuperUser 与 registered user ID 0 | 保留为身份和原生工具兼容性问题，不是当前首要安全漏洞 |

如果目标明确是“官方 Mumble 1.5.x drop-in replacement”，则 #15、#16、#22、#23、#26 应整体提升一个级别，作为原生客户端兼容发布计划的一部分。

### P3：可延期的集成和架构工作

| Issue | 主题 | 当前判断 |
| --- | --- | --- |
| [#20](https://github.com/XiaomaiTX/go-mumble-server/issues/20) | PluginDataTransmission | 插件和扩展兼容性，不影响普通语音 |
| [#21](https://github.com/XiaomaiTX/go-mumble-server/issues/21) | ContextAction callback | 需要先确定 Go event interface，不影响核心语音和 REST |
| [#24](https://github.com/XiaomaiTX/go-mumble-server/issues/24) | 多虚拟服务器 runtime | 单服务器产品定位下是 P3；若对外承诺 Murmur Meta 风格多服务器，则提升为 P1/P2 |

## 三、执行依赖关系

```text
ACL/Group 基础语义
  #3 ─┬─> #4 ─> #5 ─> #7/#14 ─> linked voice 回归测试
      └───────────────────────> #8/#10 权限场景验证

现代协议
  #22 Protobuf UDP Audio ─> #23 Version 协商 ─> #19 Listener 音量收口

身份与原生管理
  #25 外部身份边界 ─> #26 SuperUser 语义 ─> #15/#16/#18 原生管理协议
```

其中 #6 与 #7 应共享同一个 linked-channel connected-component 实现；#8 与 #9 应共享同一个 BanList replace 流程。

## 四、下一阶段实施计划

### 阶段 0：Issue 和文档状态校准

- 将 [#1](https://github.com/XiaomaiTX/go-mumble-server/issues/1) 标记为已由 `25f40d1` 解决，关闭或补充关闭说明。
- 将 [#17](https://github.com/XiaomaiTX/go-mumble-server/issues/17) 标记为核心问题已由 identity authority 解决；如需权限门控，另开 follow-up。
- 将 [#19](https://github.com/XiaomaiTX/go-mumble-server/issues/19) 改写为 Listener 持久化和现代 UDP 音量收口。
- 将 [#25](https://github.com/XiaomaiTX/go-mumble-server/issues/25) 改写为外部 authority 与 Murmur ServerAuthenticator 的剩余行为差异。
- 更新 [#23](https://github.com/XiaomaiTX/go-mumble-server/issues/23)，明确 `97eb8ce` 已宣告 control-channel `VersionV1=1.4.0`，当前仍不能宣告 `1.5.x`。
- 保持 [#27](https://github.com/XiaomaiTX/go-mumble-server/issues/27) 的 completed 状态。

### 阶段 1：安全基线

目标：在允许不受信任用户加入前，消除权限扩大、音频越权和信息泄露路径。

1. 重构 ACL/Group evaluator，覆盖 Traverse、Write、root-only 权限、ApplyHere、ApplySubs、继承、selector 和 Token 语义。
2. 将官方 ACL 规则整理为表驱动或差分测试，至少覆盖父级 allow/deny、子级覆盖、`@in/@out`、`@sub`、Token 和 root 管理权限。
3. 统一 linked-channel connected-component 计算，并对每个目标频道执行 Speak 权限检查。
4. 修复 BanList 查询权限和空列表 replace 语义。
5. 修复 Tree TextMessage，按最终 recipient channel 过滤，而不是只检查 tree root。
6. 为 VoiceTarget 补充 sender mute/suppress/self-mute、recipient deaf/self-deaf、Whisper 权限、断线和 session 复用测试。

阶段 1 验收标准：

- 未授权用户无法读取 BanList。
- 被子频道 ACL 拒绝的用户无法通过 linked voice 或 tree message 绕过限制。
- 父子频道 ACL 和 Group 继承结果与 Murmur 对照样例一致。
- VoiceTarget 断线、重连和 session 复用不会发生错投。

### 阶段 2：频道和 Legacy 协议收口

- 实现 Temporary Channel 的运行时生命周期和重启行为。
- 修复 ChannelState 的字段 presence，区分字段缺失与显式零值。
- 实现 ChannelState.parent 的频道重挂载，并防止循环和非法父节点。
- 按官方权限实现 LinkChannel add/remove。
- 完成 #2 的最后一轮行为验证。

阶段 2 验收标准：

- 原生客户端可以清空频道描述、设置 position 为零并正确移动频道。
- 临时频道无人后清理，重启后不恢复已销毁的临时频道。
- LinkChannel 的权限和音频连通结果一致。

### 阶段 3：Mumble 1.5 现代 UDP

只有在产品决定支持官方 Mumble 1.5.x 协议时执行本阶段。

1. 按官方 `MumbleUDP.proto` 实现 Audio 的解析和编码。
2. 支持 target、context、sender_session、frame_number、Opus 数据、positional data、volume adjustment 和 terminator。
3. 明确 Legacy UDP、Protobuf UDP、TCP UDPTunnel 三种路径的路由和回退规则。
4. 使用官方 Mumble 1.5.x 客户端进行 UDP ping、普通语音、Whisper、linked channel、listener 和断线重连测试。
5. 通过互操作测试后，再调整 #23 的 VersionV1/VersionV2 和能力宣告。
6. 最后补齐 Listener 的持久化和现代 UDP 音量语义。

在本阶段完成前，`ServerVersionV1` 应保持在 `1.4.x` 范围；不得通过单独修改版本字段制造未实现的 1.5 能力。

### 阶段 4：原生管理协议和身份模型

- 实现 ACL 消息的 groups/acls 查询和更新。
- 实现 UserList 的注册用户查询、重命名和注销。
- 将 UserStats 扩展为按权限返回证书、版本、网络和统计字段。
- 明确 `SessionID`、`RegisteredUserID`、API synthetic ID 和 SuperUser 的不同命名空间。
- 根据最终身份模型决定是否兼容 Murmur 的 registered user ID 0。

### 阶段 5：可选扩展

- PluginDataTransmission 的目标校验、限流和转发。
- ContextAction 的 Go callback/event interface。
- 真正的多虚拟服务器 runtime；如果不实施，则把 API 能力明确限制为单服务器。

## 五、代码基线和证据

- VoiceTarget 当前已经保留连接对象并在路由时重新解析频道、Group 和权限：[voicetarget.go](/D:/Projects/go-mumble-server/internal/mumble/voicetarget.go:26)。
- 普通语音仍通过直接 linked channel 列表路由，且当前只在发送者频道调用 Speak gate：[router.go](/D:/Projects/go-mumble-server/internal/audio/router.go:55)。
- BanList 查询和空列表更新仍共用不完整的长度判断：[handlers.go](/D:/Projects/go-mumble-server/internal/mumble/handlers.go:2127)。
- ACL 当前仍存在 `Write` 快速放行和简化 Group 继承：[evaluator.go](/D:/Projects/go-mumble-server/internal/acl/evaluator.go:74)。
- ChannelState 只为 parent 保留 presence，Name、Description、Position、MaxUsers 等显式零值仍无法表达：[channel.go](/D:/Projects/go-mumble-server/pkg/mumble/protocol/messages/channel.go:45)。
- Channel Listener 已标记为实现完成，但文档列出了持久化和现代 UDP 音量等已知偏差：[0009-channel-listening.md](/D:/Projects/go-mumble-server/docs/features/0009-channel-listening.md:1)。
- 外部身份接口已经抽象为 Authenticator、IdentityDirectory 和 IdentityProvider：[authority.go](/D:/Projects/go-mumble-server/internal/identity/authority.go:1)。
- 当前 runtime 启动路径固定加载 server ID 1，多服务器 API 尚未对应到多个运行实例：[server.go](/D:/Projects/go-mumble-server/internal/server/server.go:57)。

## 六、发布前最低测试门槛

- `go test ./...` 必须通过。
- ACL/Group 必须有官方 Murmur 对照样例或差分测试。
- 语音路由必须覆盖普通语音、Whisper、linked channel、listener、deaf/mute/suppress 和 session 复用。
- BanList 必须覆盖查询权限、非空替换、空列表清空和重启持久化。
- Tree TextMessage 必须覆盖父级允许、子级拒绝和多目标去重。
- 若宣告 Mumble 1.5.x，必须有官方客户端 Protobuf UDP 的黑盒互操作测试。
