---
status: active
doc_type: standard
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - docs/ai/repo-rules.md
  - docs/standards/testing-and-verification.md
  - docs/standards/documentation-governance.md
---

# 交付前检查清单

- [ ] 范围符合任务；未覆盖用户已有改动，未夹带无关重构或生成物。
- [ ] **文档影响**：对照生产代码、配置、schema 或测试，更新受影响的功能、接口、架构或指南；相对链接、锚点和 front matter 正确，草案不冒充当前事实。
- [ ] **测试影响**：为行为变更新增或更新回归测试，并运行[适用验证](testing-and-verification.md)；没有新增测试时说明理由。
- [ ] **边界**：REST 同步 OpenAPI 与前端消费者；协议与 Core–Edge 同步双方契约；权限变更验证拒绝路径和脱敏，不改变无关的信任边界。
- [ ] **交付说明**：列出变更与实际验证结果，分别说明文档和测试更新内容（或不更新原因）；对每个未运行的必要验证记录原因与补验步骤。

纯文档任务至少检查范围、元数据、链接与所引用的实现证据，不要求执行与变更无关的产品测试。
