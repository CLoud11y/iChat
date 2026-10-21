# 独立测试环境

P4.2.3.4 可靠隔离转移的运行时交叉故障：

```powershell
go test -mod=readonly -race ./workers -run '^TestPersistenceQuarantine' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestQuarantineRuntimeIntegration$' -count=1 -v -timeout=12m
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistence(Connections|Shutdown)Integration$' -count=1 -v -timeout=12m
```

新 suite 的九项覆盖第二轮源组正常 drain、runtime pending 后借入目标/SQL owner、目标 confirm 丢帧/Return/NACK/断线、目标确认后源退役、真实源 ACK 丢帧与自动 DLX 身份。目标返回屏障和实际网络丢帧分别报告；队列 ACK 仅用于 fixture 检查/清理，不代表 SQL 审计。原连接/退出两套 suite 回归保持旧夹具语义，完整范围见 [转移验收](../docs/mq-quarantine-acceptance.md)。

P4.2.3.3 消费处置与可靠隔离接线：

```powershell
go test -mod=readonly -race ./workers ./application -run '^Test(PersistenceDisposition|PersistenceFactory|PersistenceSupervisor)' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceDispositionIntegration$' -count=1 -v -timeout=10m
```

默认回归覆盖明确失败白名单、目标确认前零 ACK、无效回执、取消/ACK 失败及 pending 下的 publisher 所有权。真实六场景覆盖坏信封后继续处理、真实 SQL 约束回滚、目标 Return、实际 confirm/源 ACK 丢帧和提交未知不隔离。隔离队列的 fixture ACK 不代表审计 SQL，完整运行时交叉故障归下一小点；详见 [消费处置](../docs/mq-persistence-disposition.md)。

P4.2.3.2 隔离目标可靠发布及共用内核回归：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^Test(Publisher|Quarantine(Publisher|Session))' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session|QuarantinePublisher)Integration$' -count=1 -v -timeout=12m
```

新 suite 定义并发稳定快照、独立 API 连接、目标 Return+ACK、容量 NACK、确认丢帧与显式重复发布验证；原 publisher/session 真实 suite 一同回归。实际结果见 [隔离发布](../docs/mq-quarantine-publisher.md)及[进度记录](../docs/message-reliability-mq-progress.md)。fixture 的队列 ACK 只用于检查/清理，源投递处置和 SQL 审计仍归后续子项。

P4.2.3.1 隔离信封与稳定传输身份（纯函数，无外部依赖）：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestQuarantine' -count=30 -timeout=90s
go test -mod=readonly ./messaging/rabbitmq -run '^$' -fuzz '^FuzzDecodeQuarantineEnvelope$' -fuzztime=15s -parallel=2
```

覆盖原始坏字节、带类型 headers、快照所有权、稳定重投/DLX 身份、篡改和资源边界。当前 DLX 为数据模型验收，没有运行真实隔离发布或审计写库；协议和 P4.2.3 后续拆分见 [隔离信封](../docs/mq-quarantine-envelope.md)。

P3 的阶段要求、五项接受验收条件、故障矩阵及集中重跑命令见 [MQ 与接受入口验收对照](../docs/mq-reliability-acceptance.md)。集中回归包含 13 个 MQ/API suite；fixture 手动 ACK 不代表业务事务提交后 ACK，后者仍归 P4。

P4.2.2.3 监督恢复的运行时退出验收：

```powershell
go test -mod=readonly -race ./application ./workers -run '^TestPersistenceShutdown' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceShutdownIntegration$' -count=1 -v -timeout=10m
```

默认测试执行真实 executor/source/supervisor，覆盖订阅 setup 取消、晚 probe/dial、group join 与 runtime pending；实际六场景覆盖已替换组的 COMMIT/ACK drain、已提交回调及真实 Ping 返回屏障下的强制退出、部分 AMQP 握手中停止、实际 ACK 丢帧、真实数据库 backoff 中停止。明确 RunRuntime 返回不代表监督器 Done，共享 SQL 只由外部 owner 在真正 join 后关闭，见 [退出验收](../docs/mq-persistence-shutdown.md)。

P4.2.2.2 生产连接替换与运行时接线：

```powershell
go test -mod=readonly -race ./application ./workers ./messaging/rabbitmq -run 'Test(PersistenceConsumerSource|SupervisedPersistence|Persistence(Connection|Supervisor)|ConsumerWaitClosed)' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistence(Connections|Lifecycle)Integration$' -count=1 -v -timeout=12m
```

新实际 suite 使用生产 NewSupervisedPersistenceFactory → RunRuntime，验证停库恢复后的当前轮次健康、空闲连接退役、缺失绑定期间不修复/消费，以及实际后续 AMQP 握手黑洞的部分连接清理。每轮退出直接检查队列，并等待连接返回独立 fixture 基线；旧单轮工厂生命周期 suite 同轮回归。隔离/Outbox 仍为测试占位，默认门禁保持，见 [连接来源与退出归属](../docs/mq-persistence-connections.md)。

P4.2.2.1 独立持久化监督器：

```powershell
go test -mod=readonly -race ./workers -run '^TestPersistenceSupervisor' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceSupervisorIntegration$' -count=1 -v -timeout=9m
```

默认测试覆盖串行轮次、数据库离线退避、旧工作/资源真实 join、停止期间晚回调、部分启动/清理失败和完整错误树恢复门槛。实际 fixture 验证停库积压与两连接自动恢复、真实 COMMIT 后结果错误的幂等重投、CHECK 技术错误终止及初始离线时关闭后不复活；最后核对唯一业务集合与连续 seq。此 suite 的资源来源是明确测试适配，生产 MQResources/聚合就绪另由上方 P4.2.2.2 suite 验证，详见 [持久化监督器](../docs/mq-persistence-supervisor.md)。

P4.2.1 消费失败分类与有界本地重试：

```powershell
go test -mod=readonly -race ./workers ./database ./application -run 'Test(Persistence(Retry|Failure|Factory)|Command(HandlerFailure|ReplayRollback))' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration -p 1 ./tests/testenv ./workers -run '^Test(PersistenceRetryIntegration|PersistenceConsumerIntegration)$' -count=1 -v -timeout=12m
```

