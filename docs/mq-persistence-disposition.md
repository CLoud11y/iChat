# 消费处置与可靠隔离接线（P4.2.3.3）

`workers.PersistenceProcessor` 将已有单条 SQL/ACK handler 与[隔离目标 publisher](mq-quarantine-publisher.md)组合。它明确返回 `terminal` 或 `quarantined`，不把隔离成功伪装成一个空的业务终态。原 `PersistenceHandler.Handle` 的公开契约保持。

## 显式启用与资源责任

```go
publisher, err := rabbitmq.DialQuarantinePublisher(ctx, address, publisherOptions)
// 检查 err；外部 owner 持有独立隔离连接。
options := workers.DefaultPersistenceSupervisorOptions()
options.Worker.QuarantinePublisher = publisher
factory, err := application.NewSupervisedPersistenceFactory(pool, options)
// 检查 err，显式注册已有运行时角色工厂。
// 所有服务/监督器真正 Done 后，owner 才 Close publisher 与共享 pool。
```

`PersistenceOptions.QuarantinePublisher` 是显式借入的 Go 依赖，不是 YAML/env 配置或 API publisher 能力。默认 nil 维持原先仅处理业务终态的路径；typed nil 拒绝。`NewPersistenceService` 和 `NewPersistenceSupervisor`（包括每次恢复新组）使用同一 Worker 参数，已有单轮/监督工厂均可传递该依赖，无需更改默认装配。

直接处理原 delivery 时可调用 `NewPersistenceProcessor(repository, retryOptions, publisher)`。构造不联网，不拥有 SQL、源 session 或目标 session 的关闭权。服务只负责等待已准入调用返回；原 channel owner 继续持有源投递，外部 owner 继续持有目标连接。默认 worker/all 缺失隔离审计/Outbox 的门禁仍保留。

SQL 尝试、退避、目标发布、回执复核和原 ACK 共用原 processing context。正常 StopAdmission/drain 不取消已经准入的转移；强制退出取消处理，observer 超时仍须等待真正 Done。不会为隔离发布启动脱离 owner 的后台任务，也不因 observer 返回就释放借入 publisher。

## 自动隔离策略

自动转移需要真实单条 handler 直接返回的责任边界；仅看 `ClassifyPersistenceFailure == technical` 不够。以下白名单不通过错误文字匹配，不因 delivery count 或本地次数耗尽扩大：

| 来源 | 处置 |
| --- | --- |
| `InvalidDelivery`：坏 JSON、wire/版本/身份/hash 错误 | 零 SQL；按 `INVALID_DELIVERY` 保存完整快照并转移 |
| `PersistenceFailed` 且仓储已调用、回滚完成，整个错误树仅为数据约束 1048/1062/1264/1366/1406/1451/1452/3819 | 按 `DATA_CONSTRAINT` 转移；这些确定性失败不做无意义本地重试 |
| 正常 persisted/rejected、同内容 replay、ID_CONFLICT | 保持原 handler 终态及提交后单条 ACK，不发布隔离副本 |
| EOF/数据库不可用、提交或回滚未知 | 保留源投递，继续原监督恢复规则，不隔离 |
| 1213/1205 重试耗尽 | 保留事务冲突类别，不隔离 |
| schema/syntax/deployment 错误、不变量、无效终态、未知或混合错误树 | 保守停止，不把整批积压迁走 |
| 取消、任何 ACK 阶段失败 | 不启动或重试隔离，不重发 ACK |

数据约束策略依赖实际 `MessageRepository` 的回滚契约：回滚失败必带 `ErrCommandRollbackUnknown`，该标记及提交未知/取消始终否决转移。混合叶子即使包含一个允许错误号，只要另有连接、未知、其他语义边界或冲突，也拒绝。未列入白名单的技术故障需后续明确评估，不能由一个宽泛的 technical 类别自动放行。

## 发布与原投递 ACK

1. 原 handler 失败后选择受控错误码，按当前观察时间创建完整隔离快照；原文/带类型 headers 不改写。
2. 用同一处理 context 调用借入 publisher。失败或错误与 accepted 同时返回均视为转移失败。
3. 无错误时，要求 accepted、transport ID 匹配、非零 connection/channel epoch 和 tag，以及合法的 32 位小写 hex 物理 attempt。空/矛盾回执拒绝，不因 nil error 就 ACK。
4. 再检查原 context；仍有效才调用**原 delivery 的 Ack(ctx)，仅一次**。不使用副本元数据重建 ACK，不调用 Reject/requeue，也不在新 channel 复用旧 handle。
5. 原 ACK 本地返回 nil 后，结果为 quarantined，Transfer 保存目标回执，Execution 为空。ACK 没有 broker 确认回复；本地成功不证明 broker 已收到它。

