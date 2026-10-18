# Publisher 周期拓扑复核（P3.5.4.1）

Supervisor 现在持续复核已就绪 publisher 的本地拓扑，空闲连接也会检查。它复用新连接的只读契约：实际 AMQP broker 版本/vhost、management 中的版本/feature flags/队列参数/有效策略/quorum 在线成员/绑定，以及同一 AMQP 连接上的 passive exchange/queue 检查。整个周期不发布探测消息、不声明修复拓扑、不删除队列或修改策略。

## 执行与失败

唯一恢复 owner 在安装 ready session 后启动 timer，成功检查结束后重新计时。检查成功保持同一 epoch 和接单状态，不逐次创建新 publisher，也不暂停健康连接上的普通 Publish。检查与恢复串行，没有堆积的 ticker 任务或独立巡检 goroutine。

检查失败、management 不可达/超时或 passive RPC 不返回都会退休当前 epoch。Status/WaitReady 撤销就绪，之后 Publish 返回 not_published/unavailable；当时尚未得到完整确认的发布仍可能进入 unknown，已经返回的 accepted 不撤销。原始检查错误不会进入公开 Status，只保留 ErrSupervisorTopology 等固定 sentinel。

复核计入当前 connection owner 的活动调用。取消/超时的 watchdog 关闭该连接的 socket，打断 AMQP RPC；旧检查和旧发布未 quiesce 时不会创建替代 epoch。旧调用无法退出仍沿用 failed/ErrSupervisorCleanup。人工恢复拓扑或管理接口后，正常 backoff 重连重新执行完整只读核验，成功才安装新 epoch；没有自动重发命令。

Close/lifetime 取消撤销准入、取消正在运行的复核，并等待同一 owner 返回。晚到的检查成功不能恢复已经退休或关闭的 session。

## 参数与观察边界

| Go 装配参数 | 默认 | 边界 |
| --- | --- | --- |
| SupervisorOptions.TopologyCheckInterval | 10s | 10ms—60s，零值不能关闭复核；从上次完成后计时 |
| Session.OperationTimeout | 原操作期限，默认 5s | 本次 management 和 AMQP 复核共用一个总期限，复用现有校验与配置 |
| RetryMin/RetryMax、CloseTimeout | 原值 | 沿用恢复退避和旧 epoch 清理边界 |

使用 DefaultSupervisorOptions 初始化。应用的 RabbitMQConfig.Publisher 已使用该默认值，不新增 YAML/env 键。直接手写 SupervisorOptions 的调用方需提供合法间隔；纯 ValidateSupervisorOptions 在任何网络 I/O 前拒绝缺失/非法参数。

这是周期观察。拓扑在两次检查之间变化时存在发现窗口，检查中的网络读取也不是原子拓扑快照；不承诺每次 Publish 前都重新读取 management，Status/WaitReady 也不保证下一次请求一定成功。正常调度下发现故障需要等待剩余间隔及一次有界检查；默认值不是生产容量或零延迟承诺。management 故障会保守停止接单，即使 AMQP 仍可用。

## 验证与后续

```powershell
go test -mod=readonly -race ./messaging/rabbitmq -run 'Test(Supervisor|SessionTopology)' -count=10 -timeout=60s
go test -mod=readonly -race ./... -count=1
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQTopologyMonitorIntegration$' -count=1 -v -timeout=5m
```

新增 9 组默认测试覆盖多轮成功保持 epoch/准入、不重叠、空闲检查失败、脱敏、旧调用清理屏障、未修复持续未就绪、修复后新 epoch 且无重发、deadline 后错误 nil、取消 join、旧 session 晚到成功、间隔纯校验、初始化 context 与后续检查分离。受控真实 AMQP 对端另验证 passive channel.open 响应黑洞被 socket watchdog 打断，以及检查失败使已发送但丢失 confirm 的发布保持 unknown。

新增 Docker suite 的两个场景使用本轮独立 broker/vhost/随机账号/端口：

1. 已接受一条命令后移除本轮隔离队列绑定，之后没有新发布；周期复核主动撤销就绪。绑定保持缺失，不自动修复；恢复本轮绑定后新 epoch 就绪，原 accepted 内容仍在且没有重发。
2. 本轮 loopback 代理丢弃 management 响应，AMQP 独立控制连接仍可查询队列；周期复核超时撤销就绪，后续连接核验也不能就绪。恢复代理后新 epoch 可就绪，队列没有探测命令。

测试不读取开发 .env/config、不运行业务主程序，不操作共享数据；fixture 按归属标签清理。具体命令结果、runId 见[实施记录](message-reliability-mq-progress.md)。旧 broker 重启、ACK 丢帧和消费者场景本轮没有重复 Docker 实测，不将历史结果计入本轮。

P3.5.4.2 [接受就绪汇总与健康接线](runtime-readiness.md)已完成装配基础，区分 publisher 传输就绪、业务接受能力、退出准入与消费者状态。显式 api 已装配[生产接受 API](api-runtime-lifecycle.md)，legacy `/health/ready/accept` 继续返回 503。业务消费者仍归 P4；阶段要求及剩余故障范围见 [P3 验收对照](mq-reliability-acceptance.md)。