新 suite 在独立 MySQL/RabbitMQ fixture 构造实际死锁、锁超时及 CHECK 失败，验证完整回滚后同 delivery 重试、次数耗尽仍保留原文/身份、提交/回滚响应错误不重试，以及退避中 drain/Close 的责任。提交/回滚结果错误是实际 SQL 完成后的 driver 注入，非 TCP 丢包；约束/锁/1s session lock wait 只用于测试。既有单条 suite 同轮回归强杀重投与实际数据库离线，增加一次尝试/分类断言，详见 [失败分类与重试](../docs/mq-persistence-retry.md)。

P4.1.2 顺序 worker 生命周期、就绪及角色工厂：

```powershell
go test -mod=readonly -race ./workers ./application -run '^TestPersistence(Service|Factory)' -count=30 -timeout=90s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceLifecycleIntegration$' -count=1 -v -timeout=9m
```

默认测试覆盖受控准入、setup/lifetime 隔离、部分启动回滚、失败停止、多 worker、取消/超时后真实 join、重复并发关闭、探测新鲜度及工厂门禁。实际 suite 验证 SQL 提交屏障下的 graceful/forced 退出、预取重投、并发幂等、实际 ACK 丢帧、真实工厂接入 RunRuntime 和数据库停机后的显式恢复；运行时的隔离/Outbox 是测试占位角色。默认 worker/all 仍拒绝缺失后台装配，详见 [持久化生命周期](../docs/mq-persistence-lifecycle.md)。

P4.1.1 单条持久化消费适配：

```powershell
go test -mod=readonly -race ./workers -count=30 -timeout=60s
go test -mod=readonly -race -tags=integration ./workers -run '^TestPersistenceConsumerIntegration$' -count=1 -v -timeout=9m
```

默认测试验证严格 wire/终态校验、仓储结束前不 ACK、错误/取消/COMMIT 未知和 ACK 失败。实际独立 MySQL/RabbitMQ suite 验证 P2 完整事务后的 ACK、媒体/群消息、重放/冲突/权限拒绝、并发消费者、真实子进程在提交前与提交后 ACK 前强杀、成功 COMMIT 后错误注入、数据库离线恢复和坏消息原文/headers 保留。fixture 仅清理本轮归属资源，生命周期已有上方独立 suite，自动恢复仍待后续 P4，详见 [持久化消费适配](../docs/mq-persistence-consumer.md)。

P3.6.1 独立确认式发送 handler/路由与客户端协议探测：

```powershell
go test -mod=readonly -race ./service ./router ./cmd/loadtest -run 'Test(SubmitCommand|SubmissionRoute|AcceptanceProbe)' -count=30 -timeout=60s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestCommandSubmissionIntegration$' -count=1 -v -timeout=5m
```

默认测试覆盖严格有界 JSON/身份、结果证据与错误映射、Bearer-only/Redis 鉴权失败、deadline 和客户端保留原请求。真实 suite 使用新 Gin 路由、实际 JWT/Redis/MQ，验证 202 后信封、非法/黑名单/Redis 故障零发布、真实丢失 confirm 的 unknown 与并发 429、原键显式重试新 attempt、Return+ACK 的 503，以及仅停止本轮 MySQL 后仍可确认接受。fixture 控制连接读回/ACK，不是业务消费事务；默认入口未切换，详见[发送适配](../docs/command-submission.md)。

P3.5.4.2 接受/消费者就绪汇总与健康接线：

```powershell
go test -mod=readonly -race ./application ./router -run 'Test(RuntimeHealth|HealthEndpoints)' -count=30 -timeout=60s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRuntimeReadinessIntegration$' -count=1 -v -timeout=5m
```

默认测试覆盖激活/退出门禁、缺失业务能力、鉴权/publisher/消费者状态独立、typed nil、并发读取和 HTTP 固定原因/缓存/legacy 拒绝。真实 fixture 使用 RunRuntime、Gin HTTP 和空队列订阅，覆盖 Redis 探测客户端关闭/重建、发布绑定故障与恢复、消费连接关闭，以及只停止本轮 MySQL 时接受仍可就绪、进入 drain 后撤销但 MQ lifetime 保留。角色仅声明测试能力，未实现生产 202、JWT 黑名单或业务事务；测试不读开发配置，不操作共享数据，详见[运行时就绪状态](../docs/runtime-readiness.md)。

P3.5.4.1 publisher 周期只读拓扑复核：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run 'Test(Supervisor|SessionTopology)' -count=10 -timeout=60s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQTopologyMonitorIntegration$' -count=1 -v -timeout=5m
```

默认测试覆盖空闲巡检、共享旧连接生命周期、超时取消、失败撤销准入、晚到成功隔离、旧调用 join 后才恢复及无自动重发；受控 AMQP 对端验证 channel.open 响应黑洞与丢失 confirm 后的 unknown。真实 Docker suite 移除本轮隔离绑定，并通过 loopback 代理丢弃管理响应，验证无新发布也能撤销就绪、只读不修复、人工恢复后新 epoch、原 accepted 保留且无探测/重发命令。只操作并清理本轮 fixture，详见[周期拓扑复核](../docs/mq-topology-monitor.md)；整体接受 readiness 尚未接线。

P3.5.3.3.3.2.2.2 默认 legacy 统一生命周期与退出信号：

```powershell
go test -mod=readonly -race . ./health -run 'Test(LegacyRuntime|RunProcessLegacy|StorageStop)' -count=30 -timeout=60s
go test -mod=readonly -race -tags=integration . -run '^TestLegacyRuntimeIntegration$' -count=1 -v -timeout=5m
```

默认测试验证共同阶段预算、回滚、超时后保留 owner/全局/存储、最终 join 后关闭及错误保留。真实独立 MySQL/Redis suite 运行默认 Gin 路由、JWT、WebSocket/Redis 订阅和实际提交屏障，覆盖正常取消、强制超时后的原文重放、注册 Busy 的失败回滚和 MySQL 离线存活。取消由注入的 NotifyContext context 触发，未向宿主发送 OS 信号；归属标签核验后清理本轮 fixture，详见 [统一生命周期](../docs/legacy-runtime-lifecycle.md)。

P3.5.3.3.3.2.2.1 legacy cron 生命周期与注册释放：

```powershell
go test -mod=readonly -race . -run '^TestLegacyWorker' -count=50 -timeout=60s
go test -mod=readonly -race -tags=integration . -run '^TestLegacyWorkerLifecycleIntegration$' -count=1 -v -timeout=5m
```

默认测试使用真实 cron/受控 owner/store，不连接 DB/Redis；实际 suite 使用 NewLegacyFixture 的独立 MySQL/Redis，覆盖真实提交屏障后的 graceful/forced 退出、原始前缀重放幂等、旧 owner 不删除 successor、默认工厂重复 stop 后共享存储仍可用。整体入口已接线，OS 信号投递未实测，详见 [worker 生命周期](../docs/legacy-worker-lifecycle.md)。

P3.5.3.3.3.2.1 legacy HTTP/WebSocket 生命周期适配器：

```powershell
go test -mod=readonly -race . -run '^TestLegacyHTTP' -count=1 -v -timeout=45s
go test -mod=readonly -race . -run '^TestLegacyHTTP' -count=30 -timeout=60s
```

独占 loopback HTTP 服务配合实际 Gin/Gorilla/service.Chat 验证停止准入、普通请求 drain、活动及被替换 WebSocket 的 socket/订阅/任务等待、超时后再次 Close 和 Hijack 停止竞争；net.Pipe 验证部分 socket handle 清理。订阅为受控 fixture，不连接 Redis/DB，不读取开发配置或使用 Docker。适配器已接到默认入口，详见 [生命周期边界](../docs/legacy-http-lifecycle.md)。

P3.5.3.3.3.1 进程入口分流与现代运行时信号（默认单元测试，不使用 Docker）：

```powershell
go test -mod=readonly -race . -run 'Test(RunProcess|Bootstrap)' -count=1 -v -timeout=90s
go test -mod=readonly -race . -run '^TestRunProcess' -count=20 -timeout=60s
```

覆盖配置单次加载、完整/部分缺失角色在连接和信号注册前拒绝、现代执行器参数转交、可控信号 context 取消后等待清理且保留错误，以及 legacy worker 先于存储关闭/全局恢复。另使用生产 RunRuntime 验证预取消的启动拒绝；没有业务工厂或 MQ I/O。取消测试不向宿主/其他进程发送实际信号，legacy 也已接入统一退出，边界见 [进程入口](../docs/runtime-process-entry.md)。

P3.5.2.2 有界消费 channel、顺序 delivery 与单条手动 ACK/Reject：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestConsumerChannel' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(ConsumerChannel|ConsumerSession|Publisher|Session|Supervisor)Integration$' -count=1 -v -timeout=12m
```

