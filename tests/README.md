# 独立测试环境

当前验证工具链为 **Go 1.27.1**。运行前按 [README 工具链说明](../README.md#go-工具链) 设置当前终端的 `GOTOOLCHAIN=go1.27.1`，并用 `go version` 核对实际版本。

默认单元测试不启动外部服务：

`health` 包使用可控探测函数验证首次未就绪、失败后恢复、再次失败、超时、结果过期及取消退出；`router` 包验证三个健康接口的状态码、缺失探测器处理和禁用缓存。它们不替代真实 MySQL 停机恢复验收。

```powershell
go test ./...
```

启动流程的回归测试也在默认范围内：注入配置、MySQL/Redis 构造器、worker 和 HTTP 启动函数，覆盖依赖缺失、启动失败、连接释放及全局绑定恢复。MySQL 使用不联网的测试 driver，Redis 使用拒绝联网的测试 dialer；另用真实 cron 验证停止时等待正在执行的任务。可运行 `go test -race . -count=2 -shuffle=on` 检查重复执行和退出时的数据竞争。

MySQL 离线启动测试也在默认范围内：本地临时端口接受后立即关闭连接，模拟 MySQL 握手失败，验证默认 API 构造器无需连接即可启动 worker/HTTP、登录返回 503、退出关闭连接池及恢复全局绑定。另验证取消的 Ping 和独立迁移所用构造器仍在连接失败时报错。这些用例不使用 Docker 或开发数据库，不替代真实 MySQL 停机/恢复验收。

P0.2 的环境隔离验收需要本机 Docker 引擎已启动、Docker Compose 可用。首次执行可能需要下载测试镜像：

```powershell
go test -tags=integration ./tests/integration/... -count=1 -v -timeout=12m
```

这条命令自动创建、等待就绪并清理测试资源，不需要先手动执行 `docker compose up`。

P0.6.3d 的真实 MySQL 离线启动与恢复验收：

```powershell
go test -race -tags=integration . -run '^TestBootstrapMySQLRecoveryIntegration$' -count=1 -v -timeout=8m
```

测试先创建本轮 fixture 并迁移/写入测试账号，再停止该 MySQL 容器，使用默认 bootstrap 的数据库、Redis、worker 和健康探测装配启动本地临时 HTTP 服务。验证存活接口、存储未就绪、登录/联系人 503 和无 token 的 401；恢复后验证就绪、登录身份/token 与联系人查询。随后再次停机、恢复，始终使用同一 API 和仓储实例。Docker 重启可能重新分配随机宿主端口，测试以本地 TCP 转发器提供固定入口，仅更新其后端地址；应用配置、池和仓储不变，所有握手及查询均到达真实 MySQL。接受就绪仍为 503，不代表 MQ accepted 验收。容器停止/启动前核对本轮项目和 runId 标签；结束后按 fixture 归属清理，转发器等待所有连接任务退出。该测试不模拟网络黑洞或操作系统进程退出。

P0.5 的迁移命令验收同样使用独立环境：

```powershell
go test -tags=integration ./cmd/migrate -count=1 -v -timeout=8m
```

覆盖实际命令入口的退出码：连接失败、DDL 失败、首次创建四张旧表、重复迁移保留用户及消息；Redis 配置不可用时迁移仍成功。默认单元测试另行覆盖配置缺失/损坏后的非零退出，不连接数据库。迁移测试不读取开发配置，不对开发数据库执行迁移。

P0.6.2 的鉴权/注销单元测试覆盖 JWT 校验、黑名单错误分类、HTTP 401/503、请求取消及“写入成功后才下线”的顺序；不启动外部服务。真实 Redis 验收使用独立测试环境：

```powershell
go test -race -tags=integration ./tests/integration -run '^TestAuthRedisIntegration$' -count=1 -v -timeout=8m
```

该用例验证不存在的黑名单 key、封禁后的拒绝、正数且有界的 TTL、请求取消及客户端关闭；在本轮 Redis 中创建仅允许 GET/PING 的 ACL 用户，验证认证读取成功后 SET 被真实 Redis 拒绝时不会报告注销成功。测试结束关闭客户端并按归属清理本轮资源。

P0.6.3a 的账号接口默认测试使用注入的用户仓储，覆盖仓储/DB 缺失、查询写入错误、请求取消、业务失败以及登录成功后才标记在线。真实 MySQL 验收沿用独立 fixture：

```powershell
go test -race -tags=integration ./service -run '^TestAccountMySQLIntegration$' -count=1 -v -timeout=8m
```

该用例验证注册存储字段、密码校验和签发 token；8 路并发注册同一手机号只有一个成功；在本轮测试表上临时添加 CHECK 约束制造真实写入失败，随后移除；验证请求取消不写入、连接池关闭后注册/登录返回 503。schema 已由 fixture 核验，测试不改变开发数据库。

P0.6.3b.1 的联系人、好友、群列表和群成员查询默认测试覆盖仓储缺失、查询失败、请求 context 传递、空结果和成功响应；联系人查询的第二阶段失败必须返回 503，不能返回部分好友数据。真实 MySQL 验收：

```powershell
go test -race -tags=integration ./service -run '^TestContactQueriesMySQLIntegration$' -count=1 -v -timeout=8m
```

该用例在全局关系/群 manager 为空时验证注入仓储正常工作，核对四个接口的数据和空数组；验证七种仓储查询保留请求取消原因；临时重命名本轮 fixture 的群表，使好友查询成功而群查询失败，核对 503，恢复表名后重新查询成功；关闭连接池后四个接口均返回 503。变更仅限本轮隔离 schema，结束时清理环境。

P0.6.3b.2 的写入默认测试覆盖四个接口的依赖缺失、写入错误、请求取消、绑定错误及身份/context 传递；群 ID 校验和已知业务错误不会泄露底层信息。无网络事务测试验证建群开始失败、第二次写入失败、提交失败及 panic 回滚/传播。真实 MySQL 验收：

```powershell
go test -race -tags=integration ./service -run '^TestContactWritesMySQLIntegration$' -count=1 -v -timeout=8m
```

该用例验证双向好友、建群与创建者成员关系、入群、重复关系和非群主删除；在本轮关系表上设置临时 CHECK 约束，验证创建者成员写入失败会回滚群记录，反向好友写入失败不会留下单向关系。另验证取消请求不写入、不删群，群表不可用时删群返回 503，连接池关闭后四个写入接口均返回 503。约束及表名修改只限本轮隔离 schema。

P0.6.3c.1 的旧 worker 默认测试注入仓储，验证读取→落库→确认顺序、错误原因保留、空队列、批次计数异常和取消请求。读取失败不落库，落库失败不确认，确认失败返回错误；不依赖全局消息 manager 或日志文件。独立集成验收：

```powershell
go test -race -tags=integration ./scheduledTasks -run '^TestSyncMsg$' -count=1 -v -timeout=8m
```

除原有成功落库和空队列外，覆盖 Redis 客户端关闭、取消请求、真实 MySQL 写入失败后队列保留，以及通过 Redis hook 注入 LRANGE/EVAL 错误。EVAL 发送前失败用例断言数据库已提交而队列仍保留；P1.3.1 增加随后重投，验证数据库记录不变且队列确认成功。P1.2 增加 130 条消息分两轮（128 + 2）落库的实际 DB/队列断言。

P1.3.1 的旧消息幂等仓储验收：

```powershell
go test -race -tags=integration ./database -run '^TestLegacyMessagePersistenceIntegration$' -count=1 -v -timeout=8m
```

验证同批重复、跨批重放、已有/新增消息混合、七种字段冲突不覆盖原记录、批次回滚、真实 CHECK 失败、非主键唯一索引冲突不被误判为幂等成功，以及坏 JSON/空 ID 不写入。默认事务替身覆盖开始、第二次写入及 COMMIT 失败，错误原因保留。当前仍使用旧全局 message.id 主键和全部字段相等判定；冲突/坏记录会阻塞当前批次，持久隔离与连续前缀推进留给 P1.3.2，不代表进程崩溃或真实 COMMIT 响应丢失验收。

P1.2 的有界读取及 Lua 原子确认验收：

```powershell
go test -race -tags=integration ./database -run '^TestDirtyMessageBatchIntegration$' -count=1 -v -timeout=8m
```

使用本轮独立 fixture 验证 128 条读取上限、读取后追加、过期批次、非首条不匹配、队列变短/不存在、JSON 原始字节差异、空记录/二进制记录及 WRONGTYPE。另让两个确认请求与尾部追加同时执行，断言只有一个确认成功，另一个返回前缀冲突，未落库尾部完整保留。默认单元测试覆盖确认参数边界、错误链、原始批次传递及超大批次拒绝。每次 worker 调用只处理一批；前缀变化时结束本轮，下次调用重新读取。单消费者约束、数据库幂等和确认响应丢失仍是后续验收范围。

P0.6.3c.2 的 HTTP 消息默认测试覆盖仓储缺失/失败、请求取消、参数/游标错误、身份绑定、分页参数传递、历史顺序/空数组及解码失败不返回部分历史。真实验收：

```powershell
go test -race -tags=integration ./service -run '^TestMessageHTTPIntegration$' -count=1 -v -timeout=8m
```

验证真实入队内容中的发送者为鉴权用户、Redis 缓存命中、MySQL 回源、旧格式缓存迁移、缺失 DB 时只允许命中缓存、取消请求、MySQL 查询失败、分页游标不存在和损坏的历史 JSON。P1.1 更新发送故障断言：本轮脏队列设为字符串时，RPUSH 真正返回 WRONGTYPE，HTTP 返回 503 / `MESSAGE_ENQUEUE_UNKNOWN`，不执行 ZADD/PUBLISH，不创建缓存；缓存 key 设为字符串时，RPUSH 成功、ZADD 失败，仍尝试 PUBLISH 并返回旧接口成功，核对队列完整内容。另通过 hook 注入 PUBLISH 失败，断言队列和缓存保留；在真实 RPUSH 成功后的 hook 注入超时、取消、EOF，断言 HTTP unknown 与实际入队并存；入队确认后取消请求仍返回成功。关闭 Redis 客户端后发送返回 503 / unknown，历史返回普通依赖不可用 503。

默认 `TestLegacyMessageAcceptance` 使用 `net.Pipe` RESP 替身验证 RPUSH→ZADD→PUBLISH 顺序、RPUSH 拒绝/响应丢失时无后续副作用、缓存/通知失败不撤销成功；`TestLegacyMessagePreCanceled` 验证调用前取消不发送命令。HTTP 单元测试验证 unknown 分类优先于普通存储错误，不泄露内部错误。发送成功仍为 HTTP 200 / code 0，只代表旧 Redis 入队确认；未接入 MQ 202，也未实现重投幂等、缓存完整性修复或通知重试。由于共享 Redis 客户端可能自动重试，调用 RPUSH 后所有未获成功结果的错误均保守归为 unknown，最终 WRONGTYPE 等错误不被当作此前所有尝试均未接受的证据。

P0.6.3c.3a 的订阅单元测试覆盖缺失依赖、连接/握手失败、取消阻塞的握手、并发重复关闭空句柄；接收循环测试覆盖部分初始化失败的资源释放、关闭通道、nil 消息和 context 取消。真实订阅验收：

```powershell
go test -race -tags=integration ./database -run '^TestSubscriptionsIntegration$' -count=1 -v -timeout=8m
```

在全局群 manager 为 nil 时建立私聊及两个群订阅，核对 Redis NUMSUB 和实际消息内容；取消后与手动 Close 后等待消息通道关闭、NUMSUB 回到零；另覆盖无群空句柄及错误 Redis 密码的握手失败。此用例不覆盖完整 WebSocket 握手和会话替换。

P0.6.3c.3b 默认测试使用本地 `httptest` WebSocket 服务和注入的订阅，覆盖升级前依赖/订阅失败、无身份拒绝、升级失败的资源释放、私聊/群消息转发、无群连接、客户端关闭、订阅关闭、请求取消、连接替换及注销下线帧。通过阻塞读取 goroutine，验证 handler 必须等其结束后才返回并调用 Offline；测试 logger 写入 `io.Discard`，不读取开发配置或创建日志文件。

```powershell
go test -race -tags=integration ./service -run '^TestChatRedisIntegration$' -count=1 -v -timeout=8m
```

真实验收使用独立 MySQL/Redis、JWT 中间件和本地 WebSocket 客户端，验证 401、升级成功与实际 PubSub 转发；缺失 MySQL 或关闭 Redis 的连接请求返回 503，已建立的原连接仍可用，失败请求创建的订阅得到释放；正常关闭和无群用户关闭后 NUMSUB 归零。本用例不覆盖完整应用服务器退出、Redis 服务端重启或跨实例会话抢占。

P0.3 已恢复旧业务集成测试。执行消息存取、缓存删除/回源、好友、群成员和定时落库测试：

```powershell
go test -tags=integration ./database ./scheduledTasks -count=1 -v -timeout=15m
```

4 个数据库查询 benchmark 使用独立 fixture，准备、预检和清理不计入查询耗时。可以先固定迭代次数检查可运行性，避免自动校准多次创建 Docker 环境：

```powershell
go test -tags=integration ./database -run '^$' -bench . -benchtime=1x -count=1 -timeout=15m
```

`1x` 仅验证 benchmark 能执行并通过结果断言，不是稳定的性能基线。每个测试/benchmark 调用都有自己的 MySQL/Redis 环境，整套测试需要几分钟；不同包可以独立运行。数据库查询和旧 worker fixture 已不需要绑定全局 manager；部分验证全局依赖隔离的单元测试仍会临时修改全局变量，所以不使用 `t.Parallel()`。

## 每轮资源

`tests/testenv.Start(ctx, composeFile)` 生成随机 128 位 runId 和测试密码，使用仓库的 `docker-compose.test.yml`：

| 资源 | 隔离方式 |
| --- | --- |
| Compose 项目 | `ichat-test-<runId>`，拒绝复用已经存在的项目资源 |
| MySQL | 本轮独立容器；schema 为 `ichat_test_<runId>`，使用仅获该 schema 权限的 `ichat_test` 用户 |
| Redis | 本轮独立容器、独立数据卷；即使旧代码使用固定 `dirty:msgs` key，也不会访问开发 Redis |
| 端口 | 只绑定 `127.0.0.1`，由 Docker 动态分配，不占用开发环境固定端口 |
| 卷和网络 | 各轮独立，包含 Compose 项目标签与 `io.ichat.test.run` 标签 |
| 镜像 | MySQL 8.0 / Redis 7，固定已验证 digest，未改变开发 Compose |

Compose 项目名用于区分不同环境的资源，相关行为见 [Docker 项目名说明](https://docs.docker.com/compose/how-tos/project-name/)。此处还额外校验本轮归属标签，避免仅凭名称删除资源。

测试环境不读取应用的 `config/config.yml` 或开发 `.env`。调用 Docker 时显式传入 Compose 文件、项目名和空 env 文件，并移除继承的 `COMPOSE_*`、`ICHAT_TEST_*` 覆盖值；连接地址只从本轮容器映射端口中取得。Docker 自身的本机连接配置仍由 Docker CLI 使用。

`Config()` 仅在启动、健康检查及资源归属校验成功后返回连接参数。它返回配置副本，不修改 `config.Conf`、`utils.DB/RDS` 或全局 manager。

## 清理与失败

调用方先关闭 Redis/MySQL 客户端，再调用 `Environment.Close(ctx)`。推荐用 `t.Cleanup` 注册，并给清理分配独立的超时时间；`tests/integration/environment_test.go` 提供完整示例。

清理先列举本轮项目资源，并逐一核验两种归属标签。所有资源验证成功后，才按容器 → 卷 → 网络的顺序移除列举到的具体资源。任何标签缺失/不符、inspect 失败均返回错误，不开始删除；实际删除中途失败会返回错误，可再次调用 Close 处理剩余资源。

不执行 `FLUSHALL`、共享库删表、`docker system prune` 或无范围清理。启动部分失败仍会尝试清理本轮已创建的资源；拒绝复用的既有项目不会被接管或清理。

进程被强制终止时，Go cleanup 不保证执行。若发生这类中断，使用日志中的 runId 定位资源，核验项目标签与 run 标签后再清理；不要把整个 Docker 实例作为清理范围。Docker 无法访问时，清理错误会保留并报告，不能把未清理写成已清理。

## 当前验收范围

- 单元测试：非法 runId、开发 Compose 误传、既有项目碰撞、归属不符、inspect 异常、清理错误传播、重复 Close、启动失败及端口校验。
- 真实集成测试：同时存在的两套 MySQL/Redis 使用同名表/key 保存不同内容；删除第一套后，第二套数据仍完整；继承的开发配置不能覆盖本轮设置。
- 旧业务 fixture：每次创建 5 个用户、3 个群、明确的好友/成员关系；用户和群 ID 来自实际插入结果，消息 ID 包含本轮 runId 和递增编号。schema 校验后才迁移和写 fixture，全局 manager 在用例结束后恢复。
- 消息断言：双向会话的全部分页、同毫秒顺序、真实内容和媒体类型；队列中的原始消息集合；删除已存在的缓存 key 后从 MySQL 回源，且另一会话缓存和已存消息不受影响。
- 关系断言：精确好友/群/群成员 ID 集合、双向好友、无关系用户的空结果，防止两种查询同样返回错误结果却被判为通过。
- SyncMsg 断言：同步前数据库为空且队列有 3 条消息，同步后逐字段检查 3 条私聊/群聊及文本/图片/音频记录，队列为空；再次执行空队列同步不改变数据。直接断言任务返回值和数据库/队列状态，不依赖日志判断成功。
- 尚未包含：坏消息隔离、真实进程崩溃/COMMIT 响应丢失、单消费者/防重入约束、RabbitMQ vhost、MQ 故障恢复、生产环境验收。已覆盖旧 worker 在 DB 提交后 EVAL 发送前失败的幂等重投；其余故障窗口按开发规范后续小点推进。
