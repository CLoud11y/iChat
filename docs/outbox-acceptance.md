# P5.1.5：Outbox 故障验收与 P5.1 收口

`TestOutboxAcceptanceIntegration` 在同一轮独立 MySQL/Redis/RabbitMQ 环境中，验证[领取](outbox-claim.md)、[结算](outbox-settlement.md)、[通知循环](outbox-execution.md)和[角色生命周期](outbox-lifecycle.md)的故障交接。使用生产仓储、Redis 发布器与三个真实 worker 角色，不用 companion 服务补齐本 suite 的角色清单。

## 九项验收

| 场景 | 故障及实际证据 | 恢复门禁 |
| --- | --- | --- |
| Claim COMMIT 响应丢失 | SQL 代理完整读取真实 COMMIT OK 后关闭连接；SQL 两行已 leased，而调用者无领取回执、无通知 | 等数据库真实 lease 到期，再领取/通知；不使用不确定回执 |
| Complete COMMIT 响应丢失 | 真实发布成功，代理丢弃完成 COMMIT OK；SQL 已 completed，但循环不报告完成 | 后续轮询不再领取该 completed eventId，不补偿重发 |
| 发布与 Retry COMMIT 响应同时丢失 | Redis 代理收到真实 PUBLISH integer reply 后丢弃；SQL 代理再丢弃实际回退 COMMIT OK | SQL pending/NOTIFICATION_UNKNOWN/attempts=1 保留，按 available_at 重领原 eventId |
| 仅 Redis PUBLISH 响应丢失 | 独立订阅者实际收到通知，发布调用报错；回退成功 | 重领后再次收到相同 eventId，业务与原终态不重复 |
| lease 到期与迟到成功 | 真实发布后阻塞返回回调，等待数据库时间越过原 lease；新 owner 换 token/attempt 并完成 | 释放旧 nil 回调后不结算；旧 Complete/Retry 均 LeaseLost，不能覆盖新状态 |
| 领取提交后、通知前强杀进程 | 子进程运行真实 OutboxService，Claim 提交返回屏障；父进程强杀并确认其 SQL connection ID 消失 | SQL lease 保留，通知数为 0；实际到期后新 owner 完成 |
| 通知后、标完成前强杀进程 | 子进程实际 PUBLISH 成功后屏障，尚未 Complete；父进程强杀 | 已观察通知仍在，lease 到期后相同 eventId 再发，业务唯一 |
| 三角色 pending 退出与新实例接管 | 实际持久化、隔离审计和 Outbox 同时运行；Outbox 的真实 Complete 已提交但返回回调阻塞；其他角色完成原队列 ACK | 短 drain/close 返回 pending，健康 stopping，原 owner 仍保留共享 SQL；释放回调后 Done，新实例恢复剩余事件并消费后续命令 |
| 持久化提交未知与在线 Outbox 交叉 | 仅持久化角色的 SQL 代理丢失真实业务 COMMIT OK，规范消息/Outbox 已提交 | 持久化监督器退役/重投/ACK，在线 Outbox 可处理已提交事件；终态、seq、原 payload 集合均保持唯一 |

四次 SQL COMMIT OK 和两次 Redis PUBLISH reply 均由本轮 TCP 代理真实消费后丢弃，不以手工返回 EOF 替代。故障日志中的 MySQL unexpected EOF 是预期注入证据。MySQL 代理只支持固定 fixture 使用的 classic protocol，不启用 TLS、压缩或 query attributes；Redis 代理只用于有界 RESP2 命令/响应，订阅者直接连接真实 Redis。

迟到成功场景保留的是实际网络调用完成后的返回回调；不是故意绕过生产 Redis client 的 deadline。旧 callback、当前 token 与 lease 的三种边界分别核对。没有手工修改 lease_until 来冒充真实过期。

子进程是同一测试可执行文件的私有 helper，配置只经子进程环境传入，凭证不进入命令参数或日志。helper 校验本轮 schema 身份，在提交/通知后的精确屏障报告无敏感信息的 eventId/connection ID；父进程 Kill 并等待该进程退出、对应 SQL 会话消失，再恢复。

## 集合、ACK 与权限边界

前七项通过生产 PersistCommand 直接创建规范事件；后两项通过真实 MQ 接受/持久化监督器创建业务终态，并提交一条技术坏消息到隔离审计。明确区分直接仓储夹具与实际 MQ 投递，不将 SQL 行存在当作 broker 已 ACK。

三角色场景使用生产 NewSupervisedPersistenceFactory、NewSupervisedQuarantineFactory、NewOutboxSourceFactory；持久化的隔离目标有独立真实确认式 publisher，Outbox 存储/Redis 探针真实执行。观测管理 API 的两队列 ready/unacked 均为 0，主队列实际 ACK 至少 4 次、隔离队列至少 1 次，才视为责任交接结束。

最终逐一核对：

- **10 command / 10 message / 1 conversation / 20 Outbox**；全部 Outbox completed，每个 command 恰有两种事件，20 个唯一 eventId。由于至少一次恢复，实际通知允许重复，至少 24 次观察；存储业务与通知身份去重集合不扩大。
- 原消息内容/媒体/路由/seq/服务端时间、命令 hash/终态/首次创建和完成时间，以及 Outbox 身份/原 JSON/路由/创建时间在恢复前后完全一致。conversation.last_seq 等于消息条数与最大 seq，无缺号。
- **1 quarantine / 0 delivery cursor**。审计原始 body、messageId、correlationId、transport identity 和 open 状态经完整审计解码核验；不因通知成功推进 delivered。
- 所有通知均无 content/media，command.result 指向发送者、message.persisted 指向接收者，重复 eventId 的频道一致。没有将订阅/发布成功当成终端当前权限或交付证明。
- 迁移指纹及 RabbitMQ 只读拓扑复核不变；测试资源仅清理本轮所有权标签。

## 运行

```powershell
$env:GOROOT = ''
$env:GOTOOLCHAIN = 'go1.27.1'
$env:GOMAXPROCS = '2'
$env:GOCACHE = Join-Path (Get-Location) '.gocache'
$env:ICHAT_CONFIG_FILE = Join-Path (Get-Location) 'workers/.unit-config-not-present.yml'
go test -mod=readonly -p=2 -race -tags=integration ./tests/testenv -run '^TestOutboxAcceptanceIntegration$' -count=1 -v -timeout=10m
```

固定隔离配置，不读取开发 .env/config。`-run` 只选主验收入口，私有 crash helper 由父进程单独启动。具体 runId、默认回归、真实结果和清理记录见[实施进度](message-reliability-mq-progress.md)。

P5.1.1—.5 的 Outbox worker 范围收口，并补齐 P4 显式 worker 三角色的优雅退出/提交未知交叉验收。默认 worker/all 仍缺 Persistence/Quarantine 工厂注册，保持 I/O 前门禁；没有验收默认完整进程、API+三个 worker 的 all 模式交叉、三节点多数故障、磁盘损坏、管理员删数据或实际客户端交付。

下一项 **P5.2：统一 message/command.result WebSocket 帧与通知权限核验**。P5 整体保持未完成。
