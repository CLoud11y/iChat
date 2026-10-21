# 隔离信封与稳定传输身份（P4.2.3.1）

`messaging/rabbitmq.NewQuarantineEnvelope` 将原 `ConsumerMessage` 转为独立 v1 隔离信封，不要求 body 可解析或含有可信业务身份。构造、编码和解码均不联网，没有 SQL、发布或 ACK 副作用。后续 [P4.2.3.2](mq-quarantine-publisher.md)提供独立隔离目标发布；[P4.2.3.4](mq-quarantine-acceptance.md)已收口隔离 MQ 转移范围，下一项为隔离记录 worker。

## 范围与交接

| 子项 | 责任与完成标准 | 当前状态 |
| --- | --- | --- |
| P4.2.3.1 | 原文、带类型 headers、来源属性与稳定 transport identity；纯函数往返与边界验收 | 已完成 |
| P4.2.3.2 | 独立隔离 publisher；持久、mandatory、有界 I/O；confirm 与 return 合并判定，失败/未知不得视为转移成功 | 已完成，见[隔离发布](mq-quarantine-publisher.md) |
| P4.2.3.3 | 消费失败分类与转移接线；仅目标确认且无 return 后调用原 delivery 单条 ACK；取消/失败保留原责任 | 已完成，见[消费处置](mq-persistence-disposition.md) |
| P4.2.3.4 | 实际 MySQL/RabbitMQ 目标故障、确认丢失、重复转移、ACK 未知及退出交叉验收 | 已完成，见[运行时验收](mq-quarantine-acceptance.md) |

隔离记录 worker、审计幂等写库、受控 CLI 重放与 Outbox 另属后续任务。默认 worker/all 的缺失角色门禁保留。本项不改变持久化 handler、监督器、主程序装配、broker 拓扑或数据库 migration。

## v1 快照协议

外层 JSON 包含 `version=1`、`transport_id`、`error_code`、UTC `observed_at` 与 `source`。错误码仅接受 1—64 个大写 ASCII 字母/下划线；调用者必须使用受控分类，不传入底层 SQL、正文或凭证。时间和错误码用于审计，不参与传输身份。该接口不决定哪些失败允许隔离。

`source.body` 是 base64 字节串，坏 JSON、NUL、无效 UTF-8 均保留。来源属性字符串和 header 名称/字符串也按原始字节编码，避免 JSON 的 Unicode 替换改变输入。

`source.text` 的 v1 固定位置如下，修改位置或编码需要新版本：

| 下标 | 来源属性 |
| --- | --- |
| 0 / 1 | ContentType / ContentEncoding |
| 2 / 3 | CorrelationID / ReplyTo |
| 4 / 5 | Expiration / MessageID |
| 6 / 7 / 8 | Type / UserID / AppID |
| 9 / 10 / 11 | ConsumerTag / Exchange / RoutingKey |

Timestamp 按 UTC `time.Time.MarshalBinary` 保存，另保存 DeliveryMode、Priority、DeliveryTag 和 Redelivered。时间保留实际解码后的值；时区/单调时钟元数据不属于 AMQP 传输内容。快照保存的是消费 API 提供的完整数据，不声称保留 broker 原始 frame 或 header table 原始排列。

headers 使用递归带类型节点：table 按原始 key 字节排序；array 保留顺序；整数保留宽度和符号；浮点保留二进制位（含 NaN、Infinity、负零）；decimal 保留 Scale/Value；时间归一到 UTC；字符串和 byte array 各自区分。支持固定 amqp091-go v1.15.0 `Table.Validate` 接受的全部 Go 类型（本地缓存 `types.go:416` 与 `write.go` 已核对），不增加依赖。nil 与空 table/array/byte array 也分别保存。`RawHeaders()` 返回这种带类型 JSON，适合后续 `message_quarantine.raw_headers`，不能先反序列化为普通 `map[string]any` 再当成 AMQP headers。

`QuarantineEnvelope` 只持有私有编码；`Encode()`、`Source()` 与 `RawHeaders()` 返回独立数据。输入和返回的 body、nested table、array 或 byte slice 的后续修改均不影响快照。Source 是数据副本，没有原 channel/tag 的 ACK 能力；后续处置必须继续持有原 `ConsumerDelivery`。正常 `%v`、`%+v`、`%#v` 输出仅包含 transport ID。

