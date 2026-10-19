# P3 RabbitMQ 与接受入口验收对照

范围：开发规范第 6 节、P3.1—P3.6 及第 13 节中相关故障窗口。核对基线为 `2c9c3bb`；本轮执行结果、失败尝试和隔离资源记录见[实施记录](message-reliability-mq-progress.md)。本页区分已实现的传输/API 能力与尚待 P4—P8 验证的业务恢复，不能将 MQ confirm 当作落库或送达。

2026-10-18（Australia/Sydney）：P3 基础设施、可靠 publisher 与接受入口范围验收收口。全仓库两轮随机顺序 race、30 轮确认专项、vet、集成标签编译及 13 个 MQ/API 集成 suite 通过；集中集成无失败、无 skip。完整 F05—F38 故障矩阵仍按下表归属继续实施。

## 运行边界

| 入口 | 当前行为 |
| --- | --- |
| 默认 legacy | 原 Redis List 链路及受控退出，接受健康保持未配置；不返回新协议 accepted |
| 显式 api | 真实 RunRuntime、MQ supervisor、JWT/Redis 黑名单、确认式 `/auth/sendMessage` 及四个健康路由 |
| worker / all | 因缺失持久化、隔离与 Outbox 业务角色，在信号注册和外部连接前拒绝 |
| 迁移 / mqadmin | 显式独立命令；业务启动不迁移、不声明修复或删除拓扑 |

API 没有接入旧登录、账号、联系人、WebSocket、状态或历史路由；已有有效 JWT 的接受测试不代表账号到落库的完整链路。默认配置和共享开发数据的切换归 P7。未完成 consumer 时，只在独立测试环境积压命令。

版本是仓库固定且实际核验的 RabbitMQ 4.3.6 / amqp091-go v1.15.0 / Go 1.27.1，不表示当前最新版本。镜像 digest 及客户端校验和见[测试环境](mq-test-environment.md)。本地 quorum 实际成员数为 1，不能据此声称生产三副本容灾。

## 阶段要求与可执行证据

以下测试名可通过 `go test -run` 定位。“集成”表示须启用 integration 标签并显式创建独立 Docker fixture；其余为默认测试，部分默认测试使用自有真实 loopback TCP/受控 AMQP 对端。

