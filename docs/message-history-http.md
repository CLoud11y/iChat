# P6.2.3.2：历史查询 HTTP

现代 API 提供 `GET /auth/messages/history`，默认工厂从自有 SQL 池构造 `MessageRepository.HistoryMessages`。沿用 P2.7 的单页只读 REPEATABLE READ 快照、当前权限和规范 MessageDTO，与[向前补拉](message-sync-http.md)共同完成 P6.2.3 的两个 HTTP 入口。

## 请求与响应

请求必须提供 `Authorization: Bearer <JWT>`。UID 来自鉴权上下文，不接受 query/form token，不读取 GET body。鉴权先于业务参数解析；原始 query 最多 1024 字节，每个字段只允许一次：

| 参数 | 规则 |
| --- | --- |
| conversationId | 必填，规范 `p:<小UID>:<大UID>` 或 `g:<群ID>` |
| beforeSeq | 可省略，表示当前快照最新页；显式值为排他上界，规范十进制 uint64 字符串，允许 `0` 和最大值 |
| limit | 可省略（100）；显式值为规范十进制 1—500 |

显式空值、前导零、负数、加号、空白、小数、溢出、重复/未知字段、大小写别名和非法 URL 编码均为 400。`afterSeq`、`userId`、`deliveredSeq` 也属于未知字段。

示例：`GET /auth/messages/history?conversationId=p%3A1%3A2&beforeSeq=3&limit=100`，只返回 seq 小于 3 的消息。成功 HTTP 200、code=0、msg=`message history loaded`，data 为 HistoryPage；items 按 seq **升序**排列，nextBeforeSeq 为本页最老消息的 seq，下次原样作为 beforeSeq。ID、序号和水位使用 JSON 字符串，消息 DTO 与 SQL/通知统一。

已读到最老消息后，`beforeSeq=1` 的空页示例：

```json
{
  "code": 0,
  "msg": "message history loaded",
  "data": {
    "conversationId": "p:1:2",
    "beforeSeq": "1",
    "items": [],
    "nextBeforeSeq": "1",
    "highWatermark": "4",
    "hasMore": false
  }
}
```

省略 beforeSeq 时响应省略该字段；显式 `0` 保留 `"beforeSeq":"0"`，返回空 items，但仍返回已提交水位。空页保留请求游标，省略时 nextBeforeSeq 为 `"0"`。非空页的 hasMore 表示当前排他范围内还有更老消息。

| HTTP | data.reasonCode | 说明 |
| --- | --- | --- |
| 400 | INVALID_REQUEST | 参数格式错误 |
| 401 | UNAUTHENTICATED | 缺失、无效、过期或撤销的 token |
| 403 | FORBIDDEN | 纯私聊参与者/当前群权限拒绝 |
| 503 | DEPENDENCY_UNAVAILABLE | 鉴权/SQL/释放/取消/损坏/矛盾页/异常或停止准入 |

beforeSeq 超过水位是合法查询，从当前水位向前读取；不会映射为向前补拉的超水位 400。错误 code=-1，data 只有 reasonCode，不返回部分 items、业务 state 或底层诊断。所有响应为 JSON、`Cache-Control: no-store`。

## 连续范围与生命周期

每页在同一个 SQL 快照检查私聊参与者或当前群成员，再读水位和消息。当前群成员可读入群前消息，退群/删群后即使请求空页也拒绝；解除好友仍允许既有私聊参与者读取历史。分页不冻结跨请求水位，新提交不混入旧 beforeSeq 的范围，重新请求最新页可读新消息。

令 start 为水位；显式 beforeSeq 时 start 为 `min(水位, beforeSeq-1)`，`0` 单独取 start=0，避免下溢。仓储倒序读取至多 limit+1 行、校验包括额外行的连续 seq、正文 hash 和规范消息，再将本页翻转为升序。缺号或损坏导致整页失败。

`HistoryPage.ValidateFor` 出站前检查规范 query、会话和 beforeSeq 的值/有无、非 nil items、精确页长、hasMore、nextBeforeSeq、升序连续 seq、唯一消息身份和规范 DTO 的身份/路由/媒体/时间。HTTP 不再查询 SQL；正文 hash 由仓储在原快照核对。计算不需要 `水位+1`，省略游标可包含 MaxUint64，显式 MaxUint64 排除该序号。

`HistoryTimeout` 默认 3 秒，同一个父 deadline 覆盖实际 Redis 鉴权、等 SQL 连接和查询。API 写期限校验包含历史期限；没有新增 YAML/env 键、schema 或依赖。工厂仅借入自有 SQL 池，错误/nil/typed nil 进入原启动回滚。

纯权限拒绝映射 403；与 SQL 释放失败混合或已经取消的结果映射 503。handler 等实际回调返回才退出，迟到 nil 不能成功；关闭 observer 返回 pending 后仍保留 SQL/Redis，直到请求和探针真实退出。读取不按 MQ publisher 或缓存 readiness 门禁，不读 Redis 消息缓存、不读取或更新服务器 ACK。

## 验证与范围

```powershell
go test -mod=readonly -p=2 -race ./messaging ./service ./router . -run '^Test(HistoryPage|MessageHistory|APIHistory|APIStartup|APIValidation)' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionMessageHistoryIntegration$' -count=1 -v -timeout=7m
```

真实默认 API 验收覆盖三类媒体规范 DTO、省略/显式排他/零/超水位游标、分页期间新提交与重新读取最新页、群权限/私聊参与者、额外行损坏与缺号、最大 uint64 的省略/显式区别、真实 JWT 撤销、SQL 池耗尽期限、Redis 客户端故障、MySQL 离线启动/重启和资源关闭。

最大序号使用本轮独立 schema 中的临时元数据夹具，恢复后再核对原命令/消息及 ACK；不声称实际生成最大 uint64 条消息。业务由 PersistCommand 创建，MQ publisher capability 未使用；客户端模拟器、本地持久化恢复和完整发送管线仍待后续。运行、最终集合和清理证据见[实施记录](message-reliability-mq-progress.md)。
