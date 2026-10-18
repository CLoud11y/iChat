# 确认式 HTTP 发送适配（P3.6.1）

本小点提供独立的 `service.SubmitCommand`、`router.RegisterSubmission` 和新协议客户端探测；默认主程序仍是 legacy，尚未注册生产 API 工厂。不要把这个路由追加到旧 Router 的同名路径，也不能将旧发送成功解释为 MQ accepted。

现代路由显式调用 `router.RegisterSubmission(engine, tokens, publisher, timeout)`，注册 `POST /auth/sendMessage`。它不创建 listener、存储、publisher 或 worker，不修改全局，不设置健康 ready；API 实例的构造/信号/准入/探测与关闭在下一小点接线。

## 请求与鉴权

仅接受 Authorization: Bearer 头，复用真实 TokenService 的签名、过期和 Redis 黑名单校验；不接受 query/form/raw header token。缺少或非法头不会通过 FormValue 提前读取请求体。鉴权失败为 401/UNAUTHENTICATED，Redis/鉴权依赖失败为 503/DEPENDENCY_UNAVAILABLE，均不调用 publisher。

handler 仅信任中间件设置的正 `uint` uid，转换为协议正 uint64；字符串、uint64 等其他 context 值不作为已鉴权身份。客户端不能提供 senderId/fromUser。仅接受 application/json（可带 charset），不接受压缩正文；错误媒体类型沿用 400/INVALID_REQUEST。

读取前使用 MaxBytesReader，将正文限制到 16384 字节，未知长度流最多读取上限加一个判定字节。恰好上限且 JSON 合法可接受。随后严格调用 DecodeRequest → Normalize(uid, request)，共享未知/重复字段、UTF-8/代理项、字段类型、UUID、ID/枚举、内容字符数和时间规则。正文/NUL/换行原样保留，targetId/UUID/时间使用协议规范写法。

构造必须提供正值且不超过 60s 的 timeout。路由 context 覆盖鉴权及提交；handler 自身也限制 publisher 调用，不延长更短的父 deadline。超时/取消发生在发布调用前为 not_published；发生在调用后则遵循 publisher 的实际结果，不能笼统判断为“没有发出”。取消导致的正文读取失败为依赖失败，不错误要求客户端修改业务内容。

此处限制正文内存和协作式调用，尚不提供完整 HTTP 慢头/慢正文硬期限。API owner 需在 P3.6.2 配置 ReadHeaderTimeout/ReadTimeout/WriteTimeout 并管理活动请求；没有通过丢弃 goroutine 来伪造 publisher/reader 已结束。

## 发布与响应

仅调用一次 publisher，不做数据库预查询、业务权限判定、缓存/通知或 Redis 入队，也不自动重试。服务端时间交给 publisher 生成每次物理尝试的信封；原 clientMessageId/commandId/payloadHash 在显式重试中保持，publishAttemptId 更新。

响应复用共享 SubmissionResponse/Submission DTO，保留 code/msg/data；错误 reasonCode 位于 data。可规范化的请求保留其三项逻辑身份，原始 publisher 错误、URL、非法结果字段不回传。所有响应 no-store。

| publisher 证据 | HTTP / reasonCode | data.state |
| --- | --- | --- |
| accepted、无错误、匹配 commandId、规范非零 attempt UUID、非零 connection/channel epoch/tag | 202，code=0/msg=accepted | accepted |
| not_published + busy、未取得发布 tag | 429/OVERLOADED，Retry-After: 1 | not_published |
| 缺失 publisher、预取消，或有错误的合法 not_published 结果 | 503/DEPENDENCY_UNAVAILABLE | not_published |
| 完整关联的 unroutable + 错误 | 503/PUBLISH_UNROUTABLE | unroutable |
| 完整关联的 nacked + 错误 | 503/DEPENDENCY_UNAVAILABLE | nacked |
| unknown，或矛盾/不完整/未知的调用后结果 | 503/PUBLISH_UNKNOWN | publish_unknown |

accepted DTO 不包含 seq、persisted 或 delivered。已得到完整确认后，仅因 context 在写响应前取消，不撤销该已知结果；客户端可能收不到响应，仍需保留原请求恢复。unknown+busy 不能被映射为确定未发布的 429；伪造 accepted+error、错 identity/缺 metadata 也不能返回 202。

同键异内容只改变 payloadHash，本入口仍可以接受，由消费者的权威事务决定 ID_CONFLICT。发送的好友/群权限不在 HTTP 中先查询；读/状态/ACK 的 403/409 契约仍在后续阶段。

## 客户端与测试

`cmd/loadtest -mode acceptance` 是新协议探测，使用用户预先持久化的单个请求文件、显式 sender-id 和环境变量 ICHAT_LOADTEST_TOKEN。默认一次请求；-attempts=1–100 表示用户明确要求对同一内容/原键顺序重放，不是自动恢复器或吞吐压测。它不创建账号、登录或打开 WebSocket，始终保留请求文件；输出文件不能与输入指向同一文件。

仅完整的 HTTP 202/code=0/state=accepted、匹配规范身份/hash、有效 attempt 才计 accepted。旧 HTTP 200、丢失响应、错误 JSON/过大响应、错身份/缺 attempt 均计 unknown。已观察到的 accepted 不因后续失败倒退。客户端拒绝 redirect，不将 token/body 跟随到其他地址。输出仅含计数和逻辑身份，没有 token/正文/原始错误；它不证明业务落库或接收端送达，详见[压测指南](load-testing.md)。

```powershell
go test -mod=readonly -race ./service ./router ./cmd/loadtest -run 'Test(SubmitCommand|SubmissionRoute|AcceptanceProbe)' -count=30 -timeout=60s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestCommandSubmissionIntegration$' -count=1 -v -timeout=5m
```

真实 suite 使用本轮独立 MQ/Redis/MySQL、实际 JWT/Redis 黑名单、新 Gin 路由和 publisher：

1. 202 后读回实际信封，核对认证发送者、规范身份/hash、内容/NUL/时间和 attempt。
2. 输入/伪造发送者/超大正文、无效/黑名单 token、Redis 客户端关闭不发布。
3. 代理丢失实际 confirm：首请求仍在途时额外请求 429；首请求超时后 503 unknown，而队列已有原文。恢复不自动重发，显式原键重试使用新 attempt 返回 202。
4. 缺失本轮绑定，真实 Return+ACK 返回 503 unroutable，不误报 accepted。
5. 按归属核验只停止本轮 MySQL，实际 HTTP 仍可得到 MQ 确认并返回 202。

未实测真实 broker NACK 的 HTTP 路径、生产 API owner/慢头慢正文、OS 信号投递、业务提交后 ACK 或客户端持久化终态；NACK 映射由默认测试覆盖。只在独立 fixture 积压/读取命令，fixture 控制连接的 ACK 不是业务提交后 ACK。版本、runId、结果见[实施记录](message-reliability-mq-progress.md)。

下一小点 P3.6.2：生产 API 角色的受控 HTTP 生命周期、Redis 鉴权探测与运行时装配。默认现代业务工厂继续缺失；P3.6/P3 尚未整体验收，不切换开发共享数据或旧消息入口。
