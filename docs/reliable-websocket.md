# P5.3：生产 v1 WebSocket 会话与订阅生命周期

现代 API 新增 `GET /auth/chat`，通过 `router.RegisterReliableChat` 接入 `service.ReliableChat`、生产 SQL/Redis 来源及[P5.2 通知分发器](notification-frames.md)。工厂闲置构造，不启动订阅或迁移。旧 legacy Chat/频道保持旧路由；现代 API 不注册旧 `/auth/getws`。

## 连接与协议

请求必须提供 `Authorization: Bearer <JWT>`；不从 query/form 取 token 或用户 ID。JWT/Redis 黑名单在 setup 期限内检查，随后定期复核原 token 与相同 UID。默认 Gorilla 同源 Origin 检查保留；客户端提供 `ichat.v1` 子协议时协商该值。调试/模拟客户端须支持 WebSocket 自定义 Authorization header，当前未增加浏览器票据或 query token 传输。

按真实 SQL 当前有效用户和未删除群/成员关系生成频道清单，个人频道始终存在、群 ID 去重排序。超过 MaxGroups 时拒绝，不能截断列表后声称订阅完整；没有群时只订阅个人频道，不发送空 SUBSCRIBE。SQL 错误/用户失效、订阅确认失败或缺少句柄时不升级成功。

初始全部频道得到 Redis subscribe 确认后才升级和注册会话，writer 的第一帧固定为：

```json
{"type":"ready","data":{"protocolVersion":1,"sessionId":"<32位随机小写hex>","syncIntervalSeconds":10}}
```

sessionId 每次物理连接重新生成。ready 仅表示本连接初始订阅已经确认，不证明没有历史缺口或客户端已经同步。支持 P5.2 的 message/command.result、`sync.required`、P5.4 的 [ack/ack.confirmed](delivery-ack.md) 和 JSON/控制心跳；JSON 心跳严格为 `{"type":"ping|pong","data":{}}`。重复键、别名、未知字段、二进制应用帧和未实现的应用命令关闭连接。业务 ACK 在当前会话 gate 内持久化，成功提交后才确认；不接收业务发送帧。

## 单 writer 与会话替换

- 每个 API 实例持有自己的 UID 会话注册表；同一用户新连接成功升级后替换旧连接，随机 sessionId 不复用。来源/升级失败不会踢掉现有连接。
- 每个会话只有一个数据/控制 writer。服务器 JSON ping、应用 pong、WebSocket control pong 也走该 writer，覆盖 Gorilla 默认自动写控制帧的处理，避免隐含第二个 writer。
- 队列只保存有界、严格解码的通知提示和小控制帧，不长期缓存已授权正文。writer 临近写入时执行 P5.2 SQL 读取/当前权限检查，SQL 释放后才做 socket I/O；原 eventId、消息身份和 seq 保留。
- 每用户门闩串行化实际写入和 current-session 切换。替换可取消旧会话、关闭旧 socket；旧 SQL 回调晚返回时 context/current-session 检查禁止其写入。旧连接清理只移除自身记录，不清除新会话。
- 注册表引用计数覆盖所有待建立/当前/退役但仍在回调中的会话；结束后才释放容量和 UID slot，避免替换期间丢失 owner 或永久累积 UID。

以上是**单 API 实例**约束，没有多实例共享 session 所有权或全局 ACK fencing。首版不能通过扩容 API 进程直接获得全局单会话。

队列满立即取消/断开慢会话；写入和权限解析分别有期限，实际出站使用较早的解析/写期限。受控阻塞 socket 证明 write deadline 到达后结束，不通过遗弃写 goroutine 假装完成。无数据时 reader 有读期限，JSON ping/pong 与控制 ping/pong 刷新心跳活跃时间；心跳不代表业务 ACK。依赖失败、流关闭、非法帧或工作回调 panic 终止本会话，不记录可能包含正文的 panic 值。

## 刷新、取消与资源责任

默认每 10 秒复核 token 并重新读取频道列表。变化时建立并确认新的完整订阅，随后关闭/join 旧订阅；轮换期间允许重复提示，接收端仍须按消息身份/seq 去重。群删除/退群会从下一次频道列表移除；在刷新前到达或已排队的旧提示仍受 writer 的实时 SQL 权限门禁，不因旧订阅继续传出正文。

