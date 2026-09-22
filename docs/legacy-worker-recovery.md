# 旧消息 worker 所有权与恢复

启用 `LEGACY_WORKER.enabled` 后，启动先对当前 Redis DB 的固定键 `dirty:msgs:worker-owner` 执行 `SET NX`，值为每次启动生成的随机 token，无 TTL。已有注册则启动失败，不启动 HTTP 或消费任务。关闭 worker 的 API 实例不竞争锁。所有消费实例必须使用此版本，并连接同一 Redis 服务和 DB；旧版本和直接调用仓库方法不会受启动约束。

不采用 TTL：旧 worker 可能仍在执行 MySQL 事务，租约到期后让另一个 worker 接管无法保证互斥。这个阶段选择异常时停止消费、人工恢复，不提供自动选主或多 worker 消费。

运行时在读批次前、每条落库前核对 token；校验失败（包括 Redis 错误）会永久停止该 worker 的后续消费尝试，不因连接恢复自动继续。HTTP 仍可接受消息，cron 会持续记录消费错误。确认消息时，Lua 在一次原子操作内校验所有权、逐字节比较整个待确认前缀，再执行 LTRIM。失去所有权不能继续删队列，已经提交但未确认的消息由后续 worker 幂等重放。

正常返回路径先停止 cron 并等待正在运行的任务退出，再通过 Lua 比较 token 后删除锁。释放失败会记录错误并保留人工恢复责任，不影响已提交数据。当前 HTTP 服务尚无完整信号优雅停机流程，Ctrl+C、IDE 停止或强制结束进程可能不执行这一路径，应按异常退出处理。

## 恢复步骤

1. 停止连接该队列的所有启用 worker 的进程，并关闭其自动重启。确认旧进程确实退出、旧数据库会话不再执行事务；不能只凭健康检查失败或网络不可达判断它已停止。API 可用关闭 worker 的配置继续接收消息。
2. 用部署环境对应的 Redis 连接和 DB，读取 `GET dirty:msgs:worker-owner`。记录此次确认的完整 token。键不存在时跳过删除；仍须完成步骤 1。
3. 在同一 Redis DB 执行下面的比较删除，将 `EXPECTED_TOKEN` 换成步骤 2 的实际值。返回 1 表示删除成功；返回 0 表示归属已经改变，不要改用无条件 DEL，应重新核实进程状态。

   ```text
   EVAL "if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0" 1 dirty:msgs:worker-owner EXPECTED_TOKEN
   ```

4. 仅启动一个启用 worker 的实例，观察启动及消费日志，确认队列继续推进；其余实例保持关闭 worker。

SET 响应丢失可能导致启动失败但锁已存在，也采用以上流程。不要给锁设置过期时间，不要在进程仍存活时清锁。

## Redis 故障边界

本实现依赖共享 Redis 的单一权威状态以及锁键不被淘汰/删除。Redis 数据丢失、回滚、异步复制故障切换或分裂出多个可写服务，可能使新实例取得锁，而旧实例仍有已开始的 MySQL 事务。运行时校验与原子确认不能替代数据库 fencing，也不能修复 Redis 队列本身的数据丢失。

发生这些事件时先停所有 worker，再核对 Redis、MySQL 和队列状态，按上述流程恢复。不得以“当前键不存在”为自动接管依据。真正的多消费者协议留给后续 MQ 阶段。