## transport identity

身份格式为 `mq:v1:` 加 64 位小写 SHA-256，长度 70，符合现有 `transport_id varchar(128)`。它属于固定命令流 `ichat:command-transport:v1` 命名域，基于规范编码的 body、稳定属性和应用 headers 计算；不依赖 body 中可伪造的 JSON 身份，也不直接采用可能缺失/冲突的 MessageID 或 attempt ID。

以下元数据完整留在快照，但从身份输入中排除：

- ConsumerTag、DeliveryTag、Redelivered，以及消费时 Exchange/RoutingKey。
- Expiration（自动 dead-letter 时原 TTL 会被移除）。
- 根 headers 的 `x-delivery-count`、`x-acquired-count`、`x-death`、`x-first-death-{exchange,queue,reason}`、`x-last-death-{exchange,queue,reason}`。
- 根 headers 的 CC/BCC 路由副本信息。嵌套 table 中同名应用字段仍参与身份。

因此同一原始投递在连接替换、重投、自动 DLX 和主动转移重试时可复用一个 ID，审计时间与分类变化也不生成新身份。原始 body、业务 MessageID、物理 CorrelationID 或应用 header 值/类型变化会生成不同身份；header map 的插入顺序不会影响身份。测试固定一个 v1 哈希向量，防止升级时悄悄改变已存审计主键。排除项依据官方 [DLX 对消息的修改](https://www.rabbitmq.com/docs/dlx#effects)及 [RabbitMQ 4.3 quorum 计数](https://www.rabbitmq.com/docs/4.3/quorum-queues#poison-message-handling)核对；4.3 新增的 acquired count 同样属于可变元数据。

若外部发布方完全不提供可区分的物理 attempt 信息，且两次输入的全部稳定内容完全一致，只能归为同一个传输身份，不能从 delivery tag 推断独立发送次数。正常命令 publisher 每次生成新 attempt ID，不受此限制。该哈希不是签名，也不是业务幂等键；后续审计写库须核对相同身份的稳定内容，不得用 upsert 覆盖既有原文。死信头和路由变化可作为重复观察信息，不能冒充原始命令的新版本。

## 解码与失败边界

`DecodeQuarantineEnvelope` 只接受构造器输出的规范 v1 JSON；解码来源后重新构造并逐字节比较，拒绝身份篡改、未知/重复字段、不支持版本、额外 JSON、非规范编码、未知类型及错误长度。它不用于接收 broker 自动 DLX 的原命令；未来隔离 worker 需要区分主动包装与自动 DLX 原文。

单个编码信封最多 4 MiB；构造过程有同量级累计字节预算，headers 最大递归深度 16、累计节点预算 4096，根集合也预检查数量。类型不支持、循环引用或超出边界返回 `ErrQuarantineEnvelope` 和零值，没有截断/降级快照。base64 与类型标签有额外开销，允许的原文大小会小于 4 MiB。未来接线遇到这种错误必须保留原投递责任，不能 ACK、静默丢 headers 或仅保存摘要。默认 handler 的 16 KiB 命令正文校验上限保持。

## 验证与证据范围

固定 Go 1.27.1、仓库 `.gocache` 及不存在的 `ICHAT_CONFIG_FILE`，默认测试不接外部服务：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestQuarantine' -count=30 -timeout=90s
go test -mod=readonly ./messaging/rabbitmq -run '^$' -fuzz '^FuzzDecodeQuarantineEnvelope$' -fuzztime=15s -parallel=2
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
```

覆盖坏字节/全部支持类型、超出 JSON 精度的整数、特殊浮点位、深层副本所有权、固定哈希、重投/DLX 元数据变更、物理 attempt 区分、缺失身份回退、篡改与资源边界。DLX 变化在本项是纯数据模型测试，尚未执行真实 broker 转移或 SQL 审计。本项不能作为 confirm/return 判定、原消息 ACK、隔离表落库、重放或端到端可靠性的完成证据。
