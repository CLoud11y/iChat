# P6.3.4.2.3.2：通知 Redis 故障、Outbox 替换与查询兜底

本项验证通知发布失败不会阻塞业务提交和 MQ ACK；客户端可先查询原 SQL 终态，之后由新的 Outbox 实例完成原事件，迟到通知不改写已保存终态。accepted/unknown 两条路径一起验收，无额外细分。后续 P6.3.4.2.3.3.1 已验证[消费连接监督恢复](client-sender-consumer-recovery.md)，P6.3.4.2.3.3.2.1 [发布前断连恢复](client-sender-publisher-recovery.md)已完成，P6.3.4.2.3.3.2.2 [真实确认丢失恢复](client-sender-confirm-loss.md)已完成，P6.3.4.2.3.3.2.3.1 [消费 ACK 丢失恢复](client-sender-consumer-ack-loss.md)已完成，P6.3.4.2.3.3.2.3.2.1 [COMMIT 响应丢失恢复](client-sender-commit-loss.md)已完成，P6.3.4.2.3.3.2.3.2.2.1 [延迟 COMMIT 与优雅 drain](client-sender-commit-drain.md)已完成，P6.3.4.2.3.3.2.3.2.2.2 [强制关闭与迟到回调](client-sender-commit-forced-close.md)已完成，P6.3.4.2.3.4.1 [隔离审计与发送门禁](client-sender-quarantine.md)已完成，下一项 P6.3.4.2.3.4.2 受控重放与客户端原请求恢复，实施前按实际复杂度细分。

## 故障范围与装配

`TestProductionSenderRedisRecoveryIntegration` 的两个子场景各使用独立 runId 的 MySQL/Redis/RabbitMQ、JWT/黑名单、默认 API 工厂、publisher confirm、生产持久化消费、后台 Outbox、WebSocket 与 FileStore。

Outbox 有单独借入的真实 Redis 客户端，经过回环 TCP 中继；客户端关闭隐式重试、连接/读/写/池等待各一秒。初次真实 PING 成功并启动服务后，关闭中继现有 socket 并拒绝新连接。此客户端 PING 必须失败，API 直连 Redis PING 仍成功：只中断通知发布/探针路径，鉴权和个人订阅不受影响，不停止 Redis 进程。

共享消费夹具增加可选通知客户端、Outbox 参数和预期错误核对；默认入口继续用原客户端/参数，原三组消费基线同步回归。本夹具选择 Outbox poll=20ms、探针间隔=5s、重试上限=1s（Base 仍 1s，保留数据库时钟与真实随机抖动），使发布失败先实际结算并限制后续读取等待；没有修改生产默认值、手动推进 available_at 或伪造存储结果。

## 故障到恢复顺序

1. 通知通路切断后只实际发送一次命令：accepted 分支正常保存真实 202，unknown 分支由 HTTP 代理完整读取实际 202 后断开客户端 TCP。原请求、键/hash 和 attempts=1 保持，没有客户端完整终态。
2. 生产消费者提交原命令并 ACK。真实 RedisNotificationPublisher 发布失败，两个原 Outbox 事件均经生产 Retry 回到 pending，attempts>0、last_error=NOTIFICATION_UNKNOWN，无 lease 或 completed_at。随后真实 Redis PING 失败，旧 Outbox 撤销就绪、以 dependency probe 错误结束并关闭 Done。
3. 在 Redis 仍中断时，停止并加入唯一消费连接，等 broker 移除消费者再 QueueInspect，ready=0、consumers=0。若 MQ ACK 被通知阻塞，关闭后会重入队，检查必须失败。
4. 客户端以一轮/一次可用提交预算 Recover，只 GET 原 persisted，依靠仍正常的 API SQL/Redis 鉴权保存完整终态；查询不修改 pending 事件。关闭并重开磁盘，仍保存原请求、attempts=1 和完整结果。
5. 恢复通知 TCP 通路，真实 PING 成功。旧实例为 single-use，Start 仍 ErrOutboxServiceStart、原错误保持、就绪仍 false。只有确认旧 Done 已关闭后，测试才显式构造并启动新的生产 OutboxService，借用原 store/client 领取原事件。
6. 等真实 retry available_at 到期后，新实例完成两条原事件。迟到的完整 command.result 经实际生产 WebSocket 到达，连续 Observe 两次均幂等；本地终态逐字段保持。两条原事件的 eventId/payload 不变、attempts 递增、status=completed、last_error 清空。再次重开磁盘仍保持结果。

本地 HTTP 恰为 **1 POST + 1 GET 200**，不因通知失败或角色退出生成新 ID/重发原请求。每条路径分别保留 accepted/unknown 到查询兜底前，不能用业务已提交或 Outbox 正在重试伪装客户端已保存。

## 生命周期和边界

旧角色的故障只在实际 Done 关闭、核验 dependency probe 错误后被登记。cleanup 只接受这个确切错误；默认夹具及新实例的错误不被忽略。cleanup 捕获各自实例，替换当前 Outbox 不会让旧 owner 的清理误操作新实例。新实例先 join，通知客户端关闭、中继任务退出后才回收本轮基础设施。

角色替换由测试显式构造，不声称 RunRuntime 自动监督重启、默认 worker/all 已装配或 Redis 进程宕机已覆盖。完整 MQ/消费角色恢复与隔离重放仍归后两项，接收消息集/连续游标/业务 ACK、自动重连与实际前端仍未收口；磁盘重开是在同一进程内关闭并重建 owner。

## 最终集合与验证

每个独立场景最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，原 SQL 终态及 schema 指纹保持。磁盘只有一条完整终态，原 UUID 大写、目标前导零、Unicode/NUL/换行正文及纳秒时区时间保持。测试不取 MQ delivery、不直接 PersistCommand/RunBatch/control ACK、不直接结算 Outbox。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(RedisRecovery|Pipeline)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

第一条同时回归三组默认消费基线；标签编译不代表执行其他故障 suite。具体 runId、时间与资源核验见[实施记录](message-reliability-mq-progress.md)。
