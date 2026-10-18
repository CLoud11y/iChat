# RabbitMQ 隔离测试环境与版本核验

P3.1 已完成（2026-10-13，Australia/Sydney）。RabbitMQ 使用 Docker，与每轮独立 MySQL/Redis fixture 一起启动；本阶段只交付版本核验和测试基础设施。P3.2 已完成正式本地 Docker 拓扑及可靠 DLX，启动与故障验收见 [本地 MQ 拓扑](mq-local-topology.md)。有界 publisher、消费传输接口、运行时及显式 api 的生产接受入口现已实现，业务消费任务尚未接入；当前阶段对照见 [P3 验收](mq-reliability-acceptance.md)。

## 固定版本

| 项目 | 已验证值 |
| --- | --- |
| 项目 Go 工具链 | `go1.27.1 windows/amd64` |
| Broker | RabbitMQ `4.3.6`，真实 management `/api/overview` 校验 |
| 测试镜像 | `rabbitmq:4.3.6-management@sha256:8dd6e3570ddaa2ef82a6c3a8950e79c1f73893adfbdddbc374fc1f1ac8a0f5dd` |
| 本机拉取架构 | `linux/amd64`；digest 为实际拉取的仓库 digest，其他架构尚未验证 |
| Go 客户端 | `github.com/rabbitmq/amqp091-go v1.15.0`，固定在 go.mod/go.sum |
| 客户端最低 Go 版本 | `1.20`，以已下载模块的 go.mod 为准 |
| 模块校验和 | `h1:LEQL4/yp48/Wigt6A6XOu18RQRo8ZHtB5I/KZJn+gkw=` |
| 客户端 go.mod 校验和 | `h1:Hy4jKW5kQART1u+JkDTF9YYOQUHXqMuhrgxOEeS7G4o=` |

