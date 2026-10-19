# 持久化数据库故障监督（P4.2.2.1）

`workers.PersistenceSupervisor` 在单次 `PersistenceService` 之外串行管理消费轮次。数据库连接故障后，旧服务停止领取，已准入事务/ACK 完整返回，旧订阅及整轮资源释放调用结束，再退避探测；存储恢复后才获取新消费者。原投递通过 broker 重投和 P2 幂等事务恢复，没有应用内 republish、Reject/requeue 或 ACK 重试。

本项提供独立、显式注入资源来源的监督器。`application.NewPersistenceFactory`、MQResources 和聚合健康仍使用原先单次服务及初始 session，尚未接入监督器；不能只替换工厂而保留旧就绪引用。P4.2.2.2 负责生产连接来源、拓扑/部分失败清理和运行时接线，P4.2.2.3 验证运行时交叉退出。默认 worker/all 缺失隔离与 Outbox 角色的门禁保持。

## 构造、状态与恢复

`NewPersistenceSupervisor(pool, factory, options)` 只校验和保存依赖，不联网、不迁移、不创建 goroutine。SQL pool 由外部持有到监督器 Done；所有轮次复用该 pool，监督器不关闭它。

Start 要求有 deadline 的 setup 和独立 lifetime，观察首次激活尝试。首次真实 SQL Ping 确认数据库后才调用资源 factory，并由新 PersistenceService 完成订阅及最终新鲜度 Ping。首次发生明确基础设施故障时，Start 返回 nil，监督器进入不可就绪的恢复循环；这只表示监督器已运行。setup 自身超时/取消及确定的启动错误仍返回 error。Start 返回后取消 setup 不影响恢复或已准入处理，每实例只能启动一次。

| 状态 | 行为/准入 |
| --- | --- |
| idle | 尚未 Start，无后台任务 |
| starting / probing | 首次/恢复探测与资源准备，不就绪 |
| ready | 当前子服务、lifetime、全部订阅与 SQL 新鲜度同时有效才就绪 |
| retiring | 子服务已停止领取，等待旧工作、订阅及整轮资源清理真实返回 |
| backoff | 不持有活动消费组；可取消的指数抖动等待后再做有界 SQL Ping |
| stopping | 已撤销准入，任何晚返回探测或 factory 不能重新激活 |
| stopped / failed | 正常退出/终止错误，不能重新 Start |

只有一个 owner 执行 probe、factory、启动、退役及恢复，轮次不重叠，不为探测或 SQL 另起可丢弃的 goroutine。数据库持续故障期间只进行串行、限时、带退避的 SQL 探测，不反复创建消费者。一次 Ping 成功不重置失败计数；一轮持续 ready 达到 ResetAfter 后，下次故障才重置。清理耗时不计入稳定 ready 时间。

Status/ConsumerReady 只读本地状态及当前子服务，均不做 I/O。Generation 统计资源 factory 调用次数，包括部分失败；SQL 探测不增加它。Failures 为连续失败计数，封顶 32；LastFailure 仅含安全类别。Err 返回终止/清理错误，已恢复的基础设施错误不使最终正常关闭失败。恢复期间的诊断使用 Status，不用 Done 判定单轮退役；Done 代表监督器全部自有回调已返回。

## 恢复门槛

恢复检查完整错误树，所有有效分支都必须是可识别的基础设施错误，并至少有一份连接故障证据。`storage` 探测自身的 DeadlineExceeded 可进入恢复；处理阶段取消/期限耗尽不凭超时推定数据库故障。

提交/回滚未知只有在其底层也完全是连接故障时才自动换轮，由新投递判定已有终态。单独的未知标记、混合技术/未知分支、畸形信封、终态校验失败、事务重试耗尽、ACK 失败和订阅终止均终止监督器。当前小项没有自动处理所有 AMQP 故障，也没有可靠隔离能力。数据库故障不被当成业务拒绝或技术坏消息，任何恢复分支都不授权 ACK/隔离。

## 每轮资源与退出责任

factory 返回 `PersistenceConsumerGroup`，必须提供新鲜、已核验拓扑的独立 session；获取部分资源后失败也要返回 group+error。`Consumers` 返回本轮输入，`Close` 负责所有 session，包括尚未打开订阅的连接。服务先 join 并关闭已打开的订阅，随后 group Close 补齐整轮清理，底层 session Close 须幂等。

group.Close 仅调用一次，必须等其自有释放工作结束后返回；这属于注入方契约，不能拿仅表示退役/停止准入的 session.Done 充当清理 join。group Close 返回错误或 panic 均使监督器终止，禁止继续换轮。现有 MQResources 的有界观察接口不能未经适配直接当成该契约，生产实现留给 P4.2.2.2。

StopAdmission 同步撤销就绪、取消 setup/等待/探测并停止子服务领取，保留已准入 work context。Drain 等当前激活回调与 SQL/ACK 工作实际返回，健康退出时保留订阅直到 Close。Close 取消剩余工作并观察最终清理。observer 期限耗尽返回 `ErrPersistenceSupervisorPending`，不会释放共享 SQL 或宣布 Done；忽略 context 的 factory、probe、SQL 和 closer 仍由原 owner 持有，真实返回后才完成退出。停止恰逢退役或 backoff 时也不会准备下一轮。

常用 error/String/GoString 格式只有固定阶段，不打印 SQL、凭证、原文或 panic 内容；底层原因仍可由 errors.Is/As 检查。

## 显式 Go 参数

| 参数 | 默认 | 范围 |
| --- | --- | --- |
| Worker | 原 PersistenceOptions 默认 | 沿用处理/探测/关闭和本地事务重试校验 |
| SetupTimeout | 30s | 正值，≤60s，每轮共用 |
| BaseDelay | 1s | ≥10ms，指数退避基数 |
| MaxDelay | 30s | ≥BaseDelay，≤60s |
| ResetAfter | 1min | 正值，≤24h |

第 n 次连续失败的等待上限为 min(BaseDelay×2^(n−1), MaxDelay)，实际等待在上限一半至上限之间随机选择。完整参数须显式传入，可从 DefaultPersistenceSupervisorOptions 调整；无新 YAML/env。子服务的每条处理仍独占原 ProcessingTimeout，不因换轮延长旧处理期限。

## 验证

```powershell
go test -mod=readonly -race ./workers -run '^TestPersistenceSupervisor' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceSupervisorIntegration$' -count=1 -v -timeout=9m
```

默认测试覆盖初始离线、连续退避/抖动及上限、完整错误树、旧工作及 group closer 的真实 join、graceful/forced 退出、迟到 probe/factory、部分资源失败、cleanup 错误/panic、并发关闭、不可重启及错误脱敏。

实际 Docker suite 使用独立 schema/vhost 与测试资源来源，核验归属后停启本轮 MySQL。测试 connector 仅为处理容器重启后可能变化的宿主端口而更新地址，监督器始终使用同一个 sql.DB；生产无此地址切换或故障开关。场景包括实际离线期间 MQ 接受并积压、恢复后两连接消费重复身份；实际 COMMIT 后 driver 返回 EOF、自动换轮重放唯一终态；实际 CHECK 3819 终止并保留原 attempt；初始停库期间关闭，数据库恢复后旧监督器不复活。COMMIT 结果故障是 SQL 成功后的 driver 注入，非 TCP 丢包。

每条命令核对唯一 message/终态/两个 Outbox，最终检查 broker 正数→零和会话 seq 连续性。没有运行默认 main、启用生产 worker/all 或冒充隔离/Outbox/完整客户端验收。
