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

   当前命令使用旧模型的 `AutoMigrate` 创建或更新 `user`、`relation`、`group`、`message` 四张表；可对当前 schema 重复执行，后续可靠性 schema 将在 P2 引入版本化迁移。MySQL DDL 失败可能保留已执行的变更，该命令不承诺整批回滚。业务服务启动不再自动建表或修改 schema。

   ```bash
   go run .
   ```

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

当前业务进程启动仍要求 MySQL/Redis 可用；上述接口的故障响应不代表已实现数据库离线启动或后台恢复。消息链路依赖装配和完整群权限事务将继续分步改造；入群的群存在性/容量校验、删群关联关系清理仍待后续实现。写入收到 503 时可能存在提交结果不确定，不能据此断定数据库未写入。

旧消息同步任务由 bootstrap 显式传入消息仓储，读取、落库或队列确认失败会返回错误，由定时调度入口记录；Redis 读取失败不会被当成空队列，落库失败不会继续确认队列。当前仍采用旧 Redis List 链路，尚未实现提交后重投幂等、并发精确确认或有界批次，不能据此视为可靠消息投递已经完成。

`/auth/sendMessage` 与 `/auth/getMessageList` 使用注入的消息仓储，context 贯穿 Redis 和 MySQL 回源。发送者取自鉴权 UID，忽略请求体 `fromUser.id`；目标 ID 必须非零，`is_group` 为 0 或 1，发送类型支持 `text`、`picture`、`audio`。历史 `limit` 必须为 1–100；无效参数或不存在的分页游标返回 HTTP 200、`code: -1`。仓储缺失、Redis 故障、MySQL 回源失败或历史 JSON 解码失败返回 HTTP 503、`data.reasonCode: DEPENDENCY_UNAVAILABLE`，不回传底层错误或部分历史。

历史缓存命中仅依赖 Redis，缓存未命中时才需要 MySQL。发送仍采用旧缓存→入队→发布流程，成功响应尚不代表 MySQL 已持久化；入队失败会停止发布，但可能已经写入缓存，发布失败也可能已经入队。503 不能视为消息从未被接受，完整接受语义、重试幂等和群历史权限仍待后续阶段实现。

Redis 私聊/群消息订阅现在返回可关闭的句柄，确认订阅后才交给接收循环使用。context 取消、接收循环退出或群订阅初始化失败时会释放已建立的订阅；无群用户使用空句柄，不发送空的 SUBSCRIBE。群列表使用消息仓储自身的 DB。WebSocket 握手前的依赖检查、handler 整体注入及所有 goroutine 的退出等待仍待下一小点完成。
