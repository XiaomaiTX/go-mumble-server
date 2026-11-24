---
status: active
doc_type: index
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - AGENTS.md
  - docs/ai/repo-rules.md
  - docs/standards/documentation-governance.md
---

# go-mumble-server 文档索引

仓库文档的统一入口。这里是工程文档，不是运行中的 REST Swagger `/docs`；管理接口契约从下方 REST 入口进入。

| 你要做什么 | 从这里开始 |
| --- | --- |
| 了解项目、安装或快速启动 | [中文 README](../README.zh-CN.md) / [English README](../README.md)、[本地开发](guides/local-development.md)、[构建与测试](guides/build-and-test.md) |
| 代理或维护者接手任务 | [代理入口](../AGENTS.md) → [仓库级规则](ai/repo-rules.md) → [代理上手](ai/agent-onboarding.md) |
| 理解服务端或找代码入口 | [架构总览](architecture/overview.md)、[模块地图](architecture/module-map.md)、[运行与启动](architecture/runtime-and-startup.md)、[数据与契约](architecture/data-and-contracts.md) |
| 修改管理 API 或前端 | [REST 路由索引](api/route-index.md)、[接口约定](api/conventions.md)、[OpenAPI](../api/openapi.yaml)、[前端模式](patterns/frontend-guide.md)、[UI 样式](patterns/ui-style-guidelines.md) |
| 调整身份、授权或 ACL | [认证与权限架构](architecture/auth-and-permissions.md)、[管理员操作](guides/permissions-and-admin-guide.md)、[外部身份接入（Fuxi Seat 示例）](guides/fuxi-seat-external-identity.md)、[安全与凭据](standards/security-and-secrets.md) |
| 构建、发版或部署 | [构建与测试](guides/build-and-test.md)、[发布与部署](guides/release-or-deploy.md)、[Core / Edge](guides/core-edge-deployment.md)、[Caddy](guides/deployment-caddy.md)、[Home Assistant Add-on](../addon/README.md) |
| 做验证或排查运行问题 | [测试标准](standards/testing-and-verification.md)、[测试指南](guides/testing-guide.md)、[调试指南](guides/debugging-guide.md)、[交付前检查](standards/pre-completion-checklist.md) |
| 新增或调整文档 | [初始化指南](guides/project-docs-initialization.md)、[文档治理](standards/documentation-governance.md)、[编写规范](standards/doc-writing-standard.md)、[模板](templates/) |

## 协议与工程模式

- **Mumble**：[控制消息](protocol/control-messages.md)、[语音数据](protocol/voice-data.md)、[权限位](protocol/permissions.md)、[安全模式](protocol/security-modes.md)、[加密](protocol/encryption.md)。
- **实现约束**：[手写 wire 编码](architecture/protocol-encoding.md)、[Core / Edge 所有权](architecture/core-edge-protocol-ownership.md)、[依赖分层](standards/dependency-layering.md)、[编码约定](standards/coding-conventions.md)。
- **后端模式**：[连接生命周期](patterns/connection-lifecycle-pattern.md)、[处理器分派](patterns/handler-table-pattern.md)、[频道树](patterns/channel-tree-pattern.md)、[语音流水线](patterns/audio-pipeline-pattern.md)、[ACL 求值](patterns/acl-evaluation-pattern.md)、[并发状态](patterns/concurrent-state-pattern.md)。

## 当前、待定与历史

- **当前功能**：[功能索引](features/README.md)。行为以代码、测试和对应契约为准。
- **待定**：[草案与验收索引](spec/README.md)。草案、静态审计和未完成的互操作清单都不是已发布行为。
- **历史**：[参考资料索引](reference/README.md)保留早期总览和兼容性报告；已完成的方案与阶段记录归入归档。遇到冲突依照[仓库规则的权威顺序](ai/repo-rules.md#范围与权威顺序)。
