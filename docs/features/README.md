---
status: active
doc_type: index
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - docs/features/current/
  - docs/features/archived/
---

# 功能索引

| 当前功能 | 说明 |
| --- | --- |
| [0001 REST 和管理端](current/0001-api-frontend-servers-channels-config-db.md) | 服务器记录、频道、用户、ACL 和持久化配置；当前实时实例边界见[架构总览](../architecture/overview.md)。 |
| [0002 协商安全模式](current/0002-negotiated-security-modes.md) / [0003 频道约束](current/0003-channel-security-mode-enforcement.md) | 每连接协商与频道级约束。 |
| [0004 语音诊断](current/0004-voice-path-debugging.md) | 语音路径日志与诊断设置。 |
| [0005 Home Assistant Add-on](current/0005-homeassistant-addon.md) | 功能与发布结构；安装步骤在 [Add-on README](../../addon/README.md)。 |
| [0007 UserState 字段存在性](current/0007-userstate-field-presence.md) / [0008 授权与限制](current/0008-userstate-authorization-and-limits.md) | 状态同步、静音语义、操作范围与内容限制。 |
| [0009 频道监听](current/0009-channel-listening.md) | 监听订阅、语音投递与兼容边界。 |
| [0010 MumbleUDP Audio](current/0010-mumbleudp-audio.md) | Legacy 和 Protobuf 音频支持；客户端验收另见[互操作记录](../spec/draft/0013-mumbleudp-audio-validation.md)。 |

历史：[0006 依赖维护版本记录](archived/0006-dependency-update-maintenance.md)不定义当前工具链或发布版本。
