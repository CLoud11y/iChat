# P5.1.2：token 条件结算与失败回退

在[领取仓储](outbox-claim.md)基础上新增 `Complete(ctx, lease)` 和 `Retry(ctx, lease, options, reason)`。它们只结算当前 owner 的通知处理结果，不执行通知、重写事件或推进 delivered 游标。沿用现有 schema，不增加后台角色或默认配置。

## 共同门禁

1. 校验 eventId 的小写 hex 身份、规范 UUID token、非零 attempts 和数据库支持范围内的微秒 lease 截止时间；nil/已取消 context 在 SQL 前拒绝。
2. READ COMMITTED 短事务，先按 event_id `FOR UPDATE` 获取当前行；不存在、非 leased、token/attempts/lease_until 不符或已有 completed_at，均返回 `ErrOutboxLeaseLost`。
3. **取得行锁之后**再读取数据库 UTC_TIMESTAMP(6)，到期边界 `lease_until <= now` 即丧失 owner。不会用进入锁等待前的旧时钟判断是否仍可完成。
4. UPDATE 再按 event_id、status=leased、token、attempts、精确 lease_until、completed_at IS NULL 和 `lease_until > UTC_TIMESTAMP(6)` 做条件保护。零影响行视为 owner 丢失，多行视为不变量错误。
5. COMMIT 成功才返回结算回执；写入、取消、COMMIT/ROLLBACK 未知或释放失败不返回成功结果。没有自动重试。

eventId/token/attempts/deadline 构成结算凭据；payload 和路由不参与结算写入。旧 token 不能结算新领取，已经过期但尚未被回收的 owner 也不能结算。同一份 lease 的成功/失败回调竞争只允许一方生效。重复完成返回 LeaseLost，不覆盖首次时间；该错误不能用来推断事件究竟已完成、已重领、已回退还是过期。

提交发生在数据库更新判定之后；lease 不承诺网络返回前始终未过期。通知仍可能重复，不能将租约/完成状态解释为终端已收到消息。

## 完成与重试

`Complete` 将状态改为 completed，写入数据库 UTC 时钟样本的 completed_at，清空 lease_token、lease_until 和 last_error。保留原 payload、event/command/type、路由、attempts、available_at 和 created_at。仅表示通知处理完成，绝不写 delivery cursor。

`Retry` 将状态改回 pending，清空 lease_token/lease_until，保留 attempts 不变（仅 Claim 递增），以数据库 UTC 样本加随机延迟设置 available_at。last_error 允许以下受控原因码（P5.1.3 增加 INVALID_NOTIFICATION）：

| 原因 | 值 |
| --- | --- |
| 明确通知失败 | NOTIFICATION_FAILED |
| 通知结果未知 | NOTIFICATION_UNKNOWN |
| 依赖暂不可用 | DEPENDENCY_UNAVAILABLE |
| payload 语义无效 | INVALID_NOTIFICATION |

不接受任意 error/正文/连接信息作为持久诊断，未知原因码在 SQL 前拒绝。使用通知 unknown 原因并不证明原通知没发送，下一次执行必须容忍重复。

`OutboxRetryOptions{Base, Max}` 显式传入，无隐式默认值；Base 至少 1ms，Base ≤ Max ≤ 1h，精度为整数微秒。第 n 次领取失败的指数上界为 `min(Max, Base × 2^(n-1))`，按上界的一半至上界做 crypto/rand 均匀抖动，下界向上取整至微秒。增长饱和，uint32 最大 attempts 不会溢出或执行无界循环。可单独调用 `Delay(attempts)` 验证策略。

available_at 基于加锁后的数据库样本；事务/I/O 延迟可能消耗退避时间，不承诺从返回时刻起重新等待完整延迟。结算回执的 AvailableAt/CompletedAt 只在提交成功时返回，内容与 SQL 写入一致。

`ErrOutboxCommitUnknown` / `ErrOutboxRollbackUnknown` 沿用既有变量身份和 errors.Is 语义，文字扩展为 Outbox transaction，覆盖领取与结算。底层错误经固定阶段封装，不直接展示 SQL/凭证。提交未知时不立即发送/补偿或清除其他 owner 的状态；后续应核对持久状态或让过期任务由新领取恢复。本项没有续约接口。

## 验证与边界

```powershell
go test -mod=readonly -race ./database -run '^TestOutbox' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./database -run '^TestOutbox(Claim|Settlement)Integration$' -count=1 -v -timeout=10m
```

默认测试覆盖两种结算的事务顺序、错 token/attempt/deadline、已完成/缺失/过期、UPDATE 零影响行、更新/提交/回滚错误、不返回不确定成功，以及退避上下界、微秒精度和大 attempts 饱和。driver 错误仅验证错误处理，不冒充真实网络丢响应。

真实 MySQL/Redis fixture 五组验证：完成后的重复/失败回调拒绝且时间不变；退避实际写入、未到 available_at 不领取；实际行锁等待跨过数据库 lease 截止后拒绝旧完成，过期重领换 token/attempt 后旧成功与失败都被拒绝；并发成功/失败只有一个赢家；实际锁等待取消保留数据且 pool/lease 仍可使用。

五条直接 Outbox 结构夹具的身份、payload、路由、创建时间和 attempts 保持（重领一条的 attempts 除外），其他五张可靠性表为空，schema 指纹不变。session +05:30、driver loc +13，写入及回执仍使用 UTC。原领取三组同时回归。没有运行通知发送、业务权限核验、真实提交 OK 丢失或完整 worker 生命周期；这些属于后续小点。实际结果见[实施进度](message-reliability-mq-progress.md)。

[P5.1.3 通知执行循环](outbox-execution.md)已完成；[P5.1.4 生产角色与生命周期](outbox-lifecycle.md)已完成；[P5.1.5 故障验收](outbox-acceptance.md)已完成，P5.1 收口；下一项 **P5.2：统一 WebSocket 帧与通知权限核验**。P5.1 与显式 worker 三角色交叉范围已完成；P5 和完整默认进程保持未完成。
