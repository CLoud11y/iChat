# RabbitMQ publisher 重连、epoch 与拓扑就绪

P3.4.2 在 P3.4.1 owned session 上增加单一 supervisor：有界失败退避、每次尝试独立 epoch、旧 session 停止/回收，以及新连接复核后接单。P3.5.4.1 又增加[已就绪连接的周期拓扑复核](mq-topology-monitor.md)。P3.5.1 已提供 [应用配置](mq-application-config.md)，P3.5.2 已有独立的 [消费者连接](mq-consumer-session.md) 与 [消费 channel / 手动 ACK](mq-consumer-channel.md)，P3.5.3 已完成基础装配；默认现代业务工厂仍为空，[接受就绪汇总与健康接线](runtime-readiness.md)已完成装配基础，生产接受 handler 仍待 P3.6。

## 调用

管理 API 必须指向 AMQP 目标的同一个 broker。应用使用限制权限的 AMQP 账号，management 凭证仅用于只读核验。vhost 参数必须与 AMQP URI 解码后的 vhost 完全一致，包含其前导 `/`。

```go
options := rabbitmq.DefaultSupervisorOptions()
supervisor, err := rabbitmq.StartSupervisor(lifetimeCtx, amqpURL, management, vhost, options)
// 检查 err；StartSupervisor 校验配置后立即返回，后台连接。
defer supervisor.Close(context.Background())

epoch, err := supervisor.WaitReady(startupCtx)
// 检查 err；或者由进程 health 集成读取 supervisor.Status().Ready。
result, err := supervisor.Publish(requestCtx, normalizedCommand, time.Now())
// 只处理本次结果，不自动重发 unknown 命令。
```

未就绪时 Publish 立即返回 not_published/unavailable，不存储待发布命令。busy、非法输入和预取消请求不使健康 session 退休。关闭或 lifetime context 取消之后拒绝新请求，取消后的初始化结果也必须清理、不能安装为 ready。

## 恢复顺序

一个 supervisor 只有一个恢复 owner。当前 session EOF、unknown、Return、NACK 或通知/session 异常时，先从 active 中移除、停止 admission、关闭 socket；该 epoch 其他尚未取得完整证明的请求按 P3.4.1 返回 unknown。等待 session 自有调用 quiesce 后才尝试下一连接，旧调用未能在关闭边界内退出时进入 failed，拒绝重叠创建新 epoch。

新连接在 setup 的 deadline/socket watchdog 下检查实际 broker 版本与 vhost，对 management 的版本、feature flags、queue/exchange 参数、在线 quorum 成员与唯一绑定做核验，并在该 AMQP 连接上执行 passive exchange/queue 查询。Passive 本身不足以证明有效 queue arguments/bindings，因此随后再次读取管理契约；全部通过后才创建全新通知/confirm slots 并安装为 ready。

恢复路径只读，不调用 EnsureLocal、创建绑定、删除/purge 队列、改策略或启用 feature flags。初始拓扑由 [mqadmin init](mq-local-topology.md) 准备。错误类型、TTL/策略、缺失对象/绑定、副本不符或权限错误均阻止就绪；人工按授权流程修复后下一尝试重新复核。

任何旧请求的后续错误都带着原 session 引用，只有它仍是 active 时才能触发 invalidation；旧结果不能关闭替代 session，旧通知也不被转发到新连接。每次连接尝试均消耗新 connection epoch，包括失败的握手/拓扑核验；成功新 channel 的发布 tag 从 1 开始，channel epoch 与原始通知空间重新创建。epoch 在**单个 supervisor 生命周期内**单调递增，不是跨进程持久化序号；uint64 耗尽进入 failed，不回绕。

默认 backoff base 从 1 秒指数增长到 30 秒；每次实际等待在 base 的一半到 base 之间随机抖动。RetryMin 至少 1ms、RetryMax 不小于它且不超过一分钟。成功安装后重置 base；dial、setup、等待和关闭均可取消。Status 只包含状态、epoch、ready 与脱敏 sentinel，不包含原始拨号错误/凭证。

## 发布语义与剩余范围

supervisor 每次 Publish 只调用一次当前 session 的 Publish。连接恢复不会自动重放命令；unknown 不证明未入 MQ，调用者用原 clientMessageId/原内容明确重试，新的服务端 attempt ID 标识新尝试。既有 accepted 不被之后的重连撤销，消费者仍须以数据库幂等事务处理可能重复的逻辑命令。[RabbitMQ 失败后的重复与幂等边界](https://www.rabbitmq.com/docs/reliability)

Status/WaitReady 是时点观察；除每次新连接核验外，已就绪连接现在也会按 TopologyCheckInterval（默认 10s）串行复核。失败或超时退休当前 epoch；成功保持就绪，检查结束后重新计时。检查计入旧连接的 quiescence，不会与新 epoch 重叠；不自动修复或重发。周期观察存在发现窗口，具体参数、取消和测试边界见[周期拓扑复核](mq-topology-monitor.md)。管理端点与 AMQP 的部署对应关系由调用方配置；没有实现集群服务发现、跨主机自动更换 URL、三节点成员契约或多 API 会话协调。P3.5.4.2 [接受就绪汇总与健康接线](runtime-readiness.md)已完成装配基础；生产 HTTP accepted 仍待 P3.6。

## 验证

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestSupervisor' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session|Supervisor)Integration$' -count=1 -v -timeout=10m
```

单元测试验证单调 epoch/失败退避、核验前零发布、旧调用回收屏障、无法 quiesce 时失败关闭、旧结果不污染新 session、Return/NACK/unknown 恢复、busy 不触发恢复、各阶段取消、早死 session 重试、epoch 耗尽与 lifecycle 取消。

Docker 用例通过真实 TCP 代理丢弃第一个连接的确认帧，验证 unknown 后新 epoch/tag=1、原消息只有一份且没有自动重放，明确重试时使用新 attempt。移除主绑定后 Return 使 session 退休，重连始终未就绪且绑定不被自动重建；仅恢复本轮测试绑定后才再次 ready。停止并重启本轮 broker、保留数据卷，验证未消费的 accepted 原文保留、新连接恢复接单。

fixture 默认使用随机宿主端口，实测 Docker stop/start 会重新分配。这不是 supervisor 自动更换 endpoint 的功能；该重启用例检测后用本轮临时 Compose override 固定回原端口，保持 supervisor URL 不变。覆盖 stop/up/recreate 前都核对 Docker 项目和 run 标签，仅操作本轮 broker；命名卷、MySQL/Redis 和开发 `.env` 不受影响。代理支持本轮多次连接，结束时 join acceptor/转发任务并按归属清理。
