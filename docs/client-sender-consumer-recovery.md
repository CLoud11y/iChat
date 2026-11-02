# P6.3.4.2.3.3.1：消费连接中断、监督换代与发送恢复

本项验证空闲持久化消费连接实际中断后，生产监督器先加入旧 owner，再创建新的独立会话；期间已由 broker 接受的命令留在原队列，客户端以原键查询恢复。P6.3.4.2.3.3 在实施前拆为本项与发布通路/确认未知/在途消费故障交叉两项，P6.3.4.2.3.3.2.1 [发布前断连恢复](client-sender-publisher-recovery.md)已完成，P6.3.4.2.3.3.2.2 [真实确认丢失恢复](client-sender-confirm-loss.md)已完成，下一项为 P6.3.4.2.3.3.2.3，开始前按实际复杂度继续细分。

## 装配与故障范围

`TestProductionSenderConsumerRecoveryIntegration` 分别从 accepted 和 publish_unknown 出发，每个子场景各用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT/黑名单、默认 API 工厂、publisher confirm、生产 PersistenceSupervisor、持久化处理及后台 Outbox、WebSocket 与 FileStore。

消费者会话来源是测试注入的资源适配，每次实际调用生产 DialConsumer，使用相同只读管理核验和 vhost 拓扑契约，提供一个独立连接、prefetch=1。生产 PersistenceSupervisor 负责所有重试和代际顺序；适配不创建处理循环、不直接处理 delivery。Close 调用实际 session.Close 和 WaitClosed，实际加入后才减少 active 计数并关闭资源组 closed；工厂入口和持有期间都检查旧组是否仍 active。

仅消费者 AMQP URI 的主机端口指向按原字节转发的回环 TCP 中继，用户/vhost 不变。初次真实连接就绪后，空闲时关闭中继现有 socket 并拒绝重连。API publisher、broker 进程、管理/控制连接、SQL、Redis 与 Outbox 通路保持正常。这里是空闲连接故障后处理已排队原 delivery，不是已领取消息的 broker redelivery 或在途 COMMIT/ACK 故障验收。

## 两条路径

1. 初始监督器 Ready，实际持有一个资源组。中断连接后，等待旧组 session Close/WaitClosed 完成，active=0、ConsumerReady=false，监督器 Err=nil，保持管理恢复状态。
2. 故障期间只实际 POST 一次。accepted 正常接收实际 202；unknown 由代理完整读取真实 202 后关闭下游 TCP。broker 确认仍有效，队列 ready=1、consumers=0，业务 command/message/Outbox 尚为空。
3. 客户端执行一轮/一次可用提交预算 Recover，只 GET 实际匹配的 pending，并因一轮耗尽返回 ErrRecoveryExhausted。accepted 保留已知接受；unknown 可保存有效 pending 查询证据，仍保留原请求/键/hash、attempts=1。这里没有宣称 pending 永不授权重发，只是有意在下一轮重发前结束这次有限调用。
4. 关闭并重开 FileStore/Sender/HTTPRunner，核对 pending checkpoint 逐字段保持。恢复 TCP 通路后，生产监督器自动重新调用来源建立新会话，Generation 增长，实际 session Done channel 不同；每轮来源在旧组加入后才被调用，active 始终最多 1。
5. 新消费者处理原队列命令，提交并 ACK；后台 Outbox 正常完成。客户端只再 GET 原 persisted，保存完整终态，随后实际生产 WebSocket 完整结果迟到到达时幂等。再次重开磁盘仍保留原请求/稳定身份/hash、attempts=1 和完整 SQL DTO。

每组 HTTP 恰为 **1 POST + 1 GET pending + 1 GET persisted**，不隐式生成新 ID 或重新发布。Generation 统计来源调用次数，包括连接失败，不要求与成功会话数相同；资源组记录只统计实际取得的会话。

## 时间与生命周期

本夹具为消费连接选用 connect/close/write/heartbeat=1s、operation=2s；监督 setup=5s、backoff=100—500ms，worker poll=100ms、SQL probe=1s/500ms、close=3s，其他处理/事务重试沿用默认。Outbox 沿用既有正常夹具的 poll=20ms 和 Claim 关闭边界。观察等待有界，资源 Close 仍必须等真实 join；以实际就绪/加入和队列观察判断完成，不以固定 sleep 代替结果，没有改变生产默认值。

消费组 Close 的观察期限不能替代实际 transport join，WaitClosed 等实际任务退出才返回。正常结束先 drain/close 监督器、等待其 Done，所有成功取得的组均 closed、active=0、overlap=false；再等待 broker 移除消费者，QueueInspect ready=0、consumers=0，确保原投递由生产路径 ACK。借入 SQL/Redis 不由会话组关闭。

## 集合与边界

每个独立环境最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，原 SQL 终态和迁移指纹保持。本地一条完整终态；UUID 大写、目标前导零、Unicode/NUL/换行正文和纳秒时区时间保持。测试不取 MQ delivery、不直接 PersistCommand/RunBatch/control ACK，不修改 schema 或共享开发数据。

生产监督与处理是真实实现，会话来源为测试适配，API 健康输入仍为夹具；本项不报告默认 application 角色资源来源/worker/all 装配完成，不启动隔离角色或实际前端。磁盘重开为同一进程内 owner 关闭/重建；发布确认未知、在途消费交叉归下一小点，隔离重放/发送侧矩阵收口归 P6.3.4.2.3.4，接收消息集/ACK/自动重连仍待后续。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSenderConsumerRecoveryIntegration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

标签编译不代表执行其他故障 suite。具体 runId、耗时和资源核验见[实施记录](message-reliability-mq-progress.md)。
