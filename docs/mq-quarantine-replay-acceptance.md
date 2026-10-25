# P4.2.5.4：重放故障验收

P4.2.5.1—.4 范围完成。`TestQuarantineReplayAcceptanceIntegration` 在随机 MySQL/Redis/RabbitMQ fixture 中构建带 race 的实际 mqadmin 二进制，通过 CLI 子进程执行[重发](mq-quarantine-replay.md)和[终态核验](mq-quarantine-resolve.md)，核对故障窗口中的真实结果。唯一的应用回调屏障场景直接调用生产 `ReplayQuarantine`，解决仍由实际 CLI 执行。

## 十二项矩阵

| 场景 | 真实注入/屏障 | 核对与恢复 |
| --- | --- | --- |
| confirm 丢失 | AMQP 代理在 Confirm.SelectOk 后启用黑洞，实际读取并丢弃 broker basic.ack | CLI 返回 unknown/not_attempted，审计仍 open；显式新 attempt 重发，两条物理投递只形成一条业务消息 |
| Return | 只读拓扑核验和 confirm channel 建立后，fixture 暂时移除主绑定 | 实际 mandatory Return 不记录 attempt；恢复绑定后显式重发 |
| NACK | 同一时点切换绑定至私有已满 quorum 队列；预填充直到实际收到 NACK | 实际目标 NACK 不记录 attempt；恢复绑定并删除本轮容量夹具后显式重发 |
| 重发 COMMIT 未知 | SQL 代理读取真实 COMMIT OK 后丢弃并断开 | CLI accepted/unconfirmed；SQL 已是 replay_pending/同一 attempt；旧选择再次重发被拒绝，处理已发布消息后解决 |
| 解决 COMMIT 未知 | 同样丢弃实际解决事务的 COMMIT OK | CLI unconfirmed，无 resolved_at；SQL 已 resolved，再次核验返回 already_resolved，原时间不变 |
| 解决核验 ROLLBACK 未知 | 已 resolved 路径中丢弃真实 ROLLBACK OK | CLI unconfirmed；原审计和解决时间不变，显式核验恢复 |
| 发布后、审计更新前强杀 | fixture 持有审计行锁，processlist 确认实际 CLI 已在 RecordReplay 的 FOR UPDATE 等待，随后强杀该子进程并 Wait | 该锁定读只会在真实 publish confirm 后发生；审计仍 open，显式重发后两条物理投递幂等收口 |
| 两个 CLI 同时重放 | 同一行锁让两个进程都先获得旧快照并实际确认发布，再观察到两个等待的锁定读，最后释放 | 两个独立 attempt 都 accepted，仅一方 recorded，另一方 unconfirmed；SQL 不覆盖赢家，两条投递只形成一条业务结果 |
| 解决先于迟到重放回调 | 生产 ReplayQuarantine 的真实确认返回后暂缓回调；实际 CLI 先核验已提交业务终态并解决 | 释放回调后 RecordReplay 拒绝重开 resolved，输出 accepted/unconfirmed；迟到消息只复用原业务终态 |
| 仅确认发布、终态仍不存在 | 实际 CLI 已记录 replay_pending，但消息尚未消费 | resolve 拒绝；生产 handler 提交并 ACK 后才解决 |
| 已 rejected 的再次重放 | 目标不存在，生产事务形成 rejected，再次显式确认重发原命令 | 仍是原 TARGET_NOT_FOUND，无消息，只有原一条拒绝 Outbox；核验后解决 |
| SQL 入口不可用 | 关闭本轮独占 SQL TCP 代理，仅使 CLI 的 SQL 端点不可达 | Get 阶段失败且不发布，审计仍 open；恢复为直接连接后显式重发 |

SQL/AMQP 代理不修改协议负载，也不把 driver 人造错误冒充真实网络丢失。SQL packet framing 仅适用于固定夹具的 MySQL classic protocol：未启用 TLS/压缩/query attributes，不记录握手、凭证或查询正文。已有 SQL 代理增加独立的一次性 ROLLBACK 丢响应开关；AMQP 代理增加 mutex 保护、消费一次的 Confirm.SelectOk hook，默认行为不变。

Return/NACK 的拓扑变化发生在实际 CLI 已完成 VerifyLocal 后，故验证的是运行中漂移的发布判定，不绕过生产发布逻辑。容量预填充使用本轮私有 exchange，遵守 fixture 应用账号权限；所有哨兵消息只在私有容量队列中，由本轮夹具删除。未 purge 主队列或共享资源。

进程强杀只针对测试自己启动的 mqadmin PID，并等待真实退出；锁定读屏障证明已通过真实确认并进入审计更新阶段，没有依赖日志猜测时序。没有在生产代码加故障开关或修改发布/事务语义。

## 完整集合与资源

每条实际重放消息通过生产 PersistenceHandler → PersistCommand → 原 delivery ACK，重新核验 wire、完整规范内容/hash、MessageId/CorrelationId 和独立 attempt。夹具检查队列为空不代替业务事务或 ACK；每场景等待 broker ready/unacked 为零、ACK 覆盖实际处理条数、连接回到控制连接与 fixture publisher 两个 owner，再检查原主/隔离队列为空。

每场景还重新核验全部已有审计仍为 resolved、原始 v1 文档不变，包含源 binary、超过 JSON 精度范围的 int64、decimal、Unicode/NUL 及首份观察。wrapper/DLX 两种审计输入交替使用；DLX 为已验收协议夹具，本项没有重新运行 broker 自动死信。

最终集合为 **12 条唯一审计、12 条规范命令（11 persisted / 1 rejected）、11 条消息、23 条 Outbox、1 个会话、0 条 delivery cursor**。**16 个独立物理 attempt 均经生产 handler 处理并 ACK**，重复发布没有产生额外业务行；12 条审计均在实际终态核验后解决。最后复核 migration 指纹与正式拓扑，子进程、代理与测试项目资源全部清理。

原审计回归发现取消后的单次 Ping 可能命中过期物理连接，不能据此断言借入 pool 已关闭。夹具改为有界等待实际 Ping 可用，并额外确认隔离任务尚未 join、提交屏障仍未释放；真正关闭的 pool 不可能通过该检查。该调整不改变生产存储所有权或恢复逻辑。

## 命令与范围

```powershell
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestQuarantineReplayAcceptanceIntegration$' -count=1 -v -timeout=15m
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestQuarantineAuditAcceptanceIntegration$' -count=1 -v -timeout=12m
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQSessionIntegration$' -count=1 -v -timeout=8m
```

实际 runId、夹具修正前失败、回归和最终耗时见[实施进度](message-reliability-mq-progress.md)。生产 handler/仓储/CLI、依赖、配置模板和 migration 未改动，不执行自动提交。

P4.2.5 和 P4 暂停/恢复、可靠隔离、审计与受控重放这一实现范围收口。P4 整体角色退出/提交未知交叉仍依赖 Outbox 完整接入，保持未完成；默认 worker/all 门禁不变。[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；[P5.1.2 token 条件结算](outbox-settlement.md)已完成；[P5.1.3 通知执行循环](outbox-execution.md)已完成；下一项 **P5.1.4：生产角色工厂与接线**。尚未验收 Outbox 发布/lease、通知/客户端交付、完整默认 worker/all、三节点多数故障或磁盘损坏。