| 项目 | 实现边界 | 核心断言与测试 |
| --- | --- | --- |
| P3.1 固定版本与隔离 | `docker-compose.test.yml`、`tests/testenv` | `TestRabbitMQEnvironmentIntegration`（集成）核验 broker、独立 vhost/随机凭证、权限、两环境同名资源隔离；fixture 默认测试拒绝项目碰撞、错误标签和开发 Compose。 |
| P3.2 本地拓扑 / 可靠 DLX | `messaging/rabbitmq/topology.go`、`cmd/mqadmin` | `TestLocalTopologyRefusesDriftBeforeWrites` / `TestLocalTopologyCreatesQuarantineFirstAndChecksReadOnly`；`TestRabbitMQTopologyIntegration`（集成）核验单成员 quorum、参数、原文、delivery-limit、DLX 无绑定时源消息保留和恢复转发，策略/已有 classic 队列异常不自动删改。 |
| P3.3 信封 / slot / confirm-return | `messaging/rabbitmq/publisher.go` | `TestPublisherValidatedPersistentAttemptsAndReuse`、`TestPublisherACKFirstDrainsOriginalReturn`、`TestPublisherNotificationFailuresRetire`、`TestPublisherCapacityAndSlotIsolation`；`TestRabbitMQPublisherIntegration`（集成）实际并发发布、Return+ACK、reject-publish NACK 和已确认集合保留。 |
| P3.4.1 有界 I/O / unknown | `messaging/rabbitmq/session.go` 及连接 owner | `TestSessionHandshakeAndSetupHardBounds`、`TestSessionBlockedWriteAndLostConfirmAreUnknown`、`TestSessionRealTCPSilentTLSHandshake`；`TestRabbitMQSessionIntegration`（集成）丢真实 confirm/关闭响应，断言请求有界、unknown、队列正文完整、代理任务退出。 |
| P3.4.2 重连 / epoch | `messaging/rabbitmq/supervisor.go` | `TestSupervisorReconnectWaitsForOldQuiescence`、`TestSupervisorRefusesEpochWhenCleanupCannotJoin`、`TestSupervisorOldFailureCannotRetireReplacement`；`TestRabbitMQSupervisorIntegration`（集成）新 epoch、无自动重发、错误拓扑未就绪、保留数据卷的 broker 重启恢复。 |
| P3.5.1 配置 | `config/rabbitmq.go` | 配置默认、环境覆盖、纯构造与脱敏测试；非法/缺失参数在拨号前拒绝。配置加载不读取 Compose .env，不绑定旧全局。 |
| P3.5.2 独立消费传输 | `consumer_session.go`、`consumer_channel.go` | `TestConsumerSessionProducerWriteStallIsolation`、`TestConsumerChannelManualSingleDispositionAndIdentity`、`TestConsumerChannelBlockedDispositionAndShutdownBounds`；两个 consumer 集成 suite 核验独立连接、prefetch、顺序交付、单条 ACK/Reject、旧 handle 隔离、丢 ACK 后重投。没有业务事务。 |
| P3.5.3 计划 / 资源 / 执行器 | `config/runtime.go`、`application`、`process.go` | 纯角色/参数、部分构造回滚、服务先全构造后激活、API 最后启动、停止准入→drain→关闭服务→MQ；`TestMQPreparationIntegration` / `TestRuntimeExecutionIntegration`（集成）实际连接及确认。其业务服务是明确标记的 fixture，不冒充生产 worker。 |
| P3.5.3 legacy 退出 | 主包 HTTP/cron/runtime owners | 现有 legacy 生命周期默认测试检查取消、任务 join、存储归属和失败回滚。既有 legacy Docker 记录属于此前阶段；本轮 P3 集中 MQ 回归没有重跑 legacy 业务集成。 |
| P3.5.4 持续核验 / 就绪 | `supervisor.go`、`application/runtime_health.go`、`router/health.go` | `TestSupervisorPeriodicTopologyFailureRevokesIdleEpochAndWaitsForCleanup` 及周期 suite；`TestRabbitMQTopologyMonitorIntegration` / `TestRuntimeReadinessIntegration`（集成）空闲拓扑漂移、management 黑洞、新 epoch 恢复与停止门禁。ready 为缓存时点观察，不保证下一次发送成功。 |
| P3.6.1 handler / 协议探测 | `service/submit_command.go`、`router/submission.go`、`cmd/loadtest/acceptance.go` | `TestSubmitCommandResultMappingAndContradictoryReceipts`、`TestSubmissionRouteBearerOnlyAndAuthDependencyFailures`、`TestAcceptanceProbeUnknownAndDefiniteFailureClassification`；`TestCommandSubmissionIntegration`（集成）实际 202、429、unknown、Return+ACK、鉴权拒绝和 MySQL 离线接受。 |
| P3.6.2 生产 API owner | `api_runtime.go`、`process.go` | `TestAPIStopAdmissionPreservesAdmittedPublishForDrain`、`TestAPIExpiredCloseRetainsClientsUntilRequestsAndProbesJoin`、`TestAPISlowHeadersAndBodyAreBoundedByServer`；`TestProductionAPIIntegration`（集成）使用默认生产 API 工厂与实际 RunRuntime，验证 Redis GET 断连恢复、真实 confirm 后 drain、超时保留客户端、端口回滚及 MySQL 离线。 |

## P3 五项验收条件

