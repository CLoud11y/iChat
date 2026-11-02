# P6.3.4.2.3.3.2.3.1：提交后消费 ACK 丢失、重投与终态保持

本项验证生产消费已提交后，一个消费者 ACK 帧丢失会使 broker 仍持有原 unacked delivery；客户端可先保存原终态，连接中断后再由生产监督器换代、broker 重投，原信封/发布 attempt 不变且业务去重。COMMIT/ACK 交叉已拆为本项和 COMMIT 结果未知/延迟回调，下一项为 P6.3.4.2.3.3.2.3.2，实施前继续细分。父项、默认 worker/all 与接收侧仍未收口。

## 注入与观察

`TestProductionSenderConsumerAckLossIntegration` 使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT/黑名单、默认 API 工厂、生产 PersistenceSupervisor/持久化处理、后台 Outbox、WebSocket 与 FileStore。

既有帧代理增加显式消费者模式。默认 publisher confirm 模式保持原行为，并以旧两组实际 confirm 丢失同步回归；消费者模式只丢一个客户端→broker 的 Basic.Ack，记录连接/channel/tag/multiple，broker→publisher 正确认不会被当作消费 ACK。publisher 本身直连，仍返回正常 202。

消费者模式还读取真实 Basic.Deliver、Content Header 的 body size 和完整 body frame，记录连接、channel、deliveryTag、redelivered 和原信封字节；按通道重组，单消息上限 1 MiB，观察队列有界。除了目标 ACK，帧按原字节转发；没有生成投递、修改 redelivered 或重发信封。

新增只读 broker 管理观察，明确要求 messages_ready/messages_unacknowledged/consumers 字段存在，避免缺字段被当作 0；认证只使用本轮管理账号，指标读取不取走或 ACK 消息。共享最终集合断言从普通消费 fixture 中提取，普通路径的关闭顺序和检查保持。

## 实际顺序

1. 真实消费者/Outbox 就绪后 arm。客户端仅实际 POST 一次，得到正常 202；生产仓储提交后发出的单条消费者 ACK 被帧代理丢弃。
2. 观察原 Basic.Deliver：redelivered=false，严格解码/验证原信封，匹配规范 command 和实际 HTTP 回执的 publishAttemptId。独立 SQL 读取确认原 persisted，Outbox 两条已完成。
3. broker 管理接口实际观察 ready=0、unacked=1、consumers=1；消费角色仍 Ready、Err=nil、仅一次投递/一次消费 ACK。成功写出 ACK 不证明 broker 收到，也不会自动让消费者发现这次帧丢失。
4. 客户端以一轮/一次可用提交预算 Recover，只 GET 原 persisted，保存完整终态；真实 WebSocket 结果幂等，关闭并重开磁盘保持原请求/身份/hash、attempts=1 和完整 SQL DTO。在这一阶段 broker 仍持有原未确认 delivery。
5. 测试随后只断开消费连接（保留代理 listener），明确触发连接故障。生产监督器等旧组实际 Close/WaitClosed 后建立新一代，broker 实际重投原消息。第二次观察 redelivered=true、新连接、非零 deliveryTag，完整信封与第一次逐字相同，包含同一发布 attempt。
6. 生产仓储按原键去重并发出第二个消费 ACK，这次原样转发，并核对写入后的连接/channel/tag 与重投一致；实际投递/消费 ACK 各两次、只丢一个。原 SQL 终态、两条完成 Outbox 的 eventId/payload 与客户端终态保持，不新分配消息或序号。
7. 已知终态再次 Recover 不发 HTTP，再次磁盘重开保持。监督器正常 drain/close 后所有会话组 closed、active=0/overlap=false；实际 QueueInspect 要求唯一消费者退出后原消息没有重新入队，再等最终管理 ready/unacked/consumers 全为 0。

活动连接期间管理指标可能仍保留 ACK 前的采样；仅 TCP 写成功也不足以证明 broker 已收到。本项以正确转发、唯一消费者完全退出后原队列无重入、最终管理计数归零交叉证明，未将活动连接阶段的采样值作为唯一完成信号。

ACK 丢失之后的连接中断是显式的第二个故障步骤，未声称单向 ACK 写出成功可以检测远端丢失。监督换代是真实生产实现，会话来源仍是调用生产 DialConsumer 的测试资源适配；不报告默认 application 资源来源/worker/all 已装配。

## 集合和边界

HTTP 恰为 **1 POST + 1 GET**，原 publisher 正确认没有丢失，也没有客户端重发。最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，一条完整磁盘终态、attempts=1。原 UUID 大写、目标前导零、Unicode/NUL/换行正文与纳秒时区时间保持，原 SQL 结果和迁移指纹保持。

测试不调用控制 Get/ACK、不直接 PersistCommand/RunBatch、不手工修改消息/事件状态。SQL/Redis/publisher/管理通路保持正常，没有停止 broker 进程或改共享开发数据。API 健康输入为夹具，worker 显式装配；磁盘重开为同一进程内 owner 关闭/重建。COMMIT 未知留下一项，隔离重放/发送侧矩阵收口、实际前端和接收侧 ACK/连续游标/重连仍待后续。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(ConsumerAckLoss|ConfirmLossRecovery)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

第一条同时回归 publisher 模式和提取后的最终集合断言；标签编译不代表执行其他故障 suite。实际 runId、计数、ACK 元数据与资源核验见[实施记录](message-reliability-mq-progress.md)。
