# P4.2.5.3：终态核验与 resolved

新增 `mqadmin quarantine resolve` 和 `QuarantineRepository.Resolve`。只有在同一 SQL 事务中确认匹配原身份/hash 的规范 persisted/rejected，才能把审计标记为 resolved；[重发](mq-quarantine-replay.md)成功或存在 replay_pending 状态都不能代替业务终态。

## 使用与结果

先通过[只读详情](mq-quarantine-query.md)取得精确 transport ID 和当前 replay_attempt_id：

```powershell
$auditId = '替换为完整 transport ID'
go run ./cmd/mqadmin quarantine show --transport-id $auditId
go run ./cmd/mqadmin quarantine resolve --transport-id $auditId --confirm
```

有 replay_attempt_id 的记录必须显式提供当前值：

```powershell
$previousAttempt = '替换为当前 replay_attempt_id'
go run ./cmd/mqadmin quarantine resolve --transport-id $auditId --confirm --expect-replay-attempt $previousAttempt
```

没有 `--confirm`、精确身份不合法、旧 attempt 语法不合法、未知参数或额外位置参数时，配置/连接前失败。期望值与锁定读取到的当前 attempt 不同，则拒绝解决。没有重放记录的 open 行也允许解决：原命令可能已经由其他投递提交终态，不要求再发一次。

命令只加载 MYSQL 配置，沿用 `--config` / `ICHAT_CONFIG_FILE` / 默认项目根目录 `config/config.yml` 和 `ICHAT_MYSQL_*` 覆盖；`--config=` 为纯进程环境。不会连接 RabbitMQ/Redis，不要求有效运行时角色或 MQ 配置，不读取 Compose .env，不迁移 schema。总 context 15 秒，拨号/单次 socket 读写沿用 3/5 秒；CLI 拥有并最终关闭自己的 SQL pool，仓储只借用。

JSON 包含 transport_id、command_id、payload_hash、terminal_state、可选 reason_code、原 replay_attempt_id、resolution_outcome 和成功时的 resolved_at，不包含业务正文、headers、URL 或凭证。

| resolution_outcome | 含义 |
| --- | --- |
| resolved | 本次核验成功且 UPDATE 的 COMMIT 成功 |
| already_resolved | 原行已解决，本次重新核验业务终态成功，未重写原时间或 attempt |
| unconfirmed | 已完成终态核验，但本次解决调用未成功结束，不能据此判断数据库最终是否写入 |

输入/审计/终态核验失败时无结果 JSON，错误非零退出。进入已验证结果后的 SQL 更新/提交失败，会输出 unconfirmed 并非零退出；COMMIT 未知时不输出可能误导的 resolved_at。先用 show 核对当前行，再用相同选择重试核验。输出或关闭失败也可能发生在实际提交成功之后，非零退出不等于未写入。

## 事务核验

1. 锁定所选审计行 `FOR UPDATE`，恢复并完整校验 v1 审计文档、原文、可空身份/hash、错误字段、创建时间和状态组合；请求 ID、SQL 主键和文档 transport ID 必须一致。无完整已验证业务身份的坏 wire 不能解决。
2. 检查当前 replay_attempt_id 与显式选择匹配。全流程持有审计行锁；并发重放状态更新或其他解决操作必须等待。
3. 在同一事务中按原 `(sender_id, client_message_id)` 做 `FOR SHARE` 当前读取，取得已经提交的规范命令。不存在则返回 pending；sender/client/commandId/hash 任一不符则拒绝。不会把隔离状态、HTTP accepted、发布确认或客户端猜测当成终态，也不会扫描 MQ。
4. 复用现有终态结构校验，拒绝占位 claim、未知 state、非法 reason/message/conversation 组合。rejected 保留原业务拒绝原因；管理员修复权限或重放不会改变其终态。
5. persisted 还复用状态查询的实际消息核验：消息存在，与命令身份、会话、seq/已提交水位和规范内容/hash 一致，时间/DTO 字段有效。缺失或损坏不允许解决。不会回写修复消息。
6. 新解决仅设置 `state='resolved'` 和 UTC 微秒 resolved_at；条件包含旧 state、旧 attempt 及 resolved_at IS NULL，影响行数必须为 1。COMMIT 成功才返回 resolved。
7. 已 resolved 的记录同样重新核验审计和终态。通过后只回滚释放核验事务，返回 already_resolved 与原时间；没有新的 UPDATE/COMMIT。回滚失败仍报告未成功结束。

