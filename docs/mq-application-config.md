# RabbitMQ 应用配置（P3.5.1）

P3.5.3.2.1 已追加 [消费者参数与按角色生成 MQ 依赖计划](mq-runtime-parameters.md)，提供 Consumer/MQRuntime 纯构造结果；当前仍未将 MQ 连接装配进业务主程序。

本小点提供 `config.Load()` 的配置入口，以及纯构造器 `Config.RABBITMQ.Publisher()`。两者不解析 DNS、不拨号、不请求管理 API、不启动后台任务。当前主程序仍使用旧消息链路；填写这些字段不会启动 MQ 或切换发送入口。[运行模式](mq-runtime-modes.md) 已有配置与启动检查，独立连接的应用装配、readiness 和 HTTP 202 仍待后续小点。

## 缺省与配置来源

旧配置不含 `RABBITMQ` 时仍可加载。默认 URL、管理 URL、管理用户名和管理密码均为空，vhost 为 `/ichat`，其余参数见下表。四个连接/凭证字段都为空时表示未配置，显式调用 `Publisher()` 返回 `ErrRabbitMQNotConfigured`。只填写其中一部分会使 `Load()` 失败，避免悄悄使用默认账号或开发凭证。

应用读取已有 Viper 配置文件，支持 `ICHAT_CONFIG_FILE`。RabbitMQ 每个已支持字段都能由 `ICHAT_RABBITMQ_` 环境变量覆盖，即使 YAML 完全省略该字段。字段名直接转大写，不在 camelCase 中插入下划线，例如 `managementURL` 对应 `ICHAT_RABBITMQ_MANAGEMENTURL`。优先级是显式环境变量（包括空字符串）、配置文件、默认值。显式空密码覆盖文件里的密码后会校验失败，不会回退到文件凭证。

应用不自动读取 Compose 的 `.env`。Compose / `mqadmin` 的 `RABBITMQ_APP_PASSWORD` 和 `RABBITMQ_ADMIN_PASSWORD` 与应用的 `ICHAT_RABBITMQ_*` 是各自独立的输入；需要显式配置。不要覆盖已有 `config/config.yml`；模板 [config.example.yml](../config/config.example.yml) 的 MQ 字段均为空，复制后可继续运行旧服务。

以下示例只含占位凭证，全部四项需共同填写：

```yaml
RABBITMQ:
  url: amqp://ichat_app:CHANGE_ME@127.0.0.1:5672/%2Fichat
  vhost: /ichat
  managementURL: http://127.0.0.1:15672
  managementUsername: ichat_admin
  managementPassword: CHANGE_ME_ADMIN
```

URI 的 `%2Fichat` 表示 vhost **`/ichat`**；URI `/ichat` 表示 **`ichat`**，二者不同。用户名、密码与 vhost 含 URI 特殊字符时必须正确百分号编码。AMQP 应用账号使用 `mqadmin` 初始化的 `ichat_app`，不能拿管理账号替代。现有 `ichat_app` 无管理标签，不能假定其能读取管理 API；示例管理字段使用现有本地管理账号，supervisor 对它只执行读取。后续生产部署应提供有相应管理读取权限的专用核验账号，不能将管理凭证暴露给 HTTP 客户端。

管理 endpoint 必须指向 AMQP 的同一 broker；配置阶段只能检查格式，无法证明远端身份或实际权限。正式连接仍按 [supervisor](mq-supervisor.md) 核验固定版本、quorum 单节点拓扑、有效参数与绑定。配置正确不表示 MQ 已就绪。

## 参数和边界

| 字段 | 默认值 | 校验与映射 |
| --- | --- | --- |
| `publisherSlots` | 8 | 整数 1—64，映射 publisher 有界 slots |
| `publishTimeout` | 5s | `Session.OperationTimeout`，涵盖 setup 和单次发布操作 |
| `consumerWorkers` | 4 | 持久化消费者 1—32 个独立 session，审计另固定 1 个 |
| `consumerPrefetch` | 16 | 每订阅 1—256 条未确认消息，拒绝无限制 0 |
| `consumerOperationTimeout` | 5s | 消费者独立 OperationTimeout，不改变发布者的 publishTimeout |
| `connectTimeout` | 5s | TCP/TLS/AMQP 握手期限 |
| `writeTimeout` | 3s | socket Write 期限 |
| `closeTimeout` | 3s | session / supervisor 关闭期限 |
| `heartbeat` | 2s | 1—60 秒，必须为整秒 |
| `retryMin` | 1s | 最低 1ms，退避初始 base，实际等待含抖动 |
| `retryMax` | 30s | 不小于 retryMin，最高 60s |

操作/连接/写/关闭期限必须大于 0 且不超过 60s。所有时间字段需要带单位的字符串，例如 `250ms`、`5s`；拒绝 YAML 数字和空字符串。连接初始 epoch 固定采用库默认值 1，随后由 supervisor 分配，不能通过应用配置重用旧 epoch。

AMQP URI 要求显式非空用户名与密码、合法端口、无 query/fragment、vhost 与配置一致。明文 `amqp` 仅允许 localhost / loopback；远端需 `amqps`。管理端点仅允许 loopback HTTP 或 HTTPS，不接受嵌入凭证、API 路径、query/fragment。本入口沿用库的系统 CA / TLS 校验，不提供跳过证书校验字段；自定义 CA 和集群契约需后续明确实现。

`RABBITMQ` 拒绝未知字段、错误类型和无效值，错误仅包含脱敏 sentinel 与已知字段名。未配置连接时，已有 timing / slots 字段仍须合法。`RabbitMQConfig` / `PublisherConfig` 的常用 fmt 格式（`%v`、`%+v`、`%#v`，含整个 Config 的嵌套格式）会隐藏 MQ 凭证；不要直接日志输出 `.URL` 或 `.ManagementPassword`。语法解析错误也不回显原始配置文本。其他旧配置字段的日志习惯不在本次 MQ 脱敏范围内。

MQ 配置已有 publisher 与 consumer 参数及按角色生成的纯依赖计划；broker ack timeout、Outbox 和消息入口切换参数尚未加入。运行模式字段位于独立 `RUNTIME` 节，有明确的未装配启动拒绝，不能填写后就假定新角色运行。

## 使用纯构造结果

```go
conf, err := config.Load()
if err != nil {
    return err
}
settings, err := conf.RABBITMQ.Publisher()
if err != nil {
    return err
}
// settings.URL / VHost / Management / Options 已校验；尚未联网。
// 后续装配层显式调用 rabbitmq.StartSupervisor(lifetimeCtx, ...)。
```

Load 使用独立实例，不修改旧全局 `config.Conf`；Publisher 也校验直接用 Go 构造的配置，失败返回空结果。配置专项测试覆盖旧配置兼容、默认值、环境变量全字段覆盖、重复加载、显式空值、错误类型/参数、格式化脱敏和两个本机 listener 上零连接。默认单元测试不读取开发 `.env` 或开发配置、不需要 Docker。
