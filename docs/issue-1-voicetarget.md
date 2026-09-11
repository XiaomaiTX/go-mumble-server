# Issue #1：VoiceTarget 旧会话复用导致私聊错投

## 验证结果

原实现可以复现：A 将目标 1 指向 B，B 断线后 C 复用其 session ID，A 的旧目标会返回 C。新增的 `TestVoiceTargetSessionReuse` 在修复前明确失败，报错为“旧私聊目标被重定向到复用 session ID 的新用户”。

同时确认两个相关问题：目标被提前展开成静态接收者列表，无法随频道成员及权限变化更新；协议层丢失 `channel_id` 的存在性，导致仅含 session 的私聊目标意外包含根频道。

## 修复方式

- 保存目标频道、子频道、链接和组的语义，每个语音包重新计算接收者，并检查当前 Whisper 权限、组、令牌和静音状态。
- 将显式 session 目标绑定到具体连接身份。新连接即使使用相同 session ID，也无法继承旧目标；目标拥有者同样绑定连接。
- 解析时固定接收连接及 UDP 地址，发送时不再按 session ID 二次查找，从而覆盖“解析后、发送前断线重连”的窗口。
- 保留 `channel_id` 存在性；显式根频道需要 `HasChannelID: true`，非零频道仍可直接指定 `ChannelID`。嵌套目标解析错误向上传递。
- 在连接锁下发布不可变目标定义，避免目标更新和断线清理交错。

## 回归覆盖

测试覆盖 session 复用、实际 TCP 语音转发、解析与发送之间发生重连、发送者断线、仅私聊与显式根频道、清空目标、频道移动、ACL 修改、组和令牌变化、子频道及多跳循环链接、静音/耳聋过滤，以及并发连接注册、注销和路由。协议测试覆盖字段存在性及嵌套消息错误。

针对性验证命令：

```powershell
go test ./internal/mumble ./pkg/mumble/protocol/messages -run '^TestVoiceTarget' -count=1
go test -race ./internal/mumble -run '^TestVoiceTarget' -count=1
go test -race ./internal/user ./pkg/mumble/protocol/messages
```

## 本地验证结果

- 10 项新增目标及协议回归测试通过，包含并发场景的 race 检测通过。
- `internal/user` 和协议消息包的完整 race 测试通过，相关包的 `go vet` 通过。
- `git diff --check` 通过，全部变更文件通过 UTF-8 无 BOM 检查。
- 最终 `go test ./...` 未全绿：6 个包共报告 33 次 SQLite 临时文件占用导致的清理失败。抽取原始 HEAD 的 ACL 测试执行后也复现同一错误，属于既有测试未关闭数据库的 Windows 兼容问题。本次新增数据库测试均显式关闭连接。完整本地日志保存在被 Git 忽略的 `build/issue-1-full-test.log`。

## 范围与代价

本次不引入长期接收者缓存。私聊路径逐包查询当前 ACL，复杂频道/组目标会增加数据库查询开销；后续性能优化应使用完整的状态版本或失效机制，不能恢复以 session ID 快照为权威的设计。

当前项目尚未实现频道监听者状态（UserState 中相关字段会被清除），本次不新增监听功能。若将来加入，需要将当前监听者纳入动态解析并同样绑定发送连接。

参考：[Issue #1](https://github.com/XiaomaiTX/go-mumble-server/issues/1)、[Murmur v1.5.915 目标定义处理](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/murmur/Messages.cpp)、[Murmur v1.5.915 动态目标解析](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/murmur/Server.cpp)。本次参考其目标语义与权限检查，采用逐包解析而非复刻官方缓存。
