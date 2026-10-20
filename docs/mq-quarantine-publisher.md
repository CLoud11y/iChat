# 隔离目标可靠发布（P4.2.3.2）

`rabbitmq.DialQuarantinePublisher` 提供只发布到隔离目标的独立、有界 session，输入为 [P4.2.3.1 隔离快照](mq-quarantine-envelope.md)。复用现有 publisher 的 slot、epoch、confirm/return 判定与 transport owner，不复用 API 或 consumer 的连接实例。本项不决定何时隔离，也不调用原 delivery 的 ACK。

## 使用与所有权

```go
options := rabbitmq.DefaultSessionOptions()
options.Publisher.ConnectionEpoch = 1 // 新连接的 epoch 由外部 owner 分配。
session, err := rabbitmq.DialQuarantinePublisher(ctx, address, options)
// 检查 err；显式 owner 最终 Close，不能把 pending 返回当作 join。
defer session.Close(context.Background())

snapshot, err := rabbitmq.NewQuarantineEnvelope(delivery.Message, "INVALID_DELIVERY", time.Now())
// 检查 err；失败时仍保留原 delivery。
result, err := session.Publish(operationCtx, snapshot)
// 仅 err == nil && result.Outcome == rabbitmq.PublishAccepted 证明目标发布成功。
// 原 delivery 的单条 ACK 接线、失败分类与恢复策略归 P4.2.3.3。
```

调用前应按现有正式拓扑契约 provision 并核验隔离 exchange、quorum queue 和 binding。此 session 不声明/修复拓扑，不提供新的配置开关，不自动重连、补 slot 或重新发送未知发布；默认 worker/all 缺失角色门禁继续保留。新连接/发布恢复策略由后续消费 owner 显式管理，不借用 API supervisor 或初始 consumer socket。

`SessionOptions` 与 P3.4 一致：slots 1—64、非零 connection epoch，正值且有界的 connect/operation/write/close timeout。入口即使接收 Background，也会添加 operation deadline；更早的调用者期限优先。初始化失败释放已经建立的 socket/channel。连接固定关闭客户端自动 Recovery。

`Done()` 表示停止准入；不等于所有调用已经返回。`Close(ctx)` 复用连接 owner：停止准入 → 在总关闭期限内关闭 channels/connection → 等待调用退出，必要时中断 socket。重复关闭保持幂等；错误/超时仍须由 owner 处理。session 格式化不包含 endpoint、凭证或快照。

## 发布协议

| 字段 | 值与责任 |
| --- | --- |
| 目标 | 固定 `QuarantineExchange` / `QuarantineRoutingKey`；调用者不能覆盖为主命令队列 |
| 选项 | mandatory=true、immediate=false、DeliveryMode=persistent |
| ContentType / Type | `application/vnd.ichat.quarantine.v1+json` / `ichat.quarantine.v1` |
| MessageId | 快照的稳定 `mq:v1:<sha256>` transport ID |
| CorrelationId | 每次取得 slot 后新生成的 128-bit 随机值，32 位小写 hex；不同于快照内的源 attempt |
| Body | 原快照的完整规范编码；每次重试不改写其时间、错误码或来源数据 |
| 其他属性 | 不把源 TTL、优先级、UserID、ReplyTo 或 broker headers 复制到外层；来源完整保存在快照内部 |

显式重试可产生两个真实隔离副本：它们 transport ID 和 body 相同，外层物理 attempt 不同。审计去重仍归未来隔离 worker，不因生成新 attempt 而创建新的业务身份。技术隔离 wrapper 与自动 DLX 的原始命令是两种输入；未来隔离 worker 需按契约验证和区分，不能只信任 Type/ContentType 标记。

## 共用确认状态机

原 `Publisher.Publish` 的写入与等待步骤抽为私有 `Publisher.publish`，原连接准备步骤抽为 `dialTargetPublisher`。命令入口保留 Command 校验、取得 slot 后生成原 server envelope 的顺序和公开 API；隔离入口只准备自己的 Publishing。目标在构造时固定，Return 校验使用该实例的目标，并复核正文、MessageId、CorrelationId、ContentType、ContentEncoding、Type 与持久化属性。

每 slot 仍只有一条 outstanding，原始 Return/Publish/Close 各一组容量 1 通知，单个执行者独占读取。匹配 ACK 的成功分支先同步排空原始 Return channel，再复核 context、通知、session 和序号；没有 Return 才归还 slot 并报告 accepted。不存在异步 Return map 或单独的隔离确认算法。

| Outcome | 对隔离目标的含义 | 源投递责任 |
| --- | --- | --- |
| accepted 且无错误 | 已获得匹配确认并排除 Return | 可以进入后续受控 ACK 步骤；本接口不 ACK |
| not_published | 输入/取消/容量/连接状态导致零网络写入 | 保留源投递 |
| unroutable | 收到匹配的 mandatory NO_ROUTE Return | 保留源投递；不能因随后 ACK 改为成功 |
| nacked | 收到匹配的 broker 负确认 | 保留源投递 |
| publish_unknown | 发布开始后的写错、确认丢失、取消/期限、通知错误或关闭 | 保留源投递；目标可能已有副本 |

`QuarantinePublishResult` 只包含 transport/物理 attempt、epoch/tag 和 outcome，不借用业务 CommandID 字段。错误复用 `PublishError` 与固定原因，不暴露快照、broker 回复文本或底层写错正文。Return/NACK/unknown 退休该 slot，迟到确认不会进入新 slot；全退休后 unavailable，外部显式新建 session 才恢复。

取消 watchdog 仅在 slot 已取得、快照/attempt 已准备且即将执行网络调用时安装。坏快照、预取消、busy 和其他零写入失败不会中止同连接的正常请求。网络写入、confirm 等待及失败清理均有整个 operation 上限；单次写 deadline 和可中断 socket 仍由原 P3.4 transport owner 提供，没有遗弃后台 RPC。

## 验证范围

固定 Go 1.27.1、仓库 `.gocache` 和不存在的 ICHAT_CONFIG_FILE：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^Test(Publisher|Quarantine(Publisher|Session))' -count=30 -timeout=90s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session|QuarantinePublisher)Integration$' -count=1 -v -timeout=12m
```

默认测试覆盖固定目标/完整快照/新物理 attempt、ACK 分支确定性先读 ACK 再发现原始 Return、NACK 与错误 tag/目标/正文/attempt/Type、写失败、通知关闭、旧 slot 迟到确认、零写入/池容量，以及真实 AMQP 客户端连接本地受控 peer 时的握手、confirm setup、阻塞写、丢失确认和关闭硬期限。另验证隔离 session 关闭后独立 API session 仍可发布。原 P3 publisher/session 默认回归也继续执行。

实际 Docker suite `TestRabbitMQQuarantinePublisherIntegration` 定义四项：并发发布相同审计快照及独立 API 连接；移除目标绑定的真实 Return+ACK；专属 vhost 临时容量策略产生真实 NACK；代理丢弃实际 broker confirm 后 unknown，显式新 session 重试留下两个稳定身份/不同 attempt 的完整副本。读取队列及 ACK 仅用于 fixture 检验/清理，不冒充审计事务或源 delivery 的业务 ACK。原 publisher/session 两套真实 suite 同时回归，验证共享内核改造。

实际执行结果及资源归属记录见 [实施进度](message-reliability-mq-progress.md)。这里不声称消费失败策略、源 ACK、隔离 SQL 审计、完整运行时退出或三节点可靠转移已验收。下一项为 **P4.2.3.3 消费处置接线**，完整转移交叉故障留给 P4.2.3.4。
