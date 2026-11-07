# P6.3.4.1：发送结果通知与 HTTP 交叉

P6.3.4 在实施前拆成两项：本项实现严格 command.result 接入及真实生产 API/WebSocket 交叉；后续 P6.3.4.2.1 已验证[真实消费基线](client-sender-pipeline.md)，P6.3.4.2.2.1 [回执丢失恢复](client-sender-lost-response.md)已完成，P6.3.4.2.2.2 [查询/冲突交叉](client-sender-recovery-guards.md)已完成，P6.3.4.2.3.1 [SQL 查询故障恢复](client-sender-sql-recovery.md)已完成，P6.3.4.2.3.2 [Redis 通知故障恢复](client-sender-redis-recovery.md)已完成，P6.3.4.2.3.3.1 [消费连接监督恢复](client-sender-consumer-recovery.md)已完成，P6.3.4.2.3.3.2.1 [发布前断连恢复](client-sender-publisher-recovery.md)已完成，P6.3.4.2.3.3.2.2 [真实确认丢失恢复](client-sender-confirm-loss.md)已完成，P6.3.4.2.3.3.2.3.1 [消费 ACK 丢失恢复](client-sender-consumer-ack-loss.md)已完成，P6.3.4.2.3.3.2.3.2.1 [COMMIT 响应丢失恢复](client-sender-commit-loss.md)已完成，P6.3.4.2.3.3.2.3.2.2.1 [延迟 COMMIT 与优雅 drain](client-sender-commit-drain.md)已完成，P6.3.4.2.3.3.2.3.2.2.2 [强制关闭与迟到回调](client-sender-commit-forced-close.md)已完成，下一项 P6.3.4.2.3.4 隔离/重放与发送侧矩阵收口。P6.3.4/P6.3 整体尚未收口，客户端接收消息集和连接恢复仍归 P6.4/P6.5。

## 接入契约

`HTTPRunner.ObserveCommandResult(ctx, body) (needsQuery bool, err error)` 接收已经鉴权、属于当前物理连接的 WebSocket 文本帧。调用方负责连接/session 选择、ready/ping/pong、读期限和读字节上限；本项不是自动连接或重连循环。

共享 `messaging.DecodeCommandResult(body, sender)` 只接受 command.result，最多 16384 字节。包装和 data 字段、可选嵌套 message 均按 v1 严格 JSON 规则解析，拒绝未知/重复/别名/null、非法 Unicode、非字符串序号及尾随内容。调用既有 NewCommandResultFrame 校验 rejected/persisted 字段组合、规范消息与路由，并验证 eventId 等于该用户/业务键的 command.result 事件摘要。

本地适配再次匹配待发送 key/hash、commandId 和原请求 conversationId。合法但本地不存在的命令返回 ErrNotFound，不建新记录；损坏或身份不匹配返回 ErrProtocol，不写快照，也不自动发 HTTP。

| 通知 | 行为 |
| --- | --- |
| persisted 含完整规范 message | 转成共享 CommandStatus，调用核心单调状态保存 |
| rejected | 保存稳定拒绝终态，不携带消息或 seq |
| persisted 只有 messageId/conversationId/seq 引用 | 返回 needsQuery=true；不生成 MessageDTO，不将引用当成本地完整终态 |
| 同一完整终态重复到达 | 幂等，不重复写入或退回 accepted |
| 已知完整 persisted 的相同引用 | 幂等，无需重复查询 |
| 与已知终态冲突的结果或引用 | ErrContradiction，保留原终态 |

needsQuery 由调用方在当前 HTTP 任务结束后安排 Poll/Recover；同键仍遵循既有单任务规则。本项不会在被阻塞的 Submit 内部另发并行查询。服务器查询自己的既有终态不受后续群成员变更影响，因此权限变化后的引用可通过状态入口核对。

通知拥有独立的单个处理槽，不占用 HTTP 请求槽；即使 MaxConcurrent=1 且 202 回执尚未返回，也能先持久保存业务终态。等待和保存共享 StoreTimeout（默认三秒），不创建后台通知队列或丢弃回调。Close 撤销准入并等待实际通知调用退出；已知本地保存成功不因随后取消被抹除，关闭后拒绝迟到帧。存储错误遵循停止操作、重新加载的既有契约。

通知中完整消息只保存在待发送结果中，不推进接收连续游标、不发送业务 ACK，也不代表另一接收端保存完成。

## 真实生产验收

`TestProductionSenderCommandResultIntegration` 使用本轮独立 MySQL/Redis/RabbitMQ、真实 JWT/黑名单、默认 API 工厂、真实 publisher confirm、生产 WebSocket/通知 SQL 核验与 FileStore。健康输入是测试夹具，验收不声明完整进程健康汇总或默认 worker/all 可用。

回执屏障仅在真实 broker accepted 后暂停 Publish 返回，不伪造成功；夹具读取并核对真实信封，再显式调用生产 PersistCommand，成功返回后才执行夹具 control ACK，随后显式运行生产 OutboxLoop/RedisNotificationPublisher。**这不是生产持久化 consumer 或全后台进程管线**，后续真实消费服务验收见[P6.3.4.2.1](client-sender-pipeline.md)，故障矩阵仍待后续。

四组场景：

1. persisted 在 202 回执前通过真实个人订阅到达，立即保存完整 SQL DTO；重复通知幂等，迟到 accepted 只结束 inFlight。
2. NOT_FRIENDS rejected 在 202 前保存；迟到接受不能覆盖业务拒绝。
3. 群消息提交后退群，生产通知去除完整 message，只保留引用；本地不伪造终态，HTTP 回执结束后查询自己原命令取得权威终态。
4. broker 已确认但 HTTP 响应仍被阻塞，调用方实际超时并保存 unknown；后续真实通知恢复 persisted，保留原 ID/原请求和 attempts=1。

最终再次关闭/重开磁盘，核对四条本地终态、原请求、规范消息和 SQL 原终态。数据库为 4 command / 3 message / 2 conversation / 7 completed Outbox / 0 delivery cursor / 0 quarantine；本轮提交队列为空，schema 指纹不变。没有运行接收侧保存、ACK、性能或真实前端验收。

## 验证命令

```powershell
go test -mod=readonly -p=2 -race ./messaging ./tests/clientsim -run '^Test(CommandResult|HTTP)' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSenderCommandResultIntegration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

默认测试另覆盖 HTTP 阻塞时独立通知保存、完整结果/引用/最大 uint64、重复与矛盾结果、坏 eventId/字段/消息/路由、未知本地 key、存储错误的写前/写后结果、Close 等真实存储调用结束及关闭后拒绝迟到帧。实际 runId、时间、修正过的拓扑准备顺序和资源核验见[实施记录](message-reliability-mq-progress.md)。