每次刷新均发送 `{"type":"sync.required","data":{"scope":"all"}}`，ready 也给出周期同步间隔。它们是提醒，不是可靠性的唯一触发器；客户端仍须独立定期扫描/补拉。底层 Pub/Sub 可能重连并丢通知，不能把连接存活或订阅恢复当成历史完整。HTTP 补拉/实际客户端恢复仍在 P6 等后续任务。

订阅确认使用 setup 的取消/期限，但成功后解除该临时关联，保留会话 lifetime；取消已成功的 setup 不会关闭当前流。部分/迟到返回的订阅即使带 error 也保留 Close 责任。接收/刷新任务退出时关闭当前订阅；所有旧/新订阅、reader、writer 与 SQL 回调实际结束前，HTTP handler 不返回。

API 复用已验收的 hijacked 请求 owner：停止准入取消已升级会话，关闭 socket 以解除 reader/write 阻塞，并等待所有当前及被替换的 handler；observer 返回 pending 不等于处理已经完成。HTTP RequestsDone 和探针 Done 之前不关闭借入的 SQL/Redis。来源/SQL/关闭回调即使没有遵守 deadline，也继续由原请求保留资源，不能遗弃 goroutine 或提前释放客户端。

JWT 撤销/失效和身份存储故障在周期复核时关闭既有连接，有默认刷新间隔的延迟，不声称跨 Redis/SQL/socket 原子撤权。P5.2 的权限锁保护 SQL 材料化阶段，随后网络写入仍非原子权限事务；本项把检查放在临近发送处并避免正文排队，不扩大其时间承诺。

## 默认边界

| 参数 | 默认值 |
| --- | --- |
| SetupTimeout / ResolveTimeout / WriteTimeout | 3s / 2s / 2s |
| RefreshInterval / HeartbeatInterval / ReadTimeout | 10s / 20s / 60s |
| QueueSize / MaxGroups / MaxSessions | 64 / 1024 / 1024 |

操作/周期期限须在 (0,1min]，ReadTimeout 大于 HeartbeatInterval 且不超过 2min；QueueSize 1—1024、MaxGroups 1—1024、MaxSessions 1—10000。MaxSessions 包括仍保留资源的旧/待建立会话，容量用尽时新连接可被拒绝。默认值为库级装配参数，本项不增加 YAML/env 配置或 schema。

## 验证

```powershell
go test -mod=readonly -p=2 -race ./service ./database ./messaging . -run '^Test(ReliableChat|NotificationSource|Heartbeat|APIChat|APIStartup)' -count=30 -timeout=120s
go test -mod=readonly -p=1 -race -tags=integration . -run '^TestProduction(V1Chat|API)Integration$' -count=1 -v -timeout=10m
```

八组生产入口测试使用真实 JWT/Redis、SQL 数据与实际 Outbox loop：鉴权/同源、空群个人频道、规范数据帧、连接替换、入群后确认订阅、群删除后的旧提示抑制/退订、撤销 token 关闭连接，以及 API 退出后的 socket/订阅/客户端释放。采用生产默认的 10 秒刷新，未通过缩短默认参数冒充周期行为。

本 suite 的 API publisher 是明确的未使用 MQ fixture capability，业务命令直接调用生产 PersistCommand，Outbox 实际向 Redis 发布，再由生产 v1 入口发送帧；不冒充完整 HTTP 202→MQ→持久化→WebSocket 联调。原生产 API/MQ 故障 suite 同时回归，验证离线 MySQL 仍不阻止已登录用户的 MQ 接受。

最终 5 command / 5 message / 2 conversation / 10 completed Outbox / 0 cursor / 0 quarantine，schema 指纹不变。默认测试另验证迟到 SQL/来源、部分刷新、单 writer 的阻塞 socket deadline、队列溢出、旧会话不清除新记录、严格心跳、回调 panic 和 API pending 后保留客户端。运行及清理证据见[实施进度](message-reliability-mq-progress.md)。

P5.4 [业务 ACK 与持久化游标](delivery-ack.md)已完成；原生产 suite 增加 ACK 单调提交验收后为九组，最终 cursor 数量为 1。[P6.1 状态 HTTP 接口](command-status.md)已完成；P6.2.2 [会话列表 HTTP](conversations-http.md)已完成；P6.2.3.1 [补拉 HTTP](message-sync-http.md)已完成；P6.2.3.2 [历史 HTTP](message-history-http.md)已完成；P6.3.1 [发送状态核心](client-sender-simulator.md)已完成；下一项 **P6.3.2：磁盘快照与跨进程恢复**；默认 worker/all 装配与真实客户端恢复仍待完成。
