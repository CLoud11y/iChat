# 进程入口分流与现代运行时信号（P3.5.3.3.3.1）

`main → run → runProcess` 现在按配置选择 legacy bootstrap 或 `application.RunRuntime`。配置只加载一次，所选角色全部通过纯检查后才允许进入相应路径。默认现代业务构造器仍为空，api/worker/all 继续在外部连接前报出全部缺失角色；本项没有实现或注册生产处理器。

## 分流与所有权

1. 加载配置，执行 `RuntimeFactories.Check`。非法模式、新旧 worker 冲突、缺失角色均先失败，不注册信号、不建立连接、不绑定旧全局依赖。
2. legacy（包括直接零值模式）将同一份配置交给 `bootstrapLegacy`。不再加载配置，也不要求现代执行器或信号工厂。原 HTTP/cron/健康探测/存储清理顺序保持不变。
3. api/worker/all 不调用任何 legacy 依赖工厂。完整角色集合通过后，再执行纯 `Config.MQRuntime()` 校验，然后注册 `signal.NotifyContext(Background(), os.Interrupt, syscall.SIGTERM)`。
4. 将原配置、角色构造器和 `DefaultRuntimeOptions()` 交给 `RunRuntime`。入口只负责信号 context，不自行准备 MQ、调用服务工厂或清理服务；执行器继续持有全部资源，按停止准入 → drain → 关闭服务 → 关闭 MQ 退出。
5. 阻塞等待执行器返回，再 defer stop 取消 context、注销本次信号订阅。正常退出和任意失败均走此路径；不丢弃清理 goroutine，也不在信号到达时直接关闭 MQ。

`bootstrap(deps)` 保留为旧的加载配置入口，并复用 `bootstrapLegacy`。后者保留独立的现代模式拒绝检查，避免直接调用时误入旧 router/worker。现代模式不因缺少 legacy opener/server 被拒绝，legacy 也不因缺少现代 runner/notifier 被拒绝。

信号订阅保留至执行器完成 drain/close。同一退出过程中的再次中断合并为同一次 context 取消，不额外启动清理、不恢复默认退出行为来提前切断 MQ。服务回调仍须遵守执行器期限；强制终止、不可中断回调不由本项保证。

## 错误与平台边界

入口原样返回执行器错误，不能把所有 `errors.Is(err, context.Canceled)` 当作成功：启动期间取消是失败；成功 handoff 后取消且清理成功由执行器返回 nil；退出中独立清理失败可能与取消错误共同存在。`main` 保持非 nil 错误打印到 stderr 并退出 1 的行为。

Unix 下 SIGINT/SIGTERM 请求退出。Windows 的 Ctrl+C/Ctrl+Break 经 Go 转为 os.Interrupt；控制台关闭、注销和关机可能产生 SIGTERM，但操作系统仍可能终止进程，不能承诺完整 drain 预算。平台行为以固定 Go 1.27.1 本地 `src/os/signal/doc.go` / `signal.go` 为依据。本轮没有向测试宿主或其他进程发送信号。

P3.5.3.3.3.2.1 已提供[独立 HTTP/WebSocket 生命周期适配器](legacy-http-lifecycle.md)，显式持有并等待全部处理器，包括被替换的旧连接；它尚未替换默认 Gin.Run。P3.5.3.3.3.2.2.1 的[cron 生命周期](legacy-worker-lifecycle.md)已由默认 worker 工厂复用。当前仅现代分支订阅信号，legacy 保持原操作系统退出行为。下一小点 P3.5.3.3.3.2.2.2 完成 HTTP/worker/健康探测/存储的统一退出装配，再接入信号；不能在 HTTP/任务尚未 join 时提前释放存储。

## 验证

```powershell
go test -mod=readonly -race . -run 'Test(RunProcess|Bootstrap)' -count=1 -v -timeout=90s
go test -mod=readonly -race . -run '^TestRunProcess' -count=20 -timeout=60s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
```

默认测试注入配置/信号订阅/执行器，覆盖三现代模式精确转交、缺失及部分角色、非法模式与新旧 worker 冲突、MQ 参数拒绝、失效依赖/信号 handle 清理、取消后的等待和错误保留。实际生产 `RunRuntime` 另在完整工厂配置、预取消 context 下执行，证明启动取消不会连接 MQ 或构造角色。legacy 用受控 SQL driver 和禁止联网的 Redis client，验证配置只加载一次、worker 退出先于存储关闭、全局恢复。

信号取消测试使用可控 context，不是实际操作系统中断实测；运行时内部阶段与真实 MQ 的既有验证见 [执行器](runtime-execution.md)，本轮不重复 Docker，也不将旧结果计为本轮实测。测试不读取开发 .env/config、不运行实际业务主程序、不修改共享数据。

本项完成的是入口接线和现代信号所有权。默认仍只有 legacy 可运行；legacy 优雅退出、生产业务工厂、真实业务 COMMIT/ACK、consumer 恢复、readiness、HTTP 202、审计和 Outbox 继续按后续小点实现。
