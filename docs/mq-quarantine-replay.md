# P4.2.5.2：确认式重发与独立 replay attempt

本项完成 `mqadmin quarantine replay`、`application.ReplayQuarantine` 和 `QuarantineRepository.RecordReplay`。只将已验证的原业务命令重新发布一次；confirm 且无 return 后，将所选审计更新为 replay_pending，并保存这次物理发布的 UUID。不会标记 resolved、修改业务终态或删除幂等记录。列表和详情沿用[只读查询](mq-quarantine-query.md)。

## 选择与使用

先查看原因，完成必要的程序/配置修复，再选择精确 transport ID：

```powershell
go run ./cmd/mqadmin quarantine list
$auditId = '替换为列表中的完整 transport ID'
go run ./cmd/mqadmin quarantine show --transport-id $auditId
go run ./cmd/mqadmin quarantine replay --transport-id $auditId --confirm
```

没有 `--confirm`、选择不完整、额外位置参数或未知参数时，在配置读取/SQL/MQ 连接前失败。每次只选一条，无批量重放或“最新记录”的默认选择。

已记录过重发时，必须显式提供当前详情中的 replay_attempt_id，表示选择的是这一版记录：

```powershell
$previousAttempt = '替换为当前详情中的 replay_attempt_id'
go run ./cmd/mqadmin quarantine replay --transport-id $auditId --confirm --expect-replay-attempt $previousAttempt
```

原记录已 resolved、期望的旧 attempt 不匹配、文档损坏或无法完整验证为业务命令时，拒绝发布。不从坏 JSON 中猜测身份，不允许通过参数修改 sender/clientMessageId/hash/正文，也不把管理员再次重放当成重新发送业务消息。

配置路径与只读命令相同：`--config`、`ICHAT_CONFIG_FILE`、默认项目根目录 `config/config.yml`；`--config=` 仅取进程环境。MYSQL 和 RABBITMQ 使用现有键及 ICHAT_* 覆盖，不读取 Compose .env。重放需要有效 MQ URL/vhost 和 managementURL/managementUsername/managementPassword；独立 `config.LoadRabbitMQFile` 复用原严格 MQ 校验，不要求 Redis 或有效运行时角色。

## 发布与审计顺序

1. 无锁重新读取所选审计，执行完整 v1 文档/SQL 冗余字段校验；核对未解决状态和期望的旧 attempt。只有具有完整、已验证业务身份的输入才能继续。
2. 严格解码源 wire 并重新验证命令。保留 sender、clientMessageId、commandId、payloadHash、目标、媒体、正文、clientSentAt 和原 receivedAt。重复投递仍依赖原业务幂等身份。
3. 只读复核现有 RabbitMQ 拓扑，再建立独立、单 slot、有期限的 publisher session。不会建表/建拓扑，不使用重连监督器，不自动重试发布。
4. 调用已有命令 publisher，生成全新的 publishAttemptId；正文 wire 和 AMQP CorrelationId 使用同一个 UUID，MessageId 使用原 commandId。mandatory、persistent、confirm/return/NACK/未知结果判定复用既有内核；不能只看到 ACK 就忽略 return。
5. 只有完整 accepted 回执且无错误，commandId/新 attempt/epoch/tag 均有效时，调用独立 SQL 事务。锁定读取当前行，重新验证完整文档和首份观察与先前快照完全一致，核对旧 state/replay_attempt_id，拒绝 resolved/被删除/发生并发变更的记录。
6. 只更新 `state='replay_pending'`、`replay_attempt_id=新 UUID`，带旧 state/attempt 和 resolved_at IS NULL 条件，影响行数必须为 1。COMMIT 成功才报告 attempt 已记录；COMMIT/ROLLBACK 未知按原仓储错误类型保留。

SQL 事务不跨越 MQ 网络调用。首份 source/body/typed headers、观察、身份/hash、错误字段和创建时间均不更新。重发使用标准命令发布属性，不携带旧 delivery tag、死信历史、TTL 或任意源 headers；完整旧属性仍保存在原审计中。坏 wire、未知版本或摘要/AMQP 身份不一致只能查看审计，不能在该 CLI 中编辑后强行重放。

后续显式再次重放只替换 latest replay_attempt_id；现有 schema 不保存多次 attempt 的完整历史，CLI 的 JSON 是本次尝试的核对材料。新的物理尝试不参与业务幂等键和 hash。

