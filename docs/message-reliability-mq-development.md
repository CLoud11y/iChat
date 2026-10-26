# iChat 消息可靠性改造开发文档：RabbitMQ 接入与端到端恢复

> 状态：待实施的开发规范；不是已完成的功能说明。
>
> 编写日期：2026-09-14。源码基线：`cb1c8bc6ff1f0c5d4f0461ed53c74eded72a98bb`。
>
> 本文供接手开发的工程师或 AI agent 使用。先阅读第 1、2、3、12 节，再按阶段实施；不得只替换 `RPUSH` 就宣称可靠性改造完成。本文编写过程只新增文档，没有安装软件、清空数据或修改业务代码。

## 目录

- [1. 已确认决策与范围](#1-已确认决策与范围)
- [2. 成功语义与可靠性约束](#2-成功语义与可靠性约束)
- [3. 当前实现与必须先修复的问题](#3-当前实现与必须先修复的问题)
- [4. 目标架构和启动依赖](#4-目标架构和启动依赖)
- [5. 消息身份、幂等与数据模型](#5-消息身份幂等与数据模型)
- [6. RabbitMQ 拓扑和生产者](#6-rabbitmq-拓扑和生产者)
- [7. 消费事务、重试与隔离](#7-消费事务重试与隔离)
- [8. Outbox、通知和缓存](#8-outbox通知和缓存)
- [9. HTTP 与 WebSocket 协议](#9-http-与-websocket-协议)
- [10. 客户端状态机与补拉算法](#10-客户端状态机与补拉算法)
- [11. 配置、监控和容量边界](#11-配置监控和容量边界)
- [12. 分阶段开发路径](#12-分阶段开发路径)
- [13. 自动化测试与故障验收](#13-自动化测试与故障验收)
- [14. 开发环境切换、发布和回滚](#14-开发环境切换发布和回滚)
- [15. 接手 agent 检查清单](#15-接手-agent-检查清单)
- [16. 官方资料与版本核查](#16-官方资料与版本核查)

## 1. 已确认决策与范围

### 1.1 用户已经确认的决策

以下决策已在编写文档前向用户确认，后续 agent 不需要再次询问同一问题。

| 决策 | 确认结果 |
| --- | --- |
| MQ 产品 | RabbitMQ |
| 发送请求的接受边界 | 正确路由并收到 MQ publisher confirm 后，返回 `accepted`；MySQL 异步落库 |
| 业务校验时机 | 允许先接收命令，消费者再校验好友关系、群成员和幂等冲突；结果可以是 `persisted` 或 `rejected` |
| 文档与开发覆盖面 | 服务端可靠接收、落库、通知，以及客户端 ACK、重试、重连和补拉 |
| RabbitMQ 部署 | 本地 Compose 单节点；另提供生产三节点 quorum 集群配置和故障验收方案 |
| 现有数据和接口 | 开发环境数据允许清空，允许直接修改接口；不要求旧协议兼容或不停服迁移 |
| 首版会话 | 单用户单活跃会话，按用户记录收件进度；多设备独立进度不在首版范围 |
| 消息权限 | 私聊双方须为好友；群聊发送者须为当前成员；非成员不得读群历史 |
| 群历史范围 | 当前群成员可以读取全部群历史；离群或群被删除后不再有读取权限 |
| Go 工具链 | 允许升级到实施时受支持的 Go 稳定版本，并固定匹配的客户端版本 |

权限按照**消费者处理事务中的数据库状态**判断，不按照客户端声明的时间判断。例如发送命令已入队，但处理前发送者已离群，该命令可以被拒绝。已有终态的同键重试返回原结果，不因之后权限改变重新执行。

### 1.2 本文采用的实现设计

- 首版一个 API/WebSocket 实例，落库消费者和 Outbox worker 可按需独立运行。生产 RabbitMQ 三节点不等于应用已支持多 API 节点上的全局单会话。
- MQ 保存待处理命令；MySQL 保存业务终态、消息历史、同步进度和通知任务；Redis 保存鉴权黑名单与可丢失的通知/缓存。
- 消费者采用“至少一次投递 + 数据库幂等事务 + 提交后 ACK”。
- 业务消息落库后才触发内容推送；数据库不可用期间，界面保留 `accepted` 等待状态。
- MySQL 事务中同时写消息和通知 Outbox，解决“入库后、发通知前退出”的窗口。
- 首版按数据库提交顺序分配会话内 `seq`，不承诺多个客户端请求的全局顺序或 MQ 原始入队顺序。
- 首版支持现有文本、图片、音频的消息记录；附件二进制传输沿用独立文件接口，MQ 只携带消息字段/资源引用。
- 已读回执、多设备独立 ACK、跨地域容灾、超大群优化、消息编辑/撤回、全站去 Redis 依赖均不在首版范围。

如要改变接受语义、权限时机、群历史范围、数据保留或单会话范围，必须先向用户确认。并发数、批量大小等有界参数可按测试调整，并记录证据。

### 1.3 版本基线

编写时本机 `go version` 为 `go1.23.3 windows/amd64`，仓库 `go.mod` 为 `go 1.21.3`，两者不能混为一谈。

| 项目 | 文档核查基线 | 实施要求 |
| --- | --- | --- |
| Go | 官方稳定下载列出 `1.27.1` | 本次可从此版本验证；执行时复核支持周期，固定工具链并记录实际版本 |
| RabbitMQ | 原候选 `4.3.5` | P3.1 已核验并固定 `4.3.6-management` 镜像 digest，真实测试版本为 4.3.6；生产部署前复核补丁与支持期 |
| Go AMQP 客户端 | 原候选 `github.com/rabbitmq/amqp091-go v1.14.0` | P3.1 已核验并固定 v1.15.0；后续通知顺序设计以已下载固定版本源码为依据 |
| MySQL / Redis | 现有 Compose 使用 MySQL 8.0、Redis 7 | 首版复用；测试报告记录具体 patch/image digest，不在此任务中附带升级数据库大版本 |

RabbitMQ 4.3.6 / amqp091-go v1.15.0 已在 P3.1 隔离 Docker 环境验证基本 AMQP、quorum probe、权限与通知顺序前提，版本、镜像 digest、模块校验和与覆盖边界见 [MQ 测试环境](mq-test-environment.md)。正式拓扑/可靠 DLX、有界 publisher、消费传输、重连与接受 API 已完成 P3 范围验收，见 [P3 验收对照](mq-reliability-acceptance.md)；业务 consumer 和端到端恢复仍归后续阶段。客户端 v1.15.0 的 `go.mod` 最低版本为 1.20；项目仍使用 Go 1.27.1，不能误称 AMQP 库要求 Go 1.27。[Go 下载](https://go.dev/dl/?mode=json)、[RabbitMQ 发布信息](https://www.rabbitmq.com/release-information)、[客户端 go.mod](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/go.mod)

## 2. 成功语义与可靠性约束

### 2.1 四种确认必须分开

| 名称 | 谁确认 | 可以证明什么 | 不能证明什么 |
| --- | --- | --- | --- |
| Publisher confirm | RabbitMQ → API | 本次发布已按队列配置被可靠接收，且应用已排除无法路由的 return | 业务权限有效、MySQL 已提交、接收者已收到 |
| Consumer ACK | 落库 worker → RabbitMQ | 消息/业务拒绝结果及对应 Outbox 已提交，或者责任已安全转移到隔离存储 | 接收客户端已经收到 |
| Delivery ACK | 接收客户端 → 服务端 | 当前用户客户端声明已保存连续范围的消息 | 用户已经阅读、其他设备也已收到 |
| Read receipt | 用户阅读行为 | 首版不实现 | 不得把上面任何确认显示为“已读” |

### 2.2 接受状态与终态

- `accepted`：MQ 已接收命令；最终可能成功落库或因业务规则被拒绝。
- `persisted`：消息表与命令结果已在同一事务提交。此后状态不能倒退为 `accepted` 或被重复消费改为拒绝。
- `rejected`：消费者已持久化业务拒绝原因，例如 `NOT_FRIENDS`、`NOT_GROUP_MEMBER`、`TARGET_NOT_FOUND`、`SENDER_NOT_FOUND`、`SELF_SEND_NOT_ALLOWED`。
- `pending_or_unknown`：状态查询未找到终态；可能仍在 MQ 中、正在重试，也可能某次发布没有成功。**查不到数据库记录不能证明 MQ 中没有命令。**
- `quarantined`：命令因不可处理的技术问题进入可审计隔离区，需要修复/重放；它不是业务成功，也不是可直接丢弃的失败。
- `publish_unknown`：HTTP 请求结果不确定；网络写入已经开始但未获得完整确认。客户端使用原幂等键及原内容查询/重试。

`ID_CONFLICT` 的定义：相同 `(sender_id, client_message_id)` 对应不同的规范化内容。先提交的规范记录保持不变，后续不同内容请求得到冲突结果。异步入口不承诺立即发现所有冲突。

### 2.3 必须始终成立的约束

1. 未获得完整 publisher confirm 判定，不返回 `accepted`。
2. 一条有效逻辑消息最多有一条 `message` 行、一个会话 `seq` 和一组固定身份的 Outbox 事件。
3. 数据库提交之前不对原 MQ 消息 ACK；超时不能据此假定事务未提交。
4. 同一逻辑消息的重试不会覆盖已经保存的内容，不会重复推进 `seq`。
5. 队列满、磁盘告警、重试超限、死信转移失败都不得静默丢弃已接收命令。
6. 通知和 WebSocket 可以重复或遗漏，客户端通过去重与完整补拉收敛。
7. 同步游标只覆盖已连续保存的范围，不取“当前看到的最大消息编号”。
8. 已接收命令的恢复责任，只能在 MQ、MySQL 终态或持久化隔离记录之间明确交接。
9. 当前阶段未执行完整故障验收前，不承诺端到端 exactly-once 或无条件零丢失。

保障以指定故障模型为边界：三副本 quorum 多数仍可恢复、MySQL 提交按所需刷盘策略落盘、持久卷保留、无破坏性管理员删除。多数副本磁盘同时损毁和跨地域灾难不属于首版保障。

## 3. 当前实现与必须先修复的问题

### 3.1 代码地图

| 文件/函数 | 当前职责 | 改造关注点 |
| --- | --- | --- |
| [service/chat.go](../service/chat.go)：`SendMsg` | HTTP 发送、调用 `PublishAndSave` | 绑定鉴权 UID、输入校验、发布确认、接受结果 |
| [database/msgManager.go](../database/msgManager.go)：`SaveMsg` / `PublishAndSave` | Redis 缓存、脏队列、Pub/Sub | 先修旧窗口，最终移除作为新消息接受路径的职责 |
| 同文件：`GetAllDirtyMsgs` / `SaveMsgs2DB` / `RemDirtyMsgs` | List 读取、批量落库、裁剪 | 幂等、准确确认、故障测试；最终退役 |
| [scheduledTasks/syncMsg.go](../scheduledTasks/syncMsg.go)、[main.go](../main.go) | 每 60 秒落库 | 去掉吞错、防重叠；切换后停用旧 cron |
| [service/messages.go](../service/messages.go)、`LoadMsgs` | 缓存历史分页 | 以 MySQL 完整数据和会话 seq 查询，补权限/媒体类型 |
| `Chat` / `recvProc` / `websocketWriteProc` / `handleCtrlMsg` | WebSocket 会话与收发 | 单 writer、订阅释放、ACK、定期同步、取消 |
| [models/message.go](../models/message.go) | 消息结构、客户端字段转换 | 稳定身份、服务端时间、seq、不可变内容 |
| [utils/initMysql.go](../utils/initMysql.go)、[utils/initRedis.go](../utils/initRedis.go) | `init()` 中建立外部连接 | 显式构造、依赖隔离、可测试启动流程 |
| [utils/token.go](../utils/token.go)、[middlewares/jwtAuth.go](../middlewares/jwtAuth.go) | JWT 与 Redis 黑名单 | Redis 故障与 key 不存在分开处理，失败时不放行 |
| [database/groupManager.go](../database/groupManager.go)、[database/relationManager.go](../database/relationManager.go) | 群与好友关系 | 权限事务、群删除/成员变化和并发读取的一致性 |
| [utils/resp.go](../utils/resp.go)、[router/app.go](../router/app.go) | HTTP 包装与路由 | 202、409、503 和新状态契约 |
| [cmd/loadtest/main.go](../cmd/loadtest/main.go) | 正常流量压测 | 增加状态、幂等、ACK、重连、数据库对账 |

### 3.2 已确定的问题及处理要求

| 编号 | 源码事实 | 修复要求 |
| --- | --- | --- |
| L1 | `RPUSH` 错误被忽略，且与 `ZADD` 分离 | 接受记录优先，显式处理入队结果；缓存失败不掩盖持久处理责任 |
| L2 | `LLEN(...).Val()` 吞错误；`LRANGE 0,size` 多包含一个下标 | 有界读取实际批次，返回实际条数和错误 |
| L3 | MySQL 提交后 `LTRIM` 失败会重复插入 | 同键同内容为幂等成功，异内容隔离；不得简单吞掉所有 SQL 错误 |
| L4 | 多实例/cron 重叠读取并按旧长度 trim | 单消费者约束和批次头部原子核对，禁止不核对就裁剪全局队列 |
| L5 | 一个坏 JSON 或 SQL 错误阻塞全批 | 只确认已经提交/隔离的连续前缀，保留原始坏记录与原因 |
| L6 | Pub/Sub 失败只删除缓存，脏消息仍会落库 | 接受成功后通知失败不撤销消息；推送结果不作为持久化回滚依据 |
| L7 | `fromUser.id` 直接作为发送者 | 新旧过渡入口都使用鉴权 UID，不能相信请求体身份 |
| L8 | 缓存 key 存在就不回源 | 增加“缓存仅含新消息，旧历史在库”的回归；可靠补拉直接查 MySQL |
| L9 | 群历史无成员校验，媒体响应硬编码为 text | 使用当前群权限与实际媒体类型 |
| L10 | PubSub 句柄丢失，channel 关闭未检查 `ok` | 可关闭订阅、就绪握手、无 nil 解引用、连接退出释放 goroutine |
| L11 | 测试隐式连接真实服务并修改固定用户数据 | 默认单元测试无外部依赖，集成资源隔离，错误必须可断言 |
| L12 | 外部依赖 `init()` 失败直接 panic，迁移错误未处理 | 明确 bootstrap 和 migration 错误，数据库离线不阻止接收入口进程启动 |

补充事实：GORM 默认约定会将 `Message.Id` 识别为主键，不能因为没有 `primaryKey` tag 就声称没有主键；实际旧库结构仍需核实。默认 GORM 配置下 `CreateInBatches` 的数据库事务不能覆盖 Redis `LTRIM`，但也不能推断普通批次错误必然导致部分小批次已经提交。

Compose 已开启 Redis AOF 和数据卷。旧链路每 60 秒落库不等于 Go 进程退出必丢 60 秒数据；Redis 持久化和磁盘损坏有另外的故障边界。[Redis 持久化说明](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)

### 3.3 旧链路修复的具体边界

此阶段用于建立回归和可审查的修复，不能变成长期维护两套接受链路。

1. 先完成测试隔离，再为 L1—L12 添加能够失败的回归。
2. 旧 `SaveMsg` 改成检查 `RPUSH` 的接受结果，再尝试缓存和通知。入队后缓存/通知失败只记录并等待恢复，不返回“确定未接受”。Redis 写入开始后的超时同样属于不确定结果。
3. 旧 worker 固定只有一个启用实例，配置显式开启；cron 使用防重入，启动时禁止多个 legacy worker。不要仅靠进程内 mutex 宣称跨实例安全。
4. 使用 `LRANGE 0,127` 等有界读取，不以预先 `LLEN` 的结果确认消息。
5. 数据库写入或隔离成功后，通过 Redis Lua 比对当前队首原始记录与本批待确认前缀完全一致，再 `LTRIM` 实际前缀长度；不一致则不裁剪，重新读取。比较与裁剪必须在同一次 Redis 原子执行中完成。
6. DB 提交与 Redis 确认之间的崩溃，由数据库幂等解决；Lua 不承担跨系统事务功能，也没有通用业务回滚能力。
7. `SyncMsg` 返回显式错误，由调度层记录；测试不能依赖内部 recover 后的日志判断成功。
8. 新 MQ 路径启用后删除或禁用 legacy 入口与 cron；旧回归可转为仓储/确认抽象的回归，不保留生产双写。

用户允许重置开发数据，因此不需要实现旧 JSON/ZSET 的长期兼容迁移器。若实施时发现必须保留的数据，先确认范围，不能自行假定可删除。

## 4. 目标架构和启动依赖

### 4.1 数据流

```mermaid
flowchart TD
    C[发送客户端：稳定 clientMessageId] --> A[API：身份与格式校验]
    A --> Q[RabbitMQ 持久化命令队列]
    Q -->|confirm 且没有 return| R[HTTP 202 accepted]
    Q --> W[落库消费者]
    W --> T[MySQL 事务：命令终态 / 消息 / seq / outbox]
    T -->|提交后| K[Consumer ACK]
    T --> O[Outbox worker]
    O --> P[Redis Pub/Sub 通知]
    P --> G[WebSocket 网关]
    G --> D[接收客户端]
    D -->|补拉 / ACK| S[同步接口]
    S --> M[(MySQL 历史与进度)]
    T --> M
    W -->|技术坏消息| X[可靠隔离队列与审计记录]
```

MQ 只承载少量固定队列中的命令，不按用户建立 quorum queue。用户离线不会阻塞落库消费者，MQ consumer ACK 不等待客户端在线。

### 4.2 进程职责

建议提供 `all`、`api`、`worker` 三种运行模式，复用相同构造器：

- `all`：本地一进程运行 API、持久化 consumer、Outbox 与隔离记录 worker。
- `api`：HTTP/WebSocket，使用 publisher 和鉴权依赖；数据库相关接口通过仓储可用性返回明确错误。
- `worker`：持久化 consumer、Outbox 与隔离记录 worker；可多实例，依赖数据库幂等和任务领取。

首版只运行一个 API/WebSocket 实例。扩展到多个 API 实例前，必须另做全局会话所有权、踢旧连接与 ACK fencing，不能直接扩容后继续声称单用户单会话。

### 4.3 初始化与健康检查

- 从 `utils` 和各 manager 的 `init()` 移出配置读取、网络连接、迁移与 worker 启动；纯函数测试导入包不得触发外部 I/O。
- 显式注入 `MessageRepository`、`Publisher`、`Authorizer`、`Clock`、`Notifier` 等依赖；不要把逻辑继续绑在可变全局指针上。
- 数据库迁移独立命令执行，任何失败必须非零退出。业务进程不自动执行破坏性 schema 变更。
- MySQL 离线时 API 进程仍可启动；仓储初始化/重连在后台重试。不得构造 nil DB 后让查询请求 panic。
- 区分 `/health/live`、`/health/ready/accept`、`/health/ready/storage`。接受就绪依赖 RabbitMQ 确认通路和 Redis 鉴权；存储就绪依赖 MySQL。
- Redis 黑名单查询仅 `redis.Nil` 表示未封禁；其他 Redis 错误返回依赖不可用，不能故障放行。
- 生产者与消费者使用独立 AMQP 连接，避免发布背压连带阻塞消费确认。
- 优雅退出：停止接单/领取 → 限时等待在途事务 → 已提交的正常 ACK → 未完成的保留或关闭 channel 交给重投 → 关闭订阅与连接。

### 4.4 数据库故障期间的服务边界

| 功能 | MySQL 不可用时 |
| --- | --- |
| 已有有效 JWT 的格式合法发送请求 | RabbitMQ、Redis 鉴权正常且容量允许时，可返回 accepted |
| 好友/群权限及最终幂等判断 | 延后到 consumer 恢复后执行 |
| 注册、登录、关系变更、历史和状态查询、持久化 ACK | 明确返回存储依赖不可用；不能伪造成功 |
| 新消息内容推送 | 等待落库；现有连接可维持，但不承诺群权限查询可用 |

MQ 可用不代表全站可用。用户权限在排队期间发生变化时，最终结果以消费事务为准。

## 5. 消息身份、幂等与数据模型

### 5.1 身份规则

| 字段 | 规则 |
| --- | --- |
| `clientMessageId` | 客户端首次发送生成 UUID 字符串，规范化为小写标准格式；一次逻辑发送的所有重试保持不变 |
| `senderId` | 仅从服务端鉴权上下文取得，不接受请求体覆盖 |
| `commandId` | 服务端由版本前缀、senderId、clientMessageId 确定性计算 SHA-256，64 位小写 hex；分隔/编码必须无歧义 |
| `messageId` | 成功落库时等于 commandId；被拒绝命令不存在对应 message 行 |
| `payloadHash` | 对规范化不可变业务字段计算 SHA-256；含 schemaVersion、sender、目标/类型、media、content，不含重试次数、服务端时间或 traceId |
| `publishAttemptId` | 每一次物理发布生成独立 UUID，用于 confirm/return 和日志关联；不参与业务幂等 |
| `conversationId` | 私聊 `p:<较小用户ID>:<较大用户ID>`；群聊 `g:<groupID>`，服务端生成并校验 |
| `seq` | MySQL 事务中分配的会话连续序号；JSON 中使用十进制字符串，避免 JavaScript 大整数精度问题 |

规范化由共享 Go 代码完成，API 和 consumer 都重新计算并校验摘要。明确字符内容按原始 UTF-8 保存，不自动 trim 或做 Unicode 替换；枚举、目标 ID、UUID 使用规定的规范格式。客户端发送时间只作为可选元数据，不决定顺序，也不参与客户端重试时的业务内容变更。

P2.1 已在独立 `messaging` 包实现上述纯协议能力，尚未接入旧入口。具体 DTO/解码边界、UUID/ID/时间格式、1–1024 字符和 16384 字节限制、长度前缀 hash 编码与固定测试向量见 [消息协议 v1](message-protocol-v1.md)。后续 API/consumer 必须调用共享规范化与校验函数，不对原始 JSON 文本分别求摘要。

### 5.2 必要表结构

以下是存储契约，P2.2 已通过版本化 migration 建立六张新版消息表及约束；业务入口的切换仍待 P7。不要只依赖 `AutoMigrate`。文本 ID 和 hash 使用区分大小写、确定长度的 ASCII/binary 语义，业务 content 使用 utf8mb4。

| 表 | 关键字段 | 必须的约束/索引 |
| --- | --- | --- |
| `message_command` | sender_id、client_message_id、command_id、payload_hash、state、reason_code、message_id、conversation_id、created_at、completed_at | PK(sender_id, client_message_id)；UNIQUE(command_id)；state 只持久保存 persisted/rejected 终态 |
| `message` | id、sender_id、receiver_id、type、media、content、conversation_id、seq、client_sent_at、server_created_at | PK(id)；UNIQUE(conversation_id, seq)；历史查询索引(conversation_id, seq) |
| `conversation` | id、type、private_user_a/b 或 group_id、last_seq、updated_at | PK(id)；私聊参与者字段与规范 id 一致；last_seq 初始为 0 |
| `message_outbox` | event_id、event_type、command_id、conversation_id、message_id、payload、status、available_at、lease_token、lease_until、attempts、last_error、created_at、completed_at | PK(event_id)；UNIQUE(command_id, event_type)；索引(status, available_at, created_at)、(status, lease_until) |
| `message_delivery_cursor` | user_id、conversation_id、delivered_seq、updated_at | PK(user_id, conversation_id)；只能单调推进，不超过已提交 last_seq |
| `message_quarantine` | transport_id、command_id/身份（可空）、payload_hash、原始 body/headers、error_code、error_detail、state、replay_attempt_id、created_at、resolved_at | PK(transport_id)；索引(sender_id, client_message_id, payload_hash, state)；保留原始数据以供审计和重放 |

`receiver_id` 在私聊表示另一用户，群聊表示群 ID，沿用业务含义；类型枚举通过 DTO 转换，不能把旧 Go iota 数值当成稳定的外部协议。

`message_command` 中禁止提交后残留 `processing` 状态。v1 CHECK 在单条 SQL 执行时也拒绝该状态，因此 P2.3 使用事务内临时 `rejected` 行及私有随机 `__claim:` 原因标记完成身份占位；这不是业务拒绝结果，提交前必须替换为校验后的真实终态。仓储独占提交/回滚权，错误、取消及 panic 都回滚占位；读到已提交的占位标记按数据不变量错误处理。数据库 CHECK 只约束字段组合，不替代仓储的终态收口。worker 崩溃时占位随事务回滚，不需要基于时间猜测是否可以抢占。

### 5.3 幂等与冲突

- 同键同 hash：返回已经存在的终态，不再分配 seq、写消息或新建 Outbox。
- 同键不同 hash：原记录不变，当前请求为 `ID_CONFLICT`；不能使用 UPSERT 覆盖消息。
- 不同键相同内容：视为两次独立发送，允许生成两条消息。
- 已 rejected 的同键同内容重试：仍为原 rejected。用户修正权限后主动重新发送，应使用新 clientMessageId。
- MQ 发布超时、HTTP 响应丢失、进程重启：依靠同一逻辑键重试；不需要追求物理投递只有一次。
- 状态查询必须同时携带 clientMessageId 和该次规范 payloadHash；否则无法区分同 ID 不同内容的冲突请求。
- 首版不自动删除命令幂等记录或消息历史。未来引入保留期时，必须同步定义客户端重试期和旧消息重放边界。

### 5.4 seq 分配与提交可见性

消费者在业务校验通过后创建或定位 conversation 行，以 `SELECT ... FOR UPDATE` 锁定，读取 `last_seq`，分配 `last_seq + 1`，写入 message 并更新计数，最后在同一事务提交。

同一会话的后续事务必须等待前一事务提交或回滚，因此不会出现客户端先看到已提交 seq=12、之后才出现 seq=11 的情况。回滚不消耗序号；同键重投不重新分配。

所有写消息的入口，包括测试工厂、维护脚本和未来迁移器，都必须调用这个事务路径。禁止使用 Redis INCR、数据库自增 message 主键、时间戳或雪花 ID 直接替代同步水位。

这会串行化热门会话的写入，是首版为正确性接受的成本。重试后的消息可能晚于其他消息提交，首版定义的顺序就是业务提交顺序。

P2.5 的生产仓储入口为 `MessageRepository.PersistCommand`：先复用命令幂等与权限锁，再以 no-op insert 创建/锁定规范 conversation，并通过 `FOR UPDATE` 核验身份、读取 last_seq。在同一事务插入 message、条件更新 last_seq、插入两类 Outbox，最后由 P2.3 收口命令终态并提交。任何步骤失败均回滚；权限拒绝不创建 conversation 或 message，只提交 rejected 和一条结果事件。同 hash 重试及异 hash 冲突都不运行写入分支。序号达到 uint64 上限后明确返回 `ErrConversationExhausted`，不回绕、不消耗命令键，不将该技术边界伪装成业务拒绝。该入口尚未接入旧 HTTP/MQ 路径；旧 schema 的切换仍在 P7。

message 的服务端时间取数据库 `UTC_TIMESTAMP(6)`；绑定/读取显式 UTC 时钟文本，避免非 UTC DSN 的 loc 对 DATETIME 值产生额外转换。clientSentAt 缺省写 NULL，否则先按 UTC 显式截到微秒再写，原纳秒元数据不参与 hash，重试保留首次已提交值。测试覆盖固定 MySQL 8.0.44 上的协议年份边界；MySQL 官方只保证 DATETIME 的 1000–9999 年范围，1000 年之前的协议元数据在其他版本部署前需要兼容性验证。[MySQL DATETIME 范围](https://dev.mysql.com/doc/refman/8.0/en/datetime.html)。

## 6. RabbitMQ 拓扑和生产者

### 6.1 固定拓扑

| 对象 | 名称 | 要求 |
| --- | --- | --- |
| vhost | `/ichat`；测试使用每轮独立 vhost | 应用账号仅拥有必要权限 |
| 主 exchange | `ichat.commands.v1` | durable、direct |
| 主 queue | `ichat.message.persist.v1` | durable、quorum、非 exclusive、非 auto-delete |
| 主 routing key | `message.submit.v1` | 固定绑定到主 queue |
| 隔离 exchange | `ichat.quarantine.v1` | durable、direct |
| 隔离 queue | `ichat.message.quarantine.v1` | durable、quorum、非 auto-delete，无 TTL |
| 隔离 routing key | `message.quarantine.v1` | 固定绑定到隔离 queue |

本地声明 quorum 初始组大小 1，生产为 3，并验证实际队列成员数；三个 broker 进程存在不代表每条队列都有三个副本。生产节点使用独立故障域与磁盘；同一台机器上三个容器只适合演练。

主队列不设置 message TTL；容量策略使用 `reject-publish`，不能使用默认丢队头的溢出策略。首版采用明确 delivery-limit（例如 50）及可靠 DLX：`dead-letter-strategy=at-least-once`、`overflow=reject-publish`、显式 dead-letter exchange/routing key，并核验必要 feature flag。隔离队列必须先创建；DLX 不可用时保留原消息并告警。[Quorum queue 与死信约束](https://www.rabbitmq.com/docs/quorum-queues)

这些参数应写入受版本控制的拓扑/definitions 或声明代码，应用启动核验；不得依赖手工管理界面配置。参数不一致时未就绪并报错，禁止通过自动删除队列“修复”。

### 6.2 命令信封

```json
{
  "schemaVersion": 1,
  "commandId": "<64-char-hex>",
  "clientMessageId": "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1",
  "publishAttemptId": "f9333c33-257b-4341-84f3-c6d4fd3abf90",
  "senderId": "12",
  "targetType": "private",
  "targetId": "34",
  "media": "text",
  "content": "hello",
  "payloadHash": "<64-char-hex>",
  "receivedAt": "2026-09-14T00:00:00Z"
}
```

所有信封由服务器构造，不能直接把客户端请求 body 原样写 MQ。AMQP 属性设置 ContentType、持久化 DeliveryMode、MessageId=commandId、CorrelationId=publishAttemptId；生产者连接不发送 JWT 或数据库凭证作为消息字段。

### 6.3 入口处理

1. 校验 JWT、Redis 黑名单和请求大小；从鉴权上下文取得 senderId。
2. 校验 UUID、目标类型/正整数 ID、媒体枚举、内容长度；首版内容限制与数据库 `varchar(1024)` 的字符语义保持一致，另限制整个 HTTP body 的字节数。
3. 生成规范信封与 hash；此阶段不依赖 MySQL 查询权限/终态。
4. 在请求总超时内取得有界 publisher slot；池满未开始写入可明确返回过载。
5. 执行可靠发布；只有完整确认成功才 HTTP 202。
6. 写入开始后的错误或 confirm 超时通常返回 `publish_unknown`；客户端保留原键重试。明确无法路由可返回配置/服务不可用，但也应允许原键安全重试。

### 6.4 Confirm 与 return 的正确关联

固定 `amqp091-go v1.15.0` 的首版设计（P3.1 已复核源码）：有界 publisher slot 池，每个 AMQP channel 同时最多一条未完成发布。不要让多个 HTTP goroutine 无约束共用一个 channel。

- session 包含 connectionEpoch、channelEpoch；记录下一发布序号、commandId、publishAttemptId。
- 注册一组原始 `NotifyReturn`、`NotifyPublish`、`NotifyClose`，缓冲至少覆盖该 slot 的 outstanding 数；仅该 slot worker 读取它们。
- 开启 `Confirm(false)`，发布 `mandatory=true` 和持久化消息。
- 收到 `basic.return` 记录该 attempt 无法路由。收到 ACK 时，**先同步非阻塞排空原始 return channel，再做成功判定**，并核验 session/tag/attempt。
- 只有正确 ACK 且没有该 attempt 的 return 才返回 accepted。
- return、NACK、写错误、超时、连接关闭后退休该 channel；重建新 epoch，不能把晚到的确认分配给下个请求。
- 成功判定完成前，slot 不能归还池。

RabbitMQ 的 return 可先于对应 confirm，但 Go `select` 可能先读到确认 channel。这里的排空准则依赖固定客户端版本串行分派、通知未被丢弃和单接收者的前提。禁止另起 return goroutine 异步写 map 后，在 ACK 分支立即成功。v1.15.0 的 `notifyAll` 在目标通知 channel 满时等待 5 秒后可放弃该通知，因此缓冲与有界 outstanding 是正确性条件；升级客户端必须复核源码并重跑顺序测试。P3.1 已验证一条 outstanding、容量 1 时刻意先读 ACK 仍能取得原 Return；生产状态机和故障矩阵归 P3.3。[确认语义](https://www.rabbitmq.com/docs/confirms)、[客户端通知实现](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/channel.go)

### 6.5 超时和重连

v1.15.0 的 `PublishWithContext` 在调用前取消时直接报错，但不能用之后的 context 取消中断已经开始的网络 I/O。必须同时实现有界网络写超时/可中断 transport，确保 broker 背压或网络黑洞时请求和 goroutine 数量有界。

首版显式使用 `Recovery: nil` 关闭实验性自动 Recovery，由应用 supervisor 执行：停止 session 接单 → 未确定结果标 unknown → 关闭旧 channel/必要时连接 → 带抖动退避重连 → 核验拓扑 → 重建通知与 confirm 模式 → ready。生产者和消费者连接独立；若某连接被强制关闭，其全部 outstanding 发布均按 unknown 处理。[客户端 README](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/README.md)、[连接配置](https://github.com/rabbitmq/amqp091-go/blob/v1.15.0/connection.go)

## 7. 消费事务、重试与隔离

### 7.1 消费处理顺序

消费使用手动 ACK、有界 prefetch；首版每个 worker channel 顺序处理自己的 delivery，通过多个 worker 获得并发。ACK 使用 `multiple=false`，禁止确认本 channel 上尚未提交的其他消息。

```text
读取 delivery
  校验信封版本、长度、字段及重新计算的 commandId/payloadHash
  BEGIN
    插入 message_command 占位（仅事务内部存在）
    按 (sender_id, client_message_id) SELECT ... FOR UPDATE
    若已有终态：
      同 hash → 复用原结果，结束事务；提交/结束后 ACK
      异 hash → 保持原记录，结果为 ID_CONFLICT；结束事务后 ACK
    若为新命令：
      在事务内校验目标用户/群存在及当前权限
      业务拒绝 → 命令写 rejected + command.result outbox
      业务允许 → 锁定 conversation
                 分配 seq + INSERT message + 更新 last_seq
                 命令写 persisted
                 写 message.persisted 与 command.result outbox
  COMMIT
  ACK 当前 delivery
```

并发占位可以使用只触发唯一键竞争的 no-op insert，然后锁定并读取规范行。必须区分“预期唯一键已存在”与其他 SQL 错误；不得用通用 `INSERT IGNORE` 掩盖字段截断、约束错误或内容冲突。最终幂等判断以数据库唯一约束与事务为准，不能使用先 SELECT 不存在再普通 INSERT 的竞态逻辑。

P2.3 的 `MessageRepository.ExecuteCommand` 使用上述 no-op insert 和 `FOR UPDATE`，通过私有随机标记判断新命令归属，不以会受 `CLIENT_FOUND_ROWS` 影响的插入行数判断。已有记录先核验规范身份和终态，同 hash 复用，异 hash 返回不含原成功结果的 `ID_CONFLICT`。新命令处理回调只使用仓储提供的事务 SQL 句柄；最终 UPDATE 必须替换本次标记，复核真实终态后才提交。SQL 错误原样保留供后续 consumer 分类；COMMIT 报错返回明确的提交结果未知，不自动重试或返回终态成功。[MySQL 唯一键更新与行数语义](https://dev.mysql.com/doc/refman/8.0/en/insert-on-duplicate.html)、[InnoDB SQL 锁行为](https://dev.mysql.com/doc/refman/8.0/en/innodb-locks-set.html)。

同键异内容不修改规范结果。状态接口以请求的 payloadHash 与规范 hash 比较，返回该次请求的冲突；即使没有额外的冲突终态行，也能稳定查询。不得因为原命令已 persisted，就告诉异内容请求它也 persisted。

### 7.2 权限和并发变更

- 私聊：用户存在、发送者与接收者不同、双方有效好友关系存在。首版无单向陌生人发送。
- 群聊：群存在且未删除，发送者是当前有效成员。
- 已有幂等终态先返回原结果，不再次依据当前权限执行历史命令。
- 新命令的授权查询和消息写入在同一事务中；涉及的群/关系记录使用适当的共享/排他锁，避免权限检查后相关关系被并发修改而语义无定义。
- 群删除、加入等写操作须采用一致的锁顺序，并使用同一个传入的事务句柄。`CreateGroup` 已在 P0.6 将群记录和群主成员关系放入同一事务；P2.4 补充有效群主的锁定和校验。加入群先校验有效用户和有效群，避免生成孤立成员关系。
- 统一锁顺序：命令身份 → 用户（按数值 ID 升序）→ 群（如涉及）→ 关系（按唯一逻辑键 `(owner_id,target_id,type)` 升序）→ conversation。只涉及部分对象的写操作保持该顺序，不在群/关系锁之后反向锁用户。死锁作为可重试错误处理，有界重试在 P4 consumer 落实。

P2.4 的 `ExecuteAuthorizedCommand` 只在新命令分支执行授权：以 `FOR SHARE` 锁定实际用户、群及关系行，检查软删除状态；私聊要求两个方向的有效好友关系，群聊要求有效成员，群主身份不替代成员关系。授权通过后的处理回调继续使用同一事务，权限锁保持到事务结束；已有终态或内容冲突不重新检查权限。SQL 错误触发回滚，不产生业务拒绝终态。现有加好友/创建群/加入群/删除群路径采用相容的排他锁顺序；未来取消好友、退群或恢复关系也必须遵循此顺序。删除群保留既有成员记录，但已删除群不可授权或加入。[MySQL 锁定读取](https://dev.mysql.com/doc/refman/8.0/en/innodb-locking-reads.html)。

### 7.3 失败分类

下表为最终目标。[P4.2.1](mq-persistence-retry.md)已实现完整错误树分类，以及仓储成功结束回滚后的 1213/1205 有界本地重试；提交/回滚未知、连接/约束/未知错误仍保留原 delivery，不因次数耗尽被当作坏消息。[P4.2.2.1](mq-persistence-supervisor.md)提供独立监督器，[P4.2.2.2](mq-persistence-connections.md)已接入生产连接来源、真正 join 后替换和当前轮次聚合就绪；监督恢复的运行时退出验收及可靠隔离仍待后续小点。

| 分类 | 例子 | 行为 |
| --- | --- | --- |
| 业务拒绝 | 非好友、非成员、目标不存在 | 在 DB 保存 rejected 和通知事件，提交后 ACK；不放进技术死信队列 |
| 幂等重放 | 同键同 hash | 不重复执行业务；复用已有结果后 ACK |
| 幂等冲突 | 同键不同 hash | 保留规范记录，按 hash 查询返回冲突；记录指标后 ACK |
| 暂时性数据库问题 | 连接失败、死锁、短暂锁超时 | 回滚，有界退避重试；数据库整体不可用时暂停领取，避免立即 requeue 热循环 |
| 数据库提交结果不明 | COMMIT 已发出但连接断开 | 不 ACK，重试通过幂等表判定；不能补偿删除可能已经提交的 message |
| 不可解析/不支持信封 | 无法解码、未知版本、摘要不符 | 保留原始 body/headers，安全进入隔离区 |
| 持续技术处理失败 | 确定性代码/约束错误 | 在有限本地重试后安全隔离并告警，不堵住所有后续消息 |
| 发布隔离记录失败 | DLQ 不存在、目标拒绝、confirm 超时 | 不确认原消息，保留恢复责任 |

数据库整体故障不应因为业务重试次数耗尽就把全部正常消息当作坏消息。worker 应通过 readiness/circuit breaker 停止新领取，有限等待后关闭当前 channel 使未确认消息回队列，恢复后再消费。防止反复关闭/重连形成重投风暴。即使 broker 的 delivery-limit 最终触发，可靠 DLX 也必须保留消息，恢复流程包含重放这些记录。

### 7.4 技术隔离与重放

主动隔离时，将原始信封、headers、失败分类、transport identity 和诊断信息发布到隔离队列；只有目标发布收到确认且无 return，才 ACK 原 delivery。转移确认丢失可造成重复隔离，依靠稳定 transport identity 去重。

RabbitMQ 自动 DLX 与应用主动隔离都进入同一受控隔离队列。隔离记录 worker 将原始内容写入 `message_quarantine`，**数据库提交后才 ACK 隔离队列消息**；数据库离线时保留 MQ 中的记录。该表持久保存原文，不只是错误摘要。

状态查询先查规范命令终态，再查匹配 key/hash 的未解决隔离记录；记录尚未转入数据库时可返回 `pending_or_unknown`。不得扫描/破坏主队列来实现 HTTP 状态查询。

提供独立的受控 CLI 管理功能：列举 → 查看原因 → 修复程序/配置 → 选择记录 → 重新发布原命令并等待确认 → 记录 replay attempt。业务身份和 hash 保持不变，新的物理 attemptId 独立生成。确认后、标记重放之间退出会重复投递，仍由幂等兜底。

隔离记录只有在确认业务终态后才标记 resolved；“已经重发”与“已经修复成功”是两个状态。已 rejected 的业务命令不会因为管理员重放而改变终态。不得通过删除幂等记录强行重做。

## 8. Outbox、通知和缓存

### 8.1 为什么 MQ 之后仍有 Outbox

MQ 已解决 API 到落库消费者的责任交接，但“数据库提交”和“通知在线客户端”仍是两个操作。消费者若提交后退出，单独的 Pub/Sub 发布可能永远没有执行。将通知事件与 message 放入同一 MySQL 事务，后台即可恢复。[Transactional Outbox 模式](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/transactional-outbox.html)

首版 Outbox 事件：

- `message.persisted`：向私聊接收者或群频道发送新消息提示。
- `command.result`：向发送者的个人频道发送 persisted/rejected 结果；persisted 包含服务端消息身份和 seq。

`event_id` 可由 commandId 与 eventType 确定性派生；数据库唯一约束防止重复业务事件。重试同键时不重复插入事件，发送者通过状态查询也能恢复结果。

P2.5 固定 v1 事件身份为 `SHA256(UTF8("ichat:outbox:v1") || 0x00 || LP(commandId) || LP(eventType))`，LP 沿用第 5 节协议的 UTF-8 字节长度前缀编码；规范和独立固定向量见 [消息协议 v1](message-protocol-v1.md)。`messaging/outbox.go` 定义两类内部存储 payload，包含 schemaVersion=1 和固定 eventId：message.persisted 只保存身份、路由和字符串 seq 提示；command.result 保存发送者、客户端键、hash、终态，以及成功时的完整 message DTO 或拒绝时的 reasonCode。新事件以 pending、attempts=0、无 lease/完成时间写入，available_at/created_at 使用数据库 UTC 时钟。普通 INSERT 的唯一键碰撞直接失败并回滚，不用 UPDATE/IGNORE 掩盖不完整事件集合。这些是内部 Outbox payload，不替代 P5 的通知授权和 WebSocket 协议适配。

### 8.2 Outbox 领取和完成

1. 在短事务中选择到期 pending 或 lease 已过期的任务，以 `FOR UPDATE SKIP LOCKED` 领取有限批次，设置随机 lease_token、lease_until、attempts，提交。
2. 在事务外执行通知；不能持有数据库行锁等待 Redis/网络。
3. 成功后按 event_id + 当前 lease_token 条件更新为 completed。
4. 失败后按相同 token 更新 available_at 和错误，指数退避并带抖动。
5. 超时或 worker 退出由 lease 到期恢复。旧 worker 的迟到更新不能覆盖新持有者状态。

lease 使用数据库时间或明确的时钟约束，发送超时应小于 lease；必要时续约。重复发送仍可能发生，接收方必须去重。`SKIP LOCKED` 仅用于队列式任务领取，不能用于可靠历史读取或跳过未提交的会话序号。[MySQL 锁定读取](https://dev.mysql.com/doc/refman/8.0/en/innodb-locking-reads.html)

### 8.3 通知与内容

- Redis Pub/Sub 中优先携带 eventId、conversationId、messageId、seq 等提示，不把它作为消息历史存储。
- 网关收到提示后，通过 MySQL 仓储读取并再次验证当前用户有权接收，再发送统一的 WebSocket `message` DTO。
- 发送者通过 `command.result` 获得终态；成功结果返回或关联完整 message DTO，保证自己的已发送消息也能参与会话 seq 连续处理。
- 群频道订阅不等于永久授权。离群/群删除后的旧订阅不能继续向用户转发内容；群列表变化后重建订阅，空频道集合不发出无意义订阅。
- 保留 PubSub 句柄并在 session 结束时 Close；订阅需要就绪确认，读取关闭 channel 需要检查 ok。
- 所有 WebSocket 数据帧由单一 writer 发送，沿用当前单 writer 的正确设计；出站队列满或写超时可以断开慢客户端，恢复依靠补拉。

Redis PUBLISH 成功（包括没有订阅者）可以标记 Outbox 的“通知已发布”，但**不能更新 delivered 游标**。Redis Pub/Sub 本身不提供遗漏重放。[Pub/Sub 语义](https://redis.io/docs/latest/develop/pubsub/)

### 8.4 缓存定位

首版可靠同步接口直接以 MySQL 查询结果为准。可以保留或更新 Redis 缓存作为性能优化，但缓存失败不得回滚 persisted 消息，缓存 key 存在不得用作数据完整性证明。

新旧缓存使用不同 namespace，例如 `msg:v3:*`。切换后禁止新代码自动把旧 `msg:*` / `msg:v2:*` 当作权威历史迁入新协议。不要为填满缓存一次性读取某会话全部历史。

## 9. HTTP 与 WebSocket 协议

本节定义新契约。用户允许直接修改接口，不要求旧响应兼容。现有 `RespOK/RespFail` 都写 HTTP 200，需要为消息 API 增加明确的 HTTP status 支持；同步更新压测工具，不能只改服务端。

### 9.1 接口表

| 接口 | 作用 | 授权/返回 |
| --- | --- | --- |
| `POST /auth/sendMessage` | 接收消息命令 | JWT 用户作为发送者；202 accepted |
| `GET /auth/messages/status?clientMessageId=...&payloadHash=...` | 查询本次发送内容的状态 | 仅当前发送者的命令；200 persisted/rejected/quarantined/pending_or_unknown，内容冲突用 409 |
| `GET /auth/conversations?cursor=...&limit=100` | 当前用户可见会话及 lastSeq | 私聊参与者、当前群成员；稳定 conversationId 排序分页 |
| `GET /auth/messages/sync?conversationId=...&afterSeq=0&limit=100` | 向前补齐连续消息 | 私聊参与者或当前群成员；MySQL 权威读取 |
| `GET /auth/messages/history?conversationId=...&beforeSeq=...&limit=100` | 历史向前翻页 | 相同权限；按 seq 查询，返回统一 message DTO |
| `GET /auth/chat` | WebSocket | 绑定鉴权用户和 session；使用下述帧协议 |

旧 `getMessageList` 与 `/getws` 别名在切换时退役或显式指向新实现，不能继续绕过权限或读旧缓存。接口名字若在实现中调整，必须同一次修改本文、客户端协议测试和 loadtest。

### 9.2 请求与接受响应

```json
{
  "clientMessageId": "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1",
  "targetType": "private",
  "targetId": "34",
  "media": "text",
  "content": "hello",
  "clientSentAt": "2026-09-14T00:00:00Z"
}
```

```json
{
  "code": 0,
  "msg": "accepted",
  "data": {
    "state": "accepted",
    "commandId": "<64-char-hex>",
    "clientMessageId": "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1",
    "payloadHash": "<64-char-hex>",
    "publishAttemptId": "f9333c33-257b-4341-84f3-c6d4fd3abf90"
  }
}
```

此响应使用 **HTTP 202**。不能返回未分配的 seq，也不能把 state 命名为 delivered。即使相同命令已入库，入口未查询 MySQL 时仍可以返回 accepted；客户端终态不能因此倒退。

### 9.3 错误与状态查询

| HTTP | 错误/状态 | 客户端行为 |
| --- | --- | --- |
| 400 | INVALID_REQUEST | 修改输入；这是入队前确定拒绝 |
| 401 | UNAUTHENTICATED | 恢复登录后按原业务键重试，不假定之前 unknown 请求未成功 |
| 403 | FORBIDDEN | 用于读/ACK 的权限拒绝；发送业务权限异步返回 rejected |
| 409 | ID_CONFLICT | 同 key 不同 payloadHash，禁止覆盖原消息 |
| 429 | OVERLOADED | 尚未开始发布的请求被限流；带 Retry-After，原键重试 |
| 503 | DEPENDENCY_UNAVAILABLE / PUBLISH_UNROUTABLE | 依赖或拓扑故障；原键重试 |
| 503 | PUBLISH_UNKNOWN | 已开始发布但结果不确定；data.state=publish_unknown，保留原请求查询/重试 |
| 200 | rejected | 返回稳定 reasonCode；业务终态，不是基础设施重试 |
| 200 | pending_or_unknown | 退避查询；需要时同键同内容重发；不得从数据库无记录推导“可换新 ID” |

错误体保留 code/msg/data 的统一形式，新增稳定 `reasonCode`，不把数据库或 broker 原始错误字符串直接作为前端契约。状态查询先查 `(authenticatedSender, clientMessageId)`，存在但 hash 不同则 409；存在且相同返回终态，否则查询匹配隔离记录，再返回 pending_or_unknown。数据库不可用返回 503，而不是 pending。

状态查询应提供建议轮询间隔。若 HTTP 响应丢失导致客户端没有 payloadHash，使用原始请求重发即可获得同一规范 hash，不生成新的 clientMessageId。

P2.6 已实现 `MessageRepository.GetCommandStatus` 和共享 `CommandStatus` DTO，HTTP 路由及响应包装留给 P6.1。发送者必须取自鉴权上下文；clientMessageId 沿用 UUID 规范化，payloadHash 必须为 64 字符小写 hex。查询在显式只读 REPEATABLE READ 快照内完成，使用普通 SELECT，不锁定命令占位，不读取未提交结果，也不查询 MQ/Redis。

规范终态优先于隔离记录；只有命令不存在时才检查同 sender/clientMessageId/payloadHash、state 为 open/replay_pending 且 resolved_at 为空的隔离行。quarantined 与 pending_or_unknown 带 `pollAfterMs: 2000`，终态及冲突不带轮询字段。冲突只回显该次请求 key/hash，不附原成功消息、原 hash 或原拒绝原因；隔离状态不附原文、headers、transportId 或诊断错误。persisted 返回核验身份/hash、会话及已提交 seq 的完整规范消息，rejected 返回稳定 reasonCode。好友/成员/群的后续变更不重新判定发送者自己的既有终态，当前历史读取权限另由 P2.7 检查。快照中的旧隔离状态可在下一轮变为新终态；SQL、连接池、快照释放及数据不变量故障返回错误和空 DTO，不能降级成 pending。详细字段见 [协议 v1](message-protocol-v1.md#p26发送状态查询)。

### 9.4 Message DTO 与同步响应

```json
{
  "messageId": "<64-char-hex>",
  "clientMessageId": "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1",
  "conversationId": "p:12:34",
  "seq": "42",
  "senderId": "12",
  "targetId": "34",
  "targetType": "private",
  "media": "text",
  "content": "hello",
  "serverCreatedAt": "2026-09-14T00:00:01Z"
}
```

同步 response.data：

```json
{
  "conversationId": "p:12:34",
  "afterSeq": "40",
  "items": [],
  "nextAfterSeq": "40",
  "highWatermark": "40",
  "hasMore": false
}
```

读取在一致的数据库视图中完成授权、highWatermark 与消息范围：`seq > afterSeq AND seq <= highWatermark ORDER BY seq ASC LIMIT limit`。`nextAfterSeq` 取返回的最后连续 seq；没有消息时保持原游标。`hasMore` 表示这一视图中是否还有消息。limit 默认 100、最大 500，afterSeq/beforeSeq 必须为合法十进制整数。

首版没有消息删除/过滤产生的合法序号洞。若 highWatermark 大于游标却缺失下一个 seq，视为一致性错误：告警并重试，不能把游标直接跳到 highWatermark。未来加入撤回/删除时应使用占序号的 tombstone，再修改协议。

`conversations` 返回每个会话 lastSeq 和服务器 deliveredSeq，并附分页游标。客户端每个周期必须完成一轮所有可见会话扫描，下一轮从头开始；不能拿全局最大 messageId 或某次 updatedAt 作为可靠的永久增量水位，以免遗漏新会话和晚可见数据。

### 9.5 WebSocket 帧

| type | 方向 | data |
| --- | --- | --- |
| `ready` | 服务端 → 客户端 | protocolVersion、sessionId、syncIntervalSeconds |
| `message` | 服务端 → 客户端 | 与同步接口相同的 Message DTO |
| `command.result` | 服务端 → 发送者 | clientMessageId、payloadHash、state、reasonCode；persisted 可含完整 message |
| `sync.required` | 服务端 → 客户端 | 提示重扫全部会话或指定 conversationId；不是可靠性唯一触发源 |
| `ack` | 客户端 → 服务端 | conversationId、deliveredSeq |
| `ack.confirmed` | 服务端 → 客户端 | conversationId、服务端已提交的 deliveredSeq |
| `ping` / `pong` | 双向 | 心跳，不代表业务 ACK |

ACK 处理必须接收当前 userId 和 sessionId，检查会话权限、当前活动 session、`0 <= deliveredSeq <= conversation.last_seq`，在数据库单调更新后才发 ack.confirmed。相同或更小 ACK 是幂等的，返回现有进度。Redis/本地内存游标不能作为已持久提交的证明。

客户端通过连续保存后 ACK；服务端无法代替客户端证明其本地写盘，但必须防止跨用户更新、越权群 ACK 和超出已提交序号。旧 session 的迟到帧不得更新新会话状态。

## 10. 客户端状态机与补拉算法

仓库没有前端实现。开发 agent 必须交付协议文档和模拟客户端测试；实际前端接入未完成时，验收报告应明确标注，不能仅凭服务端测试声称真实客户端已经支持恢复。

### 10.1 发送侧

```text
本地保存待发送记录（稳定 ID + 原内容）
  → submitting
  → accepted → 查询/通知 → persisted 或 rejected
  → publish_unknown → 原键查询/重试 → accepted / persisted / rejected
  → quarantined → 保留记录并展示待处理，不伪装送达
```

- 在发 HTTP 前保存 clientMessageId 和原内容；请求超时、刷新、重启后继续使用它们。
- persisted/rejected 是终态。WebSocket 结果可能早于 HTTP accepted 到达，状态机不得倒退。
- 相同消息多次物理接收只展示一次；统计重复传输数，但不重复增加未读或本地消息条数。
- 未收到接受响应的命令不能简单删除本地 pending。重试前不生成新业务 ID。
- 当前用户主动修改内容或在业务拒绝后重新发送，视为新命令，使用新 ID。

### 10.2 接收与本地连续游标

1. 本地存储以 messageId 唯一，另对 `(conversationId, seq)` 校验唯一与内容一致。
2. 收到 WebSocket message 或同步 items，先持久化消息，再推进本地连续 seq；消息与本地游标尽量在同一个本地事务提交。
3. 收到 seq=45 而本地连续游标为 42，可保存 45，但游标仍为 42；主动补拉 43、44，补齐后才推进到 45。
4. 推进后发送用户级 ACK；ACK 丢失可重复发送，不重复执行业务。
5. server deliveredSeq 只是服务器记录的收件进度，不证明当前浏览器有那些本地数据。客户端本地数据清空/换浏览器时，从 0 或明确的历史恢复流程开始，不能直接跳到服务器游标。

### 10.3 重连与持续在线恢复

- 建立连接并收到 ready 后扫描可见会话列表，按本地连续游标补拉至该次 highWatermark。
- 连接恢复、消息出现缺口、收到 sync.required 时触发同步。
- 即使没有上述事件，也默认每 15 秒（可配置、带抖动）完成一轮可见会话高水位核对，恢复“最后一条通知丢失但连接一直在线”的情况。
- 补拉与实时推送可以并行到达，统一进入同一个本地幂等存储/连续游标逻辑。
- 一个会话分页直到 hasMore=false；下一次周期核对新 highWatermark，不把旧查询快照当成永久终点。
- 群成员变化后重新获取可见会话和订阅。读取接口的权限判断是最终边界；已退出群的旧订阅或旧会话 ID 不授予访问权。

HTTP 轮询和补拉必须有并发上限、退避、取消和单会话合并，避免重连后每条遗漏消息各发一个并发请求。

## 11. 配置、监控和容量边界

### 11.1 需要新增的配置

P3.5.1 已实现 publisher/management 应用配置、严格参数校验和全字段 `ICHAT_RABBITMQ_*` 覆盖，详见 [MQ 应用配置](mq-application-config.md)。`config/config.example.yml` 的连接/凭证字段默认空，不启用 MQ；配置读取和纯构造不联网，主程序尚未装配连接。以下仍是未来配置草案，包含尚未实现的 consumer/Outbox 等字段，不能直接复制进当前严格校验的 RABBITMQ 节。数值用于开发起点，不是已测容量或生产 SLA；后续实现时须同步模板并验证缺省/环境覆盖/无效值。

```yaml
MESSAGE:
  transport: rabbitmq
  protocolVersion: 1
  maxContentRunes: 1024
  maxRequestBytes: 16384
  syncPageSize: 100
  syncMaxPageSize: 500
  syncInterval: 15s
RABBITMQ:
  url: amqp://ichat_app:CHANGE_ME@127.0.0.1:5672/%2Fichat
  publisherSlots: 8
  publishTimeout: 5s
  connectTimeout: 5s
  writeTimeout: 3s
  consumerWorkers: 4
  consumerPrefetch: 16
  consumerAckTimeout: 30m
  retryMin: 1s
  retryMax: 30s
  quorumInitialGroupSize: 1
OUTBOX:
  batchSize: 100
  pollInterval: 500ms
  leaseDuration: 30s
  operationTimeout: 5s
LEGACY:
  workerEnabled: false
```

MQ username/password 在本地 `.env` 或环境中配置，不把真实凭证提交到仓库；生产使用 TLS 与限制权限的账号，管理端口不公开给业务客户端。`consumerAckTimeout` 等 broker 设置须在 broker 配置中真实生效，不能只增加一个应用配置字段。

新增 `docker-compose.test.yml`、`docker-compose.mq-ha.yml`、definitions 或配置文件时检查 `.gitignore`：当前规则忽略几乎所有 `*.yml`，需要为应提交的具体文件添加例外。

### 11.2 最小监控集合

| 范围 | 指标/日志 |
| --- | --- |
| API / publisher | accepted、publish_unknown、return、nack、池等待、发布确认 P95/P99、限流、session 重建 |
| MQ | ready/unacked、最老待处理消息年龄、发布/消费速率、磁盘/内存告警、队列副本状态、死信数 |
| 消费者 | persisted、rejected（按 reasonCode）、幂等命中、ID_CONFLICT、DB 提交延迟、暂时失败、重新投递、隔离 |
| Outbox | pending/leased、最老未完成年龄、重试、lease 过期、发送失败、完成延迟 |
| 同步和 WS | ACK 延迟、补拉数量、恢复消息数、重复帧、seq 缺口、慢客户端断开、订阅/goroutine 数 |
| 对账 | accepted 逻辑身份集合、MySQL 终态集合、未解决集合、隔离集合 |

结构化日志记录 commandId、clientMessageId、publishAttemptId、conversationId、seq、worker/session epoch 和错误分类；避免记录 JWT、密码、消息正文。指标标签不要使用 messageId/userId 等无界值。

### 11.3 容量和持久性

- MQ 是有限缓冲。队列预算根据消息平均字节、允许数据库故障时长、发布速率和副本开销测算，再设置上限与告警。
- 长期输入速率大于落库速率时积压会持续增长；MQ 不增加数据库的无限写入能力。
- 恢复后实际消费速率需要高于持续输入速率，才有能力清空积压。
- 首版临时性能参数必须通过正确性测试后再调，不先放开无限 goroutine、无限 prefetch 或每 channel 多条 outstanding。
- 核实 MySQL InnoDB 提交持久性，例如 `innodb_flush_log_at_trx_commit=1`，启用 binlog 时相应核实 `sync_binlog` 与恢复配置；将 DB 提交后的 ACK 责任与备份/恢复方案一并记录。三节点 MQ 不能替代 MySQL 的持久化、备份和故障恢复。[MySQL 参数说明](https://dev.mysql.com/doc/refman/8.0/en/innodb-parameters.html)

## 12. 分阶段开发路径

每阶段单独形成可审查提交，记录完成项与测试结果。勾选状态依据实际验收更新，阶段结束以验收条件为准，不以新增文件数为准。当前验收记录见 [实施记录](message-reliability-mq-progress.md)。

### P0：隔离测试和初始化依赖

- [x] 移出配置/DB/Redis/manager 的有副作用 init，建立显式 bootstrap 和构造器。
- [x] 把现有依赖真实数据的测试与 benchmark 放入显式 integration 测试范围；替换固定用户/群 ID 为本轮 fixture。
- [x] 为测试创建专用 MySQL schema、Redis 实例；清理仅限本轮资源。
- [x] 创建独立 RabbitMQ 测试 vhost，并限制清理归属（按既定拆分在 P3.1 随独立 Docker broker 完成）。
- [x] 升级并固定 Go 工具链，记录客户端和镜像版本；只按需要调整依赖（版本核验记录见 P0.4）。
- [x] 迁移命令与业务启动分开，迁移失败显式退出。

验收：不启动 MySQL/Redis/MQ，纯单元测试仍能运行；导入 utils/database 不联网。API bootstrap 的依赖缺失行为可测试。当前测试隔离完成前，不直接对原开发配置运行 `go test ./...`。

当前状态：P0.1—P0.6 的现阶段工作已验收收口；原清单中的 RabbitMQ vhost 项已按拆分在 P3.1 完成。已验证真实 MySQL 离线启动及同一 API 中两次恢复，但不代表 MQ accepted、网络黑洞或全服务器优雅停机验收完成。默认测试已与真实基础设施隔离；真实测试仍须显式 integration 标签及独立 fixture。

### P1：修复旧链路窗口并建立回归

- [x] 完成第 3.3 节旧接受顺序、错误处理、有界领取、精确前缀确认、幂等处理与坏消息隔离。
- [x] 修复发送身份、群历史权限、媒体类型、PubSub 释放/关闭检查。
- [x] 构造 L1—L12 对应失败测试，包括 DB 提交后、Redis 确认前退出。
- [x] 加入防重叠和 legacy worker 显式启用约束。

验收：先证明旧代码在关键回归下失败，再证明修复通过；并发追加与 stale batch 不误删未落库消息。同键同内容重试不阻塞队列。此阶段仍有旧 Redis 持久化边界，不作为最终 MQ 方案验收。

P1 已于 P1.8.2 汇总验收收口。L1—L12 可执行测试、历史失败证据与 F01—F04 范围见 [旧消息链路回归对照](legacy-reliability-acceptance.md)，本轮完整命令、结果及资源见实施记录。下一小点为 P2.1 DTO、规范化和身份/hash；尚未接入 RabbitMQ，也未改变旧 HTTP 成功语义。

当前状态：P1.1 入队接受顺序及错误语义、P1.2 有界读取和队首原子核对已完成，详见实施记录。旧入口改为 RPUSH 确认后再尝试缓存和通知；后续副作用失败记录警告并保留成功。RPUSH 调用后未获成功结果时，HTTP 503 返回 `MESSAGE_ENQUEUE_UNKNOWN` / `enqueue_unknown`，不声称确定未入队。共享 Redis 客户端可能重试，因此包括最终服务端拒绝在内的错误均保守归为 unknown；调用前取消或缺少依赖仍为普通依赖失败。成功仍为旧 HTTP 200 / code 0，未引入 MQ `accepted`/202。

旧 worker 每轮以 `LRANGE 0,127` 读取至多 128 条，落库成功后将原始批次传给 Lua，完全匹配当前队首才原子裁剪。前缀冲突返回错误、不裁剪，本轮结束；下次调用重新读取。

P1.3.1 DB 幂等落库与 P1.3.2 持久隔离/连续前缀推进已完成。沿用旧全局 message.id 主键，事务内插入遇唯一键冲突时锁定读取原记录，全部字段一致才视为成功；内容不同不覆盖原记录。worker 使用 `ProcessDirtyMsgs` 逐条提交，坏 JSON、非法消息、内容冲突及白名单 SQL 数据错误写入独立的 `legacy_message_quarantine` 后才允许确认。隔离原文使用 longblob 保存，按 SHA-256(raw) 去重；重复原文共用审计行，不记录物理投递次数。已隔离记录不会因重试或约束变化自动重新执行业务。

处理失败时只确认此前提交的连续前缀；基础设施故障、隔离写入失败和提交结果不确定的当前记录保留。新增表通过现有独立迁移命令创建，启动前须迁移；未来 MQ 隔离 schema 和受控重放另行实现。已验证消息/隔离写入后 EVAL 发送前失败的幂等恢复；P1.8.1 另已验证首条消息、隔离记录、整批提交后且 Redis 确认前强杀实际 worker 子进程，人工解除注册后由新进程幂等恢复。尚未覆盖真实 COMMIT 响应丢失、在途事务强杀或全部故障验收。

P1.4.1 显式启用/进程内防重入和 P1.4.2 跨进程单消费者约束均已完成。`LEGACY_WORKER.enabled` 默认关闭（环境变量 `ICHAT_LEGACY_WORKER_ENABLED`），开启后每 60 秒调度，跳过重叠触发；panic 恢复后可继续调度，Stop 等待当前任务。关闭 worker 不关闭 HTTP 或旧发送入队。启用实例须取得共享 Redis 中无 TTL 的排他注册，逐条校验所有权，确认时原子校验 token 和队首；异常退出需确认旧进程及数据库会话结束后人工恢复。不提供自动接管或数据库 fencing，依赖 Redis 保留权威锁状态；恢复边界见 [旧 worker 恢复说明](legacy-worker-recovery.md)。这些约束已纳入 P1.8.2 汇总验收。

P1.6 历史完整性、群权限和媒体类型已完成，详见实施记录：旧历史直接从 MySQL 有界分页，不使用 Redis 缓存作为历史依据；同一只读快照内检查当前群成员和未删除群，响应保留真实媒体类型。仅展示已落库消息，旧时间戳分页不代表可靠 seq 同步。P1.7 补齐满缓冲订阅的关闭排空/退出等待，以及订阅确认的频道核对。P1.8.1 真实 worker 崩溃恢复与 P1.8.2 L1—L12 回归汇总均已完成；P1.8.2 另补上真实 EVAL 成功后向调用方注入 EOF 的确认不确定性回归，以及通知失败后直接核对 Redis 缓存的断言。P1 收口不包含真实 COMMIT 响应丢失、Redis 数据丢失或 MQ 故障验收。

### P2：领域模型、schema 和幂等仓储

- [x] 新增规范 DTO、身份/hash 生成、枚举和输入限制。
- [x] 创建第 5 节表及唯一约束/索引，测试实际 MySQL schema。
- [x] 实现命令幂等事务、命令拒绝终态和同键/异 hash 判定（P2.3；P2.4 已落实实际权限拒绝）。
- [x] 实现 conversation seq 与 Outbox 原子写入。
- [x] 修复与消息权限相关的群/关系事务句柄与锁顺序。
- [x] 实现 status 仓储查询，使用 MySQL 权威读取（P2.6）。
- [x] 实现 sync/history 仓储查询，使用 MySQL 权威读取（P2.7）。

验收：并行同键调用只写一条消息；异 hash 不覆盖；失败事务不留下 seq 洞或 committed processing；权限拒绝没有消息/seq，但存在命令终态和结果事件。

P2.1—P2.7 的协议、schema、幂等、权限、seq/消息/Outbox 原子提交及状态/同步/历史仓储均已完成，P2 仓储范围验收收口。新版六表由独立 `cmd/migrate-reliable` 在空 MySQL schema 上显式创建并记录版本/结构指纹；旧 schema、半完成迁移与结构漂移均拒绝自动修复。旧 `cmd/migrate` 遇到新版或部分新版表也拒绝运行。ID/hash 使用 ASCII 二进制排序规则，ID/seq 使用无符号 64 位整数，时间使用 `DATETIME(6)`，协议纳秒在入库时显式截到微秒。已验证命令幂等、实际权限拒绝、权限变更与发送的事务串行化、原终态不变、会话连续序号、消息/命令/结果事件的失败回滚，以及状态查询的发送者范围、内容冲突、隔离优先级和一致快照。P2.7 的同步/历史在同一个只读 REPEATABLE READ 快照检查私聊参与者或当前群成员、会话身份和已提交水位，按连续 seq 有界读取；缺号或损坏返回错误，不返回部分页，也不读取 Redis 或服务器 deliveredSeq。详细分页规则见 [协议 v1](message-protocol-v1.md#p27会话同步与历史仓储)。仓储收口后进入 P3；P3.1 版本核查与隔离测试 vhost 已完成，见下一节。新事务尚未接入旧入口、HTTP 202 或 MQ；状态和同步 HTTP 适配归 P6，Outbox 领取及通知发布归 P5，开发环境重置及与账号表的切换归 P7。本阶段不代表端到端可靠性改造完成。

### P3：RabbitMQ 基础设施和可靠 publisher

- [x] P3.1 核验并固定 broker/client 版本，建立独立 Docker broker、测试 vhost、应用账号与归属检查。
- [x] P3.2 本地 Compose、显式管理命令与版本化声明代码创建主/隔离队列及可靠 DLX；group size=1，实际单成员核验、失效保留及恢复转发已通过。
- [x] P3.3 有界 publisher slot、服务器信封、mandatory/persistent 发布与 confirm/return 关联；原始通知排空、真实 Return/NACK 和异常 slot 退休已验收。
- [x] P3.4.1 有界连接/网络写/关闭与连接中止后的 unknown；受控 AMQP 对端、真实 TCP/TLS 黑洞及 Docker ACK/关闭响应丢失已验收。
- [x] P3.4.2 supervisor 抖动退避、单调 epoch、旧 session 回收与新连接只读拓扑复核；连接自动恢复，命令不自动重发。
- [x] P3.5.1 应用 publisher/management 配置与环境覆盖、默认兼容及脱敏严格校验；纯构造不联网，不启用 MQ。
- [x] P3.5.2.1 复用每实例独立的有界连接内核、消费者基础生命周期/只读核验与关闭；受控 Write 背压、真实响应黑洞及双向故障隔离已验收。
- [x] P3.5.2.2 单个长连接 worker 的有界消费 channel/prefetch、顺序 delivery 与原 channel 单条手动 ACK/Reject；重投、旧 handle 隔离及真实 ACK 丢帧已验收。实际业务提交后 ACK 仍归 P4。
- [x] P3.5.3.1 严格模式配置、纯角色计划和启动检查；默认 legacy 兼容，未装配新模式在外部连接前失败，不混用旧 worker。
- [x] P3.5.3.2.1 消费者有界运行参数、全字段环境覆盖与按角色生成的纯 MQ 依赖计划；构造不联网。
- [x] P3.5.3.2.2 按角色受控创建 publisher/独立消费者连接，总构造期限、无订阅准备、部分失败回滚与整组有界关闭。
- [x] P3.5.3.3.1 MQ 托管 lifecycle owner，取消后有界自动关闭、Done/Wait 和清理错误保留；不启动业务任务。
- [x] P3.5.3.3.2.1 所选业务角色构造器检查、全部缺失诊断和激活前服务实例检查；bootstrap 拒绝前不联网、不执行构造器。
- [x] P3.5.3.3.2.2 受控服务构造/全部实例检查/后台角色先激活，失败回滚及停止准入→drain→关闭服务→关闭 MQ 的有界执行器；未提供业务角色实现。
- [x] P3.5.3.3.3.1 主程序配置单次加载、legacy/现代执行器分流与现代信号 context；缺失现代业务工厂仍在连接前拒绝，不提前切断 MQ，不吞清理错误。
- [x] P3.5.3.3.3.2.1 独立 legacy HTTP/WebSocket 生命周期适配器，按请求停止准入/取消 hijacked 连接、等待全部当前及替换会话，超时保留 owner 可再次等待。
- [x] P3.5.3.3.3.2.2.1 legacy cron 停止准入、job deadline、Drain/取消后等待、一次有界注册释放及结果观察；默认 legacy 工厂复用，真实提交后取消/重放通过。
- [x] P3.5.3.3.3.2.2.2 默认 legacy HTTP/worker/健康探测/存储统一生命周期与退出信号；共同 drain/close 预算、部分启动回滚、未 join 保留 globals/存储、真实完成后清理；OS 信号投递未实测。
- [x] P3.5.4 接受 readiness 汇总/接口装配与持续拓扑核验基础；拓扑不匹配保持未就绪，默认缺失业务能力仍不就绪。
  - [x] P3.5.4.1 publisher 已就绪连接周期只读复核，失败/超时退休 epoch、撤销准入；旧检查 join 后才恢复，不修复拓扑或重发。
  - [x] P3.5.4.2 接受就绪状态汇总及运行时/健康接口接线；显式 API/鉴权/消费能力、阶段门禁、publisher 当前状态与消费者独立诊断；真实故障/恢复和 drain 已验证，未提供生产业务角色。
- [x] P3.6 显式 api 模式使用新 DTO 和 202 语义；默认配置仍为 legacy，未完成 consumer 时只使用隔离环境积压。
  - [x] P3.6.1 独立确认式 HTTP handler/路由、Bearer 鉴权、共享 DTO/严格有界读取、发布结果映射及客户端协议探测；真实 202/unknown/429/Return+ACK/MySQL 离线已验证，默认入口未切换。
  - [x] P3.6.2 生产 API 角色的受控 HTTP 生命周期、Redis GET 鉴权探测与运行时装配；启动回滚、慢请求、正常/超时退出和实际依赖故障已验证，缺失后台业务角色继续拒绝。
- [x] P3 验收收口：要求/测试/故障矩阵对照、确认超时后的旧 ACK 直接回归、13 个 MQ/API suite 集中随机顺序验收；业务消费与端到端恢复边界见 [P3 验收对照](mq-reliability-acceptance.md)。

验收：return+ACK 调度反转不误报 accepted；确认丢失重试安全；网络黑洞超时有界；旧 ACK 不污染新请求；拓扑错误不触发自动删队列。

P3.1 版本与环境见 [MQ 测试环境](mq-test-environment.md)，P3.2 拓扑见 [本地 MQ 拓扑](mq-local-topology.md)，P3.3 判定见 [MQ publisher](mq-publisher.md)，P3.4 I/O/关闭及自动恢复见 [MQ session](mq-session.md) 和 [MQ supervisor](mq-supervisor.md)，P3.5.1 配置见 [MQ 应用配置](mq-application-config.md)，P3.5.2 见 [独立消费者连接](mq-consumer-session.md) 与 [消费 channel / 手动 ACK](mq-consumer-channel.md)，P3.5.3.1 见 [运行模式配置与启动检查](mq-runtime-modes.md)，P3.5.3.2.1 见 [消费者参数与 MQ 依赖计划](mq-runtime-parameters.md)，P3.5.3.2.2 见 [MQ 连接准备与失败清理](mq-runtime-resources.md)，P3.5.3.3.1 见 [MQ 托管生命周期](mq-lifecycle.md)，P3.5.3.3.2.1 见 [角色构造器与服务实例检查](runtime-assembly.md)，P3.5.3.3.2.2 见 [服务执行与退出编排](runtime-execution.md)，P3.5.3.3.3.1 见 [进程入口分流与现代信号](runtime-process-entry.md)，P3.5.3.3.3.2 见 [legacy 统一生命周期与信号](legacy-runtime-lifecycle.md)。默认配置保持旧链路，显式 api 模式已装配确认式接受入口；P3 基础设施与接受入口范围已验收收口，业务提交后 ACK 与端到端恢复仍待后续阶段。P3.5.4.1 [publisher 周期拓扑复核](mq-topology-monitor.md)已完成，P3.5.4.2 [接受就绪汇总与健康接线](runtime-readiness.md)也已完成装配基础，默认缺失业务能力仍不就绪；P3.6.1 [独立确认式 HTTP 发送适配](command-submission.md)及客户端协议探测已完成隔离验证；P3.6.2 [生产 API 生命周期、Redis GET 鉴权探测与运行时装配](api-runtime-lifecycle.md)已完成；阶段证据与后续边界见 [P3 验收对照](mq-reliability-acceptance.md)，下一项为 P4 持久化消费适配。

### P4：持久化 consumer、失败隔离与恢复

- [x] 将 MQ delivery 调用 P2 事务仓储，事务成功后单条 ACK；可注入运行时角色，默认 worker/all 门禁保持。
  - [x] P4.1 持久化消费适配；隔离/Outbox 等必需角色未完成时保留 worker/all 门禁。
    - [x] P4.1.1 单条 delivery 的严格 wire 校验、P2 完整幂等事务、终态门禁和提交后 ACK；实际子进程提交前/后强杀、COMMIT 结果错误、并发幂等和数据库离线恢复见 [持久化消费适配](mq-persistence-consumer.md)。
    - [x] P4.1.2 顺序 worker 的受控准入、停止/drain/取消/join、角色工厂和有新鲜度的本地就绪；多 worker 组遇错停止领取，真实提交屏障、ACK 丢帧重投和 RunRuntime drain 见 [生命周期适配](mq-persistence-lifecycle.md)。
- [x] 区分权限拒绝、内容冲突、数据库暂时故障和技术坏消息；分类本身不授权隔离/ACK。
  - [x] P4.2.1 类型化失败分类、回滚未知标记、同 delivery/context 的有界事务冲突重试；实际死锁/锁超时、耗尽、约束失败、提交/回滚响应错误及退避中退出见 [分类与本地重试](mq-persistence-retry.md)。
- [x] 实现暂停/退避/恢复、可靠隔离转移、隔离记录 worker 和受控重放 CLI；完整默认角色装配与整体退出验收仍依赖 Outbox。
  - [x] P4.2.2 数据库故障暂停、退避、恢复及消费者监督；避免重投风暴，不能用测试占位角色放开生产门禁。
    - [x] P4.2.2.1 独立持久化监督器：数据库故障撤销就绪、串行退役与有界抖动探测；显式注入每轮消费者资源来源，旧任务/资源释放调用真实返回后才允许新一轮，停止/超时期间不得复活。实际数据库停机/恢复、提交未知重投及退出见 [持久化监督器](mq-persistence-supervisor.md)。
    - [x] P4.2.2.2 生产消费者连接替换与运行时接线：拓扑复核、部分连接回滚、可替换资源所有权与聚合就绪，避免引用初始 session；保留缺失角色门禁。显式监督工厂、真实 MySQL/MQ 连接替换及握手黑洞清理见 [运行时连接来源](mq-persistence-connections.md)。
    - [x] P4.2.2.3 监督恢复的运行时退出验收：连接替换/探测/SQL/ACK 与 drain、强制退出、pending 的交叉故障，明确共享存储最终释放责任；实际 COMMIT/Ping 屏障、ACK 丢帧与握手黑洞见 [退出验收](mq-persistence-shutdown.md)。
  - [x] P4.2.3 可靠隔离转移：保留原文/headers、稳定 transport identity、目标 confirm 且无 return 后才 ACK 原投递；目标故障/确认未知保留原消息。拆分与协议见 [隔离信封](mq-quarantine-envelope.md)，运行时交叉故障见 [转移验收](mq-quarantine-acceptance.md)。
    - [x] P4.2.3.1 隔离信封与稳定传输身份：完整原文、带类型 headers、来源元数据、稳定重投/DLX 身份、严格解码及有界输入；纯函数验证，不改变 ACK 或默认装配。
    - [x] P4.2.3.2 隔离目标可靠发布：独立发布通路，持久化/mandatory、有界 I/O、confirm 与 return 合并判定及 unknown 责任；共享确认内核与真实四场景见 [隔离发布](mq-quarantine-publisher.md)。
    - [x] P4.2.3.3 消费处置接线：明确失败分类，转移确认且无 return 后才 ACK 原 delivery；失败/取消不丢弃原投递。显式借入 publisher、保守数据约束白名单及真实六场景见 [消费处置](mq-persistence-disposition.md)。
    - [x] P4.2.3.4 可靠隔离转移故障验收：真实目标不可用/Return/NACK/确认未知、源连接退役、ACK 丢帧、正常 drain/pending 及自动 DLX 身份对照；九项生产工厂到 RunRuntime 场景及未覆盖边界见 [转移验收](mq-quarantine-acceptance.md)。
  - [x] P4.2.4 隔离记录 worker：区分主动 wrapper/自动 DLX 原文，核验稳定身份并幂等保存完整 body/带类型 headers，审计 SQL 提交后才 ACK 隔离队列；实际提交/ACK 丢失、进程故障与双角色交叉验收见[审计故障验收](mq-quarantine-audit-acceptance.md)。
    - [x] P4.2.4.1 输入解析与身份契约：严格区分两种格式，校验 wrapper 稳定身份与 DLX 最近来源，保留原文、带类型 headers 和属性，提供稳定内容比较材料及完整校验后的可选业务身份；纯函数/真实 broker 边界见[隔离审计输入](mq-quarantine-input.md)。
    - [x] P4.2.4.2 幂等隔离审计事务：保存完整源快照、当前投递观察及精确稳定内容，同 transport ID 核对内容而不覆盖原文；重复提交、并发、内容冲突、取消和 COMMIT 未知均保留责任。现有 schema 的版本化审计 JSON、事务及真实 MySQL 验收见[隔离审计事务](mq-quarantine-audit.md)。
    - [x] P4.2.4.3 隔离 worker 生命周期与角色工厂：实际审计提交后才 ACK 原隔离投递，数据库失败暂停/恢复、独立消费连接、多 worker、就绪及 drain/pending 的资源所有权；显式生产工厂、双角色独立来源及真实验收见[隔离生命周期](mq-quarantine-lifecycle.md)。默认装配门禁按完整角色实现情况保持。
    - [x] P4.2.4.4 审计故障验收：真实提交前/后与 ACK 前故障、COMMIT 未知、重复 wrapper/自动 DLX、内容冲突和数据库停机恢复；实际 SQL OK 丢响应、第二轮真实 ACK 丢帧、子进程强杀及双生产角色恢复/退出九项核对唯一审计和完整原文，不以 fixture ACK 代替事务证明，详见[审计故障验收](mq-quarantine-audit-acceptance.md)。
  - [x] P4.2.5 受控重放 CLI：列举/查看/选择审计记录，保持业务身份，确认式重发并记录独立 replay attempt；确认业务终态后才 resolved，未知结果保留责任。实现及真实故障收口见[重放验收](mq-quarantine-replay-acceptance.md)。
    - [x] P4.2.5.1 只读查询与选择：SQL 元数据状态筛选/有界游标分页、精确 transport ID 详情、完整审计/冗余字段核验及显式原文展示；命令只连接 MySQL，不修改状态，见[隔离审计查询](mq-quarantine-query.md)。
    - [x] P4.2.5.2 确认式重发与独立 replay attempt：重新核验精确选择与期望旧 attempt，保持业务身份/hash，独立生成物理尝试，confirm 且无 return 后事务核验旧快照并记录 replay_pending；原文/状态冲突和未知结果保留责任。CLI、真实重发/Return 与边界见[受控重发](mq-quarantine-replay.md)。
    - [x] P4.2.5.3 终态核验与 resolved：同一事务锁定核验完整审计与匹配身份/hash 的规范 persisted/rejected，persisted 还校验实际消息；仅更新解决状态/时间，保留原文/attempt/业务终态，并发解决和重复核验不改首次时间，见[终态核验](mq-quarantine-resolve.md)。
    - [x] P4.2.5.4 重放故障验收：实际 CLI confirm/Return/NACK、COMMIT/ROLLBACK OK 丢失、确认后强杀、双进程重放、迟到回调与解决交叉；十二项核对 12 条审计、16 条实际主队列物理投递和唯一业务集合，见[重放验收](mq-quarantine-replay-acceptance.md)。
- [ ] P4 整体优雅退出和 DB 提交不确定恢复；持久化/隔离监督恢复、退出及受控重放已覆盖，Outbox 和整体角色交叉仍待 P5 接入后验证。

验收：杀死提交后 ACK 前的 worker，重投只形成一条业务消息；DB 故障期间可缓冲已登录用户发送，恢复后完成业务处理；隔离转移目标故障不丢原消息。

### P5：Outbox、通知与服务端 ACK

- [ ] P5.1 lease + token + SKIP LOCKED 的 Outbox worker。
  - [x] P5.1.1 有界原子领取仓储：数据库 UTC 时钟、READ COMMITTED/SKIP LOCKED、每行独立 token、过期回收和整批提交门禁；真实锁定/并发及边界见[Outbox 领取](outbox-claim.md)。
  - [x] P5.1.2 token 条件结算与失败回退：完成/重试更新、数据库时钟指数抖动、token/attempt/deadline fencing 与迟到/过期 owner 保护，见[Outbox 结算](outbox-settlement.md)。
  - [x] P5.1.3 通知执行循环：严格 payload/路由核验、无正文提示、事务外 Redis 发布、保守 lease 预算、有界处理和取消，见[Outbox 执行](outbox-execution.md)。
  - [x] P5.1.4 生产角色工厂与接线：独立就绪/资源来源、正常 drain/强制取消、pending 与存储最终释放责任，见[Outbox 生命周期](outbox-lifecycle.md)。
  - [ ] P5.1.5 故障验收：真实提交/发布未知、lease 过期与迟到结果、进程/角色交叉恢复及唯一集合。
- [ ] 统一 message/command.result WebSocket 帧，通知前验证权限。
- [ ] 订阅就绪、群列表刷新、单 writer、关闭清理和慢客户端策略。
- [ ] ACK 绑定当前会话和用户，数据库单调更新，提交后 ack.confirmed。

验收：提交后未通知、通知后未标完成两种崩溃均可恢复；重复通知不重复业务；旧 session/越权 ACK 被拒绝；客户端离线不阻塞 MQ ACK。

### P6：补拉协议和客户端模拟器

- [ ] 实现第 9 节接口及响应样例的契约测试。
- [ ] 模拟客户端本地持久消息集、连续游标、待发送表、ACK、重连与周期同步。
- [ ] 覆盖持续在线尾消息丢失、分页期间并发写入、乱序到达、浏览器本地存储重置。
- [ ] 输出前端接入说明；真实前端不在仓库时标注实际联调待办，不伪造完成。

验收：正确性集中的所有授权消息最终被模拟客户端保存；界面模型无重复，游标无跳洞；禁止访问的群消息不返回。

### P7：切换并退役 Redis 脏队列

- [ ] 按第 14 节停止旧写入口/cron，重置明确允许的开发消息数据并应用 schema。
- [ ] 禁止新流量双写 legacy List 和 RabbitMQ；默认配置只启用 RabbitMQ。
- [ ] 移除旧 PublishAndSave/SyncMsg 生产调用、旧历史回源和旧控制协议。
- [ ] 同步更新 README、配置示例、loadtest、[压测指南](load-testing.md)。

验收：新请求不会增加 dirty:msgs；不存在旧 cron 消费；从零启动可以复现发送、落库、ACK、补拉全过程。

### P8：故障演练、容量基线与交付

- [ ] 完成第 13 节故障矩阵，保存结构化报告与对账 ID 集合。
- [ ] 实现生产三节点拓扑/部署说明，进行 broker 单节点故障和多数不可用演练。
- [ ] 完成 loadtest 新指标和回归；基于独立环境测量发送确认、落库、端到端恢复延迟。
- [ ] 固定镜像/依赖版本，记录实际配置、故障范围、通过项与未覆盖项。

验收：不能只提交吞吐量截图；需提供状态守恒、幂等、故障恢复与部署边界的证据。

### 12.1 建议的文件拆分

文件名可按现有代码风格小幅调整，但职责与阶段不能遗漏。下列新文件均为待创建。

| 位置 | 职责 |
| --- | --- |
| `config/`、`main.go` | 显式配置、运行模式、启动/退出、依赖健康状态 |
| `models/message.go` 及新 command/conversation/outbox/cursor/quarantine 模型 | 持久结构和纯业务数据 |
| `messaging/envelope.go`、`messaging/identity.go` | 规范协议、ID/hash、纯函数校验 |
| `messaging/rabbitmq/` | 拓扑、publisher slot、connection supervisor、delivery adapter |
| `database/message_repository.go` 及后续权限/查询文件 | 幂等事务、权限、seq、status/history |
| `database/outboxRepository.go`、`database/deliveryRepository.go` | lease、游标、隔离仓储 |
| `workers/messageConsumer.go`、`workers/outbox.go`、`workers/quarantine.go` | 工作循环、错误分类与恢复 |
| `service/chat.go`、`service/messages.go` 或拆分后的 handler | HTTP/WS 协议适配，避免把 AMQP 细节写进 handler |
| `migrations/`、`cmd/migrate/` | 显式 schema 演进 |
| `cmd/mqadmin/` | 只读检查、受控重放、操作审计 |
| `tests/integration/`、`tests/fault/` | 独立环境与故障测试 |
| `docker-compose.test.yml`、`docker-compose.mq-ha.yml` | 隔离测试环境和 HA 演练环境 |

## 13. 自动化测试与故障验收

### 13.1 测试基础设施

当前 `TestSaveAndLoadMsg` 会写真实用户 1/2 的消息且未设置 Id；`TestDeleteMsg` 删除固定用户 18/9 的缓存；`TestSyncMsg` 无状态断言且被测函数吞错。必须先在 P0 隔离，不能把当前测试通过当作可靠性证据。

- 默认 `go test ./...` 只跑无外部服务的测试；集成/故障使用显式 build tag 和专用配置。
- 每次运行分配 runId，对应测试 schema/vhost/Redis prefix。拒绝运行在普通开发或生产命名空间。
- 不使用 FLUSHALL 或全局删表作为 cleanup；只回收本次创建并核实归属的资源。
- 真实 MySQL、Redis、RabbitMQ 用于跨系统语义测试；mock 用于边界、调度和错误注入，不能代替 broker 故障验证。
- 在“提交后、ACK 前”“通知后、标完成前”等位置设置仅测试构建可用的 failpoint/barrier；生产构建不可开放任意崩溃控制接口。
- 测试通过明确状态等待和超时判定，避免用一个固定 sleep 猜测后台已完成。
- 关键崩溃必须停止实际子进程/关闭连接来验证恢复，不能只在同进程调用一个返回 error 的 mock。

### 13.2 故障矩阵

| ID | 注入/场景 | 必须断言的结果 |
| --- | --- | --- |
| F01 | 旧 RPUSH 失败、缓存服务正常 | 不返回确定接受；缓存不制造“已保存”假象 |
| F02 | 旧 worker 读取后并发追加/另一任务裁剪 | 仅核对过的已处理前缀可移除；不误删新消息 |
| F03 | 旧 DB 提交后 Redis 确认失败 | 重试无重复行、不因主键冲突卡死 |
| F04 | 旧批次包含坏 JSON/内容冲突 | 原文进入可恢复隔离，其余有效消息可继续处理 |
| F05 | 发布 routing key 无绑定或 exchange 不存在 | 不返回 accepted，未就绪/明确错误，无自动删拓扑 |
| F06 | return 和 ACK 同时排队，先处理 ACK | 仍判定不可接受，无误报成功 |
| F07 | broker 接收后 confirm 丢失 | 返回 unknown；原键重试后最多一条 message |
| F08 | HTTP 202 在网络中丢失 | 客户端保存 pending，原键恢复，无第二条业务消息 |
| F09 | publish 超时后旧 ACK 到达 | 不确认新 session 的其他请求 |
| F10 | broker 背压、连接黑洞、通知消费变慢 | HTTP/写入有界超时，pool/goroutine 不无限增长 |
| F11 | DB 提交前杀 worker | 未提交数据回滚、消息重投后成功，无 seq 洞 |
| F12 | DB 提交后 ACK 前杀 worker | 一条 message、唯一 seq、唯一业务 Outbox，重投正常 ACK |
| F13 | COMMIT 响应丢失 | 按幂等恢复，不删除已提交消息，不产生新 seq |
| F14 | 多 consumer 同时处理同 key/hash | 一条业务记录，全部投递可安全结束 |
| F15 | 同 key 不同 hash，含不同会话目标 | 原消息不覆盖，错误内容查询为 ID_CONFLICT，无额外 seq/通知 |
| F16 | MQ 已接受后取消好友/移除群成员/删除群 | 新命令按消费事务权限得到稳定 rejected；无 message 行 |
| F17 | 修改请求体 senderId 或跨用户 status/ACK | 身份不能伪造，不能查询/更新他人状态 |
| F18 | MySQL 离线时启动 API | Redis/MQ 正常时已有有效 JWT 可发送 accepted；DB 接口返回503，不 panic |
| F19 | MySQL 持续故障后恢复 | 正常命令未静默丢失；积压、隔离均可对账，恢复后幂等处理 |
| F20 | Redis 黑名单查询故障 | 接受入口失败关闭，不把错误当作未封禁 |
| F21 | 主队列容量限制或 broker 磁盘告警 | 新发布拒绝或 unknown；旧接受消息不被丢队头 |
| F22 | 死信目标不可用/隔离发布确认丢失 | 原消息保留或目标已有可恢复副本；无两边都缺失 |
| F23 | 隔离记录入库后、隔离 ACK 前退出 | 重投不重复审计行，原始数据完整 |
| F24 | Outbox 发布前/后分别杀 worker | 前者 lease 恢复；后者可重复提示但不重复业务 |
| F25 | Outbox lease 到期后旧 worker 才完成 | 旧 lease token 不能覆盖新持有者状态 |
| F26 | 丢最后一条 Pub/Sub 通知，WS 一直在线 | 周期高水位校验补齐，不能永远等下一条消息 |
| F27 | WS 断线、ACK 丢失、重连反复发生 | 本地只保存一次，服务器 ACK 单调，补齐全部授权消息 |
| F28 | 先收到 seq=45，再收到43/44 | 连续游标不从42直接跳45，补拉后正确推进 |
| F29 | 补拉分页期间并发提交消息 | 每个快照范围完整；后续扫描能发现新增消息，无永久跳过 |
| F30 | Redis 仅有最新消息，旧历史在 MySQL | 同步/历史完整，缓存存在不影响正确性 |
| F31 | 用户换浏览器/本地数据清空 | 不因服务器 deliveredSeq 较大而跳过缺失历史 |
| F32 | 离群后保留旧订阅与会话 ID | 无权读取或接收新群内容；群成员可读取完整历史 |
| F33 | 旧 session 迟到 ACK、慢客户端队列满 | 旧帧不更新状态，取消有界，无订阅/goroutine 泄漏 |
| F34 | 单节点 broker 重启且数据卷保留 | 未完成命令可恢复；不把此测试称为节点容灾 |
| F35 | 生产三副本队列 leader/单节点停止 | 多数可用后恢复确认/消费；故障前已接受集合完整 |
| F36 | 三节点中多数不可用 | 不返回无依据 accepted；多数恢复后继续处理，不强制不安全恢复 |
| F37 | 关闭应用时存在发布/消费/Outbox 在途操作 | 结果为已提交或明确未完成/unknown，不留下已 ACK 未存数据 |
| F38 | 新协议图片/音频记录 | media 与内容引用正确，历史不硬编码 text |

F01—F04 验证过渡修复，最终架构以 F05—F38 为主。单节点开发环境不能代替 F35/F36；生产拓扑未演练时必须在报告中标明未覆盖。

### 13.3 对账标准

使用逻辑身份 `(senderId, clientMessageId, payloadHash)` 的集合核对，不以 AMQP delivery 数或 HTTP 请求总数作为业务消息数。

- 无业务拒绝的固定有效 fixture：所有已确认 accepted 的逻辑消息，在故障恢复并完成处理后都存在唯一 message 行和 persisted 结果。
- 带权限/冲突场景的 fixture：每个逻辑请求最终对应 persisted、明确业务拒绝/内容冲突，或有原始数据的待处理隔离记录。测试结束仍 unresolved 的记录必须列出，不能从分母移除。
- 发布 unknown 的请求单独追踪，与 accepted 集合区分；重试/查询后核实真实结果。
- 预期接收集合按 `(recipient, conversationId, seq)` 构造，群成员范围按测试设定；每位授权接收者本地唯一集合完整。
- 数据库 `conversation.last_seq` 与已提交消息连续范围一致；客户端本地连续 seq 与实际本地消息集合一致。
- 技术隔离存在可恢复记录说明“没有静默丢弃”，但未完成修复的有效消息不能计为“全部成功送达”。

### 13.4 压测工具改造

当前工具把 HTTP 失败的 pending 删除，并用 `LoadAndDelete` 忽略重复接收，不能直接验证新契约。至少增加：

- 按新接口创建消息并记录 accepted/unknown/rejected；保留未确定请求。
- 建立真实好友/群 fixture；现有仅注册环形用户的方案会被新好友校验拒绝。
- 处理 command.result、发送业务 ACK、自动重连与周期补拉；提供仅稳定连接、正常流量、恢复验证三种模式。
- 分开统计唯一逻辑发送数、物理发布/接收重复数、persisted、业务拒绝、补拉恢复、最终 ACK、quarantine、unresolved。
- 分开测量 HTTP 接受延迟、接受到落库延迟、客户端实际保存的端到端延迟、恢复耗时；采用测试端单调时钟测端到端，避免跨机器墙钟差。
- 恢复等待阶段收到的消息单列，不能除以纯发送时长后称为稳态吞吐；正常稳态吞吐与最终送达率分别计算。
- 输出结构化 JSON 与 ID 对账报告，记录 commit、工具链、镜像、日志配置、资源、消息大小、并发、故障时间与恢复时间。

### 13.5 完成测试隔离后的命令约定

以下命令是 P0/P8 应交付的使用方式，当前仓库尚未实现全部文件和测试标签：

```powershell
go test ./...
docker compose -f docker-compose.test.yml up -d --wait
go test -tags=integration ./tests/integration/...
go test -tags=fault ./tests/fault/...
go vet ./...
```

并发相关测试在支持 race detector 的环境执行 `go test -race`；Windows 缺少所需 C 工具链时可使用明确记录的 Linux CI，不把跳过 race 写成通过。新脚本应显式选择测试配置，不依赖用户当前 `.env` 恰好指向安全环境。

## 14. 开发环境切换、发布和回滚

### 14.1 本项目的切换方式

用户允许清空开发数据和直接修改接口，默认使用受控停发后的重置切换，不实现在线双写迁移。

1. 盘点目标环境、数据库名、Redis 地址/namespace、MQ vhost；确认它们确实属于本次开发环境。数据清空授权不等于可以删除其他项目或共享实例的数据。
2. 停止旧发送入口与全部旧 cron，等待在途请求结束。保存必要配置和便于回滚的环境快照；本次文档编写不执行这些操作。
3. 重置消息相关开发表及旧 `dirty:msgs`、`msg:*`、`msg:v2:*` 数据，应用新 migration；测试用户/好友/群按 fixture 重建。需要重建整库时先在重置脚本中明确目标与影响范围。
4. 创建并验证 MQ 拓扑与策略；启动 worker、Outbox，再启动新接受入口/网关。
5. 使用新协议模拟器或已适配前端执行冒烟测试：accepted → persisted → 接收 → ACK → 重连补拉。
6. 检查新请求不再进入 dirty:msgs，旧 cron 不运行，旧历史路由已退役。
7. 留存故障测试报告后再开展性能压测。

脚本必须在删除前显示/校验解析后的明确目标，默认只作用于专用开发资源。不提供“对当前默认连接执行 FLUSHALL”这类隐含范围的操作。

### 14.2 本地与生产部署交付

| 环境 | 必须交付 |
| --- | --- |
| 本地单节点 | Compose 服务、持久卷、健康检查、独立账号、明确 quorum size=1、启动说明 |
| 独立测试 | 独立端口/数据库/vhost、可重复 fixture、故障注入、只清本轮资源 |
| 生产三节点 | 稳定节点名、集群发现/加入、受控 cookie/凭证、独立卷与故障域、三副本验证、TLS、监控与恢复步骤 |

生产三节点必须验证实际 queue members 和多数故障行为。三台 RabbitMQ 节点不能自动赋予单节点 MySQL 或 API 高可用。应用首版单 API 实例的限制应写入部署说明。[RabbitMQ 生产部署要求](https://www.rabbitmq.com/docs/production-checklist)

### 14.3 回滚原则

- 停止新接收，保留所有已确认 MQ 命令、未确认 delivery、Outbox 和隔离记录。
- 优先回滚到兼容当前 MQ/schema/信封版本的修复版本；禁止直接启动旧 Redis-only 二进制并遗弃 MQ 中的接受责任。
- 若数据库 schema 需要回退，先验证兼容与恢复路径，不能破坏已确认命令的幂等记录。
- 纯开发环境选择彻底重置时，报告此次测试数据被有意丢弃；这不是可靠性恢复成功。
- 不在生产以删除队列、清空 Outbox、删除唯一约束或强制不安全选主作为恢复步骤。

## 15. 接手 agent 检查清单

开始工作时：

1. 阅读本节、第 1 节决策和第 12 节阶段，重新查看 git status 与适用 AGENTS.md。
2. 核对当前代码是否仍与文档基线一致；已有用户修改不得覆盖。
3. 默认从 P0 开始。先隔离当前会触碰真实数据的测试，再运行测试套件。
4. 只在当前阶段需要时引入依赖和文件；不要同时把 MQ、schema、客户端协议全部改完才第一次验证。
5. 每阶段更新本文勾选项或独立实施记录，记录实际命令、测试结果、未覆盖环境和下一阶段。

完成实现前必须能够回答：

- [ ] 一次 HTTP 202 的具体成功证据是什么？如何排除 return？
- [ ] 发布结果未知时客户端用什么键恢复？何时才可以换新 ID？
- [ ] DB 提交后、MQ ACK 前退出，为什么不会生成两条消息？
- [ ] 同一个 ID 不同内容并发到达，哪条记录保持规范结果？
- [ ] 权限在排队期间改变，最终状态如何查询？
- [ ] Redis 和 WebSocket 的最后一次通知丢失，持续在线客户端怎样恢复？
- [ ] seq 如何避免晚提交记录被水位跳过？本地数据清空如何恢复？
- [ ] 队列满、数据库长期故障、隔离目标故障时，原始数据由谁负责？
- [ ] 多 consumer、Outbox lease 到期和旧 session 的迟到操作是否都有测试？
- [ ] 所有旧 dirty 队列写入/定时消费是否已从生产路径退役？
- [ ] 本地单节点与生产三节点各验证了哪些故障，哪些没有验证？
- [ ] 真实前端是否已接入？若仅模拟器完成，报告是否明确？

只有上述边界明确、故障矩阵有结果且不存在未说明的数据丢失/重复副作用，才能称为完成本次可靠性改造。

## 16. 官方资料与版本核查

本文的业务协议、数据模型、分阶段任务与验收矩阵是针对当前仓库设计的实施规范，不是中间件官方提供的端到端保证。中间件事实与 API 边界应以固定版本源码和以下官方文档核验。

- [RabbitMQ publisher confirm 与 consumer ACK](https://www.rabbitmq.com/docs/confirms)：确认边界、return、手动 ACK。
- [RabbitMQ Reliability](https://www.rabbitmq.com/docs/reliability)：重复投递与幂等责任。
- [RabbitMQ Quorum Queues](https://www.rabbitmq.com/docs/quorum-queues)：副本、容量、delivery-limit 与可靠死信。
- [RabbitMQ Production Checklist](https://www.rabbitmq.com/docs/production-checklist)：生产部署约束。
- [amqp091-go v1.15.0](https://github.com/rabbitmq/amqp091-go/tree/v1.15.0)：P3.1 已固定并核验的客户端源码。
- [Redis Pub/Sub](https://redis.io/docs/latest/develop/pubsub/)：通知遗漏的语义。
- [Redis Persistence](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)：旧方案的刷盘边界。
- [MySQL Locking Reads](https://dev.mysql.com/doc/refman/8.0/en/innodb-locking-reads.html)：事务锁与 SKIP LOCKED 的适用范围。
- [AWS Transactional Outbox](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/transactional-outbox.html)：数据库与通知双写问题。

实施时如果官方版本/API 已变化，先复核版本兼容和相关故障测试，再修改设计；不能仅把依赖改成 latest 并沿用旧版本保证。
