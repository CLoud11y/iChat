# P6.1：发送状态 HTTP 接口

现代 API 注册 `GET /auth/messages/status?clientMessageId=...&payloadHash=...`，通过真实 `MessageRepository.GetCommandStatus` 只读查询 JWT 用户自己的逻辑命令。默认配置仍为 legacy，本项不启用完整默认 worker/all。

请求须提供 `Authorization: Bearer <JWT>`，不接受 query/form token。只允许两个各出现一次的参数，拒绝 senderId/userId、未知参数、重复键、非法 URL 编码及超过 1024 字节的原始 query。clientMessageId 沿用 UUID 规范化（大写可转小写）；payloadHash 为 64 字符小写十六进制。鉴权先于参数解析，不读取 GET body。

## 响应契约

所有响应为 JSON `code/msg/data`，带 `Cache-Control: no-store`。

| HTTP | code / data.state | data 附加字段 |
| --- | --- | --- |
| 200 | 0 / persisted | 请求 key/hash、完整规范 message DTO |
| 200 | 0 / rejected | 请求 key/hash、稳定 reasonCode；无 message |
| 200 | 0 / quarantined | 请求 key/hash、pollAfterMs=2000；无隔离原文和诊断 |
| 200 | 0 / pending_or_unknown | 请求 key/hash、pollAfterMs=2000；不证明 MQ 中无命令 |
| 409 | -1 / ID_CONFLICT | 仅本次请求 key/hash 和 state；无原 hash、message、拒绝原因或轮询字段 |
| 400 | -1 / 无 state | data.reasonCode=INVALID_REQUEST |
| 401 | -1 / 无 state | data.reasonCode=UNAUTHENTICATED |
| 503 | -1 / 无 state | data.reasonCode=DEPENDENCY_UNAVAILABLE |

例如尚无规范终态或匹配隔离记录时：

```json
{
  "code": 0,
  "msg": "command status",
  "data": {
    "clientMessageId": "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1",
    "payloadHash": "55405570ca8bd409cbc5ed2f2b5914c09b4b5141fb4b377ef6d46045550ad591",
    "state": "pending_or_unknown",
    "pollAfterMs": 2000
  }
}
```

数据库不可用、快照释放失败、损坏/矛盾 DTO、异常及超时返回 503，不返回 pending、部分正文或 SQL 错误。409 的稳定冲突类型由 `data.state=ID_CONFLICT` 表示，保持 P2.6 DTO；状态接口不返回 accepted/publish_unknown/not_published。

## 读取与生命周期

仓储沿用单个只读 REPEATABLE READ 快照：先查规范命令，同键异 hash 为冲突，同 hash 返回原终态；不存在时才查匹配用户/key/hash 的未解决隔离记录。接口不扫描 MQ、不写命令/游标/Outbox，也不按 publisher 或缓存的 storage readiness 阻止实际 SQL 查询。Redis 仅用于真实 JWT 黑名单鉴权。

自己的既有终态不随好友解除、退群或删群重新判断；persisted 保留规范 DTO、字符串 ID/seq 和 UTC 微秒时间。通知和历史/同步仍遵循各自权限。`CommandStatus.ValidateFor` 在 HTTP 出站前核验请求身份、状态字段组合以及 persisted 消息的身份/hash/规范格式，损坏适配器不能泄露另一请求的结果。

API 默认 `StatusTimeout=3s`，同一父 deadline 覆盖 Redis 鉴权和 SQL，独立于发送确认超时。内部选项须在 (0,1min]；HTTP WriteTimeout 须覆盖 ReadTimeout 加较大的 SubmissionTimeout/StatusTimeout。本项没有新 YAML/env 配置，原默认写期限满足要求。

状态 reader 闲置构造，借入 API SQL 池，不新建资源生命周期或迁移。缺失/typed nil/失败工厂进入原启动回滚。回调即使违反 context，handler 也持有原 owner 到真实退出，超时后的 nil error 不能变成成功。状态请求的停止准入/Panic 统一返回依赖不可用；发送接口保留原 unknown 语义。observer pending 不允许提前关闭 SQL/Redis。

## 验证与边界

```powershell
go test -mod=readonly -p=2 -race ./service ./router . -run '^Test(CommandStatus|APIStatus|APIStartup|APIValidation)' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionCommandStatusIntegration$' -count=1 -v -timeout=7m
```

真实默认 API suite 六组覆盖五类状态与 SQL DTO、跨用户/参数/撤销 token、终态优先及后续权限变化、损坏消息 503、实际 Redis 客户端故障与新 API 恢复，以及 MySQL 停机期间新旧 API 503、重启恢复。Docker 若更换映射端口则显式重建 API，不声称原池能自动发现新地址。

业务通过生产 PersistCommand 建立；两条隔离行是明确的状态元数据夹具，未经过 MQ/审计 worker。最终 3 command / 2 message / 2 conversation / 5 untouched pending Outbox / 2 quarantine / 0 cursor，schema 不变。默认测试另覆盖 uint64 上界、矛盾/跨请求输出、迟到回调、工厂失败、Panic、停止准入和 pending 后客户端延迟释放。

运行证据见[实施记录](message-reliability-mq-progress.md)。P6.2 已拆分，P6.2.1 [会话发现仓储](conversation-discovery.md)已完成；P6.2.2 [会话列表 HTTP](conversations-http.md)也已完成；P6.2.3.1 [补拉 HTTP](message-sync-http.md)已完成，P6.2.3.2 [历史 HTTP](message-history-http.md)已完成，下一项 P6.3 模拟客户端发送恢复。现有 loadtest acceptance 模式仍只探测发送确认；自动状态轮询、客户端恢复和完整全管线交叉在后续实施。
