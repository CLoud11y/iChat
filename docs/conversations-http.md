# P6.2.2：会话列表 HTTP 与 API 装配

现代 API 注册 `GET /auth/conversations`，默认工厂从自有 SQL 池构造真实 `MessageRepository.ListConversations`。数据与分页沿用 [P6.2.1 会话发现仓储](conversation-discovery.md)；向前补拉 HTTP 已由 [P6.2.3.1](message-sync-http.md) 完成，[历史 HTTP](message-history-http.md)已由 P6.2.3.2 完成，恢复客户端仍待后续，默认运行模式仍为 legacy。

## 请求与响应

请求使用 `Authorization: Bearer <JWT>`，不接受 query/form token，不读取 GET body。UID 只来自鉴权上下文，客户端不能选择其他用户。

| 参数 | 规则 |
| --- | --- |
| cursor | 可选，空字符串从头开始；否则为规范 conversationId 的排他位置，允许已经不可见/不存在的会话 ID |
| limit | 可选，缺省 100；显式值须为规范十进制 1—500，拒绝空、0、负数、前导零、加号、空白、小数、溢出 |

原始 query 最多 1024 字节。重复参数（即使值相同）、未知参数、大小写别名和非法 URL 编码为 400；合法 URL 转义如 `cursor=g%3A2` 可用。鉴权先于业务参数解析，缺失/错误/撤销 token 为 401，Redis 鉴权依赖故障为 503。

成功响应 HTTP 200：

```json
{
  "code": 0,
  "msg": "conversations",
  "data": {
    "items": [
      {"conversationId":"g:10","lastSeq":"1","deliveredSeq":"0"},
      {"conversationId":"g:2","lastSeq":"2","deliveredSeq":"1"}
    ],
    "nextCursor":"g:2",
    "hasMore":true
  }
}
```

序号保留 uint64 JSON 字符串，包括最大值。有效读者没有匹配会话时，items 为 `[]`，nextCursor 保留请求值，hasMore=false。非空末页仍保留最后返回的会话 ID。所有响应为 JSON，带 `Cache-Control: no-store`。

| HTTP | data.reasonCode | 行为 |
| --- | --- | --- |
| 400 | INVALID_REQUEST | 参数格式/字段错误，未调用仓储 |
| 401 | UNAUTHENTICATED | 缺失、无效、过期或撤销的 JWT |
| 403 | FORBIDDEN | 仓储确认读者不存在/已删除的纯权限拒绝 |
| 503 | DEPENDENCY_UNAVAILABLE | 鉴权/SQL/取消/释放故障、缺失 reader、损坏/矛盾返回、Panic 或停止准入 |

错误体为 `{"code":-1,"msg":"...","data":{"reasonCode":"..."}}`，不附 items、业务 state、原始 SQL/Redis 诊断或部分元数据。非群成员的会话被过滤，并非单独的 403；旧 cursor 也不授予读取权限。只有仓储的纯 ErrConversationForbidden 且请求尚未取消时才映射 403；权限与 SQL/释放故障的 joined error 返回 503。

## 权限与生命周期

真实仓储在同一个只读 RR 快照检查当前有效用户、私聊参与者或当前群成员，并读取 lastSeq 和本用户 ACK。好友解除不撤销私聊既有历史，退群/删群后的下一页重新过滤；群主身份、普通好友关系或旧订阅不授予群内容权限。

HTTP 出站前用 `ConversationPage.ValidateFor` 再检查规范 query、非 nil items、页长/hasMore、排他顺序/不重复、私聊参与者、deliveredSeq<=lastSeq 和 nextCursor 对应最后返回行。群权限仍由仓储快照证明，HTTP 不另开事务混合两个时刻的数据。

内部 `ConversationsTimeout` 默认 3 秒，父 deadline 同时覆盖实际 Redis 鉴权、等待 SQL 池及查询；独立于发送/状态请求期限。写期限须覆盖 ReadTimeout 加 SubmissionTimeout/StatusTimeout/ConversationsTimeout 的最大值；没有增加 YAML/env 配置或 schema。

reader 工厂闲置构造、显式借入 SQL 池，失败/nil/typed nil 进入原启动回滚；查询不按 publisher 或缓存探针门禁。handler 等实际回调退出，超时后 nil 结果不能变成成功页；observer pending 不提前释放 SQL/Redis，停止准入/Panic 沿用 API 的依赖失败响应。

每个周期应从空 cursor 重新扫描，避免新增会话落在上轮位置之前。deliveredSeq 不证明当前客户端保留本地消息，不得用于跳过本地清空后的补拉，也不表示已读。

## 验证与边界

```powershell
go test -mod=readonly -p=2 -race ./messaging ./service ./router . -run '^Test(Conversation|APIConversations|APIStartup|APIValidation)' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionConversationsIntegration$' -count=1 -v -timeout=7m
```

真实默认工厂 suite 九组覆盖规范 DTO/当前用户 ACK/空数组、严格 query/JWT、排他分页和新增 ID 后重扫、退群/删群/失效读者 403、损坏游标 503、占满实际四连接池后的默认三秒期限/恢复、实际 Redis 客户端故障、MySQL 停机/离线启动/重启，以及关闭后真实客户端释放。重启若更换 Docker 映射端口则显式重建 API，本轮实际发生了端口更换，不声称原池自动发现新地址。

业务由生产 PersistCommand 创建，MQ capability 明确未使用。最终 9 command / 9 message / 7 conversation / 18 untouched pending Outbox / 3 cursor / 0 quarantine；逐条原终态/规范消息和 schema 指纹保持。没有执行完整 HTTP 202→MQ→客户端恢复管线或性能压测。运行、默认测试和清理证据见[实施记录](message-reliability-mq-progress.md)。P6.2.3.1 [补拉 HTTP](message-sync-http.md)已完成，P6.2.3.2 [历史 HTTP](message-history-http.md)已完成，P6.3.1 [发送状态核心](client-sender-simulator.md)已完成，P6.3.2 [磁盘存储](client-sender-file-store.md)已完成，P6.3.3 [HTTP 恢复](client-sender-http.md)已完成，下一项 P6.3.4 通知与生产 API 交叉恢复验收。
