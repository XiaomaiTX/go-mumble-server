---
status: active
doc_type: guide
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - AGENTS.md
  - docs/README.md
  - docs/ai/repo-rules.md
  - docs/standards/testing-and-verification.md
  - docs/standards/documentation-governance.md
---

# 项目文档初始化指南

本指南基于 [XiaomaiTX/harness-docs-builder](https://github.com/XiaomaiTX/harness-docs-builder) 的[初始化指南](https://github.com/XiaomaiTX/harness-docs-builder/blob/d62fe3b3791b0a8690f6c1bd8f31f03152a25982/docs/guides/project-docs-initialization.md)，并按 `go-mumble-server` 的目录和维护方式适配。用于为现有模块补齐文档入口，或在其他由人和 AI 共同维护的项目中复用这套结构；不用于把模板占位文字当作项目事实。

## 初始化顺序

1. 先让根目录的[代理入口](../../AGENTS.md)指向[仓库规则](../ai/repo-rules.md)，用[文档索引](../README.md)导航，不在多个入口重复规则。
2. 明确[文档治理](../standards/documentation-governance.md)与[测试和验证标准](../standards/testing-and-verification.md)，包括权威顺序、元数据与实际验证门禁。复用到新项目时，根据它的构建入口和 CI 填写，不照搬本项目命令。
3. 读实际入口、路由、契约、配置、测试和发布工作流，再补当前架构、[HTTP 接口](../api/route-index.md)和操作指南。当前实现、实际发布能力与规划要分开；遇到差异，留下可定位的证据，不补推测。
4. 已实现的能力写在[当前功能](../features/current/)；未落地的设计留在[草案](../spec/draft/)（本项目用单数 `spec`）。已有专题内容优先迁移或重用，更新入链，不并行维护另一份契约、命令清单或故障步骤。

## 草案如何转为当前文档

先确认实现、契约和测试确实覆盖要声明的行为，再把适用内容更新到对应架构、接口或当前功能文档；必要时新建当前文档。按治理标准更新 `status`、`doc_type`、`last_reviewed`、`source_of_truth`，处理旧引用，未实现事项继续留在草案。`source_of_truth` 用仓库相对路径，正文链接按当前文件位置写相对路径。
