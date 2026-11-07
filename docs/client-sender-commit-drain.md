# P6.3.4.2.3.3.2.3.2.2.1：COMMIT OK 延迟、优雅 drain 与发送终态

本项验证生产消费已提交、真实 COMMIT OK 仍在网络响应屏障时，优雅 drain 的观察期限耗尽只报告 pending，原回调/SQL/会话责任保留。客户端可查询保存已提交终态；之后原样转发迟到 OK，原消费完成 ACK，再正常 drain/close，没有新一代或重投。P6.3.4.2.3.3.2.3.2.2.2 [强制关闭与迟到回调](client-sender-commit-forced-close.md)已完成，下一项 P6.3.4.2.3.4 隔离/重放与发送侧矩阵收口，实施前继续细分；完整发送矩阵、默认 worker/all 与完整客户端恢复仍未收口。

## 实际顺序

`TestProductionSenderCommitDrainIntegration` 使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT、生产 API/WebSocket、PersistenceSupervisor、后台 Outbox 与 FileStore。只有消费 SQL pool 经过代理；API/Outbox SQL、publisher、Redis 和管理连接正常直连。AMQP 代理只观察实际帧，本轮不 arm、不丢 ACK。

1. 生产消费者就绪后，HTTPRunner 只 POST 一次并保存实际 202/accepted。观察原 Basic.Deliver：redelivered=false，完整信封严格解码/规范核验，匹配原 command、HTTP 回执身份及 publishAttemptId。
2. 消费事务完成，SQL 包代理截住真实服务器 COMMIT OK。独立 SQL 看到 persisted 和两条 completed Outbox；broker 管理实际 ready=0/unacked=1/consumers=1，原消费角色仍 Ready，投递一次、消费 ACK 零次。本地仍 accepted/attempts=1/无 Result。
3. 以 150ms 观察期限调用生产 supervisor.Drain，必须返回同时匹配 ErrPersistenceSupervisorPending 和 DeadlineExceeded 的错误。角色撤销 Ready、状态 Stopping、Done 未关闭；旧组仍未 closed，active=1/无重叠，generation 不变，ACK=0，SQL OK 尚未转发或丢弃。借入 SQL pool 仍可 Ping；没有将超时观察误当作资源加入完成。
4. 响应仍被保留时，客户端有限 Recover 只 GET 原 persisted，真实 WebSocket 完整结果幂等；原请求/身份/hash、完整 SQL DTO、attempts=1/非 inFlight 终态保存。关闭并重开磁盘 owner 后完全一致。
5. 解除响应屏障，同一 SQL 连接上的同一份真实 OK 原字节转发，完整写出后记录元数据。驱动实际返回成功；原生产回调在 drain pending 之后完成 ACK，实际连接/channel/tag 与最初 Basic.Deliver 一致，只有一次消费 ACK，未重投。
6. 原 SQL 终态、两条 completed Outbox 的 eventId/payload、客户端终态保持。已知终态 Recover 不发 HTTP，再次磁盘重开保持。后续正常 Drain/Close、Done 和所有会话都加入，generation 不变、Failures=0、Err=nil、active=0/无重叠。唯一消费者退出后实际 QueueInspect 无重入队，最终管理 ready/unacked/consumers 全零；借入 SQL pool 仍可 Ping，由测试 owner 最后关闭。

最后的真实 ACK 证明优雅 drain 观察超时没有取消已准入回调；单独的 SQL/AMQP TCP 写成功不作为 broker 完成依据。活动连接期管理指标可能保留 ACK 前采样，完成证据同时包含正确转发、唯一消费者退出后无重入队及最终管理全零。

## 代理与边界

既有消费 SQL 丢响应代理增加显式延迟转发模式。默认构造器仍丢弃 arm 后第一个真实 COMMIT OK 并关闭 socket；新构造器则在屏障解除后原样转发、清除该连接等待标记，继续正常转发后续包。两模式共享完整包读取、COM_QUERY/精确 COMMIT、sequence=1/OK/零 affected rows 与 last insert id/IN_TRANS 清除校验，以及 1 MiB 包上限。只读/转发观察通道有界，模式在启动前固定；没有伪造 SQL OK、包装驱动 Commit 或直接调用 PersistCommand/RunBatch。

默认丢响应恢复同步回归，明确区分：丢响应是实际 EOF/commit_unknown、生产换代重投后 ACK；本项是成功的迟到响应、优雅 drain pending、原回调 ACK 且不换代。这里“迟到”指晚于 drain 观察期限，未超出消费处理期限，没有触发工作强制取消。

夹具沿用 45 秒消费处理期限、45 秒 setup 和 30 秒 SQL readTimeout，以容纳 SQL/管理/客户端观察屏障，生产默认值不变。失败清理先解除响应屏障，再关闭/加入 owner；所有 pool、listener/socket 和双向转发任务都在本轮环境回收前退出。

精确计数为 **1 POST + 1 GET、attempts=1；1 COMMIT / 0 ROLLBACK / 1 forwarded OK；1 actual delivery / 1 consumer ACK；generation=1、Failures=0**。最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，原 SQL 结果和迁移指纹保持。原 UUID 大写、目标前导零、Unicode/NUL/换行和纳秒时区时间保持。

API 健康输入仍为夹具，worker 显式装配，会话来源为测试调用生产 DialConsumer 的适配；不报告默认 application worker/all 工厂完成。磁盘重开为同一进程内 owner 关闭/重建。没有停止 MySQL/broker 或修改共享开发数据、schema、依赖、生产业务代码；未覆盖强制关闭/取消后的迟到回调、事务提交前故障、进程强杀或默认 RunRuntime 的完整退出编排。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSenderCommit(Drain|Loss)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

组合测试覆盖延迟/丢响应两种模式；标签编译不代表运行其他基础设施 suite。runId、实际结果和资源核验见[实施记录](message-reliability-mq-progress.md)。
