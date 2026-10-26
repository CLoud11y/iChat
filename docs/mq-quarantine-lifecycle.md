# P4.2.4.3：隔离 worker 生命周期与角色工厂

`workers.QuarantineHandler` 将[输入解析](mq-quarantine-input.md)和[幂等审计事务](mq-quarantine-audit.md)接入原隔离投递的单条 ACK。`QuarantineService` 管理每连接顺序消费，`QuarantineSupervisor` 管理数据库/连接故障后的分轮恢复，`application.NewSupervisedQuarantineFactory` 提供显式生产角色接线。

默认 worker/all 仍在连接建立前拒绝缺失角色，没有把 Outbox 或其他默认后台工厂作为成功占位。本项测试的 RunRuntime 使用真实隔离工厂；持久化和 Outbox 是明确的 fixture companion，没有运行完整默认后台管线。

## 审计与 ACK

handler 要求有界 context，只从原 `ConsumerDelivery` 取得 ACK 能力：解析完整输入 → 保存审计 → 核对回执 transport ID 和 inserted/existing 分类 → 检查 context → ACK 原 channel/tag，multiple=false。

| 结果 | 行为 |
| --- | --- |
| inserted / existing，回执身份匹配 | 审计已有持久依据，允许 ACK 当前原投递 |
| 解析失败、存储错误、COMMIT/ROLLBACK 未知 | 零成功结果、零 ACK；保留当前投递 |
| 回执身份/分类错误 | 不变量错误，停止且不 ACK |
| 审计成功后 context 取消/超时 | 保留 SQL 审计，不再 ACK；重投按原 transport ID 核验已有行 |
| ACK 写入失败/未知 | 保留审计，停止当前角色；不把 ACK 失败当成数据库恢复证据 |

隔离 worker 不 Reject、不重新隔离、不重发原命令，也不将审计状态设为 resolved。损坏 wrapper 没有足够材料形成可信审计时停止并保留原消息，不回退猜测。错误正常格式化只显示受控阶段，底层原因仍可通过 errors.Is/As 检查。

## 生命周期与恢复

隔离角色复用既有顺序服务和监督器的准入、SQL 探测、drain、任务 join 与分轮退役机制，专用构造器固定订阅 `QuarantineQueue` 并调用审计 handler；不配置主队列 publisher 或业务本地重试。每个连接只处理一条在途审计，正 prefetch 限制 broker/client 缓冲。构造与工厂 claim 没有 I/O，Start 才探测存储和打开所有订阅。

`QuarantineOptions` 提供 PollTimeout、ProcessingTimeout、ProbeInterval、ProbeTimeout 和 CloseTimeout，默认分别为 1s、5s、2s、1s、5s，均须大于零且不超过一分钟。`QuarantineSupervisorOptions` 另外控制有界 setup、抖动退避与稳定后退避重置。库级构造器支持 `config.MaxConsumerWorkers` 范围内的多独立连接；现有 `MQRuntime` 角色计划仍为一个隔离消费者，持久化 worker 数配置不改变隔离角色连接数。真实库级测试另以两个隔离连接验证并发审计。

数据库探测/明确基础设施故障撤销本角色就绪，停止领取，等待所有已准入审计/ACK 和探针真正返回，关闭并加入旧订阅及连接组，然后抖动退避。探测数据库成功后才获取下一轮拓扑验证过的连接；数据库离线期间探测失败不会继续获取消费者。连接关闭/可恢复 setup 失败同样按整组退役重建，不复用旧 handle。

恢复判断检查完整错误树。明确存储基础设施失败、存储阶段超时和带相容底层证据的审计未知结果可以恢复；解析、回执、审计冲突/不变量、SQL 约束/schema、ACK、显式取消、未知或混合不可恢复原因均停止。SQL 死锁/锁超时不在隔离 handler 中做本地重试。SQL Ping 只证明连接可用，不等于审计事务已完成或允许 ACK。

StopAdmission 撤销就绪并取消空闲领取/探测；已准入工作在正常 drain 中保留处理期限，仍可完成审计和 ACK。Close/lifetime 取消结束 ACK 机会，但没有返回的 SQL/探针/连接回调继续由原 owner 持有。短等待返回 pending 不是 Done；只有真实任务/资源回调加入后才关闭 Done。调用者必须在隔离监督器 Done 后才释放借入的共享 SQL pool，RunRuntime 返回本身不能替代该条件。

