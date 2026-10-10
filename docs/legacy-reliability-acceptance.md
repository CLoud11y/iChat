# P1 旧消息链路回归对照

范围：开发规范第 3.3 节与 P1 验收条件。起点 `f1ec401`；本轮执行结果及资源记录见 [P1.8.2 实施记录](message-reliability-mq-progress.md#p182l1l12-回归映射缺口补齐和-p1-汇总验收)。这里的通过仅针对旧 Redis List 链路，不能作为 RabbitMQ 或端到端可靠同步的验收。

## L1—L12 与可执行断言

以下测试名可配合 `go test -run` 定位。默认测试不需要外部服务；标有“集成”的测试须启用 `-tags=integration`，只使用 `tests/testenv` 创建的独立资源。

| 项目 | 代码边界 | 回归及必须断言的结果 |
| --- | --- | --- |
| L1 入队顺序/错误 | `database/msgManager.go`、`service/send_message.go` | `TestLegacyMessageAcceptance` 检查 RPUSH→ZADD→PUBLISH；入队拒绝/响应未知时不执行缓存和通知。`TestMessageHTTPIntegration`（集成）用真实 WRONGTYPE、成功 RPUSH 后注入错误检查 HTTP 503/unknown 与实际队列；ZADD 失败不撤销已确认入队。 |
| L2 有界读取/读错误 | `database/dirty_messages.go`、`msgManager.go` | `TestDirtyQueueErrorsAreNotEmptySuccess` 不允许 Redis 错误被当成空队列；`TestDirtyMessageBatchIntegration/bounded_read`（集成）对 130 条只读 128 条；`TestSyncMsg`（集成）核对分两轮落库及队列余量。 |
| L3 提交后重放 | `database/message_persistence.go` | `TestLegacyMessagePersistenceIntegration`（集成）验证重复、七种字段冲突、非主键唯一约束和事务回滚；`TestSyncMsg` 验证 EVAL 发送前失败后重投；`TestLegacyWorkerCrashRecoveryIntegration` 强杀真实进程后由新进程恢复；`TestLegacyAcknowledgementUnknownIntegration` 验证 EVAL 已成功后调用方收到 EOF 的结果不确定性。消息和隔离审计不得重复或被修改。 |
| L4 重叠/过期批次 | `bootstrap.go`、`database/legacy_worker_lock.go`、`scheduledTasks/owned_messages.go` | 默认 `TestLegacyCronSkipsOverlappingRuns`、`TestLegacyCronResumesAfterPanic`、`TestLegacyCronWaitsForRunningJob`、`TestLegacyWorkerExplicitEnablement`、`TestOwnedMessagesBoundaries`；集成 `TestDirtyMessageBatchIntegration` 检查并发追加、双确认、非首条不匹配、短队列和原始字节；`TestLegacyWorkerOwnershipIntegration` 验证跨进程竞争、无 TTL、强杀后禁止自动接管、旧 token 无法释放新锁。 |
| L5 坏记录/连续前缀 | `database/message_quarantine.go`、`scheduledTasks/syncMsg.go` | `TestQuarantineFailureClassification`、`TestDirtyProcessingTransactionPrefix`、`TestSyncMsgInvalidProgressAndCombinedErrors`；集成 `TestDirtyMessageQuarantineIntegration` 验证坏 JSON、二进制原文、内容冲突、CHECK 错误、隔离写入失败和缺表。只确认已提交/隔离的连续前缀，基础设施错误和提交不确定的当前记录保留；约束恢复不自动业务重放已隔离原文。 |
| L6 通知失败不撤销 | `database/msgManager.go` | `TestLegacyMessageAcceptance/notify_rejected` 不返回发送失败、不执行 ZREM；`TestMessageHTTPIntegration`（集成）同时核对 HTTP 成功、脏队列完整和 Redis ZSET 中的原始消息，不能仅凭 MySQL 历史成功推断缓存保留。 |
| L7 发送者身份 | `service/send_message.go`、`middlewares/jwtAuth.go` | `TestSendMessageRequiresAuthenticatedContext` 在绑定前拒绝无效 UID；`TestSendMessageJWTIdentity` 验证请求体、query/header 不能伪造身份及 JWT/黑名单拒绝路径；`TestMessageHTTPIntegration`（集成）核对真实入队 JSON 的 SenderId。JWT 与实际 Redis 入队分别验证，不称为单一完整 HTTP 链路。 |
| L8 历史完整性 | `database/message_history.go` | `TestMessageHistoryIntegration/partial_cache_private`、`partial_cache_group`、`reverse_private`、`cursor_scope`（集成）：仅最新消息有缓存时仍能翻完 MySQL 双向私聊/群历史，排序和游标作用域正确；MySQL 缺失/失败不能以缓存冒充完整历史。 |
| L9 群历史/媒体 | `database/message_history.go`、`service/messages.go` | `TestMessageHistoryMedia`、`TestMessageHistoryAuthorization`、`TestMessageHistoryInvalidMediaIsNotPartialSuccess`；集成 `TestMessageHistoryIntegration/group_permissions` 验证非成员、离群、群软删除及当前成员可读旧历史；未知媒体/解码错误不返回部分成功，图片/音频保留真实类型。 |
| L10 订阅生命周期 | `database/subscriptions.go`、`service/chat.go` | 默认 `TestSubscriptionConfirmsRequestedChannels`、`TestSubscriptionFullBufferClose`、`TestReceiveSubscriptionCleanup`、`TestReceiveFullOutboundQueueCancellation`、`TestChatSocketLifecycle`、`TestChatWaitsForReaderBeforeOffline`；集成 `TestSubscriptionsIntegration`、`TestChatRedisIntegration` 核对就绪握手、真实消息、取消/关闭、NUMSUB 归零和失败重连不替换旧连接。满缓冲关闭不等待默认发送超时。 |
| L11 测试隔离 | `tests/testenv`、所有集成 build tag | `TestImportDoesNotInitializeDependencies`；`tests/testenv` 默认测试验证资源归属、碰撞、清理失败及误用开发 Compose 拒绝；`TestEnvironmentIsolation`（集成）验证两套同名表/key 数据隔离，清理一套不影响另一套。全量默认测试使用不存在的配置路径，fixture 用户/群 ID 均来自本轮插入。 |
| L12 初始化/迁移失败 | `bootstrap.go`、`utils/initMysql.go`、`cmd/migrate` | 默认 `TestBootstrapLifecycle`、`TestBootstrapMissingFactory`、`TestBootstrapMySQLOffline`、`TestMySQLConstructionModes`、`TestMigrationConfigFailure`；集成 `TestBootstrapMySQLRecoveryIntegration` 验证真实 MySQL 离线启动及两次恢复，`TestMigrationIntegration` 验证连接/DDL 失败非零退出、重复迁移保留数据且不依赖 Redis。 |

## 关键失败证据

本轮不在当前工作区回退旧生产代码。历史实施记录已经包含以下“先失败、后修复”的实际运行，作为 P1 验收的失败证据；它们与本轮通过结果分开记录：

| 旧行为 | 历史证据 |
| --- | --- |
| ZADD 先于 RPUSH、RPUSH 失败仍写缓存、PUBLISH 失败撤销缓存 | P1.1：`TestLegacyMessageAcceptance` 修改前定向测试失败，记录实际命令顺序。 |
| 无界领取、stale batch 误删 | P1.2：`TestDirtyMessageBatchIntegration` 修改前失败，runId `5910e47bef2586c3dcb712b479e60f6b`。 |
| 提交后重放因主键冲突阻塞 | P1.3.1：`TestSyncMsg` 修改前失败，runId `2a8bdfc2652a2d85df25af85e1b301d6`。 |
| 坏 JSON 阻塞有效尾部 | P1.3.2：`TestDirtyMessageQuarantineIntegration` 修改前失败，runId `679a98b8b4c989aaf95669ad307fac95`。 |
| cron 重叠执行 | P1.4.1：`TestLegacyCronSkipsOverlappingRuns` 修改前在首轮阻塞时进入第二轮。 |
| 缺少身份返回通用成功 HTTP 状态 | P1.5：`TestSendMessageRequiresAuthenticatedContext` 先复现 200 而非 401；鉴权 UID 覆盖伪造发送者是此前已存在的能力。 |
| 部分缓存冒充完整历史、群历史越权、媒体硬编码 | P1.6：历史集成和媒体默认回归修改前失败，集成 runId `8d241ebc49948f88850359ab158cd5bb`。 |
| 错误频道确认被接受、满缓冲关闭不排空 | P1.7：频道确认及满缓冲关闭默认回归修改前失败。 |

L11/L12 依据 P0 隔离、初始化、迁移、离线恢复的状态断言和本轮回归验收，不伪造“原始版本也曾运行同名用例失败”的记录。原版测试会访问固定用户数据，因此不为补充失败截图而运行原版集成套件。

## F01—F04 与故障边界

- F01：真实 RPUSH WRONGTYPE 加默认受控 RESP 对端，断言不返回确定接受、不制造缓存。
- F02：真实 Redis 原子前缀比较与并发追加/双确认，断言保留未提交尾部。注册约束要求全部消费者使用当前实现和同一权威 Redis DB。
- F03：分别覆盖 EVAL 发送前错误、EVAL 已执行后 hook 向调用方返回 EOF、DB 提交后强杀实际 worker。第二种是明确的错误注入，不是实际网络丢包；第三种由 OS 终止真实子进程，恢复前确认数据库会话已退出。
- F04：真实 MySQL/Redis 的坏 JSON、内容冲突、数据错误和隔离失败；隔离原文/原因持久保留，后续有效消息可推进。

P1 仍未提供自动高可用、MySQL fencing、Redis 数据丢失恢复、实际 COMMIT 响应丢失验收、运行中 Redis 网络黑洞/重订阅故障验收、完整服务信号停机或容量基线。旧时间戳分页不是可靠 seq 补拉，旧 HTTP 200 仅代表 Redis 入队确认；HTTP 202、RabbitMQ confirm/ACK、Outbox、客户端 ACK/补拉及 F05—F38 属于 P2—P8。群发送与推送内容权限也不由本阶段的历史权限修复代替。

## 重跑命令

按 [测试说明](../tests/README.md) 设置 `GOTOOLCHAIN=go1.27.1` 和仓库 GOCACHE，清空不匹配的 GOROOT，并将 ICHAT_CONFIG_FILE 指向不存在路径。集成命令需要本机 Docker 引擎可用；默认测试无此依赖。

```powershell
go test -mod=readonly -race ./... -count=2 -shuffle=on
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -race -tags=integration -p 2 ./... -count=1 -shuffle=on -json -timeout=15m
```

全量集成使用 `-p 2` 控制同时运行的包数；每个 fixture 独立创建、校验和回收资源。不执行 benchmark，不将本轮耗时作为性能基线。JSON 输出中的 `skip` 须区分无测试文件的包与测试用例跳过，未执行的用例不能算通过。
