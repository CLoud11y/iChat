# 消息协议 v1：DTO、规范化与身份

本规则由 `messaging` 包实现，始于 P2.1，后续小点在此补充持久化和查询契约。P2.2—P2.7 已实现独立 schema、幂等/权限/持久化事务和状态/同步仓储；P3.6.1 已提供独立[确认式 HTTP 发送适配](command-submission.md)，复用这里的 DTO/解码/规范化，默认旧入口尚未切换。API、consumer、仓储与客户端适配必须复用这里的规则，不能分别生成不同的摘要。

## 类型与调用边界

| 类型/函数 | 职责 |
| --- | --- |
| `SendRequest` / `DecodeRequest` | 解码客户端请求，仅包含 clientMessageId、targetType、targetId、media、content、可选 clientSentAt。解码检查 JSON 结构和字节限制；业务字段的必填/取值由 Normalize 检查。 |
| `Normalize(authenticatedSenderID, request)` | 使用已鉴权 uint64 用户 ID，校验并返回规范 `Command`，生成 commandId、payloadHash、conversationId。无 DB/Redis 调用，不提前查询权限或终态。 |
| `ValidateCommand` | 对可变的导出 Command 字段重新计算并逐项检查，拒绝非规范身份、摘要和会话。仓储不能因收到该 Go 类型就假定它已校验。 |
| `NewEnvelope(command, receivedAt)` | 校验命令，使用调用方提供的服务端时间，生成本次物理发布的随机 v4 UUID。每次物理发布重新调用。 |
| `DecodeEnvelope` / `ValidateEnvelope` | 严格解码后校验版本、所有字段、规范写法和重算的身份/hash；DecodeEnvelope 包含 ValidateEnvelope。无效时返回零值和错误，调用方仍须保留原 body 供后续隔离。 |
| `Sequence` / `NewMessageDTO` | 序号在 Go 内为 uint64、JSON 中为十进制字符串；DTO 由已校验命令、已提交 seq 和服务端创建时间构造，messageId=commandId。不分配 seq、不写数据库、不生成 accepted 响应。 |
| `NormalizeStatusQuery` / `CommandStatus` | 用已鉴权发送者、clientMessageId 和该次 payloadHash 构造状态查询，返回该次内容的状态；不接受客户端自报 senderId，不暴露冲突原消息或隔离诊断。 |

P3.6.1 独立 HTTP 适配已按此顺序实现：先限制 HTTP 读取字节数，再 `DecodeRequest(body)`，然后 `Normalize(authenticatedUID, request)`，最后由 publisher 为每次发布调用 `NewEnvelope`。不能直接将请求 body 作为信封，也不能通过普通 `json.Unmarshal` 绕过严格解码器后声称原始内容已经校验。`MaxBodyBytes` 只约束传入的字节切片；适配层在读取前使用 MaxBytesReader，未知长度流同样受限，完整 HTTP ingress 超时由下一小点的 API owner 提供。

共享摘要不是认证签名：consumer 的 senderId 来自服务端信封，broker 写权限必须只授予可信生产者；能够任意写入 broker 的人也能重算 SHA-256。本阶段没有新增认证或 broker ACL。

## 固定输入规则

