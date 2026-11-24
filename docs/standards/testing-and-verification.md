---
status: active
doc_type: standard
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - Makefile
  - go.mod
  - .github/workflows/ci.yml
  - frontend/package.json
  - scripts/build.sh
---

# 测试与验证标准

默认从仓库根目录运行。Go 版本以 [go.mod](../../go.mod) 为准，CI 使用 Go 1.25；SQLite 需要 CGO 和 C 编译器，Linux CI 安装 `libsqlite3-dev`。前端跟随 CI 使用 Node 20 和 Yarn，按锁文件安装。

| 变更范围 | 验证 |
| --- | --- |
| Go 后端 / 协议 | `make check`（gofmt 检查、vet、带竞态检测的测试）；需要定点排查时用 `CGO_ENABLED=1 go test -timeout=30s ./internal/<package>` 或对应 `pkg` 路径，换成实际包名。仅服务端构建用 `SKIP_FRONTEND=true ./scripts/build.sh`。 |
| 管理前端 | 在 `frontend/` 运行 `yarn install --frozen-lockfile`、`yarn lint` 和 `yarn build`；当前没有独立 test/typecheck 脚本，对实际改动做浏览器交互验证。 |
| 嵌入、打包或部署 | 使用 `./scripts/build.sh` 验证带 `embed_frontend` 的全量构建，并按[发布与部署指南](../guides/release-or-deploy.md)做对应产物或拓扑的 smoke test。 |
| 仅文档 | `git diff --check`；核对新增和改动页面的 front matter、适用状态、相对路径与锚点。命令和当前行为对照对应脚本、配置、路由或测试。纯文档修改可跳过产品构建和测试。 |

新功能和修复应新增或更新能锁定实际行为的回归测试。REST 变更验证 handler/权限和管理端消费，协议变更验证 wire 字段存在性、往返和旧客户端行为；Core–Edge 变更验证本地与远端边界。涉及权限或敏感字段，必须验证拒绝路径。构建、lint 和 schema 检查不能代替行为测试；自动化通过也不代表真实 Mumble 客户端验收通过。

工具链或测试环境缺失时，列出已运行的命令、未验证项、原因和可复现的后续步骤。操作示例见[测试指南](../guides/testing-guide.md)和[构建指南](../guides/build-and-test.md)。