| 要求 | 直接证据 | 结论范围 |
| --- | --- | --- |
| Return+ACK 调度反转不误报 accepted | `TestPublisherACKFirstDrainsOriginalReturn` 在原始两通知均缓冲后，刻意先读取 ACK，调用真实生产确认分支；Docker Return+ACK / HTTP 503 unroutable | 通过；没有用随机 select 命中概率代替确定性回归。 |
| 确认丢失重试安全 | `TestRabbitMQSupervisorIntegration/unknown_reconnects_with_new_epoch_without_replay`、`TestCommandSubmissionIntegration/lost_confirm_overload_and_same_key_explicit_retry` | P3 通过：unknown、原命令保留、恢复无自动重发、显式重试保留 commandId/hash 并生成新 attempt。最终仅一条业务 message 仍须 P4/P8 将 MQ 重投接入 P2 幂等事务验证。 |
| 网络黑洞超时有界 | session / consumer / topology-monitor 默认和实际代理场景；API 慢头/慢正文、handler deadline | 通过已列传输/API 场景；有界 slot、单次操作、连接 watchdog 及任务 join，不是吞吐/长期压力下内存基线或 broker 磁盘告警验收。 |
| 旧 ACK 不污染新请求 | `TestPublisherLateAckCannotReachAnotherSlot/write_failure` 与 `/confirm_timeout`；两次同 tag=1、不同 channel epoch，旧 ACK 不完成新发布，新发布必须消费自己的 ACK | 通过；本轮补齐确认超时后的直接时序回归。旧 session 故障也不能退休替代 epoch。 |
| 拓扑错误不触发自动删队列 | topology 默认拒绝写入测试、实际 classic 队列/策略/DLX 场景、supervisor drift / periodic monitor | 通过；应用只撤销 ready 并重试只读核验；由 fixture 人工恢复其自有绑定，应用不修复或重发。 |

## 故障矩阵与后续责任

“P3 部分完成”表示传输/API 断言通过，整条 Fxx 业务不变量尚未验证，不能记为完整通过。

| 故障 | 已有证据 | 当前边界与后续 |
| --- | --- | --- |
| F05 / F06 | 无路由 Return、拓扑预检/周期撤销、确定性 ACK-first 判定、真实 HTTP 503 | P3 传输/API 范围完成。 |
| F07 | 真 confirm 丢帧、503 unknown、同键显式重试/新 attempt、原文可读回 | P3 部分完成；重投后只有一条业务 message 归 P4/P8。P2 仓储幂等已有独立测试，尚未连成同一故障链路。 |
| F08 | loadtest 受控 HTTP 响应丢失时保留原请求文件，unknown 不换键；已知 accepted 不倒退 | P3 部分完成；尚无“真实服务 202 在网络丢失→客户端持久状态恢复→唯一落库”的整链测试，完整客户端归 P6/P8。 |
| F09 | 上述直接超时/旧 ACK 回归，以及旧 epoch 清理屏障 | P3 传输范围完成。 |
| F10 | 受控 Write 背压、真实 TCP/TLS/AMQP/management 响应黑洞、池满 429、通知缓冲与 slot 上限 | P3 部分完成；无 broker 资源告警、长期压力及总进程 goroutine/内存容量基线，归 P8。 |
| F17 | JWT 发送者范围、伪造 body 字段拒绝、规范 DTO；P2 状态仓储按发送者隔离 | 仅发送及仓储部分完成；新 HTTP status、跨用户 ACK 和当前 session 约束归 P5/P6。 |
| F18 | 实际 MySQL 离线，API live/accept 200、storage 503，已有 JWT 发送真实 202 | 接受部分完成；现代 DB 接口尚未暴露，不能用 404 算作预期的 DB 503。相关接口归 P6。 |
| F20 | 真实 Redis 客户端失败及 loopback 断连，鉴权 503、无发布，GET 探针撤销并恢复 ready | P3 失败关闭范围完成。 |
| F21 | 实际容量策略 reject-publish 导致 NACK，已确认原文保留；默认 handler 覆盖 NACK→503 | P3 部分完成；真实 HTTP NACK、磁盘告警和容量/持续过载归 P8。测试策略仅故障注入，结束后恢复 policy-free 契约。 |
| F22 | 实际 DLX 绑定失效时源队列保留，恢复绑定后转移 | P3 拓扑部分完成；隔离发布 confirm 丢失、审计事务/责任转移及幂等重放归 P4/P8。 |
| F34 | 停止并重启本轮 broker，保留命名卷，确认未消费命令完整且 supervisor 恢复 | P3 单节点传输范围完成；fixture 固定回原宿主端口，未实现产品服务发现，也不是节点容灾。 |
| F37 | 实际发布 drain、强制超时、启动失败回滚；API 工作未退出前保留私有客户端 | P3 发布/API 部分完成；业务消费/Outbox 在途操作、实际 OS 信号与强杀窗口归 P4/P5/P8。 |
| F11—F16 / F19 / F23 | P2 独立事务能力可复用 | MQ 与业务事务/隔离/恢复尚未接线，归 P4；没有生产 persistence/quarantine 角色。 |
| F24—F33 / F38 | 个别 P1/P2 仓储或协议测试不等于整链完成 | Outbox、通知、ACK、同步/客户端及媒体整链归 P5/P6/P8。 |
| F35 / F36 | 无三节点实测 | 生产三节点部署、实际队列三成员、单节点/多数故障归 P8。 |

