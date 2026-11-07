# P6.3.4.2.3.3.2.3.2.1：消费 COMMIT OK 丢响应与发送恢复

本项验证生产消费事务已提交、真实 COMMIT OK 未到达驱动时的恢复交叉：原 delivery 不 ACK，监督器按 commit_unknown 退休旧会话并请求新一代，broker 重投原信封，生产仓储幂等复用原终态；发送端只查询、不再次 POST。P6.3.4.2.3.3.2.3.2.2.1 [延迟 COMMIT 与优雅 drain](client-sender-commit-drain.md)已完成，下一项 P6.3.4.2.3.3.2.3.2.2.2 强制关闭后迟到回调与重投，实施前继续细分；父项、默认 worker/all、完整故障矩阵和接收侧仍未收口。

## 故障与证据

`TestProductionSenderCommitLossIntegration` 使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT/黑名单、生产 API/WebSocket、PersistenceSupervisor、后台 Outbox 与 FileStore。只有消费 SQL pool 经过测试专用明文 MySQL 包代理，API/Outbox SQL、publisher、Redis 和管理连接保持正常。

代理沿用现有 testenv SQL 故障测试的包布局，读取完整 4 字节包头和 payload，拒绝零长度或超过 1 MiB 的包；本夹具使用固定驱动、关闭 TLS/查询属性、显式开启参数插值。仅匹配 sequence=0 的完整 COM_QUERY `COMMIT`，arm 后第一条才设置响应屏障。只有实际服务器返回 sequence=1、OK 标记、零 affected rows/last insert id、清除 IN_TRANS 的响应才能进入屏障；独立 SQL 可见性再次核验提交已完成。没有生成 OK、调用仓储来替代消费者或包装驱动 Commit 返回值。

1. 原生产消费者就绪。HTTPRunner 实际 POST 一次并保存正常 202/accepted；AMQP 观察器记录第一次真实 Basic.Deliver：redelivered=false，完整信封严格解码后匹配原规范 command、HTTP receipt 身份和 publishAttemptId。
2. 代理截住服务器实际 COMMIT OK，不传给驱动。独立直连 SQL 看到 persisted 和两条已完成 Outbox；broker 管理实际 ready=0/unacked=1/consumers=1。原消费角色仍 Ready、只有一次投递、零消费 ACK，本地仍 accepted/attempts=1/无 Result。
3. 显式放行“丢弃响应”：该真实 OK 不被转发，关闭其 SQL socket，驱动实际收到 EOF。生产错误路径保留提交未知，不补偿删除、不 ACK；监督器退休并加入原会话，LastFailure=commit_unknown。
4. 只有生产监督器要求新一代时，测试工厂才进入可取消的资源获取屏障。此时真实旧组 closed、active=0/无重叠，broker QueueInspect ready=1/consumers=0，原投递仍有 broker 责任，消费 ACK 仍为零。这个屏障用于固定观察顺序，不伪造监督决策或新会话。
5. 客户端有限 Recover 只 GET 原 persisted，真实 WebSocket 结果幂等；完整 SQL DTO、原请求/身份/hash、attempts=1 和非 inFlight 终态保存。关闭并重开磁盘 owner 后完全相同。故障只在消费侧，客户端无需将 accepted 改为发布 unknown。
6. 解除资源屏障，生产监督器取得真实新连接。broker 实际 redelivered=true，新连接携带逐字相同的信封和原物理发布 attempt。生产仓储核对已有终态，以 ROLLBACK 释放 no-op 身份占用；不执行第二次 COMMIT，不新分配消息或序号。唯一消费 ACK 在此时完整转发，连接/channel/tag 与真实重投一致，AMQP 观察器本轮没有 arm 或丢弃 ACK。
7. 原 SQL 终态、两条 completed Outbox 的 eventId/payload 和磁盘记录保持；已知终态 Recover 不发 HTTP，再次磁盘重开保持。正常 drain/close 并加入全部会话后，实际 QueueInspect 无重新入队，最终管理 ready/unacked/consumers 均为零。借入的消费 SQL pool 在监督器退出后仍可 Ping。

活动消费连接期间的管理指标可能保留 ACK 前采样，参照[消费 ACK 丢失验证](client-sender-consumer-ack-loss.md)，本项用真实转发、唯一消费者完全退出后无重入队和最终管理全零交叉证明完成，不以 TCP 写成功单独判定。

## 最终集合与覆盖边界

精确计数为 **1 HTTP POST + 1 GET、attempts=1；1 消费 SQL COMMIT（OK 丢失）+ 1 重投 ROLLBACK；2 actual delivery / 1 consumer ACK（只在重投后）**。最终 **1 command / 1 message / 1 conversation（last_seq=1）/ 2 completed Outbox / 0 delivery cursor / 0 quarantine**，原 SQL 结果和迁移指纹保持。请求保留 UUID 大写、目标前导零、Unicode/NUL/换行和纳秒时区时间。

SQL 代理只有一个真实 COMMIT OK 丢响应，未停止 MySQL/broker，也没有操作共享开发数据或修改 schema/依赖/生产业务代码。API 健康输入仍为夹具，worker 显式装配；会话来源仍为测试调用生产 DialConsumer 的适配，不能报告默认 application worker/all 工厂已完成。磁盘重开是同一进程内 owner 关闭/重建。这里的短暂 SQL 响应屏障用于核对已提交状态；未覆盖取消/退出后的迟到回调、事务提交前断连、数据库或 broker 进程故障。

夹具显式设置 45 秒消费处理期限、45 秒替换 setup 期限和 30 秒 SQL readTimeout，容纳这里的管理指标观察屏障；不是修改生产默认处理期限。失败清理会先解除 SQL 响应屏障，再 drain/close 消费 owner；资源工厂屏障服从 setup context 取消。所有 pool、代理 listener/socket、双向转发任务与会话均在归属环境回收前退出。环境清理仅针对本轮 runId 的容器、卷和网络。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(CommitLoss|ConsumerAckLoss)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

组合命令回归 SQL 提交未知与消费 ACK 丢失的不同责任边界；标签编译不代表运行其他故障 suite。实际运行、早期失败（如有）及归属清理证据见[实施记录](message-reliability-mq-progress.md)。
