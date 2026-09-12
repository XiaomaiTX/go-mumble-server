# 外部身份提供者接入（Fuxi Seat 示例）

## 目标

go-mumble-server 通过通用 External HTTP Identity Provider 决定用户身份、登录资格和运行时权限组。它只负责协议会话、频道与 ACL，绝不复制外部平台的用户或职权数据库。Fuxi Seat 可作为首个接入方，但不是核心代码的特殊分支。

## go-mumble-server 配置

在启动配置的 `[auth]` 段设置：

- `mode = "external"`：启用外部认证；该模式不会回退 `registered_users`。
- `external_url`：身份提供者 HTTP 根地址，不包含 `/internal/mumble/v1`。
- `service_token`：Mumble → 身份提供者服务令牌。
- `server_instance_id`：当前语音实例的稳定标识。
- `timeout_ms`：单次外部认证/解析请求的严格超时。
- `revalidate_interval_seconds`：在线会话批量重验周期。
- `stale_grace_seconds`：提供者暂时不可用时，已在线会话可保留的最长时间；新登录始终 fail closed。
- `ca_cert`、`client_cert`、`client_key`：私有 CA 与 mTLS 客户端证书，可按部署需要启用。
- `identity_revalidate_token`：身份提供者 → Mumble 重校验令牌，必须与 `service_token` 不同。

接入方的双向令牌、Mumble 管理地址和回调超时均由接入方安全配置，不写入本服务配置仓库。

## 运行时行为

登录时，服务器把用户名、密码、证书摘要、来源 IP 和实例 ID 发送给提供者。提供者返回稳定 Mumble User ID、canonical name、资格和运行时权限组。外部服务异常或响应非法时拒绝新登录。

在线用户由定时任务批量向身份提供者拉取当前声明。失去资格时立即断开；名称或权限组变化时原子更新运行时身份、广播用户状态并失效 ACL 缓存。提供者也可以调用 `POST /internal/identity/v1/revalidate` 提前触发单用户拉取；请求体只含稳定用户 ID，不能直接注入新权限组。

外部运行时组与客户端 access token 在领域模型中完全分离：前者仅由当前 external session 的提供者声明获得，后者仅用于 `#token` ACL 选择器。因此客户端 token 不能伪造外部组；外部组也不会写入本地持久化 membership。

`SuperUser` 只接受明确的本地或权威标记，不能再通过用户名字符串获得管理员身份。Seat 外部身份响应若尝试返回 `SuperUser` 会被拒绝。

## HTTP 契约与兼容策略

认证请求为 `POST /internal/mumble/v1/authenticate`，包含 `server_instance_id`、`username`、`password` 以及可选的 `certificate_hash`、`remote_ip`。响应的 `decision` 只能是 `allow` 或 `deny`：`allow` 必须携带非零 `user_id`、`name` 和可选 `groups`、`identity_version`、`policy_version`；密码错误、账户禁用等属于 `deny`，网络错误、超时、5xx 与无效 JSON 都属于 provider error。

目录解析请求为 `POST /internal/mumble/v1/identities/resolve`，可同时携带 `user_ids` 和 `names`，响应 `identities`，因此 `QueryUsers` 以稳定 ID↔名称工作且不依赖在线 session。当前实现一次请求支持多个 ID，供周期性重验批量使用。

外部模式下，Mumble 原生 `UserList` 的 rename/unregister 不会写回身份提供者；该类 mutation 保持 unsupported/read-only。外部平台管理员角色与 Mumble protocol `SuperUser` 是独立概念，外部响应不能使用 ID 0 或 `SuperUser` 名称取得该特权。

## Seat 系统设置映射

在 Seat 的“系统管理 → 基础配置 → Mumble 连接设置”中配置：

- Mumble 管理地址：本服务 REST 地址，例如 `http://go-mumble-server:64730`。
- Mumble → Seat 服务令牌：对应本服务 `[auth].service_token`。
- Seat → Mumble 重校验令牌：对应本服务 `[auth].identity_revalidate_token`。
- 重校验超时：Seat 主动通知请求的超时毫秒数。

普通用户随后可在 Seat 的“EVE 人物管理”页面创建独立 Mumble 应用密码，并使用主人物名称登录。
