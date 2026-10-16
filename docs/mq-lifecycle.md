# MQ 资源托管生命周期（P3.5.3.3.1）

`application.StartMQLifecycle` 在 PrepareMQ 之上增加一个显式的 lifetime owner。它准备连接后持有一个关闭 goroutine，收到 lifetime 取消或 Close 请求时自动关闭整组资源。调用方可以通过 Done/Wait 等待关闭尝试完成并读取结果，不必把取消处理放进不受管理的回调。

这只管理 MQ 连接和 publisher 的既有恢复循环，不启动 HTTP、业务 worker、消费订阅、进程信号处理或 consumer 自动重连。主程序仍只运行 legacy，未装配的新模式检查继续保留。

## Context 的职责

| 入口/context | 取消后的行为 |
| --- | --- |
| PrepareMQ(setupCtx, ...) | 仅控制准备；成功返回后取消 setupCtx 不影响已移交连接 |
| StartMQLifecycle(lifetimeCtx, ...) | 同时控制准备期间取消和成功后的资源 lifetime；取消会自动关闭整组连接 |
| owner.Wait(observerCtx) | 只停止本次等待；不关闭资源，不取消其他等待者 |
| owner.Close(observerCtx) | 无论 observer 是否已取消都请求关闭；observer 只限制本次等待/后续清理重试 |

SetupTimeout 继续是单次准备的总上限，成功后由 PrepareMQ 清除其 watcher；它不会终止仍有效的 lifetime。lifetime 自身可以有 deadline，到期会主动关闭连接。复用已有 MQOptions 的范围和默认值，不新增 YAML/env 参数。

```go
mqLifetime, cancelMQ := context.WithCancel(context.Background())
defer cancelMQ()
owner, err := application.StartMQLifecycle(mqLifetime, conf, application.DefaultMQOptions())
if err != nil {
    return err
}
// owner.Publisher() / owner.Consumers()：已准备的能力；消费者尚无订阅。
// 业务装配先停止接单/领取并等待在途事务，再关闭 MQ lifetime。
cancelMQ()
return owner.Close(context.Background())
```

worker 尚未实现，示例没有实际订阅或事务。真正的业务装配必须先停止接单/领取、限时等待处理器完成提交及 ACK，再取消 mqLifetime/调用 Close。不要直接把会在停止请求时立即取消的进程 signal context 作为在途业务的 MQ lifetime；否则连接可能先于事务 drain 被关闭。角色能力和这套分阶段退出顺序在 P3.5.3.3.2 接入。

## 所有权与关闭结果

StartMQLifecycle 先调用既有 PrepareMQ，完成后再次检查 lifetime。若取消与成功准备竞争，返回 nil owner，并用独立关闭预算回收资源；工厂返回 handle+error 也清理，构造/清理错误都保留。最终检查之后发生的取消由已经移交的 owner 处理，owner 可能很快进入关闭状态。

取消或显式 Close 后访问器不再提供 publisher/consumer 能力；Consumers 返回列表副本，改副本不能改变内部清理责任。之前取得的 handle 仍由各 child Close 的准入停止/transport 中断规则约束，不能把访问器隐藏能力当作已经完成所有关闭的证明。

owner 始终用独立 context 调用 MQResources.Close，继承已有整组 CloseTimeout，而不是已取消的 lifetime/observer。child Close 共享总期限、全部调用并 join，不按连接数量累加时间。只有一次自动关闭 owner；多个 Close/Wait 不创建新的监听 goroutine。

Done 表示自动关闭尝试已完成，可以调用 Wait 读取其结果。正常取消/显式停止后的完整清理返回 nil；关闭超时或其他错误明确返回 ErrMQClose，并保留 errors.Is 的底层类别。Done 已关闭但 Wait 返回错误时，不能宣称所有 child 后台 owner 都已退出。

Close 的等待取调用方 deadline 和 CloseTimeout 中更短的一个。观察者先返回时，唯一 owner 继续执行有界清理；可以用新的 observer context 再次 Wait/Close 加入等待。等待 context 的取消不会侵占 owner 的独立清理预算。

若自动关闭失败，后续 Close 会在本次剩余预算中调用 MQResources.Close 重试 child 清理；各资源自身可能保留先前关闭错误，重试不保证成功。Wait 的自动尝试结果固定，Close 即使重试成功也保留第一次错误，避免把已发生的退出异常擦掉。Error/fmt 不输出原始 transport/factory 错误或凭证；不要记录解包后的原始 cause。

## 验证

```powershell
go test -mod=readonly -race ./application -run '^TestMQLifecycle' -count=1 -v
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestMQPreparationIntegration$' -count=1 -v -timeout=6m
```

单元测试覆盖四种模式的取消与重复关闭、无效输入、半完成/取消交接回滚和错误脱敏、Wait 取消与资源 lifetime 隔离、准备期限不结束已移交 lifetime、短观察者先返回后再次 join、lifetime deadline 后的独立清理预算、真实关闭超时的可见结果/重试、多个并发 Close 合并。

隔离 Docker suite 在原六个连接准备/失败回滚场景之外，增加 managed_api_cancel、managed_worker_cancel、managed_all_cancel、managed_all_expired_close_observer 四个场景。真实连接建好后取消 lifetime 或用已取消的 observer 请求关闭，Wait 加入完成，broker app 连接回到唯一 fixture 控制连接。主/隔离队列仍各 ready=1、unacknowledged=0、consumers=0，/api/consumers 为空；最后控制连接读取/ACK，验证完整待处理内容未改变。统计缺失值不视作 0，管理采样等待沿用原 fixture 规则。

测试不读取开发 .env/config，不启动业务主程序，不访问共享数据。实际版本、runId、时长及 cleanup 见实施记录。本轮不计为业务提交后 ACK、进程信号/在途事务优雅退出或 consumer 恢复验收。

P3.5.3.3.2.1 [角色构造器与服务实例检查](runtime-assembly.md)、P3.5.3.3.2.2 [受控服务执行与退出编排](runtime-execution.md) 已完成。RunRuntime 直接持有 PrepareMQ 资源，先 drain 服务再关闭 MQ，不将进程取消直接传给本页的连接 lifetime owner。[现代入口与信号](runtime-process-entry.md)和 [legacy 统一生命周期与信号](legacy-runtime-lifecycle.md)均已接入；下一项为 P3.5.4 接受 readiness 与持续拓扑核验。默认现代业务工厂仍为空，HTTP 202、业务事务、隔离审计与 Outbox 仍按 P3.6/P4/P5 实现。
