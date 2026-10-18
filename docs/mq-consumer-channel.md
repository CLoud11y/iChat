# 有界消费 channel 与手动 ACK（P3.5.2.2）

`ConsumerSession.OpenConsumer` 在已经核验的独立消费者连接上提供消费订阅、顺序读取和单条 ACK/Reject。当前是库与隔离测试接口；按角色 MQ 准备层可持有空闲消费者连接。[P4.1.1 持久化消费适配](mq-persistence-consumer.md)已将单条 delivery 接入 P2 完整事务并在成功后 ACK；生产 worker 生命周期与隔离审计仍待后续 P4。

## 调用与所有权

```go
session, err := rabbitmq.DialConsumer(startupCtx, amqpURL, management, vhost, rabbitmq.DefaultConnectionOptions())
// 检查 err，显式持有 session 生命周期。
defer session.Close(context.Background())
stream, err := session.OpenConsumer(setupCtx, rabbitmq.PersistQueue, 16)
// 检查 err。
delivery, err := stream.Next(waitCtx)
// 检查 err；delivery.Message 包含原文与属性，没有 AMQP Acknowledger。
// P4 worker 执行幂等事务并确认数据库提交成功后：
err = delivery.Ack(ackCtx)
// 检查 err；错误时停止该实例，重投通过数据库幂等处理。
```

首版每个 session 生命周期只创建一个消费 channel 和一个订阅。多个 worker 使用多个长连接 session，每个 worker 在自己的 channel 顺序处理；生产者连接始终独立。关闭 channel 等价于关闭其所属消费者 session，不自动重开。这个选择避免把旧 channel 的 tag/ACK 交给替代 channel，也避免一个 worker 的传输取消影响其他 worker。

队列仅允许 `PersistQueue`、`QuarantineQueue`。prefetch 为 1—256，0（无限制）与超界值拒绝；`basic.qos` 设置 count、size=0、global=false，并等待 qos-ok 后才调用 `basic.consume`。consume 使用显式随机 tag，autoAck/exclusive/noLocal/noWait 都为 false，等待 consume-ok。API 不接受任意 queue、批量 ACK、外部 tag 或原始 AMQP channel。

Open 的 channel.open、qos、consume 全部在一个 operation deadline/socket watchdog 下完成，沿用 connect/write/close 硬界。初始化 context 不控制成功返回后的订阅生命周期；使用普通 Consume，不由短期 setup context 自动发送 basic.cancel。超时或失败退役该消费者连接，调用方关闭并显式重新创建。

## 读取、确认与失败

`Next(ctx)` 最多向顺序 worker 交付一条尚未处理的 delivery。它未 ACK/Reject 前，再次 Next 返回 busy；并发 Next/处置也立即 busy。broker 正向 prefetch 限制客户端缓冲中的未确认数量，不添加自己的预取 goroutine 或无界消息缓存。这是消息数量上限，不是任意消息体的字节容量保证；协议校验/技术坏记录责任转移在 P4 完成。

Next 的等待采用调用方更早 deadline 或 session operation 上限。空队列超时/取消不写 AMQP 方法、不关闭健康连接，worker 可继续等候。交付与取消同时发生时可能成功返回 delivery；调用方依照返回结果处理其责任。接收不会自动 ACK。零 wire tag、错误 consumer tag、流关闭或 broker cancel/channel failure 会退役该消费者实例，未确认消息依靠 broker 关闭重投。

`ConsumerDelivery` 的私有记录绑定原 channel 和原 tag，公开 Message 元数据的修改不改变网络确认对象。复制 delivery 仍共享同一处置记录；成功或已经尝试的记录不能再次处置，旧记录不能确认下一条消息，旧 session 的记录不能确认重连后的同 tag 消息。

`Ack(ctx)` 固定发送 `basic.ack` 且 multiple=false。`Reject(ctx, requeue)` 固定单条 basic.reject；主队列 requeue=true 可重投，false 由既有可靠 DLX 转入隔离队列。隔离队列没有下一跳 DLX，因此禁止对它 Reject(false)，保留审计责任；其 ACK 仍需由后续审计 worker 先完成数据库保存。基础设施整体故障的退避策略属于 P4，不能靠这里的 Reject(true) 热循环实现。

