# 持久化连接替换与运行时接线（P4.2.2.2）

`application.NewSupervisedPersistenceFactory(pool, options)` 已将 P4.2.2.1 监督器接入生产 RunRuntime 的 MQ 资源来源和聚合健康。数据库故障或明确消费连接退役后，停止领取、完成旧工作及资源清理、带抖动退避，再获取完整的新连接组。就绪只观察当前轮次，初始 session 的 Done 关闭不会永久阻止恢复。

这是可显式注册的生产工厂，默认进程尚未注册完整后台角色。隔离审计与 Outbox 未实现，worker/all 仍在外部 I/O 前拒绝缺失角色；原 NewPersistenceFactory 的单轮行为保留。P4.2.2.3 将继续验证恢复过程与运行时 drain/强制退出/共享存储释放的交叉故障。

## 从准备到每轮借用

PrepareMQ 仍按原计划创建并核验所有初始连接，不订阅、不迁移。初始 broker 不可用仍使准备失败，不把这次接线改成离线启动策略。

准备成功后，资源组保存只读配置、受控消费者 dial 函数及 `PersistenceConsumerSource`。只有持久化角色的 RoleDependencies.PersistenceConsumers 获得该来源；API、隔离、Outbox 都没有这项能力。来源的构造与 claim 均无 I/O、无后台任务。

新监督工厂校验共享 pool、监督参数、角色及初始连接快照，然后只允许一个监督器 claim 来源。claim 在资源组锁下将初始持久化 session 的清理责任换成来源 owner；publisher/隔离的清理条目保持。重复 claim、错误队列/prefetch/连接、缺少真实 join 能力及已经停止的来源均拒绝。

首次 SQL Ping 健康后，监督器向来源借用初始组；数据库一直离线时不反复获取新连接，尚未借出的初始连接仍由 MQ owner 持有并在退出时清理。借用前再次对每个初始 session 做 Verify，避免使用等待存储期间过时的拓扑证据。

后续轮次按同一经过校验的 ConsumerConfig 为每个 worker 新建独立 TCP/AMQP session，再逐个只读 Verify。全部拨号、验证共用同一 setup deadline，不按 worker 重新计时；受 MQOptions.SetupTimeout、监督器 SetupTimeout 和更早的调用期限共同限制。新 PersistenceService 随后打开全部订阅，并以最后的真实 SQL Ping 建立自身新鲜度。

任一拨号/验证失败都返回 group+error，保留已经获得的所有 handle，包括 handle+error。nil/typed nil、重复连接、缺少 join 的异常能力拒绝。没有发布业务命令、领取消息、修复绑定或创建替代拓扑。

## 清理与实际 join

`ConsumerSession.Done` 只表示退役。新增 `WaitClosed(ctx)` 在 Close 后等待 close owner 完成，再观察最终的传输调用 idle 状态；不会用较早的 idle 快照忽略稍后准入的调用。它只观察、不启动关闭，也不把 observer 超时当成退出。

| 层次 | 责任 |
| --- | --- |
| 单轮 PersistenceService | 停止领取，等待已准入 SQL/ACK 和 probe，再关闭已打开订阅 |
| persistence group Close | 一次启动并发 session Close，之后逐项 WaitClosed；包括未订阅/部分获取连接；全部真实返回才返回 |
| consumer source Close | 一次清理 owner，取消并等待正在进行的获取，清理未借出或活动组；调用者只做有界观察 |
| MQResources Close | 停止整个 MQ 资源组，观察来源 owner 与独立 publisher/隔离的关闭；可再次观察 pending |
| SQL pool owner | 监督器 Done 后才可释放共享 pool，不能以 MQ 或某次 Close observer 返回代替业务任务 join |

group Close 与来源 Close 汇合到同一个 group owner；并发重复调用不重复关闭持久化 session。Group 关闭错误或 panic 仍尝试执行 join，记录安全错误，并禁止该来源创建新轮次。旧清理错误不会因活动组已移除而在最终 MQ Close 中消失。

