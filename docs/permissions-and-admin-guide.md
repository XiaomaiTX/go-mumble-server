# 管理员与频道权限说明

## 先区分两种“管理员”

### Web/API 管理账号

Web 管理端使用 SQLite 的 `users` 表登录。账号只有两个角色：`admin` 和 `user`。

- 第一个通过 `/register` 创建的 Web 账号自动成为 `admin`。
- 后续账号默认是 `user`。
- 已有 Web 管理员可在“系统管理 → Users”中把其他账号改为 `Admin`。
- 为避免把系统锁死，当前登录账号不能修改自己的角色，也不能删除自己。

这个角色控制 REST/Web 管理接口，并不等同于某个普通 Mumble 注册用户的 `UserID`。

### Mumble 协议管理员

原生 Mumble 客户端的操作权限由频道 ACL 决定。默认 root（频道 ID 0）包含：

- `all`：普通的进入、发言、私聊、文字和收听权限；
- `auth`：注册用户的自助权限；
- `admin`：授予 `Write`。

`Write` 会展开为频道管理权限，其中包括 `LinkChannel`，但本项目明确不让 `Write` 自动绕过 `Speak` 和 `Whisper` 的拒绝规则。

要让一个本地 Mumble 注册用户能够在客户端右键链接频道：

1. 先确认他已经注册，并记下 Registered User ID；
2. 在 Web 管理端打开目标服务器的 root 频道（ID 0）→ `ACL & Groups` → `Groups`；
3. 在 root 的 `admin` 组中把该 Registered User ID 加入；或在 ACL 中给该用户/组显式授予 `LinkChannel`；
4. 如果不使用 root `admin` 组而是显式 ACL，则链接的两个频道都要授予 `LinkChannel`。

链接频道的官方规则是：新增链接要求两端都具有 `LinkChannel`；删除链接只要求任一端具有该权限。Web 编辑频道时的 `Linked channels` 控件使用同一规则，并会同时保存两端链接。

## 权限如何计算

权限不是一个全局开关，而是按“用户主体 + 目标频道”计算：

1. 从 root 到目标频道逐层读取 ACL；
2. 应用 `Apply here`、`Apply subs`、`InheritACL` 和 Group 继承；
3. 按 ACL 顺序应用 grant/deny；
4. 对最终目标频道检查具体权限。

因此，用户在当前频道能发言，不代表他一定能通过链接向另一个频道发言。Linked Channel 音频会对每个目标频道再次检查 `Speak`。

## `local` 与 `external`

### local（默认）

身份和权限所需的数据由本服务器维护：

- Mumble 注册用户保存在 `registered_users`，使用证书摘要和/或密码认证；
- 未注册用户使用 User ID 0，通过 `all` 等 ACL 规则获得匿名权限；
- Web/API 账号保存在 `users`，管理员账号会映射为保留的 API synthetic User ID，供 ACL 的 `admin` 选择器识别；
- 持久化 Group 成员保存在 `channel_groups`；
- 客户端 access token 只用于 `#token` 选择器，不等同于 Web/API 角色。

### external

在 `[auth].mode = "external"` 时，Mumble 登录和身份查询交给配置的 HTTP Identity Provider：

- provider 返回稳定 User ID、规范名称和运行时组；
- provider 拒绝、超时、5xx 或非法响应都会拒绝登录，不会回退到本地注册用户；
- 外部组只存在于当前在线会话，不写入 `channel_groups`；
- 外部组可以匹配 ACL 中同名的命名组，例如 root ACL 使用 `admin` 时由 provider 声明 `admin`；
- 在线用户会按配置周期重新校验，失去资格或超过宽限期会被断开；
- 外部平台的 Web 管理员角色与 Mumble 的 `SuperUser` 不是同一个概念，外部响应不能仅靠用户名 `SuperUser` 获得特权。

### external 模式下授予链接权限

如果当前所有外部用户的“链接频道”都变灰，按下面两种方式任选其一：

1. **按外部角色授权（推荐）**：让 Identity Provider 在认证响应的 `groups` 中返回例如 `admin` 或 `voice-linker`，然后在 root 的 ACL 中添加同名 Group，授予 `LinkChannel`，并设置 `Apply subs`。新增链接时两个频道都要能匹配该 ACL。
2. **按单个外部稳定 User ID 授权**：从 Web 的在线用户列表或身份服务日志取得稳定 User ID，在 root 的 ACL 中选择 `user`，填入该 ID，授予 `LinkChannel`，并设置 `Apply subs`。

外部用户不需要、也不能通过 `Registered users` 列表加入本地 `admin` Group；外部 Group 只来自 provider 的当前响应。修改 ACL 或外部组后，建议让用户重新连接 Mumble；如果仍显示灰色，检查两个目标频道的 `PermissionQuery` 是否都包含 `LinkChannel`。

## 没有现成 Web 管理员时

如果数据库中已经存在账号但没有任何 `admin` 账号，当前登录账号无法在 Web 页面自行升权，因为页面和服务都会禁止自改角色。应先通过数据库维护流程恢复一个管理员账号，再重新登录；不要直接把 Mumble 注册用户 ID、Web 用户 ID 或外部稳定 ID 混用。