ACK/Reject 的 nil 错误只证明本地发送方法完成，**没有 broker 对消费 ACK 的确认回复，也不证明数据库事务提交**。本轮真实测试在本地 ACK 写入成功后，由代理丢弃这一帧，再关闭连接，broker 仍完整重投同一个 attempt。worker 因此必须依靠数据库幂等，而不能把网络 ACK 的返回值当作恰好一次保证。[RabbitMQ 确认边界与 channel 内 delivery tag](https://www.rabbitmq.com/docs/confirms)、[prefetch 语义](https://www.rabbitmq.com/docs/consumer-prefetch)

预取消、nil context、busy、重复记录及隔离保护是零写入拒绝，不影响健康连接或其他在途工作。一旦开始处置写入，写失败、deadline/取消、连接中止或与关闭竞争时返回 `ErrConsumerDispositionUnknown`（context 错误可由 errors.Is 判断），退役自己的 socket，不自动重发该 ACK/Reject。旧 delivery 不能被拿到新 session 重试，后续实际重投产生新 handle。错误不附原始 broker 文本、正文或凭证；delivery 的常用 fmt/GoString 格式隐藏内容。

## 通知与关闭

每个消费 channel 只注册容量 1 的原始 NotifyClose / NotifyCancel，单个有界 monitor 负责观察，异常只中止这个消费者 session。Done 是 session 停止 admission 的信号。Close 先关闭 admission，使等待 Next 和 monitor 退出，再在原 close deadline 下完成连接关闭并等待在途调用；阻塞 ACK 的 socket Write 由取消 watchdog 直接中断，未持有 delivery mutex 等待网络。

关闭没有自动 ACK 或自动 Reject；当前 delivery 和客户端尚未交付的预取消息均留给 broker 重投。正常 worker 退出可先停止业务循环、限时等待事务与正常 ACK，再关闭 session；未完成操作按重投和幂等处理。传输和执行器的受控关闭已装配；生产 consumer 的自动恢复、在途事务/ACK drain 和数据库提交不确定处理仍归 P4。

## 验证

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestConsumerChannel' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(ConsumerChannel|ConsumerSession|Publisher|Session|Supervisor)Integration$' -count=1 -v -timeout=12m
```

受控 AMQP wire peer 检查真实 qos/consume flags、单条 ACK/Reject tag 和 multiple/requeue 位；覆盖 metadata 修改、复制/重复 handle、非法输入/预取消/busy、空队列等待、broker cancel/坏 tag、初始化 RPC 黑洞、真实 Write 阻塞和并发 Close。通知任务、请求和 peer 均退出，不依赖 Docker 或开发配置。

Docker 新 suite 使用本轮独立 broker/vhost/账号：

- 实际 ready=1、unacknowledged=2 证明 prefetch=2，顺序 worker ACK 后三条命令都完整消费。
- 关闭持有一条 delivery 且另有预取的订阅，新的 session 完整读回全部原 attempt；旧 handle 无法确认新 channel 的 tag=1。
- 单条 reject/requeue 后 redelivered，同 attempt 再 reject(false) 经可靠 DLX进入隔离；审计 headers 和原文保留，隔离 Reject(false) 被拒绝。
- 先截获生产者真实 confirm，再用独立消费者 ACK/正常关闭完成责任释放；生产者仍在等待，取消后为 unknown。
- 仅在 ACK-loss 代理上解析上行 AMQP、记录并丢弃一条消费 ACK；本地 Ack 返回 nil，但关闭后新 session 获得同 attempt 的 redelivery。其余代理场景保持原 io.Copy 上行路径。

该 suite 通过本 API 接收并手动处置消息，全部是明确的隔离测试操作，没有数据库落库。五个 suites 的集中回归覆盖已有 publisher、session、supervisor 与消费者连接，资源按本轮项目/run 标签清理。默认配置仍走旧消息链路，显式 api 已支持 MQ 接受；worker/all 仍缺失后台业务角色，见 [P3 验收对照](mq-reliability-acceptance.md)。