目标成功后原 context 取消或 ACK 失败，处理器返回空 Disposition、已验证的 Transfer 和 `AcknowledgementFailed`。这既保留已确认隔离副本的证据，也保留原投递的重投责任。服务只消费错误状态；broker 中的隔离副本和未确认源投递仍由各自 owner 保有。

快照构造失败（含超限/不支持 header）、目标发布失败/未知或回执不合法返回 `QuarantineFailed`，分类为 `FailureQuarantine`。底层原始失败与目标错误均通过错误展开保留，普通格式化仍隐藏正文、SQL 和 headers。

目标失败不作为数据库故障触发“探测 SQL 成功 → 再消费”循环。现有监督器仅恢复明确的源连接/SQL 证据，`QuarantineFailed`、源 ACK 异常均 fail closed：停止准入、真实 join 旧任务/资源，等待外部 owner 修复/替换目标后显式恢复。不会在队列内立即 requeue 或反复重建消费者。

## 重复与后续责任

目标 confirm 丢失后，原消息不会 ACK；源 channel 受控关闭后原 attempt 重投，可能再次转移。目标确认后源 ACK 丢失也会导致重复。跨重投的 transport ID 稳定，目标物理 attempt 独立生成；重新创建的快照允许观察时间及 broker 审计头变化，不能要求这些快照逐字节相同。

本项只完成从主队列到隔离 MQ 的安全交接，没有把隔离表或重放 CLI 实现为占位成功。后续 [P4.2.3.4 运行时故障验收](mq-quarantine-acceptance.md)已完成并收口 P4.2.3。[输入解析](mq-quarantine-input.md)、[审计事务](mq-quarantine-audit.md)、[隔离生命周期](mq-quarantine-lifecycle.md)及[P4.2.4.4 审计故障验收](mq-quarantine-audit-acceptance.md)均已完成，P4.2.4 范围收口；[P4.2.5.1 只读查询与选择](mq-quarantine-query.md)、[P4.2.5.2 确认式重发](mq-quarantine-replay.md)及[P4.2.5.3 终态核验](mq-quarantine-resolve.md)已完成；[P4.2.5.4 重放故障验收](mq-quarantine-replay-acceptance.md)已完成，P4.2.5 范围收口；[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；[P5.1.2 token 条件结算](outbox-settlement.md)已完成；[P5.1.3 通知执行循环](outbox-execution.md)已完成；[P5.1.4 生产角色与生命周期](outbox-lifecycle.md)已完成；[P5.1.5 故障验收](outbox-acceptance.md)已完成，P5.1 收口；下一项 **P5.2：统一 WebSocket 帧与通知权限核验**，重放仍保持原业务身份。

## 验证

固定 Go 1.27.1，仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE：

```powershell
go test -mod=readonly -race ./workers ./application -run '^Test(PersistenceDisposition|PersistenceFactory|PersistenceSupervisor)' -count=30 -timeout=90s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceDispositionIntegration$' -count=1 -v -timeout=10m
```

默认测试覆盖隔离前零 ACK、完整原文/header、业务终态不隔离、白名单/混合树否决、不同阶段取消、无效回执、ACK 一次、错误隐藏、typed nil 及 drain/强制退出后保留 publisher 调用直到 join。

真实 MySQL/RabbitMQ suite 覆盖同一顺序 worker 处理坏信封后继续持久化有效命令、真实 CHECK 约束回滚后转移、启动后移除目标绑定导致监督器停止并保留原 attempt、实际目标 confirm 丢帧、实际源 ACK 丢帧后的重复隔离，以及实际 COMMIT 成功后由测试 driver 返回 EOF 时不隔离并幂等恢复。COMMIT 场景为 driver 返回结果注入，不是 SQL TCP 丢包。读取/ACK 隔离队列仅作为 fixture 观察/清理，不冒充审计入库。

实际结果、runId 及清理记录见[实施进度](message-reliability-mq-progress.md)。本项不覆盖完整默认 worker/all、SQL 审计/CLI/Outbox、真实客户端或三节点故障模型。
