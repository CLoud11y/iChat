# P5.1.4：生产角色工厂与生命周期

`workers.OutboxService` 将[通知执行循环](outbox-execution.md)接入 RuntimeService。`application.NewOutboxFactory` 借入明确提供的 SQL/Redis；`NewConfiguredOutboxFactory` 在 Start 时建立角色专用资源；`NewOutboxSourceFactory` 允许显式资源来源，并负责接住部分/迟到返回的所有权。

默认进程已为 worker/all 注册惰性的真实 Outbox 工厂；显式工厂优先。默认 Persistence/Quarantine 工厂仍未注册，所以缺失角色门禁继续在信号注册及外部 I/O 前拒绝默认 worker/all。调用方显式提供两类既有监督工厂时可以通过完整角色清单；本项不代表默认完整后台管线或交叉故障验收已完成。

## 资源与构造

所有工厂与服务构造均无外部 I/O。Outbox 拒绝 Publisher、Consumers、PersistenceConsumers 和 QuarantineConsumers 等其他角色能力，不读取聚合 Health 来建立自身就绪，也不借用全局 DB/RDS。资源来源只在 Start 后调用，必须遵守 setup context；即使返回 handle+error，服务也保留其 Close 回调。

`OutboxResources` 包含 Store、Publisher、ProbeStorage、ProbeNotification 和可选 Close。Close 非空时表示本服务承担最终释放；Close=nil 表示资源借入。正常打印服务仅显示固定名称；依赖错误以受控 stage 展示，Unwrap 保留错误归类。

配置工厂使用私有 MySQL connector 和 Redis client：SQL 使用 UTC、2 个最大连接/1 个空闲连接及有界连接/I/O 超时；Redis 禁止隐式重试与后台空闲拨号，设置有界拨号、连接池和读写超时。不会迁移、修补或清空 schema。存储探针 Ping 后只读检查 Outbox 所需列，通知探针为 Redis PING；此检查不是完整 schema 指纹或权限授权验收，部署仍须独立迁移。

默认 OutboxServiceOptions 显式选择：批次 10、lease 30s、退避 Base=1s/Max=1min、ClaimTimeout=3s、PublishTimeout=2s、SettlementTimeout=3s、PollInterval=1s、ProbeInterval=2s、ProbeTimeout=1s。沿用执行循环全部边界，探针间隔/超时也须在 (0,1min]。这些是库级装配参数，本项不新增 YAML/env 参数。

## 生命周期与 pending

Start 要求有界 setup 和独立 lifetime。资源 owner goroutine 完成来源获取、真实 SQL/Redis 探针后激活，成功后不再依赖 setup；setup 取消不能误停正常服务。若 Start 超时返回时来源或探针仍未真正返回，原 owner 继续接住迟到资源，清理后才关闭 Done。

处理与探针由同一个 owner 顺序执行，不遗弃网络/SQL 回调，不在资源关闭前启动下一轮角色。任一处理/结算或运行期探针错误会撤销就绪并停止本角色，交由 RunRuntime 退出/运维重启；本项采用 fail-stop，没有新增自动监督恢复或续约。发布 unknown 经既有 Retry 成功回退时可继续轮询。

| 操作 | 行为 |
| --- | --- |
| StopAdmission | 同步、无 RPC：撤销就绪，取消 setup、空闲/在途 Claim 与探针，不取消已提交领取批次的处理 lifetime |
| Drain(ctx) | 停止新领取，允许已领取批次继续发布/结算，等待真实回调和本角色资源清理结束 |
| Close(ctx) / lifetime 取消 | 取消处理机会；尚未结算的 lease 保留，由过期重领恢复；继续等待实际回调加入后才释放资源 |
| observer 超时 | 返回 ErrOutboxServicePending + context 错误，不关闭 Done、不释放在途 SQL/Redis，不把 pending 当成已停止 |
| 部分启动失败/迟到资源 | 清理来源返回的资源；Close 回调自身阻塞也继续由原 owner 持有 |
| Done | 所有来源/探针/批次与最终 Close 回调已真实返回；Err 可读取最终结果 |

