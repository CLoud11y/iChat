# P4.2.4.2：幂等隔离审计事务

`database.NewQuarantineRepository(pool)` 借入私有 SQL pool；`Save(ctx, input)` 只接受 [P4.2.4.1](mq-quarantine-input.md) 的不可变 `QuarantineInput`。新记录完整提交或已提交的已有记录核验成功后才返回成功。本项没有 MQ ACK、worker 订阅、自动重试、pool 关闭或重放状态更新。

## 保存契约

沿用 `001_reliable.sql` 和 `models.MessageQuarantine`，不修改已应用 migration、schema 指纹或模型。首次观察保存如下字段：

| SQL 字段 | 内容 |
| --- | --- |
| transport_id | 重新验证过的 `mq:v1:` 稳定传输身份 |
| raw_body | 完整源正文的 longblob，包含坏 JSON、无效 UTF-8 和 NUL；wrapper 存内层原文 |
| raw_headers | 下面的 v1 审计 JSON 文档，包含完整带类型 headers、AMQP 属性、观察及精确稳定内容 |
| sender_id / client_message_id / command_id / payload_hash | 输入完整通过业务 wire/摘要/AMQP 身份校验时一并保存，否则全部 SQL NULL |
| error_code | 源 wrapper 的受控分类或 BROKER 固定分类 |
| error_detail | 固定 `QUARANTINE_AUDIT_V1` 标记，避免持久保存未经控制的 SQL 错误、凭证或堆栈 |
| state / replay_attempt_id / resolved_at | 初始 open / NULL / NULL；重复观察不覆盖后续 CLI 状态 |
| created_at | 首次审计接收时间，UTC、截断到微秒，日期在 MySQL DATETIME 支持范围内 |

`raw_headers` 是带版本的完整审计文档，不能将它直接当作普通 AMQP table。`QuarantineInput.EncodeAudit()` / `rabbitmq.DecodeQuarantineAudit()` 是保存和恢复入口：

| JSON 字段 | 含义 |
| --- | --- |
| audit_version | 1 |
| format | active_wrapper / broker_dlx |
| received_at | 首次接收观察的 UTC 时间，保留纳秒 |
| source_envelope | 规范 v1 隔离信封的 base64 字节串，内含完整源 body/带类型 headers/属性及源观察时间 |
| delivery_metadata | wrapper 外层投递的带类型元数据快照，body 留空并由 source_envelope 重建；原文 DLX 为 null，因为源快照即当前投递 |
| stable_content | 精确规范稳定内容的 base64 字节串，用于逐字节比较并复核 transport ID；不只存摘要 |

源信封以字节串存储，MySQL JSON 的键重排/空白变化不会破坏内层规范编码。恢复时严格检查版本、未知字段、来源、元数据、分类、稳定内容与观察的一致性，再重新解析并比较规范文档。headers 类型、整数宽度、二进制值和所有 AMQP 属性均可恢复；不保留 Go 时区对象或原始 AMQP frame。`raw_body` 和审计文档中的源正文也必须一致。

审计文档最多 24 MiB，覆盖输入既有 body/节点预算、base64 开销及独立外层元数据预算；超限失败、不截断。raw_body 与完整源快照重复保存正文是明确的审计校验成本。JSON 参数使用 UTF-8 string，正文使用 []byte；不能将 driver 的 binary 字符集参数直接写为 JSON。部署的 MySQL/client packet 限制必须允许实际文档及 SQL 参数总量，超限错误保留为存储失败，不视为已审计。

旧的纯 headers JSON、缺失快照、未知文档版本或损坏记录不能自动补齐为成功。当前仓库此前没有生产 MQ 审计写入器；若外部已写入此类行，需要显式修复，不允许以新观察覆盖或猜测缺失原文。状态查询仅使用已有业务身份/状态索引，不读取 raw_headers 的内部布局。

## 事务与重复输入

1. 校验输入、时间、序列化完整审计材料，再开启事务。
2. 执行普通 INSERT，不使用 IGNORE、REPLACE 或覆盖式 upsert。只有 MySQL driver 直接返回的确定 1062 才进入重复核验；其他/混合错误失败。
3. 按 transport_id 做 `FOR SHARE` 锁定读取，核验完整文档、raw_body、全部可选身份、受控错误字段、created_at 和现有状态组合。锁定读看到最新已提交记录；新插入行在自身事务中同样可读。
4. 比较双方精确稳定内容与 transport ID。不同内容/主键不一致返回 `ErrQuarantineConflict`；文档损坏或冗余字段不一致返回 `ErrQuarantineInvariant`，均保留已有行。新记录额外要求保存后的完整文档与本次输入一致且状态仍为初始 open。
5. 新记录 COMMIT 成功后返回 inserted。重复记录没有成功写入，核验后 ROLLBACK 成功才返回 existing，避免给只读核验引入新的 COMMIT 不确定性。

