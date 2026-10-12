# 本地 RabbitMQ 拓扑与可靠死信转发

P3.2 使用 Docker Compose 单节点 RabbitMQ `4.3.6` 和 P3.1 已验证的镜像 digest。Go 服务尚未接入 MQ；本小点提供显式初始化、只读检查和隔离故障测试。三节点副本与多数故障验收在后续 P8 实施。

## 启动和初始化

在已有 `.env` 中追加 `.env.example` 的 RabbitMQ 设置，填写两个非空且不同的密码：`RABBITMQ_ADMIN_PASSWORD`、`RABBITMQ_APP_PASSWORD`。不要用复制模板覆盖已有配置。端口缺省为 5672 和 15672，仅映射到 `127.0.0.1`。Compose 中的 `rabbitmq_data` 持久化 broker 数据；固定 hostname 保持节点身份稳定。

```powershell
docker compose up -d --wait
go run ./cmd/mqadmin init
go run ./cmd/mqadmin check
```

`mqadmin` 默认显式读取仓库 `.env`，不读取应用 `config/config.yml`。可用 `--env-file <路径>` 指定其他 dotenv 文件，或 `--env-file=""` 仅读取进程环境。四个 `RABBITMQ_*` 变量同名的进程环境设置优先；显式空值也覆盖文件值。端口须为 1—65535 的整数，初始化要求两个密码不同。报错非零退出，不打印凭证、AMQP URL 或管理响应正文。

`init` 先只读检查 broker、feature flags 和全部既有对象，再创建没有管理标签的 `ichat_app`，权限限于 `/ichat` 的 `^ichat\.` 资源。随后用应用账号声明并复核拓扑。已经正确配置的账号不重设密码或扩大权限；已有管理标签、跨 vhost/过宽权限直接失败。部分账号初始化只允许补齐空权限；错误的已有拓扑不修改。AMQP 认证验证当前文件中的应用密码，修改 `.env` 不会静默更新 broker 密码。

`check` 仅 GET 管理 API，检查拓扑，不创建账号或队列、不改变策略、不消费消息，不需要应用密码。管理账号为 `ichat_admin`；应用使用 `ichat_app`。管理凭证不交给消息发布/消费流程。vhost 包含前导 `/`，应用 URL 示例为 `amqp://ichat_app:CHANGE_ME@127.0.0.1:5672/%2Fichat`；业务配置和连接生命周期在 P3.5 接入。

Docker healthcheck 验证节点存活、运行和本地告警；拓扑由管理命令检查。停止可用 `docker compose stop`。保留数据卷后重启可再次执行 `init`；不匹配时需排查并制定迁移方案，命令不会自动删除队列。Compose 的默认账号/密码只用于空数据卷首次引导，不能通过修改 `.env` 轮换既有凭证。

## 固定契约

声明与核验的唯一代码来源是 `messaging/rabbitmq/topology.go`，每次生成独立参数 map。顺序为隔离 exchange → 隔离 queue → 隔离 binding → 主 exchange → 主 queue → 主 binding，所有操作等待 broker 响应。

| 对象 | 名称/参数 |
| --- | --- |
| vhost | 本地 `/ichat`；测试为本轮独立 vhost |
| 主 exchange | `ichat.commands.v1`，durable/direct，非 auto-delete/internal |
| 主 queue | `ichat.message.persist.v1`，durable/quorum，非 exclusive/auto-delete |
| 主 binding | `message.submit.v1` → 主 queue，无额外绑定 |
| 隔离 exchange | `ichat.quarantine.v1`，durable/direct，非 auto-delete/internal |
| 隔离 queue | `ichat.message.quarantine.v1`，durable/quorum，非 exclusive/auto-delete |
| 隔离 binding | `message.quarantine.v1` → 隔离 queue，无额外绑定 |
| 副本 | 两条队列 `x-quorum-initial-group-size=1`；实际 members/online 各一且一致、队列 running |
| 保留 | 两条队列无 message TTL、queue expiry 或自动删除 |
| 容量 | 两条队列 `x-max-length-bytes=67108864`、`x-overflow=reject-publish` |
| 主失败投递上限 | `x-delivery-limit=50` |
| 主死信 | `x-dead-letter-strategy=at-least-once`、exchange=`ichat.quarantine.v1`、routing key=`message.quarantine.v1` |
| 隔离失败投递上限 | `x-delivery-limit=-1`，避免审计处理反复失败时被默认上限丢弃；审计 worker 退避与人工处理在 P4 实施 |