原 body/typed headers/首份观察、业务身份/hash、错误分类、创建时间和 replay_attempt_id 均保持不变；业务五表无任何写操作。事务不调用 MQ，不进行业务重做，不删除幂等记录。并发 resolver 串行核验，只有第一个写解决时间，其余读取同一时间；在它之后完成的旧快照 RecordReplay 会拒绝重新打开 resolved 行。

COMMIT/ROLLBACK 未知继续使用既有 `ErrQuarantineCommitUnknown` / `ErrQuarantineRollbackUnknown`；底层 SQL/驱动错误由固定阶段文字封装。完整性校验不证明管理员篡改前的数据来源可信，解决也不代表通知或客户端交付完成。

## 验证与范围

```powershell
go test -mod=readonly -race ./database ./cmd/mqadmin -run '^TestQuarantine(Resolve|Replay)' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./cmd/mqadmin -run '^TestQuarantine(ResolveCLI|CLI)Integration$' -count=1 -v -timeout=10m
```

默认测试覆盖两种终态、旧 attempt、已解决核验、坏文档/空身份、缺失/冲突终态、占位 claim、实际消息缺失/内容不符、事务前取消、SQL 更新/提交/回滚错误、零影响行、无误报时间、输出错误及连接释放。

真实 suite 使用独立 MySQL/Redis fixture，不启动 MQ。账号/好友为 fixture；persisted/rejected 由生产 `PersistCommand` 生成，含真实权限判断、消息/seq/Outbox 事务。六组场景核对：

- rejected 是可核验终态，解决保留原原因；persisted 需有完整实际消息，重复解决保持原时间。
- fixture replay_pending 不能代替终态；期望 attempt 不匹配时拒绝，终态实际提交后才允许解决并保留 attempt。该状态由 fixture 直接设置，不冒充真实重发确认。
- 同业务键不同 hash、无业务身份保持 open；真实消息被 fixture 故意破坏后拒绝核验，不修复原文，恢复 fixture 内容后才可解决。
- 八个仓储 resolver 同时执行，只得到一次 resolved 和七次 already_resolved，时间相同；旧快照的迟到重放更新不能重新打开该行。

逐列比较每次解决前后的业务五表，核对完整审计和全部非解决元数据；最终七条审计、四条规范 command、三条业务 message、七条 Outbox、一个会话、无 delivery cursor，复核 schema 指纹及清理。fixture SQL session 为 READ-COMMITTED/+05:30、driver loc +13，解决输出仍为 UTC。原只读 CLI 三组同时回归。实际 runId 和结果见[实施进度](message-reliability-mq-progress.md)。

后续[重放故障验收](mq-quarantine-replay-acceptance.md)已覆盖真实发布确认/提交未知、发布后状态更新前退出及并发发布/解决交叉故障，P4.2.5 范围收口。[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；[P5.1.2 token 条件结算](outbox-settlement.md)已完成；[P5.1.3 通知执行循环](outbox-execution.md)已完成；[P5.1.4 生产角色与生命周期](outbox-lifecycle.md)已完成；[P5.1.5 故障验收](outbox-acceptance.md)已完成，P5.1 收口；[P5.2 帧与权限核验](notification-frames.md)已完成；[P5.3 生产 v1 会话与订阅](reliable-websocket.md)已完成；下一项 **P5.4：当前会话绑定的业务 ACK 与持久化游标**。P4 显式三角色交叉已补齐；完整默认后台进程仍待装配。