新的 suite 通过消费者 API 验证实际 prefetch、未确认及预取消息重投、旧 handle 隔离、可靠 DLX、生产 confirm 黑洞期间独立消费 ACK，以及本地 ACK 写入成功但途中丢帧后的完整重投。全部是本轮隔离测试，没有业务数据库提交；具体契约见 [消费 channel / 手动 ACK](../docs/mq-consumer-channel.md)。

P3.5.2.1 独立消费者基础连接、有界生命周期和双向故障隔离：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestConsumerSession' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session|Supervisor|ConsumerSession)Integration$' -count=1 -v -timeout=10m
```

此连接 suite 观察真实 broker 上不同生产/消费连接，验证生产 Write 背压、双方响应黑洞、独立关闭和只读拓扑漂移拒绝；其测试读回/ACK 来自 fixture 控制连接。真正的消费者 API delivery/ACK 验证由上方新增 suite 提供，业务提交后 ACK 仍未接入。覆盖见 [独立消费者连接](../docs/mq-consumer-session.md)。

P3.4.2 supervisor 的退避、epoch、拓扑阻断和 broker 重启验收：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestSupervisor' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session|Supervisor)Integration$' -count=1 -v -timeout=10m
```

测试只读复核后接单、不自动重发 unknown 命令、旧结果隔离与本轮 broker 持久卷重启；随机端口重分配的测试处理及边界见 [MQ supervisor](../docs/mq-supervisor.md)。

P3.4.1 的有界连接/写入/关闭及真实 TCP 确认黑洞测试：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestSession' -count=1 -v -timeout=30s
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQ(Publisher|Session)Integration$' -count=1 -v -timeout=8m
```

默认单元测试使用受控 AMQP 对端、net.Pipe 和本机 TCP/TLS 黑洞；Docker 测试用代理截获真实 broker ACK，验证 unknown、消息保留及关闭响应丢失时退出。覆盖与资源边界见 [MQ session](../docs/mq-session.md)。自动恢复由 P3.4.2 supervisor 在其上管理。

P3.3 publisher slot/confirm/return 状态机及真实 Return/NACK 用例：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestPublisher' -count=1 -v
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQPublisherIntegration$' -count=1 -v -timeout=8m
```

确定性先读 ACK 后排空 Return 的生产判定测试、池满零写入、晚到 ACK 隔离和真实 broker 容量拒绝细节见 [MQ publisher](../docs/mq-publisher.md)。有界 I/O/关闭已在 P3.4.1 验收，自动重连已由 P3.4.2 supervisor 管理，当前业务入口尚未接入。

