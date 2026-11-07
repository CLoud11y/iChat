# P6.3.4.2.3.3.2.2：真实 AMQP 确认丢失与 unknown 恢复

本项完成未消费/已落库两条实际 confirm 丢失路径，不额外细分。broker 实际产生正确认，但 publisher 没收到该帧，生产 API 返回带物理 attempt 的 publish_unknown；新 epoch 不隐式发布，客户端再查询恢复。后续 P6.3.4.2.3.3.2.3.1 已验证[消费 ACK 丢失恢复](client-sender-consumer-ack-loss.md)，P6.3.4.2.3.3.2.3.2.1 [COMMIT 响应丢失恢复](client-sender-commit-loss.md)已完成，下一项 P6.3.4.2.3.3.2.3.2.2 延迟回调与退出交叉，实施前按实际复杂度细分，父项与默认 worker/all 尚未收口。

## 精确帧故障

`TestProductionSenderConfirmLossRecoveryIntegration` 每个子场景使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT/黑名单、默认 API 工厂、publisher Supervisor、生产消费/后台 Outbox、WebSocket 与 FileStore。

只有 publisher AMQP URI 主机端口经过回环帧代理，凭据/vhost 保持；消费者、SQL、Redis 和控制通路不受影响。代理沿用已有 testenv session 夹具的 AMQP 0-9-1 帧布局，转发真实八字节协议头，按七字节帧头和声明长度读取完整帧，检查结束字节，单帧上限 1 MiB。此夹具只用于本地明文 AMQP，不是通用或 TLS 代理。

代理分别计数客户端→broker 的 Basic.Publish（class=60/method=40）和 broker→publisher 的 Basic.Ack（class=60/method=80）。初次 WaitReady 后才 arm，主动丢弃一个实际完整 Basic.Ack，记录实际连接序号、非零 channel/deliveryTag 和 multiple 标志；其他帧原样转发，不生成 ACK/NACK，也不主动断开连接来伪装确认。publisher 因真实等待期限耗尽判为 unknown，随后自行退休原 session。

HTTP 代理仅增加显式选项，允许记录实际 503/code=-1/publish_unknown/PUBLISH_UNKNOWN 和规范非空 publishAttemptId，其余默认规则保持。HTTP 头和正文实际传给客户端，不制造丢 HTTP 回应；原[发布前未开始](client-sender-publisher-recovery.md)场景同步回归，仍要求无 attempt 的 not_published。

## 两条路径

| 场景 | 实际顺序 | 结果 |
| --- | --- | --- |
| pending_query_authorizes_same_key_retry | 不启动消费者；第一条 publish 的真实 Basic.Ack 被丢弃，HTTP 503 unknown；等待新 epoch，确认 Basic.Publish 仍只有一次、队列 ready=1/consumers=0、业务表为空；重开磁盘，再 GET pending、按服务器下限等待、POST 原请求、GET persisted | 第二次物理 attempt 不同、两个 POST 字节完全相同。第二次真实 202 暂缓返回时，观察两次真实 publish/两次真实 ACK（其中仅一个被丢弃）、队列 ready=2，再启动生产消费/Outbox，最终幂等成一条消息/一个 seq/两条 Outbox，本地 attempts=2 |
| committed_query_prevents_retry | 发送前启动生产消费/Outbox；真实 ACK 被丢弃，命令仍正常提交/ACK/通知完成，而客户端收到实际 HTTP 503 unknown；等待新 epoch、重开磁盘，再 GET 原 persisted | 保留一次提交预算，但终态查询直接结束，无第二次 POST。帧计数始终一次 publish/一次 ACK（被丢弃），本地 attempts=1 |

两条路径均核对真实 503 原规范身份/hash 和物理 publishAttemptId。有效恢复前的磁盘记录是 publish_unknown、Result=nil、inFlight=false，不能由 broker ACK 已产生、SQL 已提交或通知已写入 socket 推断本地已保存。

新 epoch 的实际连接数至少为 2。帧级 publish 计数在重连后仍为 1，已提交路径最终也为 1，因此消费者去重不会掩盖 publisher 的隐式重放。未消费路径的显式重试后计数为 2，实际 ACK 也为 2，且只主动丢弃一个；队列/HTTP/SQL/磁盘证据相互核对。

## 状态、等待和资源

Recover 使用三轮、最多一次提交，默认真实退避计时器和服务器 pollAfterMs 下限；没有注入成功状态、替换 timer、修改 available_at 或生成新 ID。调用方十秒请求期限和受控第二次 HTTP 回执屏障沿用既有夹具；实际 publisher 等待期限及退休/新 epoch 由原生产配置控制。

查询终态后，实际 WebSocket 完整 command.result 迟到到达仍幂等；再次重开磁盘保存原 UUID 大写、目标前导零、Unicode/NUL/换行正文、纳秒时区时间、稳定 key/hash/commandId 和完整 SQL DTO。重开是在同一进程内关闭并重建 owner。

每个独立环境最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**。原 SQL 终态和 schema 指纹保持，生产消费连接加入、broker 移除消费者后队列 ready=0/consumers=0，所有资源仅属于本轮。API/publisher owner 先退出，再关闭代理 socket 并加入双向转发任务，最后回收环境。

测试不取 MQ delivery、不直接 PersistCommand/RunBatch/control ACK；API 健康输入仍为夹具、worker 显式装配。实际 broker 进程故障、在途消费 COMMIT/ACK 丢失、默认 worker/all、隔离/重放、接收消息集/业务 ACK/重连和实际前端仍留后续。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(ConfirmLossRecovery|PublisherRecovery)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

标签编译不代表执行其他故障 suite；具体 runId、时间、实际 ACK 元数据及资源核验见[实施记录](message-reliability-mq-progress.md)。
