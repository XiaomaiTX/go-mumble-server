---
status: active
doc_type: standard
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - Makefile
  - frontend/package.json
  - frontend/eslint.config.js
---

# 编码约定

Go 按 `gofmt` 格式化，并遵循邻近 Go 包的错误处理、锁与生命周期模式。协议字段遵循[手写编码约定](../architecture/protocol-encoding.md)；并发状态与资源清理参考[并发状态模式](../patterns/concurrent-state-pattern.md)和[连接生命周期](../patterns/connection-lifecycle-pattern.md)，同时核对当前代码。

前端沿用 Vue/Vuetify 与当前组件模式，见[前端指南](../patterns/frontend-guide.md)和 [UI 样式](../patterns/ui-style-guidelines.md)。不要只为一次改动扩散新依赖、引入新抽象或批量重排不相关代码。验证命令集中在[测试标准](testing-and-verification.md)。
