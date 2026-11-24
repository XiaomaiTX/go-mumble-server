---
status: active
doc_type: standard
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - AGENTS.md
  - docs/ai/repo-rules.md
---

# 文档治理

## 单一主源

[文档索引](../README.md)是整棵文档树的导航；根目录 README 只负责项目介绍和快速上手，代理入口只指向[仓库规则](../ai/repo-rules.md)。同一个接口、运行方式或规范只维护一份权威说明，其余位置链接过去。REST 的字段契约维护在 [OpenAPI](../../api/openapi.yaml)，不在指南中重抄完整 schema。

## 状态与目录

- `active`：当前维护的规则、架构、接口、功能或指南。已实现行为放 `features/current/`，网络协议专题放 `protocol/`，可复用实现模式放 `patterns/`。
- `draft`：提案、待验证项目和开放问题，使用项目既有的 `spec/draft/`（不是上游模板的 `specs/draft/`）。不能作为当前实现的依据。
- `deprecated`：历史设计、阶段性验证或被替代的说明，使用 `spec/archive/`、`features/archived/` 或 `reference/`；标明当前入口或适用基线。保留仍被引用的记录及其上下文，不等同于重新验证了历史内容。
- `template`：`templates/` 中供复制的新文档骨架，不描述当前项目。

## 元数据与维护门禁

新建规范性文档和纳入维护的旧文档使用 YAML front matter。必填 `status`、`doc_type`、`owner`、`last_reviewed`（`YYYY-MM-DD`）和 `source_of_truth`（支撑关键事实的仓库相对路径列表）。`doc_type` 按职责选 `agent-rules`、`agent-guide`、`index`、`standard`、`architecture`、`api`、`feature`、`guide`、`reference`、`draft` 或 `template`。仅作为入口的根目录文档不要求 front matter。尚未纳入规范维护的旧专题或验收记录可以暂留原格式；实质性更新时补齐元数据。旧文档仅调整分类、链接或元数据时，不据此宣称其技术行为已重新验收。

当设计落地且行为稳定时，先在相应当前文档更新事实，随后归档旧提案；更新状态、审阅日期和入链。源代码或契约发生变化时，在同一变更中更新它的权威文档；移动页面时同时检查相对链接和片段。写法与验证见[编写规范](doc-writing-standard.md)及[完成门禁](pre-completion-checklist.md)。
