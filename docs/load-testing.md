# iChat 压测指南

本文档用于生成可复现、可写入简历的性能数据。每次结果必须同时保留代码版本、机器配置、消息大小、并发连接数和测试时长。

P3.6.1 新增独立的新协议探测模式。后文原有性能命令均为默认 `-mode legacy`，其中旧 HTTP 成功计数不代表 MQ accepted；新协议的落库/端到端容量基线仍待后续阶段。

## 确认式接受协议探测

仅对隔离环境中显式注册的新路由使用 `-mode acceptance`；默认主程序尚未接线，不能对旧服务返回的 HTTP 200 计为 accepted。该模式不创建账号或打开 WebSocket，只验证接受协议，不产生消息送达或持久化吞吐指标。

先保存一个 v1 JSON 请求到 `pending-request.json`，clientMessageId 在请求超时/unknown 后保持不变，例如：

```json
{"clientMessageId":"8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1","targetType":"private","targetId":"34","media":"text","content":"fixture probe"}
```

以下变量由本轮隔离 fixture 提供：base URL、已鉴权发送者 ID 和有效 JWT。不要把 token 写入命令行参数或结果文件。

```powershell
$env:ICHAT_LOADTEST_TOKEN = $fixtureToken
go run ./cmd/loadtest -mode acceptance -base-url $fixtureBaseURL -sender-id $fixtureUserID -request-file pending-request.json -attempts 1 -request-timeout 5s -out acceptance-result.json
```

输入文件限制 16 KiB，复用严格解码和规范化；发送者只用于校验响应身份，不写入请求。确认仅接受 HTTP 202/code=0/state=accepted、匹配的 commandId/clientMessageId/payloadHash 和有效 attempt。旧 200、丢失/错误/过大响应或错误身份都保守记录 unknown；不跟随 redirect，不打印 token/正文/底层诊断。

`-attempts` 范围 1–100，是对文件中同一请求的明确顺序重放次数；默认 1，没有自动 unknown 重试或换键，不实现业务恢复轮询/退避器。通常按一次结果和 Retry-After 安排后续显式调用。任何响应都不删除/重写输入，`-out` 也不能指向同一文件。出现一次已知 accepted 后，后续失败不会把整体状态降回 unknown；accepted 仍不是业务终态，文件继续保留到后续状态查询确认或明确放弃。

本轮测试只在自有 loopback 对端与独立 Docker fixture 验证协议，不执行实际性能压测。实现及覆盖边界见[确认式发送适配](command-submission.md)。

## 1. 指标口径

- **连接成功率**：成功建立 WebSocket 的连接数 / 尝试连接数。
- **稳定连接率**：测量结束时仍存活的连接数 / 成功连接数。
- **HTTP P95**：发送消息接口从发起请求到收到响应的 P95。
- **端到端 P95**：发起发送请求，到目标用户从 WebSocket 收到相同消息 ID 的 P95。
- **送达率**：WebSocket 实际收到的消息数 / 服务端接受的消息数。
- **实际吞吐量**：测量阶段收到的消息数 / 测量秒数。

简历应使用端到端 P95，不能用 HTTP 响应时间代替消息送达延迟。

## 2. 测试前准备

~~~powershell
docker compose up -d
docker compose ps
go run ./cmd/migrate
# 迁移成功后再启动服务。
$env:ICHAT_LEGACY_WORKER_ENABLED = "true"
$env:GIN_MODE = "release"
$env:ICHAT_LOG_LEVEL = "info"
$env:ICHAT_LOG_ACCESSENABLED = "false"
& "C:\Program Files\Go\bin\go.exe" run .
~~~

压测时使用 `info` 级别会关闭逐消息 `debug` 日志；关闭访问日志可避免每次
`/auth/sendMessage` 请求产生磁盘 I/O。异常、WebSocket 连接建立/关闭以及定时
落库结果仍会记录。测试结束后可执行以下命令恢复当前 PowerShell 会话：

~~~powershell
Remove-Item Env:ICHAT_LOG_LEVEL
Remove-Item Env:ICHAT_LOG_ACCESSENABLED
Remove-Item Env:ICHAT_LEGACY_WORKER_ENABLED
~~~

日志配置发生变化后必须重新建立性能基线。不同日志配置产生的数据必须使用不同
文件名并分别标注，不能混合计算中位数或直接作前后性能结论。

等待 MySQL 和 Redis 均为 healthy。压测工具会创建独立测试用户并以环形方式发送私聊消息，请使用专用开发数据库。

## 3. 冒烟测试

~~~powershell
go run ./cmd/loadtest -users 20 -rate 20 -duration 30s -warmup 5s -message-bytes 256 -out loadtest-smoke.json
~~~

合格条件：

- 20 个连接全部建立；
- 无异常断开；
- HTTP 失败数为 0；
- 送达率为 100% 或非常接近 100%；
- 服务端没有 panic。

冒烟测试不用于简历，只用于确认测试工具和服务链路正确。

## 4. WebSocket 长连接稳定性

