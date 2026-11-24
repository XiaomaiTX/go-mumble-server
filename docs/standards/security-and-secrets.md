---
status: active
doc_type: standard
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - configs/mumble-server.toml
  - internal/auth/
  - internal/middleware/
  - internal/app/distributed_tls.go
---

# 安全与凭据

不提交实际 JWT 密钥、密码、接入令牌、证书私钥或生产数据库；示例使用假值或本地临时证书，记录与截图需脱敏。出现泄露时按受影响的信任域轮换凭据，不能只删当前文件。

- 分清 REST 管理 RBAC、Mumble 频道 ACL 与 Core–Edge mTLS；跨域不能隐式授予权限。涉及外部身份组时，客户端 token 不得冒充身份提供者的权威组。
- 生产管理界面通过 HTTPS 对外提供；Core–Edge 校验双方证书，不为调试关闭校验。具体步骤见 [Caddy 指南](../guides/deployment-caddy.md)和 [Core / Edge 部署](../guides/core-edge-deployment.md)。
- 日志、测试输出和错误响应不应泄露认证材料；权限检查必须发生在服务端。新增或修改授权时覆盖拒绝路径，并确认错误不会暴露越权数据。

安全协议的互操作例外见[安全模式](../protocol/security-modes.md)，不能把兼容性例外当作新部署的安全默认值。
