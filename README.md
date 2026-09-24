# iChat

使用 Go 开发的 Web 消息通讯系统后端，使用 MySQL 和 Redis 存储数据。支持添加好友、创建或加入群聊，以及私聊和群聊。

## Go 工具链

项目使用 Go **1.27.1** 作为开发和测试基线，`go.mod` 要求至少该版本。已有 Go 1.21+ 且 `GOTOOLCHAIN=auto` 时，在本项目运行 Go 命令会按需下载符合要求的工具链；不会替换系统安装的 Go。Go 官方的 [发布与支持政策](https://go.dev/doc/devel/release) 和 [工具链选择规则](https://go.dev/doc/toolchain) 说明了相关行为。

为严格复现本项目的验证版本，在当前终端指定工具链：

```powershell
$env:GOTOOLCHAIN = 'go1.27.1'
go version
go test ./...
```

Linux/macOS：

```bash
export GOTOOLCHAIN=go1.27.1
go version
go test ./...
```

`go version` 应显示 `go1.27.1`。`go.mod` 声明的是最低版本，默认 `auto` 模式可以使用本机已安装的更新版本；精确复现时使用上述环境变量即可，无需修改全局 `go env -w` 设置。首次下载需要网络和 Go 缓存写入权限，离线环境需预先安装该版本。

默认单元测试不需要 MySQL/Redis。真实集成测试及 benchmark 的运行方式见 [测试说明](tests/README.md)。

## 使用 Docker Compose 启动依赖

项目中的 Go 服务运行在宿主机，MySQL 和 Redis 由 Docker Compose 启动并使用命名卷持久化数据。

1. 创建本地配置：

   ```powershell
   Copy-Item .env.example .env
   Copy-Item config/config.example.yml config/config.yml
   ```

   Linux/macOS 可使用：

   ```bash
   cp .env.example .env
   cp config/config.example.yml config/config.yml
   ```

   在本地 `.env` 中填写非空的 `MYSQL_ROOT_PASSWORD` 和 `REDIS_PASSWORD`，并将对应密码填入 `config/config.yml`。Compose 不提供默认密码，缺失或为空时会拒绝启动。

2. 启动 MySQL 和 Redis，并等待两项服务变为 `healthy`：

   ```bash
   docker compose up -d
   docker compose ps
   ```

3. 显式执行数据库迁移，成功后再启动 Go 服务：

   ```bash
   go run ./cmd/migrate
   ```

   迁移命令沿用应用配置（支持 `ICHAT_CONFIG_FILE` 和 `ICHAT_MYSQL_*` 环境变量），只连接 MySQL；不需要 Redis。成功输出 `legacy schema migration completed` 并以 0 退出；配置、连接或迁移失败输出错误并非零退出，此时应先排查错误，再启动服务。

   当前命令使用旧模型的 `AutoMigrate` 创建或更新 `user`、`relation`、`group`、`message` 和 `legacy_message_quarantine` 五张表；可对当前 schema 重复执行，后续可靠性 schema 将在 P2 引入版本化迁移。MySQL DDL 失败可能保留已执行的变更，该命令不承诺整批回滚。业务服务启动不再自动建表或修改 schema。

   P1.3.2 新增 `legacy_message_quarantine`，升级代码后须先重新执行迁移。旧 worker 每批最多读取 128 条，逐条提交消息或隔离记录，只确认已提交的连续前缀。隔离表长期保留原始字节（longblob）、SHA-256 摘要、原因与时间；完全相同的原文合并为一条审计记录，不代表物理投递次数。隔离成功不代表业务发送成功；基础设施故障或隔离写入失败时保留待处理队列。缺少该表会阻止 worker 推进，存储健康接口只检查连接，不能证明 schema 已准备好。

   可通过 SQL 查询 `legacy_message_quarantine` 的 `payload_hash`、`reason_code`、`reason_detail`、`created_at` 和 `HEX(raw_body)` 审计原文；原文可能包含消息正文，应按数据库访问权限管理。重复消费已隔离原文只确认既有隔离结果，不自动再次执行业务；本阶段没有隔离记录清理、自动重放或管理接口，后续受控重放流程不在旧 worker 中实现。

   旧消息 worker 现在默认关闭。使用旧队列时，在唯一一个负责落库的应用实例配置中显式开启：

   ```yaml
   LEGACY_WORKER:
     enabled: true
   ```

   也可在 PowerShell 当前终端设置 `$env:ICHAT_LEGACY_WORKER_ENABLED = 'true'`，然后启动：

   ```bash
   go run .
   ```

   未配置或设置为 false 时仍启动 HTTP/健康探测，但不启动消费任务；旧发送接口仍会入队，因此没有其他已启用 worker 时消息不会异步落库。开启后每 60 秒触发一次，上一轮仍在运行则跳过本次触发，不积压等待任务。任务 panic 后可恢复后续调度，正常返回时等待当前任务退出后才关闭数据库/Redis。启用 worker 的实例必须取得同一 Redis DB 中 `dirty:msgs:worker-owner` 的排他注册；第二个实例启动失败。锁无 TTL，异常退出不会自动接管；恢复方式及 Redis 数据丢失边界见 [旧 worker 恢复说明](docs/legacy-worker-recovery.md)。

   服务默认监听 `http://localhost:8080`。

4. 停止容器：

   ```bash
   docker compose down
   ```

数据库数据保存在 `mysql_data` 和 `redis_data` 命名卷中。若确实要同时删除数据，可使用 `docker compose down -v`。

端口和密码可在 `.env` 中修改；修改密码或端口后，也要同步修改 `config/config.yml`。应用配置还支持以 `ICHAT_` 开头的环境变量覆盖，例如 `ICHAT_MYSQL_HOST`、`ICHAT_MYSQL_PASSWORD` 和 `ICHAT_REDIS_ADDR`。

## 鉴权与注销

`JWT.key` 必须非空，`JWT.tokenHourLifeSpan` 必须为可表示的正整数小时数；配置无效时服务启动报错。签发和校验使用 HS256，用户 ID 必须是正整数，token 必须有有效的过期时间。

受保护接口和 `/user/logout` 使用同一个鉴权服务。token 缺失、无效、过期或已封禁时返回 HTTP 401；Redis 黑名单读写失败或鉴权依赖缺失时返回 HTTP 503。错误响应保留 `code/msg/data`，`data.reasonCode` 分别为 `UNAUTHENTICATED` 或 `DEPENDENCY_UNAVAILABLE`，不向客户端暴露底层 Redis 错误。

注销只有在黑名单写入成功后才发送 WebSocket 下线通知、更新本地在线状态并返回 HTTP 200、`code: 0`。收到 503 不能视为注销成功；写入结果可能不确定。已封禁 token 再次访问受保护接口或注销接口返回 401。黑名单 TTL 对齐 token 剩余有效期，继续使用原有 key 格式。

注册和登录使用显式传入的用户仓储。仓储缺失、MySQL 查询/写入失败或请求取消时返回 HTTP 503、`data.reasonCode: DEPENDENCY_UNAVAILABLE`，不把存储故障当成密码错误，也不返回 SQL 原始错误。账号不存在、密码不正确、重复手机号及注册密码不一致继续使用原有 HTTP 200、`code: -1` 的业务失败响应。登录只有在用户查询、密码校验和 token 签发均成功后才标记在线。

联系人列表 `/auth/getContacts`，以及 `/auth/contact/` 下的 `searchFriends`、`loadGroups`、`loadGroupUsers` 使用显式传入的查询仓储。缺失仓储、查询失败或请求取消时返回 HTTP 503、`data.reasonCode: DEPENDENCY_UNAVAILABLE`；联系人列表只有在好友和群两部分都查询成功后才返回成功，避免把查询失败误认为没有群聊。正常空结果仍返回空数组。

`/auth/contact/` 下的 `addFriend`、`createGroup`、`joinGroup`、`deleteGroup` 也使用显式传入的仓储和请求 context。存储故障返回 HTTP 503，不暴露 SQL 错误；目标用户不存在、添加自己、关系重复及群不存在/无权删除返回 HTTP 200、`code: -1`。入群/删群拒绝零、负数和溢出的群 ID。建群记录和创建者成员关系使用同一个事务，双向好友关系也在事务中写入；删群只有实际删除自己的群后才返回成功。

业务进程使用延迟连接的 MySQL 连接池，MySQL 离线不会阻止 HTTP 启动；Redis 启动检查仍须成功。访问 MySQL 的接口在连接失败时返回 503，后续操作会由连接池重新尝试连接。API 连接设置了 3 秒拨号超时、5 秒读/写超时，这些不是整个请求的总超时。当前按项目 MySQL 8 环境跳过服务端版本探测；独立迁移命令仍立即连接并检查数据库，不会因 API 的延迟连接而跳过迁移错误。隔离集成测试已验证真实 MySQL 离线启动及两次恢复，详情见 [测试说明](tests/README.md)；尚未覆盖网络黑洞或 API 进程崩溃，当前不代表实现了 MQ accepted 语义。

无需 JWT 的健康检查接口：

| GET 路径 | 含义 |
| --- | --- |
| `/health/live` | HTTP 进程存活，返回 200，不访问外部依赖 |
| `/health/ready/storage` | 最近的 MySQL Ping 成功返回 200；尚未探测、失败、停止或结果过期返回 503 |
| `/health/ready/accept` | 当前固定返回 503、`MQ_ACCEPTANCE_NOT_CONFIGURED`；待 P3 接入 MQ 确认通路后实现接受就绪 |

存储探测立即在后台开始，每次完成后等待 5 秒重试，单次 context 超时 2 秒；串行执行，不阻塞 HTTP 启动。成功结果超过 9 秒失效；就绪响应禁止缓存，不返回底层连接错误。存储就绪仅检查连接，不检查 schema、迁移版本或写入权限。探测使用已有连接池，bootstrap 返回时先取消并等待探测退出，再关闭连接池；尚未实现全服务器信号处理和 WebSocket 优雅停机。当前不要把接受就绪接口作为旧链路的流量接入条件。

完整群权限事务将继续分步改造；入群的群存在性/容量校验、删群关联关系清理仍待后续实现。写入收到 503 时可能存在提交结果不确定，不能据此断定数据库未写入。

旧消息同步任务由 bootstrap 显式传入消息仓储，读取、落库或队列确认失败会返回错误，由定时调度入口记录；Redis 读取失败不会被当成空队列，落库失败不会继续确认队列。当前仍采用旧 Redis List 链路，尚未实现提交后重投幂等、并发精确确认或有界批次，不能据此视为可靠消息投递已经完成。

`/auth/sendMessage` 与 `/auth/getMessageList` 使用注入的消息仓储，context 贯穿存储调用。发送者取自鉴权 UID，忽略请求体 `fromUser.id`；目标 ID 必须非零，`is_group` 为 0 或 1，发送及历史类型支持 `text`、`picture`、`audio`。历史 `limit` 必须为 1–100；无效参数或不存在/不属于当前会话的分页游标返回 HTTP 200、`code: -1`。历史缺失鉴权身份返回 401；非当前群成员或群不存在（含软删除）返回 HTTP 403、`reasonCode: GROUP_HISTORY_FORBIDDEN`。MySQL 不可用或历史解码/媒体类型异常返回 HTTP 503、`data.reasonCode: DEPENDENCY_UNAVAILABLE`，不回传底层错误或部分历史。

P1.6 起历史直接查询 MySQL，只返回已落库消息，不读取、迁移或回填 Redis 历史缓存。按 `(time_stamp, identifier, id)` 降序取一页，响应页内逆序供展示；游标只用消息 ID 定位当前会话中的存储记录。每页在独立的可重复读只读事务中校验群成员和群存续状态，再读取游标及历史；当前成员可读入群前的全部历史。权限在本页数据库快照建立时判定，之后离群不撤回已开始的读取，下一页重新检查。尚未实现跨页快照或按提交顺序的 `seq`，并发晚落库消息仍需重新查询，不能把旧时间戳分页当作可靠同步游标。

发送仍为旧 Redis 入队→缓存→发布流程：入队确认后缓存/发布失败不撤销成功；调用入队后结果不确定时返回 503 / `MESSAGE_ENQUEUE_UNKNOWN`。发送成功不代表 MySQL 已持久化，历史需等待启用的 worker 落库；worker 默认关闭时待处理消息会持续积压。历史仓储不依赖 Redis，但生产 HTTP 鉴权仍需要 Redis 黑名单服务。完整 MQ 接受语义、客户端重试幂等、发送权限事务和群推送权限仍待后续阶段实现。

Redis 私聊/群消息订阅返回可关闭的句柄，逐一核对所请求频道的 SUBSCRIBE 确认后才交给接收循环使用；重复或未知频道的确认视为初始化失败。context 取消、接收循环退出或群订阅初始化失败时会释放已建立的订阅；无群用户使用空句柄，不发送空的 SUBSCRIBE。群列表使用消息仓储自身的 DB。

P1.7 为每个非空订阅增加取消感知的转发协程。关闭时丢弃尚未交付的通知、关闭 Redis 连接并排空底层通道，等待内部消息读取结束及自有协程退出；即使消费者停止读取、缓冲区已满，也不必等待 go-redis 默认的 60 秒通道发送超时。重复/并发 Close 安全，取消 context 也触发相同清理。初始连接、写入和重连仍受客户端超时控制，本轮不承诺网络黑洞下的全流程硬截止时间，也不增加通知重放或群成员变化后的重新授权。

`/auth/chat` 和 `/auth/getws` 的 WebSocket handler 显式接收订阅提供者、下线回调与 logger。私聊和群订阅在 HTTP 升级之前建立，依赖缺失或订阅失败返回 HTTP 503、`DEPENDENCY_UNAVAILABLE`，失败重连不会替换已有连接。成功连接结束时先取消 context、关闭 socket 和订阅，再等待读写、接收及心跳任务结束；只有仍为当前会话的连接才执行本地下线。同用户新连接替换旧连接仅保证当前进程内的会话归属，不代表跨实例 session fencing 或消息重连恢复已完成。
