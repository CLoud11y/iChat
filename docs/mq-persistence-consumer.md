# 持久化单条消费适配（P4.1.1）

`workers.PersistenceHandler` 将一个 `rabbitmq.ConsumerDelivery` 交给 P2 的 `MessageRepository.PersistCommand`，仓储成功结束后才调用原 handle 的单条 ACK。构造不联网，不启动循环，也不持有数据库或消费者连接的关闭权。

P4.1 拆为两个可审查小点：P4.1.1 完成 wire 校验、事务结果、ACK 与重投验证；[P4.1.2](mq-persistence-lifecycle.md)已实现顺序 worker 的准入、停止、drain、取消、join、角色工厂与就绪。P4.1 范围完成，P4 整体尚未验收；默认进程未装配后台角色，worker/all 继续在 I/O 前拒绝缺失角色。

## 输入与信封校验

`NewPersistenceHandler` 接收具体的 P2 仓储；`Handle(ctx, delivery)` 要求非空 delivery 和带 deadline 的 context，整个事务及 ACK 共用该期限。nil、无期限、预取消输入不执行 SQL 或 ACK。超时不另起无法 join 的后台事务。

输入必须符合当前 v1 publisher 的 wire 契约：

- 持久消息，ContentType 为 `application/json`，ContentEncoding 为空。
- 来自 `ichat.commands.v1` / `message.submit.v1`，不能把隔离队列的审计 delivery 当作新业务命令。
- 使用共享严格 JSON 解码：正文最多 16384 字节，拒绝无效 UTF-8/Unicode、重复/未知字段、null、额外 JSON 及不支持版本。
- 重新规范化业务字段，核对 commandId/payloadHash；检查规范 clientMessageId、senderId、targetId、时间及 publishAttemptId。
- AMQP MessageID 等于 commandId，CorrelationID 等于 publishAttemptId。

正文、headers 和原 delivery 保持原样；不使用 redelivered、delivery tag 或 `x-delivery-count` 决定业务身份，也不依据这些元数据跳过事务。headers 可以包含 broker 的重投审计字段。这里校验的是传输一致性，生产者凭证和发送者身份仍由接受入口与部署权限负责。

## 事务与 ACK

仓储复用 P2 的完整事务：命令占位/唯一键锁 → 当前权限 → conversation/seq/message → 命令终态和 Outbox → COMMIT。适配器不自行插表、分配序号、发送通知或更新缓存。

| 仓储结果 | 消费处置 |
| --- | --- |
| 新 persisted | 完整事务提交后 ACK；一条 message、一个 seq、两个 Outbox 事件 |
| 新 rejected | 拒绝及 command.result 事件提交后 ACK；没有 message/seq |
| 同 key/hash 重放 | 仓储结束只读重放事务后 ACK，不重新授权或创建事件 |
| 同 key 异 hash | 原规范记录不变，返回无原结果的 ID_CONFLICT，事务结束后 ACK |
| SQL/回滚失败、COMMIT 未知 | 返回错误及空 execution，不 ACK，不补偿删除 |
| 零值、部分或矛盾的终态 | 返回错误，不 ACK |

终态门禁复核 disposition、请求身份/hash、时间、状态和消息/会话/拒绝字段，防止误把空结果或未完成占位当作成功。最终 SQL 不变量和 Outbox 原子性仍由 P2 仓储保证。

ACK 始终调用原 `ConsumerDelivery.Ack`，沿用原 channel/tag、`multiple=false` 和旧 handle 隔离。nil 错误只证明本地 ACK 方法完成；broker 没有消费 ACK 的确认回复。ACK 写入失败或结果不明返回空 execution，不重发 ACK，不把旧 handle 用到新 session。即使数据库已经提交，也通过原业务身份的重投恢复，不生成新 clientMessageId。

## 失败分类与所有权

`PersistenceError.Kind` 可用于下一步 worker 策略；正常 `%v` / `%+v` / `%#v` 格式不包含正文、headers、SQL 或凭证。`errors.Is/As` 保留底层原因供受控诊断。

| Kind | 当前行为 |
| --- | --- |
| `invalid_delivery` | wire/版本/身份错误；保留原文和原 delivery |
| `persistence_failed` | 任意仓储错误；保留底层错误，不在此处决定重试或隔离 |
| `commit_unknown` | 明确保留提交不确定性，不 ACK |
| `invalid_terminal_result` | 终态不变量错误，不 ACK |
| `acknowledgement_failed` | 仓储成功后期限到期、取消或 ACK 错误；保留重投责任 |

每次调用最多执行一次仓储、一次 ACK；没有 Next、Reject、本地重试、自动重连或隔离发布。调用者遇错必须停止领取，保留当前责任并由原 channel 的 owner 受控关闭，之后通过新 session 和数据库幂等恢复。不能立即 requeue 热循环，也不能因为暂时数据库故障耗尽本地次数就把正常命令当坏消息。自动暂停/退避/恢复、可靠隔离转移和隔离审计归后续 P4；本轮错误分类不宣称这些策略已实现。

## 验证

使用 README 的固定 Go 1.27.1、仓库 GOCACHE 和不存在的 `ICHAT_CONFIG_FILE`，不读取开发 .env/config。

```powershell
go test -mod=readonly -race ./workers -count=30 -timeout=60s
go test -mod=readonly -race -tags=integration ./workers -run '^TestPersistenceConsumerIntegration$' -count=1 -v -timeout=9m
```

默认测试覆盖 wire 错误零 SQL/ACK、原文保持、规范命令传递、各类合法终态、部分终态拒绝、仓储屏障、错误分类、COMMIT 未知、ACK 失败、输入及提交后取消。没有外部依赖。

实际 Docker suite 使用同一轮独立 MySQL/RabbitMQ：

- 文本/图片/音频及群消息真实落库，重复 attempt 和跨目标内容冲突保持唯一 message/seq/事件。
- 已接受后移除群成员，新命令得到稳定 rejected；以前成功命令重放不依据当前权限改写终态。
- 两个独立 consumer 同时处理同身份，一次 applied、一次 replayed。
- 真实子进程在实际 SQL COMMIT 前、COMMIT 成功后且 ACK 前停在仅测试构建的 driver 屏障；父进程用独立 SQL 连接核验可见性，然后强杀并 wait，原 attempt 重投分别 applied/replayed。
- 实际 COMMIT 成功后由 driver 返回 EOF，适配器报告 commit_unknown；关旧 channel 后原 delivery 重投，规范结果和事件集合不增加。此处为调用结果注入，未模拟真实 TCP COMMIT 响应丢失。
- 核验归属后只停止本轮 MySQL，处理失败不 ACK；恢复数据库并等待可用，新 session 读回原 attempt 后完成落库。
- 坏 JSON 处理失败后关旧 channel，完整 body/自定义 headers 仍可重投；不以 fixture ACK 冒充隔离审计。
- 全部成功处置后先关闭消费者，再用控制连接检查队列为空；最终核验 last_seq、消息数、最大 seq 一致，无序号洞。

本节 P4.1.1 的子进程直接使用本适配器和真实仓储/消费传输。后续 [P4.1.2](mq-persistence-lifecycle.md)补上角色生命周期和真实 ACK 丢帧与已持久化数据的组合；自动恢复、隔离审计、Outbox 通知、实际客户端及三节点仍待后续阶段。各轮独立证据不能合并称为端到端验收。
