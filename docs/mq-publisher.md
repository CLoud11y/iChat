# RabbitMQ 有界 publisher 与发布结果

P3.3 完成 publisher slot 池、服务器信封构造与 confirm/return 判定。固定 RabbitMQ `4.3.6`、`amqp091-go v1.15.0`。P3.4.1 新增拥有底层连接的有界入口，推荐使用方式见 [MQ session](mq-session.md)。当前只作为独立库及隔离测试使用，尚未接入 HTTP 202、旧发送入口或消费任务。

## 调用边界

调用者先准备正确拓扑和关闭 Recovery 的独立 AMQP 连接，随后显式创建 publisher：

```go
publisher, err := rabbitmq.NewPublisher(connection, rabbitmq.PublisherOptions{
    Slots: 8,
    ConnectionEpoch: 1,
})
// 检查 err；调用者拥有 connection，publisher 拥有自己创建的 channels。
defer publisher.Close()

ctx, cancel := context.WithTimeout(parent, 5*time.Second)
defer cancel()
result, err := publisher.Publish(ctx, normalizedCommand, time.Now())
// 只有 err == nil 且 result.Outcome == rabbitmq.PublishAccepted 才已确认接收。
```

slot 数必须为 1—64，connection epoch 必须非零，由连接所有者分配；每个新 channel 在该连接 epoch 内得到独立编号。构造失败释放已经创建的 channels，拒绝 nil/关闭/自动 Recovery 连接。publisher 不读取配置、不自行拨号、不声明拓扑；持续拓扑 readiness 与独立生产/消费连接在 P3.5 装配。

`Publish` 要求带 deadline 的 context，拒绝已取消或无限等待的调用。它校验规范化 Command，空闲 slot 不足时立即返回 busy，不启动等待 goroutine、不生成 attempt、不写 AMQP。无 live slot 或手动关闭后返回 unavailable。外层请求并发限制仍由调用者承担。

每次真正获得 slot 后，通过 `messaging.NewEnvelope` 构造新的 server envelope：稳定 commandId/payloadHash、服务器 receivedAt 和新的随机 publishAttemptId，JSON 不直接使用客户端原文。发布固定到 `ichat.commands.v1` / `message.submit.v1`，使用 mandatory=true、immediate=false、ContentType=application/json、DeliveryMode=persistent、MessageId=commandId、CorrelationId=publishAttemptId，不设置 expiration 或携带鉴权/数据库属性。

## 通知与判定

一个 slot 的 channel 同时最多一条 outstanding。只注册一组原始 `NotifyReturn`、`NotifyPublish`、`NotifyClose`，各容量 1，在 `Confirm(false)` 前注册。当前独占该 slot 的 Publish 执行者是唯一通知接收者；没有额外 Return 转发 goroutine、异步 map 或无限请求队列。

固定客户端版本先同步分派 basic.return，再分派之后的确认；容量 1 与单 outstanding 保证合法 Return 不会因缓冲满而丢失。在 Go select 可能先选到 ACK 的情况下，生产 ACK 分支先非阻塞读取原始 Return，再做成功判定。Return 校验命令/attempt、exchange/routing key、正文和持久化属性；ACK 校验当前 channel tag。未完成判定的 slot 不归还池。[RabbitMQ 确认语义](https://www.rabbitmq.com/docs/confirms)、[固定客户端通知分派](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/channel.go)

| Outcome | 含义和处理 |
| --- | --- |
| `accepted` | 匹配的正确认、没有 Return，通知/session/序号正常；只证明 MQ 接收，不是 MySQL 提交或客户端收到 |
| `not_published` | 输入/context/池容量/关闭或预写 session 检查失败，尚未调用网络发布 |
| `unroutable` | 匹配的 mandatory Return（NO_ROUTE），不得被之后的 ACK 改为 accepted |
| `nacked` | 匹配的 broker 负确认，不是 accepted |
| `publish_unknown` | 已进入发布调用后写错误、确认等待超时/取消、关闭或错误通知，不能据此证明消息未进入 MQ |

失败返回 typed `PublishError`，可通过 `errors.As` 读取 Outcome、`errors.Is` 判断原因；结果记录 connection/channel epoch、tag、command/attempt 身份。错误不包含消息正文、broker reply text 或底层写错误文本。客户端逻辑重试仍使用原 clientMessageId 和原内容；每次服务端发布生成新 attempt。

Return、NACK、写错误、确认等待超时、关闭或污染通知都会使该 channel 退休并关闭，永久退出池。晚到 ACK 留在旧 channel 的通知空间，不会进入其他 slot。成功 channel 可以连续使用，其 tag 单调增加；每条逻辑重试的 attempt 仍不同。没有自动替换或重连，退休会降低可用容量，全退休时明确 unavailable。

## 本阶段的限制

裸 `NewPublisher` 的 context 可以终止等待 confirm，**不能单靠 amqp091-go 1.15.0 的 PublishWithContext 中断已经开始的网络 I/O**。构造、退休与 Close 也包含 RPC，仍需调用者提供 transport 边界。P3.4.1 的 `DialPublisher`/`PublisherSession` 已增加 per-write deadline 和 socket 中断，覆盖握手、初始化、发布与关闭；P3.4.2 的 [MQ supervisor](mq-supervisor.md) 已管理自动恢复/epoch 重连。I/O 边界见 [MQ session](mq-session.md)。[固定 PublishWithContext 实现](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/channel.go)

服务端自动重试、HTTP 202、数据库幂等接线、消费者、隔离审计和 HA 故障验收仍按后续小点实施。关闭 publisher 只处理自身 channel，不关闭调用者 connection；连接所有者最后负责关闭连接。

## 验证

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestPublisher' -count=1 -v
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQPublisherIntegration$' -count=1 -v -timeout=8m
```

单元测试确定性地从两个已经缓冲的原始通道先取 ACK，再调用实际生产确认分支，断言 Return+ACK 不被接受；另覆盖池容量、同 channel 连续 tag、新 attempt、NACK/错 tag/错身份/错正文、通知关闭、预取消与有限 confirm 等待、部分构造失败及旧 slot 晚到 ACK。

Docker 测试采用 P3.1 的本轮独立 broker/vhost，创建 P3.2 正式拓扑：两个 slots 并发发布 12 次同逻辑命令，逐条核验信封和 AMQP 身份；移除主绑定产生真实 Return+ACK，恢复路由后旧 slot 仍不可用，新显式 epoch 可发布；临时注入本轮容量策略产生真实 NACK，验证已确认原文没有被丢队头。容量策略仅是故障注入，故意违反 P3.2 的 policy-free 契约，移除后重新核验正式拓扑。所有 fixture 按归属清理，不使用开发 `.env` 或已有资源。