原开发规范的候选是 RabbitMQ 4.3.5 / amqp091-go 1.14.0。本轮按官方发布信息核验后选择 4.3.6 / 1.15.0；客户端 1.15.0 修复握手清零时修改调用方 SASL 凭证及 Reconnect 凭证恢复问题，测试实际验证同一调用方凭证可建立两次连接。[RabbitMQ 版本信息](https://www.rabbitmq.com/release-information)、[4.3.6 发布说明](https://github.com/rabbitmq/rabbitmq-server/blob/v4.3.6/release-notes/4.3.6.md)、[客户端 1.15.0 发布说明](https://github.com/rabbitmq/amqp091-go/releases/tag/v1.15.0)

本轮保留项目已固定的 Go 1.27.1。客户端要求 Go 1.20，不把项目工具链版本误写成 AMQP 库的最低要求。

## 启动与资源归属

默认 `testenv.Start` 只启动原有依赖。MQ 用例调用：

```go
env, err := testenv.StartWithRabbitMQ(ctx, "../../docker-compose.test.yml")
// 检查 err；先注册 Environment.Close 的独立 cleanup context。
mq, err := env.RabbitMQ()
// 检查 err；mq.AMQPURL() 仅作为客户端拨号输入。
```

`StartWithRabbitMQ` 使用同一文件的 `mq` Compose profile，在全新 `ichat-test-<runId>` 项目中创建三项服务、网络和数据卷。runId 和密码每轮随机生成；不读应用配置/开发 `.env`，清除继承的 `COMPOSE_*` / `ICHAT_TEST_*` 覆盖，再显式传入文件、项目名及空 env 文件。测试端口由 Docker 动态分配，AMQP 5672 和 management 15672 只映射到宿主 `127.0.0.1`。

Docker 健康检查验证 RabbitMQ 节点存活、运行及本地告警；management HTTP 另作有界只读就绪重试。只有确认实际 broker 版本、vhost 元数据和应用账号权限之后，整个 fixture 才变为 ready，`RabbitMQ()` 才返回配置。已有同名 vhost 或用户直接拒绝采用，不通过 upsert 覆盖已有配置。写管理操作不盲目重试，失败时由新建容器的清理责任收口。

| 资源 | 归属与权限 |
| --- | --- |
| Docker 容器/卷/网络 | 同时验证 `com.docker.compose.project` 与 `io.ichat.test.run` 标签 |
| bootstrap vhost | `/ichat_bootstrap_<runId>`，仅本轮独立 broker 的测试管理账号使用 |
| 测试 vhost | `ichat_test_<runId>`；description=`iChat test run <runId>`，tags 为 `ichat-test` / `run:<runId>`，默认 queue type 为 quorum |
| 管理账号 | `ichat_test_admin`；凭证只在本轮 fixture 内用于初始化与只读检查 |
| 应用账号 | `ichat_app_<runId>`，独立随机密码、无管理标签；仅该 vhost 的 configure/write/read 权限，正则均为 `^ichat\.` |

vhost 提供逻辑资源与权限隔离；本项目还为每轮创建独立 broker 容器和卷，避免共享 broker 的物理资源与清理边界。[vhost 官方说明](https://www.rabbitmq.com/docs/vhosts)、[权限说明](https://www.rabbitmq.com/docs/access-control)

管理 HTTP helper 只接受本轮 loopback endpoint，禁用代理与跳转，限制响应到 1 MiB，并设置连接/请求超时；错误不附带原始响应正文。`RabbitMQConfig.String()` 不打印密码；`AMQPURL()` 包含凭证，只用于拨号，不写日志。

调用方先关闭 AMQP/DB/Redis 客户端，再 `Environment.Close`。Close 先验证所有 Docker 资源标签，之后才逐一移除本轮明确 ID 的容器、卷和网络；RabbitMQ 连同专用卷被回收。这里不向共享 broker 发送删除 vhost/队列请求，broker 故障也不依赖 management HTTP 来决定 Docker 清理归属。标签不符或 inspect 失败时不删除；强制终止测试可能留下资源，应依据日志中的 runId 检查后恢复清理。测试镜像留在本机缓存，开发资源不在清理范围内。

## 固定客户端源码边界

本轮核对下载到 Go module cache 的 v1.15.0 源码，并与官方固定 tag 的实现对应：

| 源码位置 | 核验结论与后续要求 |
| --- | --- |
| `Connection.reader` / `dispatchN`，`Channel.dispatch` | 连接读取循环分派帧，basic.return 的同步通知先于后续确认帧处理；同一 channel 的通知必须由指定接收者管理。 |
| `notifyAll`，`confirms.confirm` | return 与 publish confirm 使用同步通知；目标通知缓冲满时，单个发送等待 5 秒后可放弃。因此 buffer 至少容纳 outstanding，保持有界 outstanding 和及时消费是正确性前提。 |
| `Channel.NotifyPublish` / `Confirm(false)` | publisher sequence 属于 channel，应用必须关联 channel/connection epoch；ACK 不直接证明未发生 return。 |
| `Channel.PublishWithContext` | 调用前取消会立即报错；已经开始的 Publish 不受后续 context 取消中断。网络写超时/可中断 transport 必须在 P3.4 实现。 |
| `Config.Recovery` | 显式设为 nil，关闭实验性自动连接/拓扑恢复；应用 supervisor 在 P3.4/P3.5 管理 epoch、退避及拓扑复核。 |
| `Connection.openStart/openComplete` | 调用方 SASL 对象被克隆，不因内部凭证清零而被修改；实际双次握手测试已覆盖。 |
| `Connection.CloseDeadline` | 测试使用截止时间关闭连接；已关闭连接再次关闭可能返回 `ErrClosed`，仅在重复 cleanup 中将此视为已完成。 |

[v1.15.0 channel.go](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/channel.go)、[confirms.go](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/confirms.go)、[connection.go](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/connection.go)

这是后续设计的源码前提核验。真实 return/ACK 测试使用一条 outstanding、容量 1 的原始通知缓冲，刻意先读取 ACK 再读取已排队 Return；不替代 P3.3 生产 publisher 的结果状态机和完整故障矩阵。客户端升级后需要重新复核这些边界。

## 验证命令与覆盖

```powershell
go test -mod=readonly -race ./tests/testenv -count=1
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQEnvironmentIntegration$' -count=1 -v -timeout=10m
go test -mod=readonly -race -tags=integration ./tests/integration -run '^TestEnvironmentIsolation$' -count=1 -v -timeout=10m
go vet -mod=readonly -tags=integration ./...
```

测试自动启动并清理独立资源，无需手工为测试文件填写 runId 或执行 Compose。默认单元测试不启动 Docker，只使用本机 httptest 对端验证管理 API 边界。

真实 MQ 用例检查两套环境的同名 exchange/queue 保存不同内容、双方独立凭证、实际单成员 quorum probe 队列、跨 vhost/跨环境/资源前缀访问拒绝、应用账号无 management 权限、mandatory 不可路由 Return 与 publisher ACK、预取消 Publish 不消耗 tag、SASL 凭证复用，以及先清理一套后另一套待处理消息仍完整。probe 对象为 `ichat.test.probe.*`，只证明版本和隔离环境可用；正式主/隔离拓扑、可靠 DLX 和 HTTP 接线由后续独立 suites 验证；生产通知仍归 P5，不能将其计为本页 probe 的验证范围。

本页基础 probe 不覆盖 broker 重启、confirm 丢失、网络黑洞、背压及容量；这些传输场景由后续独立 suites 验证，映射见 [P3 验收对照](mq-reliability-acceptance.md)。节点/多数故障、业务隔离恢复和完整容量基线仍未完成，单节点 probe 不宣称节点容灾。
