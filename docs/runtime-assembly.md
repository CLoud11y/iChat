# 角色构造器与服务实例检查（P3.5.3.3.2.1）

P5.1.4 已加入[Outbox 生产工厂与生命周期](outbox-lifecycle.md)：默认 worker/all 选择惰性真实 Outbox 工厂，显式工厂优先；默认 Persistence/Quarantine 仍需外部注册，缺失门禁保持。健康新增独立 Outbox 能力与 `/health/ready/outbox`，资源 owner 等真实 Done 后最终释放。以下历史阶段的装配/夹具范围按原验收理解。

后续 [P4.1.2](mq-persistence-lifecycle.md)提供单轮 `NewPersistenceFactory(sharedPool, options)`；[P4.2.2.2](mq-persistence-connections.md)另提供显式 NewSupervisedPersistenceFactory，纯本地 claim 仅传给持久化角色的 PersistenceConsumers 来源，Start 后才获取/核验连接。共享 SQL 由调用方持有至服务 Done，默认进程未注册后台工厂；本页缺失角色检查及 worker/all 门禁继续适用，不能用测试隔离/Outbox 占位服务补齐生产装配。

`application.RuntimeFactories.Check(conf)` 从 Config 重新解析角色，检查所选业务服务的实际构造函数是否存在。它不调用构造函数、不创建 MQ/数据库连接、不检查依赖在线状态。缺少任一必需角色时返回空 RuntimeAssembly 和全部缺失角色，不允许使用半份装配结果。

## 两次检查

| 检查 | 时机 | 验证范围 |
| --- | --- | --- |
| RuntimeFactories.Check | 任何外部依赖构造之前 | mode/新旧 worker 冲突、全部必需服务构造器存在 |
| RuntimeAssembly.ValidateServices | 全部选中服务构造完成、任何服务启动之前 | 服务集合精确匹配选中角色，实例非 nil/typed nil，无额外角色 |

第一层按已验证模式选择以下构造器：

| 模式 | API | Persistence | Quarantine | Outbox |
| --- | --- | --- | --- | --- |
| legacy | 不要求 | 不要求 | 不要求 | 不要求 |
| api | 必需 | 不要求 | 不要求 | 不要求 |
| worker | 不要求 | 必需 | 必需 | 必需 |
| all | 必需 | 必需 | 必需 | 必需 |

legacy 沿用旧 API/可选 Redis worker 的现有 bootstrap，不把它们当作新业务角色。直接零值 Config 仍按 legacy 解释。未知模式和新模式混用旧 worker 先报 ErrRuntimeConfig，不让缺失角色掩盖配置错误。

Publisher 是 MQ 技术依赖，不能代替 API 业务服务；存在 ConsumerSession 也不能代替持久化/隔离审计处理器。MQ 参数与实际连接另由 Config.MQRuntime / PrepareMQ / StartMQLifecycle 校验。Check 不读取 MQ URI 或探测 schema，合法的角色配置也不代表依赖可用或业务已实现。

RuntimeAssembly 保留选中函数值的私有副本，Roles 返回列表副本。更改来源 RuntimeFactories 的字段或 Roles 返回值不能改变装配结果。未选中的构造器不会被保留或执行；默认零值 RuntimeAssembly 未经检查，不能通过 ValidateServices。

ValidateServices 不调用任何生命周期方法，包括 Done/Err，也不更改传入 map 或关闭其中的 handle。它的成功只描述检查时的集合；调用方不得并发改 map，后续执行器须使用同一受控服务集合。若检查失败，调用方仍负责回滚全部已返回服务及依赖，不能丢弃 handle；部分返回和回滚的编排在下一小点实现。

## 服务生命周期契约

RuntimeFactory 的输入为准备 context 与 RoleDependencies，返回 RuntimeService 和错误。构造器只能准备闲置服务，不得提前运行 HTTP、订阅消费或领取任务。其他存储/鉴权依赖由明确的构造器闭包注入，不从旧全局 manager 读取。

RuntimeService 的方法定义了后续执行器的调用边界：

