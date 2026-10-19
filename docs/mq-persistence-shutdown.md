# 监督恢复的运行时退出验收（P4.2.2.3）

本项验证生产 NewSupervisedPersistenceFactory → RunRuntime 在恢复过程中停止时的责任边界。持久化监督的 P4.2.2 范围完成；默认进程仍未注册完整后台角色，隔离/Outbox 门禁保持。下一项 P4.2.3 为可靠隔离转移，不把本轮测试中的占位角色当成隔离或通知实现。

## 两种完成通知

| 观察 | 可以据此判断 | SQL pool 责任 |
| --- | --- | --- |
| RunRuntime 返回 | 执行器完成了有界停止/drain/Close/MQ Close 观察，并报告各阶段结果 | 返回 pending/超时不表示业务任务退出 |
| supervisor.Done 关闭 | 该监督器的激活回调、子服务 SQL/ACK/probe、每轮资源 Close/join 已全部返回，不会再获取下一轮 | 显式外部 owner 才能最终释放其共享 pool |
| source/group/session 退役或关闭 | 对应 MQ 获取/清理或传输调用退出 | 不能替代 SQL/ACK 所属监督器的 Done |

顺序仍为：撤销聚合健康与业务准入 → 保留已准入处理的 work context 并 drain → 取消剩余工作 → 有界 Close → MQ 清理。SQL pool 属于调用方，不由持久化服务或 MQ owner 关闭。

RunRuntime 报告 ErrRuntimeShutdown、ErrPersistenceSupervisorPending 及 deadline 时，应保留监督器和 pool，继续等待同一 Done。不要因为任务状态是 stopping、MQ 已断开或 Close observer 已返回，就宣布共享存储可释放。若回调忽略取消，owner 持续持有它，不能承诺硬退出上限；晚返回成功也不能恢复就绪或启动新轮次。

正常 drain 中，已经提交但回调尚未返回的事务仍有机会完成原单条 ACK。强制取消后若 COMMIT 已实际成功，原消息仍可能重投；新处理器以原业务身份重放规范终态，不重复分配 seq、写 message 或 Outbox。ACK 本地写成功也不证明 broker 已收到，退出后的原 attempt 重投仍须走幂等仓储。

## 修正的启动取消边界

验收复现了恢复过程中尚在打开订阅时的正常停止误报：PersistenceService 以 Start/subscription 包装 context.Canceled，监督器此前只将 storage/连接来源阶段的纯取消当作正常停止，最终 Close 因此错误报告运行故障。

现在只在外层已经进入停止路径时，对尚未准入 delivery 的 Start/context、Start/subscription、Start/storage 纯取消做正常结束判定。完整错误树中的其他故障仍保留；处理、ACK、清理失败及 Close 超时不被吞掉。恢复政策、提交语义、事务预算、ACK 次数及默认装配均未放宽。

## 默认确定性验收

`application/persistence_shutdown_test.go` 使用真实 executor、生产连接来源及监督器，仅 SQL/AMQP 边缘和缺失的 companion 角色为本地夹具：

- 初始数据库不可用，runtime 已激活但不就绪；恢复后进入订阅 setup，此时停止必须正常退出，禁止新轮次。
- 恢复 Ping 忽略取消并晚返回成功，RunRuntime 有界返回 pending，而监督器 Done 及 SQL lease 继续保留。解除屏障后才能由外部 owner 关闭 pool，Generation 始终为 0。
- 旧组 WaitClosed 被屏障阻塞，停止和 MQ Close observer 到期仍保留同一 group owner；解除后每个旧 handle 只 Close 一次，未开始下一轮。
- 新一轮部分连接已获得，后续 dial 忽略取消并迟到返回 handle；runtime observer 返回后仍持有该获取责任，返回后补齐 Close/join，不发布为活动组、不复活。

`workers/persistence_shutdown_test.go` 独立核对取消包装的责任阶段，纯启动取消可结束，混合 EOF、处理/ACK/清理错误与未知原因不可借机被忽略。

## 实际 MySQL/RabbitMQ 验收

`TestPersistenceShutdownIntegration` 在独立 schema/vhost 上运行生产工厂和 RunRuntime，共六个场景：

| 场景 | 控制点与实际证据 |
| --- | --- |
| 已替换连接组后 graceful 退出 | 真实 COMMIT 成功后的 driver 屏障；停止后 work context 仍有效，短 Drain observer 为 pending；解除后原 ACK 完成，退出队列为空 |
| 已提交回调不响应强制取消 | RunRuntime 预算到期先返回；监督器未 Done、SQL InUse>0 且共享 pool 仍可 Ping；解除后原 attempt redelivered，重放唯一终态；最后由显式 owner Close pool |
| 新轮次后续握手黑洞中停止 | 专属代理放通首个新连接并黑洞第二个实际 AMQP 握手；取消后部分组完整清理，runtime 正常结束，解除代理故障也不重连；accepted 原 attempt 保持首次未领取状态 |
| 真实 Ping 后的晚回调 | 真实 MySQL Ping 成功后 driver 屏障忽略取消；runtime 返回 pending，SQL lease 保留；解除后 supervisor Done，聚合健康保持 stopping，无下一轮 |
| 实际 ACK 丢帧后正常退出 | 代理丢弃消费 ACK，业务已提交且本地处理成功；运行时正常退出后原 attempt 重投，仓储 replay，唯一 message/终态/两个事件不增加 |
| 数据库持续故障的 backoff 中停止 | 仅停启核验归属的本轮 MySQL；backoff 时退出，MQ accepted 积压保留；SQL 恢复后旧实例不复活，显式新处理器处理同一首次投递 |

COMMIT/Ping 屏障是实际 SQL 成功后的测试 driver 返回屏障，不是 TCP SQL 响应丢包；ACK 丢帧和 AMQP 握手黑洞使用真实 loopback 网络代理。没有新增生产故障控制接口，没有运行默认 main 或发送宿主进程信号，也不把这些证据称为完整客户端端到端验收。

所有场景结束时直接 RPC 检查主队列为空，并等待连接回到本轮两个 fixture 控制/publisher owner 基线。按每条命令核对唯一 message、规范命令终态和两个 Outbox；最终核对每会话消息数、最大 seq 与 last_seq 一致。SQL pool 的地址调整只用于 Docker 重启后的随机端口，不是生产数据库切换实现。

```powershell
go test -mod=readonly -race ./application ./workers -run '^TestPersistenceShutdown' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceShutdownIntegration$' -count=1 -v -timeout=10m
```

P4.2.3 继续实现“隔离目标发布 confirm 且无 return 后才 ACK 原 delivery”的可靠转移；隔离记录 worker、受控重放、Outbox 和完整客户端恢复仍按后续阶段验收。
