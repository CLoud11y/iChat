# 运行模式配置与启动检查（P3.5.3.1）

本小点提供严格的 `RUNTIME.mode` 配置和无 I/O 的 `Config.RuntimePlan()`。P3.6.2 已为显式 api 模式装配[确认式接受 API](api-runtime-lifecycle.md)；默认仍为 legacy，worker/all 缺失后台角色，继续在信号和外部连接前非零退出。不会把新模式默默当成旧 Redis 服务，也不会启动只完成部分角色的进程。

## 配置与兼容

```yaml
RUNTIME:
  mode: legacy
```

未配置 RUNTIME/mode（包括空 RUNTIME 节）时默认为 legacy，旧配置无需修改，模板也显式使用 legacy。`ICHAT_RUNTIME_MODE` 可覆盖 YAML，即使该键在文件中省略；优先级为显式环境变量、文件、默认值。显式空环境变量会报错，不会回退到文件的模式。

接受精确的小写字符串 `legacy`、`api`、`worker`、`all`。不自动 trim 或转换枚举大小写，拒绝空字符串、未知值、非字符串 mode、未知 RUNTIME 字段和有非空值的非对象节；Viper 的配置键名仍不区分大小写。错误不回显原始配置值，非法输入使用 `ErrRuntimeConfig` 并返回空 Config。

`Config{}` 或代码中未指定 Runtime.Mode 的配置仍由 RuntimePlan 解释为 legacy，保持现有注入构造器兼容；读取文件中的显式空/null mode 会拒绝。Load 使用独立实例，不写 `config.Conf`；重复加载不会保留此前模式。

## 角色计划

下表描述 RuntimePlan 的目标角色，不表示这些服务已经装配或就绪。

| 模式 | API/HTTP/WS | 接受命令的 RabbitMQ publisher | 持久化 consumer | 隔离审计 consumer | Outbox | 旧 Redis worker |
| --- | --- | --- | --- | --- | --- | --- |
| legacy | 是（旧入口） | 否 | 否 | 否 | 否 | 按 LEGACY_WORKER.enabled |
| api | 是 | 是 | 否 | 否 | 否 | 否 |
| worker | 否 | 否 | 是 | 是 | 是 | 否 |
| all | 是 | 是 | 是 | 是 | 是 | 否 |

新模式与 `LEGACY_WORKER.enabled=true` 互斥；Load 与直接用 Go 构造后的 RuntimePlan 都拒绝混用。legacy 保留原 worker 开关、排他注册及旧 bootstrap 生命周期，本小点不重置 Redis 注册或修改数据。全局状态在启动检查失败时不改变。

RuntimePlan 只返回 Mode/API/Publisher/PersistenceConsumer/QuarantineConsumer/Outbox/LegacyWorker 的值，不读取传输凭证、不解析主机、不建立连接或启动任务，也不判断 schema、依赖 readiness 或数据库在线状态。Publisher 指接受入口的命令发布角色，不代表后续 worker 隔离转移所需的全部 AMQP 写能力。MQ 连接校验仍使用 [应用 MQ 配置](mq-application-config.md) 的纯构造器；实际资源构造与角色能力核验由后续装配层负责。

## 当前启动边界

bootstrap 顺序为：检查必要 factory 存在 → 显式加载配置 → 解析角色并检查新业务构造器 → 模式执行器兜底检查 → 现有 legacy 依赖初始化。未装配的新模式返回可由 errors.Is 判断的 `ErrRuntimeNotAssembled`，附已经校验的模式及全部缺失角色；errors.As 可读取 RuntimeAssemblyError。非法模式/混用返回配置错误。检查在旧全局依赖绑定、legacy cron、storage health 和 HTTP 服务启动之前完成，详情见 [角色装配检查](runtime-assembly.md)。

业务主程序当前缺失后台角色的错误示例：

```text
resolve runtime assembly: requested runtime mode is not assembled yet: worker (missing roles: persistence_consumer, quarantine_consumer, outbox)
```

配置加载/角色计划与启动支持分开：纯配置加载可以读取合法的新模式，便于迁移命令继续使用自己的依赖边界；业务主程序会执行装配检查。新增 mode 不会要求独立迁移命令连接 RabbitMQ。mqadmin 仍读取自己显式提供的 dotenv/env，不使用应用运行模式。

不要通过改默认值为 all 或在 guard 后运行旧 router 来“支持”新模式。后续实现必须为选中角色提供正确的新路径和生命周期；缺失角色应明确失败，不能静默降级。HTTP 202 按 P3.6、消费事务提交后 ACK 按 P4、Outbox 按 P5 接入；角色声明不提前宣称这些能力可用。

## 验证和拆分

```powershell
go test -mod=readonly -race ./config . -run 'Test(Runtime|BootstrapRuntime)' -count=1 -v
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
```

模式测试覆盖默认/文件/省略键环境覆盖、显式空值、错误类型/枚举/未知字段、新旧 worker 冲突、纯角色边界、重复加载与直接零值配置。bootstrap 注入工厂记录实际调用，证明新模式/非法模式/混用只调用配置加载，不触发 DB、Redis、legacy worker 或 HTTP，也不修改既有全局状态；原 legacy 启动/退出与 MySQL 离线测试继续执行。没有 Docker、开发配置或真实数据操作。

P3.5.3.1 模式配置/角色计划/启动检查、P3.5.3.2.1 [消费者参数与纯 MQ 依赖计划](mq-runtime-parameters.md)、P3.5.3.2.2 [受控 MQ 连接创建与失败清理](mq-runtime-resources.md)、P3.5.3.3.1 [MQ 托管生命周期](mq-lifecycle.md)、P3.5.3.3.2.1 [角色构造器与服务实例检查](runtime-assembly.md)、P3.5.3.3.2.2 [服务构造/激活/退出编排](runtime-execution.md)、P3.5.3.3.3.1 [进程入口分流与现代信号](runtime-process-entry.md)、P3.5.3.3.3.2.1 [legacy HTTP/WebSocket 适配器](legacy-http-lifecycle.md)、P3.5.3.3.3.2.2.1 [legacy cron 生命周期](legacy-worker-lifecycle.md)、P3.5.3.3.3.2.2.2 [legacy 统一生命周期与信号](legacy-runtime-lifecycle.md) 已完成。默认 legacy 入口已统一停止 HTTP/worker/健康探测并在任务结束后关闭存储；主程序现代分支已接到 RunRuntime，显式 api 模式已装配[生产 API](api-runtime-lifecycle.md)，后台业务角色仍缺失。P3.5.4.1 [publisher 周期拓扑复核](mq-topology-monitor.md)已完成，P3.5.4.2 [接受就绪汇总与健康接线](runtime-readiness.md)也已完成装配基础，P3.6 新 DTO/202 接受及 API 生命周期已完成，下一项为 P3 总体验收核对；consumer 自动恢复与后台业务处理器仍待后续阶段。
