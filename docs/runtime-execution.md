# 受控服务构造、激活与退出（P3.5.3.3.2.2）

`application.RunRuntime(ctx, conf, factories, options)` 提供现代角色的阻塞式执行器：先检查全部必需构造器，再准备 MQ、构造全部服务、验证实例集合，最后激活。运行中等待进程 context 取消或服务意外退出；返回之前负责服务及 MQ 的有界清理。

本小点提供执行器及注入契约。[主程序分流和现代进程信号](runtime-process-entry.md)已接至此执行器，P3.6.2 已装配[生产 API](api-runtime-lifecycle.md)；落库、隔离及 Outbox 业务角色仍缺失，worker/all 继续在连接前拒绝。Check/成功激活均不是某次 accepted 或业务提交后 ACK 的证明。

## 构造与激活

1. 验证 context、RuntimeOptions、全部所选业务构造器及 MQ 参数；无效配置或缺失角色不能触发依赖工厂。RunRuntime 不运行 legacy，legacy 使用原 bootstrap。
2. PrepareMQ 创建显式持有的 publisher/独立消费者连接。重新核对实际资源的模式、publisher 选择、队列/数量/prefetch/非空消费能力，再为角色提供各自的 MQ 输入及只读 Health 观察。
3. 按稳定角色顺序构造全部服务（API、持久化、隔离、Outbox），构造器只能准备闲置对象。收集每个已返回 handle，包括 handle+error；缺失/nil/typed nil、可比较且重复使用的实例均拒绝。重复实例只保留一份清理责任。
4. 对受控服务集合执行 ValidateServices，通过后才按 Outbox → 隔离审计 → 持久化 → API 激活。API 最后启动；每次后续激活前检查已经启动的角色是否退出。
5. Start 使用共同 setup deadline 和独立 service lifetime；启动成功后清除 setup deadline 的控制。Done 必须是稳定、非 nil、只通过关闭通知退出的 channel，Err 在 Done 关闭后本地读取。构造器和服务须遵守 context，并提供非阻塞的 StopAdmission/Done/Err。
6. 全部启动和最终检查成功后发布 Health 绑定；接受/消费者就绪还需要各自显式业务能力及当前依赖状态，见[运行时就绪状态](runtime-readiness.md)。进程取消或任何清理路径先撤销健康就绪，再执行停止准入/drain；撤销健康状态不会提前取消 MQ lifetime。

API 仅获得命令 publisher；持久化获得 consumerWorkers 个主队列连接；隔离审计获得一个隔离连接；Outbox 当前没有 MQ 输入。每组消费者 slice 独立，不能通过修改元数据删除 MQ 组内部清理清单。存储/鉴权依赖由明确的工厂闭包注入。

工厂必须返回独立的服务实例；执行器能检测可比较值的重复身份，不能证明非可比较对象内部没有共享资源。各角色的物理连接独立性由 PrepareMQ 保证。工厂不得在构造时先发布命令、启动 HTTP/订阅或领取任务，接口本身不能阻止违约实现。

## Context 与期限

```go
err := application.RunRuntime(processCtx, conf, roleFactories, application.DefaultRuntimeOptions())
```

主程序现代分支已采用这一调用方式，当前仍没有可注册的完整业务 roleFactories。RunRuntime 自己持有 PrepareMQ 资源；没有把 processCtx 直接交给 StartMQLifecycle，以免停止请求先切断在途事务的 MQ 通路。

| 参数 | 默认 | 范围 | 约束 |
| --- | --- | --- | --- |
| StartupTimeout | 30s | 正值，≤60s | MQ 准备、全部构造与全部 Start 共用的工作期限，调用方更早的 deadline 优先 |
| DrainTimeout | 10s | 正值，≤60s | 所有已尝试激活角色共用的 drain 阶段预算 |
| CloseTimeout | 5s | 正值，≤60s | 所有服务 Close 和最后的 MQ Close 共用预算 |
| MQ | 既有 DefaultMQOptions | 既有范围 | MQ 自身的准备/关闭上限继续生效 |

这些是 Go 装配参数，本轮不新增 YAML/env 字段。StartupTimeout 限制正常启动工作；失败之后的回滚有独立预算，不能将启动 deadline 误当作整个失败返回的时间。PrepareMQ 自己也可能在失败返回前使用其 MQ.CloseTimeout 完成内部回滚。回滚/退出不继承已经取消的 process/setup context。

工厂和服务适配器属于受控代码，必须遵守传入的期限。执行器不通过丢弃 goroutine 来伪装不可中断回调已完成；StopAdmission 不得做阻塞等待或 RPC。最多四个角色的 Drain/Close 调用分别并发并全部 join，不按服务数重复分配完整期限。

## 正常停止和失败回滚

正常取消发生在成功激活之后时，执行器按以下顺序退出；清理全部成功返回 nil，清理异常返回错误：

