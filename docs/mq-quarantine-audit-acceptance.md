# P4.2.4.4：隔离审计故障验收

P4.2.4 的输入解析、完整幂等审计事务、提交后 ACK、消费者恢复与角色工厂已完成。本项用实际故障验证责任边界，收口隔离审计 worker 的范围；P4 整体、CLI、Outbox、默认完整后台装配和客户端恢复仍待后续阶段。

## 验收路径与判定

`TestQuarantineAuditAcceptanceIntegration` 在随机项目、独立 MySQL/RabbitMQ vhost 中调用生产隔离工厂 → RunRuntime。双角色场景同时使用生产 `NewSupervisedPersistenceFactory`、`NewSupervisedQuarantineFactory` 和真实隔离 publisher，只有 Outbox 是明确的 fixture companion。其他隔离专属场景的持久化角色也是 companion，不把它们称为完整管线。

每项恢复后都重新读取全部审计，以 transport ID 集合核对已发布输入；解码完整审计文档，逐字节核对原文、精确稳定内容及传输身份，检查可选身份全 NULL、state=open、无 replay/resolved。坏原文包含无效 UTF-8/NUL，应用 headers 包含二进制、超过 JSON 精度范围的 int64 和 decimal，类型变化会影响比较。业务集合同时核对每个规范 command 恰好一条 message、一条 persisted command、两个 Outbox，以及全表总量，没有额外业务效果。

源/隔离队列必须同时无 ready/unacked 消息，broker ACK 统计须覆盖所有已确认发布的物理副本。目标发布计数来自真实 publisher 成功返回，允许至少一次恢复产生同身份物理副本；不能把 SQL 已有审计或本地 ACK 返回成功当成 broker 已确认。每项退出还直接 Get 两队列为空，等待连接回到三个 fixture owner；最后复核正式 schema 指纹和拓扑。

## 九项故障矩阵

| 场景 | 故障证据 | 恢复与责任判定 |
| --- | --- | --- |
| 实际审计 COMMIT 后返回错误 | driver 在真实 COMMIT 成功后返回 EOF，独立回调证明注入触发 | 监督器保留 commit unknown 分类、退役并更换连接；重投核验已有审计后 ACK，仅一行 |
| 实际 SQL COMMIT OK 响应丢失 | 专属 TCP 代理转发 COMMIT，完整收到真实 MySQL OK 后丢弃并断开连接 | 客户端得到真实网络未知结果；审计已提交，恢复到新代次并按原 transport ID 验证后 ACK |
| 第二轮消费者实际 ACK 丢帧 | 隔离 session 先替换到第二轮，专属 AMQP 代理丢弃真实 consumer ACK | 本地成功不能证明 broker 确认；正常退出后原 wrapper redelivered，显式恢复，首份完整审计不被覆盖 |
| 审计 COMMIT 前强杀进程 | 自建子进程在实际 INSERT/核验完成、COMMIT 发出前屏障；外部查询无已提交行 | 强杀该 PID，未提交事务回滚、原 wrapper 重投；新 worker 保存并 ACK |
| 审计 COMMIT 后 ACK 前强杀进程 | 子进程真实 COMMIT 成功后屏障；外部查询已有审计，handler 尚未返回 | 强杀该 PID，审计和原 wrapper 均保留；重投核验 existing 后 ACK，不覆盖首次观察 |
| 自动 DLX 先到，wrapper 后到 | fixture 从主队列 Reject 触发实际 broker DLX，先保存 broker_dlx，再发布两个主动 wrapper | 一个 transport ID、一份首次 DLX 观察，完整 x-death/实际路由保留，重复 wrapper 均可安全结束 |
| 同键不同内容的存储冲突 | 将另一合法源快照置于原 transport key 下，原文/受控字段仍自洽，模拟键与内容不一致 | 真实 worker 返回内容冲突并停止，无 ACK、无覆盖或自动恢复；恢复被故意损坏的 fixture 原文档后可核验 existing |
| 两个生产角色共同停库恢复 | 两角色均进入 backoff 后积压三条有效命令各两次、三种坏源各两次；离线阶段不增加代次 | 存储恢复后两个独立来源均换代，当前聚合就绪恢复；三份坏源审计、三个唯一业务结果和六个业务 Outbox，正常 drain |
| 两角色运行时 pending | 共用 SQL pool 只拦截审计 COMMIT 后返回，持久化角色继续完成有效命令；drain/close 各 100ms | RunRuntime 返回不代表隔离 Done；SQL pool 仍可用、publisher 外部 owner 仍存在；迟到审计不 ACK，join 后再恢复原 wrapper，业务与审计不重复 |

冲突场景不声称制造 SHA-256 碰撞，也不将覆盖/删除审计作为生产重放方案；夹具只恢复自己故意损坏前保存的文档。子进程配置只通过私有环境变量传入本轮凭证，不经过命令行参数或开发 .env/config；父进程只强杀自己启动并到达事务屏障的 PID，等待其真实退出。

## 网络故障与模拟边界

SQL TCP 代理仅用于固定测试驱动的未加密、未压缩 classic protocol，按四字节 packet header 转发，识别 COM_QUERY 的 COMMIT 和对应成功 OK。它不会记录认证/SQL payload，也不作为生产代理；不声称验证 TLS、压缩、多语句或 query attributes。协议依据 [MySQL packet framing](https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_packets.html)及 [COM_QUERY](https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query.html)。

第一项是 driver 返回结果注入，第二项是实际 SQL TCP 丢响应，第三项是实际 AMQP ACK 丢帧；三者分别报告。进程强杀是真实 OS 进程终止，屏障只用于确定故障窗口，不代替进程退出。最后一项的 COMMIT 后返回屏障只证明 pending 所有权，不冒充网络丢失。

fixture 的主队列 Reject 只用于触发 broker DLX；隔离 redelivery 检查后的 Nack 只将检查过的原文交回真实恢复 worker。这些操作不属于应用主动隔离策略，未以 fixture ACK 代替审计 SQL 的证明。

## 命令与结果

固定 Go 1.27.1、空 GOROOT、仓库 `.gocache`、不存在的 ICHAT_CONFIG_FILE，使用 RabbitMQ 4.3.6 / amqp091-go v1.15.0：

```powershell
go test -mod=readonly -race ./application ./workers -run '^Test(Quarantine|Persistence)' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestQuarantineAuditAcceptanceIntegration$' -count=1 -v -timeout=15m
```

默认增加两个来源同时 claim/分别退役、替换时聚合就绪与快照隔离的验证。真实 suite 的最后审计集合为 11 个 transport ID，业务集合为 4 个规范 command/message 和 8 个业务 Outbox。实际 runId、耗时、旧八项预运行和清理记录见[实施进度](message-reliability-mq-progress.md)；标签编译不等于执行所有集成 suite。

P4.2.4.1—.4 范围完成。尚未验收默认完整 worker/all、Outbox 发布/lease、受控 CLI、实际客户端、宿主信号编排、三节点多数故障、磁盘损坏或管理员删除数据。[P4.2.5.1 只读查询与选择](mq-quarantine-query.md)、[P4.2.5.2 确认式重发](mq-quarantine-replay.md)及[P4.2.5.3 终态核验](mq-quarantine-resolve.md)已完成；[P4.2.5.4 重放故障验收](mq-quarantine-replay-acceptance.md)已完成，P4.2.5 范围收口；下一项 **P5.1：Outbox worker**，实施前拆分；重发确认不是业务已修复，只有确认业务终态后才能 resolved。
