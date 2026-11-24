---
status: active
doc_type: agent-guide
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - docs/ai/repo-rules.md
  - docs/README.md
---

# 代理上手指南

1. 先读[仓库级规则](repo-rules.md)，再由[文档索引](../README.md)进入任务对应分区。
2. 用[模块地图](../architecture/module-map.md)定位入口，检查实际调用链、邻近测试和未提交改动；不要从草案或历史报告推断当前功能。
3. 协议任务先读[手写编码](../architecture/protocol-encoding.md)；分布式任务先读[协议所有权](../architecture/core-edge-protocol-ownership.md)；REST/UI 任务先读[路由索引](../api/route-index.md)，再核对实现和 OpenAPI。
4. 按[测试与验证](../standards/testing-and-verification.md)选命令，按[完成门禁](../standards/pre-completion-checklist.md)交付；拿不到真实客户端或特定部署环境时，说明哪些结论尚未验证。
