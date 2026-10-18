# Legacy HTTP/WebSocket 请求生命周期（P3.5.3.3.3.2.1）

`legacy_http.go` 提供显式的 `legacyHTTPServer`，统一持有普通 HTTP 和已 hijack 的 WebSocket 处理器。它按请求记录所有权，包括已被同用户新连接替换、仍在退出中的旧连接；不依赖只保留当前会话的全局用户表。

本项提供可调用适配器及真实本机 HTTP/WebSocket 验证。后续 P3.5.3.3.3.2.2.2 已将它接入[默认 legacy 生命周期](legacy-runtime-lifecycle.md)，替代默认入口的 Gin.Run 并接收退出信号。

## 所有权和阶段

| 方法 | 行为 |
| --- | --- |
| `newLegacyHTTPServer(handler)` | 纯构造，不监听、不启动任务；nil handler 拒绝，不落入 DefaultServeMux |
| `Serve(listener)` | 使用调用方显式交付的 listener；http.Server 持有其关闭责任。返回只说明 accept loop 已结束，不说明全部处理器已经退出 |
| `StopAdmission()` | 同步停止业务准入，新请求得到 503 / Cache-Control: no-store；取消所有已 hijack 请求的 context，不等待处理器。普通在途 HTTP 保留执行机会 |
| `Drain(ctx)` | 停止准入，使用同一个调用方预算执行 http.Server.Shutdown 和显式请求等待；必须等全部处理器返回，包括 WebSocket 的订阅与 reader/writer/心跳任务清理 |
| `Close(ctx)` | 停止准入、取消全部请求、关闭持有的 hijacked socket 并调用 http.Server.Close，再等待全部处理器；expired observer 也先请求停止。超时后保留请求清单，可用新 context 再次等待 |

调用方最终必须同时 join Serve 和 Drain/Close。Drain 超时可以进入独立 close 阶段，但 Close 超时不能解释为任务已经结束，更不能立即释放已准入处理器仍在使用的共享 Redis/MySQL。适配器不持有存储、不恢复旧全局、不注销 legacy worker，也不自行选择退出预算；下一小点的整体 owner 必须负责这些动作及失败路径。交付的 listener/socket 须遵守 Go net/http 的关闭约定，Close 及时返回；适配器不通过丢弃 goroutine 来隐藏不遵守约定的自定义 transport。

处理器须遵守请求 context，并在返回前等待自己的全部业务任务。既有 `service.Chat` 在取消后关闭 socket/订阅，等待四个任务结束，最后处理 Offline，符合此边界。适配器在处理器完整返回后才移除记录、释放请求 context，并关闭仍持有的 hijacked socket；不支持处理器返回后把裸连接交给未托管的后台工作。

## Hijack 与 writer 兼容

HTTP ResponseWriter 保留 Gin/Gorilla 所需的 Hijacker、Flusher、Pusher、Unwrap；FlushError 保留 ResponseController 的失败结果。CloseNotifier 仅在底层实际支持时透传，兼容 Gin 的 Stream，不为不支持的 recorder 创建虚假能力或额外等待 goroutine。

Hijack 前检查准入；底层 Hijack 返回后在同一所有权锁下再次检查。若停止与底层调用竞争，新获得的 socket 会被关闭，请求被取消并返回停止错误。已经登记的连接由停止快照取消；后续关闭和处理器返回仍须 join。底层失败但返回 handle、nil connection/buffer 的坏结果同样不会交给业务处理器，已交付 handle 被关闭。

取消、socket Close 和处理器等待均在登记锁之外执行；锁只保护准入、活动请求和完成通知。没有每请求 watcher、复制的用户会话索引或丢弃任务的超时 goroutine。活动清单随在途请求数量增长，不在本项引入新的并发容量策略。

errLegacyHTTPShutdown 标记退出错误；errors.Is 同时保留 net/http 和取消/期限等独立原因。Error/fmt 只输出固定消息，调用方不应直接打印解包后的底层 cause。Serve 的原始错误交由未来整体 owner 分类；正常 Shutdown 导致的 http.ErrServerClosed 不代表业务任务退出失败。

## 验证

```powershell
go test -mod=readonly -race . -run '^TestLegacyHTTP' -count=1 -v -timeout=45s
go test -mod=readonly -race . -run '^TestLegacyHTTP' -count=30 -timeout=60s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
```

7 组专项覆盖普通请求 drain 与新请求拒绝、expired Close 仍取消并中断 I/O、未完成处理器的超时及随后 8 个并发 Close 等待、3 条真实 WebSocket 的当前/替换/其他用户会话、Hijack 停止竞争、部分返回清理、可选 writer 能力、nil 输入、空闲/重复关闭和拒绝 Serve 时的 listener 所有权。

WebSocket 使用实际 `service.Chat` 和 Gin/Gorilla，经独占 loopback listener 握手并收到真实推送帧。fixture 订阅不访问 Redis，reader 日志屏障故意保留旧会话：即使当前连接和所有订阅已关闭，Drain/Close 仍报告未 join；释放 reader 后再次 Close 成功，当前用户各执行一次 Offline，旧会话不能清掉替换会话状态。

测试只使用本机独占临时端口、net.Pipe 和受控订阅，不读取开发 .env/config、不运行业务主程序、不访问共享数据库，也不使用 Docker。OS 信号投递、实际 Redis Pub/Sub/DB 在途事务、cron/注册锁释放和完整服务器信号退出不属于本项实测。

后续 [legacy cron 生命周期](legacy-worker-lifecycle.md)与[默认 legacy 统一生命周期](legacy-runtime-lifecycle.md)已完成。现代[接受 readiness](runtime-readiness.md)、[持续拓扑核验](mq-topology-monitor.md)与[生产 API](api-runtime-lifecycle.md)也已接入；后台业务角色仍缺失，阶段边界见 [P3 验收对照](mq-reliability-acceptance.md)。
