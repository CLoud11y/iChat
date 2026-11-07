# P6.3.4.2.3.3.2.1：发布前断连、未开始回执与原请求恢复

本项验证空闲 publisher 连接实际中断后，生产 API 返回真实 not_published，原记录可重开；生产监督器恢复新 epoch 时不自动发布，客户端再按原键/原内容查询并重试。P6.3.4.2.3.3.2 开始前已拆为本项、实际 AMQP 确认丢失/unknown、在途 COMMIT/ACK 故障三项，P6.3.4.2.3.3.2.2 [真实确认丢失恢复](client-sender-confirm-loss.md)已完成，P6.3.4.2.3.3.2.3.1 [消费 ACK 丢失恢复](client-sender-consumer-ack-loss.md)已完成，P6.3.4.2.3.3.2.3.2.1 [COMMIT 响应丢失恢复](client-sender-commit-loss.md)已完成，P6.3.4.2.3.3.2.3.2.2.1 [延迟 COMMIT 与优雅 drain](client-sender-commit-drain.md)已完成，P6.3.4.2.3.3.2.3.2.2.2 [强制关闭与迟到回调](client-sender-commit-forced-close.md)已完成，下一项 P6.3.4.2.3.4 隔离/重放与发送侧矩阵收口。完整发送矩阵与默认 worker/all 尚未收口。

## 故障边界

`TestProductionSenderPublisherRecoveryIntegration` 使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT/黑名单、默认 API 工厂、publisher Supervisor、生产消费/后台 Outbox、WebSocket 与 FileStore。

共享初始化增加一个可选 publisher 地址回调，默认传 nil，原 API 配置回调仍保持。只有 publisher AMQP URI 的主机端口指向原字节 TCP 中继，凭据和 vhost 保持；消费者、控制/管理、SQL、Redis 直连本轮服务。初次 WaitReady 得到非零 epoch 后，在没有业务发布时断开中继并拒绝重连，等待生产 supervisor 撤销 Ready，仍保持管理恢复状态。

HTTP 记录代理仅在显式选项下允许记录实际 503/not_published/code=-1/DEPENDENCY_UNAVAILABLE 且无 publishAttemptId 的回应，其余 POST 仍要求真实 202/accepted/规范 attempt；默认旧回执丢失测试不启用此选项。代理不生成成功或失败回执，不改正文或身份。

## 端到端顺序

1. 断连期间客户端实际 POST。生产 supervisor 没有可用会话，未分配物理发布 attempt；API 返回 503/not_published，携带原规范 commandId/clientMessageId/payloadHash、无 publishAttemptId。客户端保存 NotPublished/attempts=1/inFlight=false/Result=nil，真实队列及可靠性业务表为空。
2. 关闭并重开磁盘，NotPublished 保持。publisher 仍不可用时，以一轮/一次可用提交预算 Recover 只 GET 实际 200/pending；状态接口不按 MQ readiness 门禁。有效逻辑 pending 查询可将本地状态记为 publish_unknown 并保存查询证据，但不增加 attempts 或更改原请求。单轮结束于 ErrRecoveryExhausted，没有进入下一轮重发。
3. 再次重开磁盘，恢复 TCP 通路，生产 publisher 监督器自动建立新 epoch。客户端没有启动恢复任务前，再次核对队列/业务表仍为空、HTTP 记录未增加，证明连接恢复不隐式重放命令。
4. 启动真实消费/Outbox 后，客户端显式运行三轮/最多一次提交 Recover：先 GET 实际 pending，遵守服务器 pollAfterMs 真实等待下限，再 POST 原请求。代理完整读取实际 202 后暂缓转发，让测试核对真实 SQL 终态/Outbox 完成；放行后 Recover 再 GET 原 persisted。
5. 迟到完整 command.result 经实际 WebSocket 到达，幂等保留查询终态；再重开磁盘保留完整结果。两个 POST 请求字节完全相同，规范键/hash/commandId 不变、原 UUID 大写/目标前导零/Unicode/NUL/换行/纳秒时区时间保持。

实际 HTTP 恰为 **POST 503 → GET pending（MQ 离线）→ GET pending → POST 202 → GET persisted**。本地 attempts=2 表示两次 HTTP 提交，第一次无 MQ attempt，只有第二次产生真实已确认发布。没有以新 ID 绕过原键，也没有把连接恢复等同于客户端可无条件重发。

## 集合、关闭与边界

最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，一份完整 persisted 磁盘记录、attempts=2。原 SQL 终态和 schema 指纹保持，生产消费连接 drain/close/WaitClosed、broker 移除消费者后，QueueInspect ready=0、consumers=0；Outbox 正常关闭沿用 Claim 边界协调。所有资源仅属于本轮，未改共享开发数据或全局服务。

本项是在业务发布前切断空闲通路，并等待 supervisor 撤销就绪，因而得到 not_published，**不是实际 AMQP confirm 丢失**；后者归 P6.3.4.2.3.3.2.2，在途消费归 .2.3。API 健康输入仍是夹具，worker 显式装配，未声明默认 worker/all、隔离/重放、接收消息集/ACK/自动重连或实际前端完成。磁盘重开为同一进程内 owner 关闭/重建。

调用方请求期限沿用受控夹具的十秒，恢复使用默认真实计时器；publisher 保持既有测试参数（一个发布槽、operation=2s），未修改生产默认配置。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(PublisherRecovery|LostResponseRecovery)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

第一条同时回归共享初始化/HTTP 代理的旧两组真实回执丢失场景。标签编译不代表运行其他故障 suite；具体 runId、时间与资源核验见[实施记录](message-reliability-mq-progress.md)。
