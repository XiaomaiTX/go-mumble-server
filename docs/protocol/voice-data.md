# 语音数据（UDP / UDPTunnel）

状态：Legacy/Protobuf 双格式实现已接入；控制通道默认宣告 1.5.0。1.5 客户端使用 Protobuf Audio，旧客户端继续使用 Legacy；真实客户端互操作按验收清单执行。

## 协商与传输

连接在认证前根据双方版本确定音频格式：双方均达到 1.5.0 才使用 Protobuf，否则使用 Legacy。未知版本使用 Legacy。认证后固定格式，UDP 与 TCP `UDPTunnel` 共用；不能通过音频首字节猜测并切换格式。未认证服务器列表 Ping 保留两种格式识别。

UDP 默认端口 64738，与 TCP/TLS 端口一致。音频明文最多 1024 字节，限制不含加密开销。加密层与音频格式独立：OCB2-AES128、AES-256-GCM、lite 明文仍由现有加密能力协商决定。TCP 回退传送同一接收者格式的明文包；mixed-crypto 频道强制 TCP，也不会改变 wire mode。

## Legacy Opus 格式

```text
客户端：header | frame_number | payload_length | opus_data | 可选位置
服务端：header | sender_session | frame_number | payload_length | opus_data | 可选位置
```

header 高三位是 codec，Opus 为 4，Ping 为 1。低五位在客户端方向是 target，在服务端方向是 context。整数使用 Mumble 自定义 varint；`payload_length` 低 13 位是字节数，第 13 位（0x2000）是终止标记。位置数据只能不存在或恰好为三个 little-endian float32（12 字节）。本次音频入口只接受非空 Opus，不实现 CELT/Speex 转码。

| 客户端 target | 路由 |
| --- | --- |
| 0 | 当前频道及链接频道 |
| 1～30 | 预先配置的 VoiceTarget |
| 31 | 服务器回环 |

旧公开接口 `ParsePacket` 保留兼容包装，新增位置字段；生产入口使用 `DecodeClientPacket` 严格校验。

## Protobuf Audio 格式

```text
0x00 | protobuf(MumbleUDP.Audio)
```

| 字段 | 编号 | wire type | 语义 |
| --- | ---: | --- | --- |
| target | 1 | varint | 客户端指定路由目标 |
| context | 2 | varint | 服务端指定接收上下文 |
| sender_session | 3 | varint | 服务端认证的发送者身份 |
| frame_number | 4 | varint | 当前包第一帧的序号 |
| opus_data | 5 | length-delimited | 非空 Opus 数据 |
| positional_data | 6 | packed fixed32 | 可选三个位置坐标 |
| volume_adjustment | 7 | fixed32 | 接收者音量 factor；0 表示未设置 |
| is_terminator | 16 | varint | 语音流结束 |

`target` 与 `context` 构成 oneof，最后出现的成员生效。singular 重复字段取最后值。编码位置时使用 packed，解码同时接受 packed/unpacked。未知合法字段（含配对 group）跳过；非法 tag、wire type、整数溢出、截断、错误坐标数和超长包拒绝。解码结果独立拥有 Opus 字节，后续读循环可安全复用输入缓冲区。

客户端提交的 sender/context/volume 不作为授权或交付属性：入口清除 sender，路由以认证 Session 覆盖，context 和音量从服务端状态计算。出站只写 context，不写 target。终止字段的 tag 为 `80 01`，字段号不是 8。

## 路由与交付

| 来源 | Context |
| --- | --- |
| 当前频道、普通链接频道 | NORMAL（0） |
| VoiceTarget 频道及其子频道/链接目标 | SHOUT（1） |
| VoiceTarget 显式用户 | WHISPER（2） |
| 频道 Listener | LISTEN（3） |
| 服务器回环 | NORMAL（0） |

重复接收者只交付一次，context 取较小值，音量 factor 独立取较大值。未设置监听音量时采用单位增益 1；Protobuf 编码省略单位增益。Legacy 保留 context，无法携带音量字段。只有发送者与接收者 plugin context 相等时才保留位置。

现有 Mute/SelfMute/Suppress、Deaf/SelfDeaf、Speak/Whisper ACL、逐链接频道权限与 VoiceTarget 连接身份绑定继续生效。服务端不解码 Opus 内容、不重采样、不转码。

## Ping

现代 Ping 为 `0x01 | protobuf(MumbleUDP.Ping)`，字段 1 为 timestamp，字段 2 为请求扩展信息；响应扩展字段 3～6 依次为 VersionV2、在线人数、人数上限、带宽上限。连接内探测严格按 wire mode 校验，普通探测回显时间戳。mixed-crypto 频道抑制连接内 UDP Ping 回应，促使客户端切换 TCP。

## 对照与验收

- [官方 v1.5.915 MumbleUDP.proto](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/MumbleUDP.proto)：字段定义；仓库固定副本位于 `pkg/mumble/audio/testdata/MumbleUDP.proto`。
- [官方 MumbleProtocol.cpp](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/MumbleProtocol.cpp)：前缀、Legacy context、位置与单位增益处理。
- [官方 AudioReceiverBuffer.cpp](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/murmur/AudioReceiverBuffer.cpp)：重复接收者、上下文与音量合并、位置过滤。
- [实施与互操作记录](../spec/draft/0013-mumbleudp-audio-validation.md)：自动测试结果和待验收项目。
