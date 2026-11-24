---
status: active
doc_type: api
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - internal/rest/router.go
  - api/openapi.yaml
  - internal/handler/auth.go
  - internal/middleware/auth.go
  - internal/middleware/rbac.go
  - internal/middleware/ratelimit.go
  - internal/httputil/response.go
  - frontend/src/utils/api.js
---

# HTTP API 约定

## 接口边界

管理端 HTTP API 使用 `/api/v1`；它不是 Mumble 客户端协议，也不是 Core–Edge 传输协议。`/health`、Swagger UI 和内部身份回调不使用管理 API 前缀。入口和权限分组见[路由索引](route-index.md)；字段、参数和状态码优先查阅[已维护的 OpenAPI 契约](../../api/openapi.yaml)及对应 handler。

- 受保护的管理请求使用 `Authorization: Bearer <JWT>`。JWT 可由[登录或注册 handler](../../internal/handler/auth.go)返回；[认证中间件](../../internal/middleware/auth.go)验证签名、声明及当前用户的 token version。需要 `admin` 的操作由 [RBAC 中间件](../../internal/middleware/rbac.go)检查 JWT 角色：无有效 JWT 返回 `401`，已认证但角色不符返回 `403`。REST 角色与 Mumble 的 ACL 分开，见[权限与管理指南](../guides/permissions-and-admin-guide.md)。
- `GET /api/v1/auth/status` 不强制认证：有效 JWT 返回 `user`，缺失或无效的认证头按匿名请求处理并查询 `hasUsers`。登录和注册共用[每 IP 每分钟 20 次的限流器](../../internal/middleware/ratelimit.go)，超限为 `429`。
- 内部身份重验使用独立的 provider Bearer token，而不是管理 JWT；它只提交稳定用户 ID 以触发重新拉取，不接收外部声明写入。配置与信任边界见[外部身份接入](../guides/fuxi-seat-external-identity.md)。

## 请求与响应

- 有请求体的管理请求使用 JSON；管理前端的[请求封装](../../frontend/src/utils/api.js)设置 `Content-Type: application/json`，有登录 token 时添加 Bearer 头。JSON 键名和省略/零值语义按具体操作确认（如 `hasUsers` 不是 `has_users`）。`{id}`、`{channelId}`、`{sessionId}` 等路径参数不可互换。
- 成功响应没有统一外层包装：有的操作返回对象或数组，`204 No Content` 无响应体。例如[已连接用户 handler](../../internal/handler/server.go)直接返回数组，不带分页外层；不要假定公共分页或批量协议。
- [JSON 错误助手](../../internal/httputil/response.go)使用 `error`、`code`、可选 `details`。部分 handler 及内部回调用 `http.Error` 输出文本响应，即使正文形似 JSON，也不保证 `application/json` 或存在 `code`。调用端先判断 HTTP 状态，不应假定所有失败都能按同一 schema 解码。

## 版本与同步

`/api/v1` 是部署路由前缀；OpenAPI 顶层的 `openapi` 字段是规范格式版本，`info.version` 是契约描述版本，均不等同于 URL 前缀。本文不另行承诺兼容期或弃用策略。

变更路由、权限或 payload 时，一起核对 [Go 路由](../../internal/rest/router.go)与 handler → [OpenAPI](../../api/openapi.yaml) → [前端调用封装](../../frontend/src/utils/api.js)及实际消费者 → 回归测试和[路由索引](route-index.md)。细节维护在适用的契约与测试，不在索引复制 schema；发现漂移按[仓库规则](../ai/repo-rules.md)确认，而不是用其中一份掩盖差异。