来源 owner 在 observer 超时后继续持有迟到 factory、partial handle 与仍未 join 的传输工作。迟到成功不能发布为活动组，也不能越过停止边界变为 ready。来源只保留未借出/当前组，不积累历史连接清理列表。

## 连接失败与恢复范围

`workers.NewPersistenceConnectionError(cause)` 标记受控连接准备阶段的失败；仅来源的连接/拓扑操作使用这一边界，此阶段尚未准入 delivery。连接或拓扑暂时不可用时，旧部分组先清理，随后按原监督策略退避再验证；缺失绑定保持缺失，直到外部修复。

监督器也识别明确的订阅退役/接收 unavailable，以及所有订阅成功前的已知 transport/setup 超时。订阅 busy/config 错误、畸形 delivery、约束/终态错误、事务重试耗尽、处理取消和 ACK 失败仍不因连接标记而自动换轮；错误树中的这些并列分支会否决恢复。初次 setup 总期限耗尽仍返回启动失败。停止造成的纯连接准备取消不会被当成新的运行故障。

新连接类别不授权 ACK、Reject、重新发布或隔离，不改写 publishAttempt。旧未确认投递交由 broker 重投，幂等仓储判定已提交终态。此项没有实现消费者空闲时的周期拓扑巡检；只在准备/替换时复核。

## 聚合健康与快照

未 claim 的资源组及旧单轮工厂保留原消费者快照语义。claim 后，Consumers 返回当前已完整准备的持久化连接加原隔离连接的副本；暂停、获取部分完成、关闭期间不暴露持久化活动能力，返回顺序不是角色契约。

runtimeHealth 同时检查资源来源是否完整可用、当前 session 的 Done、具体角色 ConsumerReady 与 runtime 停止标志。就绪读取不执行 Verify、Dial、SQL 或 ACK。初始 RoleDependencies.Consumers 保留构造时的副本，不能拿它监测后续轮次；聚合器改为从资源 owner 读取当前快照。

来源完整准备只证明连接可用，不替代子服务的订阅/存储新鲜度。进程停止和 MQ 停止仍永久压倒依赖恢复；publisher/鉴权的接受就绪计算保持独立。

## 验证

```powershell
go test -mod=readonly -race ./application ./workers ./messaging/rabbitmq -run 'Test(PersistenceConsumerSource|SupervisedPersistence|Persistence(Connection|Supervisor)|ConsumerWaitClosed)' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistence(Connections|Lifecycle)Integration$' -count=1 -v -timeout=12m
```

默认测试验证纯 claim、单一借用、角色边界、旧/新快照与聚合就绪、部分失败和共同 deadline、typed nil/重复连接、晚返回、join 超过 observer 期限、清理错误保留、连接阶段取消、恢复错误树和缺失角色门禁。

新实际 suite 运行生产 NewSupervisedPersistenceFactory → RunRuntime：

1. 核验归属后停启本轮 MySQL，MQ 接受三个身份各两个物理发布；暂停时 runtime 不退出也不新建消费者。恢复后新组处理积压，初始 session 全部退役，聚合健康重新 ready，隔离连接保持原实例。
2. 空闲时显式退役一个初始消费连接，持久化组整体 join 后重建，旧 Done 保持关闭，新的聚合健康及业务处理恢复。
3. 数据库暂停期间，积压一条 accepted 命令并移除本轮主队列绑定。恢复 SQL 后多轮新连接拒绝该拓扑，不修复、不消费积压、不终止 runtime；手工恢复本轮绑定后继续处理。
4. 专属 loopback 代理允许新一轮首个连接，黑洞第二个实际 AMQP 握手。先观察真实部分连接，再确认下一次尝试前回到仅隔离连接的基线；解除故障后恢复并处理新命令。

每个场景退出后直接 RPC 检查队列为空，并等待本轮 vhost 的连接回到 fixture 控制/publisher 基线；最终核对唯一 message/终态/两个事件和连续 seq。SQL connector 的动态地址仅处理测试容器重启后的随机宿主端口，非生产数据库切换机制。隔离/Outbox 是明确的测试占位角色，不代表审计/通知实现。
