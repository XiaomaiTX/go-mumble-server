---
status: active
doc_type: guide
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - go.mod
  - Makefile
  - scripts/build.sh
  - cmd/go-mumble-server/main.go
  - internal/config/config.go
  - configs/mumble-server.toml
  - frontend/package.json
  - frontend/vite.config.js
---

# 本地开发指南

## 选择启动方式

仅改 Go、协议或 REST 时，先用不嵌入前端的独立模式；改管理端时，在同一后端上另跑 Vite。验证最终嵌入包时，再按[构建与测试指南](build-and-test.md)构建带 `embed_frontend` tag 的二进制。Go 版本以 [go.mod](../../go.mod)和实际依赖要求为准，SQLite 路径需要 CGO/C 工具链；前端按 [CI](../../.github/workflows/ci.yml)使用 Node.js 20 和 Yarn。

## 本机启动与最小验证

从仓库根目录运行；单独使用临时 SQLite，避免开发操作写入已有数据：

```bash
local_data=$(mktemp -d)
MUMBLE_DATABASE_PATH="$local_data/mumble-server.sqlite" MUMBLE_HOST=127.0.0.1 \
  CGO_ENABLED=1 go run ./cmd/go-mumble-server \
  -config configs/mumble-server.toml -mode standalone -frontend-embed=false
```

在另一个终端用 `curl -fsS http://127.0.0.1:64730/health` 确认 HTTP 可达；示例配置的 Mumble 客户端端口为 `64738`（TCP 和 UDP）。`/health` 只证明 HTTP 活着；要验证语音，用客户端实际连接和通话。独立模式示例使用本地身份；外部 token 只按需配置，参考[环境变量示例](../../.env.example)和[外部身份接入](fuxi-seat-external-identity.md)。

若开发管理 UI，在另一个终端执行：

```bash
cd frontend
yarn install --frozen-lockfile
VITE_API_PROXY_TARGET=http://127.0.0.1:64730 yarn dev
```

打开 Vite 输出的地址（配置端口 `3000`）。[Vite 配置](../../frontend/vite.config.js)将 `/api`、`/docs` 和 `/health` 代理到后端。端口改动时同步修改代理目标，不要把前端热更新服务当作嵌入包验证。

## 启动不符预期

- 先读启动日志，确认模式、配置文件、生效监听地址和 SQLite 路径。[示例 TOML](../../configs/mumble-server.toml)说明哪些设置首次写入数据库，哪些仍是进程级配置；修改旧实例的 TOML 不等于覆盖已有数据库设置。
- 无嵌入构建的 `/` 不是管理页面；需要 Vite 或使用完整构建。构建失败和 CGO 工具链排查见[构建与测试](build-and-test.md)，局部行为排查见[调试指南](debugging-guide.md)。
