---
status: active
doc_type: architecture
owner: maintainers
last_reviewed: 2026-11-24
source_of_truth:
  - internal/edge/ingress.go
  - internal/edge/edgeruntime.go
  - internal/mumble/remoteedge.go
  - internal/mumble/handlers.go
  - internal/connection/conn.go
  - internal/cluster/control.go
  - internal/cluster/remoteedgeserver.go
---

# Core/Edge 协议所有权

客户端 Mumble 协议和 Core–Edge 内部链路是不同契约。这里说明当前代码中的职责；[第一阶段设计约束](../spec/archive/core-edge-protocol-ownership-phase-1.md)只保留历史背景。

| 场景 | Edge / 客户端接入 | Core / 权威业务 |
| --- | --- | --- |
| TLS、版本与登录 | 终止客户端 TLS，采集地址和证书身份信息，协商接入所需的协议能力 | 决定身份与登录资格、封禁、容量、会话分配和同步内容 |
| 通常的控制消息 | 按客户端连接接收、发送与排序，不自行授予频道权限 | 处理频道、用户、ACL、封禁、文本与语音目标，决定广播或有序关闭 |
| TCP Ping | 在连接层生成回复并报告可用指标 | 接收业务所需指标，不需要客户端加密状态来回包 |
| UDP、加密与音频 | 管理 UDP 端点、加密状态、探测和客户端 wire 编解码；执行下行传输选择 | 决定发送资格、语音接收者及逐接收者的投递元数据，不共享客户端 socket 或密钥 |

[接入分派](../../internal/edge/ingress.go)定义控制消息如何在本地处理或转交，不能从一个消息名称推断其全部业务逻辑都属于单侧。[远端会话入口](../../internal/mumble/remoteedge.go)和[控制传输](../../internal/cluster/control.go)负责按带代次的会话引用投递；初始同步有 flush 确认，终止控制消息使用排序并关闭的组合操作。Core epoch 与 Edge 实例代次属于内部链路，详见[数据与契约](data-and-contracts.md)。

具体行为以当前实现及对应测试为准。做传输变更时同时检查本地和远端路径；两者的探测等细节不应仅因所有权相同就被视为已互操作。部署见 [Core / Edge 指南](../guides/core-edge-deployment.md)。
