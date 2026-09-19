# MumbleUDP Audio 实施与验收记录

更新时间：2026-09-19

## 当前结论

Legacy 与 Protobuf Audio 的服务端实现已完成并接入 UDP、TCP `UDPTunnel`、路由和接收者侧编码。控制通道现在默认宣告 1.5.0；1.5 客户端使用 Protobuf Audio，旧客户端继续使用 Legacy。真实客户端互操作证据仍需按下方清单补录。

## 已完成的实现

- 增加统一的 `Frame`、`Delivery`、`WireMode` 和单次路由 `EncodingCache`。
- 实现官方 `MumbleUDP.Audio` 的手写 wire 编解码：`0x00` 前缀、oneof、packed/unpacked 位置数据、字段 16 终止标记、未知字段跳过和 1024 字节上限。
- 重构 Legacy Opus 双向编解码，保留帧号、终止标记和三个 float32 位置坐标。
- UDP 与 TCP `UDPTunnel` 按连接协商模式解析，不通过首字节猜测已认证连接的格式。
- 出站按接收者的模式、上下文、音量和位置资格编码；四种 Legacy/Protobuf 组合都经过自动覆盖。
- 路由生成 `NORMAL`、`SHOUT`、`WHISPER`、`LISTEN`，合并重复接收者时保留较小上下文和较大音量 factor。
- 服务端覆盖入站 sender session；位置音频仅在双方 plugin context 相等时保留；mixed-crypto TCP 回退不改变 wire mode。
- 保留现有加密层，不引入 `google.golang.org/protobuf` 运行时依赖，不对 Opus 内容解码或转码。

## 自动验证记录

已完成的自动验证包括：

- `go test ./...`。
- 音频路由、连接协商和服务器处理器的 `go test -race`。
- Protobuf 与 Legacy 解码器各 20 秒 fuzz 冒烟。
- 官方 `v1.5.915` `MumbleUDP.proto` 固定副本、字段号和 wire type 检查。
- Protobuf golden vector、未知字段、oneof 重复字段、浮点 bit 保真、截断/溢出/超长输入和输入缓冲区复用测试。
- Legacy/Protobuf 双向转换、UDP/TCP 入口出口、NAT 重绑定、mixed-crypto 回退、身份覆盖、Listener 音量、位置过滤和 VoiceTarget 交付元数据测试。

本次文档收尾不重复执行测试；上述结果来自本次实现过程中的已完成验证。

## 本机客户端验收方式

构建并启动本机验收实例：

```powershell
go build -o build/audio-interop/go-mumble-server.exe ./cmd/go-mumble-server
build/audio-interop/go-mumble-server.exe -config build/audio-interop/server.toml
```

Mumble Desktop 1.5.915 连接 `127.0.0.1:64738`，接受自签名证书，用户名自定，密码留空。服务端日志中的 `wire_mode=1` 表示该连接已协商 Protobuf。管理前端地址为 `http://127.0.0.1:64730`；首次使用需要在登录页创建管理账号。

本机验收至少应覆盖：两个 1.5 客户端双向语音、服务器回环、停止说话、重连、位置音频、Listener 音量、VoiceTarget、UDP 不可用时的 TCP 回退，以及一个旧版 Legacy 客户端与 1.5 客户端同时在线。

## 实际验收清单

每项都记录客户端版本、传输方式、服务端日志和结果；通过标准是“双方都能听见预期音频，未命中方保持静默，服务端无解析或加密错误”。

### 连接与版本

- [ ] Mumble Desktop 1.5.915 连接成功，服务端 Version 同时包含 `VersionV1=1.5.0` 和 `VersionV2=1.5.0`。
- [ ] 服务端日志显示 1.5 客户端 `wire_mode=1`；抓包确认 Audio 明文以 `0x00` 开头。
- [ ] 旧版官方客户端连接成功，日志显示 `wire_mode=0`，仍能正常收发 Legacy 音频。
- [ ] 1.5 客户端和旧客户端同时在线时可以互相收听。

### 音频路径

- [ ] 1.5 客户端 UDP 普通语音双向收发。
- [ ] 1.5 客户端 UDP Ping 成功，停止说话后终止包到达且播放端停止残留声音。
- [ ] 禁用或阻断 UDP 后，TCP `UDPTunnel` 仍能双向收发，且接收者格式不被改成另一种 wire mode。
- [ ] mixed-crypto 频道强制 TCP 后，1.5 客户端仍收到 Protobuf，旧客户端仍收到 Legacy。
- [ ] 重新绑定客户端 UDP 端口后，服务端重新识别发送者并继续转发。

### 路由语义

- [ ] 普通当前频道和 Linked Channel 使用 `NORMAL`，不越过频道权限。
- [ ] VoiceTarget 显式用户使用 `WHISPER`；频道、子频道和链接目标使用 `SHOUT`。
- [ ] Channel Listener 使用 `LISTEN`，音量 factor 与客户端设置一致。
- [ ] Loopback 只回送发送者，context 为 `NORMAL`。
- [ ] Deaf/SelfDeaf、Mute/SelfMute/Suppress、Speak/Whisper ACL 均按预期阻断音频。
- [ ] 同一接收者通过多条路径命中时只收到一份音频，context 取较小值、音量取较大值。

### 位置与安全

- [ ] 发送者和接收者 plugin context 相同，Protobuf 包保留三个位置坐标。
- [ ] plugin context 不同，出站包移除位置字段。
- [ ] 客户端伪造 `sender_session`、`context` 或 `volume_adjustment` 不改变服务端交付结果。
- [ ] 空 Opus、截断包、错误 wire type 和超过 1024 字节的包被丢弃，服务端不 panic。

### 证据与回滚

- [ ] 保存一次 1.5 UDP、一次 Legacy UDP、一次 TCP 回退的抓包或日志片段（不包含密钥和 Opus 原始内容）。
- [ ] 记录客户端版本、服务器 commit、测试时间、频道拓扑和每项结果。
- [ ] 若现代路径异常，确认回退到 1.4.x 后旧客户端恢复 Legacy；回滚不改数据库和用户数据。

## 尚未关闭的验收项

- [ ] Mumble Desktop 1.5.915 实际 UDP 双向语音抓包和持续说话验证。
- [ ] 旧版官方客户端 Legacy UDP/TCP 与 1.5 客户端互通。
- [ ] Mumla/Plumble 或项目支持的 Android 客户端互通。
- [ ] 丢包、乱序、断线重连和真实网络下 UDP/TCP 切换。
- [x] 控制通道默认宣告 `VersionV1=1.5.0` 和 `VersionV2=1.5.0`。

真实客户端清单完成后，才可以关闭计划中的 #22/#23，并将抓包、客户端版本和结果补入本记录。
