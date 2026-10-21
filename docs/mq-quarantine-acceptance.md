# 可靠隔离转移故障验收（P4.2.3.4）

本项在 [隔离信封](mq-quarantine-envelope.md)、[独立目标 publisher](mq-quarantine-publisher.md)和[消费处置](mq-persistence-disposition.md)之上，验收生产 `NewSupervisedPersistenceFactory → RunRuntime` 的源连接、目标发布、ACK 和退出交叉责任。默认后台角色门禁保留；测试中的隔离审计/Outbox companion 是明确占位，不能用于生产装配。

## 验收范围

| 部分 | 已有证据 | 本项补充 |
| --- | --- | --- |
| 原文与身份 | 带类型 headers、坏字节、严格编码、稳定 transport identity | 实际重投及自动 DLX 后仍保持同一传输身份 |
| 目标发布 | 独立 session、mandatory/persistent、confirm/return、NACK/unknown | 目标失败经过运行时退出仍保留原原因和源投递 |
| 原投递处置 | 目标确认前零 ACK、白名单/混合树否决、原 handle 单条 ACK | 源连接退役与真实 ACK 丢帧后不在新 session 复用旧 handle |
| 所有权 | 服务 drain/强制 join | 运行时 observer 返回 pending 后，目标调用、监督器和外部 SQL/目标 owner 继续持有责任 |

`tests/testenv` 的原监督运行时夹具只增加可选借入 publisher，默认无参数调用保持原语义。没有新增生产 failpoint、默认角色、配置或迁移。实际结果与 runId 见[进度记录](message-reliability-mq-progress.md)。

## 退出与资源边界

| 时点 | 必须成立 |
| --- | --- |
| 目标调用已准入、尚未返回 | 旧源组不能被当成已 join，不得获取下一轮；外部目标 owner 不得关闭仍被借用的 publisher |
| StopAdmission / 正常 drain | 停止领取，保留已准入处理 context，让目标确认及原 ACK 完成 |
| drain/close 观察期限到期 | RunRuntime 可以返回 pending；仍未返回的回调、监督器和源清理任务不能被丢弃 |
| 目标迟到确认、处理 context 已取消 | 保留目标副本，不再尝试源 ACK，不复活就绪/下一轮 |
| 服务与监督器真正 Done | 外部 owner 才可最终关闭目标 publisher 和共享 SQL pool |

Runtime 的 MQ 资源关闭不包含显式借入的隔离 publisher。即使目标 session 原生调用已确认成功，包装该能力的回调尚未返回时，借用仍然有效；不能仅依据原生 session 的当前空闲状态提前释放资源。

目标故障的 QuarantineFailed 与源 ACK 阶段错误仍保留为失败。SQL 健康不能证明目标恢复，纯取消不能掩盖已准入发布/ACK 责任。目标失败停止当前角色，由外部 owner 显式修复并恢复；没有立即 requeue、自动重发或热循环。

## 默认验收

`TestPersistenceQuarantineOwnership` 运行实际监督器、服务和 processor，SQL/流/ACK 边缘使用本地可控夹具，覆盖：

- 正常 drain 保留 context，目标返回后完成一次 ACK，旧组关闭一次，没有额外 generation。
- 强制 Close 返回 pending 后，取消但尚未返回的目标回调仍活跃；释放后不 ACK，真正 join 才结束所有权。
- 源/存储故障与在途目标调用同时发生时，旧组未 join 不获取下一轮；随后原 ACK 错误否决 SQL 恢复，即使同树还有 EOF。
- QuarantineFailed 包含取消、超时或 EOF 时，始终保留目标阶段，不被识别为正常退出或数据库恢复证据。

默认测试不连接外部服务；这里不把本地夹具称作真实 RunRuntime 网络验收。

## 实际 SQL/RabbitMQ 九项

`TestQuarantineRuntimeIntegration` 使用独立 schema/vhost 和真实目标 session。除最后的 broker 自动 DLX 对照外，源消费均经过生产显式工厂与 RunRuntime。

