# 生产 API 生命周期与装配（P3.6.2）

`main → runProcess → application.RunRuntime` 现在为显式 `RUNTIME.mode: api` 装配真实确认式 API。默认配置仍为 legacy；未完成 consumer 前仅在独立测试环境接受、积压命令。`worker` / `all` 继续在信号注册和外部 I/O 前报告缺失的持久化、隔离及 Outbox 角色。

本 API 只注册 `POST /auth/sendMessage` 和四个 `/health/*` 路由；发送契约见[确认式发送适配](command-submission.md)。旧登录、账号、联系人、历史、WebSocket 路由未接入这个角色，测试使用独立 JWT key 签发的已有用户 token。账号入口及完整状态、同步、通知恢复仍按后续阶段接入，不能据此切换共享开发数据。

## 配置与启动

进程只加载一次配置。先检查角色完整性、MQ 参数和 API 的 JWT、Redis/MySQL 地址、连接池及 HTTP 期限，再注册退出信号。API 构造器不联网、不启动任务；Start 才创建私有 MySQL/Redis、token 服务、探针和监听器。HTTP 复用已有受控请求 owner，使用独立 Gin engine，不绑定旧全局或启动旧 worker，不迁移 schema。

MySQL 使用延迟连接池（最多 4 个连接、2 个空闲连接），Redis 客户端延迟连接，不预建空闲连接，单次 GET 不隐式重试。依赖离线不阻止 HTTP 存活启动。MQ 由执行器拥有，publisher 的连接、持续拓扑检查及当前状态仍由原 supervisor 管理。

监听地址默认 `:8080`，兼容 `PORT`；以下为内部装配选项，没有增加 YAML/env 配置键：

| 选项 | 默认值 | 用途 |
| --- | --- | --- |
| ReadHeaderTimeout | 5 秒 | 有界读取请求头 |
| ReadTimeout | 10 秒 | 有界读取头及正文，包括慢正文 |
| SubmissionTimeout | RABBITMQ.publishTimeout | 包含实际鉴权与发布的请求 deadline |
| WriteTimeout | max(30 秒, 11 秒 + publishTimeout) | 留出正文读取和请求处理时间 |
| IdleTimeout | 30 秒 | 限制空闲 keep-alive |
| MaxHeaderBytes | 32 KiB | 请求头上限（net/http 另有读取余量） |
| ProbeInterval / ProbeTimeout | 2 秒 / 1 秒 | 后台依赖探测与缓存新鲜度 |
| DrainTimeout / CloseTimeout | 10 秒 / 5 秒 | 意外退出时自有 drain、强制关闭预算 |

正常进程退出使用 RuntimeOptions 的共享 drain/close 预算（默认 10 秒 / 5 秒）。正文仍受协议 16 KiB 限制。除写期限上限 2 分钟外，期限必须为正且不超过一分钟；写期限至少覆盖读期限与请求期限之和。

## 鉴权与健康

Redis 探针实际 GET 每实例随机 `ichat:auth:probe:<随机值>`，不存在的 key 和正常读取算成功，权限、连接及读取错误均失败。探针不写 key、不签发 token；它验证鉴权实际需要的 GET 能力，不能用 PING 代替。每个请求仍独立读取 token 黑名单，缓存 ready 不绕过鉴权。

复用健康探针 owner 的非阻塞缓存及新鲜度判断；失败、缓存过期或停止立即撤销 AuthenticationReady。AcceptanceReady 表示新 handler 已启动、accept loop 未退出且仍接单。两者交由[运行时就绪汇总](runtime-readiness.md)与 publisher 状态组合，能力方法不递归读取汇总器。MySQL 单独影响 storage readiness，不影响 MQ 接受；api 模式不宣称消费者就绪。

## 退出与资源归属

1. StopAdmission 撤销业务准入并关闭 HTTP 请求 gate，新请求得到固定 JSON 503 / not_published；已进入的请求保留原 context 和客户端。
2. Drain 关闭监听、等待实际活动请求完成，不取消原请求，也不关闭存储。成功 setup 的 context 取消不结束 API lifetime。
3. drain 结束或预算耗尽后，执行器取消 role lifetime。API 唯一清理 owner 取消请求工作 context，共用一个自有 close deadline 并行关闭 HTTP、停止两个探针。
4. Close 的等待者超时返回可识别的 pending 错误；owner 仍等待实际 HTTP 请求、探针和 accept loop 全部退出。完成后只关闭一次 Redis/MySQL，再发布 Err 与 Done。重复 Close 可再次加入同一次清理。
5. 部分启动失败（包括返回 handle + error、typed nil、绑定失败和 handoff 取消）也由同一 owner 回收。意外 accept loop 退出先撤销准入、尝试有界 drain，再清理。

Close 超时不是任务退出证明。执行器可能按自己的预算返回 shutdown failure 并关闭共享 MQ；尚未退出的 API 工作仍保留私有客户端。进程被 OS 强制终止时无法承诺这些内存任务完成。已知完整 confirm 不因随后取消而变成未知；客户端没收到响应时仍应保留原请求身份恢复，API 不自动重发。

## 验证

```powershell
go test -mod=readonly -race . -run 'Test(API|RunProcess|LegacyHTTP)' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=90s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration . -run '^TestProductionAPIIntegration$' -count=1 -v -timeout=7m
```

默认测试使用假配置与受控依赖、真实 loopback TCP，覆盖慢头/慢正文、GET 鉴权状态、停止准入与发布 drain、关闭超时保留请求和探针、各阶段部分失败、纯校验及进程默认工厂选择。实际 Docker suite 从默认进程工厂进入真实 RunRuntime/API/Redis/MySQL/MQ，覆盖 Redis 离线启动/断连恢复、真实确认后的 drain、超时仍保留客户端、端口占用回滚和 MySQL 离线接受；HTTP 回执与队列信封/attempt 匹配。

Docker 仅使用随机 runId 的 fixture，控制 MySQL 和清理前核验所有权；Redis 故障只关闭本次 loopback 代理连接。测试注入的 receipt barrier 只延迟实际 confirm 的返回，没有伪造接受或替换业务服务。最终读取/ACK 属于 fixture 控制操作，没有业务 consumer/事务提交后 ACK。OS 信号通过可控 context 测试，未实际向宿主进程投递；不代表吞吐、落库、通知、三节点或端到端恢复验收。

P3.6 的 handler、客户端协议探测及生产 API 装配范围完成。下一项为 P3 总体验收核对与文档收口；P3 整体尚未宣称验收通过。
