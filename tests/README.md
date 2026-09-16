# 独立测试环境

当前验证工具链为 **Go 1.27.1**。运行前按 [README 工具链说明](../README.md#go-工具链) 设置当前终端的 `GOTOOLCHAIN=go1.27.1`，并用 `go version` 核对实际版本。

默认单元测试不启动外部服务：

```powershell
go test ./...
```

P0.2 的环境隔离验收需要本机 Docker 引擎已启动、Docker Compose 可用。首次执行可能需要下载测试镜像：

```powershell
go test -tags=integration ./tests/integration/... -count=1 -v -timeout=12m
```

这条命令自动创建、等待就绪并清理测试资源，不需要先手动执行 `docker compose up`。

P0.5 的迁移命令验收同样使用独立环境：

```powershell
go test -tags=integration ./cmd/migrate -count=1 -v -timeout=8m
```

覆盖实际命令入口的退出码：连接失败、DDL 失败、首次创建四张旧表、重复迁移保留用户及消息；Redis 配置不可用时迁移仍成功。默认单元测试另行覆盖配置缺失/损坏后的非零退出，不连接数据库。迁移测试不读取开发配置，不对开发数据库执行迁移。

P0.3 已恢复旧业务集成测试。执行消息存取、缓存删除/回源、好友、群成员和定时落库测试：

```powershell
go test -tags=integration ./database ./scheduledTasks -count=1 -v -timeout=15m
```

4 个数据库查询 benchmark 使用独立 fixture，准备、预检和清理不计入查询耗时。可以先固定迭代次数检查可运行性，避免自动校准多次创建 Docker 环境：

```powershell
go test -tags=integration ./database -run '^$' -bench . -benchtime=1x -count=1 -timeout=15m
```

`1x` 仅验证 benchmark 能执行并通过结果断言，不是稳定的性能基线。每个测试/benchmark 调用都有自己的 MySQL/Redis 环境，整套测试需要几分钟；不同包可以独立运行。包内旧测试仍需临时绑定全局 manager，所以不使用 `t.Parallel()`。

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
- SyncMsg 断言：同步前数据库为空且队列有 3 条消息，同步后逐字段检查 3 条私聊/群聊及文本/图片/音频记录，队列为空；再次执行空队列同步不改变数据。日志只写临时目录，不依赖日志判断成功。
- 尚未包含：旧 worker 故障重投幂等、错误返回、并发确认、RabbitMQ vhost、MQ 故障恢复、生产环境验收。空队列再次执行成功不代表 DB 提交后重投幂等已经实现；这些按开发规范后续小点推进。
