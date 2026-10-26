# P5.1.3：通知执行循环

在[领取](outbox-claim.md)和[结算](outbox-settlement.md)仓储上加入独立 `workers.OutboxLoop`、严格的 `messaging.DecodeOutboxNotification` 和借入 Redis client 的发布适配。`RunBatch` 执行一个有界批次，`Run` 持续轮询。后续 [P5.1.4 生产角色与生命周期](outbox-lifecycle.md)已完成；[P5.1.5 故障验收](outbox-acceptance.md)也已完成，P5.1 收口；[P5.2 帧与权限核验](notification-frames.md)已完成；下一项为 **P5.3：v1 会话接线、订阅与单 writer 生命周期**。

## payload 与路由

存储 payload 不直接透传。解码限制 16KiB、有效 UTF-8/JSON/Unicode escape、精确字段名；拒绝未知字段、重复键、null、字段别名和非规范数字。嵌套 message 也严格核验。只接受 schemaVersion=1 和已知 eventType，重算 eventId 并比对 claim 的 event/command/type。解析失败不会发送通知或标记 completed。

- `message.persisted`：非零字符串 seq、messageId=commandId、规范 sender/target、targetType 与会话路由一致；payload 的 conversationId/messageId 必须匹配两个非 NULL 的 SQL 列。它没有 clientMessageId/content，无法独立重算 commandId；网关仍须从 MySQL 验证实际消息及权限。
- `command.result`：重算 sender+clientMessageId 对应的 commandId，校验规范 UUID 和 hash。persisted 必须有完整 message、无拒绝原因；重新 Normalize 内容以核对 payloadHash、消息身份、发送者/目标、会话、seq 和规范服务端时间，并比对 SQL 路由列。rejected 必须无 message、两路由列均 NULL，只接受当前五个业务拒绝原因。新原因需显式更新协议，不能将任意诊断传出。

输出 `Notification` 的频道/body 为私有字段，零值不能发布；Body 返回副本。两类事件均投影为**无正文提示**，不含 content/media，独立于旧 `private_`/`group_` 频道，避免旧 WebSocket 将新提示当成旧消息：

| 事件 | 频道 | 提示内容 |
| --- | --- | --- |
| 私聊 message.persisted | `ichat:notify:v1:user:<targetId>` | schemaVersion/eventType/eventId/commandId/conversationId/messageId/seq |
| 群聊 message.persisted | `ichat:notify:v1:group:<targetId>` | 同上 |
| command.result | `ichat:notify:v1:user:<senderId>` | schemaVersion/eventType/eventId/commandId/clientMessageId/payloadHash/state，成功增加会话/消息/seq，拒绝增加 reasonCode |

seq 保持规范 uint64 十进制字符串，不经浮点转换。command.result 存储的完整 message 保留在 SQL；后续网关读取终态和当前权限后生成统一 WebSocket 帧。群订阅不代表授权，通知不能用于证明历史完整或客户端已交付。当前没有新频道的生产 WebSocket 消费者。

## 执行与错误责任

依赖为借入的 `OutboxStore` 和 `NotificationPublisher`；不关闭 pool/client，不持有 SQL 事务等待发布。生产仓储 Claim 已完成整批 COMMIT 才返回；领取错误（含提交/回滚未知）不发送。批内顺序处理，无额外发布 goroutine，无自动结算重试、补偿、续约或业务/游标更新。

`OutboxOptions` 显式给出 Claim/Retry 策略、ClaimTimeout、PublishTimeout、SettlementTimeout 和 PollInterval。沿用仓储批次 1—100、lease 1s—5min、退避上限；ClaimTimeout 在 (0,1min]，PublishTimeout/SettlementTimeout 均正且两者之和严格小于 lease，轮询间隔 1ms—1min。无隐式默认配置。

在 Claim 调用前记录 Go 单调时钟起点，用 `start + lease` 作为保守批次截止；数据库采样在起点之后，Claim/COMMIT 耗时及前面各条处理均消耗预算。不拿 SQL 的 UTC lease_until 与宿主 wall clock 比大小；要求数据库时间在租约期间正常连续运行，最终归属仍由 SQL 当前 token/attempt/deadline fencing 判定。预算不足以预留结算时，不开始下一次发布，剩余租约到期恢复。

