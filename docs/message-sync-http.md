# P6.2.3.1：向前补拉 HTTP

现代 API 提供 `GET /auth/messages/sync`，默认工厂从自有 SQL 池构造真实 `MessageRepository.SyncMessages`。权限、单页 RR 快照、连续范围和规范 MessageDTO 沿用 P2.7；P6.2.3.2 已完成[历史 HTTP](message-history-http.md)，下一项 P6.3 模拟客户端发送恢复。

## 请求与响应

请求必须提供 `Authorization: Bearer <JWT>`，不接受 query/form token，不读取 GET body。UID 只来自鉴权上下文；原始 query 最多 1024 字节，所有字段只允许出现一次：

| 参数 | 规则 |
| --- | --- |
| conversationId | 必填，规范 `p:<小UID>:<大UID>` 或 `g:<群ID>` |
| afterSeq | 可省略（零）；显式值须为规范十进制 uint64 字符串，允许 `0` 和最大值，拒绝空、前导零、负数、加号、空白、小数、溢出 |
| limit | 可省略（100）；显式值须为规范十进制 1—500，拒绝空和 0 |

重复/未知字段（包括 userId、beforeSeq、deliveredSeq）、大小写别名、非法 URL 编码均为 400。鉴权先于业务参数解析，合法 URL 转义可用。示例：`GET /auth/messages/sync?conversationId=p%3A1%3A2&afterSeq=0&limit=100`。

成功 HTTP 200、code=0、msg=`messages synced`，data 为 SyncPage。达到本页水位时的空页示例：

```json
{
  "code": 0,
  "msg": "messages synced",
  "data": {
    "conversationId": "p:1:2",
    "afterSeq": "4",
    "items": [],
    "nextAfterSeq": "4",
    "highWatermark": "4",
    "hasMore": false
  }
}
```

非空 items 为现有规范 MessageDTO，文本/图片/音频类型、消息身份、路由、正文/资源引用及服务端 UTC 微秒时间与 SQL/通知一致。ID 和所有序号是 JSON 字符串，空 items 保留 `[]`。先连续保存本页到本地，再推进本地游标/ACK；接口返回不证明客户端已经保存。

| HTTP | data.reasonCode | 说明 |
| --- | --- | --- |
| 400 | INVALID_REQUEST | 参数错误，或纯 afterSeq 超过本页已提交水位 |
| 401 | UNAUTHENTICATED | 缺失、无效、过期或撤销的 token |
| 403 | FORBIDDEN | 纯私聊参与者/当前群权限拒绝 |
| 503 | DEPENDENCY_UNAVAILABLE | 鉴权/SQL/释放/取消/损坏/矛盾结果/异常，或停止准入 |

错误保留 code=-1/msg/data，data 仅有 reasonCode，无部分 items、业务 state 或底层诊断。所有响应为 JSON、`Cache-Control: no-store`。不把范围错误静默夹到水位，也不把失败伪装成空页。

## 连续范围、权限与生命周期

仓储在同一个只读 REPEATABLE READ 快照检查私聊参与者或当前群成员，读取已提交水位和连续消息；当前群成员可读入群前历史，退群/删群后下一页拒绝。解除好友不撤销既有私聊历史。HTTP 不另开权限查询，不混合两个时刻。

`SyncPage.ValidateFor` 在出站前检查规范 query、会话/afterSeq 匹配、非 nil items、精确页长/hasMore/nextAfterSeq、连续 seq、重复消息身份及规范 DTO 的身份/路由/媒体/时间。正文与存储 hash 的一致性仍由仓储在原快照验证，HTTP 不再读一次 SQL。额外行也由仓储校验；若水位还有数据却缺少期望行，整页 503，不跳洞。

校验使用 `available=highWatermark-afterSeq` 的安全差值计算数量，不计算最大水位加一；afterSeq 等于最大 uint64 的合法空页不会回绕。每页独立快照，下一页可以包含新提交，不把旧水位当作永久终点。

为了区分纯客户端范围错误与基础设施错误，仓储提供 `ErrSyncCursorAhead`，继续满足 `errors.Is(err, messaging.ErrInvalidInput)`。HTTP 仅将纯范围错误映射 400、纯权限错误映射 403；与 SQL/快照释放错误混合或已经取消时统一为 503。

`SyncTimeout` 默认 3 秒，共享父 deadline 覆盖实际 Redis 鉴权、等连接和 SQL；写期限校验覆盖 ReadTimeout 加发送/状态/列表/补拉期限的最大值。没有新 YAML/env 配置、schema 或依赖。工厂闲置构造、显式借入 SQL 池，失败/nil/typed nil 进入原启动回滚。

handler 持有实际回调到真实返回，迟到 nil 结果不能成功；observer pending 不提前关闭 SQL/Redis。查询不按 publisher 或缓存探针门禁，不读取 Redis 消息缓存，也不读取/更新服务器 deliveredSeq 来决定补拉起点。浏览器本地清空后必须从实际本地游标（必要时零）重新补齐。

## 验证与范围

```powershell
go test -mod=readonly -p=2 -race ./messaging ./database ./service ./router . -run '^Test(SyncPage|MessageSync|APISync|APIStartup|APIValidation)' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionMessageSyncIntegration$' -count=1 -v -timeout=7m
```

真实默认 API 九组覆盖三类媒体与规范 DTO、服务器 ACK 较大时从零恢复、分页期间新提交、入群前历史/退群/删群/私聊参与者、损坏额外行/缺号的整页失败、实际 token 撤销、四连接池耗尽的三秒期限、Redis 客户端故障、MySQL 离线启动/重启，以及最终客户端释放。本轮 Docker 更换映射端口后显式重建 API 池，不声称旧池自动发现新地址。

最终 7 command / 7 message / 3 conversation / 14 untouched pending Outbox / 3 unchanged cursor / 0 quarantine；逐条原终态/规范消息、三个 ACK 值和 schema 指纹不变。业务直接调用 PersistCommand，MQ publisher capability 明确未使用，不报告完整发送到客户端恢复联调。运行和清理证据见[实施记录](message-reliability-mq-progress.md)。
