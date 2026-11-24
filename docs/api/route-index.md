---
status: active
doc_type: api
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - internal/rest/router.go
  - internal/rest/mumble_adapter.go
  - internal/handler/server.go
  - api/openapi.yaml
  - api/spec.go
---

# HTTP 路由索引

## 从哪里查

[Go 路由注册](../../internal/rest/router.go)给出实际路径和中间件分组；[OpenAPI](../../api/openapi.yaml)给出已描述操作的请求/响应契约，通过[编译时嵌入](../../api/spec.go)原样发布。下表按用途和信任边界归组，不复制 schema。认证、错误和同步要求见 [HTTP API 约定](conventions.md)。

| 路径或分组 | 方法与用途 | 访问条件 | OpenAPI |
| --- | --- | --- | --- |
| `/health` | `GET`，HTTP 存活探针 | 公开 | 有 |
| `/docs`、`/api/v1/openapi.yaml` | `GET`，Swagger UI 与规范文件 | 公开；Swagger UI 从 CDN 加载资源 | 不列作业务操作 |
| `/`、`/*` | `GET`，嵌入前端的资源或 SPA 回退；无前端时只注册 `/` 文本提示 | 公开；SPA 回退排除 `/api/` | 不列作业务操作 |
| `/internal/identity/v1/revalidate` | `POST`，触发在线身份重验 | 独立身份重验 token；不是管理 JWT | 有 |
| `/api/v1/auth/status` | `GET`，查询认证状态 | 可选 JWT | 有 |
| `/api/v1/auth/login`、`/api/v1/auth/register` | `POST`，登录或注册 Web 账户 | 不要求 JWT；共享 IP 限流 | 有 |
| `/api/v1/status` | `GET`，服务器及频道统计 | JWT | **未覆盖** |
| `/api/v1/servers`、`/api/v1/servers/{id}` | `GET`，列表或详情；`POST` 集合，`PATCH`/`DELETE` 单项 | `GET` 需 JWT；其余要求 `admin` JWT | 有 |
| `/api/v1/servers/{id}/config` | `GET`/`PATCH`，读写服务器配置 | `GET` 需 JWT；`PATCH` 要求 `admin` JWT | 有 |
| `/api/v1/servers/{id}/channels`、`/api/v1/servers/{id}/channels/{channelId}` | `GET` 集合读取频道树；`POST` 集合，`PATCH`/`DELETE` 单项 | `GET` 需 JWT；其余要求 `admin` JWT | 有 |
| `/api/v1/servers/{id}/channels/{channelId}/acl` | `GET`/`PUT`，读写频道 ACL | `admin` JWT | 有 |
| `/api/v1/servers/{id}/users` | `GET`，查看在线 Mumble 会话；敏感值仅对管理员提供 | JWT | 有；见下方差异 |
| `/api/v1/servers/{id}/users/{sessionId}/{kick,mute,ban}` | `POST`，三条独立的会话管理路由 | `admin` JWT | 有 |
| `/api/v1/servers/{id}/bans`、`/api/v1/servers/{id}/bans/{banId}` | `GET`/`POST` 集合，`DELETE` 单项 | `admin` JWT | 有 |
| `/api/v1/servers/{id}/registered-users`、`/api/v1/servers/{id}/registered-users/{userId}` | `GET`/`POST` 集合，`PATCH`/`DELETE` 单项，管理已注册 Mumble 用户 | `admin` JWT | **未覆盖** |
| `/api/v1/users`、`/api/v1/users/{id}` | `GET` 集合，`PATCH`/`DELETE` 单项，管理 Web 账户 | `admin` JWT | 有 |
| `/api/v1/meta/config` | `GET`/`PATCH`，读写实例级配置 | `admin` JWT | **未覆盖** |

## 已核对的契约差异

- 表中标为「未覆盖」的操作已在 Go 中注册，但当前规范未描述；请求细节以对应 handler 和测试为核对入口，不能从规范缺失推断路由不存在。
- 内部身份回调的规范条目只在说明文字中提及独立 token，没有声明 `security` 或 `requestBody`；[Go 路由](../../internal/rest/router.go#L85-L112)会检查 token，并要求非零 `user_id`。
- 在线用户的 `address` 与 `certificate_hash` 是敏感字段。规范简介称两者对非管理员省略；[适配器](../../internal/rest/mumble_adapter.go)清空两者，但 [Go JSON 标签](../../internal/rest/router.go#L26-L42)仅对 `certificate_hash` 设置 `omitempty`。因此非管理员收到的 `address` 是空字符串，`certificate_hash` 不出现。
