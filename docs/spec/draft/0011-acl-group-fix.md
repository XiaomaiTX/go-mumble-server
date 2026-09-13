# ACL 与 Group 联合修复说明

## 文档状态

- 状态：Implemented
- 实现基线：`97eb8ce`
- 对照实现：Mumble/Murmur `v1.5.915`
- 对应 Issue：#3、#4、#5

## 修复内容

本轮将 ACL 权限计算改为从 root 到目标频道逐层执行。`Traverse` 和 `Write` 使用独立状态，祖先频道的 `Traverse` 拒绝不能再被子频道授权绕过；`InheritACL=false` 只重置普通权限，不删除已经经过的路径检查。`Write` 在最终权限掩码中展开，只隐含 Murmur 定义的频道管理权限，不再隐含 `Speak` 或 `Whisper`。Kick、Ban、Register、SelfRegister 和 ResetUser 只能从 root 获得。

Group selector 现在由 ACL 和 VoiceTarget 共用同一套解析规则，支持连续的 `!`、`~`、`#`、`$` 前缀，以及 `none`、`all`、`auth`、`strong`、`in`、`out`、`sub`。Token 使用不区分大小写的比较；证书 hash 保持区分大小写；`strong` 只接受 TLS 验证产生的可信证书链。外部 authority 组只参与普通命名组匹配，不能覆盖内置 selector，也不能由客户端 Token 伪造。

持久 Group 从当前频道向祖先收集，再按祖先到子频道应用 add、临时加入和 remove。这样子频道 remove 可以撤销父级 add，子频道 add 也可以恢复父级 remove。`inherit`、`inheritable` 和频道的 `InheritACL` 各自保持独立语义。

Evaluator 增加了不持久化的临时成员存储。会话成员同时绑定 session ID 和连接代次；断线、session ID 复用、频道删除和进程重启都不会继承旧临时权限。该能力目前只提供内部接口，不增加 REST 或原生 ACL 管理协议字段。

## 项目扩展

- API 管理员继续获得默认 Write，但使用同一套 Write 展开规则，仍可被明确拒绝 Speak 或 Whisper；角色查询不使用权限缓存，撤权提交后立即生效。
- 外部 authority 组保持会话声明的直接匹配方式，不写入数据库，也不参与持久 Group 的 add/remove 继承。
- 当前 `IsSuperUser` 表示保持不变，其权限对齐 Murmur：拥有受支持的管理权限，但不自动拥有 Speak 或 Whisper。注册用户 ID 0 的兼容问题仍由 #26 处理。
- 无会话主体不再借用同账号任意在线连接的频道、Token、证书或外部组。

## 兼容影响

以下配置会收紧权限，升级前应使用生产配置副本验证：

- 依赖 Write 自动获得 Speak 或 Whisper 的用户，需要显式授予对应语音权限。
- 在子频道授予 Kick、Ban、Register、SelfRegister 或 ResetUser 的规则不再生效，应将服务器级授权放到 root。
- 仅依赖注册用户 ID 自动获得 SelfRegister 或 MakeTempChannel 的部署，需要保留或补充 root `auth` ACL。
- 依赖父级 add 覆盖子级 remove 的 Group 配置会按正确的子级覆盖顺序改变结果。
- 未知频道、失效会话和 ACL/Group 查询错误现在统一拒绝授权。

实现不会改写现有 ACL/Group 数据，也不会重新播种已有服务器的 root ACL。

## 验收覆盖

自动测试覆盖以下行为：

- Traverse 路径阻断、Write 例外、`InheritACL` 重置和 ApplyHere/ApplySubs 组合。
- 同条 ACL 的 grant/deny 顺序、后续规则覆盖、所有 root-only 权限以及 PermissionQuery 返回掩码。
- Token 大小写、证书和 strong、内外频道、`sub` 深度参数、前缀组合与空表达式。
- 父子 Group add/remove、缺失中间组、inherit/inheritable 组合和频道 ACL 继承隔离。
- 多匿名用户、同账号多会话、会话断开及复用、临时成员和外部身份撤权。
- ACL/Group 查询失败关闭、缓存 generation 竞争、频道删除和 API 角色撤销。
- 进入频道、Speak suppress、VoiceTarget 和 Listener 的集成回归。

本地验收使用 `go test ./...`、`go vet ./...` 和格式检查。Race 检测由 Linux CI 的 `make test` 执行；该目标已包含 `CGO_ENABLED=1 go test -race`。