fixture 控制连接 Get/ACK 只用于核对传输，不是业务提交后 ACK。P3 不声称端到端 exactly-once、客户端收到、自动业务重放、生产高可用或容量保证。

## 重跑与证据记录

设置 `GOTOOLCHAIN=go1.27.1`、清空不匹配的 GOROOT、GOCACHE 指向仓库 .gocache，ICHAT_CONFIG_FILE 指向不存在的路径；不读取开发 .env/config。集中集成命令为主包一个生产 API suite 加 testenv 的 12 个 MQ suites，不是全仓库业务集成或 benchmark。

```powershell
go test -mod=readonly -race ./... -count=2 -shuffle=on -timeout=120s
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestPublisher(ACKFirstDrainsOriginalReturn|LateAckCannotReachAnotherSlot|ConfirmTimeoutAndConcurrentClose)$' -count=30 -timeout=60s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration -p 1 . ./tests/testenv -run '^Test(RabbitMQ.*Integration|MQPreparationIntegration|RuntimeExecutionIntegration|RuntimeReadinessIntegration|CommandSubmissionIntegration|ProductionAPIIntegration)$' -count=1 -shuffle=on -timeout=20m -json
```

`-p 1` 控制包间并发；每个 fixture 都使用独立随机 project/vhost/凭证和端口。Docker 故障控制、清理前检查项目/run 标签。保留 JSON 中的失败和 skip，区分包没有匹配用例与测试跳过；未执行的测试不能算通过。版本、随机顺序 seed、runId、耗时及清理结果只以当轮日志/实施记录为准，历史记录不算本轮实测。

后续 [P4.1.1](mq-persistence-consumer.md)补上单条 wire → P2 完整事务 → ACK 及实际强杀重投；[P4.1.2](mq-persistence-lifecycle.md)补上多 worker 受控退出、就绪/角色工厂、真实 ACK 丢帧重投及 RunRuntime drain；[P4.2.1](mq-persistence-retry.md)补上真实死锁/锁超时的有界本地重试与完整错误树分类；[P4.2.2.1](mq-persistence-supervisor.md)及 [P4.2.2.2](mq-persistence-connections.md)补上独立监督、生产连接替换和当前轮次就绪。以上表格仍记录 P3 收口时的覆盖边界，不把后续结果改写为 P3 实测。下一项为 P4.2.2.3 监督恢复的运行时退出验收；默认后台装配和隔离/Outbox 尚未完成，worker/all 门禁继续保留。
