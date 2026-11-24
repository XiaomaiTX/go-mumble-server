---
status: active
doc_type: guide
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - docs/standards/testing-and-verification.md
  - Makefile
  - .github/workflows/ci.yml
  - internal/rest/router_security_test.go
  - internal/rest/seat_revalidation_test.go
  - internal/mumble/handlers_audio_test.go
  - internal/app/distributed_integration_test.go
  - frontend/package.json
---

# 测试指南

## 选对验证层

[测试与验证标准](../standards/testing-and-verification.md)规定门禁和报告要求；[构建与测试指南](build-and-test.md)收录具体构建方式。本页只说明怎样选最接近风险的测试层，避免为纯逻辑错误引入不必要的整栈依赖。

| 改动 | 优先验证 |
| --- | --- |
| 协议字段、帧或兼容性 | 在 [协议实现包](../../pkg/mumble/protocol/)补编码/解码用例，覆盖边界值和需要保留的字段 presence；再按受影响的客户端行为复验。 |
| REST 路由、认证和角色 | 在 [REST 路由测试](../../internal/rest/router_security_test.go)所在的包验证路由分组和允许/拒绝行为，逐操作对照 [路由索引](../api/route-index.md)及规范；独立身份 token 的测试有[现成入口](../../internal/rest/seat_revalidation_test.go)。 |
| Mumble 音频传输或转发 | 从 [音频传输矩阵](../../internal/mumble/handlers_audio_test.go)找对应路径，区分 UDP 和 TCP tunnel，增加命中根因的回归。 |
| Core–Edge 编排或跨进程语义 | 先在对应包隔离根因，再按需要跑 [远端 Edge 集成测试](../../internal/app/distributed_integration_test.go)。 |
| 管理前端 | [包脚本](../../frontend/package.json)提供 `yarn lint` 和 `yarn build`，没有 `test` 脚本；行为改动还应启动 [本地前端与 Go 后端](local-development.md)走受影响的页面操作，而不是只看构建结果。 |

## 从局部到门禁

1. 先运行最小包或具体测试。例如仅复验管理角色：`CGO_ENABLED=1 go test -count=1 ./internal/rest -run '^TestRouterRoleEnforcement$'`。如果用例卡住，用 `-v`、`-timeout` 和 `-run` 定位，不用增加等待掩盖阻塞。
2. 代码改动按标准扩大验证范围。[Makefile](../../Makefile)的 `make check` 串起格式、vet 及开启 CGO 的 race 测试；race 包范围为 `./cmd/... ./internal/... ./pkg/...`。手动触发的 [CI](../../.github/workflows/ci.yml)还会安装依赖并构建嵌入前端产物。
3. 记录实际命令、结果及任何未执行项。行为失败与环境/工具链失败分别说明；构建通过不能替代业务验证。
