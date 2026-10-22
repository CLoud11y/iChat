# P4.2.4.1：隔离审计输入契约

`rabbitmq.ParseQuarantineInput(delivery, receivedAt)` 解析隔离队列的主动 wrapper 与 broker 自动 DLX 原文，返回不可变 `QuarantineInput`。本项提供解析、稳定内容核对材料和经过校验的可选业务身份；后续[P4.2.4.2 审计事务](mq-quarantine-audit.md)及[P4.2.4.3 生命周期与提交后 ACK](mq-quarantine-lifecycle.md)已实现。

## 输入判定

两种格式均要求当前路由为固定 `QuarantineExchange / QuarantineRoutingKey`，观察时间非零且可表示为 UTC JSON 时间。调用者仍须从已验证拓扑的隔离队列取得真实 `ConsumerDelivery`；结构体、路由和 headers 不是来源认证或数字签名。

| 输入 | 必须满足 | 失败行为 |
| --- | --- | --- |
| 主动 wrapper | ContentType 或 Type 任一命中即进入 wrapper 分支；两个标记须完整匹配 v1，持久化、无编码/TTL/优先级/ReplyTo/UserID/AppID/Timestamp，CorrelationID 为 32 位小写十六进制 attempt | 部分标记或损坏 wrapper 不回退到原文分支 |
| wrapper 内层 | 严格规范 v1 解码、重新计算 transport ID，外层 MessageID 必须一致；受控错误码仅 `INVALID_DELIVERY` / `DATA_CONSTRAINT` | 篡改、未知版本或错误码返回失败 |
| 自动 DLX 原文 | 无 wrapper 标记，非空 `x-death` 数组，各项 queue/exchange/reason/count/time/routing-keys 类型和大小合法；最新项来自固定主队列、命令 exchange 和 submit 路由 | 缺失、陌生或自相矛盾的来源返回失败 |
| DLX 最近来源 | 根 `x-last-death-{queue,exchange,reason}` 与最新项一致；count 为正 int64，time 非零；历史项仍校验结构，允许更早的其他来源 | 不根据正文外形猜测格式；不把无来源的原文当死信 |

DLX 的四种 reason 映射为 `BROKER_REJECTED`、`BROKER_EXPIRED`、`BROKER_MAXLEN`、`BROKER_DELIVERY_LIMIT`。`x-first-death-*` 保留为原始元数据，不用于替代最近来源判定。字段约定参考固定 broker 的 [RabbitMQ DLX 元数据说明](https://www.rabbitmq.com/docs/dlx#effects)，实际 rejected/expired 已有下述真实 broker 验证。

所有失败返回零输入和可由 `errors.Is(err, ErrQuarantineInput)` 判断的错误，不输出原文/header/底层错误值。超限、不支持的 header 类型、循环引用均失败，不截断。解析函数没有 ACK 能力；调用者遇到失败必须保留当前投递责任，后续 worker 应停止并告警，不能以“解析完成”作为审计成功。

## 保留内容与身份

`Envelope()` 返回原命令快照：wrapper 保留内层源数据，DLX 保留实际收到的原文、属性、死信头和当前路由。`Delivery()` 返回当前隔离投递的完整副本，包含 wrapper 的外层 body/headers/属性。两者均无 AMQP Acknowledger，修改返回的 byte slice、table 或 array 不会改变私有快照。AMQP 时间按 UTC 规范化；不保留 Go Location 指针或 broker 原始 frame。

wrapper 的 `Envelope().ObservedAt()` 是源处置观察时间，`ReceivedAt()` 是本次审计接收时间；自动 DLX 没有原应用观察时间，用接收时间构造快照，不从死信 time 伪造应用事件。重投保留原文，新的观察不影响 transport ID。

`QuarantineEnvelope.StableContent()` 返回 v1 transport ID 的规范哈希输入内容（不含命名域前缀），其可变字段排除规则见[隔离信封](mq-quarantine-envelope.md)。构造器和比较材料共用同一实现，固定 v1 哈希向量保持不变。未来写库须比较稳定内容和既有记录，不以主键相同就覆盖原文；可变死信/重投信息不能被当作新的业务版本。

输入 body 最大 4 MiB，headers 深度 16、累计节点 4096。原文分支沿用完整信封的累计与编码预算。wrapper 外层 body 已是受限规范信封，另以独立 4 MiB 累计预算捕获外层元数据，避免再次 base64 包装 body 后误拒绝合法大信封。该分支的 body 与元数据分别有界，没有共用 4 MiB 总和限制。

`Identity()` 只在源 wire 完整通过命令解码、摘要重算、规范身份和 AMQP MessageID/CorrelationID 校验后返回 senderId、clientMessageId、commandId、payloadHash。坏 JSON、摘要/AMQP 身份不符时仍保留审计快照，但不提供任何部分业务身份。DLX 只有在最近来源校验成功后，才在临时身份候选上恢复命令路由；保存的源快照仍是实际 DLX 路由。这些字段用于关联查询，不代表验证了发送人的授权或允许重放。

事务必须保留完整源快照及带类型 headers、当前投递观察和精确稳定内容，不能只存 body/哈希后丢弃 AMQP 属性。[P4.2.4.2](mq-quarantine-audit.md)已实现现有 schema 上的版本化审计 JSON 与幂等保存，`EncodeAudit()` / `DecodeQuarantineAudit()` 保留完整观察并兼容 MySQL 外层 JSON 重排。

## 验证

固定 Go 1.27.1，清空 GOROOT，使用仓库 `.gocache` 和不存在的 `ICHAT_CONFIG_FILE`，不读取开发 .env/config：

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run '^TestQuarantine' -count=30 -timeout=120s
go test -mod=readonly ./messaging/rabbitmq -run '^$' -fuzz '^FuzzParseQuarantineInput$' -fuzztime=15s -parallel=2
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestQuarantineInputIntegration$' -count=1 -v -timeout=8m
```

默认测试覆盖两格式等价身份、全部支持类型的无损副本、四种 DLX 原因及历史、可选身份校验、部分标记/篡改拒绝、零值/时间/超限/循环边界和大 wrapper 的独立元数据预算。真实 suite 使用 RabbitMQ 4.3.6 / amqp091-go v1.15.0，六项覆盖主动坏正文/有效源 wrapper 及未 ACK 重投、实际 rejected/expired 原文、有效命令的真实 DLX、篡改 wrapper 解析失败后保留原投递。队列 ACK 仅为 fixture 检查清理；没有执行审计 SQL、生产隔离 worker、CLI 重放或三节点故障验收。

实际运行结果、失败夹具修正和资源清理记录见[实施进度](message-reliability-mq-progress.md)。后续[审计事务](mq-quarantine-audit.md)、[隔离生命周期](mq-quarantine-lifecycle.md)及[审计故障验收](mq-quarantine-audit-acceptance.md)已完成，P4.2.4 范围收口；[P4.2.5.1 只读查询与选择](mq-quarantine-query.md)、[P4.2.5.2 确认式重发](mq-quarantine-replay.md)及[P4.2.5.3 终态核验](mq-quarantine-resolve.md)已完成；[P4.2.5.4 重放故障验收](mq-quarantine-replay-acceptance.md)已完成，P4.2.5 范围收口；[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；下一项 **P5.1.2：token 条件结算与失败回退**。
