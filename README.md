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
