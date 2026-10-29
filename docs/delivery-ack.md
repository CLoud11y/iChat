# P5.4：当前会话绑定的业务 ACK

现代 API 的生产 `/auth/chat` 接收 `ack`，SQL 成功提交后由原有单 writer 发送 `ack.confirmed`。两帧均不含 eventId：

```json
{"type":"ack","data":{"conversationId":"p:1:2","deliveredSeq":"12"}}
{"type":"ack.confirmed","data":{"conversationId":"p:1:2","deliveredSeq":"12"}}
```

`conversationId` 必须为规范 `p:<小UID>:<大UID>` 或 `g:<群ID>`；`deliveredSeq` 必填，为规范十进制 uint64 字符串，允许 `"0"`。拒绝 JSON 数字、负数、前导零、溢出、null、重复键、大小写别名、未知字段和缺失字段。客户端不能传 userId/sessionId；身份来自 JWT 认证的物理连接及服务器生成的 sessionId。JSON ping/pong 和 WebSocket 控制心跳仍不推进业务游标。

## 当前会话门禁

`ReliableChat.acknowledge` 明确接收本会话及 sessionId，在每 UID 的 gate 中核对 sessionId、current 指针和 context，然后调用注入的 `DeliveryAcknowledger`。生产默认工厂把同一个 `MessageRepository` 显式传给通知读取和 ACK 接口；缺失或 typed nil ACK 依赖在构造时拒绝。

gate 与新会话激活、实际 socket 写入共用，并覆盖完整 SQL 回调及 COMMIT 返回，不能只在检查 current 后立即释放。新物理连接可完成 HTTP upgrade，但必须等待旧的已准入 ACK 返回后才能激活和发送 ready；旧 ACK 与切换按这个顺序串行。替换激活后旧连接不能再调用 ACK 仓储，也不能写确认帧。SQL 不调用 socket，不把网络写入放进数据库事务。

ACK 复用 `ResolveTimeout`（默认 2 秒），包括等待 gate 和 SQL；确认帧复用既有队列和写期限。操作/权限/协议失败、提交未知或迟到结果终止当前会话，不发送成功确认，不把内部 SQL 错误暴露到帧。队列满或写失败可能发生在提交之后；保留已提交游标，由重连重复 ACK 恢复。确认是可丢失的通知，不是客户端收到消息的第二次事务。

即使注入回调违反 context、超时后仍未返回，原 handler 仍持有 gate 和依赖并等待其真实退出。API observer 可以返回 pending，但不能提前关闭共享 SQL/Redis。新连接等待超过 setup 期限则关闭，不绕过旧 gate。实现限于既定**单 API 实例**，没有跨实例 session 表或全局 fencing；不能直接横向扩容。

## 数据库事务

`MessageRepository.AcknowledgeDelivery` 使用 READ COMMITTED 写事务，依次执行：

1. 检查规范会话 ID、鉴权 UID；私聊只能由两位参与者 ACK。
2. FOR SHARE 锁读当前有效用户；群聊继续锁读未删除群及本用户当前有效成员关系。锁等待结束后读取最新权限，不依据旧订阅授权。解除好友不撤销已提交私聊历史的 ACK 权限。
3. FOR SHARE 读取现有 conversation，校验实际 type/参与者/群身份与规范 ID 完全匹配。不存在的 conversation 拒绝，包括 ACK 0；不会创建空会话。检查 `seq <= last_seq`。
4. 按 `(user_id,conversation_id)` 原子 UPSERT，`delivered_seq=GREATEST(原值,请求值)`；仅前进时刷新数据库 `UTC_TIMESTAMP(6)` 的 updated_at。首次 ACK 0 创建零游标；同值或较小值不改原时间。
5. 在同一事务锁读实际保存的 delivered_seq，检查请求值不大于结果、结果不大于已提交 last_seq，再提交。只有提交成功才返回回执值；较小 ACK 也返回原来的较大进度。

权限锁持有至提交/回滚，防止检查后撤权穿过写入；会话行锁保护 last_seq 与 cursor 更新的关系。并发 ACK 在 cursor 唯一键处串行，不覆盖其他用户的行。读取/写入/回滚/取消错误均无成功回执；COMMIT 错误保留 `ErrDeliveryCommitUnknown`，不能据此判断未提交或补偿删除游标。

服务器验证身份、权限和范围，无法证明客户端确实已经连续写入本地磁盘。客户端必须先连续保存，再 ACK。此游标不表示已读，不作为 P6 本地消息恢复的唯一起点；本地清空后仍须从实际本地游标补拉。

## 验证

```powershell
go test -mod=readonly -p=2 -race ./database ./messaging ./service . -run '^Test(DeliveryACK|APIChatACK)' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration ./tests/testenv -run '^TestDeliveryACKIntegration$' -count=1 -v -timeout=7m
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionV1ChatIntegration$' -count=1 -v -timeout=7m
```

ACK 专项八组使用独立 MySQL/Redis、生产 JWT/router/订阅和真实 WebSocket：零/重复/较小 ACK、独立用户/越权/水位限制、32 路并发乱序、真实撤权锁等待及取消、提交后确认/重连、实际 COMMIT OK 丢失、会话替换交叉、超时后迟到回调，以及无权群/伪造身份。COMMIT 故障由既有 classic MySQL TCP 代理消费真实成功响应后丢弃，不用人为返回 EOF 代替 SQL 事务证据。

专项最终 9 command / 9 message / 2 conversation / 18 untouched pending Outbox / 3 cursor / 0 quarantine；私聊接收者为 8、发送者为 1，群接收者为 1。迁移指纹不变。专项通过生产 PersistCommand 建立业务集合，不运行 MQ 消费或 Outbox，不声称 HTTP 202 到客户端的完整恢复联调。原 API 默认工厂 suite 新增真实 ACK 2→0→2，最终 5 message / 10 completed Outbox / 1 cursor；原会话八组继续回归。

默认测试另覆盖严格 JSON、uint64 上界、SQL 各阶段错误/损坏、提交未知零回执、socket 实际确认门禁、旧/错 session 不调用 SQL、panic/迟到回调和 API pending 后延迟释放客户端。具体运行证据见[实施记录](message-reliability-mq-progress.md)。P6.2.2 [会话列表 HTTP](conversations-http.md)已完成，下一项 P6.2.3 补拉/历史 HTTP 接线，完整默认 worker/all 和实际客户端恢复仍待后续。