复用的公共观察错误保持原名：服务 pending 为 `ErrPersistenceServicePending`，监督器 pending 为 `ErrPersistenceSupervisorPending`，连接来源/清理也使用共用 sentinel。`QuarantineSupervisor.Status()` 使用相同的 idle/starting/ready/retiring/backoff/probing/stopping/stopped/failed 状态与 Generation；正常打印对象不会输出源数据或配置。

## 角色与连接所有权

`RoleDependencies.QuarantineConsumers` 是只给隔离角色的单次 claim 能力。`MQResources` 为主队列和隔离队列分别保存连接来源；claim 校验队列、数量、prefetch、唯一 Done 和实际初始连接集合，只从静态清理注册转移本角色的连接。两种角色的 capability 不可混用，重复 claim 被拒绝。

隔离来源沿用经过验证的 connection dial/Verify 和真实 WaitClosed 机制，拥有部分/迟到获取的连接，旧清理实际返回后才允许新一轮。只替换隔离队列的当前快照，不关闭主队列或 publisher。聚合消费者就绪读取各已 claim 来源的当前轮次；隔离旧连接关闭后，恢复就绪不再引用它。

`NewQuarantineFactory` 提供显式单轮服务工厂；`NewSupervisedQuarantineFactory` 提供可恢复工厂。两者借用 SQL pool、拒绝错误角色依赖，不关闭 pool。与持久化工厂一同做角色检查时，Outbox 缺失仍被门禁拒绝；没有在默认进程中绕过总角色清单。

## 验证与后续

固定 Go 1.27.1、空 GOROOT、仓库 `.gocache`、不存在的 ICHAT_CONFIG_FILE，不读取开发 .env/config：

```powershell
go test -mod=readonly -race ./workers ./application -run '^Test(Quarantine|Persistence)' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestQuarantineLifecycleIntegration$' -count=1 -v -timeout=12m
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceConnectionsIntegration$' -count=1 -v -timeout=12m
```

默认测试覆盖提交/已有审计后才 ACK、全部失败零 ACK、错误隐藏、完整恢复证据、graceful/pending 的迟到保存、空闲工厂/门禁、隔离 capability/单 claim、仅本角色快照替换和 join 超出 observer 期限。

真实六场景覆盖双隔离 worker 的 wrapper 重复和实际 broker DLX 合并审计、数据库停机积压及恢复、空闲隔离连接替换、真实 COMMIT 后返回屏障中的正常 drain、RunRuntime pending 后的 SQL owner/原文重投，以及篡改 wrapper 停止且不 ACK。成功场景等待 broker 实际 ACK 统计达到已发布物理投递数，再直接检查队列为空；SQL 行存在不冒充消息已确认。旧持久化连接 suite 回归共用来源的既有四项行为。

COMMIT 后屏障是测试 driver 在真实提交成功后持有返回，不是网络丢包或进程强杀；Malformed 场景最终 ACK、pending 场景检查后的 Nack 和原始主队列 Reject 都仅是 fixture 检查/清理或触发自动 DLX，不是生产隔离策略。后续[P4.2.4.4 审计故障验收](mq-quarantine-audit-acceptance.md)已完成真实 SQL/ACK 响应丢失、进程强杀与双角色恢复/退出，P4.2.4 范围收口。实际结果、runId 与清理记录见[实施进度](message-reliability-mq-progress.md)。[P4.2.5.1 只读查询与选择](mq-quarantine-query.md)、[P4.2.5.2 确认式重发](mq-quarantine-replay.md)及[P4.2.5.3 终态核验](mq-quarantine-resolve.md)已完成；[P4.2.5.4 重放故障验收](mq-quarantine-replay-acceptance.md)已完成，P4.2.5 范围收口；[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；[P5.1.2 token 条件结算](outbox-settlement.md)已完成；[P5.1.3 通知执行循环](outbox-execution.md)已完成；[P5.1.4 生产角色与生命周期](outbox-lifecycle.md)已完成；[P5.1.5 故障验收](outbox-acceptance.md)已完成，P5.1 收口；下一项 **P5.2：统一 WebSocket 帧与通知权限核验**。P4、Outbox、三节点和完整默认后台角色仍待完成。