64 MiB 是本地起点，不是容量测量或严格磁盘上限。Quorum 的 reject-publish 可短暂超出限制；须同时监控磁盘、内存、积压与被拒发布。目标满时，已接收消息仍由源队列保留转移责任。[Quorum 容量与死信语义](https://www.rabbitmq.com/docs/quorum-queues)

本地专用 vhost 不允许 user/operator policies；检查既有 arguments 和 effective policy，拒绝策略覆盖。Quorum 的普通 overflow policy 也可能覆盖声明参数，不能只依靠 AMQP 重声明判断有效配置。缺失对象可初始化，已有 classic、错误参数、TTL、策略、额外路由或副本不符均失败。无 delete、purge、修改策略或自动启用 feature flags 的修复路径。[策略优先级](https://www.rabbitmq.com/docs/policies)

版本必须为 `4.3.6`，`quorum_queue` 与 `stream_queue` flags 必须 enabled；缺失/关闭时报错。新节点通常已启用稳定/required flags，仍检查实际状态。[Feature flags](https://www.rabbitmq.com/docs/feature-flags)

## 恢复边界

源队列只在隔离目标确认接收后移除死信；绑定缺失、目标拒绝或不可用时保留责任，broker 记录 warning 并周期重试，可产生重复转发。主队列发布仍须使用持久消息，生产 mandatory/confirm 判定在 P3.3 实施。[可靠死信前提](https://www.rabbitmq.com/docs/quorum-queues)

固定版本的 `dead_letter_worker_publisher_confirm_timeout` 缺省为 **180000 ms**。恢复路由不保证立即转发，测试保留默认值并允许一个重试周期及调度余量；不能把超过 90 秒误判为丢失。[4.3.6 默认配置](https://github.com/rabbitmq/rabbitmq-server/blob/v4.3.6/deps/rabbit/Makefile)、[死信重试实现](https://github.com/rabbitmq/rabbitmq-server/blob/v4.3.6/deps/rabbit/src/rabbit_fifo_dlx_worker.erl)

RabbitMQ 4.3 的 `basic.nack(requeue=true)` 不增加失败 delivery-count，`basic.reject(requeue=true)` 会增加。超过 50 次真实失败后转入隔离队列；后续消费者须结合故障分类与退避，不能依赖无限 nack 循环。[失败投递计数](https://www.rabbitmq.com/docs/quorum-queues#poison-message-handling)

`VerifyLocal` 是当时的拓扑检查，不是持续服务 readiness。`TopologyChannel` 调用者负责连接/I/O 截止时间。管理命令设置 45 秒操作 deadline、3 秒关闭 deadline；HTTP 每请求最多 5 秒、响应最多 1 MiB、禁用代理与重定向。线上 supervisor、epoch、持续检查和告警接入在 P3.4/P3.5 及后续阶段实施。单节点不承诺节点容灾。

## 验证

```powershell
go test -mod=readonly -race ./messaging/rabbitmq ./cmd/mqadmin -count=1
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQTopologyIntegration$' -count=1 -v -timeout=10m
```

集成测试沿用 P3.1 `StartWithRabbitMQ`，正式对象名只用于本轮独立 broker/vhost；经典队列冲突和管理命令另用该临时 broker 中的新 vhost。全部随有归属标签的容器/卷清理，开发 `.env`、已有容器和卷不参与测试。

覆盖实际单成员 quorum、幂等声明、persistent 原始正文/身份保留、reject 隔离、delivery-limit 超限、隔离反复失败保留、DLX 路由失效时源队列计数及恢复转发、operator policy 失败、已有 classic 队列内容保留；实际子进程执行两次 `mqadmin init` 和一次只读 `check`。尚未覆盖 broker 重启、容量饱和、三节点多数故障、生产 accepted、隔离审计或受控重放。