| 场景 | 实际注入与证据 | 恢复责任 |
| --- | --- | --- |
| 已替换源组的正常退出 | 先到 Generation 2，实际目标确认后阻塞借入回调；停止后短 drain 为 pending，work context 仍有效；释放后正常 ACK/退出 | 目标与共享 SQL 均由外部 owner 在 join 后释放 |
| pending 与迟到确认 | 同样使用第二轮源组，runtime drain/close 各 150ms；RunRuntime 返回时监督器 Done 未关闭、目标调用仍活跃、目标 Done 未关闭且 pool 可 Ping | 释放后不再源 ACK，外部关闭目标/pool；原 attempt 重投，再隔离保持稳定身份 |
| 目标 confirm 丢帧 | 专属代理截获并丢弃真实 broker confirm，目标 operation 到期；运行时报告角色失败并退出 | 目标副本和原源投递均保留，显式恢复产生同身份/新 attempt 副本 |
| mandatory Return | 运行时激活后移除隔离 binding；Return 经运行时/清理包装仍可 errors.Is 检查 | 无目标副本，源未 ACK；修复 binding 后显式恢复 |
| 容量 NACK | 仅本轮 vhost 的临时 operator policy；独立填充发布者先观察真实 NACK，再投递源命令 | 运行时保留源，之前确认的积压不丢；移除策略后显式恢复 |
| 目标已确认后源连接退出 | 保留目标回调，关闭当前源 session；观察旧组 retiring 且 Generation 仍为 1；释放后 ACK 失败，运行时退出 | 不创建新 generation 处理旧 ACK，目标副本与原源 attempt 均可核对 |
| 第二轮真实源 ACK 丢帧 | 源连接替换后，专属代理丢弃源 ACK，本地 ACK 返回成功；运行时正常退出 | 新 session 收到原 redelivered attempt，再隔离保持稳定身份，不能把本地 ACK 成功当成 broker 已确认 |
| 空闲目标连接断开 | 仅关闭目标专属网络代理，等待目标停止准入，再发送源输入 | not_published/unavailable 经角色失败保留，不被当作数据库恢复而反复消费 |
| 自动 DLX 原文对照 | fixture 从主队列 Reject(false) 触发真实 broker DLX，观察原始 body、x-death 与路由改变 | 原始 DLX delivery 与主动 wrapper 不混淆；转换成审计快照后，transport ID 与初始命令流输入一致 |

目标确认后的返回屏障只存在于测试 wrapper：它发生在真实 broker 确认之后，不冒充网络确认丢失。confirm/ACK 丢帧另由实际 loopback 代理执行。容量测试不假设策略生效后第一个发布一定 NACK，而以前置的实际负确认作为故障就绪证据。

每项关闭后直接检查主队列/隔离队列为空，连接恢复到两个 fixture 控制/publisher owner；仅操作本轮归属的策略/绑定/连接。目标队列的 Get/ACK 是 fixture 的检查与清理，不是审计事务。自动 DLX 的 Reject 也是测试故障控制，不代表应用主动隔离使用 Reject。最终确认本轮坏输入没有创建 message、message_command、Outbox 或 SQL quarantine 行，并复核正式拓扑。

## 命令

使用固定 Go 1.27.1、仓库 `.gocache` 和不存在的 `ICHAT_CONFIG_FILE`，不读开发 .env/config：

```powershell
go test -mod=readonly -race ./workers -run '^TestPersistenceQuarantine' -count=30 -timeout=90s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestQuarantineRuntimeIntegration$' -count=1 -v -timeout=12m
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistence(Connections|Shutdown)Integration$' -count=1 -v -timeout=12m
```

原连接替换/退出 suite 回归用于验证可选 publisher 的夹具扩展保持原默认路径；不能把 integration 标签编译当成执行了全部集成 suite。

## 后续边界

P4.2.3 的范围是保守白名单下从主 MQ 到隔离 MQ 的可靠责任交接，不等于完整 P4 或端到端完成。[P4.2.4.1 输入契约](mq-quarantine-input.md)已完成主动 wrapper/自动 DLX 解析及身份校验，下一项 P4.2.4.2 幂等保存完整审计；SQL 提交后才 ACK、worker 生命周期和受控 CLI 重放仍待后续实现。

本项未运行生产默认 worker/all、独立隔离进程强杀、宿主 OS 信号、三节点 quorum 故障、磁盘损坏、目标数据被管理员删除或完整实际客户端。明确数据约束以外的 schema/未知技术失败仍按 P4.2.3.3 保守停止；未扩大自动隔离范围。审计、重放、Outbox 和完整运行时角色的交叉验收继续属于后续阶段。
