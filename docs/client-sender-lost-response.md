# P6.3.4.2.2.1：接受回执丢失与原请求恢复

本项在[真实消费基线](client-sender-pipeline.md)上验证：HTTP 接受回执丢失后，磁盘重开仍保留原请求与 unknown；只有有效 pending 查询允许重发，已持久化的查询结果直接结束恢复。P6.3.4.2.2.2 已完成[查询/冲突/迟到结果交叉](client-sender-recovery-guards.md)，P6.3.4.2.2 范围收口。P6.3.4.2.3.1 [SQL 查询故障恢复](client-sender-sql-recovery.md)已完成，P6.3.4.2.3.2 [Redis 通知故障恢复](client-sender-redis-recovery.md)已完成，P6.3.4.2.3.3.1 [消费连接监督恢复](client-sender-consumer-recovery.md)已完成，P6.3.4.2.3.3.2.1 [发布前断连恢复](client-sender-publisher-recovery.md)已完成，P6.3.4.2.3.3.2.2 [真实确认丢失恢复](client-sender-confirm-loss.md)已完成，P6.3.4.2.3.3.2.3.1 [消费 ACK 丢失恢复](client-sender-consumer-ack-loss.md)已完成，P6.3.4.2.3.3.2.3.2.1 [COMMIT 响应丢失恢复](client-sender-commit-loss.md)已完成，下一项 P6.3.4.2.3.3.2.3.2.2 延迟回调与退出交叉，P6.3 整体仍未收口。

## 实际故障注入

`TestProductionSenderLostResponseRecoveryIntegration` 每个子场景使用独立 runId 的 MySQL/Redis/RabbitMQ、真实 JWT/黑名单、默认 API 工厂与真实 publisher confirm，复用生产 PersistenceService/后台 OutboxService 和 FileStore。

HTTP 代理实际转发到生产 API，完整读取并严格解码真实 202/code=0/accepted，核对物理 publishAttemptId 格式后，才通过 Hijack 关闭下游 TCP。调用方没有收到响应头或正文；代理没有生成 accepted、pending 或 persisted。客户端与代理的 POST 均禁用 GetBody 重放，代理不修改转发的成功响应内容。

开始发送前关闭 WebSocket，并通过 Redis PUBSUB NUMSUB 确认个人频道订阅为 0。恢复只能依赖磁盘与生产状态 HTTP，不能由通知提前补齐终态。丢失响应后，Submit 返回 ErrHTTPUnavailable，本地保存 publish_unknown、attempts=1、inFlight=false；随后关闭并重开 FileStore/Sender/HTTPRunner，继续使用原键、原 hash 和原请求。

## 两条恢复路径

| 场景 | 真实 HTTP 顺序 | 结果 |
| --- | --- | --- |
| pending_query_authorizes_original_resend | POST（真实 202 被丢弃）→ GET pending_or_unknown → POST accepted → GET persisted | 最初不启动消费者，确认队列保留第一次发布；有效查询并等待服务器建议下限后，两次 POST 字节完全相同，物理 attempt 不同；第二次 202 暂缓转发，确认队列 ready=2、consumers=0 后启动生产消费/Outbox；两个 delivery 最终只生成一个业务终态 |
| persisted_query_prevents_resend | POST（真实 202 被丢弃）→ GET persisted | 预先启动生产消费/Outbox，等待原命令落库且通知结算完成，再重开磁盘恢复；保留一次重发预算，但查询终态直接结束，不出现第二次 POST |

有限 Recover 使用 MaxRounds=3、MaxSubmissions=1，其余退避沿用默认值和真实计时器；未消费路径记录实际 pending 回应和第二次发布的时间，核对服务器 pollAfterMs 等待下限。此项没有替换计时器或伪造状态回应。为了容纳受控的第二次回执屏障，只有测试调用方的 RequestTimeout 设为 10 秒，生产 API/worker 超时及模拟器默认值未改变。

每条路径恢复后再关闭、重开真实磁盘，逐字段核对原始 UUID 大写、目标前导零、Unicode/NUL/换行正文、纳秒时区时间，以及规范 key/hash/commandId、完整 SQL 结果。前者保留 attempts=2，后者 attempts=1，均为 persisted、inFlight=false；不会另建 ID。

## 集合与边界

每个独立场景最终均为 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，本地磁盘一条完整终态。未消费路径两次真实 accepted 含不同的 publishAttemptId，逻辑身份/hash 相同；重复消费不额外分配消息、序号或 Outbox。

测试不领取 MQ delivery、不调用 PersistCommand/RunBatch/control ACK。最终 drain/close/join 唯一消费连接，等待 broker 移除消费者后以 QueueInspect 核对 ready=0、consumers=0，证明物理投递都已由生产消费路径结算；再次读取原 SQL 终态和迁移指纹确认保持。

共享正常路径夹具在 Outbox Claim 外包一层取消感知的互斥准入，实际领取和结算仍委托生产仓储。停止时先等待正在进行的 SQL Claim 返回，持有准入后再 Drain，阻止新的 SQL Claim 混入关闭；不修改领取结果、不忽略服务错误。取消进行中的领取可能报告提交/回滚未知，该独立故障窗口仍由既有 Outbox 故障 suite 和后续矩阵验收。

两个 worker 服务由测试显式装配，API 健康输入仍是夹具，未启用默认 worker/all 或监督恢复；磁盘重开是同一进程内 owner 关闭/重建。真实响应丢失发生在代理到客户端这一段，不声明 API/broker 崩溃、COMMIT 未知或任意网络故障均已覆盖。隔离/基础设施故障仍归 P6.3.4.2.3，接收消息集、ACK 和自动重连仍归 P6.4/P6.5。

## 验证命令

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(LostResponseRecovery|Pipeline)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

第一条同时回归使用共享磁盘初始化夹具的三组消费基线；标签编译不代表执行其他故障 suite。具体运行、时间和 runId 资源核验见[实施记录](message-reliability-mq-progress.md)。
