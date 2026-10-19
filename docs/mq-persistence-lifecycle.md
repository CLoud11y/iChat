# 持久化 worker 生命周期与角色适配（P4.1.2）

`workers.PersistenceService` 在 P4.1.1 单条处理器之上实现受控消费组，并满足 `application.RuntimeService` 和 `RuntimeConsumerReadiness`。`application.NewPersistenceFactory` 可将明确持有的共享 SQL pool 和执行器准备的持久化消费者接入 RunRuntime。

P4.1 的单条处理和生命周期范围完成。默认进程工厂仍只装配 API；隔离审计与 Outbox 尚未实现，worker/all 缺失角色门禁保持，不能把测试占位角色用于生产装配。默认 legacy、YAML/env、迁移和共享开发数据均未切换。

## 构造与激活

工厂及服务构造均不联网、不订阅、不探测或迁移，也不创建后台 goroutine。调用者显式传入共享 SQL pool；服务以该 pool 构造 P2 仓储，不读取旧全局 manager。

消费者列表限定 1—32 个独立、已准备的 session，prefetch 为 1—256。拒绝 nil/typed nil、nil/已关闭/重复的 Done channel；复制输入元数据，调用方更改原 slice 不会改写服务计划。工厂额外拒绝 publisher 和非主队列输入，不回读聚合 Health。

Start 要求有 deadline 的 setup context 和独立 lifetime。每个实例只激活一次，关闭后不能重启：

1. 检查上下文，以有界真实 MySQL Ping 确认初始存储可用。
2. 在共同 setup 期限中依次打开主队列订阅；保留所有返回的 handle，包括 handle+error，失败时统一回滚。
3. 全部订阅成功后再次 Ping，避免把较早的存储状态当作激活时的新鲜状态。
4. 发布激活状态后，每个 session 启动一个顺序消费循环和一个本地终止观察任务；另有一个存储探测任务及一个清理 owner。数量由连接数限制，不为每条消息新建后台事务 goroutine。

只有全部订阅/探测成功后才调用 Next/处理消息；setup 失败、取消、并发 Close 或部分打开失败不会启动业务处理。成功后的 setup 取消不影响 worker lifetime。

## 消费与失败

每个连接最多有一条交给处理器的 delivery。读取等待和处理分别有界，处理预算包含 P2 事务及原 handle 的单条 ACK。空队列的健康等待超时继续等待；取消与 delivery 同时到达时，停止后返回的 delivery 保持未确认，不再开始处理。

StopAdmission 与处理器之间有本地准入锁。停止前已准入的任务可继续；停止后即使 Next 返回成功，也不会新增业务准入。业务 rejected、同内容重放和 ID_CONFLICT 已由 P4.1.1 安全处置，nil 结果使循环继续。

任一处理错误、传输终止、存储探测失败、探测状态过期或受控任务 panic，都让整个持久化组停止领取。取消各方 poll/probe，保留已准入任务在各自处理预算内完成的机会，随后自动关闭已经打开的订阅。错误保留原始 errors.Is/As 原因，常用格式仅显示固定阶段，不打印 SQL、正文或 panic 内容。

P4.1.2 的停止策略仍用于每轮最终处理错误；后续 [P4.2.1](mq-persistence-retry.md)已让 handler 在同一处理期限内有限重试明确事务冲突，可通过 PersistenceOptions.Retry 配置。它不立即 requeue、重开 session 或主动隔离，失败/耗尽后的存储恢复不会让旧实例重新 ready。[P4.2.2.1](mq-persistence-supervisor.md)在此之外提供独立监督器来准备新轮次，本文单次服务本身仍不自动恢复或隔离。

## 退出与资源归属

| 操作 | 行为 |
| --- | --- |
| StopAdmission | 同步撤销就绪并停止新领取；取消等待/探测及未完成 setup，不做 RPC，不取消已准入处理 |
| Drain(ctx) | 停止准入，等待 setup、各消费循环及探测真实返回，包括事务/ACK；健康退出时保留订阅，等待 Close |
| Close(ctx) | 撤销准入、取消剩余处理，限时观察清理完成；可在 Start 前、部分失败后和并发调用 |
| Done / Err | Done 仅在自有任务及所有订阅 Close 调用真实返回后关闭；Err 提供保留的处理/清理错误 |

Drain/Close observer 过期返回 `ErrPersistenceServicePending`，保留 deadline 和已知错误原因。observer 超时不代表任务退出，后续可再次等待同一个 owner；不会重复 Close 订阅、释放共享 SQL 或丢弃晚返回的打开 handle。服务构造后闲置 Close 不触碰尚未打开的 session。