~~~powershell
go run ./cmd/loadtest -users 100  -rate 0 -duration 10m -warmup 15s -out ws-100.json
go run ./cmd/loadtest -users 300  -rate 0 -duration 10m -warmup 15s -out ws-300.json
go run ./cmd/loadtest -users 500  -rate 0 -duration 30m -warmup 15s -out ws-500.json
go run ./cmd/loadtest -users 1000 -rate 0 -duration 30m -warmup 15s -out ws-1000.json
~~~

满足以下条件的最高一档可以称为稳定连接数：

- 连接成功率不低于 99%；
- 持续运行至少 30 分钟；
- 异常断开率低于 1%；
- 内存没有持续线性增长；
- 服务没有 panic。

## 5. 消息吞吐与延迟

~~~powershell
go run ./cmd/loadtest -users 100 -rate 50  -duration 5m -warmup 15s -message-bytes 256 -out msg-50.json
go run ./cmd/loadtest -users 100 -rate 100 -duration 5m -warmup 15s -message-bytes 256 -out msg-100.json
go run ./cmd/loadtest -users 100 -rate 200 -duration 5m -warmup 15s -message-bytes 256 -out msg-200.json
go run ./cmd/loadtest -users 100 -rate 500 -duration 5m -warmup 15s -message-bytes 256 -out msg-500.json
~~~

旧队列压测仅在一个应用实例显式启用 worker；共享 Redis DB 的第二个 worker 会被排他注册拒绝。异常退出的遗留锁按[恢复说明](legacy-worker-recovery.md)处理，不可直接删除后并发重启。每轮最多处理 128 条，周期为 60 秒，重叠触发会跳过，因此固定等待 60 秒不能保证积压已清空；每档之间应确认测试队列处理完成。稳定消息吞吐量的建议口径：

- HTTP 失败率低于 1%；
- 消息送达率不低于 99%；
- 端到端 P95 不超过预设目标，例如 200 ms；
- 没有连接异常断开或消息积压持续增长。

最终档位至少重复 3 次，简历使用三次结果的中位数。

## 6. 记录环境和资源

~~~powershell
Get-Process iChat | Select-Object CPU,WorkingSet64,Threads
docker stats --no-stream
go version
docker version
git rev-parse HEAD
Get-ComputerInfo | Select-Object CsProcessors,CsTotalPhysicalMemory,WindowsProductName,WindowsVersion
~~~

压测客户端最好运行在另一台机器。若服务端和压测客户端在同一台机器，简历必须写明“单机本地环境”。

## 7. MySQL 历史消息查询

P1.6 起旧历史接口直接查询 MySQL，不使用 Redis 历史缓存。排序为 `(time_stamp, identifier, id)`，私聊条件包含双向收发；群查询另在同一读事务内检查当前成员和未删除的群。下面的索引仅为待测候选，本轮未迁移索引或取得性能基线。

1. 准备 10 万、50 万、100 万条消息。
2. 选择一个包含大量消息的会话。
3. 对真实历史消息 SQL 执行 EXPLAIN ANALYZE。
4. 记录实际扫描行数和执行时间。
5. 添加与查询条件、排序方式匹配的联合索引。
6. 对相同数据和 SQL 再测至少 30 次。

可先评估以下索引，但添加前必须以真实执行计划为准：

~~~sql
CREATE INDEX idx_message_private_history
ON message (sender_id, receiver_id, type, time_stamp DESC, identifier DESC, id DESC);

CREATE INDEX idx_message_group_history
ON message (receiver_id, type, time_stamp DESC, identifier DESC, id DESC);
~~~

~~~sql
EXPLAIN ANALYZE
SELECT *
FROM message
WHERE type = 2
  AND ((sender_id = 1 AND receiver_id = 2)
    OR (sender_id = 2 AND receiver_id = 1))
ORDER BY time_stamp DESC, identifier DESC, id DESC
LIMIT 20;
~~~

优先记录实际扫描行数和 P95，因为单次耗时容易受到 MySQL Buffer Pool 和操作系统缓存影响。

## 8. Redis 缓存

在专用 Redis 实例上，分别在测试前后运行：

~~~powershell
docker compose exec redis redis-cli -a $env:REDIS_PASSWORD INFO stats
~~~

记录 keyspace_hits 和 keyspace_misses 的增量：

~~~text
命中率 = hits 增量 / (hits 增量 + misses 增量) × 100%
~~~

这是 Redis 实例的全局数据，也包含 JWT 黑名单等访问。P1.6 的历史读取不访问 Redis，不能把该指标写成历史消息缓存命中率。

## 9. 简历模板

~~~text
在 [CPU/内存/操作系统] 的单机环境下，使用 256 B 消息持续压测 [X] 分钟，
稳定维持 [X] 个 WebSocket 长连接；在 [X] msg/s 下消息送达率达到 [X%]，
端到端消息延迟 P95 为 [X ms]。
~~~

~~~text
针对历史消息查询设计 [字段列表] 联合索引，在 [X 万] 条消息数据下，
将 EXPLAIN ANALYZE 实际扫描行数由 [X] 降至 [X]，
查询 P95 由 [X ms] 降至 [X ms]。
~~~

只有达到预设成功率并重复验证的数据，才能作为简历数字。
