---
status: active
doc_type: guide
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - .github/workflows/release.yml
  - .github/workflows/preview-docker-ci.yaml
  - .github/workflows/docker-image-build.yml
  - .github/workflows/addon.yml
  - Dockerfile
  - docker-compose.example.yaml
  - addon/config.yaml
  - scripts/build-deb.sh
  - Makefile
---

# 发布与部署指南

## 先选择目标产物

发布前使用[测试与验证标准](../standards/testing-and-verification.md)的门禁并验证嵌入前端构建；本地打包命令统一见[构建与测试指南](build-and-test.md)。GitHub Actions 中几条路径的触发和产物不同：

| 路径 | 触发 | 产物与去向 |
| --- | --- | --- |
| [Release](../../.github/workflows/release.yml) | 推送 `v*` 标签 | GitHub Release：Linux amd64、arm64 两个裸二进制及对应 `.deb`。先构建嵌入前端，再在匹配架构的 `goreleaser-cross` 容器中构建。 |
| [Release 快照](../../.github/workflows/release.yml) | 手动运行 | 工作流 `deb-packages` artifact，仅上传 `.deb`，不创建 GitHub Release。 |
| [Docker Image CI](../../.github/workflows/preview-docker-ci.yaml) | `main` 推送（受工作流的路径过滤限制）或手动运行 | Docker Hub 的 `latest` 和七位提交 SHA 标签；当前配置只构建 `linux/amd64`。这不是 Git 标签发布。 |
| [Docker Image Build](../../.github/workflows/docker-image-build.yml) | 手动运行 | 导出的 `go-mumble-server.tar.gz` 工作流 artifact，不推送 registry。 |
| [Addon](../../.github/workflows/addon.yml) | 推送 `v*` 标签或手动运行 | 向 GHCR 推送 Home Assistant 的 `amd64` 和 `aarch64` add-on 镜像；标签推送时去掉前导 `v`，手动运行时从 [add-on 配置](../../addon/config.yaml)取版本（脚本缺省为 `latest`）。核对 Supervisor 所用版本与镜像标签一致。 |

本地 `make release-snapshot` 是 `make build-deb` 的别名，调用[本地打包脚本](../../scripts/build-deb.sh)生成 `dist/*.deb`，不会发布。

## 部署与验证

1. 按目标机器的架构选产物。单机容器可参考 [Compose 示例](../../docker-compose.example.yaml)的 SQLite 数据卷、TOML 挂载与端口；Debian 服务和 REST 反代见 [Caddy 部署说明](deployment-caddy.md)。分布式场景使用 [Core–Edge 部署说明](core-edge-deployment.md)，不要把单机 HTTP 探针套到 Edge。
2. 升级前确认持久数据路径、保留原产物和配置并备份 SQLite。外部身份凭据从运行环境注入，按[外部身份接入说明](fuxi-seat-external-identity.md)配置；不要存入仓库。升级后不要假定旧库能被旧二进制直接读取，回退时应根据实际变更验证原产物、配置和备份的恢复路径。
3. 部署后在实际 REST 监听地址验证 `/health`，然后打开管理 UI 并验证受影响的 API；Mumble TCP/UDP 与语音需另做客户端连接/收发验证。若使用 Core–Edge，从 Edge 入口做客户端验证，不把 Core HTTP 存活当作链路可用。