## 结果与未知责任

JSON 只包含 transport_id、command_id、payload_hash、replay_attempt_id、publish_outcome、attempt_record，不输出原文、URL、凭证或底层异常。选择/输入检查失败时没有 JSON；开始进入发布流程后，即使失败也保留可用身份和尝试信息，并非零退出。

| publish_outcome | attempt_record | 含义 |
| --- | --- | --- |
| accepted | recorded | 目标确认且本次 SQL COMMIT 成功；审计为 replay_pending，尚未验证业务终态 |
| not_started / unroutable / nacked | not_attempted | 未获得可记录的成功发布回执，不调用审计状态更新 |
| unknown | not_attempted | 发布责任未知；不会自动重发或标记解决 |
| accepted | unconfirmed | 目标已确认，但本次审计更新未获成功确认；可能是并发变更、SQL 错误、取消或提交未知 |

“unconfirmed”描述审计更新调用的结果，不能据此认定数据库一定没写入。先用 show 核对当前 state/attempt，结合原业务身份查询规范终态；不要因非零退出就自动重跑。并发操作可能都已实际发布，只有仍匹配旧快照的一方可以记录该次状态，另一方返回 accepted/unconfirmed；不会覆盖赢家或重新打开 resolved 行。业务幂等仍是重复发布的兜底。

确认后、SQL 更新前进程退出仍可能造成“已发布、未记录”；本项不声称 SQL 与 MQ 原子提交。输出失败或清理失败同样可能发生在发布/记录成功之后。总操作 context 45 秒；publisher 使用现有配置的连接/写入/操作期限，最终以独立 5 秒 context 关闭，随后释放 CLI 自有 SQL pool。库函数仅借入仓储和发布能力，不拥有资源或 MQ delivery ACK。

## 验证与范围

```powershell
go test -mod=readonly -race ./database ./cmd/mqadmin ./config -run '^Test(QuarantineReplay|RabbitMQOnlyFile)' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./cmd/mqadmin -run '^TestQuarantine(ReplayCLI|CLI)Integration$' -count=1 -v -timeout=10m
```

默认测试覆盖强制精确选择/确认、lazy MQ 连接、业务内容与原接收时间、Return/NACK/未知/错误混合回执拒绝、先确认再记录、SQL 错误与 COMMIT/ROLLBACK 未知、旧状态/原文冲突、受控错误输出、输出失败和 publisher→SQL 清理顺序。

真实 MySQL/RabbitMQ suite 经 CLI 默认入口重放 wrapper/DLX 协议审计，各两次，共四条确认物理发布；逐条检查实际入队的原业务字段/hash/时间、新 UUID 和 AMQP 属性，核对原审计及所有非重放字段不变。旧快照写状态被拒绝；未携带最新 attempt 的再次重放、无业务身份和 resolved 记录不发布。另在真实拓扑核验/连接成功后，由 fixture 暂时解除绑定，获得真实 mandatory Return，确认不记录 attempt、原记录仍 open，之后恢复绑定。

最终五条审计：两条 replay_pending、一条无业务身份 open、一条 fixture resolved、一条 Return 后 open；业务五表为空，schema 指纹及正式拓扑不变。只读 CLI 原三场景同时回归。DLX 是既有协议输入夹具；fixture 对入队消息的 ACK 仅用于检查/清理，没有代替生产消费事务或终态证明。真实网络确认丢失、提交未知、进程故障及并发发布交叉验收留给 P4.2.5.4。实际 runId、结果与清理见[实施进度](message-reliability-mq-progress.md)。

后续[P4.2.5.3：终态核验与 resolved](mq-quarantine-resolve.md)及[重放故障验收](mq-quarantine-replay-acceptance.md)已完成，P4.2.5 范围收口；已 rejected 不会被管理员重放改成 persisted。[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；[P5.1.2 token 条件结算](outbox-settlement.md)已完成；[P5.1.3 通知执行循环](outbox-execution.md)已完成；[P5.1.4 生产角色与生命周期](outbox-lifecycle.md)已完成；[P5.1.5 故障验收](outbox-acceptance.md)已完成，P5.1 收口；[P5.2 帧与权限核验](notification-frames.md)已完成；下一项 **P5.3：v1 会话接线、订阅与单 writer 生命周期**。P4 显式三角色交叉已补齐；完整默认后台进程仍待装配。