| 字段 | v1 规则 |
| --- | --- |
| schemaVersion | 仅支持整数 1；未知版本返回 `ErrUnsupportedVersion`，不按旧版尝试处理。 |
| clientMessageId | 36 字符 `8-4-4-4-12` 十六进制加连字符形式；输入允许大小写并规范为小写；拒绝 nil UUID、无连字符、花括号、URN、空白及非 hex 字符。不强制客户端 UUID 的版本/variant 位；服务端 publishAttemptId 使用随机 v4/RFC variant。 |
| senderId | API 只使用鉴权上下文的正 uint64；客户端请求没有此字段。信封中使用无前导零的十进制字符串。 |
| targetId | 请求中必须为字符串，含 1–20 个 ASCII 十进制数字，范围 1–18446744073709551615；前导零规范为无前导零形式。拒绝数值 JSON、正负号、空白、小数、指数和溢出。 |
| targetType / media | 严格使用 `private` / `group` 与 `text` / `picture` / `audio`，不忽略大小写或 trim；旧 iota 数值不是新协议枚举。 |
| content | 1–1024 个 Unicode code point（Go rune）；必须为合法 UTF-8。原样保留空格、CRLF、NUL、组合字符与 U+FFFD；不 trim、不做 NFC/NFD 转换。仅空白内容合法，空字符串不合法。图片/音频内容为引用字符串，本阶段不验证资源存在性。 |
| JSON body | 请求和信封均至多 16384 字节，必须恰为一个对象；字段名大小写精确，拒绝未知字段、重复字段（包括转义后同名）、null、类型错误及尾随内容。必填字段缺失由后续规范化/信封校验拒绝。 |
| UTF-16 转义 | 合法代理对解码为对应 UTF-8 字符；孤立或错误配对代理项拒绝，避免 encoding/json 静默替换为 U+FFFD。合法的字面 U+FFFD 与 `\ufffd` 保留。 |
| clientSentAt | 可缺省或为空字符串；非空使用严格 RFC3339，秒的小数点只能为点，最多 9 位小数。规范为 UTC RFC3339Nano，保留纳秒。客户端时间不决定顺序，也不参与身份/hash。JSON null 不作为缺省。 |
| receivedAt / serverCreatedAt | 服务端传入的非零时间，UTC 年份在 1–9999，输出 UTC RFC3339Nano。该协议精度不等于未来 MySQL 存储精度；P2.2 需要明确存储方案。 |
| seq | JSON 必须为规范十进制字符串：`"0"` 或无前导零的正整数，最大 uint64。0 可作游标；消息 DTO 构造器拒绝 0。拒绝 JSON number，避免 JavaScript 2^53 以上精度丢失。 |

API 接受部分可规范化写法；consumer 只接受已经规范的信封。例如请求 targetId=`"00034"` 可以规范为 `"34"`，但信封中的 `"00034"` 即使附带正确规范摘要也会被拒绝。时间的时区偏移和多余小数尾零在 API 规范化，信封须使用 UTC 最简写法。

私聊 conversationId 按 **uint64 数值** 排序生成为 `p:<小ID>:<大ID>`，群聊为 `g:<groupID>`。本阶段不检查好友/成员关系，不因目标暂不存在而拒绝语法合法命令；私聊同 ID 的业务处理也留给权限事务。`ValidateCommand` 重算会话并核对，不能信任客户端传入的会话。

## 身份与 hash 字节编码

使用 SHA-256，输出 64 字符小写 hex。不是对原始 JSON 文本或 Go map 序列化结果求摘要。

设 `LP(s) = uint32_big_endian(len(UTF8(s))) || UTF8(s)`，`||` 表示字节拼接。长度是 UTF-8 **字节数**，包括任何 NUL；正文字符数限制另行计算。

```text
commandId preimage = UTF8("ichat:command:v1") || 0x00
                    || LP(senderId) || LP(clientMessageId)

payloadHash preimage = UTF8("ichat:payload:v1") || 0x00
                      || LP("1") || LP(senderId) || LP(targetType)
                      || LP(targetId) || LP(media) || LP(content)
```

所有字段先按上述规则规范化。字段顺序、domain、长度编码与 schemaVersion 都是协议的一部分；修改时必须显式引入新版本并定义重放兼容策略。

- 同一 sender/clientMessageId 对应同一 commandId，内容变化只改变 payloadHash，供 P2.3 判断 `ID_CONFLICT`。
- 不同 clientMessageId、相同业务内容：commandId 不同，payloadHash 相同，属于不同逻辑发送。
- 更换 sender：commandId 和 payloadHash 都改变。
- publishAttemptId、receivedAt、clientSentAt 不进入任何业务摘要；traceId 和重试计数尚未加入 DTO，也不应进入摘要。
- clientSentAt 随重试变化不会改变业务身份/hash；后续幂等仓储必须返回首次已提交的规范结果，不能因此覆盖消息元数据。

固定测试向量（由独立 Python hashlib/struct 计算，与 Go 实现交叉核对）：

```text
senderId = 12
clientMessageId = 8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1
targetType = private
targetId = 34
media = text
content = hello

commandId = 55faac68e13e0a6d5b9562536d3b0c7cf90552416747c9986500b2202fe96fa2
payloadHash = 55405570ca8bd409cbc5ed2f2b5914c09b4b5141fb4b377ef6d46045550ad591

content 改为：空格、CR、LF、中、😀、NUL（按此顺序）
payloadHash = db3a3b345db4485e071be38f136425b0d3489769106319e83dd662242e88f273
```

错误可通过 `errors.Is` 区分 `ErrInvalidInput`、`ErrUnsupportedVersion`、`ErrIdentityMismatch`。错误不回传原始正文或底层解析内容；HTTP 状态和 MQ 隔离分类在对应适配阶段映射。

