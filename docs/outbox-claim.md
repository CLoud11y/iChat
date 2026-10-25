# P5.1.1：Outbox 有界原子领取

P5.1 拆分为五步：

| 小点 | 范围 | 状态 |
| --- | --- | --- |
| P5.1.1 | 领取仓储、数据库时钟、随机 token、到期回收及 SKIP LOCKED | 完成，本文 |
| P5.1.2 | token 条件完成/失败回退、退避参数与迟到结果保护 | 完成，见[结算契约](outbox-settlement.md) |
| P5.1.3 | payload 语义核验、事务外通知执行循环与有界失败处理 | 待实现 |
| P5.1.4 | 生产角色工厂、独立就绪/连接及 drain/pending 所有权 | 待实现 |
| P5.1.5 | 提交未知、发送后退出、lease 过期/迟到结果与角色交叉故障验收 | 待实现 |

`database.NewOutboxRepository(pool)` 借入 SQL pool；`Claim(ctx, OutboxClaimOptions)` 只领取记录。没有通知发布、完成/失败更新、自动重试、续约、业务表/游标写入或默认后台角色装配。沿用既有 message_outbox schema、索引、CHECK 和 payload，不修改 migration/model。

## 领取契约

- limit 1—100，lease 1 秒—5 分钟，精度为整数微秒；非法选项、nil context 或已取消请求在 SQL 前拒绝。
- 短 READ COMMITTED 事务，读取一次数据库 UTC_TIMESTAMP(6)。显式解析 DATETIME 文本为 UTC，不依赖 driver loc 或 SQL session 时区。
- 选择 available_at 到期的 pending，或 lease_until 到期的 leased；按 available_at、created_at、event_id 排序，LIMIT + FOR UPDATE SKIP LOCKED。
- 每行生成独立 crypto/rand UUID v4 token，将状态改为 leased、lease_until 设为数据库时钟样本加 lease、attempts 加一。保留原 payload/身份/路由、可用时间、创建时间和 last_error；条件更新核对 event_id、旧状态/旧 token 和未完成状态，影响行数必须为 1。
- 验证 eventId 与 commandId/eventType 派生关系、日期及状态/lease 组合、到期性、未完成状态、attempts 不溢出和 payload 为非空合法 JSON（最大 1 MiB）。坏行整批回滚，不截断、跳过或假装 completed。
- 所有更新 COMMIT 成功才返回 lease。任一行失败、取消、COMMIT/ROLLBACK 未知均不返回部分结果。空批次仅回滚释放读事务，成功返回非 null 空 slice。

返回值包括 event/command/type、可空 conversation/message 路由、未经重新编码的存储 JSON、token、UTC lease 截止时间及新 attempts。默认 String/GoString 隐藏正文；错误使用固定阶段文字，保留 errors.Is/As 链但不直接展示 SQL/凭证。仓储不拥有 pool 关闭权。

本项只做存储与领取结构校验，尚未验证 payload 的完整业务语义或接收权限；领取结果不是可直接发送的通知。后续执行循环必须核验 payload 并在事务外发送，且考虑剩余 lease 时间。数据库时钟在事务开始采样；慢事务/提交延迟可能消耗 lease，不能把返回时刻视为 lease 刚开始。

SKIP LOCKED 允许临时空批次，也可能跳过排序靠前的锁定行；没有公平性或固定批次大小保证，空结果不能证明永久无任务。过期记录会生成新 token；提交未知时可能已经领取，调用者不能立即发送不确定结果，等待后续过期回收。完成/失败更新的 token fencing 由后续已完成的[P5.1.2](outbox-settlement.md)提供，领取本身不结算。

## 验证

```powershell
go test -mod=readonly -race ./database -run '^TestOutboxClaim' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./database -run '^TestOutboxClaimIntegration$' -count=1 -v -timeout=7m
```

默认测试检查事务顺序、空批次、部分写入后的整批回滚、非法记录/选项、过期/未来 lease、attempt 上限、取消和提交/回滚未知不返回 lease。

真实 MySQL/Redis fixture 不启用 MQ。17 条直接插入的 Outbox 结构夹具（不是生产业务终态/通知证明）覆盖 pending 到期/未来、leased 过期/未过期和 completed；核对原存储 JSON、新 token/attempt、未到期项不变。另用实际行锁/未提交 pending 更新证明跳过锁定任务，10 路并发检查无重复 event owner/token，并在事务结束后领取临时被跳过的剩余集合。返回的每份 token 都经另一查询确认已提交。其他五张可靠性表为空，schema 指纹不变。

SQL session 时区 +05:30、driver loc +13，lease 仍为 UTC。实际 runId、初轮并发断言修正和清理结果见[实施进度](message-reliability-mq-progress.md)。尚未验证真实 COMMIT 丢响应、通知发送、lease 结算/续约或进程退出。

后续[P5.1.2：token 条件结算与失败回退](outbox-settlement.md)已完成。下一项 **P5.1.3：通知执行循环**。P5.1、P5 和默认完整后台角色保持未完成，P4 整体交叉验收在 Outbox 接入后继续。
