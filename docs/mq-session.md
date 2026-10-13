# RabbitMQ 发布连接的 I/O 和关闭边界

P3.4 拆为两个可审查小点：**P3.4.1 有界连接、网络写入、确认等待与关闭（已完成）**；P3.4.2 supervisor 退避重连、单调 epoch 和新连接拓扑复核也已完成，见 [MQ supervisor](mq-supervisor.md)。当前仍是独立库与隔离测试，业务入口、HTTP 202 和消费者尚未接入。

## 推荐入口

```go
options := rabbitmq.DefaultSessionOptions()
options.Publisher.ConnectionEpoch = 1 // 每次新连接由所有者分配，后续 supervisor 管理。
session, err := rabbitmq.DialPublisher(ctx, amqpURL, options)
// 检查 err；session 拥有 socket、AMQP connection 和 publisher channels。
defer session.Close(context.Background())

result, err := session.Publish(requestCtx, normalizedCommand, time.Now())
// 只有 err == nil 且 result.Outcome == rabbitmq.PublishAccepted 才已确认接收。
```

默认配置：8 slots，connect=5s，operation=5s，单次 write=3s，close=3s，heartbeat=2s。四个 timeout 必须为正且不超过一分钟；heartbeat 必须为 1—60 秒的整秒值。slots/epoch 沿用 P3.3 约束。调用方 context 更早到期时采用更早边界；Publish 即使使用 Background 也有 operation 上限。

`DialPublisher` 显式拨号，connect timeout 覆盖 TCP/TLS/AMQP 握手，operation timeout 覆盖全部 channel/confirm 初始化。Recovery 固定 nil。TLS 配置克隆后使用，不改调用方对象，最低 TLS 1.2 且拒绝跳过证书校验；明文 AMQP 只允许 localhost/loopback。URL 必须显式提供账号/密码，不接受 query/fragment 覆盖超时或 heartbeat。错误及 Session.String 不包含 URL 凭证、broker 原始错误或消息正文。

## 如何中断阻塞

底层 transport 为每次 Write 设置 write deadline。单次 Write 超时、Read 失败/EOF 或 session 的 RPC/发布取消 watchdog 触发时，先关闭 admission，再直接关闭底层 socket；不先等待 AMQP channel.close。`net.Conn.Close` 释放实际正在等待的 Read/Write，固定客户端的 reader shutdown 随后释放 channel RPC。[Go net.Conn 契约](https://pkg.go.dev/net#Conn)、[固定客户端连接实现](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/connection.go)

发布 watchdog 在取得 slot 并进入网络调用前才启用。已取消、非法 Command、错误时间或 busy 的零写入请求不会因为自己的 deadline 中止别人的连接。operation timeout 覆盖写入、确认等待及后续失败处置；单次 Write deadline 不能替代整个操作 deadline。

每个 active Publish、slot 退休/关闭或初始化/关闭阶段最多一个有界取消回调。回调只停止接单并关闭 socket，返回前停止/等待该回调完成。不会把阻塞 RPC 放进一个被超时请求遗弃的 goroutine。[context.AfterFunc 的停止与完成语义](https://pkg.go.dev/context#AfterFunc)

同连接上尚未取得完整证明的在途请求均为 publish_unknown；已有完整匹配 Return/NACK 的明确结果保留，已完成 accepted 不被之后的关闭撤销。消息可能已经在 broker 中，重试仍保留原 clientMessageId 和原内容、重新生成 attempt。此处不自动重发命令。

`Done()` 在连接中止或显式停止接单时关闭，可供连接所有者观察；不代表所有 AMQP 任务已完成析构。单个 channel 的明确 Return/NACK 正常退休后会降低 slot 数；连接未损坏时不自动重建 slot，耗尽时 Publish 返回 unavailable。P3.4.2 将管理这些恢复情形。

## 关闭与所有权

`Close(ctx)` 先拒绝新请求，再关闭自身 channels 与 AMQP connection。close deadline/watchdog 覆盖整个关闭过程，不按 slot 数累加等待；关闭响应缺失时强制关闭 socket，返回 deadline/关闭失败。它等待当前 session 的调用退出，若等待耗尽则返回 deadline，而不是无限等待。重复关闭保持幂等；并发关闭等待第一轮或遵守自己的更短 deadline。

裸 `NewPublisher(connection, options)` 仍属于 P3.3 的底层接口，调用者拥有连接并自行负责有界 I/O。新 `PublisherSession` 拥有全部资源、没有公开 connection/channel 或允许外部注册通知的入口；对应硬超时承诺只适用于这个有界入口及正常 net.Conn 实现。Go 调度/系统负载仍影响实际唤醒时间，配置值不是毫秒级 SLA。

单个 session 不自行恢复、不声明/持续复核拓扑、不分配单调 epoch。P3.4.2 supervisor 已在新连接接单前只读复核拓扑并重建 session。P3.5.1 已提供应用配置；P3.5.2 已增加 [消费者基础连接](mq-consumer-session.md) 和 [消费 channel / 手动 ACK](mq-consumer-channel.md)，复用每实例独立的连接内核，没有共享 publisher/consumer socket 或修改确认状态机。主程序装配与持续 readiness 仍待后续小点。生产三节点、磁盘损毁、MySQL 终态和端到端幂等接线也不属于此次验收。

## 验证

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestSession' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session)Integration$' -count=1 -v -timeout=8m
```

纯单元测试使用实际 v1.15.0 客户端和受控 AMQP wire 对端/net.Pipe，不读开发配置、不启动 Docker。覆盖对端不读协议头、握手无响应、channel/confirm 初始化无响应、真实 Write 背压、消息已发送但确认不回、两个 slots 共享取消、关闭 RPC 无响应、空闲 EOF、前一请求 watchdog 已停止、预写/忙请求不伤害连接，以及已进入 Write 的请求被并发 Close 中断。另用本机真实 TCP 端口验证 TLS 握手黑洞及 socket 关闭。

Docker 用例沿用 P3.1 本轮专用 broker/vhost，准备 P3.2 正式拓扑。TCP 代理在 session 初始化后丢弃真实 broker 的下行 ACK/心跳，观察实际 ACK 被截获后取消请求；两个请求都返回 unknown，随后从独立连接读取 broker 中完整消息。另覆盖 operation deadline、Return 后的 channel.close-ok 被丢弃、connection.close-ok 被丢弃和正常关闭。代理关闭会停止 acceptor、关闭两端 socket 并等待两条转发任务退出；全部 broker 资源按归属清理，不使用开发 `.env` 或现有容器/卷。