P3.2 正式 RabbitMQ 拓扑与可靠 DLX 故障测试使用本轮专用 Docker broker，默认死信重试周期为 180 秒，完整用例可能需要数分钟：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq ./cmd/mqadmin -count=1
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQTopologyIntegration$' -count=1 -v -timeout=10m
```

覆盖主/隔离队列、失败投递上限、DLX 失效保留及恢复、策略/经典队列不匹配时数据保留和实际管理命令，详见 [本地 MQ 拓扑](../docs/mq-local-topology.md)。仅开启本轮 fixture，开发 `.env` 不参与测试。

当前验证工具链为 **Go 1.27.1**。运行前按 [README 工具链说明](../README.md#go-工具链) 设置当前终端的 `GOTOOLCHAIN=go1.27.1`，并用 `go version` 核对实际版本。

默认单元测试不启动外部服务：

P2.1 新增纯标准库的 `messaging` 协议测试，详细契约见 [消息协议 v1](../docs/message-protocol-v1.md)。覆盖固定身份/hash 向量、规范化和正文保留、严格 JSON/Unicode、整数及长度边界、信封重算、发布 attempt 和字符串序号；没有接入旧 HTTP 或数据库。可单独运行：

```powershell
go test -mod=readonly -race ./messaging -count=1
go test -mod=readonly ./messaging -run '^$' -fuzz '^FuzzRequestNormalization$' -fuzztime=20s -parallel=2
```

fuzz 使用 Go 子进程与本机缓存，检查解码不崩溃和成功规范化后的 JSON 往返不改变命令；不需要 Docker。短时 fuzz 通过不代替后续数据库幂等或端到端恢复验收。

P1 汇总的 L1—L12 测试名称、关键失败证据、F01—F04 故障边界和完整重跑命令见 [旧消息链路回归对照](../docs/legacy-reliability-acceptance.md)。P1.8.2 为 `TestMessageHTTPIntegration` 增加直接读取 Redis ZSET 的断言，验证通知失败后缓存仍在；MySQL 历史成功不再被用作缓存保留的证据。

新增确认结果不确定性验收：

```powershell
go test -mod=readonly -race -tags=integration ./scheduledTasks -run '^TestLegacyAcknowledgementUnknownIntegration$' -count=1 -v -timeout=8m
```

该用例在真实 Lua 已成功确认 128 条前缀后，由 Redis hook 向调用方返回 EOF；验证 worker 返回错误并永久停止本实例的消费尝试，未处理的第 129 条原文仍在队列且未落库，隔离记录已提交，其他实例不能抢占尚未释放的注册。原 worker 已无在途调用后正常释放注册，再用新 owner 处理尾部和完整重投，核对消息和审计记录保持不变。此处是成功执行后的错误注入，不是网络丢包；实际子进程崩溃由下面的 P1.8.1 用例验证。

P1.8.1 的真实进程崩溃恢复验收：

```powershell
go test -mod=readonly -race -tags=integration ./scheduledTasks -run '^TestLegacyWorkerCrashRecoveryIntegration$' -count=1 -v -timeout=8m
```

三个场景分别在首条消息提交、坏记录隔离提交、整批提交后阻塞测试子进程，此时尚未确认 Redis 队列。父进程从独立 MySQL 连接核对已提交内容，向队尾追加一条未读取消息，再强杀该 worker 并等待退出；确认其 MySQL 会话消失、注册锁不自动释放且新消费者被拒后，按恢复文档比较 token 删除锁。新子进程重新消费，逐字段核对全部消息、逐字节核对隔离原文及摘要/原因/时间，再重投一轮验证消息和审计行不重复、不改变。

每个场景使用单独的 Docker fixture；测试配置只由父进程通过 stdin 传给子进程，不读取开发配置。屏障和子进程入口只存在于带 `integration` 标签的 `_test.go` 文件中。失败清理先结束子进程，再关闭连接并按归属标签回收本轮资源。本测试覆盖旧链路 DB 已提交、Redis 尚未确认的实际进程退出，不代表 COMMIT 响应丢失、在途事务强杀、Redis 数据丢失或 MQ 故障已验收。P1 汇总结果见开发实施记录 P1.8.2。

P1.7 的 `TestSubscriptionConfirmsRequestedChannels` 使用受控 RESP 对端验证错误/重复频道不能通过就绪确认；`TestSubscriptionFullBufferClose` 用真实 go-redis 解析和缓冲、内存 net.Pipe 填满消息队列，验证手动关闭、仅取消 context、并发关闭后通道关闭且自有任务退出。`TestReceiveFullOutboundQueueCancellation` 验证私聊/群聊在 WebSocket 发送队列满时仍能响应取消并释放订阅；原有通道关闭/nil 消息/升级失败回归保留。

```powershell
go test -race -tags=integration ./database ./service -run '^(TestSubscriptionsIntegration|TestChatRedisIntegration)$' -count=1 -v -timeout=8m
```

真实 Redis 用例新增三轮未读取通知后的取消与并发 Close，核对消息通道已关闭及 NUMSUB 回到 0；原有私聊/多群就绪、错误密码、手动关闭，以及真实 JWT/WebSocket 连接、初始化失败、客户端断开的回归保留。这些测试不代替 Redis 重启、网络黑洞、群权限动态变更或消息恢复验收。

P1.6 的 `TestMessageHistoryMedia` 覆盖文本/图片/音频及原始内容引用，`TestMessageHistoryAuthorization` 覆盖 401、403、鉴权 UID 和错误信息隐藏，未知媒体类型不得返回部分历史。真实分页、当前群成员/软删除权限和存储故障使用独立 fixture：

```powershell
go test -race -tags=integration ./database ./service -run '^(TestMessageHistoryIntegration|TestMessageHTTPIntegration|TestSaveAndLoadMsg|TestDeleteMsg)$' -count=1 -v -timeout=8m
```

验证缓存只有最新一条时仍可翻完私聊双向/群聊全部已落库历史、相同时间戳及 identifier 的 ID 排序、伪造游标字段和跨会话游标、刚入群可读旧消息、离群/群删除后拒绝读取。历史无 Redis 依赖；缺 DB、DB 查询失败或请求取消时不得回退到缓存。旧发送测试仍核对 Redis 入队接受和错误语义。历史仅展示已落库消息，未落库消息需等待启用的 worker 处理；旧时间戳游标不是未来按提交顺序的 seq 同步协议。

P1.5 的 `TestSendMessageRequiresAuthenticatedContext` 验证缺失、零值或错误类型 UID 在解析请求前返回 401，且不访问消息仓库。`TestSendMessageJWTIdentity` 串联真实 JWT 中间件和发送 handler，覆盖私聊/群聊伪造身份、省略发送者、缺失/无效/错误签名 token、撤销和黑名单不可用；黑名单及消息仓库使用替身。`TestMessageHTTPIntegration` 使用独立 Redis 验证伪造 `fromUser.id` 不进入队列，并回归原有接受语义。

P1.4.1 的配置测试验证旧 worker 默认关闭、YAML 显式开关、`ICHAT_LEGACY_WORKER_ENABLED` 覆盖及非法值拒绝。bootstrap 测试验证关闭时不调用 worker 工厂（允许缺少该工厂）、开启时的启动/停止和失败清理顺序；真实 cron 测试阻塞任务跨越多个周期，验证不会重叠，任务结束或 panic 后可继续调度，Stop 等待当前任务退出。现有 MySQL 离线/恢复集成用例显式设置开启，保留其原来的 worker 生命周期范围。P1.4.2 增加真实 Redis 与独立测试子进程的注册竞争、正常释放、强制退出后拒绝接管、人工恢复、旧持有者不得释放新锁及原子确认校验；默认测试覆盖逐条检查所有权与部分持久化边界。恢复限制见 `docs/legacy-worker-recovery.md`。

`health` 包使用可控探测函数验证首次未就绪、失败后恢复、再次失败、超时、结果过期及取消退出；`router` 包验证三个健康接口的状态码、缺失探测器处理和禁用缓存。它们不替代真实 MySQL 停机恢复验收。

```powershell
go test ./...
```

启动流程的回归测试也在默认范围内：注入配置、MySQL/Redis 构造器、worker 和 HTTP 启动函数，覆盖依赖缺失、启动失败、连接释放及全局绑定恢复。MySQL 使用不联网的测试 driver，Redis 使用拒绝联网的测试 dialer；另用真实 cron 验证停止时等待正在执行的任务。可运行 `go test -race . -count=2 -shuffle=on` 检查重复执行和退出时的数据竞争。

MySQL 离线启动测试也在默认范围内：本地临时端口接受后立即关闭连接，模拟 MySQL 握手失败，验证默认 API 构造器无需连接即可启动 worker/HTTP、登录返回 503、退出关闭连接池及恢复全局绑定。另验证取消的 Ping 和独立迁移所用构造器仍在连接失败时报错。这些用例不使用 Docker 或开发数据库，不替代真实 MySQL 停机/恢复验收。

P0.2 的环境隔离验收需要本机 Docker 引擎已启动、Docker Compose 可用。首次执行可能需要下载测试镜像：

```powershell
go test -tags=integration ./tests/integration/... -count=1 -v -timeout=12m
```

这条命令自动创建、等待就绪并清理测试资源，不需要先手动执行 `docker compose up`。

P0.6.3d 的真实 MySQL 离线启动与恢复验收：

```powershell
go test -race -tags=integration . -run '^TestBootstrapMySQLRecoveryIntegration$' -count=1 -v -timeout=8m
```

测试先创建本轮 fixture 并迁移/写入测试账号，再停止该 MySQL 容器，使用默认 bootstrap 的数据库、Redis、worker 和健康探测装配启动本地临时 HTTP 服务。验证存活接口、存储未就绪、登录/联系人 503 和无 token 的 401；恢复后验证就绪、登录身份/token 与联系人查询。随后再次停机、恢复，始终使用同一 API 和仓储实例。Docker 重启可能重新分配随机宿主端口，测试以本地 TCP 转发器提供固定入口，仅更新其后端地址；应用配置、池和仓储不变，所有握手及查询均到达真实 MySQL。接受就绪仍为 503，不代表 MQ accepted 验收。容器停止/启动前核对本轮项目和 runId 标签；结束后按 fixture 归属清理，转发器等待所有连接任务退出。该测试不模拟网络黑洞或操作系统进程退出。

P0.5 的迁移命令验收同样使用独立环境：

```powershell
go test -tags=integration ./cmd/migrate -count=1 -v -timeout=8m
```

覆盖实际命令入口的退出码：连接失败、DDL 失败、首次创建四张旧业务表和 `legacy_message_quarantine`、重复迁移保留用户、消息及隔离原始字节；Redis 配置不可用时迁移仍成功。默认单元测试另行覆盖配置缺失/损坏后的非零退出，不连接数据库。迁移测试不读取开发配置，不对开发数据库执行迁移。

P0.6.2 的鉴权/注销单元测试覆盖 JWT 校验、黑名单错误分类、HTTP 401/503、请求取消及“写入成功后才下线”的顺序；不启动外部服务。真实 Redis 验收使用独立测试环境：

```powershell
go test -race -tags=integration ./tests/integration -run '^TestAuthRedisIntegration$' -count=1 -v -timeout=8m
```

该用例验证不存在的黑名单 key、封禁后的拒绝、正数且有界的 TTL、请求取消及客户端关闭；在本轮 Redis 中创建仅允许 GET/PING 的 ACL 用户，验证认证读取成功后 SET 被真实 Redis 拒绝时不会报告注销成功。测试结束关闭客户端并按归属清理本轮资源。

P0.6.3a 的账号接口默认测试使用注入的用户仓储，覆盖仓储/DB 缺失、查询写入错误、请求取消、业务失败以及登录成功后才标记在线。真实 MySQL 验收沿用独立 fixture：

```powershell
go test -race -tags=integration ./service -run '^TestAccountMySQLIntegration$' -count=1 -v -timeout=8m
```

该用例验证注册存储字段、密码校验和签发 token；8 路并发注册同一手机号只有一个成功；在本轮测试表上临时添加 CHECK 约束制造真实写入失败，随后移除；验证请求取消不写入、连接池关闭后注册/登录返回 503。schema 已由 fixture 核验，测试不改变开发数据库。

P0.6.3b.1 的联系人、好友、群列表和群成员查询默认测试覆盖仓储缺失、查询失败、请求 context 传递、空结果和成功响应；联系人查询的第二阶段失败必须返回 503，不能返回部分好友数据。真实 MySQL 验收：

```powershell
go test -race -tags=integration ./service -run '^TestContactQueriesMySQLIntegration$' -count=1 -v -timeout=8m
```

该用例在全局关系/群 manager 为空时验证注入仓储正常工作，核对四个接口的数据和空数组；验证七种仓储查询保留请求取消原因；临时重命名本轮 fixture 的群表，使好友查询成功而群查询失败，核对 503，恢复表名后重新查询成功；关闭连接池后四个接口均返回 503。变更仅限本轮隔离 schema，结束时清理环境。

P0.6.3b.2 的写入默认测试覆盖四个接口的依赖缺失、写入错误、请求取消、绑定错误及身份/context 传递；群 ID 校验和已知业务错误不会泄露底层信息。无网络事务测试验证建群开始失败、第二次写入失败、提交失败及 panic 回滚/传播。真实 MySQL 验收：

```powershell
go test -race -tags=integration ./service -run '^TestContactWritesMySQLIntegration$' -count=1 -v -timeout=8m
```

该用例验证双向好友、建群与创建者成员关系、入群、重复关系和非群主删除；在本轮关系表上设置临时 CHECK 约束，验证创建者成员写入失败会回滚群记录，反向好友写入失败不会留下单向关系。另验证取消请求不写入、不删群，群表不可用时删群返回 503，连接池关闭后四个写入接口均返回 503。约束及表名修改只限本轮隔离 schema。

P0.6.3c.1 的旧 worker 默认测试注入仓储，验证读取→落库→确认顺序、错误原因保留、空队列、批次计数异常和取消请求。读取失败不落库，落库失败不确认，确认失败返回错误；不依赖全局消息 manager 或日志文件。独立集成验收：

```powershell
go test -race -tags=integration ./scheduledTasks -run '^TestSyncMsg$' -count=1 -v -timeout=8m
```

除原有成功落库和空队列外，覆盖 Redis 客户端关闭、取消请求、真实 MySQL 写入失败后队列保留，以及通过 Redis hook 注入 LRANGE/EVAL 错误。EVAL 发送前失败用例断言数据库已提交而队列仍保留；P1.3.1 增加随后重投，验证数据库记录不变且队列确认成功。P1.2 增加 130 条消息分两轮（128 + 2）落库的实际 DB/队列断言。

P1.3.1 的旧消息幂等仓储验收：

```powershell
go test -race -tags=integration ./database -run '^TestLegacyMessagePersistenceIntegration$' -count=1 -v -timeout=8m
```

验证同批重复、跨批重放、已有/新增消息混合、七种字段冲突不覆盖原记录、批次回滚、真实 CHECK 失败、非主键唯一索引冲突不被误判为幂等成功，以及坏 JSON/空 ID 不写入。默认事务替身覆盖开始、第二次写入及 COMMIT 失败，错误原因保留。这些是直接调用 `SaveMsgs2DB` 的整批事务测试；P1.3.2 起 worker 使用 `ProcessDirtyMsgs` 逐条提交和隔离，仍复用同一消息幂等逻辑。

P1.3.2 隔离与连续前缀验收：

```powershell
go test -race -tags=integration ./scheduledTasks ./cmd/migrate -run '^(TestDirtyMessageQuarantineIntegration|TestSyncMsg|TestMigrationIntegration)$' -count=1 -v -timeout=10m
```

真实 Redis/MySQL 验证坏 JSON 前后有效消息均可落库；无效 UTF-8/NUL 原文、非法消息、ID 冲突和 CHECK 数据错误保留完整原文及原因。隔离提交后 EVAL 发送前失败再消费，不重复审计或覆盖原记录；移除原 CHECK 后也不自动执行业务重放。用仅拒绝本轮特定摘要的隔离表 CHECK 制造隔离写入失败，断言已提交前缀被确认、坏记录和未处理尾部保留；解除故障后继续推进。缺隔离表时队列不变，恢复表后可继续。

默认测试覆盖数据错误白名单、未知/连接/缺表/死锁/锁超时错误不隔离、原文校验、每条事务提交、提交失败不计入完成数、已完成前缀与双重错误保留、非法完成数不确认。记录按 SHA-256(raw) 去重并核验原始字节；完全相同的多个物理投递共用审计行。测试不代表真实网络 COMMIT 响应丢失、进程退出或多 worker 完整恢复。

P1.2 的有界读取及 Lua 原子确认验收：

```powershell
go test -race -tags=integration ./database -run '^TestDirtyMessageBatchIntegration$' -count=1 -v -timeout=8m
```

使用本轮独立 fixture 验证 128 条读取上限、读取后追加、过期批次、非首条不匹配、队列变短/不存在、JSON 原始字节差异、空记录/二进制记录及 WRONGTYPE。另让两个确认请求与尾部追加同时执行，断言只有一个确认成功，另一个返回前缀冲突，未落库尾部完整保留。默认单元测试覆盖确认参数边界、错误链、原始批次传递及超大批次拒绝。每次 worker 调用只处理一批；前缀变化时结束本轮，下次调用重新读取。单消费者约束、数据库幂等和确认响应丢失仍是后续验收范围。

P0.6.3c.2 的 HTTP 消息默认测试覆盖仓储缺失/失败、请求取消、参数/游标错误、身份绑定、分页参数传递、历史顺序/空数组及解码失败不返回部分历史。真实验收：

```powershell
go test -race -tags=integration ./service -run '^TestMessageHTTPIntegration$' -count=1 -v -timeout=8m
```

验证真实入队内容中的发送者为鉴权用户。P1.6 将历史断言更新为只读 MySQL：入队未落库时为空、落库后可读、旧格式/损坏缓存不影响结果、缺 DB 和 MySQL 查询失败时返回 503，并保留取消请求和缺失分页游标检查。P1.1 的发送故障断言保留：本轮脏队列设为字符串时，RPUSH 真正返回 WRONGTYPE，HTTP 返回 503 / `MESSAGE_ENQUEUE_UNKNOWN`，不执行 ZADD/PUBLISH，不创建缓存；缓存 key 设为字符串时，RPUSH 成功、ZADD 失败，仍尝试 PUBLISH 并返回旧接口成功，核对队列完整内容。另通过 hook 注入 PUBLISH 失败，断言队列保留；在真实 RPUSH 成功后的 hook 注入超时、取消、EOF，断言 HTTP unknown 与实际入队并存；入队确认后取消请求仍返回成功。关闭 Redis 客户端后发送返回 503 / unknown，直接调用历史 handler 仍可从 MySQL 读取（此测试未经过依赖 Redis 的 JWT 黑名单中间件）。

默认 `TestLegacyMessageAcceptance` 使用 `net.Pipe` RESP 替身验证 RPUSH→ZADD→PUBLISH 顺序、RPUSH 拒绝/响应丢失时无后续副作用、缓存/通知失败不撤销成功；`TestLegacyMessagePreCanceled` 验证调用前取消不发送命令。HTTP 单元测试验证 unknown 分类优先于普通存储错误，不泄露内部错误。发送成功仍为 HTTP 200 / code 0，只代表旧 Redis 入队确认；未接入 MQ 202，也未实现重投幂等、缓存完整性修复或通知重试。由于共享 Redis 客户端可能自动重试，调用 RPUSH 后所有未获成功结果的错误均保守归为 unknown，最终 WRONGTYPE 等错误不被当作此前所有尝试均未接受的证据。

P0.6.3c.3a 的订阅单元测试覆盖缺失依赖、连接/握手失败、取消阻塞的握手、并发重复关闭空句柄；接收循环测试覆盖部分初始化失败的资源释放、关闭通道、nil 消息和 context 取消。真实订阅验收：

```powershell
go test -race -tags=integration ./database -run '^TestSubscriptionsIntegration$' -count=1 -v -timeout=8m
```

在全局群 manager 为 nil 时建立私聊及两个群订阅，核对 Redis NUMSUB 和实际消息内容；取消后与手动 Close 后等待消息通道关闭、NUMSUB 回到零；另覆盖无群空句柄及错误 Redis 密码的握手失败。此用例不覆盖完整 WebSocket 握手和会话替换。

P0.6.3c.3b 默认测试使用本地 `httptest` WebSocket 服务和注入的订阅，覆盖升级前依赖/订阅失败、无身份拒绝、升级失败的资源释放、私聊/群消息转发、无群连接、客户端关闭、订阅关闭、请求取消、连接替换及注销下线帧。通过阻塞读取 goroutine，验证 handler 必须等其结束后才返回并调用 Offline；测试 logger 写入 `io.Discard`，不读取开发配置或创建日志文件。

```powershell
go test -race -tags=integration ./service -run '^TestChatRedisIntegration$' -count=1 -v -timeout=8m
```

真实验收使用独立 MySQL/Redis、JWT 中间件和本地 WebSocket 客户端，验证 401、升级成功与实际 PubSub 转发；缺失 MySQL 或关闭 Redis 的连接请求返回 503，已建立的原连接仍可用，失败请求创建的订阅得到释放；正常关闭和无群用户关闭后 NUMSUB 归零。本用例不覆盖完整应用服务器退出、Redis 服务端重启或跨实例会话抢占。

P0.3 已恢复旧业务集成测试。执行消息存取、缓存删除/回源、好友、群成员和定时落库测试：

```powershell
go test -tags=integration ./database ./scheduledTasks -count=1 -v -timeout=15m
```

4 个数据库查询 benchmark 使用独立 fixture，准备、预检和清理不计入查询耗时。可以先固定迭代次数检查可运行性，避免自动校准多次创建 Docker 环境：

```powershell
go test -tags=integration ./database -run '^$' -bench . -benchtime=1x -count=1 -timeout=15m
```

`1x` 仅验证 benchmark 能执行并通过结果断言，不是稳定的性能基线。每个测试/benchmark 调用都有自己的 MySQL/Redis 环境，整套测试需要几分钟；不同包可以独立运行。数据库查询和旧 worker fixture 已不需要绑定全局 manager；部分验证全局依赖隔离的单元测试仍会临时修改全局变量，所以不使用 `t.Parallel()`。

## P2.6 状态仓储

默认 `database` / `messaging` 测试覆盖查询输入、状态优先级、只读快照、无锁读取、失败返回空结果及损坏记录；不读取开发配置或访问外部存储。真实 MySQL 状态查询及共享命令读取解析器回归：

```powershell
go test -mod=readonly -race -tags=integration ./database -run '^Test(CommandStatusIntegration|CommandRepositoryIntegration)$' -count=1 -v -timeout=8m
```

每个顶层用例使用独立 fixture，校验 schema 归属后创建新版六表及必要的账号/关系/群表。新增状态测试核对当前发送者范围、同键异 hash 不泄露原结果、匹配未解决隔离记录、终态优先级、权限后续变更不改自己的终态，以及非 UTC session/driver 的规范时间。

测试用真实 driver 屏障暂停未提交发送，证明状态查询不等待行锁、不读到临时占位；另一屏障在命令查询后暂停，期间提交命令并解决隔离，证明后续查询仍来自相同 REPEATABLE READ 快照。默认 READ-COMMITTED 的测试连接用于核验仓储显式隔离级别。临时重命名 fixture 表、篡改后恢复消息正文、取消 context 和关闭池验证故障不能变为 pending。用例最后核对无 committed claim、schema 指纹不变和 fixture cleanup。

该测试不验证 HTTP 状态路由、隔离生产/重放、MQ 队列状态、真实 COMMIT 响应丢失或同步/历史权限；这些仍按对应小点实施。

## P2.7：可靠同步与历史

```powershell
go test -mod=readonly -race ./database ./messaging -count=1
go test -mod=readonly -race -tags=integration ./database -run '^TestReliableMessageHistoryIntegration$' -count=1 -v -timeout=8m
```

默认测试覆盖规范会话/游标输入、默认及最大 limit、字符串序号、最大 uint64、空页、私聊参与者和群成员检查、有界 lookahead、正反向缺号、损坏记录及 SQL/迭代/关闭/快照释放错误。任何失败都不得返回部分页或可推进的游标。

真实测试使用独立 fixture，消息均先由 `PersistCommand` 生成。覆盖双向私聊完整补拉与历史翻页、文本/图片/音频及 Unicode/NUL 原文、UTC 微秒输出、服务器 deliveredSeq 已较大时仍从本地 0 恢复、授权但没有消息的会话、入群后读取全部历史、离群/删群后的下一页拒绝，以及解除好友后私聊参与者仍可读历史。

测试连接默认 READ-COMMITTED、session 时区 +05:30、driver loc +13。真实 driver 屏障在授权/水位 SELECT 后用另一连接提交新消息或撤销权限：本页仍按旧快照读取，下一页检查新状态；授权后首次创建的会话也不能混入旧快照。实际消息及 Outbox 已写入但未提交的发送事务不会阻塞查询或泄露新 seq，提交后可从原游标补齐。

故障用例只在归属已验证的 schema 中移动消息 seq、篡改后恢复正文、临时重命名后恢复指定表，验证缺号/损坏/真实 1146 错误不会伪造完整结果。最大 uint64 测试刻意把两条已生成消息和水位临时移动到上界，仅证明该局部范围及最新历史没有溢出，不冒充完整连续历史；随后恢复。最终核对迁移指纹、无内部命令占位和 fixture cleanup。

P2 仓储集中回归：

```powershell
go test -mod=readonly -race -tags=integration ./database -run '^Test(CommandRepositoryIntegration|MessagePermissionsIntegration|ReliableMessagePersistenceIntegration|CommandStatusIntegration|ReliableMessageHistoryIntegration)$' -count=1 -v -timeout=8m
```

这些用例尚未验证新 HTTP 同步路由、会话列表、浏览器本地保存/周期恢复、消息通知或 ACK；对应 P5/P6。仓储不读取 Redis，也不通过服务器 deliveredSeq 代替客户端本地数据。

## 每轮资源

`tests/testenv.Start(ctx, composeFile)` 生成随机 128 位 runId 和测试密码，使用仓库的 `docker-compose.test.yml`：

| 资源 | 隔离方式 |
| --- | --- |
| Compose 项目 | `ichat-test-<runId>`，拒绝复用已经存在的项目资源 |
| MySQL | 本轮独立容器；schema 为 `ichat_test_<runId>`，使用仅获该 schema 权限的 `ichat_test` 用户 |
| Redis | 本轮独立容器、独立数据卷；即使旧代码使用固定 `dirty:msgs` key，也不会访问开发 Redis |
| RabbitMQ（按需） | `StartWithRabbitMQ` 开启 `mq` profile；本轮独立 broker/卷、`ichat_test_<runId>` vhost 及专用无管理权限应用账号 |
| 端口 | 只绑定 `127.0.0.1`，由 Docker 动态分配，不占用开发环境固定端口 |
| 卷和网络 | 各轮独立，包含 Compose 项目标签与 `io.ichat.test.run` 标签 |
| 镜像 | MySQL 8.0 / Redis 7，固定已验证 digest，未改变开发 Compose |

Compose 项目名用于区分不同环境的资源，相关行为见 [Docker 项目名说明](https://docs.docker.com/compose/how-tos/project-name/)。此处还额外校验本轮归属标签，避免仅凭名称删除资源。

测试环境不读取应用的 `config/config.yml` 或开发 `.env`。调用 Docker 时显式传入 Compose 文件、项目名和空 env 文件，并移除继承的 `COMPOSE_*`、`ICHAT_TEST_*` 覆盖值；连接地址只从本轮容器映射端口中取得。Docker 自身的本机连接配置仍由 Docker CLI 使用。

`Config()` 仅在启动、健康检查及资源归属校验成功后返回连接参数。它返回配置副本，不修改 `config.Conf`、`utils.DB/RDS` 或全局 manager。

## 清理与失败

调用方先关闭 Redis/MySQL 客户端，再调用 `Environment.Close(ctx)`。推荐用 `t.Cleanup` 注册，并给清理分配独立的超时时间；`tests/integration/environment_test.go` 提供完整示例。

清理先列举本轮项目资源，并逐一核验两种归属标签。所有资源验证成功后，才按容器 → 卷 → 网络的顺序移除列举到的具体资源。任何标签缺失/不符、inspect 失败均返回错误，不开始删除；实际删除中途失败会返回错误，可再次调用 Close 处理剩余资源。

不执行 `FLUSHALL`、共享库删表、`docker system prune` 或无范围清理。启动部分失败仍会尝试清理本轮已创建的资源；拒绝复用的既有项目不会被接管或清理。

进程被强制终止时，Go cleanup 不保证执行。若发生这类中断，使用日志中的 runId 定位资源，核验项目标签与 run 标签后再清理；不要把整个 Docker 实例作为清理范围。Docker 无法访问时，清理错误会保留并报告，不能把未清理写成已清理。

## 当前验收范围

- 单元测试：非法 runId、开发 Compose 误传、既有项目碰撞、归属不符、inspect 异常、清理错误传播、重复 Close、启动失败及端口校验。
- 真实集成测试：同时存在的两套 MySQL/Redis 使用同名表/key 保存不同内容；删除第一套后，第二套数据仍完整；继承的开发配置不能覆盖本轮设置。
- 旧业务 fixture：每次创建 5 个用户、3 个群、明确的好友/成员关系；用户和群 ID 来自实际插入结果，消息 ID 包含本轮 runId 和递增编号。schema 校验后才迁移和写 fixture，全局 manager 在用例结束后恢复。
- 消息断言：双向会话的全部分页、同毫秒顺序、真实内容和媒体类型；队列中的原始消息集合；删除已存在的缓存 key 后从 MySQL 回源，且另一会话缓存和已存消息不受影响。
- 关系断言：精确好友/群/群成员 ID 集合、双向好友、无关系用户的空结果，防止两种查询同样返回错误结果却被判为通过。
- SyncMsg 断言：同步前数据库为空且队列有 3 条消息，同步后逐字段检查 3 条私聊/群聊及文本/图片/音频记录，队列为空；再次执行空队列同步不改变数据。直接断言任务返回值和数据库/队列状态，不依赖日志判断成功。
- 已覆盖旧 worker 的显式启用、防重入、跨进程注册与人工恢复，以及消息/隔离提交后、Redis 确认前的真实进程崩溃和幂等重投；隔离失败与连续前缀推进另有集成回归。
- RabbitMQ 版本、独立 vhost/账号、双环境同名资源隔离及基本 AMQP/quorum probe 已在 P3.1 验证；详细命令与源码边界见 [MQ 测试环境](../docs/mq-test-environment.md)。常规 fixture 仍不启用 MQ，测试必须显式调用 `StartWithRabbitMQ`。
- 按角色准备 MQ 连接：`go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestMQPreparationIntegration$' -count=1 -v -timeout=6m`。验证 legacy/api/worker/all 的真实独立连接数量、无订阅/无消息领取、成功准备后的 context 取消隔离、部分初始化后握手超时与全资源回收；主/隔离队列待处理内容完整。详见 [MQ 资源准备](../docs/mq-runtime-resources.md)。publisher/consumer 传输和拓扑的其他实际故障范围见实施记录。
- 同一 MQ 准备 suite 另包含 4 个 managed lifecycle 场景：api/worker/all 取消自动关闭、all 的已取消 Close observer 先返回但资源仍独立清理，Done/Wait 加入后回到 broker 控制连接基线。详见 [MQ 托管生命周期](../docs/mq-lifecycle.md)。不包含业务任务或在途事务的进程退出。
- 服务执行器：`go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRuntimeExecutionIntegration$' -count=1 -v -timeout=7m`。fixture 角色验证 drain 中仍可获得真实 publisher confirm、构造/激活失败回滚、drain 超时后的关闭，以及原主/隔离内容和已确认命令完整；不启动业务订阅或实际数据库事务。详见 [运行时执行器](../docs/runtime-execution.md)。
- 生产 API 生命周期：`go test -mod=readonly -race -tags=integration . -run '^TestProductionAPIIntegration$' -count=1 -v -timeout=7m`。从默认生产工厂进入实际 RunRuntime/API，验证真实 Redis GET 断连恢复、确认后的 drain、关闭超时保留客户端、端口占用回滚和 MySQL 离线接受；仅使用独立 fixture，无业务 consumer。默认 loopback TCP 测试另验证慢头/慢正文期限。详见 [API 生命周期](../docs/api-runtime-lifecycle.md)。
- 尚未包含：实际 COMMIT 响应丢失、在途事务强杀、新 MQ 业务提交后 ACK/审计/Outbox、受控隔离重放、三节点与生产环境验收；其余故障窗口按开发规范后续小点推进。