重复键的共享锁和锁定读依据 [MySQL 8.0 的 INSERT 锁说明](https://dev.mysql.com/doc/refman/8.0/en/innodb-locks-set.html)与[锁定读说明](https://dev.mysql.com/doc/refman/8.0/en/innodb-locking-reads.html)。重复分支使用共享读取，避免将并发重复投递持有的共享锁升级为排他锁；其他 SQL 死锁/连接失败仍向调用者返回，不在仓储内无限重试。

wrapper 和自动 DLX 的可变路由、TTL、死信历史、consumer tag、观察时间和分类可以不同；只有稳定内容完全相同才共用首条审计。不会覆盖第一份观察、递增物理投递计数、重开 resolved 行或替换 replay attempt。不同源物理 CorrelationID 会形成不同 transport ID，即使它们属于同一个业务命令。

## 失败与责任

| 结果 | 调用者责任 |
| --- | --- |
| inserted / existing 且无错误 | 具有持久审计依据；后续 worker 仍须核验运行状态并由原 delivery handle ACK |
| INSERT/读取/约束/内容冲突/取消失败 | 返回零结果，保留当前 MQ 投递；不创建 message、command、Outbox、seq 或 resolved |
| ErrQuarantineCommitUnknown | COMMIT 返回错误，不声称未提交；返回零结果，按原 transport ID 重投后核验，禁止删除可能已经提交的记录 |
| ErrQuarantineRollbackUnknown | 回滚未获确定成功，包含重复核验分支；返回零结果，不允许 ACK |

错误保留可由 errors.Is/As 检查的底层原因和未知阶段标记，正常 `%v` / `%+v` / `%#v` 仅显示受控阶段；日志不要自行展开底层 SQL 错误。仓储不拥有连接替换、存储探测或原 MQ ACK 能力，这些属于 P4.2.4.3。

## 验证

使用 Go 1.27.1、空 GOROOT、仓库 `.gocache`、不存在的 ICHAT_CONFIG_FILE，避免开发配置：

```powershell
go test -mod=readonly -race ./database ./messaging/rabbitmq -run '^TestQuarantine(Audit|Repository)' -count=30 -timeout=120s
go test -mod=readonly ./messaging/rabbitmq -run '^$' -fuzz '^FuzzDecodeQuarantineAudit$' -fuzztime=15s -parallel=2
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./database -run '^TestQuarantineRepositoryIntegration$' -count=1 -v -timeout=8m
```

默认测试覆盖序列化/恢复、MySQL JSON 重排兼容、版本/元数据篡改拒绝、SQL 事务边界、空结果、错误隐藏、完整性、重复状态保留，以及 1062 后未读到请求主键时拒绝成功。真实 MySQL suite 验证两格式/可选身份/状态查询、12 路并发一条审计、wrapper/DLX 稳定内容合并、损坏与不同内容保留、大快照、实际既有 CHECK 错误、实际 INSERT 后失败/取消、COMMIT 前回滚后错误、实际 COMMIT 后响应错误及重复分支回滚后错误。最后核对业务五表为空、schema 指纹未改变及资源清理。

COMMIT/ROLLBACK 错误通过真实事务完成后的 driver 返回结果注入，不是 SQL TCP 丢包或进程强杀。CHECK 场景只在 fixture driver 将写入参数的 transport_id 改为空，由正式既有约束返回 3819，不修改输入解析或 schema。本 suite 的 DLX 输入是与已验收解析协议一致的数据 fixture；真实 broker rejected/expired 解析见 [P4.2.4.1](mq-quarantine-input.md)。本事务 suite 没有执行 MQ ACK、生产隔离 worker、运行时退出、重放 CLI 或三节点故障。后续[隔离生命周期](mq-quarantine-lifecycle.md)与[审计故障验收](mq-quarantine-audit-acceptance.md)已完成提交后 ACK、真实 SQL/ACK 丢失、进程故障和双角色恢复，P4.2.4 范围收口。实际结果见[实施进度](message-reliability-mq-progress.md)。[P4.2.5.1 只读查询与选择](mq-quarantine-query.md)、[P4.2.5.2 确认式重发](mq-quarantine-replay.md)及[P4.2.5.3 终态核验](mq-quarantine-resolve.md)已完成；[P4.2.5.4 重放故障验收](mq-quarantine-replay-acceptance.md)已完成，P4.2.5 范围收口；[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；下一项 **P5.1.2：token 条件结算与失败回退**。
