# go-mumble-server

**[English](README.md) | 简体中文**

一个现代化的、从零开始实现的 [Mumble](https://www.mumble.info/) 语音聊天服务器，使用 Go 编写。已实现 Legacy 与 Mumble 1.5 Protobuf Audio 路径。

Mumble 协议实现是一个**可复用的 Go 库**(`pkg/mumble/`),可以独立导入,用于构建客户端、机器人、桥接器或其他工具。

**状态：Beta** —— 核心实现已完成，客户端兼容性验收仍在进行。[Releases](https://github.com/dchote/go-mumble-server/releases) 提供 Linux amd64/arm64 二进制文件和 .deb 软件包。

## 概述

go-mumble-server 以现代优先级重新构想了 Mumble 服务器:单一静态二进制文件、零运行时依赖、内置 REST 管理 API,并用 Go 简洁的并发模型取代原版的 C++/Qt 复杂性。

**TCP/TLS :64738 上的 Mumble 协议** —— 完整控制通道，使用手写 Go wire 编码。
**Mumble 语音协议** —— 支持 Legacy 和 Mumble 1.5 Protobuf Audio 封包格式；服务端不进行音频转码。
**UDP :64738 上的语音** —— 低延迟 AEAD 加密音频,支持 TCP 隧道回退。
**:64730 上的 REST 管理 API** —— 管理与监控,Swagger 文档位于 `/docs`。
**Web 管理界面** —— Vue 3 + Vuetify 前端嵌入二进制文件中,随 REST API 一起提供服务。

**逐客户端安全协商** —— Legacy(默认)、secure 或 lite。标准 Mumble 客户端自动使用 legacy;支持 secure 的客户端在可能时升级。混合模式频道(例如 legacy 与 lite 客户端共存)会自动通过 TCP 隧道中继,以确保每个客户端的加密正确性。

### 截图

![Dashboard](images/dashboard.png)

*仪表板 —— 虚拟服务器及频道数量与已连接用户。*

![虚拟服务器详情](images/virtual_server_details.png)

*虚拟服务器详情 —— 频道树、已连接用户、配置和注册用户。*

![编辑虚拟服务器](images/virtual_server_edit.png)

*编辑虚拟服务器 —— 配置名称、端口、欢迎文本和限制。*

![用户操作](images/user_actions.png)

*用户操作 —— 通过右键菜单踢出、禁言或封禁已连接用户。*

## 功能特性

### 服务器

- **完整的 Mumble 协议** —— 全部 27 种控制消息类型,UDP 和 TCP 语音传输
- **Opus 音频** —— 使用 Opus；支持 Legacy 和 Mumble 1.5 Protobuf 封包格式，不进行音频转码
- **频道层级** —— 树状结构,支持链接、临时频道和频道监听。根据 Mumble 协议,根频道 ID 恒为 0;客户端会收到完整的频道树和用户同步(包括根频道中的用户)。
- **ACL 权限** —— 基于组的访问控制,支持继承、令牌和逐频道覆盖
- **文本消息** —— 私聊、频道和全树消息,支持 HTML
- **耳语 / 语音目标** —— 定向音频发送到特定用户、频道(包括根频道)或组
- **用户管理** —— 基于证书的身份、注册和服务器密码
- **虚拟服务器** —— 单进程中运行多个逻辑服务器
- **可协商的安全层级** —— Legacy(OCB2-AES128,默认)、Secure(AES-256-GCM)或 Lite(面向受限设备的明文 UDP);通过 TLS 逐客户端协商。混合模式频道强制走 TCP 隧道中继。
- **密码安全** —— 注册用户使用 Argon2id 密码哈希,API 用户使用 bcrypt
- **REST API** —— 服务器管理、监控与集成,Swagger UI 位于 `/docs`
- **Web 管理界面** —— Vue 3 + Vuetify 前端嵌入服务器二进制文件
- **SQLite 存储** —— 用户、频道、ACL 和封禁的零配置持久化

### 协议库

- **可作为 Go 模块导入** —— `import "github.com/dchote/go-mumble-server/pkg/mumble"`
- **消息类型** —— 全部 27 种控制消息的原生 Go 结构体，以及 Legacy/Protobuf Audio 手写 wire 编码；不依赖 protobuf 运行时
- **封包成帧** —— 针对 6 字节 TCP 头格式的读写函数
- **处理器表** —— 服务器和客户端代码均可使用的消息分发基础设施
- **CryptState** —— UDP 语音封包的 AEAD 加解密(OCB2-AES128 legacy、AES-256-GCM secure 或 lite 直通)
- **音频封包** —— 解析/构建音频封包,含 varint 编码、编解码器 ID、语音目标
- **核心类型** —— `Channel`、`User`、`Permission`;ACL、封禁、语音目标和文本消息以 `protocol/messages` 结构体传输
- **无服务器依赖** —— 纯协议原语,与服务器内部零耦合

## 环境要求

- Go 1.25+
- Node.js 20+ 和 Yarn(用于前端开发)

## 构建

完整的构建与测试指南见 [docs/build-and-test.md](docs/build-and-test.md)。

### 完整构建(前端 + 服务器)

```bash
./scripts/build.sh
```

这会构建 Vue 前端、将 dist 复制到 Go embed 位置,并编译服务器二进制文件到 `build/go-mumble-server`。前端通过 `//go:embed` 嵌入二进制文件。

### 构建 Debian 软件包(.deb)

在 macOS 或 Linux 上,你可以使用 Docker 在本地构建 `.deb` 软件包(与 CI 相同):

```bash
make build-deb
# 或
./scripts/build-deb.sh
```

输出位于 `dist/*.deb`(amd64 和 arm64)。如果前端已构建,可使用 `SKIP_FRONTEND=1 ./scripts/build-deb.sh` 跳过前端步骤。详情见[构建与测试](docs/build-and-test.md#building-debian-packages-deb-on-macos-or-linux)。推送版本标签(如 `v0.1.0`)时,GitHub 上的 **Releases** 会包含 Linux amd64/arm64 二进制文件和这些 .deb 软件包。

### 仅构建服务器(跳过前端)

```bash
SKIP_FRONTEND=true ./scripts/build.sh
```

或直接:

```bash
go build -o go-mumble-server ./cmd/go-mumble-server
```

### 运行测试

```bash
CGO_ENABLED=1 go test -timeout=30s ./...
```

使用 `-timeout=30s` 避免挂起。竞态检测:`CGO_ENABLED=1 go test -race -timeout=60s ./...`

`pkg/mumble/protocol/messages` 在每次测试运行时还会对上游 `Mumble.proto` 的 vendored 副本执行 schema 一致性检查,在发布前捕获线缆格式漂移(错误的字段编号/类型,或未经甄别的上游字段)。见 [docs/architecture/protocol-encoding.md](docs/architecture/protocol-encoding.md#schema-conformance-lint)。

## 运行

```bash
# 使用默认值启动(Mumble 在 :64738,REST 在 :64730)
./go-mumble-server

# 使用配置文件
./go-mumble-server -config /path/to/mumble-server.toml

# 使用环境变量
MUMBLE_MUMBLE_PORT=64738 MUMBLE_REST_PORT=64730 ./go-mumble-server
```

## Docker 部署

构建镜像并使用 Docker Compose 运行:

```yaml
services:
  go-mumble-server:
    image: go-mumble-server:latest   # 构建命令:docker build -t go-mumble-server:latest .
    command: ["-config", "/config/mumble-server.toml"]
    ports:
      - "64738:64738/tcp"
      - "64738:64738/udp"
      - "64730:64730/tcp"
    environment:
      MUMBLE_DATABASE_PATH: /data/mumble-server.sqlite
      MUMBLE_EXTERNAL_AUTH_SERVICE_TOKEN: ${MUMBLE_EXTERNAL_AUTH_SERVICE_TOKEN:-}
      MUMBLE_IDENTITY_REVALIDATE_TOKEN: ${MUMBLE_IDENTITY_REVALIDATE_TOKEN:-}
    volumes:
      - mumble-data:/data
      - ./configs/mumble-server.toml:/config/mumble-server.toml:ro
      # - ./certs:/certs:ro          # 私有 CA 或 mTLS 证书目录
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:64730/health"]
      interval: 15s
      timeout: 5s
      retries: 3
      start_period: 5s

volumes:
  mumble-data:
```

说明:

- 外部身份部署可先执行 `cp .env.example .env`,再填写两个不同方向的服务令牌;`.env` 已被 Git 忽略。
- SQLite 数据位于 `mumble-data` 命名卷;TOML 在每次启动时读取。认证模式、URL、实例 ID、超时和证书路径统一写入 TOML,两个身份令牌通过宿主机环境、`.env`(不要提交)或容器平台 Secret 注入。修改身份配置后需要重启容器。
- 镜像以 root 启动,修复 `/data` 所有权后降权到内置 `mumble` 用户(UID/GID 999)。无论宿主机目录的所有者是谁,绑定挂载的数据目录都能开箱即用。
- 在引入此行为之前构建的镜像(≤ commit `f5e651f`)上,需要手动修复绑定挂载的数据目录:
  `mkdir -p mumble-data && sudo chown 999:999 mumble-data` —— 或改用命名卷。
- 健康检查使用镜像内置的 `curl`,访问 `/health` REST 端点(端口 64730)。

## Home Assistant 插件

你可以将 go-mumble-server 作为 [Home Assistant 插件](https://www.home-assistant.io/addons/)运行。在 Home Assistant 中添加此仓库:

1. **设置** → **插件** → **插件商店** → **仓库**
2. 添加:`https://github.com/dchote/go-mumble-server`
3. 安装 **go-mumble-server**,配置选项并启动插件。

管理 Web 界面可通过插件的 **打开 Web UI** 按钮(ingress)访问。Mumble 协议端口(默认 64738)对外开放供客户端连接。插件文档见 [addon/README.md](addon/README.md)。如果你使用此仓库的 fork 作为插件仓库,需要[更新插件镜像 URL](addon/README.md#forks) 或自行构建并推送镜像。

## 配置

配置采用**双层**模型:

1. **引导配置**(TOML 文件 / 环境变量 / 命令行标志)—— 在打开数据库之前必须确定的进程级设置:数据库路径、TLS 证书/密钥路径、日志级别,以及是否嵌入前端。
2. **数据库**(SQLite)—— 所有其他设置存储在 SQLite 中,可在运行时通过 REST API 和 Web 界面修改。首次运行时,引导值会填充数据库表(全局设置写入 `meta_config`,逐虚拟服务器设置写入 `server_configs`)。

### 引导设置(TOML / 环境变量 / 标志)

按优先级顺序从以下来源加载:命令行标志、环境变量(`MUMBLE_` 前缀)、TOML 文件、默认值。

| 设置项 | 环境变量 / 标志 | 默认值 | 说明 |
|---------|-----------|---------|-------------|
| REST 端口 | `MUMBLE_REST_PORT` | `64730` | REST API + Web 界面端口(也可在 TOML `network.rest_port` 中设置) |
| 数据库路径 | `MUMBLE_DATABASE_PATH` | `mumble-server.sqlite` | SQLite 数据库文件 |
| TLS 证书 | `MUMBLE_SSL_CERT_PATH` | (自动生成) | TLS 证书(PEM) |
| TLS 密钥 | `MUMBLE_SSL_KEY_PATH` | (自动生成) | TLS 私钥(PEM) |
| 日志级别 | `MUMBLE_LOG_LEVEL` | `info` | `debug`、`info`、`warn`、`error` |
| 前端嵌入 | `-frontend-embed` | `true` | 在 REST 端口上提供内嵌 Web 界面 |

完整的 TOML 模板见 `configs/mumble-server.toml`。TOML 文件还包含数据库设置的初始值;这些值仅在首次运行时用于填充数据库。

### 数据库设置

通过 REST API(`/api/v1/meta/config`、`/api/v1/servers/:id/config`)和 Web 界面设置页管理。

**全局**(`meta_config` 表):

| 设置项 | 默认值 | 说明 |
|---------|---------|-------------|
| 主机 | `0.0.0.0` | 绑定地址 |
| Mumble 端口 | 64738 | Mumble 协议端口(TCP + UDP) |
| REST 端口 | 64730 | REST API + Web 界面端口 |
| Bonjour | false | mDNS/Bonjour 局域网发现 |
| 注册名称 | `go-mumble-server` | 局域网发现时显示的名称 |

**逐服务器**(`server_configs` 表):

| 设置项 | 默认值 | 说明 |
|---------|---------|-------------|
| 最大用户数 | 100 | 最大并发用户数 |
| 最大带宽 | 72000 | 每用户最大带宽(bps) |
| 欢迎文本 | | 服务器欢迎消息(HTML) |
| 服务器密码 | | 服务器级密码 |
| 默认频道 | 0 | 新连接的频道 ID |
| 需要证书 | false | 要求客户端证书 |
| 频道嵌套上限 | 10 | 最大频道树深度 |
| 频道数量上限 | 1000 | 最大频道总数 |

完整配置参考见 [docs/technical-overview.md](docs/technical-overview.md)。

### 外部身份提供者

`[auth].mode` 默认为 `local`,保持本地 registered user、密码和证书登录兼容。设为 `external` 后,协议层只通过 External HTTP Identity Provider 完成认证和稳定 ID↔名称查询;provider 的 `deny` 会拒绝登录,超时、5xx 或无效响应同样 fail closed,绝不会回退到本地账户。

提供者的认证与目录 endpoint 分别由 `authenticate_path` 和 `resolve_path` 配置,默认采用 `/api/internal/mumble/v1/...`,可适配其它 HTTP 身份系统而无需修改核心代码。认证响应必须给出非零 stable user ID、canonical name 和可选的权威运行时组。客户端 access token 与权威组分别保存,权威组不持久化到 SQLite;在线会话会按 `revalidate_interval_seconds` 重验,并受 `stale_grace_seconds` 约束。详见[外部身份提供者接入文档](docs/fuxi-seat-external-identity.md)。

## Web 管理界面

服务器内置 Vue 3 + Vuetify 管理前端,嵌入 Go 二进制文件并在 REST API 端口(默认 `:64730`)上提供服务。在浏览器中打开 `http://localhost:64730` 即可访问管理界面。

功能:服务器状态仪表板、频道树管理、已连接用户列表、ACL 编辑器、封禁列表管理、服务器配置和虚拟服务器控制。

管理员、频道 ACL、`local`/`external` 身份模式和频道链接的操作说明见[管理员与频道权限说明](docs/permissions-and-admin-guide.md)。

可通过 `-frontend-embed=false` 在运行时禁用前端,适用于 headless/仅 API 部署。

## REST API

管理 API 默认运行在端口 64730(可通过配置中的 `rest_port` 或 `MUMBLE_REST_PORT` 修改)。交互式 Swagger 文档位于 `/docs`。Web 界面即为此 REST API 的消费者。关于使用 HTTPS 和 TLS 终止的生产部署,见[使用 Caddy 部署](docs/deployment-caddy.md)。

```bash
# 服务器健康状态
curl http://localhost:64730/health

# 虚拟服务器
curl http://localhost:64730/api/v1/servers

# 频道树
curl http://localhost:64730/api/v1/servers/1/channels

# 已连接用户(包含 session_id、user_id、name、channel_id、禁言状态、is_admin)
curl http://localhost:64730/api/v1/servers/1/users

# 服务器配置
curl http://localhost:64730/api/v1/servers/1/config

# 全局(meta)配置
curl http://localhost:64730/api/v1/meta/config

# 频道 ACL
curl http://localhost:64730/api/v1/servers/1/channels/0/acl

# 封禁列表
curl http://localhost:64730/api/v1/servers/1/bans

# 注册用户
curl http://localhost:64730/api/v1/servers/1/registered-users
```

## 使用协议库

`pkg/mumble/` 下的包可被任何 Go 项目导入,用于构建 Mumble 客户端、机器人或工具:

```go
import (
    "github.com/dchote/go-mumble-server/pkg/mumble/protocol"
    "github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// 连接并发送版本信息
conn, _ := tls.Dial("tcp", "localhost:64738", &tls.Config{InsecureSkipVerify: true})
protocol.WriteMessage(conn, protocol.MessageVersion, &messages.Version{
    Release:   "MyBot 1.0",
    OS:        "linux",
    OSVersion: "amd64",
})

// 读取循环 —— 通过处理器表分发
table := protocol.NewHandlerTable()
// table[protocol.MessagePing] = func(msgType, payload, ctx) error { ... }
for {
    msgType, payload, err := protocol.ReadPacket(conn)
    if err != nil {
        break
    }
    table.Dispatch(msgType, payload, conn)
}
```

该库处理成帧、原生 Go 消息编码(无 protobuf)、UDP 的 CryptState、音频封包解析,并提供所有核心 Mumble 类型。

**协议策略:** 不使用 Google protobuf。见 [docs/architecture/protocol-encoding.md](docs/architecture/protocol-encoding.md)。连接管理和处理器逻辑由你的代码提供。

完整的包布局和库/服务器边界见 [docs/technical-overview.md](docs/technical-overview.md)。

## 本地开发

如需带热重载的前端开发,请单独运行 Vite 开发服务器和 Go 后端。

**终端 1** —— 启动 Go 服务器(仅 API,不嵌入前端):

```bash
go run ./cmd/go-mumble-server -frontend-embed=false
```

**终端 2** —— 启动 Vite 开发服务器(前端热重载):

```bash
cd frontend && yarn dev
```

打开 [http://localhost:3000](http://localhost:3000)。Vite 开发服务器会将 `/api`、`/docs` 和 `/health` 代理到 `http://localhost:64730` 的 Go 服务器。

如需使用不同的后端 URL:

```bash
VITE_API_PROXY_TARGET=http://localhost:64730 yarn dev
```

## 项目结构

```
go-mumble-server/
├── cmd/
│   ├── go-mumble-server/       # 主入口 + 前端嵌入
│   │   ├── main.go
│   │   ├── embed.go             # //go:embed frontend-dist
│   │   └── frontend-dist/       # Vite 构建输出(由构建脚本复制)
│   └── test-client/            # 协议测试客户端(pkg/mumble,无外部依赖)
│       └── main.go
├── frontend/                    # ── Vue 3 + Vuetify 管理界面 ──
│   ├── src/
│   │   ├── components/          # 可复用 UI 组件
│   │   ├── composables/         # Vue 组合式 API 辅助函数
│   │   ├── layouts/             # 应用布局(已认证、默认)
│   │   ├── pages/               # 基于文件的路由页面
│   │   ├── plugins/             # Vuetify、路由、store 设置
│   │   ├── store/               # Vuex store 模块
│   │   ├── styles/              # 主题和全局样式
│   │   ├── utils/               # API 客户端、格式化工具
│   │   ├── App.vue
│   │   └── main.js
│   ├── index.html
│   ├── package.json
│   ├── vite.config.js
│   └── yarn.lock
├── pkg/
│   └── mumble/                  # ── 公共协议库 ──
│       ├── protocol/messages/   # 原生 Go 消息结构体(无 protobuf)
│       ├── protocol/            # 封包成帧、消息类型、处理器表
│       ├── crypto/              # CryptState(legacy + secure 模式)
│       ├── audio/               # 音频封包、varint、编解码器 ID
│       ├── channel.go           # Channel 类型
│       ├── user.go              # User 类型
│       └── permission.go        # 权限位掩码
├── internal/                    # ── 服务器实现 ──
│   ├── server/                  # 虚拟服务器生命周期
│   ├── mumble/                  # Mumble 协议处理器
│   ├── transport/               # TCP/TLS 和 UDP 监听器
│   ├── audio/                   # 音频路由与扇出
│   ├── channel/                 # 频道树状态 + 根频道 ID 迁移
│   ├── user/                    # 用户会话生命周期
│   ├── acl/                     # ACL 求值引擎 + 默认填充
│   ├── ban/                     # 封禁列表管理
│   ├── config/                  # 引导配置(TOML)+ 数据库配置加载
│   ├── database/                # SQLite 持久化 + 模型
│   ├── handler/                 # REST API 路由处理器
│   └── rest/                    # REST 路由 + SPA 服务
├── api/
│   └── openapi.yaml             # OpenAPI 3.0 规范
├── configs/
│   └── mumble-server.toml       # 默认配置
├── scripts/                     # 构建、打包和工具脚本
├── docs/                        # 文档
├── research/                    # 参考实现
├── go.mod
└── LICENSE
```

## 文档

### 核心

- [产品概述](docs/product-overview.md) —— 项目愿景与功能摘要
- [技术概述](docs/technical-overview.md) —— 架构、子系统和设计决策
- [使用 Caddy 部署](docs/deployment-caddy.md) —— 反向代理设置(生产环境推荐)

### 协议

- [控制消息](docs/protocol/control-messages.md) —— TCP 消息目录(类型 0–26)
- [协议编码](docs/architecture/protocol-encoding.md) —— 原生 Go 线缆编码、proto2 字段存在性、快照与增量回显规则、schema 一致性检查
- [语音数据](docs/protocol/voice-data.md) —— UDP 音频封包格式与路由
- [安全模式](docs/protocol/security-modes.md) —— 逐客户端加密层级(legacy、secure、lite)与混合模式强制
- [加密](docs/protocol/encryption.md) —— TLS、AEAD 密码、密码哈希
- [权限](docs/protocol/permissions.md) —— 权限位掩码定义

### 前端

- [前端指南](docs/patterns/frontend-guide.md) —— Vue 3 与 Vuetify 约定
- [UI 样式指南](docs/patterns/ui-style-guidelines.md) —— 视觉设计规范

### 模式

- [连接生命周期](docs/patterns/connection-lifecycle-pattern.md) —— 从客户端连接到断开
- [处理器表](docs/patterns/handler-table-pattern.md) —— 按类型 ID 分发消息
- [频道树](docs/patterns/channel-tree-pattern.md) —— 层级化频道管理
- [音频流水线](docs/patterns/audio-pipeline-pattern.md) —— 语音路由与转发
- [ACL 求值](docs/patterns/acl-evaluation-pattern.md) —— 权限解析算法
- [并发状态](docs/patterns/concurrent-state-pattern.md) —— goroutine 安全的共享状态

## 测试客户端

`cmd/test-client/` 中的测试客户端使用 `pkg/mumble`(无外部 Mumble 库)验证频道同步和连通性:

```bash
go build -o test-client ./cmd/test-client
./test-client localhost:64738
```

## 客户端兼容性

go-mumble-server 兼容任何实现标准 Mumble 协议的客户端:

- [Mumble](https://www.mumble.info/)(桌面端 —— Windows、macOS、Linux)
- [Mumla](https://f-droid.org/packages/se.lublin.mumla/)(Android,F-Droid)—— 基于 Humla;禁言 UI 遵循服务器回显的 `self_mute`/`self_deaf`
- Plumble 及其他基于 Humla 的 Android 客户端
- 基于 `pkg/mumble/` 构建的自定义 Go 客户端

Murmur 快照与增量回显规则及 Humla 禁言语义见 [docs/features/0007-userstate-field-presence.md](docs/features/0007-userstate-field-presence.md),与之配套的授权规则、内容限制和录制策略见 [0008](docs/features/0008-userstate-authorization-and-limits.md)。

## 许可证

MIT 许可证 —— Copyright (c) 2026 Daniel Chote。见 [LICENSE](LICENSE)。