| 方法 | 契约 |
| --- | --- |
| Start(setup, lifetime) | 在 setup 期限内激活，成功后使用独立 lifetime |
| StopAdmission() | 同步停止新接单/领取，不等待在途任务或进行 RPC |
| Drain(ctx) | 在 ctx 内等待已接任务完成，包括提交和应有的 ACK |
| Close(ctx) | 取消剩余任务、限时 join 自己持有的工作，不关闭共享 MQ 组或共享存储池 |
| Done() / Err() | 退出通知；Done 关闭后读取退出结果 |

RoleDependencies 承载本角色的 MQ 能力以及只读 Health 观察。执行器仅给 API 命令 publisher，仅给消费者对应固定队列的连接列表，当前 Outbox 没有 MQ 输入。Health 的激活/退出门禁及显式业务就绪能力见[运行时就绪状态](runtime-readiness.md)。此处定义契约，没有提供这些业务服务实现，也没有执行 Start/停止/Drain/Close。接口和非空实例不能证明处理器遵守业务语义，仍须在 HTTP 202、提交后 ACK、审计及 Outbox 实现中验证。

## 启动诊断

缺失构造器使用 `application.ErrRuntimeNotAssembled`。原主包 ErrRuntimeNotAssembled 保留为同一 sentinel，现有 errors.Is 调用继续有效。可以使用 errors.As 读取 RuntimeAssemblyError.Mode/MissingRoles；缺失列表按 API、持久化、隔离、Outbox 稳定排序，返回副本。

兼容 bootstrap 仍使用空的新业务构造器集合并拒绝现代模式；默认 runProcess 已装配生产 API，worker/all 仍在任何外部构造器前拒绝，例：

```text
resolve runtime assembly: requested runtime mode is not assembled yet: worker (missing roles: persistence_consumer, quarantine_consumer, outbox)
```

服务实例问题使用 ErrRuntimeServices。未知 map key 不回显，所有错误只包含已验证模式、已知角色名或固定原因，不输出凭证、消息正文、URL 或非法模式原值。第二层检查不接受未选中的额外服务，即使其值为 nil。

默认程序已为显式 api 模式注册[生产 API 构造器](api-runtime-lifecycle.md)；[进程入口](runtime-process-entry.md)提供到 [RunRuntime 执行器](runtime-execution.md)的现代分流和信号接线。legacy bootstrap 的新模式兜底拒绝继续保留，避免直接调用时落入旧 router/worker。Check 成功不是 ready 的证明；迁移 CLI/mqadmin 不新增业务装配或 MQ 网络依赖。

## 验证与下一项

```powershell
go test -mod=readonly -race ./application . -run 'Test(RuntimeAssembly|BootstrapRuntime)' -count=1 -v
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
```

测试覆盖模式选择、只要求选中构造器、逐项/全部缺失、源函数/角色列表副本、配置及全局不变、先验证模式冲突、完整/缺失/nil/typed nil/多余服务、空装配拒绝、原清理 handle 不变及凭证脱敏。测试构造函数和六个生命周期方法全部设为 panic，证明两层检查不执行它们；两个专有本机 listener 验证 AMQP/management 地址没有连接。

实际执行 bootstrap 的测试验证结构化缺失角色、sentinel 兼容，且只出现配置加载事件，旧全局状态不变。默认 legacy 生命周期与全仓库回归继续执行。测试不访问 Docker、开发配置/.env 或共享数据；没有更改 AMQP 库或重复旧 Docker 结果。

P3.5.3.3.2.2 [受控服务构造、激活与分阶段退出编排](runtime-execution.md) 已在此检查层之上实现；它持有自己的服务集合，失败时回滚，不在 Check/ValidateServices 中执行生命周期方法。[现代入口与信号](runtime-process-entry.md)和 [legacy 统一生命周期与信号](legacy-runtime-lifecycle.md)均已接入；[生产 API](api-runtime-lifecycle.md) 已为显式 api 模式装配，后台业务角色仍缺失。P3.5.4.1 [publisher 周期拓扑复核](mq-topology-monitor.md)已完成，P3.5.4.2 [接受就绪汇总与健康接线](runtime-readiness.md)也已完成装配基础；P3.6 接受入口与 API 生命周期已完成，阶段证据与后续边界见 [P3 验收对照](mq-reliability-acceptance.md)，下一项为 P4 持久化消费适配；后台业务处理器与 consumer 恢复仍待后续阶段。