1. 同步停止全部已尝试激活服务的准入，顺序为 API、持久化、隔离、Outbox；全部停止后才开始任何 drain。
2. 并发 Drain，并保留 service lifetime 和 MQ 连接，让已接任务在独立期限内完成提交/ACK。仅定义机会和调用顺序，具体数据库不确定提交处理仍由 P4 实现。
3. 所有 Drain 返回后取消 service lifetime，在同一个关闭期限下并发 Close 全部已构造服务，包括未启动和失败返回的 handle。
4. 所有服务 Close 调用返回后，最后调用 MQResources.Close。即使前面的服务已耗尽关闭期限，仍触达每个 MQ child Close 停止准入/中断 transport；超时明确报告，不能假称所有后台任务已经退出。

构造失败时从未激活任何服务，跳过停止准入/drain，仍关闭所有已返回对象和 MQ。Start 调用前即记录 attempted，失败的 Start 可能已经创建订阅/工作，故也停止准入并 drain；从未尝试 Start 的闲置服务只 Close。Close 必须支持尚未 Start 或 Start 部分失败的对象。

构造、激活或交接期间取消返回启动失败并回滚；成功 handoff 后的 process 取消是正常退出请求。任何角色主动退出（即使 Err 为 nil）会触发 ErrRuntimeStopped 和全组停止；启动期间发现后台角色退出时不会继续激活 API。Done 发送值而非关闭也被拒绝。没有自动重建业务角色、重订阅或重放命令。

错误分为 ErrRuntimeOptions、既有角色/MQ 配置错误、ErrRuntimeStart、ErrRuntimeStopped、ErrRuntimeShutdown；errors.Is 保留启动、drain、服务关闭、MQ 关闭和取消/期限的独立原因。Error/fmt 只包含固定阶段与已验证角色，原始依赖错误可能含凭证，不能打印解包后的 cause。

## 已退役 MQ 连接的关闭竞态

全仓库回归复现了既有 consumer 取消后的 Close 竞态：owner 已关闭准入/中断 transport，但 AMQP reader 尚未发布 closed 状态，Close 再次写 connection.close 可能得到伪关闭错误。

连接内核现在在 Close 入口记录 owner 是否已退役。已退役连接只 join 关闭工作并中断自身 transport，不发起新的 graceful-close 握手；健康连接仍使用原有 CloseDeadline，并保留关闭响应黑洞的超时行为。新增受控 peer 测试强制保持 transport 健康但先退役准入，证明 Close 不再写新的握手；原 broker cancel/坏 delivery 测试连续 50 轮复测。

## 验证与范围

```powershell
go test -mod=readonly -race ./application -run '^TestRunRuntime' -count=1 -v -timeout=30s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^(TestRuntimeExecutionIntegration|TestRabbitMQSessionIntegration)$' -count=1 -v -timeout=7m
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQConsumerChannelIntegration$' -count=1 -v -timeout=6m
```

单元测试覆盖角色能力分配、所有构造先于激活、API 最后启动、全部准入停止先于 drain、service lifetime/MQ 保留到 drain 结束、全部服务 Close 先于 MQ Close；逐构造/激活失败、nil/typed nil/实例复用、异常退出/Done 违约、统一启动/退出期限、成功返回与取消竞争、多个错误保留与脱敏、坏 MQ 快照和部分返回也被覆盖。

真实测试使用 StartWithRabbitMQ 的独立 Docker 资源和 fixture 服务；fixture 没有业务处理器或订阅，不声称验证真实 DB COMMIT/ACK。四场景分别为 drain 中仍收到实际 publisher confirm、构造器 handle+error 回滚、部分激活失败、drain 超时。后台角色在 drain 中实际 Verify 消费连接；退出后 app 连接回到唯一控制连接。主/隔离原文均保留，另完整核对 drain 中发布的信封/attempt；仅最终 fixture 控制连接读取/ACK。

共享关闭内核另回归原 session 黑洞和消费者重投/ACK 丢帧 suites。版本、runId、时长与 cleanup 见实施记录。测试不读取开发 .env/config、不启用业务主程序、不修改共享数据。

P3.5.3.3 的[进程入口](runtime-process-entry.md)、[HTTP/WebSocket](legacy-http-lifecycle.md)、[cron](legacy-worker-lifecycle.md)及[默认 legacy 统一生命周期](legacy-runtime-lifecycle.md)已完成基础装配和退出接线；默认入口通过 net/http owner 管理请求，收到信号后先 drain 再关闭，未 join 时保留存储。[生产 API](api-runtime-lifecycle.md) 已为显式 api 模式装配，仍拒绝缺失后台角色。[接受就绪汇总与健康接线](runtime-readiness.md)、周期拓扑复核及 P3.6 接受入口已完成；阶段证据与后续边界见 [P3 验收对照](mq-reliability-acceptance.md)，下一项为 P4 持久化消费适配，提交后 ACK、consumer 恢复、审计和 Outbox 仍按后续阶段完成。
