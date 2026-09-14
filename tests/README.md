# 独立测试环境

默认单元测试不启动外部服务：

```powershell
go test ./...
```

P0.2 的环境隔离验收需要本机 Docker 引擎已启动、Docker Compose 可用。首次执行可能需要下载测试镜像：

```powershell
go test -tags=integration ./tests/integration/... -count=1 -v -timeout=12m
```

这条命令自动创建、等待就绪并清理测试资源，不需要先手动执行 `docker compose up`。当前只运行基础设施隔离验收，旧业务集成测试仍等待 P0.3 的 fixture 改造。

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
- 尚未包含：旧消息业务 fixture、RabbitMQ vhost、MQ 故障恢复、生产环境验收。这些按开发规范后续小点推进。