每次发布的 deadline 不晚于批次截止减 SettlementTimeout，并受 PublishTimeout/父 context 约束；每次结算单独受 SettlementTimeout、批次截止及父 context 约束。借入依赖必须遵守 deadline/context。Redis v8 网络中途取消最多等当前 I/O deadline；资源 owner 仍须配置有界 dial/read/write/pool 超时，不通过脱离上下文的 goroutine 宣称操作已结束。

| 结果 | 行为 |
| --- | --- |
| 合法提示，及时发布成功（包括零订阅者） | Complete 当前 lease |
| payload 无效 | Retry，固定 `INVALID_NOTIFICATION` |
| 已调用发布但报错、超时或迟到 nil | Retry，固定 `NOTIFICATION_UNKNOWN`；可能已经发布，容忍重复 |
| 父 context 取消 | 停止新发布/结算，保留未结算 lease 等待到期 |
| 结算错误、owner 丢失、提交/回滚未知 | 立即返回，不重发/改写当前 lease，不继续后续条目 |
| 结算回执身份/状态/时间字段形状矛盾 | 返回 invariant 错误，不统计完成 |

`RunBatch` 回传 Claimed/Published/Completed/Retried/Deferred，错误时保留此前已确认计数。Published 只统计及时返回 nil 的调用，未知发送不计；Deferred 含剩余及结果未确认的条目，不代表它们在数据库中必定未完成。错误展示固定 stage，底层 cause 仅通过 Unwrap 保留，不拼接正文、SQL 或连接信息。生产 Redis client 可能有内部重试，仍是至少一次语义。

`Run` 每批（含空批、部分处理批）后可取消地等待 PollInterval，SKIP LOCKED 暂空继续轮询；遇错误返回生命周期 owner，由 P5.1.4 决定监督/就绪/退出策略。没有热循环重试。后续 [P5.1.4](outbox-lifecycle.md)已实现正常 drain 与强制取消的区别及 pending/资源释放责任；本循环接口的 context 取消仍是停止处理，角色 drain 由服务层管理。

## 验证

```powershell
go test -mod=readonly -race ./messaging ./workers ./database -run '^TestOutbox' -count=30 -timeout=120s
go test -mod=readonly -race -tags=integration ./workers -run '^TestOutboxLoopIntegration$' -count=1 -v -timeout=8m
```

默认测试覆盖身份/路由/内容 hash 矛盾、重复/别名/嵌套字段、uint64 上界投影、严格终态、无正文、顺序处理、Claim 未知不发、未知发布回退、迟到结果、取消、结算未知后保留剩余批次、耗尽预算不发、空批轮询及非法边界。RESP peer 完整读取 PUBLISH 后不回响应，验证真实 Redis client 等待受 deadline 限制，不把它当作真实 Redis 已处理的证据。

真实独立 MySQL/Redis suite 三组：生产 PersistCommand 生成私聊/群聊成功和权限拒绝事件，订阅确认后核对路由和五条无正文提示；单连接 pool 下，发布回调可查询已提交 leased 行，证明未持有 SQL 连接等待网络。关闭订阅后真实 PUBLISH 成功，回调注入 unknown，固定原因回退后自然到期重领相同 eventId，零订阅者也可完成。额外坏 payload 不发布，仅回退。

最终 4 command / 3 message / 2 conversation / 8 Outbox（7 completed、1 invalid pending），cursor/quarantine 均 0，schema 指纹不变。unknown 是**真实发布成功后返回错误注入**，不是 Redis TCP 丢响应；后续 [P5.1.5](outbox-acceptance.md)已补齐真实提交/发布未知、进程故障和角色交叉。runId、命令结果和资源清理见[实施进度](message-reliability-mq-progress.md)。

P5.1.3 及后续 P5.1 故障范围均已完成，P4 显式 worker 三角色交叉已补齐；P5 与默认完整 worker/all 仍待收口。
