# Legacy 统一生命周期与进程退出（P3.5.3.3.3.2.2.2）

默认入口现在是 `main → runProcess → runLegacyRuntime`。配置只加载一次，纯检查所选依赖后注册 os.Interrupt/SIGTERM，再构造真实 HTTP、可选 worker、健康探测和存储。HTTP 使用已实现的 legacyHTTPServer；默认入口不再调用 Gin.Run。旧 bootstrap/serve 回调保留兼容用途，默认进程不会向它们降级。

## 启动与回滚

1. 检查模式、期限、HTTP/listener/health 和存储工厂；仅在 LEGACY_WORKER.enabled 时要求 worker 工厂及参数。缺失现代业务角色、非法模式/冲突仍在信号注册和连接之前失败。
2. 在共同 startup context 下创建惰性 MySQL pool、打开 Redis，成功后绑定旧全局，启动健康探测并构造 HTTP handler。
3. 显式创建 listener，再注册可选 worker。只有全部成功且最后一次 setup 检查通过，才启动 Serve。listener 已绑定不等于业务 HTTP 已启动。
4. setup 取消不再控制成功交接后的资源。进程取消进入下方退出流程，不能先关闭 Redis/取消在途 worker 的事务 context。

工厂失败时同一 owner 回滚已返回的资源，包括 pool/client/HTTP/listener/worker 的 handle+error。未交付的对象不由 owner 清理；未交给 Serve 的闲置 listener 单独关闭。nil/typed nil 不充当有效 handle。失败返回 handle 时调用方仍须等待其 cleanup；默认 runner 已处理这一责任。

MySQL 仍是惰性构造，离线不阻止存活接口启动；Redis 注册/启动失败仍致命。不自动迁移 schema，不因 TCP 连接成功宣称存储/接受就绪，`/health/ready/accept` 保持原 503。

## 停止与共同预算

正常取消、显式 Close、启动失败或 HTTP accept loop 意外退出都由一个 lifecycle owner 执行清理：

1. 同步 StopAdmission：HTTP 先停止新请求/取消 WebSocket，worker 再停止 callback 准入；全部停止后才执行任何 drain。
2. 在一个独立 Background/DrainTimeout 下并发 Drain HTTP 和 worker。普通已准入 HTTP 与当前 worker 保留执行机会；HTTP 包括已替换但仍清理中的旧 WebSocket。
3. 在一个新的共同 CloseTimeout 下并发 Close HTTP、Close worker、StopContext 健康探测，并等待 accept loop。即使 observer 过期，组件也请求取消/中断；worker 自己仍须等待全部 callback 后才能比较释放注册。
4. 核对四种真实完成信号：HTTP RequestsDone、Serve 返回、worker Done、health Done。全部完成后读取 worker 的最终释放结果，恢复旧全局，依次关闭 Redis/MySQL。

| Go 装配参数 | 默认 | 范围/意义 |
| --- | --- | --- |
| StartupTimeout | 30s | 正值 ≤60s；全部启动工作共用，失败后的回滚另有预算 |
| DrainTimeout | 10s | 正值 ≤60s；所有 drain 共用，不按参与者重复分配 |
| CloseTimeout | 10s | 正值 ≤60s；服务/健康探测/accept-loop 关闭等待共用 |
| Address | `:8080` 或 `:` + PORT | 保留 Gin.Run 原有 OS 环境变量语义；不新增配置键 |
| Worker | 原默认参数 | 默认每 60s、job context 30s、一次注册释放 5s |

信号订阅保持到有界退出结果返回，重复退出请求合并。正常激活后取消且清理成功返回 nil；启动取消、服务意外退出（即使 Serve 返回 nil）、清理失败均返回错误，main 按原约定非零退出。原始错误可含凭证，Error/fmt 仅输出固定阶段，errors.Is 保留各个独立原因。

## 期限耗尽与资源保留

`startLegacyLifecycle` 返回 owner。Result 观察有界退出尝试，Wait/Done 观察实际最终清理；Close 请求停止并等待，短 observer 不夺走 owner 的责任。nil context 拒绝。

若关闭阶段结束仍有任务未 join，Result 返回 errLegacyRuntimePending 和期限等原因。同一 manager 继续持有旧全局、Redis/MySQL，等待全部完成信号；随后收集可能迟到的 worker 释放失败，真正关闭存储再发布 Done。已发布的 Result 不被改写，Wait 保留历史超时并提供最终清理错误。不能因 Close/Result 返回超时而先关池，也不能在 Done 之前并发启动另一个 legacy 实例。

默认 main 得到 pending 错误后会按既有逻辑退出 1，进程终止可能留下 worker 注册，按[旧 worker 恢复步骤](legacy-worker-recovery.md)处理。嵌入调用者可保留 handle，解除阻塞后用 Wait join。未强行终止仍在执行的 goroutine，也未通过无条件删除锁来伪装优雅退出。

生命周期 callback 必须响应传入 context，listener/socket Close 遵守 Go net/http 约定。存储 Close 只在任务确认退出后同步执行，不能给任意不响应的自定义 callback/closer 承诺硬上限。Windows 控制台关闭/注销/关机仍可能被 OS 提前终止；本轮测试没有向宿主或其他进程发送实际 OS 信号。

## 验证与后续

```powershell
go test -mod=readonly -race . ./health -run 'Test(LegacyRuntime|RunProcessLegacy|StorageStop)' -count=30 -timeout=60s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration . -run '^TestLegacyRuntimeIntegration$' -count=1 -v -timeout=5m
```

默认测试验证全部准入先于 drain、共享阶段 context、worker 可选、逐阶段部分返回回滚、无效输入/缺失角色零 I/O/零信号注册、启动工作期限、意外 accept-loop 退出、多个独立错误与脱敏，以及 pending 后保留 globals/pool、真实结束后再清理。健康探测的过期 StopContext 先取消，保留正在退出的 probe，之后可再次等待。

真实 suite 使用 NewLegacyFixture 的独立 Docker MySQL/Redis、默认 Gin 路由和真实 JWT/WebSocket/Redis 订阅。通过注入 NotifyContext 的取消验证正常 drain/关闭；强制超时在真实 DB COMMIT 后保留 job 屏障，Result 返回时存储与注册仍在，job 结束后完成清理，原始记录由新 owner 幂等重放。另验证已存在注册的启动失败回滚和 MySQL 离线仍提供 live/存储未就绪。

测试不读开发 .env/config、不运行业务主程序，不访问共享数据；只操作本轮 fixture 并核验标签后清理。信号取消为可控 context，未实测 OS 信号投递。具体 runId/时长见实施记录。本轮不将 legacy DB/Redis 测试计为 MQ 提交后 ACK 或 accepted 验收。

P3.5.3.3 的基础装配与入口范围收口；现代生产角色工厂仍为空，api/worker/all 继续拒绝。P3.5.4.1 [publisher 周期拓扑复核](mq-topology-monitor.md)已完成，下一项为接受就绪状态汇总及运行时/健康接口接线。HTTP 202、业务消费者、审计、Outbox 和端到端恢复仍按后续阶段实现，P3 整体尚未验收。
