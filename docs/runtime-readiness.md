# 接受就绪汇总与健康接口（P3.5.4.2）

[P4.1.2 持久化服务](mq-persistence-lifecycle.md)已实现具体 ConsumerReady：本地订阅/准入/lifetime 加真实 SQL 探测的新鲜度。后续 [P4.2.2.2](mq-persistence-connections.md)的显式监督工厂通过生产连接来源管理恢复，聚合健康改读当前轮次，旧 session 的 Done 不再阻止新轮次 ready；旧单轮工厂保持停止策略。隔离/Outbox 在集成中仍是明确测试占位服务，不代表完整生产 worker 已装配。

RunRuntime 现在为每次现代运行实例创建一个只读 Health capability，通过 RoleDependencies.Health 传给选中的服务工厂。它组合运行阶段、业务能力和当前 MQ 状态，不把 Start 成功或一个可连接的 publisher 当作业务接受就绪。

P3.6.2 已装配[生产 API 与 Redis GET 鉴权监控](api-runtime-lifecycle.md)，显式 api 模式可运行确认式接受入口。worker/all 仍因缺失后台业务角色在外部 I/O 前拒绝；默认 legacy Router 继续返回接受未配置。

## 状态来源

| 观察 | 必要条件 | 不作为该观察的条件 |
| --- | --- | --- |
| Acceptance | api/all 模式；全部选中服务成功激活；API 显式 RuntimeAcceptance 能力；AcceptanceReady 与 AuthenticationReady 为真；publisher State=ready、Ready=true 且 epoch 非零；运行未退出 | MySQL 在线、本地消费者在线或远程 worker 的状态 |
| Consumers | worker/all 模式；全部选中服务成功激活；当前连接组完整且每个 session 的 Done 非 nil/未关闭；持久化/隔离角色显式 ConsumerReady 为真；运行未退出 | publisher 或 API 鉴权是否在线 |

RuntimeAcceptance 的 AcceptanceReady 必须表示新的确认式接受 handler 已装配且仍接单；AuthenticationReady 必须包括 Redis 鉴权依赖的有时效缓存状态，不能只表示 JWT key 已配置。生产 API 已实现这些能力及实际 handler。缺少接口、方法返回 false 都保持未就绪。

RuntimeConsumerReadiness.ConsumerReady 必须包含本角色已激活订阅、业务准入和存储可用性。准备阶段的空闲 ConsumerSession 不足以证明业务消费就绪；缺少接口或当前任一连接关闭均不可 ready。持久化重连/暂停由具体监督器负责；当前仅在准备/替换时核验 MQ 拓扑，健康请求不会调用 Verify、OpenConsumer 或 ACK。

任一选中服务 Done 关闭会触发整个 RunRuntime 退出，因此健康观察先报告服务不可用，随后变为 stopping。本地消费者连接关闭而服务仍在恢复时，只影响 Consumers；MySQL 故障时命令仍可由 MQ 接受，不因等待落库而撤销接受能力。是否能返回某次 accepted 仍必须由该次 publish 的完整 confirm/return 判定决定。

## 生命周期与并发

1. 工厂构造及任何部分激活期间，所选能力为 starting。工厂/Start 即使查询 Health，也不能提前获得 ready。
2. 所有服务成功启动、Done 检查及最终 setup 检查通过后，owner 发布不可变绑定。setup context 取消不结束该实例；健康读取按当时的服务缓存状态和 publisher Status 汇总。
3. 进程 context 取消立即使后续观察变为 stopping；启动回滚、意外退出等清理路径也在任何 StopAdmission/Drain 前永久停止该健康 owner。
4. 撤销健康状态不取消 MQ/role lifetime。原有停止准入 → drain → 取消 lifetime → 关闭服务/MQ 的顺序保持，既有在途工作保留 drain 机会。
5. 保留的 Health 在运行返回后继续报告 stopping，不能被迟到的依赖恢复改回 ready。MQ 组显式关闭也撤销所选就绪状态。

健康请求只读取本地缓存，不联网、等待、发布或开启订阅。业务能力方法也必须是非阻塞缓存读取，不能回读聚合 Health，否则会递归。AuthenticationReady/ConsumerReady 所依据的监控应提供新鲜度/过期撤销；本汇总器不自行发起 Redis/MySQL 探测。

