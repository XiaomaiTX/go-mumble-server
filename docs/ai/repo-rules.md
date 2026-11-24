---
status: active
doc_type: agent-rules
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - AGENTS.md
  - go.mod
  - Makefile
---

# 仓库级规则

## 范围与权威顺序

`go-mumble-server` 是 Go 实现的 Mumble 语音服务器，包含独立可导入的协议库、REST 管理接口及嵌入式 Vue 管理端。根目录的代理入口只委托给本页；[文档索引](../README.md)负责导航，不另立规则。

同一事项的文档发生冲突时，依次查阅：本页 → [工程标准](../standards/) → [当前架构](../architecture/) → [接口](../api/) → [当前功能](../features/current/) → [操作指南](../guides/) → [草案](../spec/draft/) → [模板](../templates/)。协议和工程模式专题按索引进入；归档和参考不参与裁决，草案和模板不能证明当前行为。

代码、配置、schema 和测试用来确认实际行为；[OpenAPI](../../api/openapi.yaml)和既有测试也可能体现应履行的契约。若文档与实现不一致，先读调用链、配置和测试，区分过期说明与实现缺陷；不要为了消除差异而擅自改变行为。Issue、聊天和外部资料只能提供线索，不能替代仓库证据。

## 必须遵守

1. 保持 [依赖方向](../standards/dependency-layering.md)：`pkg/mumble` 不依赖服务端 `internal`；Core 拥有权威状态，Edge 负责客户端传输，不在 Edge 重做 Core 的授权判定。
2. 控制消息和音频使用手写 wire 编码；不引入 Google protobuf 运行时或生成代码。更改字段时保留 proto2 presence、零值和兼容性语义，见[协议编码](../architecture/protocol-encoding.md)。
3. 区分 REST JWT/RBAC、Mumble 身份/频道 ACL 和 Core–Edge 信任边界；授权由服务端强制执行，遵循[安全与凭据标准](../standards/security-and-secrets.md)。
4. 接口变更同步检查生产方、schema、调用方、测试和相关文档。REST 使用 [API 约定](../api/conventions.md)；协议和 Core–Edge 变更同时检查新旧客户端或两端行为。
5. 先复用既有实现模式；不夹带无关重构，不覆盖用户已有改动。当前事实、操作指南、规划和历史记录分开，遵循[文档治理](../standards/documentation-governance.md)。
6. 每次代码变更必须做文档与测试影响检查，在交付说明中分别记录更新内容或不更新的理由。验证和跳过条件见[测试标准](../standards/testing-and-verification.md)与[完成门禁](../standards/pre-completion-checklist.md)。

## 上手

读[文档索引](../README.md)和[代理上手指南](agent-onboarding.md)，再按任务读取最相关的文档与代码。新建文档使用[模板](../templates/)；更改当前行为时更新原有权威文档，不另起平行说明。
