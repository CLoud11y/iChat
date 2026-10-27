# P5.2：统一通知帧与当前权限核验

增加 `messaging.DecodeNotificationHint`、两种统一 `ServerFrame`、`MessageRepository.ResolveNotification` 和 `service.NotificationDispatcher`。它们把[Outbox 无正文提示](outbox-execution.md)转换为来自 SQL 的规范消息或命令终态，不直接透传 Redis/Outbox 中的正文。

本项提供单 writer 的分发适配；后续 [P5.3 生产会话与订阅](reliable-websocket.md)已将它接入现代 API `/auth/chat`。旧 Chat/旧频道保持独立旧路径，业务 ACK 在 P5.4 实现。

## 帧契约

两类数据帧都使用 `{ "type": ..., "eventId": ..., "data": ... }`。eventId 沿用规范 commandId+eventType 的 v1 固定派生，不包含租约或通知尝试；重复通知保留相同事件身份。seq 是规范 uint64 十进制字符串，不能转换为浮点数。

| type | data | 当前权限 |
| --- | --- | --- |
| `message` | 与同步/历史接口相同的完整 MessageDTO | 当前有效用户；私聊只向实际接收者频道发送；群消息要求群未删除且接收者仍为有效成员 |
| `command.result` persisted | clientMessageId、payloadHash、state、messageId、conversationId、seq；有内容读取权限时增加完整 message DTO | 只向原发送者；群成员资格/群有效状态丢失时保留自己的终态与引用，省略 message |
| `command.result` rejected | clientMessageId、payloadHash、state、reasonCode；无消息或引用 | 当前有效用户且为原发送者，不因目标不存在/发送权限拒绝而丢掉自己的终态 |

成功结果引用始终存在，即使 message 被省略也仍为 persisted，不重新判定历史发送结果为 rejected。发送者可以依据引用、状态查询和允许的补拉处理自己的发送记录。退群/删群后不能利用旧结果事件继续取得正文。现有 `GetCommandStatus` 的发送者状态查询契约不变，本项不重写其 HTTP 行为。

私聊历史权限沿用 P2.7：参与者身份决定既有消息的可读性，解除好友关系只影响新的发送资格，不撤销已经提交的历史。非参与者、错误收件人频道不能借提示读取内容。群历史按当前成员资格判断，允许当前成员读取其可见会话，未引入加入时间水位政策。

`ServerFrame` 只能通过 `NewMessageFrame` / `NewCommandResultFrame` 创建，校验规范 DTO、身份/hash、时间、路由、非零 seq 和终态字段组合。零值无效；Bytes 返回副本，构造后调用方修改 message/seq 指针不会改变帧或匹配元数据。普通格式化隐藏正文。

## 不可信提示与 SQL 门禁

1. `DecodeNotificationHint(channel, body)` 限制 16KiB、有效 JSON/UTF-8/Unicode，拒绝重复键、别名、未知字段、null、缺字段、跨事件多余字段、错误版本、非规范 seq/UUID/ID。只接受实际发布器的无正文投影。
2. 重算 eventId，并将 payload 与实际 Redis 频道绑定。command.result 还按个人频道 UID+clientMessageId 重算 commandId；私聊频道必须属于会话参与者、群频道必须与 group 会话一致。解码成功只证明结构与路由一致，不证明当前权限。
3. 调用方传入鉴权上下文中的 UID。个人频道 UID 不符在 SQL 前抑制；群频道仍须查询 SQL，订阅本身不授予访问权。
4. `ResolveNotification` 建立短 READ COMMITTED 事务，锁读当前用户有效状态；从 SQL 读取规范命令，核对 commandId/hash/终态。群内容权限以当前 group 和 relation 的 FOR SHARE 读判断，持有权限锁直到本次数据材料化结束。
5. message 提示比对 canonical conversationId/messageId/seq，并验证私聊 SQL 的真实 receiverId 等于个人频道用户。command.result 比对 sender、客户端键/hash、状态/原因与消息引用。规范消息重新校验内容 hash、身份及已提交会话水位；所有正文和媒体类型均来自 SQL。
6. 事务 Rollback 释放所有锁与连接后才返回帧。SQL/扫描/规范数据/权限读取/取消或释放错误均清空帧；缺少规范命令和无权限的 message 返回空帧。命令拥有者失去群内容权限时只返回规范终态引用。

READ COMMITTED 的当前锁读会等待先行撤权事务，随后按其提交结果决定权限；不会用进入等待前的旧成员快照放行。读取先获得权限锁时，撤权更新需等待该次短读取结束。

此权限判定有明确时间边界：权限锁保护 SQL 材料化阶段，网络写入在锁释放后，撤权仍可能在随后发生。不能声称与外部 WebSocket 字节写入原子提交。P5.3 应在单 writer 即将发送时调用分发器，队列保存提示而不是长期保存已授权正文，缩短检查到发送的窗口；会话替换、退出和慢客户端队列处理由该层负责。

## 分发适配

`NewNotificationDispatcher(reader)` 闲置构造并拒绝 nil/typed nil reader。`Dispatch(ctx, uid, channel, body, enqueue)` 要求有 deadline 的 context；严格解码和用户范围检查后调用仓储，核对返回帧与原提示完整匹配，再检查取消并调用有界 enqueue/write 回调。

回调只在 SQL 已返回、所有连接/锁释放后执行，不在事务中做 Redis/WebSocket 网络调用。数据库失败、错误帧、取消或 enqueue 失败均不报告成功；回调成功仅表示本地出站操作返回，不表示客户端已接收或保存。错误以固定阶段隐藏诊断，Unwrap 保留底层分类；不拼接正文、SQL 或连接信息。

本项没有定期补拉、通知重放、业务 ACK、游标推进、Outbox 领取/完成、JWT 鉴权或 session fencing。authenticated UID、当前活动 session 和回调生命周期由后续会话层提供；通知和帧重复仍由 eventId/messageId/seq 去重及补拉收敛。

## 验证

```powershell
go test -mod=readonly -p=2 -race ./messaging ./database ./service -run '^Test(Notification|ServerFrame)' -count=30 -timeout=120s
go test -mod=readonly -p=2 -race -tags=integration ./service -run '^TestNotificationDeliveryIntegration$' -count=1 -v -timeout=7m
```

默认测试覆盖严格提示/频道、uint64 上界、帧与原提示匹配、完整/引用终态、指针与字节副本、跨用户 SQL 前抑制、退群/删群/用户失效、读/释放失败零帧、回执矛盾、取消和 enqueue 错误。

真实独立 MySQL/Redis suite 八组，使用生产 PersistCommand 生成私聊、群聊和拒绝事件，经生产 Outbox 投影、真实 Redis PUBLISH/订阅和本地 WebSocket 连接验证统一帧。单连接 SQL pool 下，出站回调可立即 SELECT 1，确认网络前已释放连接；逐字比较 DTO 与规范 SQL 状态查询结果。

另外验证旧群订阅退群后抑制正文、自己的成功群终态降为引用、删群/删用户后失败关闭、解除好友仍可读既有私聊；真实撤权持有 relation 行锁时，processlist 确认 FOR SHARE 正在等待，提交后抑制旧提示；锁等待取消无帧，释放后 pool 和通知可恢复。

WebSocket endpoint 是测试传输夹具，UID 来自测试注入，不冒充生产聊天路由或 JWT 会话接线。最终 3 command / 2 message / 2 conversation / 5 pending Outbox（attempts=0）/ 0 cursor / 0 quarantine，schema 指纹不变。实际 runId、验证结果和清理见[实施进度](message-reliability-mq-progress.md)。