## P2.5：持久化时间与内部 Outbox 事件

生产事务入口 `MessageRepository.PersistCommand` 在命令幂等及授权通过后，锁定 conversation 分配连续 seq，将 message、last_seq、命令终态和固定身份 Outbox 一起提交。成功生成 message.persisted 与 command.result；业务拒绝只生成 command.result，不生成 message/seq。原键同 hash 重试保留原消息、序号、时间和事件；异 hash 不改变原记录。序号耗尽是显式仓储错误，不分配 0，也不提交业务拒绝。

clientSentAt 在数据库中缺省为 NULL，非空先转 UTC 并**截取**到微秒，再绑定时钟文本写入 DATETIME(6)。例如 `.999999999` 保存为 `.999999`，不会舍入到下一秒。该元数据仍不参与身份/hash；改变重试的 clientSentAt 不修改首次提交值。serverCreatedAt 来自数据库 UTC 微秒时钟，成功结果事件中的 message DTO 使用相同值。固定 MySQL 8.0.44 的集成测试包含 0001 和 9999 年边界；MySQL 官方只保证 1000–9999 年的 DATETIME 范围，因此升级/替换数据库版本时须验证更早年份的兼容性。[MySQL DATETIME](https://dev.mysql.com/doc/refman/8.0/en/datetime.html)。

两类内部事件的 eventId 均由规范 commandId 和事件类型派生：

```text
eventId preimage = UTF8("ichat:outbox:v1") || 0x00
                  || LP(commandId) || LP(eventType)
```

eventType 只允许 message.persisted / command.result，eventId 为 SHA-256 小写 hex。独立 Python hashlib/struct 固定向量，以前文的 commandId 为输入：

```text
message.persisted eventId = de4b88c7c0deffb887f8c4d22bd07c788aee7de11c45b130bc63141fbac6de2d
command.result eventId    = 56f143e224be2614fc13bc2fb656d1ab3e1cb46564871fbbcb85ca509c27ba30
```

| 内部事件 | 存储 payload |
| --- | --- |
| message.persisted | schemaVersion、eventId、commandId、conversationId、messageId、字符串 seq、senderId、targetId、targetType；不包含消息正文 |
| command.result | schemaVersion、eventId、commandId、senderId、clientMessageId、payloadHash、state；成功包含完整 message DTO，拒绝包含 reasonCode 且无 message |

schemaVersion 为 1，事件身份不含通知 attempt/lease。Outbox 中的身份和完整 DTO 不授予接收权限；P5 worker/网关仍需检查当前授权并适配实际通知/WS 帧，Outbox 发布已由 P5.1 完成，P5.2 提供严格提示、SQL 权限核验及帧适配，见[通知帧](notification-frames.md)。命令处理 SQL 错误回滚；COMMIT 报错保持 `ErrCommandCommitUnknown`，不能据此 ACK 或换键重发。

## P2.6：发送状态查询

`MessageRepository.GetCommandStatus(ctx, authenticatedSender, clientMessageId, payloadHash)` 只查询当前发送者自己的逻辑命令。clientMessageId 沿用 UUID 规范化规则；payloadHash 必须为 64 字符小写 hex，不接受缺省、空白或大写。HTTP 适配层必须从鉴权上下文取 UID，不能信任查询参数中的发送者。

所有读取使用同一个显式只读 REPEATABLE READ 快照，不加行锁、不写记录、不扫描 MQ/Redis。先查规范命令；同键异 hash 返回 ID_CONFLICT，同 hash 返回原 persisted/rejected。没有规范命令时，才查同 sender/clientMessageId/payloadHash 的未解决隔离记录（open 或 replay_pending，且 resolved_at 为 NULL）；隔离记录可缺少 commandId。两类记录都不存在时返回 pending_or_unknown，不能据此推断命令未入队。

| state | 附加字段 | 后续 HTTP 映射 |
| --- | --- | --- |
| persisted | 完整规范 `message` DTO，无轮询字段 | 200 |
| rejected | 稳定 `reasonCode`，无 message/轮询字段 | 200 |
| ID_CONFLICT | 只回显本次请求的 key/hash；不附原消息、原 hash 或原拒绝原因 | 409 |
| quarantined | `pollAfterMs: 2000`；不附隔离原文、headers、transportId 或诊断错误 | 200 |
| pending_or_unknown | `pollAfterMs: 2000`；无 message/reasonCode | 200 |

例如，尚无终态的 data DTO 为：

```json
{
  "clientMessageId": "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1",
  "payloadHash": "55405570ca8bd409cbc5ed2f2b5914c09b4b5141fb4b377ef6d46045550ad591",
  "state": "pending_or_unknown",
  "pollAfterMs": 2000
}
```

persisted DTO 从同一快照内的 message 重建并核对命令身份、hash、会话及已提交 seq，时间按 UTC 微秒读取。好友解除、离群或删群不会重新判定自己既有发送终态；会话历史/同步的当前读取权限由 P2.7 单独处理。查询期间新终态提交时，本轮可以返回快照中的旧隔离状态，下轮返回终态，避免混合两个时刻误报 unknown。

数据库、快照释放或数据不变量故障返回错误和空结果，不伪装为 pending_or_unknown，也不把底层错误放入状态 DTO。HTTP 路由、code/msg/data 包装和依赖故障映射仍归 P6.1；当前只完成仓储契约。隔离生产/审计/重放仍归 P4。

## P2.7：会话同步与历史仓储

`MessageRepository.SyncMessages(ctx, authenticatedReader, conversationId, afterSeq, limit)` 和 `HistoryMessages(ctx, authenticatedReader, conversationId, beforeSeq, limit)` 只读取 MySQL。鉴权 UID 由适配层提供；不读取 Redis、MQ 或服务器 deliveredSeq，客户端本地清空后仍可从 0 补齐。

conversationId 只接受规范形式：私聊 `p:<小ID>:<大ID>` 的两个正 uint64 ID 必须严格递增，群聊 `g:<正ID>`；拒绝前导零、倒序、相同参与者及未知前缀。游标为规范十进制 uint64 字符串，允许 0；空字符串代表省略。limit=0 表示仓储默认 100，其他值必须为 1–500；HTTP 适配层仍需校验显式参数，不能把解析失败转换为默认值。

每页显式开启只读 REPEATABLE READ 事务，使用普通 SELECT，在同一视图中完成授权、会话身份核验、highWatermark 和消息读取。私聊只允许会话两位参与者，解除好友后仍可读取已有历史；群聊要求该快照中群未软删除且当前用户有未软删除的成员关系，群主不能绕过成员条件。拒绝返回 `ErrConversationForbidden`，不附带水位或消息。权限按本页快照判定，之后的离群/删群不撤回已经开始的读取，下一页重新授权。

| 查询 | 范围与返回 |
| --- | --- |
| sync | `afterSeq < seq <= highWatermark`，按 seq 升序；省略 afterSeq 等于 0。nextAfterSeq 为最后返回序号，无消息时保持输入游标。afterSeq 超过当前水位返回输入错误，不能静默回退或改写本地游标。 |
| history | beforeSeq 是排他的上界，省略时从当前快照最新消息开始；显式 0 返回空页，高于水位的上界按当前已有范围读取。SQL 降序取最近一页，items 逆序为升序展示，nextBeforeSeq 为最旧返回序号；空页保持输入游标，省略输入时为空页的 0。 |

两类响应都含 conversationId、items（空结果为 `[]`）、highWatermark 和 hasMore。sync 另含 afterSeq/nextAfterSeq；history 另含 nextBeforeSeq 和可省略的 beforeSeq。所有序号字段仍是 JSON 字符串，最新历史不会计算 `highWatermark+1`，因此最大 uint64 不回绕。授权通过但会话尚无提交记录时返回水位 0 和空页；不存在的群或非成员仍拒绝读取。

每次 SQL 最多读取 `limit+1` 行，额外一行仅用于确认后续范围完整，不作为返回 item。对本次范围（含额外行）逐一检查预期连续 seq，并核验规范消息身份、会话、正文 hash、媒体和 UTC 微秒时间；消息对应的规范命令必须为 persisted。缺少预期消息、缺少命令、内容损坏或 SQL/迭代/快照释放错误返回空结果和错误，不返回部分页、不跳过缺号、不伪造 hasMore=false。检查是有界范围检查，不宣称每页审计全部历史。

每页建立独立快照，不维持跨页数据库事务。分页期间的新提交可被下一页或后续周期扫描发现；客户端仍应持久化连续本地游标、去重并定期核对。HTTP 路由、会话发现及周期同步归 P6，通知和 ACK 归 P5；本阶段没有改动旧历史入口。

## 验证与后续

`go test ./messaging` 覆盖固定向量、无歧义编码、字段变更、重试元数据、严格 JSON/Unicode、内容/字节边界、时间格式、超大 ID/序号、信封篡改和 DTO。`FuzzRequestNormalization` 检查解码/规范化不崩溃，成功数据经 JSON 往返后业务命令不变。

P2.1 的原始范围只包括协议；数据库表/约束、命令终态、权限事务、会话 seq、Outbox、状态及同步/历史仓储已按 P2.2—P2.7 分项实施。可靠 publisher 和独立确认式 HTTP 适配已完成对应库/隔离验证；SubmissionResponse/Submission 定义 code/msg/data、逻辑身份和本次物理 attempt，错误 reasonCode 位于 data，响应不包含 seq。完整应用入口与通知仍待后续阶段；旧发送与历史接口保持现有行为。


## P5.2：通知数据帧与内容权限

统一包络为 `{type,eventId,data}`，eventId 使用前述固定派生。message 的 data 与同步 MessageDTO 完全一致。command.result 的 data 包含 clientMessageId/payloadHash/state，拒绝仅增加 reasonCode；成功始终增加 messageId/conversationId/字符串 seq，有当前内容读取权限时再包含完整 message。

发送者退群/群删除后，自己的成功终态仍为 persisted，保留消息引用并省略 message；旧群订阅不允许继续取得正文。私聊历史权限仍按参与者身份，解除好友不撤销已提交历史。网关按当次 SQL 权限锁读决定，不将 Pub/Sub 或旧结果 payload 当成授权依据。

分发器在 SQL 连接/锁释放后才调用出站回调，网络与撤权不是原子操作；P5.3 已在单 writer 临近发送时调用，并完成真实 v1 订阅/会话接线。当前 P5.2 传输验收使用明确的测试 WebSocket endpoint，未改变旧 Chat 路由。完整字段、限制和验收见[通知帧与权限](notification-frames.md)。


## P5.3：生产 v1 WebSocket 会话

现代 API 使用 `GET /auth/chat`，只接受 Authorization Bearer，保留同源 Origin 检查，可协商 `ichat.v1`。全部初始频道确认后第一帧为 ready，data 含 protocolVersion=1、随机 32 位小写 hex sessionId 和 syncIntervalSeconds=10。旧 legacy Chat 保持原协议。

每实例每用户一个 current 会话；替换与实际 socket 写入串行，旧回调/清理不能写入或移除新会话。队列仅存提示，writer 在写入前通过 P5.2 校验 SQL 内容权限；单 writer 也承担所有数据/控制心跳。刷新成员列表时确认新流后再关闭旧流，发送 sync.required/all，并周期复核原 JWT。重复提示、刷新/重连窗口的丢失仍靠客户端去重与周期补拉收敛。

当前接受严格 JSON ping/pong（data 为空对象）、业务 ack 及 WebSocket 心跳控制帧。队列满/写超时/依赖失败结束会话，API pending owner 等所有会话与 SQL/订阅回调实际退出后才释放客户端。字段、默认限制、单实例范围与验收见[可靠 WebSocket 生命周期](reliable-websocket.md)。

## P5.4：业务 ACK 与已提交游标

客户端帧为 `{"type":"ack","data":{"conversationId":"p:1:2","deliveredSeq":"12"}}`，成功提交后返回 `{"type":"ack.confirmed","data":{"conversationId":"p:1:2","deliveredSeq":"12"}}`，无 eventId。序号是必填的规范 uint64 字符串，允许零；不接受客户端 userId/sessionId、未知字段、别名、重复/null/缺失字段。用户及 sessionId 从当前物理连接内部绑定。

单实例每用户 gate 串行完整 ACK SQL/COMMIT 与会话激活；新会话 ready 后旧 session 不得再次调用仓储或写确认。READ COMMITTED 事务按当前有效用户/群/成员锁读授权，再锁读规范 conversation/已提交 last_seq；不能越权或超过水位。游标按用户/会话唯一键单调推进，较小/重复 ACK 返回原进度且不改时间。不存在会话（含 ACK 0）拒绝，不建空会话。

只有 SQL 成功提交才排队确认。权限/依赖/提交未知/取消错误关闭会话且不确认；提交后确认丢失可在重连后重复 ACK。客户端自行证明本地连续保存，服务器游标不表示已读，也不能代替本地清空后的补拉起点。期限、资源责任、跨实例限制及故障验收见[业务 ACK](delivery-ack.md)。
