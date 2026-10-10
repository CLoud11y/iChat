# 消息协议 v1：DTO、规范化与身份

本规则由 `messaging` 包实现，属于 P2.1。当前未接入旧 HTTP/Redis 入口，未创建新 schema，也未发布 RabbitMQ 消息。后续 API、consumer、仓储与客户端适配必须复用这里的规则，不能分别生成不同的摘要。

## 类型与调用边界

| 类型/函数 | 职责 |
| --- | --- |
| `SendRequest` / `DecodeRequest` | 解码客户端请求，仅包含 clientMessageId、targetType、targetId、media、content、可选 clientSentAt。解码检查 JSON 结构和字节限制；业务字段的必填/取值由 Normalize 检查。 |
| `Normalize(authenticatedSenderID, request)` | 使用已鉴权 uint64 用户 ID，校验并返回规范 `Command`，生成 commandId、payloadHash、conversationId。无 DB/Redis 调用，不提前查询权限或终态。 |
| `ValidateCommand` | 对可变的导出 Command 字段重新计算并逐项检查，拒绝非规范身份、摘要和会话。仓储不能因收到该 Go 类型就假定它已校验。 |
| `NewEnvelope(command, receivedAt)` | 校验命令，使用调用方提供的服务端时间，生成本次物理发布的随机 v4 UUID。每次物理发布重新调用。 |
| `DecodeEnvelope` / `ValidateEnvelope` | 严格解码后校验版本、所有字段、规范写法和重算的身份/hash；DecodeEnvelope 包含 ValidateEnvelope。无效时返回零值和错误，调用方仍须保留原 body 供后续隔离。 |
| `Sequence` / `NewMessageDTO` | 序号在 Go 内为 uint64、JSON 中为十进制字符串；DTO 由已校验命令、已提交 seq 和服务端创建时间构造，messageId=commandId。不分配 seq、不写数据库、不生成 accepted 响应。 |

未来入口调用顺序：先限制 HTTP 读取字节数，再 `DecodeRequest(body)`，然后 `Normalize(authenticatedUID, request)`，最后为每次发布调用 `NewEnvelope`。不能直接将请求 body 作为信封，也不能通过普通 `json.Unmarshal` 绕过严格解码器后声称原始内容已经校验。`MaxBodyBytes` 只约束传入的字节切片，读取前的 HTTP 内存限制须由 P3 适配层落实。

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

## 验证与后续

`go test ./messaging` 覆盖固定向量、无歧义编码、字段变更、重试元数据、严格 JSON/Unicode、内容/字节边界、时间格式、超大 ID/序号、信封篡改和 DTO。`FuzzRequestNormalization` 检查解码/规范化不崩溃，成功数据经 JSON 往返后业务命令不变。

P2.1 不创建数据库表/约束，不实现命令终态、会话 seq 分配、权限事务、Outbox 或可靠 publisher；这些能力分别按 P2.2 及后续小点实施。旧发送与历史接口保持现有行为。
