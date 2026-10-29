# P6.3.3：模拟客户端 HTTP 提交与发送恢复

`tests/clientsim.HTTPRunner` 将[发送状态核心](client-sender-simulator.md)接到有界 HTTP。P6.3.3 已完成；本轮服务器使用真实 loopback HTTP 夹具，生产 JWT/MySQL/MQ/通知交叉归下一项 P6.3.4。

## 构造、请求与所有权

调用 NewHTTPRunner(sender, DefaultHTTPOptions(origin, tokenProvider))，传入已有 Sender 和可刷新 Bearer token 的函数。构造纯校验，不调用 tokenProvider 或联网。Origin 仅允许 http/https 源站，可带末尾 `/`；拒绝用户信息、路径前缀、query 和 fragment。token 不保存到快照。

| 选项 | 默认值 | 范围 |
| --- | --- | --- |
| RequestTimeout | 3 秒 | 正值，最多 1 分钟；覆盖并发槽等待、token、提交检查点及 HTTP |
| StoreTimeout | 3 秒 | 正值，最多 1 分钟；用于已发起尝试的本地结算 |
| MaxConcurrent | 2 | 1—8；每个 runner 的实际请求槽上限 |

runner 自有 client/transport，不使用环境代理、Cookie jar 或自动解压，不跟随重定向。POST 不提供 GetBody 和幂等重放 header，由恢复策略显式决定何时再提交；固定 Go 1.27.1 的 Request.isReplayable/Transport.shouldRetryRequest 路径已在本机复核。

Submit(ctx, key) 完成一次提交，Poll(ctx, key) 完成一次查询；Recover 执行有限恢复。相同键的活动任务合并为一个 owner，重复入口立即返回 ErrInFlight，不额外发送请求。不同键共享有界槽，退避时释放请求槽，但保留该键的任务所有权。应用应由一个 runner 管理同一 Sender。

Close 撤销准入，取消 token/请求/槽等待及退避，等待实际调用返回后关闭空闲连接；并发 Close 加入同一次清理。它不关闭 Sender 或借入磁盘存储，调用方须在 runner 退出后再关闭 FileStore。回调应遵守 context；不通过丢弃 goroutine 假装不合作的回调已结束。

## 提交、查询与回执校验

提交使用 `POST /auth/sendMessage`，Bearer 仅放 Authorization 头。先保存 submitting 检查点并取得 Attempt，再对原 Request 字段值编码发送；不从回执替换本地 key/hash、正文或客户端时间。查询使用 `GET /auth/messages/status`，只携带本地规范 clientMessageId 和 payloadHash，不带 GET body。

所有响应先有界读至 16384 字节加一个超限判定字节。只接受 application/json（可带 UTF-8 charset）、无 Content-Encoding、包含独立 no-store 指令的响应。共享 DecodeSubmissionResponse/DecodeCommandStatusResponse 检查包装必填 code/msg/data、精确字段名、重复/转义后重复字段、null、UTF-8/UTF-16 及尾随内容；嵌套 message 同样严格校验。

| 入口与响应 | 行为 |
| --- | --- |
| 提交 202/code=0/accepted | 核对原身份/hash、规范 publishAttemptId 和状态/reason 组合后保存 accepted |
| 提交 429 或 503 的完整固定身份回执 | 验证 HTTP/code/state/reason 配对；保存 not_published 或 publish_unknown，保留原请求 |
| 提交响应丢失、不可用或不可信 | 使用独立 StoreTimeout 保存未知结果；既有 accepted/终态不倒退 |
| 查询 200/code=0 | 共享 ValidateFor 核对原用户/key/hash、状态组合和完整规范消息，再保存状态 |
| 查询 409/code=-1/ID_CONFLICT | 保留冲突记录，恢复任务停止，禁止覆盖旧消息 |
| 400/401/403 固定错误 | 返回请求/认证错误，保留原记录；发送侧不将其伪装业务 rejected |
| 查询 503、网络故障或坏回执 | 不改写为 pending，不从失败推导可以重新提交 |

旧 HTTP 200 发送成功、错 code、错身份、缺 attempt、损坏消息和重定向均不能证明接受或持久化。查询 503 即使携带 pending 数据也拒绝。消息序号使用共享字符串 uint64 解码，支持最大值。

网络/读取失败后，即使调用者已取消，仍通过 WithoutCancel 保留值并使用独立、有期限的结算 context，同步完成 CompleteUnknown。有效完整回执也先结算后返回；存储结果未知时停止网络恢复，要求重开快照。已知保存成功不因随后取消被抹除，终态始终受核心单调规则保护。错误默认格式化不暴露源站、token、正文或底层诊断，errors.Is 可检查原因。

## 有限恢复策略

DefaultRecoveryOptions：MaxRounds=8、MaxSubmissions=2、BaseBackoff=1 秒、MaxBackoff=15 秒。允许 1—100 轮，提交预算 0—MaxRounds；退避为正且上限不超过一分钟。0 提交预算表示只查询。

1. 每轮先读本地记录。persisted/rejected 结束；ID_CONFLICT 返回 ErrBlocked。
2. queued 可使用提交预算先发一次；已有 accepted/unknown/not_published 先查询。quarantined 只查询。
3. 只有完整、匹配的 pending_or_unknown 查询才允许下一轮按原键原请求重发；查询故障不授权重发。每次提交后重新查询，不连续重发。
4. 每轮最多一个请求。指数退避按 MaxBackoff 封顶，服务器 pollAfterMs/Retry-After 为等待下限；Retry-After 支持秒数和 HTTP 日期，最多采纳一分钟。服务器提示可超过客户端 MaxBackoff。
5. 取消/关闭立即打断计时器和请求等待；存储、身份和固定请求错误停止。轮数耗尽返回 ErrRecoveryExhausted，保留非终态记录。

次数是上限，token/槽等待/检查点失败可能提前结束或消耗一次提交预算，但不会增加网络请求。调用方可提供更短的整体 context。此处没有永久后台轮询、隐式新 ID、删除 pending、自动登录、接收游标或业务 ACK。

## 验证与范围

```powershell
go test -mod=readonly -p=2 -race ./messaging ./tests/clientsim -run '^TestHTTP' -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

真实 loopback HTTP 覆盖 202/各查询终态/最大 seq、固定错误、重复/未知/null/别名/Unicode/嵌套坏 JSON、错身份/HTTP/code、超大/错误媒体/压缩/缓存头、拒绝重定向、实际断开响应、慢 body 和取消。并发与关闭测试用屏障证明请求槽上限、同键不重复、token 或本地结算回调返回前不会结束 owner。

真实 FileStore 流程：首 POST 已读原请求后主动断开；重开磁盘、查询 pending、显式同键同内容提交 202、再次查询 persisted、再次重开磁盘保留终态。核对两次 POST 请求字节完全相同和原 hash/计数/完整消息。服务器为夹具，不是生产 MQ/SQL 管线。

恢复测试以确定性计时器记录建议等待值（包括 2 秒 poll 下限、3 秒 Retry-After），不把该测试报告成真实耗时测量；另用实际长计时器验证取消/Close 能打断等待。预算、隔离/冲突、认证、SQL/协议失败和存储已写后报错均单独覆盖。阶段证据见[实施记录](message-reliability-mq-progress.md)，下一项 P6.3.4。
