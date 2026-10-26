# P4.2.5.1：隔离审计只读查询与选择

P4.2.5 拆为四个小点，每次单独实现和验收：

| 小点 | 范围 | 状态 |
| --- | --- | --- |
| P4.2.5.1 | 列表、详情、精确选择与完整审计校验 | 完成，本文 |
| P4.2.5.2 | 保持业务身份和 hash，确认式重发原命令并记录独立 replay attempt | 完成，见[受控重发](mq-quarantine-replay.md) |
| P4.2.5.3 | 核验匹配身份/hash 的规范业务终态，再事务更新 resolved | 完成，见[终态核验](mq-quarantine-resolve.md) |
| P4.2.5.4 | confirm/return/未知结果、发布后写状态前退出、并发操作及终态核验故障验收 | 完成，见[重放验收](mq-quarantine-replay-acceptance.md) |

本次完成 `cmd/mqadmin quarantine list|show` 和借入 SQL pool 的 `QuarantineRepository.List/Get`。操作仅执行无锁 SELECT；不领取 MQ 消息、修改审计状态、迁移 schema 或启动后台服务。现有 `mqadmin init/check` 保持原入口。

## 使用

在项目根目录运行：

```powershell
go run ./cmd/mqadmin quarantine list
go run ./cmd/mqadmin quarantine list --state all --limit 20
go run ./cmd/mqadmin quarantine list --state replay_pending --limit 20
```

复制列表返回的完整 transport ID，作为精确选择；`show` 没有“最新一条”或批量选择隐式默认值。例如在 PowerShell 中：

```powershell
$auditId = '列表返回的完整 mq:v1: 加 64 位小写十六进制 ID'
go run ./cmd/mqadmin quarantine show --transport-id $auditId
go run ./cmd/mqadmin quarantine show --transport-id $auditId --include-raw
```

默认使用 `ICHAT_CONFIG_FILE`；未设置时使用相对项目根目录的 `config/config.yml`。也可显式指定 `--config path`，或用 `--config=` 仅读取进程环境。仅取 MYSQL.user/password/host/port/dbname，环境变量 `ICHAT_MYSQL_USER/PASSWORD/HOST/PORT/DBNAME` 优先，显式空值不会退回文件值。不自动读取 Compose `.env`，不要求 Redis、RabbitMQ 或运行时角色配置有效。凭证不通过命令参数传入。

一次查询总 context 为 15 秒；拨号 3 秒，单次 socket 读写 5 秒。命令拥有并最终关闭自己的单连接 SQL pool；仓储借入 pool，不负责关闭。参数校验先于配置读取和连接创建。配置、SQL 和输出错误隐藏底层文本；错误非零退出，空列表成功输出空数组。查询错误不输出部分页面。

## 列表与分页

`list` 默认 `--state open --limit 50`。state 可取 open、replay_pending、resolved、all；limit 范围 1—100。返回 JSON：

```json
{"records":[],"audit_verified":false}
```

非空记录包含 transport_id、可空业务身份/hash、error_code、state、replay_attempt_id、UTC 创建/解决时间和 body/audit 字节数。所有身份以字符串输出，避免 JavaScript 数字精度丢失。不返回原文、headers 或错误详情中的任意文本。

按 `transport_id` 的 ascii_bin 升序读取，`--after` 为不含该记录的游标，最多查询 limit+1 行元数据。有后页时返回 `next_after`，使用**相同 state 筛选**和该值继续；末页省略该字段。不是按创建时间排序。各页是独立的实时视图；并发插入到游标之前或修改状态时，跨页遍历不提供固定快照保证。

列表只查询元数据和 `OCTET_LENGTH`，不载入完整大文档，也不证明其可重放。`audit_verified:false` 明示此边界；文档内容损坏但元数据可读取的记录仍可列出。选择后必须由详情重新核验，后续重发还必须再次读取并核验，不能把旧列表作为授权或并发锁。

## 详情与原文

`show --transport-id` 只接受精确 `mq:v1:` 加 64 位小写十六进制的身份。通过一次无锁 SELECT 读取同一行的完整字段，复用 [P4.2.4.2](mq-quarantine-audit.md) 校验：

- 恢复带版本审计文档，校验源快照、带类型 headers、稳定内容和观察格式。
- 对比完整 raw_body、受控错误分类、固定审计标记、微秒创建时间及全部可空业务身份/hash。
- 核验重放/解决字段组合，确认文档、SQL 主键及请求的 transport ID 完全一致。

默认详情只输出元数据、format 和 `audit_verified:true`。`--include-raw` 额外输出 `raw_body_base64` 和 `audit`：前者完整保存无效 UTF-8、NUL 等源字节，后者为既有带类型 v1 审计 JSON，恢复后保留首份观察和精确稳定内容，不将 AMQP int64/decimal/binary 转成普通 JSON 数字。不做正文修复、截断或重新规范化业务命令。

完整性校验不证明记录来源可信，不代表业务已成功，也不等于可直接重发。业务 wire 无效的审计仍可查看，业务身份全 NULL。损坏或主键冲突返回失败且不覆盖原行；缺失记录明确报 not found，不能据此推断 MQ 中没有命令。

## 验证与下一项

```powershell
go test -mod=readonly -race ./database ./cmd/mqadmin -run '^TestQuarantine(Query|CLI|SQL)' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=120s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration ./cmd/mqadmin -run '^TestQuarantineCLIIntegration$' -count=1 -v -timeout=7m
```

默认测试覆盖参数先行、分页/空结果、完整性/主键冲突、取消/SQL/遍历/关闭/输出错误、不输出部分页面、原文显式展示、配置覆盖和资源释放。SQL adapter 拒绝事务/非预期操作，检查查询没有 FOR SHARE。

真实 suite 使用独立 MySQL/Redis fixture，不启动 RabbitMQ。保存六条 wrapper/DLX 协议输入（有/无有效业务身份），设置 fixture 的 open/replay_pending/resolved 三种状态；经实际命令入口查询全部状态和逐条游标、校验完整原文与带类型文档，逐字段比较前后所有审计列，业务五表保持空。故意破坏一个 fixture 文档后仍能列表发现，详情拒绝且不覆盖；最后复核 schema 指纹及清理。DLX 文档为已验证协议夹具，本项不冒充真实 broker DLX 或重发验收。实际运行记录见[实施进度](message-reliability-mq-progress.md)。

后续 [P4.2.5.2：确认式重发与独立 replay attempt](mq-quarantine-replay.md)、[P4.2.5.3：终态核验与 resolved](mq-quarantine-resolve.md)及[重放故障验收](mq-quarantine-replay-acceptance.md)已完成，P4.2.5 范围收口。确认重发不能直接标记 resolved，提交或发布未知继续保留责任。[P5.1.1 Outbox 领取仓储](outbox-claim.md)已完成；[P5.1.2 token 条件结算](outbox-settlement.md)已完成；[P5.1.3 通知执行循环](outbox-execution.md)已完成；[P5.1.4 生产角色与生命周期](outbox-lifecycle.md)已完成；下一项 **P5.1.5：故障验收**。P4 整体及完整默认后台角色仍依赖 Outbox。
