# MumbleUDP Audio

## 状态：已实现

服务端已支持 Legacy 和 Mumble 1.5 Protobuf Audio 两种封包格式。控制通道默认宣告 1.5.0；1.5 客户端使用 Protobuf Audio，旧客户端继续使用 Legacy。

## 功能范围

- UDP 与 TCP `UDPTunnel` 使用同一连接级音频格式协商结果。
- 入站音频统一解析为 `Frame`，出站按接收者生成 `Delivery` 并重新编码。
- 支持普通语音、Linked Channel、VoiceTarget、Channel Listener 和服务器回环。
- 出站交付携带 `NORMAL`、`SHOUT`、`WHISPER`、`LISTEN` 上下文及 Listener 音量。
- 发送者身份由认证连接确定；位置音频只在双方 plugin context 相同时保留。
- 支持 Legacy/Protobuf 互转，不解码、不重采样、不转码 Opus 内容。

## 兼容性

Legacy 客户端继续使用 Legacy Audio；Mumble 1.5 客户端使用 Protobuf Audio。mixed-crypto 频道可以强制 TCP 回退，但不会改变接收者协商的音频格式。

字段定义、编解码约束、自动验证和真实客户端验收清单见[实施与验收记录](../spec/draft/0013-mumbleudp-audio-validation.md)。
