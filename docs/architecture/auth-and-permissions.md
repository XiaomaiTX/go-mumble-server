---
status: active
doc_type: architecture
owner: engineering
last_reviewed: 2026-11-24
source_of_truth:
  - internal/rest/router.go
  - internal/middleware/auth.go
  - internal/middleware/rbac.go
  - internal/service/user.go
  - internal/mumble/handlers.go
  - internal/mumble/identity_authority.go
  - internal/identity/external_http.go
  - internal/mumble/identity_revalidation.go
  - internal/acl/evaluator.go
  - internal/acl/rbac_strategy.go
---

# 认证与权限

## 范围

区分 REST 管理账号、Mumble 身份和频道授权。操作步骤与具体规则示例见[权限与管理指南](../guides/permissions-and-admin-guide.md)及[伏羲席位外部身份指南](../guides/fuxi-seat-external-identity.md)；逐路由的访问级别见 [API 入口](../api/route-index.md)。

## 当前实现

### REST 管理平面

REST 登录使用 `users` 表的账号。注册时，账号表为空则授予首个账号 `admin`，否则授予 `user`。受保护路由解析 Bearer JWT，按账号查找当前签名密钥，校验签名、声明和 `token_version`；管理员组再检查 `admin` 角色。修改角色同时轮换该账号的签名密钥并增加令牌版本，删除账号后查不到验签凭据。浏览器的菜单和路由隐藏不是授权边界。

### Mumble 客户端身份

- **`local`**：由 Core 的本地身份提供方按服务器密码、REST 账号凭据以及 `registered_users` 登录分支判定；本地注册用户可凭匹配的已登记证书哈希或密码验证。未关联账号的客户端以访客身份进入；持有服务器密码本身并不授予管理员角色。通过 REST 账号凭据登录 Mumble 时，使用高位标记的合成 `UserID`，不与普通本地注册用户共用编号。
- **`external`**：Core 向提供方进行登录验证和身份查询，使用返回的规范名、稳定用户 ID、组与身份/策略版本。提供方失败时拒绝新登录，不回退到本地身份。该模式不替换 REST 自己的账号或 JWT。在线重验由定时任务或带独立服务令牌的内部回调触发；回调只给出用户 ID，服务器重新向提供方取结果。明确不再符合资格时断开；查询失败或缺项则根据上次成功校验时间应用宽限期。

### Mumble 频道授权

频道权限由 Core 的 ACL 计算，不以 REST JWT 或管理界面的 `is_admin` 展示标记作为依据。计算沿根到目标频道的路径按 `priority, id` 应用条目，区分本频道与子频道作用域，结合组、稳定用户 ID、会话属性、访问令牌或证书选择器。`InheritACL` 不是直接截断祖先链；权限评估错误时拒绝授权。对用合成 ID 登录的 REST 管理员，当前数据库角色参与 ACL（如 `admin` 组匹配和默认 `Write`），不是一个跳过 ACL 的通行证。

## 关键入口

- [REST 路由分组](../../internal/rest/router.go)、[JWT 校验](../../internal/middleware/auth.go)、[角色变更与账号删除](../../internal/service/user.go)。
- [本地身份提供方](../../internal/mumble/identity_authority.go)、[外部 HTTP 提供方](../../internal/identity/external_http.go)、[在线重验](../../internal/mumble/identity_revalidation.go)。
- [ACL 计算器](../../internal/acl/evaluator.go)、[组解析](../../internal/acl/group.go)、[REST 账号与 Mumble ID 映射](../../internal/acl/rbac_strategy.go)。

## 不变量

- 登录通过只确认身份，不等于拥有目标频道、用户操作或服务器管理的权限；每个入口须执行其服务器端权限检查。
- 登录身份不能以显示名、Web 界面标记或单独的在线 Session ID 替代。权限缓存与在线重验须区分会话代次，避免迟到结果影响复用同一编号的新会话。
