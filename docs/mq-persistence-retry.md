# 消费失败分类与有界事务重试（P4.2.1）

持久化处理器现在只对明确的事务冲突做有限本地重试，保留同一个 command、delivery 和 ACK handle。所有 SQL 尝试、退避及最终 ACK 共用原处理 context，不重新发布 MQ，不领取下一条，也不延长处理期限。

业务 persisted/rejected、同内容重放和 ID_CONFLICT 仍是仓储的正常终态，不作为技术错误重试。默认后台装配门禁、legacy 配置和数据保持原状；数据库连接故障的跨轮次恢复见后续 [独立监督器](mq-persistence-supervisor.md)，生产运行时接线及可靠隔离继续留给后续 P4。

## 分类与责任

`ClassifyPersistenceFailure(err)` 按类型、固定错误号和责任阶段分类，不匹配错误文本。检查完整的单/多错误展开树，避免只取第一个 MySQL 错误而掩盖并存的连接、回滚或未知错误。服务的阶段包装不改变底层 delivery 的类别。

| 类别 | 例子 | 当前处理 |
| --- | --- | --- |
| 空类别 | 成功，包括业务拒绝/重放/冲突 | 仓储终态校验后单条 ACK |
| `transaction_retryable` | MySQL 1213 死锁、1205 锁等待超时 | 仓储已结束事务，才允许在同一 delivery 内有限重试 |
| `infrastructure` | EOF、连接错误、driver bad connection、已知容量/访问/部署故障 | 不做本地事务重试，不当作坏消息，返回失败并保留恢复责任 |
| `commit_unknown` | COMMIT 报错，即使底层是 1213 | 不 ACK、不本地重试、不补偿删除，通过新投递幂等恢复 |
| `rollback_unknown` | 回滚调用失败，即使原错误是 1205/1213 | 不本地重试，保留原错误与回滚不确定性 |
| `cancelled` | 处理取消或期限耗尽 | 中止等待，不新增尝试、不 ACK |
| `acknowledgement` | 仓储成功后 ACK 阶段失败/取消 | 不重发 ACK，不进入事务重试循环 |
| `malformed_delivery` | 严格 wire、版本、身份/hash 校验失败 | 零 SQL/ACK，保留原文与 headers |
| `technical` | 终态不变量、已知 SQL/约束/表结构错误 | 不自动重试，保留诊断和原投递 |
| `unclassified` | 未知错误号/类型、只有类似死锁文字的错误 | 保守停止，不自动重试或隔离 |

`technical` 也可能表示程序或 schema 故障，不证明某条业务消息是坏消息。**任何类别都不单独授权 ACK、Reject 或隔离**。持续基础设施故障、未知错误及重试耗尽不能被重写为“坏消息已处理”。同一树出现提交/回滚未知时优先保留这些责任；ACK 阶段和取消也不会因带有暂时错误号而被误当事务冲突。

