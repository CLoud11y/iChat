# P6.2.1：会话发现仓储与分页协议

`MessageRepository.ListConversations(ctx, authenticatedReader, cursor, limit)` 只读列出用户当前可见的既有 conversation 行。此项提供 DTO 和仓储；`GET /auth/conversations` 的鉴权、参数/响应包装及生产 API 接线归 P6.2.2，sync/history HTTP 接线归后续小点。

## 字段与游标

默认 limit=100，允许 1—500；库入口的 limit=0 表示使用默认值。UID 必须非零，HTTP 层后续须从鉴权上下文取得，不能信任请求中的用户 ID。cursor 缺省/空字符串从头开始，否则必须是规范 `g:<群ID>` 或 `p:<小UID>:<大UID>`。

结果按 conversation.id 的 ASCII 二进制顺序排列，使用排他条件 `id > cursor`，例如 `g:10 < g:2 < p:1:10 < p:1:2`。cursor 只是扫描位置，不是授权凭据，不要求仍存在或仍可见；退群/删群后仍可用旧位置继续翻页。

```json
{
  "items": [
    {"conversationId":"g:10","lastSeq":"1","deliveredSeq":"0"},
    {"conversationId":"g:2","lastSeq":"2","deliveredSeq":"1"}
  ],
  "nextCursor":"g:2",
  "hasMore":true
}
```

这是 data DTO 示例，HTTP 包装尚未接线。ID/游标为字符串，lastSeq/deliveredSeq 使用现有 Sequence 的规范 uint64 JSON 字符串，支持零和最大 uint64。每页返回最多 limit 个 items，nextCursor 取最后返回的 ID；空页保持请求 cursor，items 为 `[]`。hasMore 根据同一快照内的额外一行决定，额外行也须通过身份/水位校验，但不返回、不推进 nextCursor。末页即使 hasMore=false，也保留最后返回 ID。

## 权限与快照

每页使用独立只读 REPEATABLE READ 事务：先确认当前用户存在且未删除，随后在同一个快照查询会话、当前成员权限及本用户 ACK 游标。读者缺失/被删除返回 ErrConversationForbidden，而有效但没有可见会话的用户得到成功空页。

- 私聊：只允许规范会话的两位参与者，不重新要求当前好友关系，保留既有历史权限。
- 群聊：群和当前成员关系均须未删除，relation.type 必须为 GroupRelation。群主身份、普通好友关系及旧订阅本身不授予访问权。
- 只枚举已提交的 conversation 行，不根据好友/群成员关系凭空生成会话。加入但尚无 conversation 的群不会出现在列表中。
- deliveredSeq 只连接当前用户的 `message_delivery_cursor`，缺失时为零；其他用户的较大 ACK 不得混入结果。

返回前校验规范 ID/type/私聊参与者或群身份、严格排序、不重复和 `deliveredSeq <= lastSeq`。SQL、scan/迭代/rows 关闭、snapshot 释放、取消或损坏错误均清空整个结果，不返回部分页/假空页。无需消息正文、Redis、MQ、写事务或 FOR UPDATE/SKIP LOCKED；未提交的会话/成员/游标更新既不授权新数据，也不导致跳过旧的已提交会话。

权限以本页数据库快照建立时为准；稍后撤权不会撤回已经开始的页，下一页/下一轮重新判定。列表是发现元数据，不审计全部历史消息或证明每个 seq 对应的内容完整；实际补拉仍使用 P2.7 的连续范围校验。

每页最多读取 limit+1 个匹配结果，SQL 实际扫描/过滤工作量可能大于返回行数，本项未增加索引、schema 或容量承诺。读操作遵守调用者 context，没有分离查询 goroutine；HTTP 请求期限和 owner 装配在 P6.2.2 落实。

## 周期扫描与本地恢复

跨页不维持一个长期快照。新增会话可能排在当前 cursor 之前，已扫描会话也可能又提交消息，因此每个周期都必须从空 cursor 完成一轮全量可见会话扫描。nextCursor、updatedAt 或最大 messageId 都不能作为永久增量水位。

deliveredSeq 只是服务器的持久 ACK，不证明当前浏览器保留了本地数据；本地清空后仍从自己的实际连续游标（必要时零）补拉，不能跳到服务器 ACK。列表也不表示已读。

## 验证

```powershell
go test -mod=readonly -p=2 -race ./messaging ./database -run '^TestConversation' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration ./database -run '^TestConversationsIntegration$' -count=1 -v -timeout=7m
```

真实八组验证权限/ASCII 排序/本用户 ACK、排他及失效游标、同快照内并发消息提交/ACK/撤权、跨页插入后下一轮重扫、退群/删群/错误关系类型、未提交更新不阻塞旧视图、真实 uint64 元数据边界，以及损坏游标/等待连接取消/关闭 pool 的失败清空。快照测试显式将连接默认隔离设为 READ COMMITTED，证明方法自己建立 RR；写入经另一实际 SQL 连接完成。

最终 11 command / 11 message / 7 conversation / 22 untouched pending Outbox / 3 cursor / 0 quarantine。最大序号测试是临时元数据边界夹具，随后恢复，不声称实际生成了最大 uint64 条消息。逐条比较生产命令的原终态和规范消息，schema 指纹不变。未启用 HTTP 路由、MQ/Outbox 管线或实际客户端恢复；运行及清理记录见[实施进度](message-reliability-mq-progress.md)。下一项 P6.2.2 会话列表 HTTP 契约与 API 装配。
