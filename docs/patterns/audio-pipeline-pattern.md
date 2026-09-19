# 音频处理流水线

状态：双格式实现已接入，默认宣告 1.5.0；真实客户端互操作按验收清单执行。

## 数据流

```text
UDP 解密 / TCP UDPTunnel
    ↓
认证连接的 WireMode（双方版本决定，认证后固定）
    ↓
DecodeClientPacket → Frame（自有 Opus 缓冲区）
    ↓
认证 Session 覆盖身份 → 频道、ACL、VoiceTarget、Listener 路由
    ↓
Delivery（context、音量）→ 接收者 plugin context 位置过滤
    ↓
按 WireMode × Context × VolumeAdjustment × HasPosition 缓存编码
    ↓
接收者 CryptState 加密并发 UDP / 相同明文通过 TCP 回退
```

Frame 是发送者的音频事实，Delivery 是服务端生成的接收语义。路由器不接收预编码音频；旧的插入 Session 重写函数已经移除。服务器只重新封装协议，不解码或修改 Opus 内容。

## 路由规则

普通当前/链接频道使用 NORMAL；显式用户 VoiceTarget 使用 WHISPER；频道 VoiceTarget 使用 SHOUT；监听者使用 LISTEN；服务器回环使用 NORMAL。逐频道 Speak 和 Whisper ACL、发送者静音和接收者 deaf 检查保留。VoiceTarget 的显式用户绑定连接对象，防止 session 复用将耳语泄漏给新用户。

多条路径命中同一人时先去重，context 取最小值、音量 factor 取最大值，两者独立合并。仅在双方 plugin context 相等时保留位置数据。

## 编码和传输

每次路由创建独立 EncodingCache，不跨音频包缓存；同组接收者复用一次编码结果，加密仍逐连接执行。缓存键包括模式、上下文、音量的 float32 位模式和位置资格。缓存不会共享密钥、nonce 或密文。

Legacy 与 Protobuf 双向转换保留 Opus、帧号、位置与终止标记。Protobuf 前缀是 0x00，target/context 是 oneof，位置使用 packed fixed32，`is_terminator` 是字段 16。详见[语音数据](../protocol/voice-data.md)。

UDP 地址已建立时优先 UDP；没有地址、加密失败或频道处于 mixed-crypto 时，使用同一接收者格式通过 UDPTunnel 发送。UDP 连接探测根据 wire mode 解析；服务器列表探测无已认证连接，继续支持双格式。

## 验证与观测

自动测试覆盖编解码固定向量、oneof、unknown field、畸形输入、浮点位模式、四种新旧组合、UDP/TCP 入口和出口、身份覆盖、NAT 重绑定、mixed-crypto 回退、位置过滤和 Listener 音量。路由语义单元测试直接断言 Delivery。

`voice_debug` 以低频记录解析模式、传输方式、解析失败和普通路由编码组数；发送日志保留 UDP/TCP 选择，不输出音频原始字节。性能基准区分单次 Protobuf 编码和 100 个同组接收者的缓存命中路径。

正式启用 1.5 之前仍需真实客户端确认停止说话、重连、丢包/乱序、位置音频以及新旧客户端互通，不能用单元测试代替互操作证据。

## 官方源码对照

- [MumbleProtocol.cpp](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/MumbleProtocol.cpp)
- [AudioReceiverBuffer.cpp](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/murmur/AudioReceiverBuffer.cpp)
- [MumbleUDP.proto](https://github.com/mumble-voip/mumble/blob/v1.5.915/src/MumbleUDP.proto)