本地重试的白名单依据 MySQL 的事务边界：死锁会回滚整个事务，锁等待超时通常只回滚当前语句，因此处理器必须等 P2 仓储完成整笔事务的回滚后才能重试。[InnoDB 错误处理](https://dev.mysql.com/doc/refman/8.0/en/innodb-error-handling.html)、[服务器错误号](https://dev.mysql.com/doc/mysql-errors/8.0/en/server-error-reference.html)。

## 仓储回滚边界

`MessageRepository.ExecuteCommand` 增加 `ErrCommandRollbackUnknown`。defer 回滚失败时，将该标记、原始事务错误和回滚错误一起保留，并返回空 execution。只忽略 database/sql 自己的精确 `sql.ErrTxDone`；包含其他错误的包装/组合不借此掩盖回滚失败。

现有 `ErrCommandCommitUnknown` 保持原含义。提交未知、回滚未知比原始 MySQL 冲突号优先，不能因为 `errors.As` 找到一个 1213 就启动另一笔事务。没有更改 SQL 锁顺序、事务写入、权限、seq 或 Outbox 原子性。

## 重试参数与执行

`NewPersistenceHandler` 使用默认策略；`NewPersistenceHandlerWithRetry(repository, options)` 显式配置。`PersistenceOptions.Retry` 将同一策略传给托管 worker，`NewPersistenceFactory` 在闲置构造前校验，无新增 YAML/env。

| Go 参数 | 默认 | 范围/含义 |
| --- | --- | --- |
| MaxAttempts | 3 | 1—5，含第一次仓储调用；1 表示不再尝试 |
| BaseDelay | 25ms | 正值，指数退避的起始上限，不是实际最短等待 |
| MaxDelay | 250ms | ≥BaseDelay，且≤5s |

完整零值策略沿用默认值，支持既有未填 Retry 的 Go 装配；部分填写但不合法则拒绝。第 n 次失败后的等待上限为 `min(BaseDelay*2^(n-1), MaxDelay)`，在该上限一半到上限之间随机选择正等待，避免零间隔重试。等待用原 context 的同步 timer，可取消，不产生额外后台任务。

每次必须等待上一笔仓储事务完整返回，再退避并重试；中途变为连接、约束、提交/回滚未知或取消时立即停止。只有最后仍是明确事务冲突且次数耗尽时，错误附 `ErrPersistenceRetriesExhausted`，原类别仍为 `transaction_retryable`；不把它改为技术坏记录。

`PersistenceError.Attempts` 记录本次 delivery 的仓储调用数，错误时 execution 始终为空；不是数据库驱动内部连接尝试数。底层错误仍能通过 errors.Is/As 检查，常用格式不打印原文、SQL 或凭证。成功后依然只调用一次原 handle 的单条 ACK，不为每次事务尝试 ACK。

ProcessingTimeout 默认仍为 5s，覆盖全部尝试/等待/ACK。若锁等待先耗尽该预算，会取消而不会保证多次尝试；本轮没有调低生产 MySQL 锁等待期限。StopAdmission/Drain 保留已准入重试的 work context；强制 Close 取消退避，不新开事务、不 ACK，仍等待工作真实 join 后关闭订阅。

## 验证与后续

```powershell
go test -mod=readonly -race ./workers ./database ./application -run 'Test(Persistence(Retry|Failure|Factory)|Command(HandlerFailure|ReplayRollback))' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration -p 1 ./tests/testenv ./workers -run '^Test(PersistenceRetryIntegration|PersistenceConsumerIntegration)$' -count=1 -v -timeout=12m
```

默认测试覆盖完整错误树优先级、未知文字不匹配、生命周期包装、成功后单次 ACK、相同身份/context、次数和抖动边界、耗尽不改类别、错误类别中途变化、取消/期限、ACK 失败不重试、回滚错误不被 sql.ErrTxDone 包装隐藏及工厂拒绝坏策略。

真实 MySQL/RabbitMQ 新 suite 有 8 个场景：

- integration driver 在实际 message INSERT 后加入同一事务的测试表锁，构造真实 MySQL 1213/1205；前者用重事务作 blocker，后者仅对测试连接设 1s lock wait。仓储实际回滚后，本地第二次完整事务成功，只有一份 message/seq/Outbox。
- 持续实际锁超时到三次，三笔事务回滚、零提交，原 delivery 仍 busy；关闭后同 publishAttempt 重投并成功，没有提前处置。
- 实际回滚成功后向调用方注入 EOF，即使原错误是实际 1205，仍为 rollback_unknown 且只调用仓储一次。
- 实际 COMMIT 成功后向调用方注入 1213，仍为 commit_unknown 且一次调用；原投递重放已有终态。以上两项是调用结果注入，不是 TCP 响应丢包演练。
- 在本轮 schema 给下一 seq 增加临时 CHECK，获得实际 3819；一次回滚，不重试/ACK。去掉约束后原 attempt 可恢复。
- 托管服务进入本地退避后 graceful drain，仍可重试成功并 ACK；另一个场景强制 Close，立即取消退避、零额外尝试/提交，原 delivery 后续恢复。

既有单条 suite 增加 COMMIT 未知与数据库实际离线的“一次尝试及类别”断言，继续覆盖提交前后强杀重投。既有生命周期 suite 回归多连接、实际 ACK 丢帧及 RunRuntime drain。最终核对唯一终态/消息/事件和连续 seq；所有 SQL 锁/约束故障仅作用于本轮独立 fixture，生产无故障控制接口。

P4.2.1 范围完成。每轮服务在最终错误后仍停止整组领取并受控关闭。后续 [P4.2.2.1 独立监督器](mq-persistence-supervisor.md)及 [P4.2.2.2 运行时连接来源](mq-persistence-connections.md)已能在明确数据库/消费连接故障后退役旧轮次、抖动探测并安全替换连接；它们不把本地重试耗尽或 ACK/技术错误自动换轮。下一项 **P4.2.2.3：监督恢复的运行时退出验收**；可靠隔离转移、隔离审计/重放、Outbox 和完整客户端恢复尚未完成，默认 worker/all 门禁继续保留。
