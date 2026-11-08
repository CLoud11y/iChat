# P6.3.4.2.3.4.1：真实隔离审计与发送端禁止重发

本项把已接受命令的真实数据约束失败、隔离 MQ 转移、审计 worker 落库和客户端查询保护接在一起。分别覆盖客户端已保存 accepted，以及真实 202 回执丢失后保存 publish_unknown；审计存在时只查询 quarantined，不自动或显式重发，不把技术隔离伪装为业务终态。

P6.3.4.2.3.4 已拆为本项、P6.3.4.2.3.4.2 受控重放/终态核验与客户端恢复，以及 P6.3.4.2.3.4.3 发送侧矩阵和范围汇总。后两项实施前继续细分；完整发送矩阵、默认 worker/all、实际前端及接收侧仍未收口。

## 真实数据约束与责任转移

`TestProductionSenderQuarantineIntegration` 的两个子场景各使用独立 runId 的真实 MySQL/Redis/RabbitMQ、JWT、生产 API、PersistenceSupervisor、隔离 publisher、QuarantineService 和 FileStore。API 健康输入为夹具，角色显式装配；不报告默认 worker/all 资源工厂完成。

1. 只有消费 SQL pool 接入私有连接适配，沿用既有 retrySQLConn 的实际辅助 SQL 故障方式。生产真实 message INSERT 成功后，在同一事务执行一次 `UPDATE message SET seq=0 WHERE id=?`，触发现有 seq>0 CHECK；把服务器原始 MySQL 3819 错误原样返回。没有伪造错误、改写原规范信封/原 INSERT 或直接调用仓储处理命令；先前 INSERT、命令占用和会话写入都由生产事务回滚。独立计数要求 1 BeginTx/1 辅助 UPDATE/1 实际 3819/0 Commit/1 Rollback，无本地事务重试。
2. HTTPRunner 实际 POST 原请求，生产 API 真实 202；accepted 路径收到完整回执，unknown 路径只丢弃该 HTTP 回应，不丢 publisher confirm。两条磁盘初态都保留原请求/身份/hash、attempts=1、非 inFlight、无业务 Result。
3. 观察原真实 Basic.Deliver，redelivered=false；完整信封严格解码并核对规范 command、实际 HTTP receipt 身份及 publishAttemptId。真实约束错误触发生产事务回滚，保守 DATA_CONSTRAINT 策略把完整源命令转移隔离目标。
4. 独立 publisher 帧观察器核对目标实际新增一次 Basic.Publish/一次 broker Basic.Ack；源消费 ACK 匹配原 delivery 的连接/channel/tag。没有主动丢任何 AMQP 帧。正常 drain/close/加入原持久化 owner 后，主队列为空/无消费者，隔离队列实际 ready=1/consumers=0，先证明 MQ 目标持有责任。
5. 这时才启动真实隔离消费连接和 QuarantineService。观察实际 wrapper delivery，解码 DATA_CONSTRAINT，核对其中原 source body 与主队列原信封逐字相同，原 commandId/发布 attempt 保持。生产审计仓储提交后才对 wrapper 单条 ACK；测试不使用控制 Get/ACK 或手工保存审计。
6. 用只读 QuarantineRepository.Get 重新核验完整审计文档/冗余字段，核对 transportId、规范 sender/client/command/hash、原稳定内容、state=open、无 replayAttemptId/ResolvedAt。正常 drain/close/加入审计 service/session 后，停用 SQL 故障适配，恢复正常写入条件。

两个业务角色均实际处理原 broker delivery。最终 ACK 证据结合完整转发、原 owner 加入后无重新入队以及主/隔离两队列管理 ready/unacked/consumers 全零；不以 SQL 行存在或单次 TCP 写成功冒充 broker 已确认。

## 客户端查询保护

故障适配已停用、两角色已加入后，客户端实际 GET 原命令。生产状态接口从 SQL 审计返回 200/code=0/quarantined，消息字段为空，身份/hash 与原记录绑定；本地保存完整非终态 DTO、原请求及 attempts=1。

- 显式 HTTPRunner.Submit 返回 ErrBlocked，没有发 HTTP，也不增加 attempts。
- 关闭并重开 FileStore 后记录完全相同，显式 Submit 仍被本地门禁拒绝。
- 有限 Recover 给足两轮、两次可用提交预算，实际只做两次 GET；服务器仍为 quarantined，因此返回 ErrRecoveryExhausted。该错误表示本轮查询结束且没有业务终态，不等于丢失记录或自动解除隔离。
- 再次磁盘重开仍相同。只读 Get 前后完整审计（含首份观察/时间/原文）相等，仍为 open；恢复正常数据库写入条件本身不授权客户端重新发布。

每条路径精确 **1 POST + 3 GET、attempts=1**。源真实 delivery/ACK 各 1，隔离目标新增真实 publish/confirm 各 1，审计 wrapper delivery/ACK 各 1。原 UUID 大写、目标前导零、Unicode/NUL/换行和纳秒时区时间保留；原规范信封及物理发布 attempt 保持。

最终 **0 command / 0 message / 0 conversation / 0 Outbox / 0 delivery cursor / 1 immutable open quarantine audit**，主/隔离两队列均无 ready/unacked/consumer。业务事务回滚，没有分配持久业务序号或部分结果。这里没有启动 Outbox worker，也没有业务 command.result：本项只验收技术隔离，不报告 persisted/rejected 或接收侧交付。

## 范围与清理

故障只存在于本轮消费 pool 的连接适配，API/审计 SQL 不受影响；角色和会话先加入，再由测试 owner 释放借入 pool/代理并回收环境。最终实现没有创建 CHECK/trigger 或改变表结构、数据库权限/全局设置，结束时重新执行迁移验证原 schema 指纹。生产迁移、隔离白名单、依赖和默认运行模式均未修改。会话来源仍为测试调用生产 DialConsumer 的适配；磁盘重开为同一进程内 owner 关闭/重建。

未执行 CLI/应用受控重放、replay_pending/resolved 转换、隔离后业务终态及迟到结果交叉；这些归 P6.3.4.2.3.4.2。未重跑全部隔离故障、进程强杀或默认完整运行时，也未据此完成发送侧矩阵。

```powershell
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProductionSender(Quarantine|ConsumerAckLoss)Integration$' -count=1 -v -timeout=8m
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

原消费 ACK 丢失专项同步回归参数化后的只读队列探针，默认仍观察主队列；新场景同时观察主/隔离队列，缺指标字段不视作 0。实际结果、runId、故障边界、早期失败与资源核验见[实施记录](message-reliability-mq-progress.md)。
