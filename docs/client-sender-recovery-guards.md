# P6.3.4.2.2.2：查询失败、同键冲突与迟到结果

本项完成发送恢复中“不应重发”的三个生产接口交叉场景，与[接受回执丢失恢复](client-sender-lost-response.md)共同收口 P6.3.4.2.2。后续 P6.3.4.2.3.1 已完成[SQL 查询故障恢复](client-sender-sql-recovery.md)，P6.3.4.2.3.2 [Redis 通知故障恢复](client-sender-redis-recovery.md)已完成，P6.3.4.2.3.3.1 [消费连接监督恢复](client-sender-consumer-recovery.md)已完成，下一项 P6.3.4.2.3.3.2 发布通路/确认未知与在途消费交叉。P6.3.4.2/P6.3、默认 worker/all 与接收侧恢复仍未收口。

## 共享夹具

`TestProductionSenderRecoveryGuardsIntegration` 的三个子场景各自使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT/黑名单、默认 API 工厂、publisher confirm、生产 PersistenceService/后台 OutboxService、v1 WebSocket 与 FileStore。

既有 HTTP 代理增加显式选项：读取实际生产响应后，可原样转发、关闭下游 TCP 或等待屏障再原样转发。状态、身份、HTTP code 和正文来自实际 API，代理不生成 pending、409 或终态。旧回执丢失 suite 的默认行为仍是丢弃第一次真实 202、暂缓第二次真实 202；本轮同步回归。

调用方仍使用单个 HTTP 槽、十秒请求期限以容纳测试屏障；Recover 为三轮、最多两次提交预算，默认退避/真实计时器不变。通知只有调用 ObserveCommandResult 才进入本地状态；测试显式控制该调用，不把服务器已发送通知等同于本地已保存。

## 三组验收

| 场景 | 实际证据 | 本地结果 |
| --- | --- | --- |
| lost_status_responses_never_authorize_repost | 第一次真实 202 被丢弃；无消费者时，三次实际 GET 均返回匹配的 pending，代理逐次断开下游 TCP；虽有两次提交预算，HTTP 顺序仍只有 POST→GET→GET→GET，队列 ready=1、consumers=0 | 返回 ErrRecoveryExhausted/ErrHTTPUnavailable，保留原 unknown、attempts=1 和原请求；磁盘重开后保持。恢复转发并启动生产消费后，再 GET 原 persisted，保存终态并再次重开磁盘；全程无重发 |
| same_key_conflict_blocks_original_overwrite | 原请求通过实际 HTTP、消费与通知保存完整 persisted。同一 Sender.Queue 拒绝改内容复用 key；另一个私有目录显式建立同 key/不同 hash 的 interrupted checkpoint，然后只 GET 实际 409/code=-1/ID_CONFLICT | 查询按请求的 hash 返回冲突，不暴露原 hash/消息；冲突记录与原请求都保留。原 hash 的迟到真实 command.result 被 ErrProtocol 拒绝；重开冲突磁盘后 Recover/Submit 均 ErrBlocked，不再 HTTP；原磁盘和 SQL 终态不变 |
| terminal_notification_precedes_late_pending | 第一次真实 202 丢失，重开磁盘后 GET 实际 pending，在代理读取完整响应后暂缓返回；启动生产消费/Outbox，完整 command.result 通过真实 WebSocket 到达并保存，然后才放行旧 pending | 同键 Recover 尚在请求中时通知可以保存终态；迟到 pending 被单调核心忽略，保留完整 persisted/attempts=1，不触发预算中的 POST。HTTP 仅 POST→GET；重开磁盘后仍完整，已知终态再次 Recover 不发 HTTP |

冲突分支的独立 checkpoint 通过 Queue/BeginSubmit/CompleteUnknown 明确建立，表示本地已记录但未确认的派发。没有发送冲突 HTTP POST/MQ 消息，不能将该分支报告成消费者冲突隔离验收；其生产消费处置和隔离恢复归后续矩阵。

失败查询分支验证的是完整生产 pending 回应在代理到客户端这一段丢失：客户端没有取得权威 pending，因此不能从失败推导可重发。损坏 JSON、HTTP 503/身份错误的默认测试沿用 P6.3.3；本轮没有声称实际 SQL/Redis/broker 故障已覆盖。

## 集合与关闭

每个独立环境最终均为 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，原 SQL 终态逐字段保持。失败查询/迟到 pending 分支保留一份磁盘终态；冲突分支保留一份原终态磁盘及一份同键异内容的 blocked 磁盘，未覆盖或删除请求。

每组先 drain/close/join 唯一生产消费连接，等待 broker 移除消费者后以不取消息的 QueueInspect 核对 ready=0、consumers=0；Outbox 使用共享 Claim 边界协调正常停止，不忽略未知错误。再次查询原终态、复核迁移指纹。测试不领取 MQ delivery、不直接 PersistCommand/RunBatch/control ACK；清理只针对归属本轮的资源。

API 健康输入仍是夹具，worker 服务显式装配，未启用默认 worker/all、监督恢复、隔离角色或实际前端。磁盘重开是在同一进程内关闭并重建 owner，接收消息集/连续游标/业务 ACK 与自动重连仍归 P6.4/P6.5。

## 验证命令

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(RecoveryGuards|LostResponseRecovery)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

首条执行新三组和旧两组真实场景。标签编译通过不等于执行其他故障 suite；具体时间、runId 与资源核验见[实施记录](message-reliability-mq-progress.md)。
