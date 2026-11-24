---
status: active
doc_type: architecture
owner: engineering
last_reviewed: 2026-11-24
source_of_truth:
  - cmd/go-mumble-server/main.go
  - internal/config/config.go
  - internal/config/dbconfig.go
  - internal/app/app.go
  - internal/app/local.go
  - internal/app/edge.go
  - internal/app/distributed_tls.go
  - internal/server/server.go
  - internal/app/distributed_integration_test.go
---

# 运行与启动

## 范围

说明配置来源、三种模式的进程装配与生命周期。操作步骤见[构建与测试](../guides/build-and-test.md)、[Core/Edge 部署](../guides/core-edge-deployment.md)和 [Caddy 部署](../guides/deployment-caddy.md)；验证要求见[测试与验证标准](../standards/testing-and-verification.md)。

## 当前实现

### 配置与装配顺序

1. [main.go](../../cmd/go-mumble-server/main.go) 解析参数。[配置加载器](../../internal/config/config.go)先设默认值，再读取 `-config` 显式指定的 TOML，随后应用受支持的 `MUMBLE_*` 环境变量。已通过加载阶段校验的模式可再用 `-mode` 覆盖；`-frontend-embed` 控制是否尝试加载嵌入的 SPA。缺少前端资源不会阻止服务启动。
2. [app.Build](../../internal/app/app.go) 先按最终模式校验必要依赖，再选择运行图。`standalone` / `core` 打开 SQLite 并迁移模型，进入 [Server.Prepare](../../internal/server/server.go)；`edge` 直接走[接入侧装配](../../internal/app/edge.go)，不读取数据库。
3. `Server.Prepare` 确保 `meta_config` 的 ID `1` 存在，然后读取数据库中的 meta 配置和虚拟服务器 `1` 的配置，合成运行配置，补齐默认虚拟服务器和 per-server 配置行。已落库的网络/JWT 和服务设置以数据库合成结果为准。注意：缺少 `server_config` 行时，[加载函数](../../internal/config/dbconfig.go)先返回 `DefaultServerConfig()`；随后按已合成的值建行，不应假定 TOML `[server]` 会直接覆盖这些设置。
4. 本地运行图构建权威 Core 和本地 Edge；配置了成对的 Mumble TLS 文件则走文件路径，否则从数据库获取或生成虚拟服务器证书。`Prepare` 绑定 Mumble TCP/TLS 与 UDP，再接线 HTTP 管理平面。`edge` 也在装配时绑定客户端端口，但客户端证书走[文件加载或自签名生成](../../internal/transport/tls.go)，不使用数据库。服务循环由 `Run` 启动。

### 模式与监听

| 模式 | 进程内组件与监听 |
| --- | --- |
| `standalone` | Core、Local Edge、SQLite 和 REST/可选 SPA。默认 Mumble TCP/TLS 和 UDP 同用 `64738`，HTTP 管理端口为 `64730`。 |
| `core` | 保留 `standalone` 的运行图，并将 `RemoteEdgeServer` 接到实际会话注册表；显式配置 `[distributed].edge_listen` 才打开 Edge 接入监听。 |
| `edge` | 共享客户端接入、Edge 运行时、远程 Core 客户端。没有 SQLite、REST 或本地业务 Core；必须设置 Core 地址、Edge ID 和 mTLS 客户端凭据。 |

[Core/Edge mTLS](../../internal/app/distributed_tls.go)与客户端 Mumble TLS 是两套独立连接和身份。Core/Edge 链路支持注册、心跳和重连；无法连接 Core 时 Edge 不回退到本地认证。信号或运行退出后，入口取消运行上下文并调用带 10 秒超时上下文的 `Shutdown`；本地运行图关闭 HTTP、客户端接入及可选 mDNS，应用再释放数据库句柄。

## 关键入口

- [模式校验](../../internal/config/config.go)、[本地模式装配](../../internal/app/local.go)、[Edge 模式装配](../../internal/app/edge.go)、[Server 生命周期](../../internal/server/server.go)。
- [模式装配测试](../../internal/app/app_test.go)断言实际依赖图；[跨进程链路集成测试](../../internal/app/distributed_integration_test.go)通过真实 mTLS 链路接入远程 Edge，验证 Mumble 登录、初始同步及 Core 会话激活。

## 不变量

- 先校验模式依赖，再打开数据库或绑定客户端端口。`core` 模式本身不等于已打开远程 Edge 监听。
- `edge` 不承载管理平面；数据库运行配置只参与 `standalone` / `core` 装配。
- 可用性检查要匹配对应平面：HTTP `/health` 只返回管理进程的简单状态，不验证客户端登录或远程 Edge 链路。
