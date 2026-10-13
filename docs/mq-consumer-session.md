# 独立消费者连接生命周期（P3.5.2.1）

本小点新增 `ConsumerSession`，并从 publisher session 提取每次拨号独立创建的连接 owner。共享的是连接管理代码：生产/消费实例各自拥有 socket、AMQP connection、write mutex、取消回调、Done、在途调用计数与关闭状态，任何一方的超时/关闭不会主动关闭另一方。

RabbitMQ 建议生产者和消费者使用独立连接，以避免生产连接的 flow control 影响消费确认。本轮验证了受控生产 Write 背压和真实连接下行响应黑洞的隔离；尚未注入 broker 资源告警，也尚未实现消费者 delivery/ACK API，不能将这次核验 RPC 的隔离冒充消费 ACK 验收。[RabbitMQ 连接与 flow control](https://www.rabbitmq.com/docs/connections#flow-control)、[flow control 机制](https://www.rabbitmq.com/docs/flow-control)

## 使用

```go
options := rabbitmq.DefaultConnectionOptions()
consumer, err := rabbitmq.DialConsumer(startupCtx, amqpURL, management, vhost, options)
// 检查 err；消费者另行拨号，不接收 publisher 的 AMQP connection。
defer consumer.Close(context.Background())

err = consumer.Verify(checkCtx) // 显式复核，不会自动周期运行。
// 检查 err；失败后该实例永久停止，调用方必须关闭、显式重建。
```

`DialConsumer` 的 ctx 只控制初始化；成功返回后取消 startupCtx 不会关闭已经交给调用方的连接。调用方持有生命周期所有权，必须 `Close`；不能假定传入启动 context 后就自动完成应用退出装配。API 构造器不接受可借用的 AMQP connection，也不暴露底层 connection/channel 或允许对同一连接发布。

`DefaultConnectionOptions()` 与 publisher 的传输默认值相同：connect=5s、operation=5s、write=3s、close=3s、heartbeat=2s，含可选 TLS 配置。没有 publisher slots 或 connection epoch 字段。默认值和 URI/TLS 校验由同一连接内核提供；operation/connect/write/close 为正且不超过一分钟，heartbeat 为 1—60 的整秒值。明文仅允许 loopback/localhost，远端需 AMQPS；显式非空用户名/密码、合法端口、无 query/fragment。TLS 克隆、最低 1.2 且拒绝跳过证书校验；客户端 Recovery 保持 nil。

## 初始化和核验

新连接先完成有界 TCP/TLS/AMQP 握手，再在一个 operation deadline / socket watchdog 下执行与 publisher supervisor 相同的只读准备：

1. 核对该 AMQP 连接实际 broker 版本 4.3.6 和 vhost。
2. 管理 API 精确检查本地单节点 quorum 拓扑、有效参数、成员、策略和绑定。
3. 在该消费者 AMQP 连接上新建临时 channel，对主/隔离 exchange 和 queue 执行 passive 查询，随后关闭临时 channel。
4. 再次读取管理契约，弥补 passive 无法证明全部参数与绑定的限制。

缺失/漂移/权限不足不返回可用实例，没有主动声明、绑定、删除、purge 或策略修复。管理 endpoint 必须指向 AMQP 的同一 broker；配置对应关系仍由部署保证。本地单节点契约、账号权限与显式初始化见 [MQ 拓扑](mq-local-topology.md)。

`Verify(ctx)` 在同一消费者连接上重复上述检查，有 operation 上限；每个实例最多一项核验在途，多余请求立即返回 `ErrConsumerBusy`。nil context、已取消请求和 busy 请求不发送 AMQP 方法，也不因自己的 deadline 中断另一项核验。主动核验失败/超时会停止该消费者 admission 并关闭自己的 socket；后续 Verify 返回 unavailable，恢复绑定也不会复活旧实例。没有后台重连、命令重发或自动周期核验。

初始化或正常核验的 watchdog 会在返回前停止/等待；父 context 后续取消不影响已经成功完成的操作。Verify 成功仅是时点观察，不能保证下一操作或整个服务 readiness。

## I/O、关闭和错误

连接 owner 复用 P3.4.1 的每次 Write deadline、Read EOF/错误停止 admission、socket 中断实际阻塞 RPC 与有界关闭。没有为阻塞 RPC 另起被请求遗弃的 goroutine。`Close(ctx)` 停止 admission，按自身 close 上限或更短父 deadline 关闭 AMQP connection，随后等待该 owner 自有核验调用退出。重复/并发 Close 遵守首轮结果及各自期限；Done 表示停止 admission，不表示所有客户端任务已经析构。机制及时间边界见 [MQ session](mq-session.md)。

错误仅包含 `ErrConsumerConfig`、`ErrConsumerTopology`、`ErrConsumerUnavailable`、`ErrConsumerBusy` 或已有脱敏的 `ErrSessionConnect` / `ErrSessionClose` 与 context 错误；不回传底层 broker 诊断、URL 或密码。关闭缺少响应属于失败/超时，不能报告优雅关闭成功。

## 验证与后续

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestConsumerSession' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session|Supervisor|ConsumerSession)Integration$' -count=1 -v -timeout=10m
```

纯测试使用真实 AMQP 客户端和 net.Pipe 对端，验证握手/初始化/关闭响应黑洞、空闲 EOF、并发关闭和在途 RPC 回收、预取消/busy、初始化 context 与 watchdog 结束、双方故障隔离及错误脱敏。生产 Write 确已进入阻塞 socket 后，消费者 channel.open/close 仍可完成；反向消费者 RPC 无响应后生产者仍获得匹配 confirm。

Docker 用例使用本轮独立 broker/vhost/账号，管理 API 观察生产/消费两个不同连接与各自关闭。TCP 代理观察并丢弃真实 publisher ACK，消费者核验仍完成；消费者 RPC 回复黑洞时另一生产连接仍发布 accepted。移除绑定使已有核验/新连接失败，确认绑定未被修复，手动恢复后仅显式新实例可用。另覆盖真实消费者 connection.close-ok 缺失。消息原文/attempt 与队列空断言由 fixture 原有控制连接读回/ACK，**不是 ConsumerSession 的消费能力**。代理只记录帧计数/事件，不打印协议内容；fixture 按项目和 run 标签清理。

P3.5.2 继续拆分：本轮 **P3.5.2.1 连接内核与独立消费者基础生命周期**；下一小点 **P3.5.2.2 有界消费 channel / 手动 ACK 的传输所有权及隔离验证**，开始前仍可细分。应用配置的 consumer 参数、consumer 自动恢复、运行模式、readiness/持续核验与 HTTP 接线尚未装配；业务消费事务及提交后 ACK 仍在 P4。本轮未改变旧 Redis 链路。