正常 stop 取消 Claim 的 context，已成功提交并返回的 claim 仍属于该次准入，可完成整批；领取提交未知保持原错误，不因 stop 取消而抹掉不确定性。预算耗尽的批内剩余 lease 按执行循环规则到期恢复。Close 不提前清除 token 或伪造完成。

`RunRuntime` 保持停止全部准入 → 并行 drain → 取消 lifetime → Close 服务/MQ 的顺序。其观察超时返回不等于所有服务 Done。借入共享存储的调用方必须等待全部使用者 Done，才能关闭 pool/client；配置工厂的私有资源由 Outbox owner 自行延后释放，调用方不能在 runtime 返回时重复关闭。返回失败或 pending 均不转移此责任。

## 独立就绪与健康接口

`RuntimeOutboxReadiness.OutboxReady()` 只读本地缓存：来源/依赖已检查、已激活、准入未停止、lifetime 有效，并且探针样本年龄不超过 ProbeInterval+ProbeTimeout。长批次导致样本过期时撤销 ready，下次实际探针成功后更新；不凭“有任务完成”替代探测。

runtime 的 `RuntimeStatus.Outbox` 与 Acceptance/Consumers 独立。缺少能力或 false 返回 OUTBOX_UNAVAILABLE；api/legacy 没有选中 Outbox 时为未配置；部分启动 starting；退出 stopping。publisher/消费者故障不会被解释为 Outbox 自身依赖失败；任一角色实际 Done 导致的整个运行时退出仍按统一策略撤销全部就绪。

新增 `/health/ready/outbox`：ready 为 200，未配置为 503/OUTBOX_NOT_CONFIGURED，其余按固定阶段返回 503，均 no-store。接口无网络 I/O，不改变原有接受/消费者/存储健康语义。worker 模式自身不增加 HTTP listener；这是现有 API/观察适配可暴露的独立状态。

## 验证与边界

```powershell
go test -mod=readonly -race ./workers ./application ./router . -run '^Test(Outbox|RuntimeHealth|RuntimeHealthHTTP|RunProcess)' -count=30 -timeout=120s
go test -mod=readonly -race -tags=integration ./application ./workers -run '^TestOutbox(Lifecycle|Loop)Integration$' -count=1 -v -timeout=8m
```

默认测试覆盖闲置构造/关闭、foreign capability 拒绝、显式工厂优先、Start/setup/lifetime 分离、停止时保留在途处理、强制取消、来源/探针/资源关闭迟到、回调 panic/错误、探针新鲜度、typed nil/非法参数、独立就绪和 HTTP 结果。RunRuntime 的 companion 夹具验证：短 drain/close 返回后，迟到 Claim 仍由真实 Outbox 服务保留资源，回调退出才最终关闭。

真实 MySQL/Redis 六组验证配置工厂、借入资源可继续使用、私有资源关闭、SQL/Redis 启动失败清理、PUBLISH 成功后的正常 drain 和强制 pending。后两组在真实发布后的返回回调设置屏障，释放后分别形成 completed 和保留 leased，资源仅在实际加入后关闭。它们不是 TCP 丢响应或进程强杀；两条直接 Outbox 结构夹具只证明本项生命周期，原通知执行 suite 继续验证生产终态 payload。

本项最终集合：2 Outbox（1 completed、1 leased），其余五张可靠性业务表为 0，schema 指纹不变。[P5.1.5 故障验收](outbox-acceptance.md)已完成，P5.1 收口；下一项 **P5.2：统一 WebSocket 帧与通知权限核验**。P5.1 与显式三角色交叉范围已完成；P5 和默认完整后台进程保持未完成。运行记录和清理证据见[实施进度](message-reliability-mq-progress.md)。
