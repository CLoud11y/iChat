# iChat

使用 Go 开发的 Web 消息通讯系统后端，使用 MySQL 和 Redis 存储数据。支持添加好友、创建或加入群聊，以及私聊和群聊。

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

3. 启动 Go 服务：

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