RuntimeStatus 是按值返回的时点观察，不保证下一请求成功；多个依赖的读取不是原子远程快照。停止标记、进程取消和 MQ 关闭在返回前再次核对。能力本身不暴露 setter/Close，格式化不输出其 context/依赖；公开状态只包含固定枚举。

## HTTP 适配

现代 API 工厂在自己构造的 Gin engine 上调用 `router.RegisterHealth(engine, storage, inputs.Health)`。旧 `router.Router(...)` 内部始终使用 nil runtime capability，不能凭 Config 中的现代模式使旧发送路由宣称支持 accepted。

| GET | 就绪含义 | 未就绪示例 |
| --- | --- | --- |
| /health/live | 进程 HTTP 存活，200 | 不合并依赖状态 |
| /health/ready/storage | 原有 StorageReady，MySQL 探测 | 503 STORAGE_UNAVAILABLE |
| /health/ready/accept | RuntimeStatus.Acceptance | 503 MQ_ACCEPTANCE_NOT_CONFIGURED / RUNTIME_STARTING / RUNTIME_STOPPING / RUNTIME_SERVICE_UNAVAILABLE / AUTHENTICATION_UNAVAILABLE / MQ_PUBLISHER_UNAVAILABLE |
| /health/ready/consumers | 本进程的 RuntimeStatus.Consumers | 503 CONSUMERS_NOT_CONFIGURED / RUNTIME_STARTING / RUNTIME_STOPPING / RUNTIME_SERVICE_UNAVAILABLE / MQ_CONSUMER_UNAVAILABLE |

所有 ready 路由都使用 Cache-Control: no-store，成功仅返回 `{"status":"ready"}`。未知枚举保持 503/READINESS_UNAVAILABLE，不回显外部错误。nil/typed nil runtime capability 按未配置处理。api 模式的消费者状态、worker 模式的接受状态都为未配置，不能推断远程实例。legacy 的新增消费者健康路由同样返回未配置 503。

## 验证与后续

```powershell
go test -mod=readonly -race ./application ./router -run 'Test(RuntimeHealth|HealthEndpoints)' -count=30 -timeout=60s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRuntimeReadinessIntegration$' -count=1 -v -timeout=5m
```

默认测试覆盖模式/激活、缺失业务能力、鉴权与 publisher 当前 epoch、消费者独立故障、nil/typed nil、MQ 关闭、进程取消、StopAdmission/drain 前撤销、并发观察、启动失败/服务意外退出、context 格式化脱敏，以及 HTTP 状态/固定原因/缓存/legacy 拒绝。

真实 suite 使用本轮隔离 MySQL/Redis/RabbitMQ、生产 RunRuntime、实际 Gin HTTP 和真实空队列订阅。四场景验证 Redis 探测客户端关闭后重建、发布绑定异常后的新 epoch 恢复、消费者连接关闭独立诊断、仅停止本轮 MySQL 时 storage/consumers=503 而 accept/live=200；进程取消后 drain 屏障内 accept/consumers=503，MQ/role lifetime 仍可用。

这些角色显式声明 fixture 就绪能力，不是生产 HTTP accepted、JWT 黑名单校验或业务事务处理器。测试中的 Redis 故障是本轮客户端关闭/重建，没有停止 Redis 服务。健康监控复用有时效 Storage probe，实际 API 的 GET 鉴权状态来源已在 P3.6.2 装配；不能用 fixture 冒充生产装配。MySQL 关闭期间的 EOF 日志属于本轮主动故障。

P3.5.4 的拓扑巡检和就绪装配基础范围收口；P3 基础设施与接受入口范围已验收收口。P3.6.1 [独立确认式 HTTP 发送适配](command-submission.md)及客户端协议探测已完成隔离验证，P3.6.2 [生产 API 生命周期、Redis GET 鉴权探测与运行时装配](api-runtime-lifecycle.md)已完成，阶段证据与后续边界见 [P3 验收对照](mq-reliability-acceptance.md)，下一项为 P4 持久化消费适配。默认入口尚未切换，保持未实现角色门禁；业务提交后 ACK、消费者恢复、Outbox 和端到端验收仍在后续阶段。
