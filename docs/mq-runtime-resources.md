# 按角色准备 MQ 连接与失败清理（P3.5.3.2.2）

`application.PrepareMQ` 将配置计划变成显式持有的 MQ 资源。它从 `config.Config` 重新生成并验证计划，不接收可被任意修改的 MQRuntimePlan。配置错误、缺失必需工厂和已经取消的 setup context 在外部 I/O 前失败。

这仍是独立的依赖准备层。业务 bootstrap 的新模式装配检查继续保留；当前主程序仅运行 legacy。准备连接不代表 HTTP 202、业务事务、隔离审计、Outbox 或整体 readiness 已实现。

## 创建的资源

| 模式 | 命令 publisher | 持久化消费者连接 | 隔离审计连接 |
| --- | --- | --- | --- |
| legacy | 无 | 无 | 无 |
| api | 1 个 Supervisor | 无 | 无 |
| worker | 无 | consumerWorkers 个 | 1 个 |
| all | 1 个 Supervisor | consumerWorkers 个 | 1 个 |

每个消费者单独 DialConsumer，拥有自己的 TCP/AMQP connection；不共用 publisher 或其他 worker 的连接。默认消费者数量为 4+1，上限 32+1；all 另有一个 publisher connection（重连时按 Supervisor 的旧连接回收规则更替）。这不是测得的生产容量。

publisher 通过 StartSupervisor 异步建立连接并恢复；api 在 broker 离线时也可以完成资源准备，此时 Status.Ready=false，Publish 不能无依据返回 accepted。worker/all 的消费者连接必须在总准备期限内完成只读拓扑核验，否则整个准备失败。PrepareMQ 的成功只表示资源已移交给调用方，不能直接用作接受 readiness。

消费者准备阶段不调用 QoS、Consume、Next、ACK 或 Reject。`Consumers()` 的每项保存固定 Queue、Prefetch 和连接能力，实际订阅须等对应业务处理器及存储能力装配完成。两类连接复用既有版本/vhost/拓扑核验；发现缺失或漂移只失败，不声明、修复或删除拓扑。

## 期限与所有权

```go
resources, err := application.PrepareMQ(setupCtx, conf, application.DefaultMQOptions())
if err != nil {
    return err
}
defer resources.Close(context.Background()) // 实际装配层必须处理关闭错误。
// resources.Publisher()：发布/就绪能力；可能仍在连接或退避。
// resources.Consumers()：闲置连接的列表副本，尚无消费订阅。
```

MQOptions 是 Go 装配参数，本轮没有添加新 YAML/env 字段：

| 参数 | 默认 | 范围 | 含义 |
| --- | --- | --- | --- |
| SetupTimeout | 30s | 正值，不超过 60s | 所有连接创建共用一个总准备 deadline；调用方更早的 deadline 优先 |
| CloseTimeout | 5s | 正值，不超过 60s | 整组关闭/失败回滚的总上限；不按连接数累加 |

各 session 的 connect/operation/write/close 期限继续生效；总准备期限可以更早中断它们。32 个连接不保证在任意网络条件下都能完成 30s 的默认准备，超时即失败回滚。

setupCtx 仅控制准备。publisher 使用资源组拥有的独立 lifetime，在准备期间由取消 watcher 转发 setup 取消；成功返回之前移除并 join watcher。成功移交后，setupCtx 的取消或期限不会关闭已准备的连接，也不会停止 publisher 恢复。调用方必须显式 Close；进程信号、业务任务和服务 lifetime 的装配归 P3.5.3.3。

任何步骤失败，返回 nil 资源并关闭所有已经拿到的 handle，包括工厂同时返回 handle+error 的情况。回滚用独立的 Background context 加 CloseTimeout，不能继承已经取消的准备 context。尚未返回的部分连接由既有 DialConsumer 内核自行中断/回收。

Close 首先停止资源能力访问并取消 publisher lifetime，随后在同一个 close deadline 下并发关闭所有持有资源。即使传入的 context 已取消，仍调用每个 child Close，使其停止准入并中断自身 transport。最多 34 个关闭调用，全部 join；不为每个连接重新分配完整关闭时间，也不遗弃 Close 调用。工厂是包内实现，返回的 closer 必须遵守 context 期限。

并发 Close 合并为同一次关闭尝试，每个等待者保留自己的 deadline。成功后重复 Close 为 nil；失败后可以再次 Close 尝试等待仍在退出的 publisher owner。child session 自身可能保留此前的关闭错误，重试不保证清除错误。关闭超时/错误明确返回，不宣称后台 owner 全部退出。`Consumers()` 返回副本，调用方删改副本不会删改内部清理清单；开始关闭后访问器返回空能力。

配置错误使用既有 config sentinel；工厂/准备错误为 ErrMQPrepare，关闭错误为 ErrMQClose，选项错误为 ErrMQOptions。errors.Is 保留底层分类和取消/期限信息；Error/fmt 不打印原始 factory/transport 错误、URL、密码或正文。不要记录解包后的原始 cause。准备失败与清理失败同时发生时，两者都被保留。

## 验证

```powershell
go test -mod=readonly -race ./application -count=1 -v
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestMQPreparationIntegration$' -count=1 -v -timeout=6m
```

单元测试使用注入的包内工厂及专有本机 peer，覆盖角色/最大数量、零 I/O 校验、半完成/typed nil、各步骤回滚、总 deadline、取消交接、关闭并发/重试、清理错误传播与脱敏，以及离线 API 异步恢复。

真实测试只使用 StartWithRabbitMQ 的独立 Docker project/vhost、账号、端口和卷。主/隔离队列各预放一条持久消息，验证四种模式的独立连接数量、成功交接后的取消隔离、零订阅且 ready=1/unacknowledged=0，以及关闭后回到控制连接基线。代理让后续 AMQP 握手收不到回复，验证 worker/all 已有连接后的实际超时与完整回滚；最后仅 fixture 控制连接读取并手动 ACK，逐字段验证待处理内容/attempt 未变化。

管理 API 的连接/队列统计有约 5s 采样延迟，真实超时场景使用 8s 总准备期限，以观察回滚前的连接；单元测试保留毫秒期限证明期限不按 worker 重置。统计缺失字段不视作零，订阅另直接检查 /api/consumers。测试不读取开发 .env/config，也不启用业务 bootstrap。

后续：P3.5.3.3 生命周期装配与角色能力核验；consumer 自动恢复、readiness、HTTP 202、提交后 ACK 和 Outbox 继续按后续阶段完成。
