# P6.3.4.2.3.3.2.3.2.2.2：强制关闭后的迟到 COMMIT 回调与重投

本项验证真实 MySQL Commit 已成功、驱动回调仍未返回时的强制关闭：观察期限耗尽返回 pending，工作已取消但回调/SQL/会话责任仍有 owner。客户端可只查询保存原终态；释放迟到成功回调后，旧消费者保留取消错误且不 ACK，不会自动复活。待旧 owner 完全加入，显式启动新的生产 owner，由 broker 重投原信封、仓储去重并 ACK。

该项与[优雅 drain](client-sender-commit-drain.md)、[真实 COMMIT OK 丢响应](client-sender-commit-loss.md)及[消费 ACK 丢失](client-sender-consumer-ack-loss.md)补齐当前 MQ/在途消费交叉范围。P6.3.4.2.3.4.1 [隔离审计与发送门禁](client-sender-quarantine.md)已完成，下一项 P6.3.4.2.3.4.2 受控重放与客户端原请求恢复，实施前继续细分。完整发送矩阵、默认 worker/all、实际前端及接收侧仍未收口。

## 屏障和观察

`TestProductionSenderCommitForcedCloseIntegration` 使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT、生产 API/WebSocket、PersistenceSupervisor、后台 Outbox 和 FileStore。消费 SQL pool 用私有 driver.Connector 包装器，沿用既有 testenv lifecycleCommitConnector 方式；只有底层真实 Tx.Commit 返回 nil 后才阻塞首个 arm 回调，明确忽略该测试屏障期间的取消，解除后原样返回原结果。Commit/Rollback 真实调用分别计数，BeginTx 的原工作 context 保存供观察取消。

这里没有延迟或丢弃 SQL wire OK，没有伪造事务成功或注入取消结果；数据库事务已完成，尚在途的是驱动回调及 database/sql 对连接的占用。包装器保留真实连接的 Prepare/事务/Ping 路径，SQL 仍由生产仓储执行。API/Outbox SQL 与 Redis/publisher/管理通路保持正常；AMQP 代理仅观察，不 arm、不丢 ACK。

## 实际顺序

1. 原生产消费者就绪后，HTTPRunner 实际 POST 一次并保存 202/accepted。观察原 Basic.Deliver，严格核验规范 command、完整信封及实际 HTTP publishAttemptId，redelivered=false。
2. 真实 Commit 成功后进入回调屏障。独立 SQL 看到 persisted 和两条 completed Outbox，broker 管理实际 ready=0/unacked=1/consumers=1。原角色仍 Ready，投递一次、消费 ACK 零次；本地 accepted/attempts=1/无 Result，回调尚未返回。
3. 以 150ms 观察期限调用原 supervisor.Close，必须返回 ErrPersistenceSupervisorPending/DeadlineExceeded。原工作 context 实际 Canceled；角色非 Ready/Stopping，Done 未关闭，原组 active=1/未 closed、generation 不变，回调未 finished、SQL pool InUse 至少 1、消费 ACK=0。pool 仍可 Ping，不能因观察超时释放借入存储或启动重叠 owner。
4. 回调仍被保留时，客户端有限 Recover 只 GET 原 persisted，真实 WebSocket 完整结果幂等，保存原请求/身份/hash、完整 SQL DTO、attempts=1/非 inFlight。关闭并重开磁盘 owner 后终态完全一致。
5. 解除屏障，驱动原成功结果返回。生产处理器在 ACK 前核验取消，不发 ACK；旧 supervisor 的 Done 与会话实际加入后，保留 AcknowledgementFailed/Attempts=1/context.Canceled，未改成 commit_unknown，也没有重新就绪或递增 generation。原 pool InUse=0，所有旧资源 active=0。客户端传输加入不代替 broker 的消息重新就绪，最多等待 15 秒核验 QueueInspect 实际 ready=1/consumers=0，原消息仍由 broker 持有；该条件满足前不启动新 owner。
6. 只有此时测试才显式创建并启动一个新的 PersistenceSupervisor，借入仍由测试 owner 保留的同一 SQL pool，使用新的真实消费连接。旧 owner 仍处于已结束状态，不报告自动重启。broker 实际 redelivered=true，原信封逐字相同、publishAttemptId 不变；仓储复用已有终态并 ROLLBACK no-op 事务，唯一 ACK 对应新连接上的真实投递。
7. 原 SQL、两条 completed Outbox 的 eventId/payload 和磁盘终态保持。已知终态 Recover 不发 HTTP，再次磁盘重开保持。新 owner 正常 Drain/Close/Done，两个组 closed/active=0/无重叠，旧 generation 保持 1，新 owner generation=1/Failures=0/Err=nil；最终 QueueInspect 无重入队，管理 ready/unacked/consumers 全零。
8. 两个 owner 加入且消息责任已核对后，借入 pool 仍可 Ping、InUse=0。最后由测试 owner 显式 Close pool，并验证后续 Ping 失败；不是 supervisor 释放了调用方存储，也没有在 pending 时提前释放。

实际计数为 **1 POST + 1 GET、attempts=1；1 COMMIT / 1 replay ROLLBACK / 1 held callback returned；2 actual delivery / 1 consumer ACK（旧 owner 为 0）**。原 owner 终止且不复活，新 owner 的 generation 单独从 1 开始，不能合称同一 supervisor 自动换代。

最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**。原 UUID 大写、目标前导零、Unicode/NUL/换行和纳秒时区时间、完整 SQL 结果及迁移指纹保持。活动连接期管理采样不单独证明 ACK；最终同时要求正确转发、唯一消费者退出后无重入队和管理全零。

## 范围和复现

消费处理/setup 期限为测试显式 45 秒、SQL readTimeout 为 30 秒，以容纳观察屏障，生产默认值不变。失败清理先解除回调屏障，再关闭/加入 owner，最后释放 pool/代理/本轮环境。回调屏障只用于一次实际成功 Commit，不直接调用 PersistCommand/RunBatch、控制 Get/ACK 或手工改业务状态。

API 健康输入仍为夹具，worker 显式装配，会话来源为测试调用生产 DialConsumer 的适配。没有执行默认 application worker/all 自动资源工厂或完整 RunRuntime 退出编排；磁盘重开仅同一进程内 owner 重建。未停止数据库/broker、修改 schema/依赖/生产代码或操作共享开发数据；未把本项报告为网络 OK 丢失、进程强杀或完整发送侧收口。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSenderCommit(ForcedClose|Drain)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

组合回归强制关闭和优雅 drain 的取消/ACK 边界；其他 MQ 故障采用对应已完成小点的实测记录，不代表本轮重跑全部矩阵。具体 runId、计数和清理见[实施记录](message-reliability-mq-progress.md)。
