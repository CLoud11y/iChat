# P6.3.4.2.1：生产消费与发送恢复基线

本项将发送模拟器接到真实持久化消费服务和后台 Outbox，验证 HTTP 接受、SQL 业务终态、WebSocket 通知和本地磁盘之间的关系。P6.3.4.2 在实施前拆为消费基线、响应丢失/原键重发交叉、基础设施故障与隔离恢复三项；P6.3.4.2.2.1 [回执丢失恢复](client-sender-lost-response.md)已完成，P6.3.4.2.2.2 [查询/冲突交叉](client-sender-recovery-guards.md)已完成，P6.3.4.2.3.1 [SQL 查询故障恢复](client-sender-sql-recovery.md)已完成，P6.3.4.2.3.2 [Redis 通知故障恢复](client-sender-redis-recovery.md)已完成，P6.3.4.2.3.3.1 [消费连接监督恢复](client-sender-consumer-recovery.md)已完成，P6.3.4.2.3.3.2.1 [发布前断连恢复](client-sender-publisher-recovery.md)已完成，P6.3.4.2.3.3.2.2 [真实确认丢失恢复](client-sender-confirm-loss.md)已完成，下一项为 P6.3.4.2.3.3.2.3。P6.3.4/P6.3 整体仍未收口。

## 装配与资源

`TestProductionSenderPipelineIntegration` 与[通知交叉验收](client-sender-notifications.md)共用独立基础设施/API 初始化夹具，每个 suite 分配自己的 runId、MySQL schema、Redis 实例和 RabbitMQ vhost。仅验证归属后迁移、创建用户/好友/群关系；默认 API 工厂使用自身 SQL/Redis 客户端和真实 JWT/黑名单、publisher confirm 与 v1 WebSocket。

测试显式装配 `DialConsumer`、`NewPersistenceService`、`NewOutboxService`：一个真实独立消费连接、prefetch=1、生产严格解码/事务持久化/提交后手动 ACK，以及生产后台领取/Redis 发布/完成结算。Outbox 轮询间隔仅在本夹具设为 20ms，持久化空闲读取为 100ms；其他处理/租约期限沿用默认值。测试不读取 MQ delivery，不调用 PersistCommand、RunBatch 或 control ACK。

后续 P6.3.4.2.2.1 为共享正常路径夹具增加 Claim 外层准入：停止前等待实际 SQL 领取返回，再持有准入并 Drain，不在领取事务进行时取消。不生成或修改仓储结果，不忽略未知错误；进行中领取被取消的故障验收仍归 Outbox 专项及后续矩阵。

回执屏障复用既有夹具，只在实际 broker accepted 后延迟 Publish 返回，不伪造发布成功。API 的健康输入仍是夹具，两个 worker 服务由测试显式启动，未运行生产监督恢复、隔离角色或默认 worker/all 进程工厂。此项证明正常消费到发送端恢复链路，完整进程装配及故障窗口继续单独验收。

## 三组场景

| 场景 | 核对内容 |
| --- | --- |
| persisted_before_HTTP | 私聊经生产 consumer 提交，后台 Outbox 经 Redis/生产 WebSocket 返回完整 command.result；HTTP 仍在屏障内，本地先保存 SQL 规范终态；重复帧幂等，迟到 202 不覆盖终态 |
| rejected_before_HTTP | 非好友命令经生产 consumer 保存 NOT_FRIENDS；后台结果先于 202，本地保留 rejected 和稳定原因，无消息/序号分配 |
| offline_group_restart_query | 关闭当前 WebSocket，并确认发送者个人 Redis 频道订阅数为 0；群命令照常提交/通知完成/消费 ACK，客户端只知道 accepted；关闭并重开 FileStore/Sender/HTTPRunner 后，以一次只查询的 Recover 得到原 SQL 终态，再重开磁盘验证保存 |

每组原请求保留 UUID 大写、目标前导零、Unicode/NUL/换行正文和纳秒时区时间。每组终态均在磁盘重开后核对原请求、规范 key/hash/commandId、attempts=1、inFlight=false 及完整 SQL 结果。离线恢复只查询，不生成新 ID、不重发、不依赖 Redis 历史通知重放；这里的重开是同一测试进程中的 owner 关闭/重建，跨进程强杀证据仍见[P6.3.2](client-sender-file-store.md)。

## 最终集合和关闭

最终精确为 **3 command / 2 message / 2 conversation / 5 completed Outbox / 0 delivery cursor / 0 quarantine**，两条会话 last_seq 均为 1。逐条原 SQL 终态保持不变，三个本地磁盘各保留一条完整终态，迁移指纹复核通过。

测试先停止持久化准入并 drain/close/join 唯一消费者，drain/join Outbox，并等待 broker 移除消费者，再以不取消息的 QueueInspect 检查 ready=0、consumers=0。若 ACK 遗漏，关闭唯一连接后未确认 delivery 会重新入队，检查必须失败。最后关闭并等待 API；原 fixture SQL/Redis 仍可使用，验证借入资源未被服务提前关闭。清理使用独立期限，按本轮归属标签核验 Docker 资源。

客户端离线不阻塞 MQ ACK，Outbox completed 也不代表客户端已保存。接收消息集、连续游标、ACK 和自动重连仍归 P6.4/P6.5；未运行真实前端或吞吐/容量验收。

## 验证命令

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(CommandResult|Pipeline)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

固定工具链和运行/资源证据见[实施记录](message-reliability-mq-progress.md)。第一个命令同时回归四组显式仓储驱动的旧通知测试和三组后台消费测试，不能将标签编译通过当作执行其他故障 suite。


P6.3.4.2.3.2 为共享夹具增加可选通知 Redis 客户端/Outbox 参数；默认仍按原配置启动。正常和失败角色的 cleanup 各捕获其本身，预期错误只接受已核验并 Done 的确切旧角色错误，不会忽略新实例或默认路径的错误；真实替换范围见[Redis 通知故障恢复](client-sender-redis-recovery.md)。
