# P6.3：模拟客户端发送恢复

仓库尚无实际前端。`tests/clientsim` 提供发送状态核心、磁盘适配与有界 HTTP 恢复；P6.3.1—P6.3.3 已实现，生产服务与通知交叉按后续小点验收。

| 小点 | 范围 | 状态 |
| --- | --- | --- |
| P6.3.1 | 待发送记录、稳定身份、单调状态与可恢复快照；注入式原子提交 | 已完成 |
| P6.3.2 | 磁盘快照适配、独占所有权、跨进程恢复及存储故障验收 | 已完成 |
| P6.3.3 | 有界 HTTP 提交、状态查询、退避/取消和原键重试 | 已完成 |
| P6.3.4 | 通知结果与 HTTP 交叉、真实生产 API 的发送恢复验收 | 下一项 |

本地接收消息集/连续游标归 P6.4，重连/周期同步归 P6.5；不将本项待发送记录中的终态消息当作接收侧完整消息集或 ACK 依据。

## 本地记录与提交边界

`OpenSender(ctx, authenticatedUser, store)` 借入单用户 `SnapshotStore`。该 store 必须由一个 Sender 独占，旧 Sender 停止使用后才可重新加载；核心不创建 goroutine，不关闭借入存储。Load 返回 nil 表示新存储，Save 必须在整个快照原子提交成功后才返回 nil，不能保留/修改传入字节或回调 Sender；具体追加日志、Sync、操作系统锁和跨进程恢复见 [P6.3.2 磁盘存储](client-sender-file-store.md)。

使用顺序：

1. 调用方首次创建稳定 UUID，构造 `messaging.SendRequest`；`Queue` 将原请求完整保存后返回记录。
2. 保存记录的 `ClientMessageID` 为规范键，调用 `BeginSubmit(ctx, key)`；核心先保存 attempts 和 inFlight，再返回含原请求的 Attempt。
3. 后续传输应再次检查 context，并使用 Attempt.Request 发请求。成功解码的证据交给 CompleteSubmission；响应丢失或不能使用时调用 CompleteUnknown。
4. 将完整、已解码状态查询结果交给 ApplyStatus。引用式 command.result 应继续查状态，不能伪造完整消息。

P6.3.1 核心不发送 HTTP 或解析原始网络 JSON；P6.3.3 已提供[有界 HTTP 与恢复](client-sender-http.md)，负责 HTTP status/code、响应字节上限、严格解析、鉴权及有限调度。CompleteSubmission 验证逻辑身份/hash、允许状态与 reasonCode、必要的规范 publishAttemptId；ApplyStatus 调用共享 ValidateFor，核对原发送者/key/hash 和完整规范消息。

Queue 保留原始字段值，包括 UUID 大小写、目标 ID 的可规范写法、正文空白/NUL/Unicode 和纳秒客户端时间。规范 commandId/hash 由共享 Normalize 派生；不从回执替换本地 hash。已存在规范键只接受完全相同的原请求，即使只改时间或规范写法也拒绝覆盖。主动改内容或业务拒绝后再次发送须显式创建新 UUID；核心不因超时、缺记录或重启生成新业务键。

每个键最多一个未完成本地尝试；attempts 是 JSON 字符串计数，不是业务消息数或 MQ 发布次数。完成回调必须同时匹配当前计数和原请求；旧/重复/伪造票据返回 ErrStaleAttempt。终态、冲突和隔离记录禁止 BeginSubmit；显式重试 accepted 仍保留既有接受证据。

## 状态规则

| 证据 | 本地行为 |
| --- | --- |
| 尚未提交 | queued；保存成功后才可提交 |
| 首次或 not_published 后再次提交 | submitting + inFlight；先写快照再返回 Attempt |
| accepted | 记录接受证据；不是业务终态或送达证明 |
| 丢失响应 / publish_unknown | 保留原请求，记 publish_unknown；已有 accepted 不倒退 |
| not_published / unroutable / nacked | 记本次 not_published，保留记录与原键；不推断其他历史尝试未成功 |
| pending_or_unknown | 较弱的非终态，不覆盖 accepted、quarantined、ID_CONFLICT 或业务终态 |
| quarantined / ID_CONFLICT | 保留原请求并阻止重新提交，等待终态核对或用户处理；不能伪装成功 |
| persisted / rejected | 业务终态；重复相同结果幂等，迟到 accepted/unknown/隔离/冲突不倒退 |
| 不同业务终态、不同规范已提交消息 | ErrContradiction，保留原终态和快照 |

终态可先于提交回执保存，之后 accepted 只结算本地 inFlight。完整规范消息保存在待发送终态结果中，不推进接收连续游标。完整匹配的 persisted/rejected 也可核对此前冲突或隔离记录。

存储 Save 返回错误时，写入可能已经完成：本次操作不返回新记录/Attempt，当前 Sender 拒绝继续操作或读取旧状态，须重新加载快照。格式化错误不带底层诊断，errors.Is 仍可检查原因。即使上层恢复存储 panic，原 Sender 也保持不可用。已知 Save 成功不因随后 context 取消而抹除；传输必须独立检查取消。

重新加载时清除 inFlight：中断的 submitting 变为 publish_unknown，保留原请求和尝试计数；已知 accepted、隔离、冲突或终态保留。恢复变更也先保存再允许使用；失败不返回可用 Sender。

## 快照与验证范围

私有 version=1 快照绑定规范 senderId，记录按规范 UUID 排序，最多 512 条、16 MiB。包含原请求、逻辑身份/hash、状态、字符串 attempts、inFlight 及可选完整结果；没有 token、密码或自动删除逻辑。容量耗尽或计数到达最大 uint64 时明确失败，不丢记录、不回绕。

加载会重算请求身份/hash并验证状态组合及完整结果。只接受本编码器产生的精确 JSON：重编码比较拒绝未知/重复/null 字段、额外空白/尾随数据、非法 Unicode 替换、非规范数字及错误顺序。跨用户、重复记录、坏版本/状态/hash/消息整份拒绝，不修补或跳过损坏记录。返回值深复制结果指针，默认格式化 Sender/记录/Attempt 隐去正文。

```powershell
go test -mod=readonly -p=2 -race ./tests/clientsim -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

P6.3.1 受控字节存储覆盖状态与对象重建恢复；P6.3.2 以真实文件和强杀子进程验证 Windows 磁盘及跨进程恢复；P6.3.3 验证真实 loopback HTTP 及磁盘重开的发送恢复。证据分别见[磁盘存储](client-sender-file-store.md)和 [HTTP 恢复](client-sender-http.md)。生产 JWT/MySQL/MQ/通知交叉及实际前端联调仍待后续。运行记录见[实施进度](message-reliability-mq-progress.md)，下一项 P6.3.4。
