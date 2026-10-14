# 消费者参数与按角色生成 MQ 依赖计划（P3.5.3.2.1）

本小点补充消费者的有界参数、`RabbitMQConfig.Consumer()` 和 `Config.MQRuntime()`。这些构造器只返回经校验的依赖输入，不解析 DNS、不拨号、不请求管理 API、不建立订阅或启动 worker。当前业务程序的 api/worker/all 装配检查继续拒绝未完成角色，默认 legacy 不会启用 MQ。

## 消费者参数

在既有 RABBITMQ 节增加：

```yaml
RABBITMQ:
  consumerWorkers: 4
  consumerPrefetch: 16
  consumerOperationTimeout: 5s
```

| 字段 | 默认值 | 范围与含义 | 环境变量 |
| --- | --- | --- | --- |
| consumerWorkers | 4 | 持久化消费者 1—32 个独立 session，每 session 一个消费 channel | ICHAT_RABBITMQ_CONSUMERWORKERS |
| consumerPrefetch | 16 | 每订阅 1—256 条未确认消息，0 无限制值被拒绝 | ICHAT_RABBITMQ_CONSUMERPREFETCH |
| consumerOperationTimeout | 5s | 正时间字符串，不超过 60s，消费者核验/订阅初始化/读取等待/ACK/Reject 的 operation 上限 | ICHAT_RABBITMQ_CONSUMEROPERATIONTIMEOUT |

隔离审计消费组固定独立使用 **1 个 session**，不把 consumerWorkers 再用于隔离组；它使用相同 prefetch/消费者操作上限。worker/all 的未来消费者连接数为 consumerWorkers+1，配置上限 33；api 不生成消费连接配置。32 是配置准入上限，不代表测得的服务器吞吐量或生产容量。prefetch 是消息数量限制，不是任意消息体的字节内存承诺。

已有 connectTimeout/writeTimeout/closeTimeout/heartbeat 继续用于两类连接。发布者使用 publishTimeout，消费者独立使用 consumerOperationTimeout，二者都映射各自的 OperationTimeout，不会因为调整一个就改变另一个。消费者操作上限不代表数据库事务期限，也不设置 broker 的 consumer ACK timeout。consumerAckTimeout、Outbox 参数、自定义 CA 或三节点契约未在此实现。

所有新字段支持 YAML 完全省略时的显式环境覆盖、显式空值拒绝和严格类型检查，与 [MQ 应用配置](mq-application-config.md) 的来源/优先级一致。整数不接受浮点、布尔或 null；时间需带单位的字符串。未配置 MQ 连接时，文件中的 workers/prefetch/时间值仍须合法；旧配置省略它们可正常读取默认值。

## 纯构造结果

`RabbitMQConfig.Consumer()` 返回 `ConsumerConfig{URL,VHost,Management,Options,Workers,Prefetch}`。它校验直接用 Go 构造的输入，与 DialConsumer 复用无 I/O 的 ValidateConsumerOptions，避免应用与连接层的 URI/vhost/timeout/TLS 规则漂移。Management 是有界只读核验客户端的配置，没有发出请求。凭证全空返回 ErrRabbitMQNotConfigured，部分/无效配置失败并返回空结果，不填入默认账号。

`Config.MQRuntime()` 先调用 RuntimePlan 校验模式/新旧角色冲突，再只构造选中角色的 MQ 输入：

| 模式 | command publisher 配置 | consumer groups |
| --- | --- | --- |
| legacy | 无 | 无 |
| api | 1 份 PublisherConfig | 无 |
| worker | 无 | PersistQueue × consumerWorkers；QuarantineQueue × 1 |
| all | 1 份 PublisherConfig | 同 worker |

返回对象 `MQRuntimePlan` 只包含 Mode、可选 Publisher 和 Consumers 列表；每个 ConsumerGroup 带固定 Queue 和 ConsumerConfig。它不实现 API/Outbox 业务、readiness 或实际资源所有权。管理客户端配置可复用，未来每个消费实例仍须独立 DialConsumer；不能共享 publisher AMQP connection，也不能把多份管理客户端配置当作已经建立的连接。

```go
conf, err := config.Load()
if err != nil { return err }
plan, err := conf.MQRuntime()
if err != nil { return err }
// plan 中只是下一层连接构造器的输入；此处没有任何 TCP/AMQP 会话。
```

缺失必需连接配置会明确失败，all 的后一个依赖校验失败也只返回空计划，不泄漏半份可供错误启动的 publisher 计划。legacy 不构造 MQ 输入。直接 Go 配置只验证选中依赖所需参数，例如 worker 不构造命令 publisher；Load 的文件校验仍校验所有受支持字段，避免配置中的错误被忽略。

ConsumerConfig/MQRuntimePlan 的 `%v`、`%+v`、`%#v`（包括 consumer groups 的嵌套格式）隐藏凭证；错误只包含已知字段、角色描述及脱敏 sentinel。直接输出 URL/密码字段或把内部配置序列化到 HTTP 不属于这些 fmt 保护。

## 验证与剩余范围

```powershell
go test -mod=readonly -race ./config -run 'Test(MQRuntime|RabbitMQEnvironment)' -count=1 -v
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
```

测试覆盖消费者默认/上下界、全字段环境覆盖、重复加载、错误数值/类型/空覆盖/溢出、直接构造校验、角色独立选择、审计固定单实例、新旧模式冲突、必需依赖缺失/半计划拒绝、配置不变和凭证脱敏。AMQP/management 两个本机 listener 观察 Load、Consumer 和各角色 MQRuntime 执行后的零连接。测试不访问 Docker 或开发配置。

P3.5.3.2.1 消费者参数与纯 MQ 依赖计划、P3.5.3.2.2 [按角色准备 MQ 连接与失败清理](mq-runtime-resources.md) 已完成。调用后者才会实际联网；配置计划本身继续保持零 I/O。启动/恢复/关闭生命周期与完整角色能力仍在 P3.5.3.3；HTTP 202、业务提交后 ACK 和 Outbox 分别按 P3.6/P4/P5 接入。