订阅在所有处理/探测 join 后并发 Close，共用一个清理预算。当前消费 API 的一个订阅对应一个专用 session，因此关闭订阅会退役该 session；MQResources 保留整组所有权并可随后幂等关闭。服务不关闭共享 MQ 组或 SQL pool。外部 SQL owner 必须等服务 Done 后才能关闭 pool，不能因为 RunRuntime/Close observer 已返回超时就提前关闭。

回调须遵守 context；对忽略取消的 SQL/测试回调不能承诺硬退出上限。它们真实返回前，owner、共享存储责任和 pending 状态继续保留。传输故障已使原连接失效时，仍依靠数据库幂等恢复，不能用旧 handle 确认新 channel。

## 就绪与参数

ConsumerReady 只读取本地激活/停止标志、lifetime、订阅 Done 和最近成功 SQL 探测时间，不执行 I/O。探测最大有效年龄为 `ProbeInterval + 2*ProbeTimeout`；超期立即不可 ready，迟到探测成功不能恢复已停止实例。它是时点观察，不能保证下一事务一定成功。

| Go 参数 | 默认 | 校验 |
| --- | --- | --- |
| PollTimeout | 1s | 正值，≤60s |
| ProcessingTimeout | 5s | 正值，≤60s，含事务与 ACK |
| ProbeInterval | 2s | 正值，≤60s |
| ProbeTimeout | 1s | 正值，≤60s |
| CloseTimeout | 5s | 正值，≤60s，全部订阅共用 |
| Retry | 3 次，25ms 基数/250ms 上限 | 完整零值沿用默认，详细校验见 [本地重试](mq-persistence-retry.md) |

这些是显式 Go 装配参数，本轮没有新增配置字段。订阅 setup 及网络读写/关闭还受到既有 MQ 期限约束，不能把处理预算当作网络硬上限。

## 验证与后续

```powershell
go test -mod=readonly -race ./workers ./application -run '^TestPersistence(Service|Factory)' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceLifecycleIntegration$' -count=1 -v -timeout=9m
```

默认测试验证闲置构造、参数/输入/角色门禁、setup/lifetime 隔离、顺序准入、晚 delivery、空队列、多 worker 失败停止、部分 handle 回滚、启动中 Close、处理/探测/closer 的真实 join、并发重复关闭、超时、panic、错误脱敏与共享 SQL 保留。

实际 Docker suite 包含 6 个场景：

- 真实 COMMIT 成功后的 driver 屏障使事务回调尚未返回；停止准入保留 work context，释放后完成 ACK，未处理的预取消息由新 session 接手。
- 强制 Close observer 超时后，已提交数据、未返回回调和共享 SQL 继续存在；解除屏障并 join 后原 attempt 重投，复用唯一 message/seq/两个 Outbox。
- 两个独立顺序 worker 处理八个业务身份及其重复 attempt；先观察队列正数，再核对实际 broker 归零，不仅凭首份消息落库就结束。
- loopback AMQP 代理实际丢弃消费 ACK，worker 本地成功；关闭后原 attempt 重投，已提交数据/seq/Outbox 不增加。
- 真实 NewPersistenceFactory → RunRuntime，处理器在实际 COMMIT 后进入 drain，聚合就绪变 stopping，最后 ACK/服务/MQ 依序结束，共享 SQL 仍可用。隔离/Outbox 为明确的测试占位角色，不提供审计或通知功能。
- 核验归属后停止本轮 MySQL，空闲服务也因真实 Ping 失败退出；MQ 可继续接受，存储恢复后旧服务不复活，显式新服务处理积压。最终核对消息数、最大 seq 和 last_seq 一致。

没有运行默认业务 main、切换模式、投递宿主 OS 信号或启用真实隔离/Outbox。P4.1.1 的强杀证据与本轮受控退出/ACK 丢帧证据均保留各自边界；未实现三节点、实际客户端和完整端到端恢复。

[P4.2.1 分类与本地重试](mq-persistence-retry.md)、[P4.2.2.1 独立监督器](mq-persistence-supervisor.md)及 [P4.2.2.2 运行时连接替换](mq-persistence-connections.md)已完成。本文的 PersistenceService 仍是每轮不可重启的子服务；显式 NewSupervisedPersistenceFactory 使用监督器和当前轮次健康，原 NewPersistenceFactory 保留单轮行为。下一项 **P4.2.2.3：监督恢复的运行时退出验收**；可靠隔离/审计继续分步推进，默认后台角色门禁保持。
