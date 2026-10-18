# Legacy cron 任务生命周期与注册释放（P3.5.3.3.3.2.2.1）

`startLegacyWorker(setup, store, redis, options)` 在原有注册协议之上提供 `legacyCronWorker`：先纯检查参数/排程，再取得无 TTL 的 Redis token，启动单个 cron，最后由一个 manager 持有停止、任务等待和释放责任。[默认 legacy 统一生命周期](legacy-runtime-lifecycle.md)已使用此 owner，协调 HTTP、健康探测、存储与退出信号。

## 阶段与所有权

| 方法 | 行为 |
| --- | --- |
| `StopAdmission()` | 同步关闭 callback 准入；不做 Redis RPC、不等待任务。已经排入 cron 的 callback 也须经过相同 gate |
| `Drain(ctx)` | 停止准入，等待 cron.Stop 报告所有 callback 返回；保留当前 job context，不释放 token |
| `Close(ctx)` | 停止准入、取消 job lifetime、请求最终释放并等待；过期 observer 仍发出停止请求，随后返回观察期限错误 |
| `Wait(ctx)` / `Done()` | 观察唯一 manager 的最终结果，不自行请求停止。Done 在所有 job 返回且一次 Release 调用完成后关闭 |

manager 只调用一次 cron.Stop。即使 Close observer 已取消，也继续等待 cron 的全部任务返回，再用独立的 Background/ReleaseTimeout 调用比较 token 的 Release。每个 Wait/Close 读取同一不可变结果；释放失败或响应不确定不自动重试，不因随后再次 Close 将错误擦掉。

Done 表示任务已 join、释放尝试已结束，不表示锁已成功删除。释放失败保留 errors.Is 原因及人工恢复责任；旧 token 不得删除新持有者的注册。[原恢复步骤](legacy-worker-recovery.md)及 Redis 故障边界继续适用，没有 TTL、自动抢锁或数据库 fencing。

setup 只控制注册和交接，成功交接后取消不结束任务 lifetime。若已成功取得 token，但交接前 setup 已取消，返回 handle+startup error，并自动请求关闭；调用方仍须等待该 handle 的清理结果，再释放其依赖。注册返回失败（包括 SET 响应未知）仍没有 handle，不执行补偿 DEL 或自动重试取得所有权。

## 参数与执行

| Go 装配参数 | 默认 | 边界 |
| --- | --- | --- |
| Schedule | `@every 60s` | 使用固定 cron v3.0.0 解析；不新增 YAML/env |
| JobTimeout | 30s | 正值且 ≤60s，每次 SyncMsg 的工作 context 上限 |
| ReleaseTimeout | 5s | 正值且 ≤60s，任务全部返回后的一次注册释放工作上限 |
| setup | 调用方提供 | 默认 legacy 工厂沿用原 5s 注册 context |

每次 callback 使用独立 job deadline，调用既有 OwnedMessages/SyncMsg；私有 running gate 跳过重叠 callback，defer 在失败或 panic 后归还 gate。panic 和依赖错误只记录固定类别，errors.Is 在受控报告/退出 API 中保留原因，不回显凭证或 payload。job 失败继续走原日志/后续排程策略，不变成锁释放成功或已处理消息的证明。

30s 是有界工作参数，不是已测出的生产容量。超时可能留下已提交但未确认的前缀；不能据此认定事务回滚。原流程只确认已持久化前缀，取消后的确认失败/未知由后续 owner 幂等重放。所有权检查/确认失败仍可能永久暂停该 owner 的消费，按恢复文档处理，不因网络恢复自动继续。

原默认工厂返回的 stop 函数先用 30s 预算 Drain，再 Close 并等待 manager 真正完成，退出错误仍按旧契约记录日志；显式接口为后续整体 owner 提供可返回的退出结果。stop 不关闭共享 Redis/MySQL。存储/日志 callback 必须及时响应 context；若不遵守，兼容 stop 仍保持等待，不能声称整个进程一定在 35s 内退出。显式 Close 超时也不允许上层释放仍被任务使用的存储。

## 验证

```powershell
go test -mod=readonly -race . -run '^TestLegacyWorker' -count=1 -v -timeout=45s
go test -mod=readonly -race . -run '^TestLegacyWorker' -count=50 -timeout=60s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration . -run '^TestLegacyWorkerLifecycleIntegration$' -count=1 -v -timeout=5m
```

7 组默认测试用真实 cron 的短排程与受控 store/owner，覆盖停止后 callback 拒绝、Drain 不取消 job/不释放锁、过期 Close 仍取消但必须等待、提交/确认/释放顺序、job deadline、panic 后恢复与非重叠、8 个并发 Close、独立 release deadline/错误保持、setup lifetime 分离、交接回滚和零 I/O 参数检查。没有手动调用正在执行的 callback 来假装 cron 已经 join。

真实 suite 使用 NewLegacyFixture 的独立 MySQL/Redis：graceful 在实际 DB 提交后的屏障暂停，Drain 后 token 仍 Busy，Close 才释放；forced 在相同窗口取消，job 返回前 token 仍 Busy，之后原始队列内容保留，下一 owner 重放只保留一行。另验证旧 owner 释放失败不删除 fixture successor、不重复 Release，以及默认工厂重复 stop 不关闭共享存储。重放过程中 MySQL duplicate-key 日志是既有幂等校验路径，测试会核对最终行数和队列。

默认测试不读开发 .env/config、不访问真实 DB/Redis。实际场景只触碰本轮 fixture，归属标签核验后清理；没有启动业务主程序或发送 OS 信号，也不把本轮 legacy 提交/确认测试当作 MQ/P4 验收。版本、runId、命令结果见实施记录。

P3.5.3.3.3.2.2.2 [统一生命周期与信号接入](legacy-runtime-lifecycle.md)已完成。先停止各方准入，保留 drain 机会，确认 HTTP 和 job 全部 join 后才恢复旧全局、关闭存储。P3.5.4.1 [publisher 周期拓扑复核](mq-topology-monitor.md)已完成，P3.5.4.2 [接受就绪汇总与健康接线](runtime-readiness.md)也已完成装配基础；下一项为 P3.6 HTTP 202 接受入口，P3 尚未整体验收。
