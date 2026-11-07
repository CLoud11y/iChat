# P6.3.4.2.3.1：SQL 查询连接中断与发送恢复

本项验证 API 自有 SQL 池的实际 TCP 通路中断时，生产状态接口返回 503，发送端不能据此重发或伪造结果；通路恢复后，同一 API 池查询原终态。P6.3.4.2.3 实施前已拆成 SQL、Redis/Outbox、MQ/消费角色、隔离重放与矩阵收口四项，P6.3.4.2.3.2 [Redis 通知故障恢复](client-sender-redis-recovery.md)已完成，P6.3.4.2.3.3.1 [消费连接监督恢复](client-sender-consumer-recovery.md)已完成，P6.3.4.2.3.3.2.1 [发布前断连恢复](client-sender-publisher-recovery.md)已完成，P6.3.4.2.3.3.2.2 [真实确认丢失恢复](client-sender-confirm-loss.md)已完成，P6.3.4.2.3.3.2.3.1 [消费 ACK 丢失恢复](client-sender-consumer-ack-loss.md)已完成，P6.3.4.2.3.3.2.3.2.1 [COMMIT 响应丢失恢复](client-sender-commit-loss.md)已完成，P6.3.4.2.3.3.2.3.2.2.1 [延迟 COMMIT 与优雅 drain](client-sender-commit-drain.md)已完成，P6.3.4.2.3.3.2.3.2.2.2 [强制关闭与迟到回调](client-sender-commit-forced-close.md)已完成，下一项 P6.3.4.2.3.4 隔离/重放与发送侧矩阵收口。完整发送矩阵、默认 worker/all 和接收侧仍未收口。

## 故障边界与装配

`TestProductionSenderSQLRecoveryIntegration` 分别从 accepted 和 publish_unknown 开始，每个子场景各用一套独立 runId 的 MySQL/Redis/RabbitMQ，真实 JWT/黑名单、默认 API 工厂、publisher confirm、生产 PersistenceService/后台 Outbox 和 FileStore。

共享环境初始化增加一个可选的 API 配置回调，默认调用路径保持原配置；本轮只将 API 的 MySQL 地址指向回环 TCP 中继。中继复用既有按原字节双向复制的故障夹具，不解释或生成 MySQL 包。显式 Ping 确认 API 池已有真实连接后，关闭中继现有上下游 socket，并拒绝新连接；直接调用原 API 池 Ping 必须失败，而独立 fixture/消费者池 Ping 仍成功。

HTTP 记录代理逐字转发生产 API 的实际回应，不注入 503 或 pending。WebSocket 提前关闭并确认个人 Redis 频道订阅数为 0，通知不能提前写入本地结果。本轮中断的是 API SQL 网络路径，MySQL 进程、消费者直连 SQL、Redis 和 RabbitMQ 保持可达；不将此项报告为数据库进程重启或事务提交未知验收。

## 两条初始状态

| 初始状态 | 实际产生方式 | SQL 故障期间 |
| --- | --- | --- |
| accepted | 真实 POST 获得 broker confirm 和完整 202，客户端保存 accepted | 不降级为 unknown、不把查询失败当 pending，不重发 |
| publish_unknown | 记录代理完整读取实际生产 202 后关闭下游 TCP，客户端保存 unknown | 保留原 unknown/请求和尝试次数，不根据失败查询重发 |

每条路径按以下顺序验证：

1. API SQL 中断前仅实际发布一次，保留原请求/键/hash、attempts=1、inFlight=false、Result=nil。
2. 切断 API SQL 后，以 MaxRounds=2、MaxSubmissions=1 运行有限 Recover。两次实际 GET 均为 503/code=-1/DEPENDENCY_UNAVAILABLE，没有 state、message、业务身份或 pollAfterMs；返回 ErrRecoveryExhausted/ErrHTTPUnavailable，本地记录逐字段保持。MQ ready=1、consumers=0，业务 command/message/Outbox 尚为空。
3. API SQL 仍中断时启动直连 SQL 的生产消费者和后台 Outbox，原命令正常提交、通知完成。再次运行两轮 Recover，实际仍是两个 503，不能从业务已提交或通知已完成推断本地已持久化，也不能重发。客户端记录仍保持最初 accepted/unknown。
4. 关闭并重开 FileStore/Sender/HTTPRunner，故障期间保存的记录保持。恢复 TCP 通路，等待原 API 池 Ping 成功，核对自有 SQL handle 未被替换，再以一轮/一次可用提交预算 Recover。只发出一次 GET，取得原完整 persisted，随后再次重开磁盘核对结果。

每组实际 HTTP 恰为 **1 POST + 4 GET 503 + 1 GET 200**，没有第二次 POST。退避用默认真实计时器；调用方十秒请求期限沿用受控夹具配置，生产 SQL/请求期限和模拟器默认值未改变。

## 最终集合与责任

每个独立场景最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，本地一条 persisted、attempts=1 的完整终态。原 UUID 大写、目标前导零、Unicode/NUL/换行正文和纳秒时区时间完整保留，规范身份/hash 及 SQL 终态逐字段匹配，迁移指纹不变。

生产消费连接 drain/close/WaitClosed、broker 消费者移除后，QueueInspect ready=0、consumers=0。Outbox 沿用 Claim 边界协调正常停止，不忽略未知错误；API 在中继之前关闭自身客户端，中继任务加入后再清理本轮基础设施。测试不领取 MQ delivery、不直接 PersistCommand/RunBatch/control ACK，不停止共享容器或改动开发数据。

API 健康输入仍是夹具，worker 服务由测试显式装配。Redis/Outbox 角色故障、MQ/消费监督恢复、隔离重放归后续三项；默认 worker/all、实际前端、跨进程强杀和接收消息集/ACK/自动重连仍未在本项收口。磁盘重开是在同一进程内关闭并重建 owner。

## 验证命令

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(SQLRecovery|Pipeline)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

第一条同时回归可选配置回调为空时的三组真实消费基线；标签编译不代表执行其他故障 suite。具体 runId、耗时与资源核验见[实施记录](message-reliability-mq-progress.md)。
