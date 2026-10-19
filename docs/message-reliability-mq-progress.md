# 消息可靠性改造实施记录

依据：[开发规范](message-reliability-mq-development.md)第 12 节。代码起点：`cb1c8bc6ff1f0c5d4f0461ed53c74eded72a98bb`。

执行约定：每次只实施一个小点，完成代码审查和对应测试后提供 commit message；用户确认后才继续下一个小点。不自动提交。阶段完成必须满足原规范验收条件。

当前总进度：**P0.1—P0.6、P1 和 P2 仓储范围已验收收口；P3.1—P3.4.2 已完成固定版本、正式拓扑/可靠 DLX、有界 publisher/I/O、unknown 与 supervisor 抖动退避/单调 epoch/新连接只读复核，真实 broker 重启恢复及无自动重发已验收；P3.5.1 已完成应用配置；P3.5.2 已完成独立消费者连接、每 worker 有界消费 channel/prefetch、顺序 delivery、原 channel 单条手动 ACK/Reject，以及真实重投/ACK 丢帧/故障隔离；P3.5.3.1 已完成严格运行模式/角色计划/启动检查；P3.5.3.2.1 已完成消费者有界参数、独立操作超时和按角色生成的无 I/O MQ 依赖计划。下一项为 P3.5.3.2.2 按角色受控创建 MQ 连接与失败清理，开始前继续细分。当前业务主程序仅运行 legacy；新事务与 publisher 尚未接入旧入口、HTTP 202 或业务消费任务，不代表端到端可靠性改造完成。旧 worker 仍依赖不自动过期的单消费者注册及人工恢复，依赖共享 Redis 保留锁状态。** 原总清单的 RabbitMQ 测试 vhost 已按既定安排在 P3.1 完成。以下各小点的“当前”“未覆盖”等表述是当时的历史记录，最新结论以文末对应实施记录为准。

## 开发拆分

下表每个编号代表一次实施边界；复杂项在开始前继续细分，不能将一整行阶段一次性修改完。

| 阶段 | 按顺序实施的小点 |
| --- | --- |
| P0 | P0.1 安全单元测试入口；P0.2 独立 MySQL/Redis 测试资源与归属校验；P0.3 用本轮 fixture 恢复旧集成测试和 benchmark；P0.4 核验并固定 Go 工具链；P0.5 独立迁移命令与失败退出；P0.6 完善依赖注入、缺失依赖处理和 bootstrap 测试。MQ 测试 vhost 在 P3 引入 broker 时创建 |
| P1 | P1.1 入队接受顺序及错误语义；P1.2 有界读取和队首原子核对；P1.3 DB 幂等及坏记录隔离；P1.4 worker 启用约束和防重入；P1.5 鉴权发送身份；P1.6 历史完整性、群权限和媒体类型；P1.7 PubSub 生命周期；P1.8 补齐 L1—L12 回归及跨系统崩溃测试 |
| P2 | P2.1 DTO、规范化和身份/hash；P2.2 schema 与唯一约束；P2.3 幂等命令结果；P2.4 权限事务和锁顺序；P2.5 seq、消息和 Outbox 原子提交；P2.6 状态仓储；P2.7 同步/历史仓储 |
| P3 | P3.1 核验 broker/client 版本和隔离 vhost；P3.2 本地拓扑及可靠 DLX；P3.3 有界 publisher 和 confirm/return 关联；P3.4.1 I/O/关闭硬界与 unknown；P3.4.2 supervisor 退避重连、单调 epoch 和拓扑复核；P3.5.1 应用配置与纯构造；P3.5.2.1 连接内核与独立消费者基础生命周期；P3.5.2.2 有界消费 channel / 手动 ACK 传输所有权；P3.5.3.1 模式配置/角色计划/启动检查；P3.5.3.2.1 消费者参数与纯 MQ 依赖计划；P3.5.3.2.2 受控 MQ 连接创建/失败清理；P3.5.3.3 生命周期装配与角色能力核验；P3.5.4 接受 readiness 与持续拓扑核验；P3.6 隔离环境接入 HTTP 202 |
| P4 | P4.1 消费事务与提交后 ACK；P4.2 错误分类和数据库故障退避；P4.3 可靠隔离转移；P4.4 隔离审计 worker；P4.5 受控重放 CLI；P4.6 提交不确定和退出恢复 |
| P5 | P5.1 Outbox lease 领取；P5.2 token 条件完成和重试；P5.3 通知协议与内容授权；P5.4 WS 订阅及慢客户端处理；P5.5 session fencing；P5.6 ACK 持久化与确认 |
| P6 | P6.1 状态接口契约；P6.2 会话列表及补拉/历史接口；P6.3 模拟客户端发送恢复；P6.4 本地幂等和连续游标；P6.5 重连及周期同步；P6.6 分页/乱序/本地清空测试；P6.7 前端接入文档 |
| P7 | P7.1 明确开发资源的切换脚本；P7.2 停旧入口/cron 并切换；P7.3 删除旧队列路径及旧协议；P7.4 README、配置和压测指南同步 |
| P8 | P8.1 发布故障矩阵；P8.2 消费/隔离/Outbox 故障矩阵；P8.3 WS/同步恢复矩阵；P8.4 三节点配置；P8.5 单节点及多数故障演练；P8.6 loadtest 对账和容量基线；P8.7 版本、环境与交付报告 |

## P0.1：安全单元测试入口

状态：实现和代码审查完成；测试结果见下方。本小点不代表整个 P0 完成。

### 改动

- 配置改为显式 `config.Load()`，使用独立 Viper 实例，返回读取/解析错误，不在包导入时读取本机配置。
- MySQL/Redis 改为显式构造器，返回错误；失败连接释放资源，不在导入时联网。
- 四个 manager 改为接收依赖的构造器；旧服务在 `main.run()` 中按顺序装配。
- 旧迁移暂时由 `run()` 显式调用并检查错误；连接构造器本身不迁移。后续 P0.5 再从业务启动中移走迁移。
- 启动失败返回错误并非零退出；正常返回路径先停 cron、等待其任务退出，再关闭外部连接。
- 保留纯消息排序测试；将 5 个依赖真实数据的 database 测试、4 个 benchmark 及 1 个 SyncMsg 测试置于 `integration` 标签下。
- 上述旧集成用例在 fixture 完成前显式 `Skip`，即使手动启用标签也不会访问固定用户数据。跳过不算业务测试通过。
- 日志测试只在临时目录写文件并清理资源，补充配置错误/独立读取、无隐式初始化、manager 装配、Redis 取消和迁移缺失 DB 的测试。

### 代码审查

- 检查配置读取 → 连接构造 → 迁移 → manager 装配 → cron/HTTP 的顺序，避免 manager 捕获尚未初始化的 DB/Redis。
- 检查异常分支，修复 GORM 自动 Ping 失败后可能残留连接池的释放窗口。
- 检查退出顺序，避免连接已关闭而 cron 仍在执行。
- 检查所有旧测试及 benchmark 的 build tag/Skip，确保纯测试仍执行；日志测试清理后可重复运行。
- 本小点复审未发现新增阻断问题。旧 handler 和部分 manager 之间仍使用全局依赖，尚未完成可独立装配的整个业务层；这部分列入后续 P0.6/P2。

### 验证

环境：Windows amd64，`go1.23.3`，现有 `go.mod` 为 `go 1.21.3`；本轮未升级工具链、添加依赖或启动外部服务。

测试进程设置 `GOCACHE=<仓库>/.gocache`；设置 `ICHAT_CONFIG_FILE=<仓库>/__unit_test_config_must_not_be_loaded__.yml`（不存在的路径），验证单元测试不依赖开发配置。配置读取用例自行使用临时文件。

| 命令 | 结果 |
| --- | --- |
| `go test ./... -count=1` | 通过（初次验证） |
| `go test ./... -count=2 -shuffle=on` | 通过，包含最终连接清理和取消测试；验证重复及乱序执行 |
| `go vet ./...` | 通过 |
| `go test -tags=integration ./database ./scheduledTasks -run 'Test(SaveAndLoadMsg\|DeleteMsg\|GetGroups2\|SearchFriends\|GetGroupUsers\|SyncMsg)$' -bench . -benchtime=1x -v` | 6 个旧测试和 4 个 benchmark 均明确跳过；仅验证隔离护栏，不是集成验收 |
| `go test -race ./... -count=1` | 通过；Windows 本机使用已有 MinGW GCC 执行，未跳过 race |
| `git diff --check` | 通过 |

未覆盖：真实 MySQL/Redis 启动与迁移、独立 fixture、MQ、数据库离线 API 接受语义、端到端故障恢复。当前业务启动仍要求 MySQL/Redis 可用；原规范 P0 勾选项保持未完成。

建议 commit message：`refactor(test): isolate unit tests from implicit infrastructure initialization`

P0.1 已提交：`3c6439c`。用户已确认继续 P0.2。

## P0.2：独立 MySQL/Redis 测试资源与归属校验

状态：实现和代码审查完成；最终验证结果见下表。使用说明：[独立测试环境](../tests/README.md)。

### 改动

- 新增专用 `docker-compose.test.yml`，并添加对应 gitignore 例外；开发 Compose 保持原样。
- 每轮创建随机 runId 对应的独立 Compose 项目、MySQL schema、Redis 实例、数据卷及网络；端口仅绑定本机且动态分配。
- 应用测试连接使用本轮 schema 的专用 MySQL 用户；连接密码每轮生成，不读取开发配置/凭证。
- 新增 `tests/testenv`：显式启动、健康检查、取得本轮连接配置、归属验证和清理；默认单元测试导入它不会启动 Docker。
- 先检查项目未被占用，再创建本轮资源；检查所有资源的项目标签和 runId 标签后才允许删除。不会使用全局清库、删表或 Docker prune。
- 新增独立环境的真实集成验收，旧业务测试的 Skip 保留，等待 P0.3 替换固定用户/群 fixture。

### 代码审查

- 检查配置来源：显式文件/项目名/空 env 文件，清除继承的 Compose 和测试覆盖值；禁止误传开发 Compose 文件。
- 检查清理范围：只访问格式合法的随机 runId 项目，全部归属核验先于删除；发现标签不符或 inspect 错误就返回错误。
- 检查失败路径：项目碰撞时不接管/删除原项目；启动部分失败仍保留本轮清理责任，使用独立超时清理。
- 检查生命周期：关闭客户端后移除容器/卷/网络，Close 可重复调用；强制杀死测试进程不保证自动清理，限制已写入使用说明。
- 检查修改范围：本轮只改测试基础设施及文档，没有推进 P0.3 或修改消息业务逻辑。

### 实测环境与结果

- Go：`go1.23.3 windows/amd64`；Docker Engine：`28.4.0`；Compose：`2.39.2-desktop.1`。
- MySQL 实测：`8.0.44`；Redis 实测：`7.4.9`。
- MySQL 固定 digest：`sha256:f37951fc3753a6a22d6c7bf6978c5e5fefcf6f31814d98c582524f98eae52b21`。
- Redis 固定 digest：`sha256:6ab0b6e7381779332f97b8ca76193e45b0756f38d4c0dcda72dbb3c32061ab99`。
- 首轮 runId：`026942674dac8fc9be824ea41dac6f49`、`7cec4d78e2923730e62c8ce98f4a6985`；真实隔离测试通过，耗时 40.99 秒；之后按测试标签查询，容器/卷/网络均无残留。
- 固定 digest 后的最终验收 runId：`2e684a9d0ca79b4850ee18dc9d87162a`、`95fe57c76e0100b9141383776feba01c`；创建、隔离及清理全部通过，耗时 39.33 秒。

| 命令 | 结果 |
| --- | --- |
| `go test ./... -count=2 -shuffle=on` | 通过 |
| `go vet ./...` | 通过 |
| `go vet -tags=integration ./tests/...` | 通过 |
| `go test -race ./tests/testenv -count=1` | 通过 |
| `go test -tags=integration ./tests/integration -run '^TestEnvironmentIsolation$' -count=1 -v -timeout=12m` | 首轮及固定镜像摘要后的最终验收均通过 |
| `git diff --check` | 通过 |

测试使用仓库 `.gocache`；真实测试经授权访问本机 Docker。两套环境使用同名 SQL 表和 Redis key，但数据不同；清理第一套不影响第二套的数据。未运行原开发数据测试，未启动应用服务或修改开发数据。

建议 commit message：`test: add isolated MySQL and Redis environments with ownership checks`

P0.2 已提交：`7690f69`。用户已确认继续 P0.3。

## P0.3：用独立 fixture 恢复旧集成测试和 benchmark

状态：实现、代码审查和本小点测试均已完成，已提交为 `c2b4412`。

### 改动

- 新增仅在 `integration` 构建中可用的 `tests/testenv/legacy.go`；复用 P0.2，每个测试/benchmark 调用拥有独立环境，迁移前核对实际 schema 与本轮 runId。
- 创建 5 个用户、3 个群及明确的好友/成员关系，所有用户/群 ID 来自 MySQL 插入结果；消息使用非空、带 runId 的唯一 ID，不再依赖用户 1/2/9/18 或群 2 等固定开发数据。
- 恢复原 5 个 database 集成测试和 1 个 SyncMsg 集成测试，删除 P0.1 的临时 Skip。
- 消息测试逐字段校验队列、双向分页、同毫秒排序及媒体字段；缓存删除测试必须先存在目标 key，再断言回源内容、另一会话缓存和数据库记录均正确。
- 好友/群测试断言 fixture 的精确 ID 集合及空结果，不以日志或两种查询结果相等作为唯一依据。
- SyncMsg 测试对照同步前后 MySQL 完整记录和 Redis 队列，覆盖私聊、群聊及三种媒体；同一用例再次执行空队列不改变已存结果。
- 恢复 4 个 benchmark，初始化/预检/清理排除在计时之外，计时前后均检查结果；`-benchtime=1x` 只做可运行性验收。

### 代码审查

- 确认 fixture 创建成功后才绑定遗留全局 manager，cleanup 恢复原值，再关闭客户端，最后校验归属并清理环境；不使用包内并行测试。
- 确认默认测试仍不启动外部服务，所有恢复后的真实测试和 fixture helper 都带 `integration` 标签。
- 确认 fixture 中有无关系用户及无关好友/群，避免查询忽略 owner/type 条件时误判通过。
- 确认消息断言检查真实字段与顺序；不把“空队列重复执行”描述为“重复投递幂等”。
- 确认 SyncMsg 的全局日志文件隔离到临时目录，关闭后切换为丢弃输出，支持测试重复执行；仍通过数据库/队列断言检查结果。
- 本轮没有修改生产代码或推进 P1 修复。SyncMsg 吞错、旧队列确认窗口等已知问题仍按后续阶段处理。

### 验证

沿用 P0.2 固定镜像（MySQL 8.0.44 / Redis 7.4.9）与本机 Go 1.23.3。测试缓存为仓库 `.gocache`；集成测试显式将 `ICHAT_CONFIG_FILE` 指向不存在的路径，确认不依赖开发配置。

| 命令 | 结果 |
| --- | --- |
| `go test ./... -count=1` | 通过 |
| `go test -tags=integration ./database ./scheduledTasks -run '^$'` | 编译通过，不执行外部测试 |
| `go vet -tags=integration ./database ./scheduledTasks ./tests/testenv` | 通过 |
| `go vet ./...` / `go vet -tags=integration ./...` | 均通过 |
| `go test -race -tags=integration ./database -count=1 -shuffle=on -bench . -benchtime=1x -v -timeout=15m` | 5 个恢复的集成测试、3 个原有单元测试及 4 个 benchmark 全部通过；无 Skip；耗时 181.081 秒 |
| `go test -race -tags=integration ./scheduledTasks -count=2 -v -timeout=8m` | SyncMsg 连续两次通过；无 Skip；耗时 41.967 秒 |
| `git diff --check` | 通过 |

本轮共创建 11 套独立 fixture 环境（database 测试 5 套、benchmark 4 套、SyncMsg 2 套）。数据库测试随机种子为 `1789382903695118200`。所有清理调用均成功，结束后按测试标签只读查询 Docker 容器、卷、网络，三项均无残留；race detector 未报告数据竞争。benchmark 的单次测量不作为吞吐量或稳定延迟结论。

建议 commit message：`test: restore legacy integration tests with isolated fixtures and assertions`

P0.3 已提交：`c2b4412`。用户已确认继续 P0.4。

## P0.4：核验并固定受支持的 Go 工具链

状态：工具链升级、代码审查和本小点兼容性测试均已完成，已提交为 `671f818`。

### 版本核查与改动

- 2026-09-17 核对 [Go 官方下载清单](https://go.dev/dl/?mode=json) 和 [发布记录](https://go.dev/doc/devel/release)：稳定版本为 `go1.27.1`，发布于 2026-09-01。当前受支持的稳定主版本系列为 1.27 和 1.26；原本使用的 1.23.3 已不受支持。
- 将 `go.mod` 的最低 Go 版本从 `1.21.3` 改为 `1.27.1`，本轮验证均显式指定 `GOTOOLCHAIN=go1.27.1`。工具链实际输出为 `go version go1.27.1 windows/amd64`。
- 通过 Go 的工具链下载机制写入用户 Go 缓存，未替换系统 Go，也未运行 `go env -w` 修改全局设置。
- README 和测试说明补充版本要求、当前终端精确选择方式以及首次下载/离线使用条件。
- `go.mod` 的 `go` 行是最低版本要求；省略 `toolchain` 行时默认建议同一版本，不额外添加重复的 `toolchain go1.27.1`。本机已安装更高版本时，`auto` 可以选用更高版本；严格复现由 `GOTOOLCHAIN=go1.27.1` 保证。依据：[官方工具链规则](https://go.dev/doc/toolchain)。
- 本轮不引入 RabbitMQ 客户端；客户端版本在 P3 接入时核验。继续使用 P0.2 已固定摘要的 MySQL 8.0.44、Redis 7.4.9 测试镜像。
- 现有数据访问客户端保持原版本：`gorm.io/gorm v1.25.11`、`gorm.io/driver/mysql v1.5.7`、`github.com/go-sql-driver/mysql v1.8.1`、`github.com/go-redis/redis/v8 v8.11.5`。

### 代码审查

- 修改范围限定为 Go 版本声明及文档，未使用新语法批量改写业务代码。
- 区分系统自带 Go、项目最低版本和本轮实际执行工具链，避免把 `go.mod` 最低版本当作对任意更高工具链的强制降级。
- 构建和测试使用 `-mod=readonly`，不允许验证过程自动改变依赖；核对 `require` 和 `go.sum` 变化。
- 兼容性检查不需要升级依赖；`require` 列表和 `go.sum` 均保持不变。显式 `GOTOOLCHAIN=local` 检查仍输出原系统的 `go1.23.3`。
- 审查 [Go 1.27 发布说明](https://go.dev/doc/go1.27)，以实际构建、静态检查、race 和数据库/Redis 集成测试验证当前代码的兼容性。

### 验证

除表中明确标注的 `auto`/`local` 版本选择检查外，下述命令均在当前进程设置 `GOTOOLCHAIN=go1.27.1`、`GOCACHE=<仓库>/.gocache`；测试的 `ICHAT_CONFIG_FILE` 指向不存在的路径，集成资源仍由 fixture 显式创建。

| 命令 | 结果 |
| --- | --- |
| `go version` | `go version go1.27.1 windows/amd64` |
| 默认 `GOTOOLCHAIN=auto` 下的 `go version` | 自动选用 `go1.27.1` |
| `GOTOOLCHAIN=local go list ./...` | 按预期拒绝系统旧版 1.23.3，提示最低要求为 1.27.1 |
| `go test -mod=readonly ./... -count=1` | 通过 |
| `go vet -mod=readonly ./...` | 通过 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go build -mod=readonly ./...` | Windows amd64 通过 |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly ./...` | Linux amd64 交叉编译通过；不是 Linux 实机运行验收 |
| `go mod verify` | `all modules verified` |
| `go test -mod=readonly -race -tags=integration ./... -count=1 -shuffle=on -bench 'Benchmark(GetGroups2?\|SearchFriends2?)$' -benchtime=1x -v -timeout=15m` | 重跑全部通过；包含 6 个旧业务集成测试、1 个环境隔离测试和 4 个 benchmark；无 Skip、无 race 报告 |
| `git diff --check` | 通过 |

首轮集成执行因 Docker Desktop 引擎未运行而失败，各 fixture 在创建前的资源查询阶段报错；没有把该次执行记为通过。随后后台启动 Docker Desktop，核实引擎 `28.4.0` 可用，再重跑完整验收。

最终数据库包测试耗时 185.679 秒，SyncMsg 包 21.604 秒，环境隔离包 41.022 秒。本轮成功创建并清理 12 套独立环境（数据库用例及 benchmark 共 9 套，SyncMsg 1 套，环境隔离 2 套）；数据库测试随机种子为 `1789575870426881000`。结束后按测试标签查询容器、卷和网络，三项均无残留。

未覆盖：Linux/macOS 实机运行、MQ 新客户端兼容性、旧链路故障修复和端到端可靠性。本轮 benchmark 为可执行性检查，不作为性能基线。

建议 commit message：`build: require Go 1.27.1 and document reproducible toolchain selection`

P0.4 已提交：`671f818`。本轮用户要求阅读文档继续开发，继续实施 P0.5。

## P0.5：独立迁移命令与失败退出

状态：实现、代码审查和本小点测试完成，已提交为 `37c44e2`。本小点不代表整个 P0 完成。

### 改动

- 新增 `cmd/migrate`，通过 `go run ./cmd/migrate` 显式迁移；沿用 `config.Load()` 的配置文件及环境变量规则，仅连接 MySQL。
- 删除业务 `main.run()` 中的迁移调用；服务启动不再自动建表或更新 schema。连接构造器继续不承担迁移职责。
- 迁移命令在配置、连接、DDL 或连接池关闭失败时报告错误并非零退出；成功时输出 `legacy schema migration completed`。迁移返回后先关闭连接池，再返回到 `main()` 决定进程退出结果。
- 本轮保留 `MigrateLegacy` 对四张旧表执行 `AutoMigrate`，供独立命令和隔离 fixture 使用；P2 的版本化可靠性 schema 尚未实现。MySQL DDL 可能部分成功，不承诺整批回滚。
- README 和压测指南增加“先迁移成功，再启动服务”的步骤；测试说明增加迁移验收命令。
- 新增无外部依赖的配置错误子进程测试，以及使用 P0.2 隔离资源的真实命令集成测试。

### 代码审查

- 核对生产迁移调用仅存在于 `cmd/migrate`；旧集成 fixture 的显式迁移继续保留，不改变旧业务测试准备流程。
- 检查失败路径和资源释放：配置失败不连接数据库，连接失败沿用构造器清理；DDL 错误不会绕过连接池关闭，关闭错误不会覆盖原错误。
- 子进程调用真实 `main()` 并检查退出码、stderr 和成功标记，避免只检查内部返回值而遗漏 CLI 错误退出行为。
- 集成资源使用随机 runId；核对实际 schema 后才写入测试视图和业务行；子进程清除继承的 `ICHAT_*` 设置，仅使用本轮配置。先关闭数据库客户端，再按归属校验清理容器、卷和网络。
- 迁移成功测试使用不可用的 Redis 地址，验证命令不依赖 Redis；连接构造后检查空库，确认建表来自显式迁移。

### 验证

环境：Windows amd64、`go1.27.1`、Docker Engine `28.4.0`；沿用 P0.2 固定摘要的 MySQL/Redis 测试镜像。命令显式设置 `GOTOOLCHAIN=go1.27.1` 和仓库 `.gocache`，使用 `-mod=readonly`；没有变更 `go.mod` 或 `go.sum`。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 全部通过，默认测试不启动 Docker 或数据库 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含新增迁移命令及集成测试 |
| `go test -mod=readonly -race -tags=integration ./cmd/migrate -count=1 -v -timeout=8m` | 通过；集成用例耗时 22.60 秒，包总耗时 25.245 秒；无 Skip、无 race 报告 |
| `git diff --check` | 通过 |

实际覆盖：配置文件缺失/损坏的退出码 1；错误 MySQL 凭证的退出码 1；通过同名视图制造真实 DDL 失败并检查退出码 1；移除本轮测试视图后成功创建四张旧表；插入用户和消息后再次迁移，逐字段校验消息、核对用户业务字段和记录条数；Redis 配置不可用不影响成功迁移。

本轮仅创建一套测试环境，runId 为 `ae53a148584a1bd3435228821bf44dfc`，测试内所有 cleanup 调用均成功；结束后按本轮 runId 标签只读查询容器、数据卷和网络，三项均无残留。Docker 初次只读探测受到沙箱访问限制，经授权访问引擎后执行上述验收；未对开发数据库执行迁移。

未覆盖：任意旧 schema 演变或部分 DDL 成功后的自动恢复、迁移并发互斥、Linux/macOS 实机运行、数据库离线 API 启动及依赖缺失响应。后者继续由 P0.6 和后续 MQ 接入阶段落实。

建议 commit message：`refactor(db): separate legacy migrations from application startup`

下一小点：P0.6，完善依赖注入、缺失依赖处理和 bootstrap 测试。

## P0.6 拆分

P0.5 已提交为 `37c44e2`。用户确认继续下一步。按“复杂项在开始前继续细分”的执行约定，将 P0.6 拆为以下边界：

- P0.6.1：启动工厂注入、失败返回、资源清理和 worker 生命周期测试（已提交）。
- P0.6.2：鉴权依赖注入，Redis 缺失/故障时明确拒绝请求并返回依赖不可用，覆盖黑名单读取和注销写入失败。
- P0.6.3：服务/仓储依赖装配、MySQL 缺失时的 API 行为及恢复，补齐 API bootstrap 验收；开始时按涉及接口继续细分。

P0.6.1 仍保留旧进程的“启动依赖失败即退出”行为，不代表规范第 4.3 节的数据库离线 API 启动、后台恢复或整个 P0 已经完成。

## P0.6.1：可注入的启动流程与资源生命周期

状态：实现、代码审查和本小点测试完成，已提交为 `7820a45`。

### 改动与审查

- `main.run()` 委托给 `bootstrap`，显式注入配置加载、MySQL/Redis 构造、worker 启动和 HTTP 启动函数。默认实现继续使用现有真实依赖，配置与监听方式不变。
- 启动前检查工厂是否齐全；拒绝 nil/未初始化 DB、无效连接池、nil Redis 和缺少 worker 停止句柄。初始化失败不启动后续 worker/HTTP，错误保留原因链。
- MySQL 已打开但 Redis 失败时释放连接池；worker/HTTP 失败时按逆序释放资源；连接池关闭错误与原始错误合并，不掩盖其中任何一个。
- 将旧全局配置及 manager 绑定集中为临时兼容桥。所有外部依赖构造成功后才绑定；退出先停止并等待 worker，再恢复原绑定，最后关闭 Redis 和 MySQL，避免残留已关闭客户端。
- cron 同步检查任务注册结果，返回可等待在途任务结束的 stop 函数；若 worker 工厂部分启动后同时返回错误和 stop，仍执行清理。
- 审查确认本轮没有迁移调用、消息业务变更或新的第三方依赖。全局兼容桥不能并发运行多套 bootstrap；服务层实例注入和完整 HTTP/WebSocket 优雅退出仍待后续实现。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`，`GOCACHE=<仓库>/.gocache`。全项目测试将 `ICHAT_CONFIG_FILE` 指向不存在的文件；配置测试自行使用临时文件。没有启动 Docker 或访问开发 MySQL/Redis。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly . -count=1 -v` | 新增启动生命周期、工厂缺失、cron 等待与错误配置测试，以及原配置缺失测试通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目重复、乱序测试通过，无 race 报告；包含恢复非空旧全局绑定的最终用例 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，检查包含 integration 文件 |
| `git diff --check` | 通过 |

生命周期矩阵覆盖 14 条路径：配置/MySQL/Redis/worker/HTTP 失败、缺失或无效返回、部分 worker 启动、正常返回、关闭失败及 HTTP 与关闭同时失败。断言实际调用顺序、配置传递、原错误保留、全局绑定恢复、Redis/MySQL 关闭顺序；真实 cron 的阻塞任务测试验证 stop 等待任务结束。

本轮没有重新执行真实数据库集成测试；连接构造器、旧业务和迁移代码均未改动。数据库离线运行、请求级 503、Redis 鉴权故障处理及运行中依赖恢复不在本小点验收范围内。

建议 commit message：`refactor(bootstrap): inject startup dependencies and verify cleanup lifecycle`

下一小点：P0.6.2，鉴权依赖注入与 Redis 缺失/故障处理。

## P0.6.2：鉴权依赖注入与 Redis 故障处理

状态：实现、代码审查和本小点测试完成，已提交为 `6e64385`。P0.6/P0 整体仍未完成。

### 改动

- 新增实例化 `TokenService` 和 `TokenBlacklist` 接口；JWT 配置及 Redis 黑名单依赖通过构造器传入。登录签发、受保护路由校验和注销共享实例，不再由 token 工具函数读取 `config.Conf` 或 `utils.RDS`。
- bootstrap 的 HTTP 启动工厂显式接收配置与 Redis，并构造鉴权服务后传入 router；原生命周期测试增加参数传递断言。
- Redis GET 仅在 `redis.Nil` 时视为未封禁；客户端缺失、连接关闭、超时、权限错误及其他读取错误均拒绝放行。SET 检查实际错误，不再吞掉注销写入失败。
- 缺失/无效/过期/已封禁 token 返回 HTTP 401，鉴权依赖不可用返回 HTTP 503；错误体使用 `code/msg/data` 和 `data.reasonCode`，不回传 Redis 原始错误或 token。
- 注销自行验证签名和过期时间，不接受调用方传入未经验证的 claims；只有黑名单写入成功后才发送下线帧、调用本地 Offline 并返回成功。失败时不修改本地在线状态。写入不确定时不能将 503 当作已注销。
- 请求 context 贯穿黑名单读写；TTL 使用 token 的实际剩余时长，写入前若已过期直接拒绝，避免 0/负 TTL 造成异常保留。维持原黑名单 key，既有封禁继续生效。
- 固定签发/校验 HS256，强制过期时间，使用无符号整数直接解析用户 ID，拒绝 0、负数、小数、字符串和溢出值；去除原先浮点格式化四舍五入解析。构造时检查 JWT key 非空及有效正整数小时寿命。
- README 补充鉴权/注销响应行为；测试说明补充独立 Redis 验收命令。

### 回归与代码审查

- 修改前先添加两条故障回归，在关闭的 Redis 客户端上运行旧 `TokenValid`/`BanToken`：均按预期失败，分别复现“读取失败仍接受 token”和“写入失败仍报告注销成功”。修改后保留对应行为回归，并覆盖 Redis adapter、HTTP 及真实 Redis 层。
- 单元测试覆盖无效 JWT 不访问黑名单、原 token/请求 context 传递、TTL、校验后写入前到期、nil 服务/Redis、错误原因链，以及 18 组 HTTP 场景。
- HTTP 场景断言失败请求不进入业务 handler、不调用 Offline、不发送 WebSocket 下线帧；成功注销必须先完成黑名单写入，之后相同 token 访问受保护接口被拒绝。
- 审查所有生产调用点，移除旧全局 token 函数和调用；登录的数据库查询及其他业务 manager 仍属旧全局依赖，留给 P0.6.3。未修改消息接受/持久化路径、迁移、MQ 或 session fencing。

### 验证

环境：Windows amd64、`go1.27.1`、Docker Engine `28.4.0`；沿用 P0.2 固定镜像摘要。Go 命令使用仓库 `.gocache` 和 `-mod=readonly`，最终全项目测试及集成测试的 `ICHAT_CONFIG_FILE` 指向不存在的路径；未改变依赖版本或读取开发配置。

| 命令 | 结果 |
| --- | --- |
| 修改前 `go test -mod=readonly ./utils -run '^TestRedis(ReadFailureRejectsToken\|WriteFailureRejectsLogout)$' -count=1` | 两条回归按预期失败，确认旧问题 |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly ./utils ./service . -count=1` | 补齐 HTTP 场景后通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 最终全项目重复/乱序测试通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./tests/integration -run '^TestAuthRedisIntegration$' -count=1 -v -timeout=8m` | Docker 引擎启动后重跑通过；用例 20.42 秒，包总耗时 24.163 秒 |
| `git diff --check` | 通过 |

真实 Redis 验收覆盖：未封禁 token 可用、封禁后拒绝、黑名单 PTTL 为正且不超过剩余有效期上界、取消请求、已关闭客户端；在独立 Redis 中配置只允许 GET/PING 的 ACL 用户，验证读取成功但 SET 被拒绝时返回依赖错误，且未产生黑名单 key。

首轮集成失败原因是 Docker Desktop 引擎未运行，失败发生在创建资源前（runId `c270c829b9507df28377dd631f146a97`），未将其记为通过。后台启动引擎后，最终成功 runId 为 `93c184581e3acfba9e03189680368d2a`；cleanup 成功，按该 runId 标签查询容器、卷和网络，三项均无残留。未访问或修改开发数据库/Redis。

未覆盖：运行中 Redis 服务端重启/网络黑洞演练、MySQL 缺失时的 API 启动与恢复、其余业务依赖注入、完整 HTTP/WebSocket 优雅退出及全局 session fencing。真实前端对新 401/503 响应尚未联调。

建议 commit message：`fix(auth): inject token dependencies and fail closed on Redis errors`

下一小点：P0.6.3，服务/仓储依赖装配与 MySQL 缺失行为；开始前按接口范围继续细分。

## P0.6.3 按接口拆分

用户确认继续下一项。按既定细分约定，实施边界为：

- P0.6.3a：账号注册/登录的用户仓储注入、请求 context 和存储故障响应（已提交）。
- P0.6.3b.1：联系人/好友/群列表/群成员查询依赖注入，补齐查询故障响应。
- P0.6.3b.2：添加好友及建群/入群/删群的写入依赖注入、请求 context 和存储故障响应；涉及事务时明确边界，不替代 P2 的完整权限事务验收。
- P0.6.3c：消息、WebSocket 与旧 worker 的依赖装配，明确缺失依赖边界。
- P0.6.3d：MySQL 离线时 API 启动、后台恢复和就绪检查，验证完整 bootstrap 行为。

本轮不将某组接口能返回 503 等同于整个 API 已支持离线启动。既定 P1 权限/历史/消息接受修复及 P2 权限事务要求仍需分别验收。

## P0.6.3a：账号仓储注入与存储故障响应

状态：实现、代码审查及本小点测试完成，已提交为 `8d359e2`。

### 改动与审查

- 新增 `service.AccountUsers` 接口，注册、登录和注销在线状态回调由 router 使用同一个注入的用户 manager 装配；账号 handler/router 不再读取 `database.Umanager`。bootstrap 将已装配实例传入 HTTP 工厂，生命周期测试核对实例身份。
- 用户查询和注册方法显式接收 context，并用 `db.WithContext` 传入 GORM。缺失 manager、DB 或未初始化 DB 返回存储不可用错误；nil manager 的本地下线操作可安全执行。
- 用户仓储区分 `ErrUserNotFound`、`ErrPhoneRegistered`、`ErrStorageUnavailable`；存储错误保留原始原因链。并发注册以现有唯一约束为最终边界，将 MySQL 1062 映射为重复注册，不覆盖已有用户。
- 注册/登录在缺失仓储、查询/写入失败或仓储异常返回空用户时返回 HTTP 503，响应不包含 SQL 原始错误。登录不会将存储故障当作密码错误，也不会在失败后标记在线。
- 账号不存在、错误密码、重复手机号、密码不一致和请求绑定失败仍使用原有 HTTP 200、`code: -1` 业务失败形式；真实请求字段、成功响应、密码处理方式保持现有协议。
- 好友添加对用户查询方法的调用增加 `context.Background()` 以匹配签名；关系服务尚未改为请求级 context，留给 P0.6.3b。其余群事务和消息链路未扩展到本轮。
- 审查了 nil/错误路径、token 签发前后顺序、全局绑定隔离和并发重复键分支；README 和测试说明补充具体响应与验收方式。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`，仓库 `.gocache`，Go 命令使用 `-mod=readonly`；最终全项目及真实集成测试的 `ICHAT_CONFIG_FILE` 指向不存在的文件。沿用 P0.2 固定摘要的 MySQL/Redis fixture；没有读取或修改开发数据库。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly ./service ./database . -count=1` | 新增仓储缺失、账号故障/业务响应及注入用例通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目重复/乱序测试通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 最终检查通过 |
| `go test -mod=readonly -race -tags=integration ./service -run '^TestAccountMySQLIntegration$' -count=1 -v -timeout=8m` | 修正故障注入方式后完整通过；用例 20.07 秒，包总耗时 22.695 秒 |
| `git diff --check` | 通过 |

默认测试覆盖两种账号操作共 12 个故障场景、6 个业务/绑定错误场景、成功注册登录和 token 签发失败；断言错误契约、密码不外泄、请求 context、失败不标记在线。将旧全局用户 manager 替换为不可用实例后，注入仓储的正常请求仍成功。

真实 MySQL 测试覆盖：注册行字段及密码校验、登录 token 身份、用户不存在、8 路并发同手机号注册（1 成功、7 重复注册、仅 1 行）；实际观察到 MySQL 1062 并正确转换；通过本轮表上的临时 CHECK 约束制造 MySQL 3819 写入失败，HTTP 返回 503 且无新增用户；预先取消的 context 保留 `context.Canceled` 原因且不写入；关闭连接池后注册和登录均返回 503。

首轮 runId `2d454f8e0f16ba536b4dde8761834368` 在创建故障注入触发器时遇到 MySQL 1419（启用 binlog 时测试用户缺少 SUPER 权限），该轮不记为通过。随后改用测试表 CHECK 约束，无需提高账户权限或修改服务器参数。最终通过 runId 为 `d976b757bebdae798ceefd537fc48948`。两轮 cleanup 均成功；结束后按各自 runId 查询容器、卷和网络，均无残留。

未覆盖：MySQL 服务器停止/重启及网络黑洞、提交结果不确定时的账号恢复、其他业务接口的依赖缺失行为、API 离线启动/后台恢复，以及真实前端联调。本轮不重新宣称 P0.6 或 P0 整体完成。

建议 commit message：`refactor(accounts): inject user repository and report storage failures`

下一小点：P0.6.3b，联系人/好友/群依赖注入和存储故障响应。

## P0.6.3b.1：联系人、好友和群查询依赖注入

状态：实现、代码审查及本小点验收完成，已提交为 `fb856a0`。

本轮先处理查询接口；添加好友、建群/入群/删群留给 P0.6.3b.2，避免同时扩展到旧群事务和写入业务规则。

### 改动与审查

- 新增 `FriendQueries`、`GroupQueries` 和 `ContactQueries`，bootstrap 将已装配的关系/群 manager 传入 HTTP 工厂，router 显式构造四个查询 handler。启动生命周期测试核对传入实例。
- 联系人、好友、群列表、群成员查询不再读取全局 manager。好友/群仓储的七个查询入口接收 context，并传入 GORM；`GetGroupIds` 改用自身 DB，不再依赖全局关系 manager。
- 缺失 manager、DB 或未初始化 DB 返回 `ErrStorageUnavailable`；真实查询错误保留原因链。复用账号接口的存储错误包装及 HTTP 503 响应，`data.reasonCode` 为 `DEPENDENCY_UNAVAILABLE`，不向客户端返回原始 SQL 错误。
- 修复联系人组合查询的部分成功：好友查询失败立即终止，群查询失败也返回 503，只有两部分均成功才组装联系人。成功数据字段和空数组行为保持原协议；好友/群列表身份来自鉴权上下文，群成员查询保留原 `group_id` 参数。
- 旧 WebSocket 群订阅仅补充 `context.Background()` 以适配查询签名；其依赖装配和生命周期留给 P0.6.3c。查询 fixture 移除全局关系 manager 绑定，既有三项查询测试及四个 benchmark 同步适配 context。
- 未修改群写入、事务、消息权限、MQ、迁移或依赖版本；本轮没有引入新的群成员查询权限规则。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`，仓库 `.gocache`，`-mod=readonly`；全项目与集成测试将 `ICHAT_CONFIG_FILE` 指向不存在的文件。真实测试使用 P0.2 的固定摘要镜像和独立 fixture。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly ./service ./database . -count=1` | 新增仓储及 HTTP 查询测试通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目重复/乱序测试通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含新增真实 MySQL 用例 |
| 真实 MySQL：`TestGetGroups2`、`TestSearchFriends`、`TestGetGroupUsers`（`-race -tags=integration -count=1`） | 三项分别通过，19.82 / 19.83 / 19.72 秒 |
| 四个查询 benchmark（`-race -tags=integration -benchtime=1x`） | 均完成运行和结果断言；单次迭代不作为性能基线 |
| `go test -mod=readonly -race -tags=integration ./database ./service -run '^TestContactQueriesMySQLIntegration$' -bench '^BenchmarkSearchFriends2$' -benchtime=1x -count=1 -v -timeout=8m` | 最后两项补跑完整通过；数据库包 22.733 秒，服务用例 19.93 秒、包 21.781 秒 |
| `gofmt -l`（全部变更 Go 文件）、`git diff --check` | 通过 |

默认测试覆盖四个接口的依赖缺失、nil DB、查询错误和取消请求；验证组合查询不得返回部分数据、鉴权 UID 不被请求体覆盖、成功及空结果、用户密码不外泄、错误请求不查询。仓储层另外验证七个入口在 nil receiver/nil DB/空 GORM DB 下不 panic。

真实 MySQL 用例在全局关系/群 manager 为 nil 时验证四个接口的数据集合和空数组；七个仓储入口在请求取消时同时保留 `context.Canceled` 和 `ErrStorageUnavailable`；临时重命名本轮群表后，好友查询仍成功、群查询真实报 MySQL 1146，联系人接口返回 503；恢复表名后同一 handler 再次返回完整联系人。关闭连接池后四个接口均返回 503。没有读取或修改开发数据库。

首轮集成会话已取得三项查询测试和前三个 benchmark 的通过输出，随后会话句柄失效，未取得最后两项的完整终态，因此只补跑 `BenchmarkSearchFriends2` 和 `TestContactQueriesMySQLIntegration`。补跑 runId 分别为 `c838c28c5f306a5fe3ab84f30578ab8c`、`4af7743e4b0e9591b4fbc4813a82b9b3`，结果均通过且 cleanup 成功。结束后只读查询全部带 `io.ichat.test.run` 标签的容器、卷和网络，三项均为空；没有手工删除其他资源。

未覆盖：MySQL 服务端停止/重启或网络黑洞、写入接口的依赖故障、数据库离线 API 启动与后台恢复、真实前端联调及后续权限规则。P0.6/P0 整体仍未完成。

建议 commit message：`refactor(contacts): inject query repositories and report storage failures`

下一小点：P0.6.3b.2，添加好友及建群/入群/删群的写入依赖注入与存储故障响应。

## P0.6.3b.2：好友及群写入依赖注入与故障响应

状态：实现、代码审查和本小点测试完成，已提交为 `5755e94`。

### 改动与审查

- 新增 `FriendWrites`、`GroupWrites`、`ContactWrites`；bootstrap 将关系/群仓储传入 HTTP 工厂，router 显式构造添加好友、建群、入群、删群 handler。生命周期测试核对实际传入的写入实例。
- 四个 handler 及关系/群写入方法不再读取全局 manager。添加好友使用自身 DB 查询目标用户；入群使用自身 DB 写入关系。请求 context 贯穿查询、写入与事务，缺失/未初始化 DB 安全返回存储不可用。
- 未知错误与存储故障统一返回 HTTP 503、`data.reasonCode: DEPENDENCY_UNAVAILABLE`；只将已知业务错误映射为固定提示，不返回原始 SQL。手机号为空、添加自己、用户不存在、重复关系、群不存在/无权删除保留 HTTP 200、`code: -1`。MySQL 1062 及 GORM 重复键错误映射为关系已存在。
- 建群使用 `WithContext(ctx).Transaction`，群记录和创建者成员关系都使用同一个 `tx`；检查事务整体返回值，开始、写入或提交失败不会被报告为成功。panic 触发回滚并继续传播，不再被旧 defer 吞掉。
- 双向好友关系显式在一个事务中写入；删群检查 SQL 错误及 `RowsAffected`，非群主、群不存在或已删除不再返回成功。继续沿用群记录软删除，不在本小点扩展关系清理。
- 入群/删群继续接受字符串 `groupId`，改用无符号解析并拒绝零、负数、小数及溢出，避免负数转成巨大 uint。操作用户来自鉴权 context，请求体 UID/ownerId 不能覆盖。
- 审查确认查询方法、消息链路、迁移、依赖版本均未变更。服务启动仍依赖 MySQL/Redis；入群存在性/容量、已删除群成员关系、并发权限锁序及消息权限校验仍按后续阶段实施，本轮不宣称完整群权限事务已完成。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`，仓库 `.gocache`，使用 `-mod=readonly`。全项目与真实集成测试将 `ICHAT_CONFIG_FILE` 指向不存在的文件，集成测试沿用固定摘要镜像和专属 schema/Redis fixture，没有读取或修改开发数据库。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly ./database -run '^TestCreateGroupTransactionFailures$' -count=1` | 修正测试替身的事务/LastInsertId 实现后，五种事务场景通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 最终全项目重复/乱序测试通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 最终检查通过 |
| `go test -mod=readonly -race -tags=integration ./service -run '^TestContactWritesMySQLIntegration$' -count=1 -v -timeout=8m` | 通过；用例 20.40 秒，包总耗时 22.934 秒，无 Skip 或 race 报告 |
| `gofmt -l`（全部变更 Go 文件）、`git diff --check` | 通过 |

默认测试覆盖四个接口的成功、依赖缺失、nil DB、未知错误、请求取消和绑定失败，共 24 个场景；另覆盖业务错误脱敏、非法群 ID、五个仓储写入方法的 nil receiver/nil DB/空 GORM DB，以及重复键分类。无网络事务替身验证正常提交、Begin 失败、第二次写入失败、Commit 失败及 panic 的错误传播和提交/回滚路径；前两次运行发现的是测试替身接口实现问题，修正后验证通过，未据此声称真实服务器发生过提交失败。

真实 MySQL 验证：全局用户/关系/群 manager 为 nil 时，注入仓储仍能写入精确的双向好友、群及创建者关系、入群关系；重复写入实际触发 1062 并转为业务错误。非群主删除失败且群仍在，群主删除后查询不可见，再删返回业务失败。临时 CHECK 约束实际触发 3819，分别验证成员写入失败回滚群记录、反向好友写入失败不留下任意一侧关系；预先取消请求保留原始取消原因且不写入、不删群。临时重命名群表制造 MySQL 1146，删群返回 503；关闭连接池后四个接口均返回 503。

本轮 runId：`af8f6340ddbad9e340da515dfd973bc4`。fixture cleanup 成功；按该 runId 标签只读查询容器、卷和网络，三项均无残留。临时约束和表名变更均限本轮隔离 schema。

未覆盖：真实网络中断或服务器重启下的提交结果不确定、并发群生命周期/权限锁序、入群业务校验、删群关联关系清理、数据库离线启动及前端联调。503 不保证写入未发生，完整恢复语义仍需后续实施。P0.6/P0 整体未完成。

建议 commit message：`refactor(contacts): inject write repositories and handle transaction failures`

下一小点：P0.6.3c，消息、WebSocket 与旧 worker 的依赖装配；开始时按接口与生命周期边界继续细分。

## P0.6.3c 拆分

- P0.6.3c.1：旧同步 worker 的仓储注入、context 和错误返回（已提交）。
- P0.6.3c.2：HTTP 消息发送/历史接口依赖注入及依赖缺失响应。
- P0.6.3c.3：WebSocket 依赖装配和订阅生命周期；开始时按需要细分。

## P0.6.3c.1：旧同步 worker 仓储注入与错误返回

状态：实现、代码审查及本小点验收完成，已提交为 `1e52fa9`。

### 改动与审查

- `SyncMsg(ctx, store) error` 显式接收 `MessageStore`，不再读取 `database.Mmanager` 或调用进程日志。bootstrap 将已装配实例传给 worker 工厂，生命周期测试核对注入实例；cron 调度边界负责记录失败及兜底 panic，停止时仍等待在途任务结束。
- 任务返回读取、落库、确认阶段的错误并保留原因链；依赖缺失安全返回错误。空批次直接返回，批次长度/数量不一致或数量为负时不落库、不确认；落库失败绝不继续确认。
- 三个仓储入口增加 context 和缺失依赖检查。`LLEN.Result()` 检查真实错误，不再用 `.Val()` 将 Redis 错误误认为空队列；`LRANGE` 的结束下标改为 `size-1`，返回实际读取条数。确认拒绝非正数量，数据库/Redis 错误统一保留 `ErrStorageUnavailable`。
- `SaveMsgs2DB` 显式传递 context，空输入不执行写入，JSON 解析错误标明批次索引。既有消息缓存回源测试适配签名，旧同步集成测试不再绑定全局消息 manager 或初始化日志文件。
- 仍保留旧全量读取、数据库提交后 LTRIM 的处理方式；没有加入重投幂等、有界领取、并发精确前缀确认、坏消息隔离、防重叠或跨进程单 worker 约束。`size-1` 修正不等于 P1 的完整确认算法；确认失败可能导致下次重复键阻塞，仍需 P1 处理。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`；最终全项目与集成测试将 `ICHAT_CONFIG_FILE` 指向不存在的文件。真实测试使用独立 MySQL/Redis fixture，未访问开发数据。

默认测试覆盖成功、空队列、三个阶段失败、计数不匹配、负数批次、取消请求，以及依赖缺失；验证原始错误、调用顺序、批次内容、确认数量和同一 context。Redis 无网络 hook 验证 LLEN/LTRIM 故障不会返回成功。

真实测试保留三条消息逐字段落库/队列清空和空队列不改变数据的回归；新增关闭 Redis 客户端、取消请求不改变 DB/队列、临时重命名本轮 message 表制造真实 MySQL 1146 并保留队列。Redis hook 分别注入 LRANGE 失败和 LTRIM 失败，后者验证 MySQL 已提交且队列仍保留；这是命令层故障注入，不是服务器网络故障演练。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly ./scheduledTasks ./database . -count=1` | 新增 worker/仓储测试及启动回归通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目重复/乱序测试通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 最终检查通过 |
| `go test -mod=readonly -race -tags=integration ./scheduledTasks ./database -run '^(TestSyncMsg\|TestDeleteMsg)$' -count=1 -v -timeout=8m` | 两项通过，worker 20.50 秒、缓存删除/回源 20.40 秒 |
| `go test -mod=readonly -race -tags=integration ./scheduledTasks -run '^TestSyncMsg$' -count=1 -v -timeout=8m` | 补齐 LRANGE 故障用例后最终通过，用例 19.98 秒，包总耗时 22.422 秒 |
| `gofmt -l`（全部变更 Go 文件）、`git diff --check` | 通过 |

三套 fixture 的 runId 分别为 `4329a49892519cda995f2f7ab7f8b61e`、`0bcd6cf84aa6bb537b6038cf23cd4b1d`、`060ee69a55c0f7376a2f317ba18c817d`。cleanup 均成功；结束后逐个 runId 只读查询容器、卷和网络，均无残留。

未覆盖：真实服务端重启/网络黑洞、MySQL 提交结果不确定、worker 防重叠、跨进程租约、有界领取、重投幂等、精确前缀确认、坏消息隔离及退出超时。cron 仍使用后台 context 并等待在途任务结束；context 贯穿仓储不代表已经实现退出时主动取消。消息 HTTP/WS 仍待下两小点装配，P0.6/P0 整体未完成。

建议 commit message：`refactor(worker): inject message store and propagate sync failures`

下一小点：P0.6.3c.2，HTTP 消息发送/历史接口的依赖装配与缺失依赖响应。

## P0.6.3c.2：HTTP 消息发送和历史查询依赖注入

状态：实现、代码审查及本小点测试完成，已提交为 `596126c`。

### 改动与审查

- 新增 `service.MessageStore` 接口，bootstrap 将实例传入 HTTP 工厂，router 显式构造 `SendMsg`/`GetMessageList`。发送 handler 移到 `service/send_message.go`；HTTP 消息接口不再读取全局 `Mmanager`，WebSocket 仍留给 c.3。
- 发送、发布、历史查询、旧缓存迁移及 MySQL 回源全部显式接收 context。缺失 Redis/manager 安全返回存储错误；MySQL 仅在回源时检查，缓存命中不要求 DB 可用。真实底层错误保留原因链并映射为 503，不向客户端暴露 SQL/Redis 细节。
- 检查 `RPUSH.Err()`，入队失败后停止发布并返回 503，修复原先入队失败仍报告成功的问题。发送者绑定为鉴权 UID，请求体 `fromUser.id` 不再决定发送身份。这是与本轮调用边界直接相关的局部修复，不代表 P1 接受链路或权限修复完成。
- 发送校验非零目标 ID、有效 `is_group` 和已支持的媒体类型；历史校验非零目标、`is_group` 及 1–100 页大小，仓储也拒绝无效会话类型/分页参数，避免负数/零页大小触发异常范围或无效类型走入旧 panic。
- 明确区分分页游标不存在和依赖故障：前者返回固定业务错误，保留 HTTP 200、`code: -1`。历史 JSON 解码失败返回 503，不再把零值或部分历史当成成功；正常空数组和逆序展示保持原行为。
- 旧缓存→入队→发布顺序、发布失败后的缓存删除逻辑、历史媒体字段映射均未重构。入队失败可能留下缓存，发布失败可能留下待落库消息；503 不是“没有写入”的证明。群成员权限、分页游标协议和媒体历史映射仍由 P1 单独修复，不新增 RabbitMQ 或 accepted 语义。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`。全项目和集成测试将 `ICHAT_CONFIG_FILE` 指向不存在的文件；使用独立 MySQL/Redis fixture，不访问开发数据。

默认测试覆盖两接口的仓储缺失、nil Redis、未知错误、取消请求和绑定失败；另验证无效分页/类型、固定业务错误脱敏、注入实例在全局 manager 为 nil 时可用、发送者 UID、媒体/内容和历史分页/群类型/cursor/context 传递、空数组和顺序、损坏 JSON 不返回部分结果。仓储测试覆盖 nil receiver/Redis、关闭 Redis 原因链、无效消息和会话类型。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly ./service ./database . -count=1` | 新增消息 HTTP/仓储及启动测试通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目重复/乱序测试通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，覆盖集成文件 |
| `go test -mod=readonly -race -tags=integration ./service ./database ./scheduledTasks -run '^(TestMessageHTTPIntegration\|TestSaveAndLoadMsg\|TestDeleteMsg\|TestSyncMsg)$' -count=1 -v -timeout=10m` | 四项全部通过；HTTP 用例 27.46 秒、存取分页 26.30 秒、缓存回源 19.91 秒、同步任务 25.94 秒，无 Skip 或 race 报告 |
| `gofmt -l`（全部变更 Go 文件）、`git diff --check` | 通过 |

真实 HTTP 用例核对入队消息的全部字段与鉴权发送者，缓存命中及 MySQL 回源内容、无 DB 的缓存命中与冷读失败、旧格式缓存迁移、取消发送不增加队列、取消查询、MySQL 1146 回源失败及恢复后成功、游标不存在、缓存中有效前缀但损坏 JSON 的失败。将本轮 `dirty:msgs` 改成字符串后，真实 RPUSH 返回 WRONGTYPE，HTTP 返回 503，记录到的调用仅有 ZADD/RPUSH，未执行 PUBLISH；另以 hook 注入 PUBLISH 失败，确认消息已入队且 HTTP 返回 503。客户端关闭后两接口均为 503。未将 hook 故障写为真实网络故障。

四套 fixture runId 分别为 `cd76baad1d29cd7290fe643d6478c54d`（HTTP）、`7a543c6f55d0273d062cb0edd0b91888`（存取）、`55c3a46822aab675f0e3b743da81d069`（回源）、`fac51f9bd8703d58c74257d5c818847d`（worker）。cleanup 均成功，结束后逐个 runId 只读检查容器、卷和网络，均无残留。

未覆盖：真实前端联调、网络黑洞和服务端重启、发送原子性/重试幂等、内容/身份完整约束、群历史权限、历史媒体映射及游标协议修复、MySQL 离线启动和 WebSocket 生命周期。页大小上限限制的是本次响应，不会限制旧全量回源的读取量。P0.6/P0 和 P1 均未整体完成。

建议 commit message：`refactor(messages): inject HTTP message store and report dependency failures`

下一小点：P0.6.3c.3，WebSocket 依赖装配与订阅生命周期；实施前按复杂度继续细分。

## P0.6.3c.3 拆分

- c.3a：Redis 订阅句柄、确认/关闭语义，以及接收循环的资源归属（本轮）。
- c.3b：WebSocket handler 整体依赖注入、升级前依赖失败响应和连接退出等待；开始时如有需要继续细分。

## P0.6.3c.3a：可关闭的订阅句柄与接收循环清理

状态：实现、代码审查和本小点验收完成，已提交为 `cf5bb22`。

### 改动与审查

- 将订阅实现移到 `database/subscriptions.go`，返回 `Subscription` 接口（`Channel`/`Close`），调用方拥有并负责释放成功取得的句柄。句柄使用 `sync.Once`，重复或并发关闭安全；context 取消触发关闭，手动关闭也会释放取消监听 goroutine。
- 私聊/群订阅显式接收 context，检查缺失 manager/Redis、无效 UID；群查询用消息仓储自身的 DB，不再读取全局 `Gmanager`。无群时返回 nil 消息通道的空句柄，不建立 Redis PubSub；群 channel 去重。
- 建立非空订阅后逐一接收 SUBSCRIBE 确认，确认阶段共用五秒读取时间预算，失败即关闭连接并保留依赖错误原因。预算针对确认读取，初始连接/写命令仍受 Redis 客户端超时及传入 context 约束，不宣称全流程五秒硬上限。
- `recvProc` 显式接收订阅提供者并返回错误；取得私聊/群句柄后立即注册 Close，即使提供者同时返回部分句柄和错误也释放资源。群订阅失败立即结束会话，不继续提供不完整订阅。
- 私聊/群通道读取检查 `ok` 和 nil 消息，避免关闭通道导致 nil 解引用；空群通道禁用该 select 分支。接收循环退出取消会话；错误由 Chat 调用边界记录。原消息解析和转发协议保持不变。
- 当前 Chat 仍从全局 manager 取得提供者与 Offline 依赖，订阅仍发生在升级之后；handler 整体注入、升级前 503、所有会话 goroutine 的退出等待留给 c.3b。Redis PubSub 仍是尽力通知，不代表已实现消息补拉、跨实例会话 fencing 或重连可靠性。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`；最终全项目和真实集成测试将 `ICHAT_CONFIG_FILE` 指向不存在的文件。未访问开发数据库。

默认测试覆盖 nil manager/Redis/DB、无网络 dial 失败、已关闭 Redis、预先取消的 context；使用内存 `net.Pipe` 接收订阅命令但不回复，验证取消可以中断正在等待的握手。空句柄支持十路并发 Close。接收循环覆盖十二种退出/初始化路径，核对 Close 次数、部分失败释放、错误原因、UID/context 传递及退出取消会话。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly ./database ./service -count=1` | 订阅及接收循环测试通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目重复/乱序测试通过，无 race 报告 |
| `go test -mod=readonly -race ./database -run '^TestSubscriptionCancelInterruptsHandshake$' -count=2` | 补齐“收到命令后再取消”的同步后，两次通过 |
| `go vet -mod=readonly -tags=integration ./...` | 最终静态检查通过 |
| `go test -mod=readonly -race -tags=integration ./database -run '^TestSubscriptionsIntegration$' -count=1 -v -timeout=8m` | 通过，用例 20.20 秒，包总耗时 22.631 秒，无 Skip 或 race 报告 |
| `gofmt -l`（全部变更 Go 文件）、`git diff --check` | 通过 |

真实用例在全局 `Gmanager` 为 nil 时建立私聊和两个群的订阅，逐个频道发布并核对原消息全部字段；NUMSUB 为各 1。无群用户返回空句柄并可重复关闭。取消后两个通道关闭，所有频道 NUMSUB 回到 0；后台 context 下手动 Close 也验证通道关闭和计数归零。错误密码触发真实 Redis 认证错误，返回存储依赖错误且没有成功句柄。没有把 PubSub 尽力转发当作可靠投递验收。

本轮 runId 为 `6d7d950c60950e5fc89b9b8a0b634197`。fixture cleanup 成功，按该 runId 只读查询容器、卷和网络，三项均无残留。

未覆盖：完整 WebSocket 握手、真实客户端断开/替换连接、所有会话 goroutine 的退出等待、满缓冲慢客户端、运行中 Redis 重启及重订阅、群成员变化后的权限更新、跨实例 session fencing。上述整体连接行为留给 c.3b 及后续阶段，P0.6/P0 与 P1 仍未整体完成。

建议 commit message：`fix(pubsub): own subscription handles and close them on session exit`

下一小点：P0.6.3c.3b，WebSocket handler 显式依赖、升级前故障响应及退出等待。

## P0.6.3c.3b：WebSocket handler 注入、升级前故障响应与退出等待

状态：实现、代码审查和本小点验收完成，已提交为 `1c063c0`。

### 改动与审查

- bootstrap 将消息订阅实例显式传入 HTTP 工厂，router 为 `/auth/chat`、`/auth/getws` 装配同一个 handler；下线回调与注销接口来自同一个用户仓储。`service/chat.go` 不再读取全局消息/用户/群 manager 或全局 logger，日志实例也由 router 注入。
- `Chat` 使用请求 context 派生会话 context；在 HTTP 升级前取得私聊及群订阅并校验返回句柄。依赖缺失、订阅失败、取消请求返回统一 503；无鉴权 UID 返回 401。只有升级成功后才注册/替换会话，失败重连不影响旧连接或本地在线状态。
- 提取 `openSubscriptions` 和统一资源持有者，以 `sync.Once` 保证整个会话只关闭一次；部分初始化失败、升级失败及正常退出均释放取得的资源。空群通道仍支持无群用户连接。
- 通过 WaitGroup 跟踪写 socket、读 socket、接收订阅与心跳四个 goroutine。退出先 cancel，再关闭 socket 解除阻塞读取，关闭订阅，等待所有任务退出，最后清理会话归属和执行 Offline。写队列关闭时安全退出。
- 在会话注册锁内核对归属并执行本地下线，防止“旧连接核对后、新连接注册前后”的窗口误清理替换连接；仍仅为当前进程内协调，不宣称跨实例 fencing。Offline 是本地快速回调，不能在回调中重新进入会话注册操作。
- c.3a 接收循环测试同步调整为先取得资源再接收，仍核对关闭次数和取消；订阅后端无需修改。本轮没有增加消息 ACK、重连补拉、群权限动态更新、Origin 策略或全服务器优雅停机。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`。默认 WebSocket 用例仅监听本地临时端口，订阅由替身提供，logger 写入 `io.Discard`；全项目测试将 `ICHAT_CONFIG_FILE` 指向不存在的文件。

默认测试覆盖八种升级前场景及六种真实本地 socket 生命周期：客户端关闭、订阅关闭、请求取消、同 UID 替换、注销下线帧、无群用户；逐字段检查转发的私聊/群媒体消息。另通过日志 hook 控制读取 goroutine 暂停，证明取消后虽已关闭订阅，handler 仍等待 reader 退出，且 Offline 不会提前执行。生命周期用例将全局用户和消息 manager 设为 nil，验证实例注入生效。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮实现通过 |
| `go test -mod=readonly -race ./service -run '^TestChat' -count=1 -timeout=30s` | 本地 HTTP/WebSocket 用例通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 最终全项目重复/乱序测试通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含真实集成用例 |
| `go test -mod=readonly -race -tags=integration ./service -run '^TestChatRedisIntegration$' -count=1 -v -timeout=8m` | 通过，用例 20.12 秒、包总耗时 21.425 秒，无 Skip 或 race 报告 |
| `gofmt -l`（全部变更 Go 文件）、`git diff --check` | 通过 |

真实集成用例使用独立 MySQL/Redis fixture 和 JWT 中间件，验证无 token 返回 401、有效 token 升级成功、私聊及群消息经 Redis 转发到 WebSocket。缺失 DB 或 Redis 客户端已关闭时，重连在升级前返回 503，原连接仍能收消息且不会被标记下线；正常断开及无群用户断开后，等待 handler 返回并验证订阅计数归零、Offline 次数正确。未访问开发数据库。

首次集成执行的进程会话失效，未将其计为通过。重新执行时 Docker 引擎未运行，fixture 在创建资源前失败；启动 Docker Desktop 后重跑上述命令通过。成功 runId 为 `42d0b8025485ae301ce668ccbe84a7ff`，fixture cleanup 成功；最终按 `io.ichat.test.run` 标签只读检查容器、卷和网络，三项均无残留。

未覆盖：运行中 Redis 重启及重订阅、满缓冲慢客户端的完整验收、全服务器 Shutdown 对已升级连接的管理、群成员变化后的权限更新、跨实例会话协调以及可靠投递/重连补拉。Origin 策略保持原实现，P0.6/P0 与 P1 均未整体完成。

建议 commit message：`refactor(websocket): inject session dependencies and wait for cleanup`

下一小点：P0.6.3d，MySQL 离线 API 启动、恢复及就绪检查；开始前按启动、依赖恢复和接口验收进一步细分。P0.6/P0 仍未整体完成。

## P0.6.3d 拆分

- d.1：API 使用延迟连接的 MySQL 句柄，离线启动和请求错误回归（本轮）。
- d.2：后台存储探测、取消/退出和健康/就绪接口；接受就绪须按当前旧链路与未来 MQ 链路区分，不提前宣称 MQ 可接受。
- d.3：隔离环境中真实 MySQL 离线启动、恢复和接口验收；根据 d.2 的实际实现继续细分。

## P0.6.3d.1：MySQL 离线时 API 启动

状态：实现和本小点检查完成，已提交为 `5fd9067`。

### 改动

- 新增 `utils.NewMySQL`，同时禁用 GORM 自动 Ping 和 MySQL 版本查询，仅构造有效连接池句柄；bootstrap 默认使用该构造器，MySQL 离线不再阻止进入 worker/HTTP 启动阶段。保留原有资源关闭顺序、全局绑定恢复和 Redis 启动检查。
- API 按项目 MySQL 8 环境初始化方言；设置拨号 3 秒、读/写各 5 秒超时，不把它们当作完整请求的总时间上限。每次操作使用同一连接池，连接失败后后续操作仍可重新尝试，不替换仓储或全局 DB 指针。
- `utils.OpenMySQL` 保留立即连接和版本检查，独立迁移命令及现有 fixture 继续使用它。两种构造方式均不自动迁移；构造失败依然返回错误，由 bootstrap 阻止启动。
- 更新 README 和测试说明。此小点未添加后台探测、就绪接口或 MQ accepted 语义。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`，测试配置指向不存在文件。新用例仅使用本地临时 TCP 端口，接受连接后立即关闭以模拟 MySQL 握手失败；未启动 Docker 或访问开发数据库。

`TestBootstrapMySQLOffline` 使用默认 MySQL 构造器，验证离线时仍执行 worker/HTTP、真实登录 handler 返回 503 和 `DEPENDENCY_UNAVAILABLE`、退出停止 worker/关闭连接池并恢复全局绑定。`TestMySQLConstructionModes` 验证延迟构造成功、取消 Ping 保留 context 错误，以及迁移所用立即连接构造器失败。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `gofmt -l`（变更 Go 文件）、`git diff --check` | 通过 |

未覆盖：真实 MySQL 服务器离线/恢复、网络黑洞、后台重试与就绪状态切换、所有接口的完整离线组合。该回归在注入的 HTTP 启动函数内调用真实登录 handler，不等同于启动真实完整 HTTP/Redis/MySQL 环境。P0.6/P0 仍未整体完成。

建议 commit message：`refactor(bootstrap): allow API startup with MySQL offline`

下一小点：P0.6.3d.2，后台存储探测与健康/就绪接口。

## P0.6.3d.2：后台存储探测与健康/就绪接口

状态：实现和本小点检查完成，已提交为 `4fd1954`。

### 改动

- 新增 `health.Storage`，在后台立即对已有 MySQL 池执行 PingContext，每次完成后等待 5 秒，单次 context 超时 2 秒；串行探测，不阻塞 API 启动，不替换仓储/连接池，不执行迁移。
- 初始未就绪；最近成功转为就绪，失败转为未就绪，成功结果超过 9 秒也失效。状态读写加锁，HTTP 只读快照。探测函数必须遵守 context，停止时取消并等待正在运行的探测，避免遗留分离 goroutine。
- bootstrap 拥有探测生命周期，并显式向 HTTP/router 传入只读接口；HTTP 返回或报错时，先停止探测，再停止旧 worker、恢复绑定和关闭客户端。现有启动失败清理测试继续覆盖。
- 增加无需 JWT 的 GET `/health/live`（200）、`/health/ready/storage`（200/503）。后者仅表示 MySQL 连接可用，不验证 schema、迁移版本或写入权限；响应禁用缓存且不泄露底层错误。
- `/health/ready/accept` 当前固定 503 和 `MQ_ACCEPTANCE_NOT_CONFIGURED`，明确未来 MQ 确认通路尚未配置；不把 Redis Ping 成功等同于可靠接受就绪，也不改变旧发送接口行为。P3 再接入确认通路和 Redis 鉴权依赖。
- README 记录接口含义、时序参数和使用边界；不添加全服务器信号处理或 WebSocket 优雅退出。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`，测试配置指向不存在文件；本轮不使用真实数据库或 Docker。

探测测试覆盖首次未就绪、失败后恢复、再次失败、超时、过期、缺失监视器和取消后等待退出（含重复 Stop）。路由测试覆盖三个接口、状态码、缺失依赖及禁用缓存。启动测试核对健康依赖已传入 HTTP 工厂。

首次 race 重复测试发现旧生命周期替身把物理连接 Close 次数当作池关闭次数；后台 Ping 与断言同时使用池可能建立两条连接。将该生命周期测试池限定为单连接后，保留原有关闭顺序断言，最终重跑通过；未改变生产池大小。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./... -count=1` | 首轮通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 修正测试连接计数后，全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过 |
| `gofmt -l`（变更 Go 文件）、`git diff --check` | 通过 |

未覆盖：真实 MySQL 停机/恢复与就绪切换、网络黑洞、schema/权限探测、MQ 接受就绪、全服务器优雅退出。P0.6/P0 仍未整体完成。

建议 commit message：`feat(health): monitor MySQL and expose readiness endpoints`

下一小点：P0.6.3d.3，隔离环境中的真实 MySQL 离线启动、恢复与接口验收。

## P0.6.3d.3：真实 MySQL 离线启动与恢复验收

状态：实现和本小点验收完成，已提交为 `a70c1aa`。

### 改动与验收范围

- 新增根包 integration 用例，fixture 迁移和写入账号后停止真实 MySQL 容器，再执行 bootstrap。使用默认 MySQL/Redis 构造器、旧 worker 和健康探测；HTTP 启动函数改用本地临时端口的 httptest.Server，挂载完整 router 和真实 Redis JWT 黑名单服务。
- 离线时验证 live 为 200、storage 为 503、accept 为 503；无 token 的联系人请求为 401，有效 JWT 的联系人查询和登录为 503。先恢复一次，再运行中停止并恢复一次，验证同一 API、连接池和仓储的 storage 恢复为 200、登录返回正确用户和有效 token、联系人查询成功。
- bootstrap 返回后验证探测已停止、池已关闭、全局绑定恢复。未修改生产代码或引入 MQ 接受语义。
- 为 testenv 增加本轮 MySQL 停止/启动控制，先验证初始化状态、合法 runId、唯一容器以及项目/run 标签；拒绝外部资源及无法验证的容器。默认测试覆盖归属拒绝、模糊/损坏结果和重启后端口更新。
- Docker 重启会重新分配随机宿主端口。集成用例使用仅测试的本地 TCP 转发器保持应用入口固定，重启后刷新转发目标；不替换应用 DB 或模拟 SQL 响应。转发器清理取消拨号、关闭连接并等待全部转发任务退出。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`，配置指向不存在文件。集成测试只访问本轮独立 MySQL/Redis，不读取开发配置。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目通过，无 race 报告 |
| `go test -mod=readonly -race ./tests/testenv -count=2` | 最终端口刷新/归属检查用例通过 |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过，包含 TCP 转发器和集成用例 |
| `go test -mod=readonly -race -tags=integration . -run '^TestBootstrapMySQLRecoveryIntegration$' -count=1 -v -timeout=8m` | 最终通过，用例 38.80 秒、包 40.080 秒，无 Skip 或 race 报告 |
| `gofmt -l`（新增 Go 文件）、`git diff --check` | 通过 |

首次运行 `c9898b8d3f1da9da1b0a96ae2a2a55d6` 恢复等待超时；诊断运行 `c662eafb34b666424d8d6193c221e1a2` 确认 Docker 重启导致随机端口变化。这两次均未计为通过。修正测试网络入口后，最终 runId `0412832a4e1eaa05bbc62680f2afaac3` 两次恢复均成功，fixture cleanup 成功。结束后按 `io.ichat.test.run` 标签只读检查容器、卷和网络，三项均无残留。

未覆盖：网络黑洞、数据库写入提交不确定、全接口离线组合、API 子进程崩溃/信号退出、WebSocket 停机恢复及 MQ 缓冲接受。此轮为真实 MySQL/Redis + 进程内 bootstrap + 实际本地 HTTP 请求验收，不宣称操作系统级应用重启验收。P0 整体完成状态待下一轮汇总核对。

建议 commit message：`test(bootstrap): verify MySQL outage and recovery in isolation`

下一小点：P0 总体验收核对与文档收口，确认剩余边界后进入 P1 旧消息链路修复。

## P0 总体验收核对与收口

状态：P0.1—P0.6 现阶段验收完成；本轮只更新文档，未修改生产逻辑。RabbitMQ vhost 按开发拆分中的原定安排留给 P3.1，未勾选为已完成。

### 条件与证据

| 验收条件 | 代码与证据 | 结论 |
| --- | --- | --- |
| 导入配置/utils/database 不联网、不自动初始化 manager | `config.Load`、显式构造器、`database.TestImportDoesNotInitializeDependencies`；源码检查无 `func init()` | 满足 |
| 默认测试不要求 MySQL/Redis/MQ | 本轮不存在配置文件环境下全项目 race 重复/乱序测试通过；真实测试和 benchmark 使用 integration 标签 | 满足；部分默认测试使用本地临时 socket，不等同于完全禁止网络系统调用 |
| 旧数据测试使用本轮 fixture | `database/legacy_integration_test.go`、`scheduledTasks/syncMsg_test.go` 与 `tests/testenv/legacy.go`；固定身份已替换，旧用例无临时 Skip；历史实测见 P0.3/P0.4 | 满足 |
| MySQL/Redis 隔离和归属清理 | 随机项目、schema、凭证及独立实例；项目/run 标签双重核对；P0.2 实测双环境互不影响，d.3 最近真实运行清理无残留 | 满足 |
| 工具链与版本可复现 | `go.mod` 为 1.27.1，README 指定 GOTOOLCHAIN；客户端版本固定，测试镜像固定 digest，版本核验记录见 P0.2/P0.4 | 满足；本轮实测 go1.27.1 windows/amd64，未重新进行外部版本调研 |
| 迁移独立、失败非零退出 | `cmd/migrate/main.go`；AutoMigrate 仅由独立命令和测试 fixture 调用，生产 bootstrap 不迁移；P0.5 集成记录 | 满足 |
| bootstrap 依赖缺失行为可测试 | 工厂注入和清理测试；账号、联系人、消息、鉴权及 WS 故障响应回归 | 满足；Redis 启动失败仍阻止 API 启动 |
| MySQL 离线启动与恢复 | d.3 默认依赖装配、完整 router、真实 MySQL/Redis、实际 HTTP；同一池/仓储两次恢复及退出清理 | 满足该范围，未扩展为网络黑洞或应用子进程退出验收 |
| RabbitMQ 测试 vhost | 尚未引入 broker | 移交 P3.1，未实现 |

历史集成结果沿用各小点已记录的实际运行，本轮没有重跑 Docker 集成测试或 benchmark，也不把历史记录表述为本轮全套实测。最近一次集成为 d.3 的 runId `0412832a4e1eaa05bbc62680f2afaac3`。

### 本轮检查

设置 `GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`，测试时将 `ICHAT_CONFIG_FILE` 指向不存在文件。

| 命令 | 结果 |
| --- | --- |
| `go version` | go1.27.1 windows/amd64 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含集成代码 |
| `git diff --check` | 通过 |

### 后续归属与下一项

- P1：旧发送链路仍为缓存→入队→通知；worker 仍全量读取并按数量裁剪，尚无完整幂等、坏记录隔离或显式单实例启用约束。历史群权限、媒体类型等也需修复。发送者身份和订阅清理已有回归，但不据此整项勾选 P1。
- P2—P4：新模型、命令幂等事务、MQ 可靠接受及消费落库尚未开始；接受就绪保持 503。
- P5—P6：ACK、补拉、群成员变动、跨实例会话协调与完整通知恢复未实现；现有 WS 清理不等同于这些协议验收。
- 后续生命周期/故障验收：全服务器信号退出、运行中 Redis 重启、网络黑洞、提交不确定及真实前端联调继续按总规范实施，不纳入本次已通过范围。

下一小点为 **P1.1：入队接受顺序及错误语义**。按规范 3.3，先针对入队失败和入队成功后缓存/通知失败建立会失败的回归，再调整 RPUSH→缓存→通知顺序；区分入队确认、后续副作用失败及写入结果不确定，避免接受后误报确定失败。不在这一小点接入 MQ 或同时改造 worker 的领取/确认协议；实施时如需继续拆分，先记录边界。

建议 commit message：`docs: close P0 acceptance and define P1 handoff`

## P1.1：入队接受顺序及错误语义

状态：实现、代码审查和本小点测试完成，尚未提交。本轮只实施 P1.1，P1 整体验收尚未完成。

### 改动

- `SaveMsg` 先校验参数、依赖和调用前 context，再执行 RPUSH；只有入队确认后才执行 ZADD。入队失败或结果未知时不写缓存、不通知。
- RPUSH 成功后，缓存失败记录带 message_id / sender_id 的警告，仍尝试通知；通知失败记录警告，不删除已有缓存，不撤销队列，不将已确认入队改为失败。
- 增加 `ErrMessageEnqueueUnknown`，保留 `ErrStorageUnavailable` 和底层错误链。HTTP 优先识别它，返回 503、`reasonCode=MESSAGE_ENQUEUE_UNKNOWN`、`status=enqueue_unknown` 和原 `messageId`，不泄露底层错误。
- 分类采取保守边界：调用 RPUSH 后任何未获成功结果的错误都归为 unknown，包括最终 WRONGTYPE 或客户端关闭。核对当前 go-redis v8.11.5 的 `baseClient.process/_process` 和 `shouldRetry`：共享客户端可在读响应失败后重试，最终错误不足以排除之前某次已接受；本轮不修改全局重试配置。调用前已取消、依赖缺失仍为普通依赖失败，非法参数仍走原校验响应。
- 成功保持旧接口 HTTP 200 / code 0，仅表示 Redis 已确认入队。没有引入 RabbitMQ、HTTP 202、新幂等协议或自动重试建议。
- 更新默认故障回归、HTTP 错误契约测试及原有真实消息 HTTP 集成测试；同步测试说明和开发规范状态。

### 先失败、后修复与代码审查

- 修改生产代码前运行 `go test -mod=readonly ./database -run '^TestLegacyMessage' -count=1`，回归实际失败：成功路径顺序为 ZADD→RPUSH→PUBLISH；RPUSH 失败前已有 ZADD；ZADD 失败阻止入队；PUBLISH 失败返回错误并执行 ZREM。调用前取消用例通过。
- 首次回归的测试日志路径使用绝对 Windows 路径，与现有 logger 的仓库相对路径拼接规则不符，额外报告了目录错误；改为相对仓库的临时目录路径后验证通过，测试日志不写入开发日志目录。
- 审查 SaveMsg / PublishAndSave 的全部调用点：签名不变，直接 SaveMsg 调用也采用入队确认语义；独立 PublishMsg 仍返回其通知错误。确认 HTTP 优先匹配 unknown，避免被通用依赖错误分支覆盖。
- 核对成功后取消请求的窗口、缓存失败后继续通知、通知失败保留缓存、unknown 不执行后续命令，且底层错误原因仍可用 `errors.Is` 判断。
- 保留 Redis 自动重试可能产生重复队列记录的现有限制；数据库幂等归属 P1.3。本轮不改 worker 领取/裁剪，不宣称修复历史完整性或通知恢复。

### 验证

Windows amd64，实测 `go1.27.1`、Docker Engine `28.4.0`；沿用固定摘要的独立 MySQL/Redis fixture。测试设置 `GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`、不存在的 `ICHAT_CONFIG_FILE`。Docker 沙箱访问受限后经授权执行隔离集成测试，不读取开发数据。

| 命令 | 结果 |
| --- | --- |
| 修改前定向回归（见上文） | 按预期失败，复现旧接受窗口 |
| `go test -mod=readonly ./database ./service -run 'Test(LegacyMessage\|MessageHTTP)' -count=1` | 修复后通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含集成代码 |
| `go test -mod=readonly -race -tags=integration ./service -run '^TestMessageHTTPIntegration$' -count=1 -v -timeout=8m` | 通过，用例 20.40 秒、包 21.656 秒，无 Skip 或 race 报告 |
| `gofmt -l`（变更 Go 文件）、`git diff --check` | 通过 |

真实集成 runId：`e13d8ab28f370c6d20fe887704dc2a68`，fixture cleanup 成功。验证真实 RPUSH WRONGTYPE 时队列原值保持、未创建缓存；真实 ZADD WRONGTYPE 时消息完整入队且 HTTP 成功；通知 hook 失败后队列和缓存保留。RPUSH 成功后的 hook 分别注入超时、取消、EOF，验证实际入队与 HTTP unknown 并存；这属于结果丢失的受控故障注入，不是实际网络黑洞/服务器断连验收。另验证确认入队后取消请求仍成功。

未覆盖：真实网络响应丢失/黑洞、Redis 服务端重启与持久化故障、worker 并发确认、数据库幂等重投、坏消息隔离、缓存完整性、通知重试及 MQ 接受。没有重跑与本项无关的全部 Docker 集成测试或 benchmark。

建议 commit message：`fix(messages): enqueue before side effects and expose uncertain outcomes`

下一小点：**P1.2：有界读取和队首原子核对**。按开发规范 3.3，为有界批次及 Lua 精确前缀确认建立并发追加和 stale batch 回归；不在本轮继续实施。

## P1.2：有界读取和队首原子核对

状态：实现、代码审查和本小点测试完成，尚未提交。开始时 P1.1 已提交为 `8e1980d`；两份设计/进度文档仍为原有未跟踪文件，本轮在其上增补记录，不自动提交。

### 改动与边界

- 保留 `GetAllDirtyMsgs` 旧接口名，改为单次 `LRANGE 0,127`，最多返回 128 条；去掉先 LLEN 再全量读取，返回数量来自实际记录长度。
- `RemDirtyMsgs` 改为接收原始记录切片，拒绝空确认和超过 128 条的确认。Redis EVAL 在一次原子执行中读取队首、逐条按原始字节比对；只有全部相同才 LTRIM 实际前缀长度。队列不足或任何一条不符均不写入，并返回 `ErrDirtyMessagePrefixMismatch`。
- `SyncMsg` 将实际落库批次原样传给确认接口，拒绝仓储返回的超大批次，保持读取→落库→确认及错误链。每轮只处理一批；前缀变化时结束本轮，下次调度重新读取，不在尚无幂等的阶段增加即时消费重试。
- Redis 错误仍按存储错误返回，不能据此断言 Lua 未执行。DB 已提交但确认失败/响应丢失的窗口尚需 P1.3 幂等解决；Lua 不负责跨系统事务，P1.4 的单消费者和防重入约束仍未实现。
- 当前 cron 周期未调整，单轮最多 128 条；本项没有容量调优，持续高于处理能力的发送会积压。上限约束记录条数，不是消息字节总量。

### 回归与代码审查

- 修改生产代码前，用原有数量确认接口运行真实隔离回归：130 条被全量读取；相同旧批次再次确认时，剩余 `uncommitted` 记录被误删除，断言失败。该次测试中的辅助断言还错误捕获了父测试对象，导致失败后提前结束；已改为显式传入当前子测试，未将未执行用例记为通过。
- 默认测试核对 LRANGE 上限、确认原始参数、依赖错误、非法确认长度，以及 worker 不落库超大批次、不吞前缀冲突、不更改待确认内容。
- 真实 Lua 回归覆盖读取后追加、过期批次、第二条内容不匹配、队列变短/不存在、语义相同但字节不同的 JSON、空/二进制记录、WRONGTYPE。两个确认请求与尾部追加并发执行 8 轮，每轮只有一个确认成功，另一个返回冲突，未确认尾部和新追加消息均保留。
- worker 集成增加 130 条真实消息，第一轮入库 128 条且保留 2 条，第二轮完成剩余记录；保留 MySQL 失败不确认、取消不修改及确认命令失败后 DB/队列状态断言。故障 hook 从 LTRIM 改为 EVAL 发送前失败。
- 首轮 worker 集成发现 fixture ID 后缀未补零，`ORDER BY id` 与生成顺序在 10 条后不同；断言改为排序期望副本，避免修改输入批次，再重跑通过。
- 全项目 race 首轮发现 P1.1 的 net.Pipe 响应丢失回归偶发返回 `io.ErrClosedPipe`，而非 `io.EOF`。两者均为实际读错误，改为接受这两种底层原因，同时保留 unknown 分类及已读取 RPUSH 的断言；没有放宽为任意错误。
- 审查所有 RemDirtyMsgs 调用及 worker 接口，生产路径不再按裸数量确认；Lua 先完成全部比较再写入，无先裁剪部分前缀的分支。不增加依赖，不改数据库 schema。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`，测试配置指向不存在文件。真实测试经授权使用独立 fixture，不读取开发配置。

| 命令 | 结果 |
| --- | --- |
| 修改前 `go test -mod=readonly -tags=integration ./database -run '^TestDirtyMessageBatchIntegration$' -count=1 -v -timeout=8m` | 按预期失败，复现全量读取及过期批次误删 |
| `go test -mod=readonly ./database ./scheduledTasks -count=1` | 通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 修正 pipe 错误断言后最终全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过 |
| `go test -mod=readonly -race -tags=integration ./database ./scheduledTasks -run '^(TestDirtyMessageBatchIntegration\|TestSyncMsg)$' -count=1 -v -timeout=8m` | database 全部通过（用例 20.80 秒，包 23.474 秒）；worker 因上述期望排序失败，该次不计通过 |
| `go test -mod=readonly -race -tags=integration ./scheduledTasks -run '^TestSyncMsg$' -count=1 -v -timeout=8m` | 排序修正后通过（用例 20.21 秒，包 22.662 秒），无 Skip/race 报告 |
| `gofmt -l`（变更 Go 文件）、`git diff --check` | 通过 |

修改前 runId：`5910e47bef2586c3dcb712b479e60f6b`；Lua 验收：`3aab9a290e7a06ea0d963703eb17ac80`；worker 首轮：`bcc20777d3ab4d627112e18dc63fceb4`；worker 最终：`16dc2c7d3814dcbed372b32584894a44`。四套 fixture 的 cleanup 均成功。没有将预期失败、测试断言失败或历史测试结果写成本轮通过。

未覆盖：数据库幂等重投、坏记录隔离、DB 提交后进程退出、确认响应丢失/Redis 重启、多 worker 完整消费流程、吞吐基线和 MQ。并发 Lua 验收仅证明当前前缀确认边界，不代表整个 worker 已可多实例安全运行。

建议 commit message：`fix(messages): bound dirty batches and atomically verify queue prefixes`

下一小点：**P1.3：DB 幂等及坏记录隔离**；按约定待用户确认后继续，不在本轮推进。

## P1.3.1：DB 幂等落库

状态：实现、代码审查及本小点测试完成，未提交。开始时 P1.2 改动仍在工作区，本轮保留并继续增量开发，没有将其当作已提交版本。

### 实施拆分与改动

P1.3 涉及事务幂等和新的隔离存储职责，实施前按“一次一个小点”拆为 P1.3.1 DB 幂等落库、P1.3.2 持久隔离与连续前缀推进。本轮只完成第一项，不勾选整个 P1.3。

- 沿用现有全局 message.id 主键，不提前引入 P2 的 sender/clientMessageId、hash 或新 schema。批次解析时拒绝空 ID，避免多个无身份记录被错误合并。
- 将批次写入放在一个显式事务中，逐条 INSERT。只有 MySQL 1062 或 GORM 的明确重复键错误才进入核对分支；不使用 INSERT IGNORE，不更新已有记录。
- 重复键后按 ID 使用 SELECT FOR UPDATE 读取数据库现存记录，避免普通一致性读的旧快照。包括 ID、发送者、接收者、会话类型、媒体、正文、时间和 identifier 的全部字段一致才幂等成功。
- 字段不同返回可 errors.Is 判断的 `ErrMessageConflict`；非主键唯一冲突但对应 ID 不存在时保持失败。连接、CHECK、其他 SQL 或提交错误保留原因并返回存储错误，不能当作重复成功。
- 任一记录失败，本批新增记录回滚，worker 不确认；同批重复、跨批重放和新旧混合可正常提交。解析失败、冲突和 SQL 坏记录仍会阻塞本批，隔离归属 P1.3.2。
- 代价：从批量 INSERT 改为事务内逐条 INSERT，重复记录另有一次锁定查询；尚未测吞吐，不宣称性能提高。单 worker 限制及死锁/锁超时错误保留，未宣称支持完整多 worker 消费。

### 回归与审查

- 修改生产逻辑前，为 TestSyncMsg 增加“DB 已提交、EVAL 发送前失败后重试”断言。旧逻辑实际报 MySQL 1062，队列无法推进；修复后重试不新增行，队列清空。
- 默认事务测试覆盖开始失败、第二次写入失败回滚、COMMIT 错误返回及成功提交顺序。提交错误返回不意味着数据库一定未提交，保留队列供后续幂等恢复。
- 真实仓储测试覆盖同批重复、跨批反向顺序重放、新旧混合；分别修改七个非 ID 字段，断言明确冲突、原行不变、该事务先写的新行回滚；另外验证 CHECK 3819、非主键唯一索引冲突、坏 JSON 与空 ID。
- 首轮仓储测试的临时 content 唯一索引超过 utf8mb4 的索引长度限制，测试 DDL 失败；改成 content(100) 的测试专用前缀索引后重跑通过。没有修改生产 schema。
- 审查没有通用吞错、覆盖已有消息或先查再无约束写入；重复分支查询失败仍返回错误。事务与 Lua 确认分离，P1.2 的队首核对保持不变。

### 实测

Windows amd64；`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`，测试配置指向不存在文件；Docker 仅用于本轮独立 fixture。

| 命令 | 结果 |
| --- | --- |
| 修改前 `go test -mod=readonly -tags=integration ./scheduledTasks -run '^TestSyncMsg$' -count=1 -v -timeout=8m` | 按预期因重放主键重复失败 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 生产实现更新后全项目通过，无 race 报告 |
| `go test -mod=readonly -race ./database -run '^TestLegacyMessageTransactionFailures$' -count=2` | 新增事务故障用例通过 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./database ./scheduledTasks -run '^(TestLegacyMessagePersistenceIntegration\|TestSyncMsg)$' -count=1 -v -timeout=8m` | worker 通过（21.06 秒，包 23.679 秒）；仓储因测试索引 DDL 失败，不计通过 |
| `go test -mod=readonly -race -tags=integration ./database -run '^TestLegacyMessagePersistenceIntegration$' -count=1 -v -timeout=8m` | 修正测试索引后通过（20.19 秒，包 22.630 秒），无 Skip/race 报告 |
| `gofmt -l`（本轮变更 Go 文件）、`git diff --check` | 通过 |

runId：修改前 `2a8bdfc2652a2d85df25af85e1b301d6`；仓储首轮 `0c401ee285f246794618e553cbde0b82`；worker 通过 `50be85077dd285ff3a982ee8f22acbdb`；仓储最终 `765d08bbfac4079479b606e3edac29be`。各 fixture cleanup 均成功。

未覆盖：坏记录持久化隔离、连续前缀部分确认、真实 COMMIT 响应丢失、进程退出、并发消费者完整恢复、新 MQ 身份协议和容量基线。事务替身提交错误和 EVAL 发送前故障不等于真实网络故障演练。

建议 commit message：`fix(messages): make legacy persistence idempotent without overwriting conflicts`

下一小点：**P1.3.2：坏记录持久化隔离与连续前缀推进**，需要原文/原因可恢复、隔离失败不确认原记录；本轮不继续实施。

## P1.3.2：坏记录持久化隔离与连续前缀推进

状态：实现、代码审查及本小点测试完成，未提交。开始时 P1.3.1 及此前代码已提交为 `6de4c76`，工作区只有原有两份未跟踪规范/进度文档；本轮未改动开发数据库。

### 改动

- 新增 `legacy_message_quarantine`：SHA-256 原文摘要主键、longblob 原始字节、原因码/详情、创建时间，无 TTL 或自动删除。完全相同的原文共用一条审计记录；旧 List 无 transport identity，不能把审计行数当作物理投递次数。
- 表由 `MigrateLegacy`/`cmd/migrate` 显式创建，业务启动不迁移；README 提示升级后先执行 `go run ./cmd/migrate`。独立 fixture 自动迁移；开发库未执行本轮迁移。
- 新增 `ProcessDirtyMsgs`，每批至多 128 条，每条独立事务提交；返回已持久化消息或隔离记录的连续前缀长度。直接调用 `SaveMsgs2DB` 保留 P1.3.1 整批事务语义，worker 改用新接口并复用消息幂等函数。
- 坏 JSON/非 UTF-8 原文、缺失身份/非法类型媒体、ID 内容冲突，以及 SQL 数据错误白名单 1264/1366/1406/3819 可以隔离。未知错误、1062 非规范消息冲突、缺表、断连、权限、死锁和锁超时均保留队列责任，不通用吞错。
- 每条事务先检查已有隔离摘要并核验原始字节；重投既有隔离记录只复用隔离结果，不能因 schema/校验变更自动重新执行业务。摘要重复写入分支核对原文，发现不一致返回错误而不是确认。
- worker 对非零已完成前缀执行原有 Lua 核对确认，即使之后处理失败，也只确认此前的连续前缀。当前事务失败或 COMMIT 结果不明不计入完成数；确认也失败时用 errors.Join 保留两个原因。返回非法完成数时不确认。
- 隔离成功表示责任已转移到持久化审计，不等于业务成功或已送达。原文可通过受限数据库查询查看/导出；本轮不增加隔离重放 CLI、管理 HTTP、自动清理或 MQ schema。

### 先失败、审查与测试

- 修改前真实回归 `[有效消息, 坏 JSON, 有效消息]` 因解码失败阻塞，`TestDirtyMessageQuarantineIntegration` 按预期失败。修改后两条有效消息入库，坏原文留在隔离表，队列确认完成。
- 默认测试覆盖错误分类、非法消息/原文字节、摘要记录原文不符、每条事务边界、第二条写入失败返回前缀、COMMIT 失败返回零、worker 的部分确认/非法完成数/双重错误。
- 真实测试验证包含 NUL 和非 UTF-8 的原始字节不丢失；坏 JSON、非法消息、ID 冲突和真实 CHECK 3819 均保留原因。重复原文以及隔离提交后 EVAL 发送前失败的重投，不重复写审计、不覆盖消息。移除消息表 CHECK 后再重投也不自动将已隔离消息写入业务表。
- 通过仅拒绝指定摘要的隔离表 CHECK 制造真实隔离写入失败：前面的有效记录已提交并确认，坏记录和后续有效记录完整保留；解除约束后可继续。隔离表临时不可用时不确认，恢复后可继续。
- 迁移命令集成验证首次创建五张表，重复迁移保留用户、消息以及二进制隔离原文。全部写入、临时约束和改名仅限本轮隔离 fixture。
- 审查 MySQL 数据错误分类只发生在 INSERT 语句失败后，COMMIT 错误在事务外传播，不会被当成可隔离数据错误；隔离自身写入失败也不递归隔离。审查确认参数只包含返回的已提交前缀，不越过第一条失败记录。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`；全项目测试将 `ICHAT_CONFIG_FILE` 指向不存在文件。真实测试经授权使用独立 MySQL/Redis。

| 命令 | 结果 |
| --- | --- |
| 修改前 `go test -mod=readonly -tags=integration ./scheduledTasks -run '^TestDirtyMessageQuarantineIntegration$' -count=1 -v -timeout=8m` | 按预期失败，坏 JSON 阻塞有效尾部 |
| `go test -mod=readonly ./... -count=1` | 初步实现通过 |
| `go test -mod=readonly ./database ./scheduledTasks -count=1` | 新增定向单元测试通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 最终全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./scheduledTasks ./cmd/migrate -run '^(TestDirtyMessageQuarantineIntegration\|TestSyncMsg\|TestMigrationIntegration)$' -count=1 -v -timeout=10m` | 三个用例均通过，无 Skip/race 报告；隔离 20.89 秒、SyncMsg 21.83 秒、迁移 22.90 秒 |
| `gofmt -l`（本轮变更 Go 文件）、`git diff --check` | 通过 |

runId：修改前 `679a98b8b4c989aaf95669ad307fac95`；隔离验收 `e679740b10119fb883e329f44bd63150`；worker 回归 `8a4c284ad5b89d2646e4c263472e3828`；迁移 `deb61cfb6ae8e28986cba13e46df4599`。fixture cleanup 均成功。最终 scheduledTasks 包耗时 45.468 秒，迁移包 25.698 秒。

未覆盖：真实 COMMIT 响应丢失、Redis/应用进程退出、完整多 worker 并发、受控隔离重放、吞吐基线及 MQ。每条事务和隔离查询增加 DB 往返，未进行性能调优。新逻辑要求隔离表可用；只 Ping 的存储就绪不能代替迁移检查。单 worker 强制约束仍归属 P1.4，完整跨系统崩溃回归归属 P1.8。

建议 commit message：`fix(messages): quarantine bad records and acknowledge durable prefixes`

下一小点：**P1.4：worker 启用约束和防重入**。本轮止于 P1.3.2，不自动提交或继续下一小点。

## P1.4.1：显式启用和进程内防重入

状态：实现、代码审查和本小点测试完成，未提交。起点 `2d6a767`；开始时仅原有规范/进度文档未跟踪。

### 边界与改动

- P1.4 在实施前拆成 P1.4.1 显式启用/进程内防重入、P1.4.2 跨进程单消费者约束。后者还需处理锁丢失和异常退出恢复，本轮不以进程内同步声称跨实例安全。
- 新增 `LEGACY_WORKER.enabled`，默认 false；支持 YAML 或 `ICHAT_LEGACY_WORKER_ENABLED` 环境变量覆盖，即使配置文件省略该节也能用环境变量开启。非法布尔值使配置加载失败。
- bootstrap 只在明确启用时调用 worker 工厂并检查 stop 句柄；关闭时不调用工厂，也不要求该工厂存在。HTTP、健康探测和旧发送入队保持可用，没有其他启用的消费者时消息会积压而不会异步落库。
- cron 仍每 60 秒调度一次，增加 SkipIfStillRunning，上一轮未结束则跳过新触发，不排队等待。Recover 位于 Skip 包装内部：核对固定 cron v3.0.0 源码，Skip 在 Run 返回后才归还令牌，必须先恢复 panic 才能保证后续触发继续运行。
- 保持正常返回时 Stop 等待正在执行的任务，先停 worker 再恢复全局绑定及关闭连接。原有业务任务 panic 日志恢复保留，调度包装额外保护传入的任务。
- README、示例配置、测试说明和压测指南记录默认关闭、启用方式和当前单实例部署要求；没有修改用户开发配置，没有启动应用或迁移数据库。

### 回归与审查

- 修改前运行真实 cron 重叠回归，第一轮阻塞时第二轮实际进入，测试按预期失败（1.12 秒）。修复后跨越多个调度周期只运行一个任务，解除阻塞后恢复下一轮。
- 增加任务首轮 panic、后续轮次仍能执行的回归，防止令牌被永久占用；保留 Stop 等待、非法调度表达式和 nil 任务测试。
- 配置测试覆盖省略默认、YAML true/false、仅环境变量开启、环境变量关闭及非法值。生命周期测试覆盖显式开启、关闭/工厂为 nil、启动及部分启动失败、正常/错误退出和清理顺序。
- 原 MySQL 离线单元及真实恢复用例显式设置 Enabled=true，保留其 worker 启动覆盖，不通过默认关闭来绕过原验收条件。真实集成同一 API/连接池经历两次 MySQL 恢复，退出资源清理成功。
- 审查开关判断位于配置加载后，启用却缺少工厂时在连接资源前失败；关闭时仍完整释放 HTTP/健康探测使用的客户端。

### 验证

Windows amd64，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`、不存在的测试配置路径。真实集成只访问本轮独立 MySQL/Redis，未操作开发数据。

| 命令 | 结果 |
| --- | --- |
| 修改前 `go test -mod=readonly . -run '^TestLegacyCronSkipsOverlappingRuns$' -count=1` | 按预期失败，复现任务重叠 |
| `go test -mod=readonly . ./config -count=1` | 修复后通过 |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目通过，无 race 报告，根包 15.039 秒 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration . -run '^TestBootstrapMySQLRecoveryIntegration$' -count=1 -v -timeout=8m` | 通过，用例 38.79 秒、包 40.067 秒，无 Skip/race 报告 |
| `gofmt -l`（变更 Go 文件）、`git diff --check` | 通过 |

集成 runId：`76c19a2ae2893cfe2f78ca3d81ef29df`，fixture cleanup 成功。该集成验证启用状态下的启动/恢复兼容性，任务重叠和 panic 恢复由真实 cron 的默认测试验证，不声称跨进程故障验收已完成。

未覆盖：跨进程排他、锁丢失、异常退出后的接管、运行中的 Redis 重启以及全服务器信号停机。P1.4 整体尚未完成；当前必须在部署上只开启一个实例。

建议 commit message：`fix(worker): require explicit enablement and skip overlapping legacy jobs`

下一小点：**P1.4.2：跨进程单消费者约束与失效恢复边界**。本轮不继续实施、不自动提交。

## P1.4.2：跨进程单消费者约束与失效恢复边界

状态：实现、审查和本小点测试完成，未自动提交。未修改开发配置或开发数据库。

- 启用的 worker 在启动 cron 前取得同一 Redis DB 的固定 `dirty:msgs:worker-owner` 注册，SET NX + 随机 token，无 TTL；第二个实例启动失败。关闭 worker 的 API 不竞争注册，MySQL 离线启动能力保留。
- 不使用自动续租/过期接管：Redis 所有权不能撤销已开始的 MySQL 事务。异常退出保留锁；恢复前必须确认旧进程及其数据库事务已停止。
- 批次读取及每条持久化前核对所有权，校验失败或结果不明后永久停止该 worker 的消费尝试。确认通过同一段 Lua 校验 token、比较原始前缀、LTRIM，失去归属不能删队列。前缀不匹配仅报错，不删除。
- 正常返回先停止 cron 并等待任务完成，再用 Lua 比较 token 释放注册；失败记录日志。旧持有者不能释放后继者的锁。当前信号退出不保证执行正常返回路径，Ctrl+C/IDE 停止可能需要人工恢复。
- 新增 `docs/legacy-worker-recovery.md`，明确比较删除操作、SET 响应不明、Redis 重启丢数据/故障切换/淘汰/分裂的边界。不能在锁丢失后直接启动另一实例；不声称具备数据库 fencing 或自动高可用。旧版本及直接仓库调用不受启动注册约束。
- 所有权检查增加每条记录一次 Redis 往返，仍每批最多 128 条；本次没有做吞吐调优。README、测试和压测说明同步。

验证环境：Windows amd64、GOTOOLCHAIN=go1.27.1、仓库 .gocache、-mod=readonly、不存在的 ICHAT_CONFIG_FILE。

| 验证 | 结果 |
| --- | --- |
| `go test -mod=readonly ./...` | 通过 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go test -mod=readonly -race ./scheduledTasks -count=2` | 后补所有权边界单元测试通过：读前失效、第二条前失效、数据库失败只确认持久化前缀 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./database . -run '^(TestLegacyWorkerOwnershipIntegration|TestBootstrapMySQLRecoveryIntegration)$' -count=1 -v -timeout=8m` | 经授权使用独立 Docker fixture，通过；所有权用例 23.54 秒，MySQL 恢复用例 43.66 秒，无 Skip/race 报告 |
| `git diff --check`、变更 Go 文件 gofmt | 通过 |

真实子进程验证竞争被拒、正常释放、强制退出仍阻止新进程、确认旧进程退出后的人工恢复；真实 Redis 验证无 TTL、旧 token 释放失败、ACK 原子检查、丢失后不自动恢复、取消引起的不确定性停止。runId：所有权 `ac888bd8c4d64e8d7236f7aa71d7ac97`；MySQL 恢复 `af3837030189c1354b66e6864aca1d4e`。fixture 清理完成。首次沙箱内 Docker 访问被拒，提升权限后重新执行通过，该次拒绝不计业务验收失败。

未覆盖真实 Redis 网络黑洞/复制故障切换、在途 MySQL 事务被强杀后的完整消息对账、实际 COMMIT 响应丢失、信号优雅停机及容量基线；相关完整故障矩阵留在 P1.8/后续 MQ 阶段。恢复依赖运维先停止旧消费者，禁止自动抢锁。

建议 commit message：`fix(worker): enforce exclusive legacy ownership with explicit recovery`

下一项：**P1.5：鉴权发送身份**。本轮不继续下一项、不自动提交。
## P1.5：鉴权发送身份

状态：实现、代码审查和本小点测试完成，未自动提交。开始时上一轮代码已提交，仅原有两份规范/进度文档未跟踪。

### 改动与边界

- 审查确认旧发送 handler 已在 P0 阶段用鉴权 UID 覆盖 fromUser.id，生产路由 `/auth/sendMessage` 已挂载 JwtAuth；既有真实 Redis 用例也检查了伪造发送者不入队。本轮不把这些既有能力算成新增修复。
- 补齐 handler 自身的身份边界：在请求绑定和仓库访问前读取一次 uint 类型 UID；缺失、零值或错误类型统一返回 HTTP 401 / UNAUTHENTICATED。此前是 HTTP 200 的通用参数错误。后续消息 SenderId 只使用这一鉴权值。
- 保留旧 fromUser 字段的请求兼容性，合法但伪造的身份会被鉴权身份覆盖；查询参数、顶层 senderId/userId/uid 和 X-User-ID 均不能覆盖发送者。Conv2Msg 仍用于历史游标转换，不改变其历史语义。
- 新增串联真实 TokenService、JwtAuth 和 SendMsg 的回归；黑名单与消息仓库使用替身，无外部依赖。覆盖私聊、群聊、省略 fromUser、缺失/无效/错误签名 token、已撤销 token、黑名单不可用。拒绝请求不得触达消息仓库。
- 本轮仅处理发送身份；发送权限、历史访问权限、未来 MQ status/ACK 的跨用户隔离不借此宣称完成。无数据库迁移或开发配置改动。

### 验证

Windows amd64、GOTOOLCHAIN=go1.27.1、仓库 .gocache、-mod=readonly、不存在的 ICHAT_CONFIG_FILE。

| 命令 | 结果 |
| --- | --- |
| 修复前 `go test -mod=readonly ./service -run '^TestSendMessage' -count=1` | 新增缺失身份回归按预期失败：HTTP 200 而非 401；JWT 身份回归已通过 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./service -run '^TestMessageHTTPIntegration$' -count=1 -v -timeout=8m` | 独立 Docker MySQL/Redis 通过，用例 20.09 秒、包 21.367 秒，无 Skip/race 报告 |
| 变更 Go 文件 gofmt、`git diff --check` | 通过 |

真实集成 runId：`cf9280db103ca7dffdcb7868ebb09cdc`，fixture 清理完成。入队 JSON 的 SenderId 为注入的鉴权 UID，而非请求体伪造值；同时回归原有缓存、历史和入队不确定性语义。JWT 链路和真实 Redis 入队分层验证，本轮未将它们组合成一个真实服务端到端测试。

建议 commit message：`fix(messages): require authenticated identity before binding send requests`

下一项：**P1.6：历史完整性、群历史权限和媒体类型**。本轮不继续下一项、不自动提交。

## P1.6：历史完整性、群历史权限和媒体类型

状态：实现、代码审查和本小点测试完成，未自动提交。开始时仅原有两份规范/进度文档未跟踪。Serena 已成功激活 iChat 并完成 Go 符号读取；本轮未修改开发配置、开发数据库或 schema。

### 改动与边界

- `LoadMsgs` 直接查询 MySQL 已落库历史，不以 Redis key 存在作为完整性证据；删除仅供旧读取路径调用的缓存分页、全量回填和旧缓存迁移方法。发送的 Redis 队列/缓存/通知流程保留。
- 每页最多 100 条，按 `(time_stamp, identifier, id)` 降序读取，HTTP 页内逆序展示。相同时间和 identifier 由唯一消息 ID 确定顺序；游标 ID 必须属于当前会话，排序值取数据库记录，不信任客户端时间、identifier 或内容。私聊一次查询双向收发，严格限定消息类型和双方身份。
- 群权限、游标和消息页使用显式 REPEATABLE READ 只读事务共享快照；群和成员关系均须未软删除。非成员、已离群、群已删除/不存在统一返回 HTTP 403 / `GROUP_HISTORY_FORBIDDEN`，不泄露底层错误。当前成员可读取入群前的全部已落库历史，群主也不能绕过缺失的成员关系。
- 权限以本页首次一致性读取建立的快照为准：之后离群不会撤回已开始的读取，下一页重新检查。未建立跨页固定快照；旧客户端时间戳仍不等于提交顺序，分页期间晚落库的消息需重新查询。可靠 seq 补拉、权限写事务及群内容推送鉴权仍留在后续阶段。
- 历史 handler 在请求绑定前检查鉴权 UID，缺失/错误类型返回 401；媒体映射复用 `Conv2MsgInfo`，返回真实 `text`/`picture`/`audio` 和原内容引用，未知媒体/解码错误返回 503，不返回部分历史。
- 历史仓储无需 Redis，但生产 HTTP 的 JWT 黑名单仍依赖 Redis。MySQL 不可用时不回退到缓存；入队尚未落库的消息暂不出现在历史中，需等待已启用 worker。README、测试说明和压测 SQL 同步；没有新增索引或宣称性能改善。

### 回归与审查

- 修改前媒体回归复现 picture/audio 被返回为 text；真实集成复现仅最新消息缓存下分页失败、非成员读取成功，以及缺 DB 时缓存仍被视为完整历史。首次编译因继承 GOROOT 指向 Go 1.23.3 而失败，随后仅在测试进程清除该变量；Docker 初始未就绪，提升权限并启动检查后才运行真实 fixture。这些环境失败不计为业务回归证据。
- 真实 MySQL/Redis 覆盖私聊双向和群历史逐页完整集合、相同 timestamp/identifier、伪造游标字段、跨会话/类型和不存在游标；新成员可读全部旧消息，软删除成员关系/群后拒绝读取（仍保留群缓存及其他成员关系）。
- 仓储故障覆盖缺 DB/空 GORM、无 Redis、请求取消和消息表临时不可用。HTTP 回归覆盖待落库消息不冒充历史、损坏/旧格式缓存不影响 MySQL 历史、关闭 Redis 后直接调用历史 handler 仍可成功，以及既有入队 unknown/缓存和发布失败语义。
- 默认单元回归覆盖媒体映射、未知媒体不返回部分结果、401/403、鉴权身份、错误隐藏。代码审查核对 SQL 会话范围、软删除条件、存储游标与 ORDER BY 一致、事务出错不返回已读出的部分结果、历史代码没有 Redis 回退。

### 验证

Windows amd64；测试进程 `GOROOT` 清空、`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`，`ICHAT_CONFIG_FILE` 指向不存在路径。真实测试仅使用本轮独立 Docker MySQL/Redis fixture。

| 命令 | 结果 |
| --- | --- |
| 修复前 `go test -mod=readonly ./service -run '^TestMessageHistoryMedia$' -count=1` | picture/audio 按预期失败 |
| 修复前 `go test -mod=readonly -tags=integration ./database -run '^TestMessageHistoryIntegration$' -count=1 -v -timeout=8m` | 分页、群权限、缺 DB 的缓存读取按预期失败；fixture 20.78 秒 |
| `go test -mod=readonly ./database ./service -count=1` | 通过 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./database ./service -run '^(TestMessageHistoryIntegration|TestMessageHTTPIntegration|TestSaveAndLoadMsg|TestDeleteMsg)$' -count=1 -v -timeout=8m` | 四个用例全部通过，无 Skip/race 报告；database 82.123 秒、service 39.841 秒 |
| 变更 Go 文件 gofmt、`git diff --check` | 通过 |

runId：修复前 `8d241ebc49948f88850359ab158cd5bb`；最终历史 `8cb09349ece8f02ae7b4c6165438eb57`（21.02 秒）、HTTP `e08b84d19969f2523dfc7256f2fbfaef`（37.82 秒）、旧存取 `9e58e405712260f980bca65bbf10b788`、缓存驱逐 `35db7e95fb55755e99d145029e85d6a3`。全部 fixture cleanup 成功。

未覆盖：跨页并发晚提交恢复、真实网络中断/读事务提交失败、群变更与正在执行的历史读取之间的并发调度演练、容量及索引性能、群推送权限和新协议 F30/F32/F38 全链路验收。此轮是 L8/L9 旧链路修复，不代表 P1 整体或 MQ 方案完成。

建议 commit message：`fix(messages): read persisted history with group authorization and media types`

下一项：**P1.7：PubSub 生命周期**。本轮不继续下一项、不自动提交。

## P1.7：PubSub 生命周期

状态：实现、代码审查及本小点测试完成，未自动提交。起点 `b454190`（P1.6 已提交）；开始时只有原有规范/进度文档未跟踪。Serena 项目激活和 Go 符号读取正常；未读取或修改 `.env`、未操作开发数据库。

### 既有能力与新增修复

- P0.6.3c.3 已实现可关闭订阅、升级前订阅确认、失败时释放部分句柄、接收通道关闭/nil 检查、会话取消和四个 WebSocket 任务的退出等待。本轮回归这些能力，不将它们重复算作新增实现。
- 核对本地固定依赖 `go-redis/v8@v8.11.5/pubsub.go`：内部缓冲通道满时，发送 select 只等待消费者或默认 60 秒超时，不监听 PubSub.Close；直接关闭连接并不能立即释放这条转发协程。
- 为非空订阅增加一个自有转发协程，输出通道不额外缓冲，保留 go-redis 原有有界缓冲、健康检查与自动重连。停止时关闭 Redis 连接并排空内部通道，直到库的消息读取协程关闭通道；随后关闭对外通道。手动 Close 等待取消监听和自有转发任务结束，不要求调用方继续消费；仅取消 context 也会走清理路径。
- 使用独立的 `stop` 和外部 `Close`：工作协程只发出停止信号，外部调用再 Wait，避免协程等待自身。保留 sync.Once，重复或并发 Close 返回同一关闭结果。无群的空句柄仍无 Redis 连接/消息通道。
- 订阅确认除 Kind 外，还核对未确认的请求频道集合，重复/未知频道不得被计入就绪；失败仍关闭连接并返回存储依赖错误。
- 关闭会丢弃尚未交付的 PubSub 通知，不代表业务回滚或可靠投递。初始连接/写命令和库内重连仍受 Redis 客户端超时约束；本轮解决满缓冲的发送等待，不承诺网络黑洞下全流程固定截止时间。每个非空订阅增加一个转发 goroutine，未测容量。

### 回归与审查

- 修复前 `TestSubscriptionConfirmsRequestedChannels` 复现重复/未知频道确认被接受；`TestSubscriptionFullBufferClose` 复现 Close 返回后仍有未排空消息。net.Pipe 使用超过库默认 100 条缓冲的 RESP 消息及大 payload，等待对端完整写入，不通过固定 sleep 猜测缓冲已满。
- 最终默认测试覆盖满缓冲下手动关闭、仅 context 取消、八路并发关闭；核对自有任务退出且对外通道关闭。补充群通道 nil 消息和私聊/群聊 WebSocket outbound 满时取消仍释放两类订阅；既有初始化失败、关闭、无群、socket 断开/替换等回归保留。
- 真实 Redis 新增三轮未消费通知后的取消/四路并发关闭，核对对外通道已关闭且 NUMSUB 回到 0；原有私聊、多群就绪及转发、错误密码、后台 context 手动关闭继续通过。真实 JWT/WebSocket 集成继续验证失败重连不影响原会话，以及客户端断开后 handler 退出、订阅计数归零。
- 首次真实订阅集成的新循环误用前一场景已取消的 context，发布返回 context canceled；改为该轮 cycleCtx 后单独重跑通过。此失败属于测试 fixture 错误，不是生产订阅故障。
- 审查取消监听不读取后续初始化的 messages 字段；所有 WaitGroup Add 在句柄交给外部前完成；stop 不 Wait，避免并发 Close/取消/转发退出相互等待；先停连接再排空，底层通道关闭后才报告转发退出。

### 验证

Windows amd64；测试进程清空 GOROOT，`GOTOOLCHAIN=go1.27.1`、仓库 `.gocache`、`-mod=readonly`、不存在的 `ICHAT_CONFIG_FILE`。默认测试无外部服务；真实测试使用隔离 Docker fixture。

| 命令 | 结果 |
| --- | --- |
| 修复前 `go test -mod=readonly ./database -run '^TestSubscription(ConfirmsRequestedChannels|FullBufferClose)$' -count=1 -timeout=15s` | 错误频道、重复确认及关闭满缓冲三个回归按预期失败 |
| `go test -mod=readonly -race ./database ./service -run 'TestSubscription|TestEmptySubscription|TestReceiveSubscription|TestChat' -count=1 -timeout=45s` | 初步修复通过 |
| `go test -mod=readonly -race ./... -count=1` | 最终生产实现和默认测试全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./database ./service -run '^(TestSubscriptionsIntegration|TestChatRedisIntegration)$' -count=1 -v -timeout=8m` | WebSocket 通过（33.65 秒，包 35.391 秒）；订阅因测试 context 使用错误失败 |
| `go test -mod=readonly -race -tags=integration ./database -run '^TestSubscriptionsIntegration$' -count=1 -v -timeout=8m` | 修正 fixture 后通过（29.62 秒，包 34.304 秒），无 Skip/race 报告 |
| 变更 Go 文件 gofmt、`git diff --check` | 通过 |

runId：订阅首轮 `8a4b8b150d721f89e8913dde6646c71d`，最终 `fa0bca9102024461e84c99c8cb78166f`；WebSocket `d65969662f4ac46de1fde7892256a6ff`。各轮 fixture cleanup 均成功。

未覆盖：运行中 Redis 重启/网络黑洞和重订阅故障、库内健康检查协程的显式 join（依赖库的 Close/exit）、真实慢网络写 socket 的完整压力验收、群成员变更后的订阅/内容权限刷新、可靠重连补拉、全服务器信号停机和跨实例 session fencing。P1.8 完整回归/跨系统崩溃矩阵尚待实施，P1 总项保持未勾选。

建议 commit message：`fix(pubsub): drain buffered deliveries and wait for subscription shutdown`

下一项：**P1.8：补齐 L1—L12 回归及跨系统崩溃测试**。开始前按复杂度细分；本轮不继续下一项、不自动提交。

## P1.8.1：真实 worker 崩溃恢复

状态：实现、代码审查和本小点测试完成，未自动提交。起点 `af8fb6b`（P1.7 已提交）；开始时仅原有两份规范/进度文档未跟踪。按文档的单小点边界，将 P1.8 拆为：本轮 P1.8.1 真实崩溃恢复；下一轮 P1.8.2 L1—L12 测试映射、缺口补齐和 P1 汇总验收。未修改生产 Go 代码、依赖、开发配置或开发数据。

### 改动与审查

- 新增 `scheduledTasks/crash_recovery_integration_test.go`，仅在 integration 测试二进制内提供屏障和子进程入口；生产构建没有新增故障控制开关。
- 三个独立 fixture 分别在首条消息、隔离记录、整批记录提交后阻塞子进程，此时尚未 Redis ACK。真实调用 `SyncMsg`、`OwnedMessages` 和 MySQL/Redis 仓储，屏障不替换事务或确认逻辑。
- 父进程通过另一数据库连接断言已提交的精确记录及隔离原文，向队尾追加未被旧批次读取的消息，然后 `Process.Kill` 并 `Wait`。核对队列全部原始字节未变、已提交记录未变、注册锁无 TTL 且再次注册被拒。
- 等待旧 MySQL connection ID 从 PROCESSLIST 消失后，执行恢复文档规定的 token 比较删除；由全新子进程消费，核对私聊文本/图片及群聊音频全部字段。新消费者正常退出释放注册，随后再重投完整队列，验证消息不重复、审计原文/原因/时间不改变、队列继续清空。
- 子进程只通过 stdin 接收本轮 fixture 配置，校验本机地址和实际 schema，不调用应用配置加载或迁移。父进程掌握随机 runId 与隔离资源；清理顺序确保子进程先退出，再释放连接和回收 fixture。所有等待有超时，使用提交标记和数据库会话状态而非固定 sleep 推测完成。
- 复审检查屏障位于真实事务返回之后，DB/队列断言独立于子进程日志；子进程异常退出、未到达屏障、恢复失败及资源清理错误均使测试失败。同步测试说明及开发规范，修正文档中仍称 P1.4.2 未完成的过期状态。

### 验证

Windows amd64；`go version go1.27.1 windows/amd64`，测试进程清空 GOROOT，设置 GOTOOLCHAIN=go1.27.1、仓库 `.gocache`、-mod=readonly、不存在的 ICHAT_CONFIG_FILE。Docker Engine 实测 28.4.0；沿用固定摘要的隔离 MySQL/Redis 镜像。首次沙箱内 Docker 查询被拒，提升权限后检查和测试成功；该环境拒绝不计业务回归证据。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly -tags=integration ./scheduledTasks -run '^TestLegacyCrashWorkerProcess$' -count=1` | 编译及未启用子进程入口通过，不访问外部依赖；不是崩溃验收 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -race -tags=integration ./scheduledTasks -run '^TestLegacyWorkerCrashRecoveryIntegration$' -count=1 -v -timeout=8m` | 三场景全部通过；用例 67.16 秒、包 70.527 秒，无 Skip/race 报告 |
| 新增 Go 文件 gofmt、`git diff --check` | 通过 |

runId：消息提交 `ea7d58899ffc61e5e287d6029d06c255`（22.44 秒）、隔离提交 `6462e7b73ed6541a99eadcd183ec4173`（22.47 秒）、整批提交 `e0929eab10b1f47998fa6f9543d7ee34`（22.26 秒）。三套 fixture 清理均成功。每个场景强杀一个实际消费子进程，再运行两个正常消费子进程完成恢复及重投对账。

本轮为现有 L3/L4/L5、F03/F04 增加真实进程退出证据，未重跑原始旧版代码，不能宣称本轮新增了旧版失败实测。尚未覆盖实际 COMMIT 响应丢失、事务进行中强杀、Redis 网络黑洞/数据丢失、全服务器信号停机、完整 L1—L12 汇总及 MQ 故障；P1 总项保持未勾选。

建议 commit message：`test(worker): verify legacy crash recovery after durable commits`

下一小点：**P1.8.2：L1—L12 回归映射、缺口补齐和 P1 汇总验收**。本轮不继续下一小点、不自动提交。

## P1.8.2：L1—L12 回归映射、缺口补齐和 P1 汇总验收

状态：实现、代码审查及汇总验收完成，P1 按第 3.3 节与第 12 节边界收口。起点 `f1ec401`（P1.8.1 已提交）；开始时仅原有两份规范/进度文档未跟踪。本轮未修改生产 Go 代码、schema、依赖或开发配置，未自动提交。

### 审查结论与改动

- 新增 [旧消息链路回归对照](legacy-reliability-acceptance.md)，将 L1—L12 映射到代码边界、41 个可执行测试及精确状态断言；按实际证据区分历史修复前失败、本轮通过、受控错误注入与真实进程崩溃。F01—F04 均有相应旧链路回归；不将后续 MQ 故障矩阵提前勾选。
- 发现 P1.6 改为 MySQL 历史查询后，`TestMessageHTTPIntegration` 的历史成功断言已经不能证明 PUBLISH 失败仍保留缓存。本轮直接读取 Redis ZSET，检查唯一成员包含原始消息 JSON，同时保留成功响应、脏队列及命令顺序断言。
- 新增 `TestLegacyAcknowledgementUnknownIntegration`：129 条队列中，第一批的坏记录及 127 条有效消息已提交，真实 Lua 成功裁剪 128 条后由 hook 返回 EOF。检查错误同时保留 EOF 和所有权失效分类，第 129 条尚未落库且原文留在队列，worker 后续调用永久拒绝，新实例也不能抢占未释放注册。
- 原 worker 同步调用已经结束后正常释放注册，用新 owner 消费尾部，再重投完整 129 条并分两轮确认；全部消息逐字段相同、隔离原文/原因/时间保持不变。该测试是 Lua 成功后的错误注入，不冒充网络断线；P1.8.1 的三个真实子进程强杀场景本轮一并重跑。
- 复审事务返回与 ACK 顺序、精确队首比较、提交/隔离幂等、消息身份和历史权限、订阅退出、测试资源归属与退出顺序。确认二进制队列用例使用真实 NUL/换行字节。未发现需要在本小点改动生产逻辑的阻断问题。
- 所有新增故障注入均位于 integration `_test.go`；默认测试不启动 MySQL/Redis。文档中的 41 个测试名全部核实存在，且在本轮完整 JSON 结果中逐一找到 pass。

### 验证

Windows amd64、Go 1.27.1；测试进程清空 GOROOT、设置 GOTOOLCHAIN=go1.27.1、仓库 `.gocache`、-mod=readonly、不存在的 ICHAT_CONFIG_FILE。集成资源继续使用固定 digest 的独立 MySQL/Redis，隔离用例实测 MySQL 8.0.44、Redis 7.4.9。

| 命令/核对 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目默认测试连续两轮通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含新增集成代码 |
| `go test -mod=readonly -race -tags=integration -p 2 ./... -count=1 -shuffle=on -json -timeout=15m` | 完整通过；JSON 事件覆盖约 319.38 秒，121 个顶层测试（含辅助入口），含子测试共 358 条 pass；零 fail、零测试用例 skip，无 race 报告 |
| 对照表的测试名与实际结果核对 | 41 个名称全部存在并通过 |
| gofmt、`git diff --check` 及新文件 `git diff --no-index --check` | 无格式或空白错误 |

JSON 原始结果保存在本机被忽略的 `loadtest-results/p1-acceptance.jsonl`，不自动加入版本控制。无测试文件的包产生 package-level skip，未计作测试用例跳过。完整集成不运行 benchmark，耗时不作为性能基线。

| 包 | 本轮包耗时 |
| --- | --- |
| 根包（bootstrap/cron） | 48.730 秒 |
| cmd/migrate | 24.699 秒 |
| database | 209.458 秒 |
| scheduledTasks | 137.107 秒 |
| service | 104.345 秒 |
| tests/integration | 61.895 秒 |

关键用例：新增确认 unknown 23.53 秒；三个实际崩溃场景合计 67.13 秒；真实消息 HTTP 20.23 秒；历史权限/完整性 20.54 秒；原子队首确认 20.21 秒。包并行运行，包耗时不能相加当作总时长。

本轮创建 26 套独立 fixture，所有 cleanup 均成功。主要 runId：

| 场景 | runId |
| --- | --- |
| 确认 unknown | `11fba265e6bda9ef064a43b3e1499690` |
| 消息/隔离/整批提交后崩溃 | `54051aa3a284dc77941e5c3b6de834db` / `7681a67451757b8147d7bddfd7ebda98` / `23726ffc73d177987cf8da051b6e6545` |
| 消息 HTTP / 历史 / 队首确认 | `e8dce4564063fbf2779c4805edd28b6c` / `43c39b8327047714d6aaa9e893dfe83f` / `c4afbfda8a85112fd64df1ab6a2588bd` |
| 幂等仓储 / 隔离 / worker 同步 | `e5cc13a5227edfcea34d87359e76ef0c` / `6864f5c032269cbaca69385c0b5367b0` / `97c5380b0980128f61a8bc00831eb48b` |
| 所有权 / PubSub / WebSocket | `3dc33aec148f38316e40ca4381072a58` / `6d3f085cd5b002d474b449e62cb2e28c` / `2880273d8196503c25b946e811c53835` |
| MySQL 停机恢复 / 迁移 | `24008f37d17fd191cc7281b742adac4f` / `4df0102a2bfaa436c4094d92cb8a0f99` |
| 环境隔离两套资源 | `6b9b43ac726a63995f837bdd3e48d51c` / `d7a4ef9a203ecf3ddd4bdaada4123649` |

随机种子：database `1791615216536843400`、scheduledTasks `1791615247891635900`、service `1791615391185167500`；其余包种子在 JSON 日志中。

### 收口与交接

开发规范 P1 四项已依据本轮完整通过及历史关键失败证据勾选。仍未验收实际 COMMIT 响应丢失、在途事务强杀、Redis 网络黑洞/数据丢失/复制切换、全服务器信号停机和容量；不具备自动接管或数据库 fencing。Redis List 的持久化边界、人工恢复前置条件、历史时间戳分页局限保持不变。MQ、Outbox、客户端 ACK/seq 补拉及 F05—F38 按后续阶段实现，不能由本次 P1 收口宣称完成。

建议 commit message：`test: close P1 regression coverage and verify uncertain legacy acknowledgements`

下一小点：**P2.1：DTO、规范化和身份/hash**。先按规范第 5 节定义新命令/消息的数据边界、规范化与稳定身份/hash 测试；schema 与唯一约束留在 P2.2。本轮不继续下一小点、不自动提交。

## P2.1：DTO、规范化和身份/hash

状态：实现、代码审查和本小点测试完成（2026-10-11，Australia/Sydney）。起点 `a0eddea`（P1.8.2 已提交）；开始时仅原有两份规范/进度文档未跟踪。未修改旧生产入口、schema、依赖或开发配置，未自动提交。P2 整体尚未验收。

### 实现与明确边界

- 新增独立 `messaging` 包：请求 DTO、规范命令、发布信封、统一消息 DTO、字符串序号、枚举、错误分类及共享校验。只依赖 Go 标准库，无 DB/Redis/MQ 初始化。
- senderId 由 Normalize 的鉴权 UID 参数提供；请求 DTO 没有 sender 字段，严格解码拒绝未知字段。UUID 规范为小写标准连字符形态，目标正 uint64 ID 规范为无前导零十进制；私聊会话按数值排序生成，群会话按群 ID 生成。好友/群权限留在消费者业务事务。
- commandId 使用固定版本 domain、sender/clientMessageId 的 UTF-8 字节长度前缀编码后 SHA-256；payloadHash 对 schemaVersion、sender、目标类型/ID、media、content 按固定顺序编码，不包含 clientMessageId、发布 attempt、服务端时间或客户端时间。完整编码和独立固定向量写入 [消息协议 v1](message-protocol-v1.md)。
- 本文补充实现所需的精确输入规则：正文 1–1024 个 code point、JSON body 至多 16384 字节；正文不 trim、不替换 Unicode，空白/NUL/组合字符保留。请求 ID 允许规范化前导零，信封只接受规范形式。字段枚举区分大小写，旧 iota 不进入新协议。
- 严格 JSON 拒绝未知/重复/大小写别名字段、null、类型错误、多对象、非法 UTF-8 与孤立 UTF-16 代理项。合法代理对和字面 U+FFFD 保留，避免标准 JSON 解码器的隐式替换改变正文。字段语义由 Normalize/ValidateEnvelope 检查；DecodeRequest 单独调用只保证结构与字节边界，不能替代鉴权后 Normalize。
- 每次 NewEnvelope 生成独立随机 v4 publishAttemptId；ValidateEnvelope 从信封重算身份/hash，检查 canonical UUID、ID、版本及时间，不信任传入摘要。摘要不是认证签名，后续仍需可信 broker 写权限。
- 客户端可选时间规范为 UTC RFC3339Nano，不参与业务摘要；检查严格时间格式、最多 9 位小数，避免 time.Parse 的宽松接受与截断。MySQL 时间精度尚待 P2.2 决定，不将协议纳秒精度宣称为数据库保证。
- Sequence 内部 uint64、JSON 必须为十进制字符串；0 可作游标，消息 DTO 构造拒绝未分配的 0。NewMessageDTO 重验规范命令并令 messageId=commandId，但不分配 seq、不证明事务已提交。后续仓储只能在正确提交边界使用。

### 审查与验证

审查输入/服务端字段分离、数字范围和排序、hash 编码歧义、所有不可变字段敏感性、元数据排除、Unicode 原文保留、JSON 重复字段及大小写别名、导出结构可变后的再校验。补上独立 Python hashlib/struct 计算的 ASCII 与中文/emoji/CRLF/NUL 固定向量，避免只用同一实现对照自身。未发现本小点阻断问题。

Windows amd64、GOTOOLCHAIN=go1.27.1、清空 GOROOT、仓库 `.gocache`、-mod=readonly；完整默认测试使用不存在的 ICHAT_CONFIG_FILE。未启动 Docker 或访问开发数据库。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./messaging -count=1 -cover` | 新协议测试通过，语句覆盖率 95.9% |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on` | 全项目连续两轮通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含现有集成代码 |
| `go test -mod=readonly ./messaging -run '^$' -fuzz '^FuzzRequestNormalization$' -fuzztime=20s -parallel=2` | 通过，82,074 次执行；实际包耗时 21.976 秒，无失败语料 |
| `go test -mod=readonly -race ./messaging -count=1 -cover` | 补上最终 Unicode 固定向量后通过，1.833 秒，覆盖率 95.9% |
| gofmt、新文件空白检查 | 通过 |

默认回归覆盖同键重试、不同键同内容、发送者/目标/类型/media/content 变更、正文不 trim、不做 NFC/NFD、0/溢出/大于 2^53 的 ID 和 seq、媒体枚举、1024 字符和 16384 字节边界、合法/非法代理项、请求伪造字段、信封篡改、未知版本、发布 UUID 和时间规范化。模糊测试检查解码/规范化无崩溃及合法命令经 JSON 往返保持一致；短时 fuzz 不代表穷尽输入空间。

未执行真实集成重跑：本轮无旧业务接线、迁移或数据库改动，全项目默认回归和集成静态检查覆盖兼容性；真实协议存储/权限/幂等事务、MQ 发布与终态/补拉测试在相应阶段接入。HTTP 读取前内存限制尚需适配层落实，当前 byte-slice decoder 只检查已传入字节数。

建议 commit message：`feat(messaging): define canonical v1 DTOs and deterministic message identities`

下一小点：**P2.2：schema 与唯一约束**。按规范第 5.2 节实现版本化迁移、数据库模型与真实 MySQL 约束测试，明确 ID/hash 的 ASCII/binary 语义、uint64 数值和时间存储精度。本轮不继续下一小点、不自动提交。

## P2.2：schema 与唯一约束

状态：实现、代码审查和本小点测试完成（2026-10-11，Australia/Sydney）。起点 `439cbf9`（P2.1 已提交）；开始时两份开发文档为既有未跟踪文件。本轮未执行开发库迁移、未重置任何开发数据、未自动提交。P2 整体未验收。

- `migrations/001_reliable.sql` 创建 `message_command`、`message`、`conversation`、`message_outbox`、`message_delivery_cursor`、`message_quarantine` 六表：命令逻辑键和 commandId、会话 seq、Outbox 命令/事件类型及投递游标均有数据库唯一约束；增加历史、Outbox 到期/租约、隔离审计索引。状态 CHECK 限制命令仅落库 persisted/rejected 终态，Outbox 租约字段与状态一致；conversation 身份字段与规范 ID 一致。跨表的游标单调且不超过 last_seq 留给 P5.6 事务路径保证，MySQL CHECK 不能直接表达该跨行约束。
- `migrations.ApplyReliable` 用同一 MySQL 连接取得命名锁，只有空 schema 可以首次迁移；将规范化换行的 SQL 定义 SHA-256 和六表实际 `SHOW CREATE TABLE` 指纹记录到版本表。重复执行核验版本/定义/结构，旧表、半完成 DDL、未知版本和结构漂移全部显式报错，不自动重建或删除。MySQL DDL 不具整批事务回滚能力，失败后的部分 schema 需人工检查。`cmd/migrate-reliable` 显式运行新版迁移并在失败时非零退出；旧 `MigrateLegacy` 遇到新版/部分新版表时拒绝 `AutoMigrate`。
- 新版仓储模型集中在 `models/reliable.go`，与旧 `models.Message` 分离，不使用 GORM 自动建新版表。ID/hash 和会话键为 ASCII 二进制语义，正文为 `utf8mb4` 且最多 1024 字符，整数为无符号 `BIGINT`，时间为 `DATETIME(6)`。真实 MySQL 会对多于六位的小数进行舍入，因此 P2.3 写入前须显式截到微秒，不能默默改变 P2.1 的纳秒协议值。
- 隔离 MySQL 8 集成测试验证并发首次迁移、重复迁移和保留数据、旧迁移保护、唯一键/CHECK/FK、1024 字符边界、中文/emoji/NUL 原文、二进制隔离原文、最大 uint64 游标、时间精度、结构漂移及空版本行的半完成状态。测试只在随机 runId 的 Docker fixture 中执行，并完成清理。CLI 配置失败的真实退出码也由单元测试覆盖。

验证：`go test -mod=readonly ./... -count=1`、`go test -mod=readonly -race ./... -count=1`、`go vet -mod=readonly -tags=integration ./...`、`go test -mod=readonly -race -tags=integration ./migrations -run '^TestReliableSchemaIntegration$' -count=1 -v -timeout=8m` 均通过；`git diff --check` 通过。隔离集成 runId `691101dec31a7b8dc98aa30df8f98430`，测试约 20 秒，清理成功。

迁移尚未接入业务入口；新版六表不包含旧账号/关系/群表，开发环境的数据重置与业务切换仍按 P7 进行。P2.3 需要在不提交 `processing` 状态的前提下实现命令逻辑键竞争、同键同 hash 复用和异 hash 冲突；本轮没有实现该事务路径。

建议 commit message：`feat(messaging): add versioned reliability schema and MySQL constraints`

下一小点：**P2.3：幂等命令结果**。只实现命令终态及同键/异 hash 的事务判定和并发回归；权限锁、conversation seq 与 Outbox 原子写入分别在后续小点完成。本轮不继续下一小点、不自动提交。

## P2.3：幂等命令结果

状态：实现、代码审查和本小点测试完成（2026-10-11，Australia/Sydney）。起点 `bc89ce4`（P2.2 已提交）；开始时仅两份开发文档为既有未跟踪文件。本轮未改 schema、依赖、旧入口或开发配置，未迁移开发库、未重置开发数据、未自动提交。P2 整体验收尚未完成。

- 新增独立注入 `*sql.DB` 的 `MessageRepository.ExecuteCommand`。先调用 P2.1 共享 `ValidateCommand`，再在事务内用只对既有 commandId 做 no-op 的唯一键插入及 `FOR UPDATE` 完成命令身份竞争；不采用先查后写、不吞通用 SQL 错误、不覆盖原 hash/结果。命令新归属才运行处理回调，已有同 hash 返回完整原终态及原时间，异 hash 返回 `ID_CONFLICT` 且不附带原成功结果。
- v1 CHECK 不允许事务内 `processing`，本轮没有改写已提交的 migration v1。内部用带随机 `__claim:` 原因的临时 rejected 行占位；私有标记只用于归属判断，避免依赖受连接 `CLIENT_FOUND_ROWS` 设置影响的插入行数。回调只能获得隐藏 Commit/Rollback 的事务 SQL 句柄；它是受信任的内部处理函数，禁止 DDL、SQL 事务控制、外部网络调用及另开连接写业务。仓储验证最终 decision、带本次标记条件 UPDATE、重新读取真实终态，再执行 COMMIT。非法 decision、回调错误、SQL 错误、取消、panic 和标记被篡改都回滚。已提交的占位标记作为数据不变量错误处理，不自动抢占或重做。
- `CommandDecision` 支持 persisted 或带稳定大写原因码的 rejected；消息/会话身份由规范命令派生，不由回调任意改写。拒绝重试不再次检查当前权限、不能变成后来的成功。意外 commandId 唯一键碰撞而逻辑键不存在时返回不变量错误，不能误作合法幂等命中。
- 终态时间使用数据库 `UTC_TIMESTAMP(6)`；读取 `DATETIME` 的显式格式化文本并按 UTC 解析，避免旧 MySQL 池的 `loc=Local` 将 UTC 时钟值解释为本地时间。协议客户端时间只参与共享校验，本步不写 message.client_sent_at；P2.5 仍须落实显式微秒截取。
- 新命令 COMMIT 报错返回 `ErrCommandCommitUnknown` 并保留底层错误，返回结果为空且不自动重试。既有终态的 replay/conflict 不改数据，使用回滚释放锁；回滚失败也不返回成功结果。SQL 暂时错误及有界重试归后续 consumer；本轮仓储不把数据库错误伪装成业务 rejected。

验证：

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./database -run '^TestCommand' -count=1 -v` | 通过；无外部 I/O 的输入校验及事务边界测试 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，含新版集成代码 |
| `go test -mod=readonly -race -tags=integration ./database -run '^TestCommandRepositoryIntegration$' -count=1 -v -timeout=8m` | 通过；最终集成用例约 21 秒，包耗时 23.763 秒，无 Skip/race 报告 |
| gofmt、空白检查 | 通过 |

真实 MySQL 覆盖：16 并发同键只有一条规范命令、一条消息和一次事务内测试写入；两种 hash 并发竞争只有一个先提交结果，另一种 hash 的八次请求全部冲突；顺序正文/目标冲突不覆盖消息；不同键同内容及不同发送者同客户端键独立（含最大 uint64 sender）；持久拒绝在权限变化模拟下仍复用；不同时区/CLIENT_FOUND_ROWS 的独立池给出相同结果。阻塞中的占位及业务写入对其他连接不可见，等待者超时不返回虚假终态；第一事务失败后新调用可安全接管。回调失败、panic、非法 processing、SQL 错误、取消及占位篡改全部没有残留命令/测试写入，并可用原键重试成功。另覆盖异常唯一键碰撞和已提交内部标记检测。

默认测试通过私有 database/sql connector 注入 begin/claim/read/处理/finalize/commit/rollback 故障，验证失败没有终态返回、panic 传播且回滚、Commit 结果未知不自动重试。该故障注入不是实际 COMMIT 响应丢失证明；真实连接断开、进程退出、MQ ACK 与提交不确定恢复仍归 P4.6/P8。

隔离 runId：首轮 `bd93958f2e0de3d6d0080f3adb6fcab1`，最终 `16c1d6649571c33fb58992861ea14f26`；fixture cleanup 均成功。测试中的固定 seq=1 消息和 `command_test_effect` 只用于验证回调的事务效果，不是生产序号分配/权限/Outbox 实现。当前 handler 是后续仓储接入点，调用方必须在同一事务完成实际业务写入后返回可信 decision；本轮没有开放生产发送接口。

建议 commit message：`feat(messaging): add transactional command idempotency and terminal results`

下一小点：**P2.4：权限事务和锁顺序**。在新命令的同一事务内校验目标、好友关系和群成员，并修复相关群/关系写路径的事务句柄及锁顺序。本轮不继续下一小点、不自动提交。

## P2.4：权限事务和锁顺序

状态：实现、代码审查和本小点测试完成（2026-10-11，Australia/Sydney）。起点 `9a02026`（P2.3 已提交）；开始时两份开发文档为既有未跟踪文件。本轮未改 schema 或依赖，未迁移开发库、未重置开发数据、未自动提交。P2 整体验收尚未完成。

- 新增 `database/message_permissions.go` 和 `MessageRepository.ExecuteAuthorizedCommand`。先复用 P2.3 的命令身份竞争，只有新命令才在同一事务中检查权限；私聊要求两个有效用户、不同发送/接收者及双向有效好友关系；群聊要求有效发送者、未删除群及有效成员关系。群主不能绕过成员条件。所有授权读取使用 `FOR SHARE`，读取实际行后检查软删除状态，权限锁持续到业务回调和命令终态的事务结束。
- 稳定拒绝原因包括 `SENDER_NOT_FOUND`、`TARGET_NOT_FOUND`、`SELF_SEND_NOT_ALLOWED`、`NOT_FRIENDS`、`NOT_GROUP_MEMBER`。拒绝不调用业务写入回调；数据库 SQL 错误保留原始原因并回滚，不伪装成业务拒绝。原命令成功或拒绝后，即使当前权限改变，同 hash 重试仍复用原终态；异 hash 仍为冲突，不能重新授权后覆盖结果。
- 新增 `database/contact_locks.go`，统一锁顺序为命令 → 数值 ID 升序的用户 → 群（如涉及）→ `(owner_id,target_id,type)` 升序的关系 → conversation（P2.5）。现有联系人写路径采用相容的排他锁：加好友在同一事务锁定双方用户并重新核验手机号对应身份，再按关系键顺序创建双向关系；创建群锁定有效群主，沿用 P0.6 已修复的群/群主成员关系原子写入；加入群锁定并校验用户和群后写成员关系；删除群锁定群后核对群主并软删除。
- 加入不存在或已删除的群返回新业务错误 `ErrGroupNotFound`，service 保留原统一响应形式并避免泄露 SQL 错误；不存在的用户不能创建群或加入群。删群仍保留既有成员记录，已删除群不能发送或加入。本轮没有添加取消好友、退群或恢复关系接口；以后实现这些写路径时必须遵循同一锁顺序，既有软删除关系重新添加的行为没有扩展。

代码审查检查共享锁与现有排他写路径的顺序、事务句柄贯穿、手机号查询后的身份重验、软删除和缺失行区别、业务错误/基础设施错误分类，以及 replay/conflict 不重新授权。更新原创建群事务故障替身以支持事务内用户锁，并补充锁查询失败回滚；新集成代码只在独立 fixture 创建旧身份三表，不使用 GORM 迁移新版消息表，末尾再次核验新版六表结构指纹未改变。

验证环境：Windows amd64、`GOTOOLCHAIN=go1.27.1`、清空 GOROOT、仓库 `.gocache`、`-mod=readonly`；测试的 `ICHAT_CONFIG_FILE` 指向不存在路径。真实测试使用固定镜像的独立 Docker MySQL 8.0.44 / Redis 7.4.9 fixture。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./database ./service -count=1` | 通过；权限决策/锁顺序、SQL 故障和联系人业务错误单元回归 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含新增集成代码 |
| `go test -mod=readonly -race -tags=integration ./database -run '^TestMessagePermissionsIntegration$' -count=1 -v -timeout=8m` | 通过，七组子测试，包耗时 26.777 秒；无 Skip/race 报告 |
| `go test -mod=readonly -race -tags=integration ./service -run '^TestContactWritesMySQLIntegration$' -count=1 -v -timeout=8m` | 最终回归通过，包耗时 23.482 秒；实际验证旧 HTTP 写入、重复/非群主失败、CHECK 故障原子回滚、取消、SQL 错误及关闭池 |
| gofmt、`git diff --check` 和新增文件空白检查 | 通过 |

权限集成 runId `7278109125a24f8f9c8cafd238ec39a8`，联系人最终回归 runId `912c4a28ce31ec4f41a1d7adc53cdc1c`；fixture cleanup 均成功。新增真实用例覆盖十二种权限拒绝、权限变化后的成功/拒绝原终态复用及内容冲突、发送持锁期间取消好友/删除群/加入群/成员软删除超时等待、发送完成后的变更和新命令拒绝、删群后入群不产生孤立关系、授权 SQL 故障不残留命令占位，以及十六个正反方向并发加好友只有一组双向关系。取消好友/成员变更/关系恢复只在测试 fixture 内执行，不代表新增生产端点。

当前新授权仓储尚未接入旧发送入口、HTTP 202 或 MQ。权限拒绝已有命令终态，但结果 Outbox 留给 P2.5；测试中的固定 seq=1 和测试副作用表仅证明授权与写入的事务边界，不是生产序号分配。消息、conversation seq 和 Outbox 的统一原子写入、consumer 死锁重试及真实 COMMIT 响应丢失恢复仍待相应小点落实。本轮无需开发库迁移。

建议 commit message：`feat(messaging): enforce transactional permissions and contact lock order`

下一小点：**P2.5：seq、消息和 Outbox 原子提交**。实现会话锁定与连续序号分配，将消息、命令结果及对应 Outbox 放入同一事务，并验证失败不产生序号洞。本轮不继续下一小点、不自动提交。

## P2.5：seq、消息和 Outbox 原子提交

状态：实现、代码审查和本小点测试完成（2026-10-11，Australia/Sydney）。起点 `cfa8afa`（P2.4 已提交）；开始时两份开发文档为既有未跟踪文件。本轮未改 schema、依赖、开发配置或旧入口，未迁移开发库、未重置开发数据、未自动提交。P2 整体验收尚未完成。

- 新增 `database/reliable_message_persistence.go` 和生产事务入口 `MessageRepository.PersistCommand`。复用 P2.3 的命令键竞争与终态收口、P2.4 的同事务权限锁；只在新命令且权限允许时创建/定位规范 conversation。no-op insert 不改已有会话元数据，随后 `FOR UPDATE` 核验私聊参与者或群身份、读取 last_seq，按 last_seq+1 插入规范 message 并条件更新计数，继续持有锁直到整个事务提交。
- 成功命令在同一事务生成一条 message、persisted 终态、message.persisted 和 command.result 两条 Outbox；权限拒绝只生成 rejected 终态和 command.result，不创建 message/conversation，也不推进已有会话序号。消息/计数/任意事件/终态收口任一步失败均回滚。已有同 hash 终态和异 hash 冲突都不执行新写入，不消耗 seq、不覆盖消息或事件。COMMIT 故障继续返回 `ErrCommandCommitUnknown` 和空结果，不自动 ACK/重试；调用方恢复规则仍沿用原业务键。
- 序号为 uint64 且检查上限：last_seq 已为最大值时返回 `ErrConversationExhausted`，回滚命令占位，不回绕为 0、不提交虚假的业务拒绝。异常会话身份或计数条件更新行数返回不变量错误，SQL 错误保留底层原因；consumer 的分类、隔离与有界重试仍待 P4。
- 新增 `messaging/outbox.go`：两类内部事件 DTO、eventType 枚举和稳定 eventId。身份使用独立 `ichat:outbox:v1` domain 及 commandId/eventType 的字节长度前缀 SHA-256；独立 Python 固定向量写入协议文档并由单元测试校验。message.persisted 保存路由、身份及字符串 seq 提示，不含正文；command.result 保存发送者、客户端键、hash、终态及成功时的完整规范 message DTO，拒绝时只有原因且无 message。普通 INSERT 的唯一冲突不被 UPDATE/IGNORE 吞掉，新事件为 pending、attempts=0、无租约/完成时间。
- message 的服务端时间取数据库 UTC 微秒时钟，DTO/事件使用同一值；clientSentAt 缺省为 NULL，非空先转 UTC 并显式截到微秒，绑定时钟字符串避开非 UTC DSN 的 loc 转换。元数据重试不参与 hash，保留首次提交值。真实测试在固定 MySQL 8.0.44 上验证 0001 与 9999 年边界；MySQL 官方只保证 DATETIME 的 1000–9999 年范围，更早年份在其他数据库版本部署前需兼容性验证，详见协议文档。

代码审查检查锁顺序、同会话提交可见性、身份核验、无符号上限、条件更新、事件集合唯一性、拒绝结果原子提交、SQL/COMMIT 错误传播及时间/字符原文保留。旧消息持久化文件和旧生产接口没有功能改动；低层 `ExecuteCommand` / `ExecuteAuthorizedCommand` 保留给可信内部处理，后续生产 consumer 必须使用包含全部写入的 `PersistCommand`。

验证环境：Windows amd64、`GOTOOLCHAIN=go1.27.1`、清空 GOROOT、仓库 `.gocache`、`-mod=readonly`；测试的 `ICHAT_CONFIG_FILE` 指向不存在的路径。真实资源为固定镜像的独立 Docker MySQL 8.0.44 / Redis 7.4.9 fixture。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./database ./messaging -count=1` | 通过，包含事件身份/输入、JSON 序号、客户端时间和仓储输入校验 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，含新增集成代码；最终故障注入调整后另执行 database 包静态检查并通过 |
| `go vet -mod=readonly -tags=integration ./database`、`go test -mod=readonly -tags=integration ./database -run '^$'` | 最终集成测试的静态检查及编译通过 |
| `go test -mod=readonly -race -tags=integration ./database -run '^TestReliableMessagePersistenceIntegration$' -count=1 -v -timeout=8m` | 最终全部通过，八组顶层子测试；包耗时 26.226 秒，无 Skip/race 报告，最终 schema 指纹一致 |
| gofmt、`git diff --check` 及新增文件空白检查 | 通过 |

最终隔离 runId `2be215af5a2e47f855f1a4db284faba9`，fixture cleanup 成功。真实用例覆盖：文本/图片/音频和 Unicode/NUL 原文；数据库 session 时区 +05:30、driver loc +13 及 CLIENT_FOUND_ROWS 下的一致存储；同键/元数据重试保留原值；拒绝只生成一条结果事件，权限恢复后原拒绝不变；十六个同键请求只持久化一次，十六个异 hash 竞争仅保留一个原结果；二十四个双向私聊和二十四个群聊独立命令均形成连续序号及各自两条事件。

故障回归在真实 MySQL driver 上修改测试请求的绑定值，分别触发既有 message/CHECK、last_seq/NOT NULL、第一事件/CHECK、第二事件/CHECK、终态/CHECK 错误，并核对所有命令/消息/事件回滚及原键恢复不消耗 seq；拒绝结果事件失败也不留下命令终态。另一种 driver barrier 在两条实际 Outbox INSERT 后暂停事务，独立连接看不到该事务的任何写入，后续反向发送等待并超时；分别验证释放后提交及取消后回滚，原键恢复和等待者重试后序号仍连续。uint64 上限测试刻意构造接近耗尽的独立会话，仅验证边界，不冒充真实连续历史。

首轮 runId `e97baf9727738296631ac4925eb504e9` 在测试用户的 phone 长度上失败；修正为不超过 varchar(32) 的唯一 phone/identifier 后重跑。第二轮 `7f25305bad4d5c3556c8486e2d379f12` 的业务子测试全部通过，但临时添加再删除 CHECK 的故障注入导致最终 SHOW CREATE TABLE 指纹不一致；已改为上述不修改 schema 的真实约束故障注入，再完整执行至通过。失败轮次均不记为验收通过，fixture cleanup 均完成；没有放宽 schema 指纹护栏或修改 migration v1。

当前只完成仓储与内部事件协议，尚未接入旧发送入口、HTTP 202 或 MQ；没有启动 Outbox worker 或发送通知。P2.6 状态查询、P2.7 同步/历史、P4 消费和提交不确定恢复、P5 通知领取/发布仍待后续阶段。本轮无需开发库迁移；实际开发 schema 重置/切换继续归 P7。

建议 commit message：`feat(messaging): atomically persist messages, sequences and outbox events`

下一小点：**P2.6：状态仓储**。以鉴权发送者、clientMessageId 和 payloadHash 查询规范终态/冲突或 pending_or_unknown，并明确数据库错误与技术隔离状态边界。本轮不继续下一小点、不自动提交。

## P2.6：状态仓储

状态：实现、代码审查和本小点测试完成（2026-10-12，Australia/Sydney）。起点 `cb150ac`（P2.5 已提交）；开始时两份开发文档为既有未跟踪文件。本轮未改 schema、依赖、开发配置或旧入口，未迁移开发库、未重置开发数据、未自动提交。P2 整体验收尚未完成。

- 新增 `messaging/status.go`：共享查询规范化、五种状态和 `CommandStatus` DTO。输入为已鉴权非零 uint64 发送者、规范 UUID 客户端键和 64 字符小写 hex hash；客户端键允许大写输入并规范为小写，hash 不放宽格式。查询身份复用原命令 domain 和编码，覆盖固定向量及最大 uint64。
- 新增 `database/message_status.go` 和 `MessageRepository.GetCommandStatus`。所有查询显式使用只读 REPEATABLE READ 快照及普通 SELECT；不锁行、不写命令、不查询队列。先按发送者/客户端键查询规范命令并核验身份及终态：异 hash 返回该次请求的 ID_CONFLICT；同 hash 返回原 persisted/rejected。规范命令始终优先于隔离记录。
- persisted 从同一快照中的 message/conversation 重建 DTO，核对消息归属、命令 ID/hash、会话、正 seq 和已提交 watermark；UTC 微秒读取不受 session 时区或 DSN loc 影响。rejected 返回稳定 reasonCode。发送者自己的历史终态不因之后解除好友、离群或删群而改变；当前历史/同步授权仍归 P2.7。抽取 `scanCommandRow` 供命令执行的锁定读取和状态快照复用，保持原幂等事务流程。
- 没有规范命令时，才按相同 sender/clientMessageId/payloadHash 检查 open/replay_pending 且 resolved_at 为空的隔离记录，不要求可缺省的 commandId。匹配返回 quarantined，两类均无记录返回 pending_or_unknown；均建议 `pollAfterMs: 2000`。冲突不暴露原消息/hash/拒绝原因，隔离不暴露原文、headers、transportId 或诊断错误；终态和冲突无轮询字段。
- SQL、连接池、快照释放及数据不变量错误返回空 DTO 和错误，不能降级为 pending。单元测试覆盖优先级、输入校验、只读事务参数、无锁读取、各阶段故障和损坏记录；真实 MySQL 用例覆盖发送者隔离、冲突/隔离字段边界、终态保持及一致快照。同步协议文档、开发清单和测试说明。

代码审查检查鉴权身份边界、规范终态优先级、同键异内容、隔离逻辑键匹配、无未提交结果/行锁、快照释放、原始错误传播、消息重建及 UTC 时间。当前仓储信任调用方传入的 authenticatedSender；未来 HTTP 必须从鉴权上下文获取 UID，不能读取请求中的 senderId。

验证环境：Windows amd64、`GOTOOLCHAIN=go1.27.1`、清空 GOROOT、仓库 `.gocache`、`-mod=readonly`；测试的 `ICHAT_CONFIG_FILE` 指向不存在的路径。真实资源为固定镜像的独立 Docker MySQL 8.0.44 / Redis 7.4.9 fixture。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./database ./messaging -count=1` | 通过，包含查询规范化、状态决策、失败与损坏记录边界 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 通过，含新增集成代码 |
| `go test -mod=readonly -tags=integration ./database -run '^$'` | 新增集成测试编译通过 |
| `go test -mod=readonly -race -tags=integration ./database -run '^Test(CommandStatusIntegration\|CommandRepositoryIntegration)$' -count=1 -v -timeout=8m` | 全部通过，包耗时 48.685 秒；原命令仓储回归 23.28 秒，新增状态仓储七组子测试 21.37 秒；无 Skip/race 报告，最终 schema 指纹一致 |
| gofmt、`git diff --check` 及新增文件空白检查 | 通过 |

原命令仓储回归 runId `ff0d25a227256b2b084ba8d45e6346f7`；状态仓储 runId `c6a37e725a66ff0b573c1b1ce40e0bf3`。fixture cleanup 均成功。状态测试的独立连接默认 READ-COMMITTED、session 时区 +05:30、driver loc +13，验证仓储显式事务选项及规范 UTC 输出。

真实并发验证有两个确定性屏障：其一在实际消息及两条 Outbox 写入后、提交前暂停发送，状态查询在 2 秒超时内返回 pending_or_unknown，无临时 rejected 泄露；释放并提交后查询 persisted。其二在状态查询首次实际命令读取后暂停，在另一连接提交规范命令并解决隔离行，再继续查询；本轮仍返回快照中的 quarantined，下轮返回 persisted，不因混合两个快照而误报 unknown。SQL 故障通过临时重命名本轮 fixture 的指定表触发真实 1146 错误，恢复名称后继续测试；消息正文损坏、调用前取消和已关闭连接池均返回错误及空结果。所有操作仅限归属已校验的隔离 schema。

当前只完成状态仓储与 DTO，尚未新增 HTTP 状态接口、接入旧发送入口或 MQ，也未实现隔离生产/审计/重放、Outbox 发布或同步/历史仓储。状态 HTTP 契约归 P6.1，隔离链路归 P4，Outbox 领取/通知归 P5。本轮无需开发库迁移；实际开发 schema 重置/切换继续归 P7。

建议 commit message：`feat(messaging): add authenticated command status repository`

下一小点：**P2.7：同步/历史仓储**。在 MySQL 权威快照中检查当前会话读取权限，按已提交 seq 有界补拉/分页，不跨越提交 watermark。本轮不继续下一小点、不自动提交。

## P2.7：同步/历史仓储与 P2 收口

状态：实现、自审和对应测试完成（2026-10-13，Australia/Sydney）。起点 `9e6c723`（P2.6 已提交）；开始时仅两份开发文档为既有未跟踪文件。本轮没有修改 schema、依赖、开发配置或旧入口，没有迁移/清空开发数据库，没有自动提交。P2 仓储范围验收收口，下一项为 P3.1。

### 实现与边界

- 新增 `messaging/history.go`：规范 conversationId 与游标校验、共享有界查询参数、SyncPage/HistoryPage DTO。鉴权读者必须非零，私聊参与者按数值严格递增，会话与游标不接受前导零；所有序号仍输出字符串，limit 默认 100、最大 500。
- 新增 `database/reliable_message_history.go`，实现 `MessageRepository.SyncMessages` / `HistoryMessages`。每页显式开启只读 REPEATABLE READ 事务，用普通 SELECT 在同一快照核对授权、会话身份、last_seq 和消息。没有写入、行锁、Redis/MQ 读取或服务器 deliveredSeq 依赖。
- 私聊只允许参与者，解除好友后可读保留历史；群聊检查未删除群及有效成员关系，群主没有特权。权限在本页快照判定，之后撤销不回收在途读取，下一页重新授权。授权通过但尚无消息会话返回水位 0、空数组；无权读取返回 `ErrConversationForbidden` 和空结果。
- sync 按 `afterSeq < seq <= highWatermark` 升序补拉，空页不推进游标，超过水位的输入返回错误。history 使用排他的 beforeSeq；省略时取最新页，显式 0 返回空页；降序选取后按升序输出，nextBeforeSeq 为本页最旧序号。最新历史不计算水位加一，最大 uint64 不溢出。各页独立快照，不承诺跨页冻结历史。
- 每次最多读取 limit+1 行，逐条核对预期连续 seq（含额外行）、persisted 命令对应关系、规范身份/正文 hash/媒体与 UTC 微秒时间。缺号、缺命令、数据损坏、SQL/迭代/关闭/快照释放错误均返回空 DTO 和错误，不返回部分页、不跳过消息、不伪造 hasMore=false。这是有界范围校验，不是全历史扫描。
- 更新协议文档、开发清单和测试说明，记录 beforeSeq/空游标/未来游标/无消息会话的明确规则及权限快照边界。HTTP 参数解析与响应映射、会话发现和客户端周期同步仍属 P6。

自审检查鉴权 UID 来源、私聊身份与群权限边界、普通读取与一致快照、会话字段核验、uint64 加减上限、lookahead 与 hasMore、消息正文 hash、UTC 时间、空页数组和错误时清空结果，以及查询/迭代/事务释放的资源处理。首次真实测试发现查询写错会话列名，已改为 migration v1 的 `private_user_a/private_user_b`，没有修改 schema 或绕过指纹校验。

### 验证

环境：Windows amd64、`GOTOOLCHAIN=go1.27.1`、清空 GOROOT、仓库 `.gocache`、`-mod=readonly`；默认测试的 `ICHAT_CONFIG_FILE` 指向不存在的路径。启动本机 Docker Desktop 后，真实测试只使用固定镜像的独立 MySQL 8.0.44 / Redis 7.4.9 fixture。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly ./database ./messaging -count=1` | 通过，涵盖输入、分页、权限、整数边界、缺号、损坏与失败清空结果 |
| `go test -mod=readonly -race ./... -count=1` | 最终全项目通过，无 race 报告 |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过，包含新增集成代码 |
| `go test -mod=readonly -tags=integration ./database -run '^$'` | 新增集成代码编译通过；最终增强用例另由实际集成运行编译验证 |
| `go test -mod=readonly -race -tags=integration ./database -run '^Test(CommandRepositoryIntegration\|MessagePermissionsIntegration\|ReliableMessagePersistenceIntegration\|CommandStatusIntegration\|ReliableMessageHistoryIntegration)$' -count=1 -v -timeout=8m` | 最终五组全部通过，包耗时 110.743 秒；新同步/历史用例八组子测试，20.74 秒；无 Skip/race 报告 |
| gofmt、`git diff --check` 及新增文件空白检查 | 通过 |

最终集中回归的隔离资源：

| 仓储测试 | runId |
| --- | --- |
| 权限 | `3e998bc9549d7508e1141a28af217482` |
| 命令幂等 | `ca9d48e98858534ca05d6cad9910bd12` |
| 状态 | `6885fe3955ffd40c56da19ccdf2bcd77` |
| 同步/历史 | `13186213fc3adbc0090fd44b557b7062` |
| 原子持久化/序号/Outbox | `6e608e67165078408a04a92ec25806b8` |

五套 fixture cleanup 均成功，各测试最后重新验证迁移指纹及不存在已提交内部占位。真实用例覆盖双向私聊完整正反分页、文本/图片/音频/Unicode/NUL 原文、UTC 微秒时间、服务器 deliveredSeq 已大但本地 0 仍完整恢复、空会话、当前群成员可读入群前历史、离群/删群拒绝下一页，以及解除好友后的保留历史。

新增查询连接默认 READ-COMMITTED、session 时区 +05:30、driver loc +13。真实 driver 屏障在授权 SELECT 后从另一连接提交新消息，再撤销成员/删除群；本页水位和内容仍为原快照，下一页拒绝。另一屏障在授权后首次创建消息会话，本页仍为水位 0，下一页补齐首条消息。持久化屏障在实际消息和两条 Outbox 写入后暂停发送，2 秒截止内同步不阻塞、不泄露未提交序号；提交后从原游标恢复。

故障测试在本轮 schema 内临时移动 seq、篡改后恢复正文、重命名后恢复指定表，实际验证缺号、hash 损坏及 MySQL 1146 错误都不返回部分页。最大 uint64 测试把两条已通过 PersistCommand 生成的消息及计数临时移动到上界，验证向前补拉和最新历史的局部范围没有回绕，随后恢复；不冒充完整连续历史。取消和关闭连接池也返回错误及空结果。

首轮真实 runId `478cb4bcad50a58c9db183da6d101591` 因上述错误列名失败，cleanup 成功，不记为通过；修正后 runId `c89723f65762520eef37688bf15afc92` 的六组子测试通过并成功清理，再增强一致快照和 uint64 用例，以上述最终集中回归结果为准。初始单元测试的高位 uint64 参数暴露了测试替身缺少 MySQL NamedValueChecker 行为，补上测试 driver 的无符号参数支持后通过；最终真实 MySQL 上界用例同时验证实际绑定。

### P2 收口与后续

P2 清单已全部勾选。集中回归再次确认并行同键唯一持久化、异 hash 不覆盖、权限拒绝仅有命令终态/结果事件、失败不留下序号洞或内部占位，以及同步/历史的当前授权和连续范围。这些结果只验收仓储范围；新生产 HTTP/MQ 接线、真实 COMMIT 响应丢失/进程崩溃恢复、通知 worker、ACK、客户端补拉、HA 与容量仍按后续阶段实施。当前不承诺端到端恢复完成，本轮无需开发库迁移。

建议 commit message：`feat(messaging): add authorized snapshot sync and history repositories`

下一小点：**P3.1：核验 broker/client 版本和隔离 vhost**。按固定版本源码与官方资料核验，再建立独立测试 MQ 资源和归属检查。本轮不继续下一小点、不自动提交。

## P3.1：broker/client 版本核验与 Docker 隔离测试 vhost

状态：实现、自审及本小点验证完成（2026-10-13，Australia/Sydney）。起点 `848dc60`（P2.7 已提交）；开始时仅两份开发文档为既有未跟踪文件。本轮新增测试用 RabbitMQ 和固定客户端依赖，没有修改开发配置、业务入口或 schema，没有清空开发数据，没有自动提交。P3 整体仍未完成。

### 版本与实现

- 复核官方版本信息、release notes 和已下载模块源码。原候选 4.3.5 / v1.14.0 调整为 RabbitMQ **4.3.6** / amqp091-go **v1.15.0**；客户端新版本修复 SASL 凭证复用，保留项目 Go 1.27.1。`go.mod/go.sum` 固定客户端，`go list -m -json` 实际记录客户端最低 Go 1.20 与模块校验和；镜像已实际拉取并固定 digest，架构 linux/amd64。
- 镜像为 `rabbitmq:4.3.6-management@sha256:8dd6e3570ddaa2ef82a6c3a8950e79c1f73893adfbdddbc374fc1f1ac8a0f5dd`。在 `docker-compose.test.yml` 增加可选 `mq` profile、稳定容器 hostname、独立命名卷、节点运行/本地告警健康检查，以及只绑定 127.0.0.1 的随机 AMQP/management 端口。
- 增加 `testenv.StartWithRabbitMQ`；复用原随机 runId、凭证、明确 Compose 文件/项目名、配置覆盖过滤与资源归属护栏。默认 `Start` 不启用 broker。MQ 启动后只读探测 management 就绪，核验实际版本，拒绝采用已有同名 vhost/用户，再建立 `ichat_test_<runId>` vhost 与 `ichat_app_<runId>` 独立随机密码用户。
- 测试 vhost 带 description 和两项 run 标签，默认 queue type=quorum。应用账号无管理标签，仅拥有本轮一个 vhost 中 `^ichat\.` 前缀的 configure/write/read 权限；管理凭证仅在本轮独立容器初始化/检查使用。所有归属、权限和版本验证通过后才暴露 MQ 配置。
- HTTP helper 限定 owned fixture/有效 runId/loopback endpoint，禁用代理和跳转，有界连接/请求/响应大小，不把原始响应正文放进错误；写操作不盲目重试。配置的 String 方法不打印密码，AMQPURL 仅作拨号输入。清理仍在全部 Docker 资源标签核验后逐个删除本轮明确 ID，不向共享 broker 删除 vhost/队列，不依赖 broker 可用才能判断容器清理归属。
- 复查 v1.15.0 的串行通知、5 秒通知丢弃边界、调用前取消与在途 I/O 不可取消、`Recovery: nil` 及 SASL 克隆。完整依据、固定校验和、使用方式和未覆盖边界写入 [MQ 测试环境](mq-test-environment.md)，同步 README、测试说明和开发规范。

自审检查可选 profile 与原 Start 的兼容性、资源枚举/归属校验/失败 cleanup 顺序、凭证生成与不打印、vhost/user 名称碰撞拒绝、权限仅限一个测试 vhost/资源前缀、management 无代理/跳转、只读就绪重试与写入失败边界、固定客户端源码行为和测试关闭顺序。没有添加生产 MQ 初始化、自动恢复、publisher 或 HTTP accepted 接线。

### 验证

环境：Windows amd64、GOTOOLCHAIN=go1.27.1、清空 GOROOT、仓库 `.gocache`、-mod=readonly。默认回归使用不存在的 ICHAT_CONFIG_FILE；真实资源只由隔离 fixture 创建。Docker image pull / inspect 验证上述 digest 与 linux/amd64，真实 management API 断言版本 4.3.6。

| 命令 | 结果 |
| --- | --- |
| `go list -mod=readonly -m -json github.com/rabbitmq/amqp091-go` | 实际版本 v1.15.0，最低 Go 1.20，模块/GoMod 校验和已记录 |
| `go mod verify` | 通过，全部模块校验一致 |
| `go test -mod=readonly -race ./... -count=1` | 全项目通过，无 race 报告 |
| `go test -mod=readonly -race ./tests/testenv -count=1` | 最终通过，2.294 秒；含名称碰撞、版本/元数据/权限错误、部分失败、配置就绪、URI/日志边界及 HTTP 超限/跳转/取消 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 新增真实测试编译通过 |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过，含新增集成代码 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQEnvironmentIntegration$' -count=1 -v -timeout=10m` | 最终五组子测试全部通过，测试 46.08 秒，包耗时 48.468 秒；无 Skip/race 报告 |
| `go test -mod=readonly -race -tags=integration ./tests/integration -run '^TestEnvironmentIsolation$' -count=1 -v -timeout=10m` | 原 MySQL/Redis 双环境回归通过，测试 39.12 秒，包耗时 41.602 秒；MySQL 8.0.44 / Redis 7.4.9，cleanup 成功 |
| gofmt、`git diff --check` 及新增文件空白检查 | 通过 |

最终 MQ runId：`f0e8fc1d628834dc7af4f012abdde55f` / `b5f5d448a75c237a25882cedd0a5166b`。原资源回归 runId：`c40390640c116bbc8c445a4e90d4a4f6` / `bd8f6eccfaa886e5934d1120b92b3a32`。四套 fixture cleanup 均成功，先清第一套 MQ 后显式核对其 Docker 资源为空，第二套的同名 quorum 队列中原 pending 消息仍完整。

真实 MQ 验证：两套环境使用相同 `ichat.test.probe.*` exchange/queue 名称和不同二进制正文；实际 queue type=quorum、单成员、durable、非 auto-delete；应用账号不能进入已存在的其他 vhost、bootstrap vhost，不能复用到另一 broker，也不能声明前缀之外的资源或访问 management。使用原始容量 1 通知缓冲、单 outstanding，刻意先读不可路由 publish 的 ACK，再非阻塞取得带匹配 MessageId/CorrelationId/正文的 Return；预取消 publish 不消耗下一个 tag。相同调用方 SASL 对象可建立两次真实连接，Recovery 始终显式关闭。

首轮 MQ runId `a8f4d3778b03301a63f58911c8414f46` / `d8a1616d74ae5e76aee3efe0331eaa96` 的业务断言通过，但测试 cleanup 对已显式关闭的连接再次 CloseDeadline 得到 `ErrClosed`，因此整轮失败，不能记为通过。已修正测试关闭 helper，只在重复 cleanup 接受该明确错误；重新执行至上述最终通过。首轮资源 cleanup 也成功，没有吞通用关闭错误或放宽归属护栏。单元测试编写时的复制 sync.Mutex 问题由 vet 检出并改为按索引取测试状态指针，最终静态检查通过。

### 后续边界

本阶段只完成版本核验、Docker 隔离资源和基本 AMQP/quorum probe 兼容性，未创建正式主/隔离 exchange/queue/策略，未实现可靠 DLX、生产 publisher、写入硬超时/epoch 重连、消费任务、HTTP 202 或 HA。return/ACK 用例验证后续排空设计的前提，不代替生产 accepted 判定的完整故障验收；probe 的单成员队列不代表节点容灾。常规本地 RabbitMQ 服务与正式拓扑在 P3.2 加入，业务仍使用原 Redis 消息链路。

建议 commit message：`test(mq): pin RabbitMQ versions and provision isolated Docker vhosts`

下一小点：**P3.2：本地拓扑及可靠 DLX**。使用已固定版本建立本地正式单节点 quorum 拓扑、主/隔离队列及可靠死信策略，核验必要 feature flags 和不一致时的失败行为。本轮不继续下一小点、不自动提交。

## P3.2：本地拓扑及可靠 DLX

状态：2026-10-13（Australia/Sydney）完成实现、自审、单元测试、静态检查和真实 Docker 验证。代码基线为用户提交的 P3.1 `7c09dd4`；本小点不代表整个 P3 已完成。

### 改动

- 常规 `docker-compose.yml` 增加与 P3.1 相同固定 digest 的 RabbitMQ 4.3.6 management 服务，固定 hostname、独立持久卷、loopback AMQP/management 端口及节点/运行/告警健康检查。`.env.example` 新增独立 admin/app 密码与两个端口；未修改已有开发 `.env` 或启动开发容器。
- 新增 `messaging/rabbitmq` 版本化声明和管理 API helper。按隔离 exchange/queue/binding、主 exchange/queue/binding 顺序创建；两条队列 durable/quorum、非 exclusive/auto-delete、group size=1、无 TTL/expiry，容量起点各 64 MiB、overflow=reject-publish。主队列 delivery-limit=50、at-least-once DLX 与固定路由；隔离队列 delivery-limit=-1，避免默认上限再次丢弃隔离原文。
- `PreflightLocal` 在所有写入前检查完整既有拓扑；`VerifyLocal` 仅只读检查，要求 broker 4.3.6、quorum_queue/stream_queue enabled、实际 members/online 各一且一致、running、精确 arguments 和唯一绑定。本地专用 vhost 拒绝 user/operator policies 及 effective policy 覆盖；不自动 delete、purge、改策略或启用 feature flags。
- 新增 `cmd/mqadmin init|check`。显式读取 `.env`/指定 dotenv 文件，进程环境可覆盖（包括空值），不读取应用配置。`init` 建立无管理标签、仅 `/ichat` 的 `^ichat\.` 权限应用账号，通过应用 AMQP 连接声明和复核；已有密码不覆盖、过宽或跨 vhost 权限失败，空权限可补齐。`check` 仅 GET 拓扑，不消费消息、不要求 app 密码。初始化和检查独立于旧业务 bootstrap。
- 管理命令设置真实 AMQP transport deadline（操作 45 秒、关闭 3 秒），关闭 Recovery；HTTP 单请求 5 秒、响应 1 MiB、禁代理/重定向，错误和 String 不包含密码/URL 凭证/响应正文。该一次性管理连接不是 P3.4 的生产 publisher I/O 超时实现。
- 新增 [本地 MQ 拓扑说明](mq-local-topology.md)，更新 README、测试说明及版本核验文档；记录 vhost 前导 `/` 的 URL 转义、空卷首次账号引导、容量/副本及 readiness 边界。

### 自审与故障证据

- 复核 Go/AMQP 依赖只在显式调用联网；默认单元测试不读开发配置或启动 Docker。预检先检查全部对象再写，主队列不匹配时也不会先创建隔离对象；参数 map 每次新建，调用者不能修改公共契约。检查 operator policy 优先级，避免只看声明参数误报可靠死信启用。
- 单元测试覆盖版本/缺失关闭 flag、classic/non-durable/exclusive/auto-delete、实际副本和在线状态、TTL/错误 DLX/overflow/容量/delivery-limit、用户与 operator 策略、有效策略、exchange/绑定不匹配，断言失败时零声明写入；覆盖隔离先创建、只读/取消、管理 HTTP 边界、凭证不泄露、账号幂等/部分创建恢复及拒绝过宽权限。
- 真实 fixture 正式拓扑重复声明两次成功。主队列 persistent 原文经 reject 进入隔离队列，MessageId、CorrelationId、DeliveryMode、中文/emoji/NUL 字节及 x-death 原因/来源保持正确。4.3 basic.reject(requeue=true) 递增 delivery-count，51 次失败超过 limit=50 后可靠隔离；隔离原文再失败 55 次仍可读取并最后 ACK。
- 解除隔离绑定后 `VerifyLocal` 失败，主队列死信仍计数 `messages=1, ready=0, unacked=0`，隔离队列没有投递；恢复绑定后按 broker 默认周期转发原文，源计数变为 0。真实 operator overflow 覆盖时初始化失败，移除本轮测试策略后哨兵原文仍在。另一专用 vhost 的已有 classic 同名队列初始化失败，原类型/消息保留，隔离队列没有被部分创建。
- 同一隔离 broker 中另建本地 `/ichat` 与 bootstrap admin，实际 Go 子进程两次执行 `mqadmin init` 和一次 `check` 均成功，应用密码能够真实连接和幂等声明。未用开发 `.env`、现有容器或卷，所有资源随本轮 fixture 归属清理。
- 首轮 `d4ef44719b9e3c975267f9784d165d1e` 除 DLX 恢复等待外的当时场景通过，但恢复用例只等 90 秒，整轮失败（122.269s），不能记为验收通过。检查 RabbitMQ v4.3.6 默认配置与 worker 源码确认重试周期 180000 ms，改为只对该恢复读取允许 4 分钟、其余读取 20 秒；保留真实 broker 默认值。最终完整重跑至下表通过；首轮及最终轮资源均成功清理。

### 验证

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Linux amd64、RabbitMQ 4.3.6 / amqp091-go v1.15.0。设置 GOROOT 为空、GOTOOLCHAIN=go1.27.1、GOCACHE 为仓库 `.gocache`、ICHAT_CONFIG_FILE 为不存在的测试路径。管理命令子进程使用临时独立 dotenv 文件，过滤继承的 RABBITMQ_* 覆盖。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./messaging/rabbitmq ./cmd/mqadmin -count=1` | 通过，2.545s / 2.243s |
| `go test -mod=readonly -race ./... -count=1` | 全仓库通过，未发现 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 带标签编译通过 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQTopologyIntegration$' -count=1 -v -timeout=10m` | 六个子场景全部通过；test=214.44s，包=217.022s；runId=`f5d4df60c6086c354889726dcbb84560` |
| `docker compose --env-file <临时测试 env> --file docker-compose.yml config --format json` | 解析通过；只输出/核验镜像 digest、hostname、两项 loopback 端口、卷和健康检查，不启动开发资源 |
| gofmt、`git diff --check` | 通过 |

### 后续边界

仅实现本地单节点正式拓扑、版本/配置复核、可靠 DLX 前提和上述失效恢复。不代表三副本 HA、broker 重启/磁盘丢失、容量饱和、生产 publisher/HTTP 202、消费者事务、隔离审计或受控重放。64 MiB 是开发参数，quorum 容量限制非严格磁盘上限；隔离失败无上限须在 P4 配合审计 worker 退避和人工处理。拓扑检查是时点结果，持续 readiness/告警与独立生产/消费连接由后续阶段装配。当前业务仍走 Redis 旧链路。

建议 commit message：`feat(mq): add local quorum topology and reliable dead lettering`

下一小点：**P3.3：有界 publisher 和 confirm/return 关联**。在独立测试环境实现有界 outstanding、mandatory/persistent 发布与正确关联，排除 return+ACK 时序导致误报 accepted；超时/unknown/epoch 与业务接入继续按既定后续小点实施。本轮不继续下一小点、不自动提交。

## P3.3：有界 publisher 和 confirm/return 关联

状态：2026-10-13（Australia/Sydney）完成实现、自审、race 单元测试、静态检查与真实 Docker 验证。代码基线为用户提交的 P3.2 `5d25d5c`；整个 P3 尚未完成。

### 改动与实现选择

- 新增 `messaging/rabbitmq/publisher.go`，显式接收已有且关闭 Recovery 的连接，创建 1—64 个私有 channel slots。连接所有者提供非零 connection epoch，每个 channel 具有独立编号、递增发布 tag 和独立原始 Return/Publish/Close 通知缓冲（容量均为 1）。部分构造失败关闭已创建 channels；publisher 拥有 channels，调用者拥有 connection。
- 每个 slot 只有一个执行中的 Publish，当前独占者直接读取原始通知，不创建 Return 转发 goroutine 或异步状态 map。池满立即返回 busy、不生成 attempt、不写网络；这是本小点采用的有界过载策略。关闭或所有 slot 退休返回 unavailable。
- 校验 Command 与有 deadline 的 context，获得 slot 后构造新的服务端 Envelope/随机 attempt，固定 exchange/routing key、mandatory=true、immediate=false、application/json、persistent、MessageId=commandId、CorrelationId=attempt。正文和逻辑身份使用 P2 协议函数，逻辑重试保持原键/内容但更换 attempt。
- ACK 分支先读取原始 Return buffer，再检查确认 tag、通知状态、channel 下一 tag 和取消状态，成功判定后归还 slot。Return 校验命令、attempt、路由、持久属性和原始正文；错误通知不会变成成功。只有匹配正确认且无 Return 才得到 accepted。
- 明确区分 not_published、unroutable、nacked 和 publish_unknown，失败返回 typed PublishError；网络调用后写错误或缺少完整证明均保守归 unknown。Return/NACK/写错误/等待超时/关闭/通知污染使 channel 永久退出池并关闭，不自动重建；晚到旧 ACK 不能进入另一 slot。
- 新增 [MQ publisher 说明](mq-publisher.md)，更新 README 和测试入口，明确 connection/transport 所有权、代码尚未接入业务，以及 P3.4 的网络黑洞/硬超时与自动重连边界。

### 自审与测试证据

- 复核固定模块 cache v1.15.0 的 Channel.dispatch、NotifyReturn/NotifyPublish/NotifyClose、GetNextPublishSeqNo 和 PublishWithContext，与官方固定 tag 对照。Return 的串行同步分派与 cap=1/单 outstanding 是排空判定前提；不使用容易产生时序窗口的异步 Return map。
- 单元测试确定性地让 peer 填满两个原始通知缓冲，先读取 ACK，再调用实际生产 confirmed 分支，断言匹配 Return+ACK 得到 unroutable 并退休。另验证正常连续 tag/新 attempt、池满零写入、两个 slot 并发、错误 tag/command/attempt/body、NACK、写错误、通知关闭、close+ACK、预取消/无限 context/篡改 Command/错误时间无写入、有限 confirm 等待和部分构造失败清理。
- 旧 slot 写失败退休后注入晚到 tag=1 ACK，另一 slot 的 tag=1 请求仍必须等自己的正确确认；证明通知空间隔离，不把裸 tag 作为跨 channel 的身份。Close 先停止接单，已进入发布调用但未取得证明的任务为 unknown；成功线性化后发生关闭不撤销既有 MQ 证明。
- 真实 broker 使用两个 slots 并发发布 12 次同逻辑命令，逐条从正式主队列读取和 ACK，核验 JSON 信封、稳定 commandId/hash、每次不同 attempt、AMQP 持久化/类型和中文/emoji/NUL 正文。
- 移除本轮主绑定产生真实 mandatory Return+ACK，publisher 得到 unroutable；恢复路由后旧 publisher 仍 unavailable 且没有新消息，显式新 connection epoch 从 tag=1 发布成功。没有自动重连或自动业务重试。
- 临时为本轮专用 vhost 注入容量策略 max-length=1，保留 reject-publish，实际 broker 最终 NACK。失败 slot 不再复用，所有此前 accepted 原文仍可逐条核验，未丢弃队头。该策略是故障注入，故意违反 P3.2 的 policy-free 契约；测试移除本轮策略并重新 VerifyLocal，资源最终按归属清理。
- 初次 Docker 运行 `df5c76a388d4be5638a079ea7520fbde` 因 Docker Desktop Linux engine 未运行，在任何资源创建之前失败，不能记为通过。启动 Desktop 并核验引擎 28.4.0 后完整重跑至下表通过；开发 `.env`、既有容器/卷未参与测试。

### 验证

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Linux amd64、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用隔离 GOCACHE 与不存在的 ICHAT_CONFIG_FILE；真实 AMQP fixture 设置底层 socket deadline 并清理自有连接。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./messaging/rabbitmq -run '^TestPublisher' -count=1 -v` | 全部通过，2.199s |
| `go test -mod=readonly -race ./... -count=1` | 全仓库通过，未发现 race；rabbitmq 包 3.258s |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 编译通过 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQPublisherIntegration$' -count=1 -v -timeout=8m` | 三个子场景全部通过；test=21.53s，包=23.995s；runId=`d3953e39bf6668f73e337305fe307f00`，cleanup 成功 |
| gofmt、`git diff --check` | 通过 |

### 后续边界

P3.3 有界的是 slots/outstanding 和通知空间，confirm 等待响应 context；**v1.15.0 已开始的网络写入、channel 创建/退休/关闭 RPC 仍依赖调用者的 transport deadline**。没有宣称网络黑洞、goroutine/退出时间硬界、自动恢复或持续 readiness 已验收。P3.4 将补齐网络写硬超时/可中断连接、unknown 处置与 monotonic epoch 重连，P3.5 装配独立生产/消费连接及拓扑就绪，P3.6 才在隔离环境接入 HTTP 202。旧业务路径、HTTP 状态和旧 worker 未修改。

建议 commit message：`feat(mq): add bounded publisher slots and correlate confirms with returns`

下一小点：**P3.4：超时、unknown、epoch 重连**。开始前按复杂度细分网络 I/O/关闭硬界和 supervisor 恢复验收，继续保持每次一个可审查小点。本轮不继续下一小点、不自动提交。

## P3.4.1：有界连接、网络 I/O/关闭与 unknown

状态：2026-10-13（Australia/Sydney）完成实现、自审、race 单元测试、静态检查和真实 Docker/TCP 代理验收。代码基线为用户提交的 P3.3 `a29baf3`。按前一条记录的复杂度拆分约定，本轮仅完成 P3.4.1，P3.4.2 自动恢复仍待实施。

### 改动

- 新增 `messaging/rabbitmq/session.go` 的 `DialPublisher`/`PublisherSession`，显式拥有 socket、AMQP connection 和 publisher channels；无 import 联网，不公开连接供额外 listener 使用。默认 8 slots、connect/operation=5s、write/close=3s、heartbeat=2s；配置有界校验，Recovery 固定 nil。
- Connect context 覆盖 TCP/TLS/AMQP 握手，Setup context 覆盖 channel/confirm 初始化；每次底层 Write 更新 deadline，发布 Operation context 覆盖写入、confirm 与失败处置。watchdog 直接停止 admission 并关闭 socket，中断真实正在等待的 I/O，由固定客户端 reader shutdown 释放 RPC，不遗弃一个仍运行的阻塞 RPC goroutine。
- P3.3 publisher 增加只供 owned session 设置的中断 hook：仅进入发布调用时注册 watchdog，非法/预取消/busy 请求不会关闭其他请求的连接；slot 退休和 Close 的 RPC 也有 close 上限。裸 NewPublisher 接口仍由调用者负责自己的 transport 边界。
- Shared connection 被中止时尚未完成完整证明的在途任务返回 unknown；空闲 Read EOF/失败和客户端关闭 transport 也关闭 admission/Done。明确匹配 Return/NACK 的结果保留，不把关闭 RPC 的失败改成 accepted，也不撤销已完成的成功。
- Close 先拒绝新工作，总 close deadline 覆盖全部 channel 与 connection RPC；响应缺失时强制关闭底层 socket，等待自身调用退出或返回 deadline。关闭 socket 幂等，watchdog 回调在操作返回前停止或 join；重复/并发关闭遵守边界。
- AMQP 明文仅允许 loopback/localhost；AMQPS 使用克隆 TLS 配置，至少 TLS 1.2、不跳过证书验证；拒绝 URL query/fragment 覆盖超时/heartbeat，错误和 Session.String 不泄露 URL 凭证或原始 broker 文本。业务配置加载/独立连接仍在 P3.5。
- 新增 [MQ session 说明](mq-session.md)，更新 publisher 文档、README、测试说明及阶段拆分，明确 I/O 边界只适用于新 owned session，不将自动重连计为完成。

### 自审与故障证据

- 复核 v1.15.0 的 DialConfig/TLS/Open、握手清除 deadline、channel RPC、Close/CloseDeadline、reader shutdown 与 heartbeat 读 deadline。不能只在 dial 时设置一次 deadline，也不能先阻塞等待 channel.Close 再打算关闭 socket；中断路径不调用 AMQP RPC。
- 使用实际 AMQP 客户端和受控 AMQP wire peer/net.Pipe，验证对端不读协议头、握手不回 Start、channel.open/confirm.select 不响应、真实 Write 被阻塞、完整命令已到达但 confirm 缺失、两条在途任务共享中止、Return 已匹配但 close-ok 缺失、channel/connection 关闭无响应。peer 任务退出、socket 仅关闭一次；预取消、非法输入、busy 不影响健康连接。
- 增加 TCP dial 的更短父 deadline、空闲 peer EOF、成功 watchdog 已停止（父 context 后续取消和间隔超过操作时间不会影响新请求）、并发 Close 中断已通过 transport Write 屏障的请求。另使用本机真实 TCP listener 接收 TLS ClientHello 后保持无响应，验证握手 deadline 和 peer socket 退出。
- 自审补上了 transport Read 失败/EOF 与客户端关闭 transport 时同步停止 admission，避免空闲断线未通知后续 owner。最终代码重新跑完整 publisher/session Docker 回归与全仓库检查。
- 真实 Docker fixture 中通过两个 slots 向正式队列发布；TCP 代理在初始化后丢弃下行帧，**先观察两个真实 broker ACK 已被截获**再取消第一请求。两请求均及时 unknown，之后用另一连接逐条读取对应 attempt 的完整信封/身份/正文并 ACK，证明 unknown 不表示未入 MQ。
- 另验证 250ms operation deadline（实际约 0.26s）、Return 后 channel.close-ok 被截断（约 0.37s）、connection.close-ok 被截断（约 0.31s）和正常 session/关闭。后两种缺失响应均由有界中断释放，不等待永远缺失的 RPC 响应。代理清理停止 acceptor、关闭两端 TCP sockets 并 join 两个转发任务。
- 所有 Docker 资源来自本轮 StartWithRabbitMQ；不读取开发 `.env`、不改变已有容器或数据卷。受控 peer、TLS listener 和 TCP proxy 属于测试进程并清理。没有网络规则修改、全局 Docker prune 或永久故障注入。

### 验证

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Linux amd64、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用隔离 GOCACHE、不存在的 ICHAT_CONFIG_FILE，所有测试配置显式构造。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./messaging/rabbitmq -run '^TestSession' -count=1 -v -timeout=30s` | 通过；增加真实 TCP TLS case 前的专项跑为 4.703s，该 case 已进入最终全仓库跑 |
| `go test -mod=readonly -race ./... -count=1` | 最终代码全仓库通过，未发现 race；rabbitmq 包 5.080s |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 编译通过 |
| 最终 publisher + session 集成回归（完整命令见 [MQ session](mq-session.md)） | 最终两个 suites 全部通过；publisher=21.64s，session=22.35s，包=46.454s |
| gofmt、`git diff --check` | 通过 |

最终 runId：publisher=`250e516bf53ef5afcb0182c6ef54b048`，session=`53bc03f28b208737e07ec45026a99da8`，cleanup 均成功。最初 session 集成 runId `98e1c1fe5e7fd4a554c781ddac7e257e` 已通过（24.962s），补齐 idle EOF 后仍以上述完整最终跑作为验收依据；没有未处理的失败。

### 后续边界

此次覆盖 owned session 的连接/写入/确认/退休/关闭期限、socket 中断与 unknown 责任边界。没有自动拨号恢复、抖动退避、单调 epoch 分配、新连接拓扑复核、slots 重建或命令自动重发；整个 P3.4 尚未完成。Done 表示连接/admission 已停止，不是完整 AMQP 析构完成信号；已退休 slots 耗尽的恢复策略在 P3.4.2。配置值不代表高负载下毫秒级调度 SLA。未切换 HTTP/旧 Redis 链路，未实现消费者或数据库终态接线。

建议 commit message：`feat(mq): bound publisher session I/O and shutdown with socket interruption`

下一小点：**P3.4.2：supervisor 退避重连、单调 epoch 和新连接拓扑复核**。重建前停止旧 session，未知结果不自动重发，复核拓扑及通知/confirm 后再接单。本轮不继续下一小点、不自动提交。

## P3.4.2：supervisor 退避重连、单调 epoch 和新连接拓扑复核

状态：2026-10-13（Australia/Sydney）完成实现、自审、race 单元测试、静态检查和最终 Docker 回归。代码基线为用户提交的 P3.4.1 `979a069`；P3.5/P3.6 尚未实施。

### 改动

- 新增 `messaging/rabbitmq/supervisor.go` 的 StartSupervisor、Status、WaitReady、Publish、Close。单一后台 owner 负责连接/回收/等待，无命令队列或自动重放。未就绪/已关闭/lifecycle 已取消时零发布；busy/输入错误不退休健康 session。
- 默认 backoff base 从 1s 指数增长到 30s，实际等待在 base 的一半至 base 间随机抖动；成功安装重置 base。每次尝试消耗独立 connection epoch，包括失败握手/拓扑检查。uint64 耗尽进入 failed，不回绕；epoch 范围是单个 supervisor 生命周期，不是跨进程持久序号。
- 给 owned session 增加 setup preparation hook（仍在原 setup deadline/socket watchdog 下）和 quiescence 观察。新的 AMQP 连接检查实际 broker 4.3.6/vhost，管理 API 精确契约核验、同连接 passive exchange/queue 查询、再读有效拓扑；随后建立新的原始通知与 confirm slots 才 ready。全程没有主动声明/创建/绑定修复、delete/purge 或策略修改。
- Return/NACK/unknown、连接关闭或 session/通知异常时移除 active、停止旧 admission、关闭 socket；其他旧在途任务沿用 unknown 语义，已完整确认的结果保留。等待旧 session 自有调用结束后才建立下一连接；无法 quiesce 进入 failed，拒绝重叠新 epoch。旧结果只能 invalidate 原 active session，不能关闭替代 session。
- Start 返回前只校验配置，后台恢复不阻塞构造；vhost/URI 必须一致，management helper 必须有效，TLS 参数克隆。Status 仅返回脱敏 sentinel；Close 停止 admission、取消连接/退避、按自身 close 边界等待 owner。补充直接检查 lifetime context，取消后的 setup 结果必须清理，不能因为取消回调调度延迟而安装 ready 或接单。
- 测试 TCP proxy 支持本轮多次连接，可只丢弃第一个连接的下行确认；停止时 join acceptor 与全部转发任务。新增 [MQ supervisor](mq-supervisor.md)，更新 session、publisher、README、测试说明及阶段清单。

### 自审与故障证据

- 检查 owner 单一、旧 session 在新连接前 abort/Close/quiesce、已死亡但尚未安装的 session 重试、原始 channel/confirm/tag 的新 epoch 隔离、所有取消阶段和 uint64 上界。固定客户端 Recovery 保持 nil，恢复由应用 owner 完成。
- 单元测试覆盖失败尝试 epoch 1/2/3 后 epoch 4 就绪、真实时间退避下界/纯 jitter 上下界与 cap、连接前零发布、拓扑失败阻断、旧调用回收屏障、无法回收的失败关闭、旧调用迟到错误不退休新 session、Return/NACK/unknown 恢复与 busy 不恢复、dial/backoff/ready 取消、早死 session、epoch 耗尽及 lifecycle 取消/setup 迟到清理。
- 真实 broker 的第一连接通过 TCP proxy 丢失 ACK，Publish 返回 unknown；新连接只读复核后 epoch 增大且 tag 从 1 起。独立读取旧 attempt 的完整原文后队列为空，证明无自动重放；显式同键/同内容重试得到新 attempt，并再次完整读取/ACK。
- 已 ready 后移除本轮主绑定，真实 Return 导致旧 session 退休；接下来核验失败、WaitReady 超时且 Publish 为 not_published。管理 API 证实绑定仍为空，没有被 supervisor 修复。只在测试中恢复该绑定后新 epoch 才 ready，能够发布。
- 对本轮 broker 做 stop/up（全部动作前核验 compose project/run labels）、保留命名卷和已 accepted 未消费消息，恢复原 endpoint 后 supervisor 自动连接并复核；旧原文完整保留、新连接成功发布、tag 重置。测试不修改开发 `.env`、共享资源或业务数据。
- 首轮 supervisor runId=`6a56e6355536f76faecf1f973c3a5bef` 的前两场景通过，但 broker 重启后 30s 未恢复，整轮失败（74.782s），不能计为通过。后续实际日志确认 Docker 为随机发布端口重新分配：中间轮 AMQP 51323→51540、management 51322→51541；最终轮 64492→64702、64491→64703。修正的是 fixture 重启场景：检测后用本轮临时 !override 文件固定回原端口、保留数据卷，supervisor URL/凭证不更换。标签验证和清理边界保留，没有把动态服务发现加入产品。
- 修正后 session + supervisor 完整跑通过（69.421s）。最终补齐 lifetime 取消边界后再次跑全仓库与三个真实 suites，以下方结果作为最终验收依据；所有失败/中间/最终 fixture 均清理成功。

### 验证

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Linux amd64、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE，配置显式构造。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./messaging/rabbitmq -run '^TestSupervisor' -count=1 -v -timeout=30s` | 专项通过，2.390s；后续 lifecycle cases 已进入最终全仓库跑 |
| `go test -mod=readonly -race ./... -count=1` | 最终全仓库通过，未发现 race；rabbitmq 包 5.270s |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 编译通过 |
| 最终 publisher + session + supervisor 集成回归（命令见 [MQ supervisor](mq-supervisor.md)） | 三个 suites 全部通过；publisher=21.79s、session=22.27s、supervisor=44.63s，包=91.164s |
| gofmt、`git diff --check` | 通过 |

最终 runId：publisher=`c593aae32ea368789f73b3571f6a15bc`，session=`280085f71eaf5ff4ff98aa71a9e22fe3`，supervisor=`bcf524b09f7117e66848493ebb8127e6`；cleanup 全部成功。中间通过轮 session=`b43d0f1d75255f264b80e5dbf16cab33`、supervisor=`490bc2ac4291caeef194f10c2e1b6819` 也已清理。

### 后续边界

P3.4 的现阶段独立库验收已收口：有界 I/O/关闭、unknown、退避重连和新 epoch 只读复核。仍没有应用 MQ 配置、独立 producer/consumer 装配、整体 health/持续拓扑复核、HTTP 202、消费事务或三节点契约。Status 是时点观察；management 必须配置为 AMQP 同 broker，passive 查询并不能单独验证参数/绑定，所以组合管理核验。跨主机服务发现、跨进程 epoch 持久化与多 API 单会话协调不在本小点范围。

建议 commit message：`feat(mq): supervise publisher reconnects with monotonic epochs and topology checks`

下一项：**P3.5：独立连接和拓扑就绪装配**。开始前根据配置、运行模式、producer/consumer 生命周期和 health 范围继续细分，保持每次一个可审查小点；HTTP 202 按 P3.6 接入。本轮不继续下一项、不自动提交。

## P3.5.1：应用 RabbitMQ 配置、环境覆盖与纯构造

状态：2026-10-14（Australia/Sydney）完成实现、自审、配置专项测试、最终全仓库 race 与静态检查。代码基线为用户提交的 P3.4.2 `d968b10`。本轮只完成配置边界，P3.5 独立连接和应用装配仍未收口。

### 改动

- 将 P3.5 继续拆分为 P3.5.1 配置、P3.5.2 独立生产/消费连接生命周期、P3.5.3 运行模式装配、P3.5.4 接受 readiness 与持续拓扑核验；后续开始时仍可细分，不一次实施整个阶段。
- 新增 `config.RabbitMQConfig`、`PublisherConfig` 及显式 `Publisher()` 构造器。加载不修改 `config.Conf`；构造只生成有界管理客户端与 supervisor 参数，不解析 DNS、不拨号、不请求管理 API、不启动后台任务。
- 已有配置无 MQ 节仍可加载，使用库的 slots=8、connect/publish=5s、write/close=3s、heartbeat=2s、retry=1s—30s 和初始 epoch=1。四个连接/管理凭证字段默认空；全部为空表示未配置，纯构造返回 `ErrRabbitMQNotConfigured`，部分配置则报错。
- 显式读取每个已支持键与 `ICHAT_RABBITMQ_*`，即使 YAML 省略也可环境覆盖；显式空字符串也覆盖文件内容，空密码不会回退到文件密码。应用不自动读取 Compose `.env`。
- 严格校验 slots、带单位的时间、整秒 heartbeat、retry 边界、字段类型/未知键、vhost、显式 URI 凭证及 loopback/TLS 限制。将 supervisor 的原有检查提取为无 I/O 的 `ValidateSupervisorOptions`，StartSupervisor 与应用配置复用同一入口，恢复算法和拓扑核验不变。
- MQ 配置单独解码，错误不包含 URI、密码或原始解析值；MQ 配置与 publisher 设置的 fmt / GoString 输出脱敏，覆盖嵌套在完整 Config 中的格式。语法读取错误隐藏原配置文本，并保留迁移命令既有的 `read config` 前缀。
- 更新默认安全的 `config/config.example.yml`、README、阶段清单及 [MQ 应用配置](mq-application-config.md)。纠正规范草案中的 `/ichat` URI：实际 vhost `/ichat` 必须编码为 `%2Fichat`。明确尚未实现的 consumer/Outbox 字段不能复制进当前严格校验的 MQ 节。

### 自审与验证

- 配置测试覆盖旧配置/模板兼容、默认值与 supervisor 映射、全部字段环境单独提供及覆盖 YAML、重复加载无凭证残留、部分配置、显式空密码/URI/时间、错误 vhost/URI/远端明文/管理路径与嵌入凭证、slots 和时间上下界、未知字段、数字/浮点/null 类型、脱敏错误及三种 fmt 格式。
- 两个本机 TCP listener 分别观察 AMQP/管理地址，Load 与 Publisher 完成后 Accept 均只超时，未收到连接。无真实数据库、Redis、RabbitMQ 或 Docker 操作，不读取开发配置和 `.env`。
- 首轮全仓库检查发现原迁移 CLI 的 malformed-config 测试要求 `read config` 前缀；只修正脱敏错误的兼容前缀，未弱化测试。修正前的两次全仓库结果为失败，不计入验收；最终完整回归两项迁移测试及所有包通过。

环境：Windows amd64、Go 1.27.1、GCC/race。设置 GOROOT 为空、GOTOOLCHAIN=go1.27.1、GOCACHE 为仓库 `.gocache`、ICHAT_CONFIG_FILE 为不存在的测试路径；配置用例仅使用临时文件或版本控制的模板。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./config -count=1 -v` | 初始配置专项通过（2.705s）；后续新增 null/模板/解析错误 cases 已包含在最终全仓库回归 |
| `go test -mod=readonly -race ./... -count=1` | 最终全部通过；config=2.178s，rabbitmq=5.269s，主包=9.418s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过，覆盖 integration 标签代码的静态检查 |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

本轮未修改 AMQP 协议操作、重连状态机或 fixture；无 I/O 的配置边界已由单元测试验证，未重复运行 Docker 集成故障套件，也未将其记为本轮实测。

### 后续边界

填写 MQ 配置不会启动主程序中的 publisher、consumer 或改变 HTTP 状态；新事务仍未接入旧入口。本轮没有运行模式、独立 consumer 连接、应用接受 readiness、持续拓扑复核、HTTP 202 或消费落库。配置检查不能证明实际 broker 身份/权限；管理端点必须指向 AMQP 的同一 broker，连接后的核验仍由现有 supervisor 执行。consumer/broker ACK timeout/Outbox、自定义 CA 与三节点契约在对应后续小点实现。

建议 commit message：`feat(config): add validated RabbitMQ publisher settings and environment overrides`

下一小点：**P3.5.2：独立生产/消费连接生命周期**。连接所有权、I/O/关闭期限与拓扑核验需保持隔离，生产背压不能连带阻塞消费确认；开始前按实际装配复杂度继续细分。本轮不继续下一小点、不自动提交。

## P3.5.2.1：有界连接内核与独立消费者基础生命周期

状态：2026-10-14（Australia/Sydney）完成实现、自审、专项及全仓库 race 测试、静态检查、真实消费者连接验收及完整 publisher/session/supervisor 回归。代码基线为用户提交的 P3.5.1 `e8d22f6`。P3.5.2 继续拆分；本轮不实现 delivery / ACK，不代表消费链路或整个 P3.5 已完成。

### 改动

- 从 `PublisherSession` 提取私有 `connectionOwner` 与 `ConnectionOptions`：握手/准备 deadline、TLS 克隆/安全限制、每次 Write deadline、Read 失败停止 admission、socket 中断、在途计数/quiescence 和重复/并发有界关闭。每次拨号创建自己的所有权实例，生产/消费不共用 socket、write mutex 或取消信号；publisher hooks 只负责本 owner 的停止/关闭。
- PublisherSession 改为使用连接内核，保留原 DialPublisher/Publish/Done/Close、slots/epoch/mandatory/persistent/confirm-return 与 unknown 语义；supervisor 恢复算法不变。独立执行确认状态机的 publisher 不向消费者提供连接或通知空间。
- 新增 `DialConsumer`、`ConsumerSession.Verify/Done/Close`。新连接无 publisher slots、不创建 confirm 模式、不发布，初始化和显式 Verify 使用相同实际 AMQP 连接的版本/vhost/passive 查询与两轮管理契约核验。核验失败退役自己的 socket，不主动声明/绑定/删除/修复拓扑，不自行重连或消费消息。
- Verify 最多一项在途，busy/nil/预取消请求不损害其他工作；主动核验有 operation 上限，失败/超时后实例永久不可用。Close 停止 admission、有界关闭并等待该 owner 的在途核验；Done 不是完整析构信号。Dial 的 ctx 仅是初始化范围，成功返回后由调用方显式关闭。
- 复用原单接收者固定客户端和受控 AMQP peer，不添加依赖。测试 TCP proxy 新增原子丢帧计数，以证明真实消费者核验回复被拦截；不记录 payload 或凭证。
- 更新 [独立消费者连接](mq-consumer-session.md)、session/supervisor、README、测试入口与阶段清单。下一点细分为 P3.5.2.2 消费 channel / 手动 ACK 的传输所有权和隔离验证；业务事务提交后 ACK 仍属 P4。

### 自审与故障证据

- 检查 publisher setup hooks 安装与取消/EOF 的竞争、旧 session quiescence、各自 close deadline、TLS 对象克隆及 socket 只关闭一次；没有把阻塞 RPC 放到遗弃 goroutine，也没有共享连接实例。
- 受控真实 AMQP 客户端/net.Pipe 验证不读握手、握手无 Start、channel.open 无响应、拓扑检查错误脱敏、重复核验与初始化 context 取消、busy/预取消/无效配置、核验超时、空闲 EOF、缺少 connection.close-ok，以及并发 Close 中断已进入 transport 的 RPC、最后在途计数归零。
- 在生产者实际 Write 阻塞后执行消费者 channel.open/close，证明不同 write mutex/socket；生产者取消得 unknown 后消费者仍可核验。反向消费者核验 RPC deadline 使该实例退休，生产者仍收到匹配 confirm/accepted。
- 真实 RabbitMQ 管理 API 观测 fixture 控制连接之外两个不同 AMQP 连接：逐一新增消费者和生产者，关闭消费者只移除其连接，生产者仍可 accepted。消费者没有借用控制连接或 publisher connection。
- 真实代理先截获 publisher ACK，再验证另一消费者 AMQP 核验成功，发布调用仍未完成；显式取消后 unknown 原消息完整保留，没有自动重放。反向代理截断消费者核验响应并记录丢帧，另一个生产连接在故障期间与消费者中止后均能 accepted。
- 只在本轮 vhost 移除主绑定，已有消费者 Verify 和新 Dial 均失败，管理 GET 证明绑定未被修复；人工恢复测试绑定后旧实例仍 unavailable，显式新建实例核验通过。另截获消费者真实 connection.close-ok，关闭按期限失败，不影响生产者。
- 所有消息读回/ACK 使用 fixture 既有控制连接，仅验证 accepted/unknown 原文和 attempt 保留；这不是 ConsumerSession 的 delivery/ACK 验收。没有 broker 资源告警注入、数据库事务或消费者重连验收。

### 验证

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Engine 28.4.0、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用仓库 GOCACHE、不存在的 ICHAT_CONFIG_FILE；仅使用显式 fixture 配置，无开发 `.env`/配置或共享数据操作。Docker Desktop 起初未运行，经授权后台启动后运行本轮独立测试。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./messaging/rabbitmq -run '^TestConsumerSession' -count=1 -v -timeout=30s` | 专项全部通过，2.898s |
| `go test -mod=readonly -race ./... -count=1` | 最终全仓库通过；主包=10.249s，rabbitmq=5.935s，config=2.226s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 集成标签编译通过 |
| 四个真实 suites 的集中回归（命令见消费者连接文档） | 全部通过；consumer=36.39s、publisher=21.64s、session=22.43s、supervisor=44.59s，包=127.480s |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

本轮 runId：consumer=`954b3ebd9c7e9084791007a75d8c57a8`、publisher=`c63c66ab4a1e9a8d24d741837bc5d9c3`、session=`bd211c2f90e53387e2a61abb37cf23ae`、supervisor=`7109cc1d6b0a712f2068dd81cc3af0ba`。四轮 cleanup 均成功；共有 16 个真实子场景通过。supervisor 重启仍按既有 fixture override 恢复原随机端口，观察到 AMQP 56514→49938、management 56515→49939 的分配变化，未改变 supervisor URL 或引入服务发现。

### 后续边界

本轮没有消费 channel / delivery/ACK、prefetch、consumer 后台重连、应用运行模式、health/readiness/持续拓扑复核、HTTP 202 或事务落库。Verify 是显式时点检查；应用配置尚未提供消费者参数映射。两个库级独立连接已验证，主程序仍未装配这两个连接，业务继续使用旧链路。生产多数故障、资源告警、提交不确定和端到端故障矩阵仍在后续阶段。

建议 commit message：`feat(mq): add independently owned bounded consumer sessions`

下一小点：**P3.5.2.2：有界消费 channel / 手动 ACK 的传输所有权与隔离验证**。开始前按 channel 创建、prefetch、delivery 生命周期和关闭范围继续细分，保持每次一个可审查小点；业务事务提交后 ACK 仍按 P4 实施。本轮不继续下一小点、不自动提交。

## P3.5.2.2：有界消费 channel、顺序 delivery 与手动 ACK/Reject

状态：2026-10-14（Australia/Sydney）完成实现、自审、专项及最终全仓库 race、静态检查、真实消费传输验收与全部 MQ 回归。代码基线为用户提交的 P3.5.2.1 `01aab6b`；本轮只完成传输接口，不接入业务事务、HTTP 或旧链路。

### 改动

- 每个 ConsumerSession 生命周期最多创建一个消费 channel/订阅；多 worker 使用多个独立长连接 session。只允许主/隔离队列，prefetch=1—256，qos count/global=false、consume autoAck/exclusive/noLocal/noWait=false 并等待 broker 回复。channel.open/qos/consume 共用 operation 上限和 socket 中断 watchdog。
- 新增 ConsumerChannel.Next/Done/Close 和 ConsumerDelivery.Ack/Reject。每个 channel 最多向顺序 worker 交付一条未完成记录；pending 或并发读取/处置立即 busy，客户端其余未确认缓存由 broker prefetch 限制。Next 只有有界等待，没有自动 ACK，空闲 timeout/取消不退役健康连接。
- 公开消息体、headers 和属性不包含原始 AMQP Acknowledger。私有 handle 保存原 channel/tag/一次处置记录；修改 metadata 或复制 handle 不能改确认目标、重复确认或确认后一条消息。旧 session handle 不得确认新 session 复用的 tag=1。
- ACK 固定 multiple=false；Reject 固定 basic.reject 单条，支持主队列 requeue 与可靠 DLX。隔离队列没有下一跳 DLX，禁止 Reject(false)，保持审计责任。零写入拒绝不伤害其他在途工作；已开始写入的失败/超时/取消/关闭竞争返回脱敏 ErrConsumerDispositionUnknown，退役自己的连接，不自动重试处置。
- 单个容量 1 原始 NotifyClose / NotifyCancel 接收 monitor 处理 broker cancellation/channel failure；父 session Close 先停止 admission/join monitor，再有界关闭并等待在途 Next/处置。网络 I/O 不持有 delivery mutex，socket Close 能释放实际阻塞 Write。关闭不代替 worker ACK/Reject，未确认和预取消息交回 broker 重投。
- 消費 ACK 没有 broker 确认回复，Ack 的 nil 仅表示本地发送完成。真实代理仅在 ACK-loss 场景解析上行协议并丢弃一条 basic.ack，证明 nil 后仍可重投；其他 proxy 场景保留原 io.Copy 上行。没有打印/保存 payload 或修改网络全局规则。
- 更新 [消费 channel / 手动 ACK](mq-consumer-channel.md)、连接/session/supervisor、README、测试入口和阶段清单。ErrConsumerBusy 文本泛化为 consumer operation busy，errors.Is 身份保持原变量；最终纯测试覆盖此次文本及补充的 idle Next/并发处置用例。

### 自审和故障证据

- 检查唯一 channel 预留、初始化 failure/EOF/关闭的竞争、setup context 不自动取消成功订阅、raw 通知单接收者、monitor join、调用计数归零，以及所有零写入错误不会中断其他在途调用。
- 受控真实 AMQP 客户端/wire peer 检查 qos count=2/size=0/global=false、consume 各 flags=false、body/持久化属性、ACK tag=1/multiple=false、Reject tag=2/requeue=true；metadata 改成 tag=999 仍只发送原 tag，复制/重复 handle 不会再写帧。
- 原 channel.open、qos、consume 回复缺失均及时中断；ACK/Reject 实际进入 net.Pipe Write 后阻塞，由写期限或并发 Close 释放为 unknown。并发第二处置不进入 Write。broker cancel、wire tag=0、空闲 Next 取消/超时与并发 Close 均不留遗弃的调用/monitor；关闭释放未确认责任。
- 真实主队列 ready=1、unacknowledged=2 证明 prefetch=2；API 顺序接收/单条 ACK 三个完整 attempt 后队列清空。管理统计字段采用指针，未返回字段不能冒充 0。
- 同样的三条消息中，持有一条且另一条已预取时关闭旧订阅；新 session 完整收到全部原文/属性/attempt，原持有记录标 redelivered，tag 从 1 重新开始；对旧 handle ACK 被拒绝，不能误确认新 tag=1。
- 主队列单条 Reject(true) 后同 attempt redelivered，再 Reject(false) 通过可靠 DLX 进入隔离；原文/属性/x-death 保留，隔离 Reject(false) 被拒绝；仅在隔离 fixture 中显式手动 ACK 后两个队列为空，不冒充数据库审计已保存。
- 截获真实 publisher confirm 后，该发布仍 pending；独立消费者实际 Next/Ack/正常关闭，控制连接确认无遗留消息，生产者显式取消得 unknown。生产者确认背压没有阻塞消费 ACK。
- 另一场景中消费者 Ack 返回 nil，但代理确认只丢弃一条真实 basic.ack；关闭后新的 session 获得同原文/attempt 的 redelivery。证据明确消费 ACK 不是恰好一次证明；业务必须提交后 ACK 并使用数据库幂等处理重投。

### 验证

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Engine 28.4.0、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE，配置显式构造；未读取开发 `.env`、未启动业务主程序或修改共享数据。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./messaging/rabbitmq -run '^TestConsumerChannel' -count=1 -v -timeout=30s` | 专项通过，2.933s；后续追加的并发/idle 等 cases 进入最终全仓库回归 |
| `go test -mod=readonly -race ./... -count=1` | 最终全部通过；主包=10.404s，rabbitmq=6.618s，config=2.271s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过，最终补充用例后复核 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 集成标签编译通过 |
| 五个真实 suites 集中回归（命令见消费 channel 文档） | 全部 21 个真实子场景通过；channel=45.10s、consumer session=41.01s、publisher=21.83s、session=22.61s、supervisor=58.82s，包=191.826s |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

本轮 runId：channel=`074e4ec91d05c49d76d2f92b92f9c276`、consumer session=`151cf907a4efc6207201c611969d51b6`、publisher=`12694eaef46bd7c5fc144548ed525b02`、session=`17087fb25784b8f60db28902619832ae`、supervisor=`7eabca7756166985375e3e18f6187705`；五轮 cleanup 均成功。supervisor 回归重启观察到 AMQP 64680→61031、management 64679→61032 的端口分配变化，既有 fixture override 恢复原 endpoint，不改变产品 URL。

### 后续边界

P3.5.2 的独立连接与消费传输库级范围收口。多个 worker 连接的应用配置/数量上限与运行模式、主程序生命周期、consumer 自动恢复、接受 health/readiness/持续拓扑核验、HTTP 202 和业务事务仍未装配。prefetch 只保证未确认消息数量，P4 继续负责协议校验、权限/幂等事务、数据库故障退避与提交后 ACK/持久隔离。本轮不宣称生产资源告警、DB COMMIT 不确定、三节点或端到端验收完成。

建议 commit message：`feat(mq): add bounded consumer channels and fenced manual dispositions`

下一项：**P3.5.3：运行模式与依赖装配**。开始前根据配置、角色能力、后台恢复和关闭范围继续拆成可审查小点。HTTP 202 按 P3.6、消费事务按 P4 接入；本轮不继续下一项、不自动提交。

## P3.5.3.1：严格运行模式、角色计划与启动检查

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项和全仓库 race、静态检查。代码基线为用户提交的 P3.5.2.2 `be794b8`。P3.5.3 继续拆分；本轮不装配新角色或切换业务链路。

### 改动

- 新增 `RuntimeMode`、`RuntimeConfig` 和无 I/O 的 `Config.RuntimePlan()`，选择 legacy/api/worker/all 的 API、RabbitMQ publisher、持久化 consumer、隔离审计 consumer、Outbox 和旧 worker 角色。计划是值对象，不创建服务、连接或任务，也不改变配置/全局状态。
- `config.Load` 严格读取 `RUNTIME.mode`：文件省略时 legacy，显式 `ICHAT_RUNTIME_MODE` 可覆盖省略键/文件值，空覆盖必须失败；拒绝错误类型、未知字段/模式、枚举大小写和空白变体。读取使用独立 Viper，不保留先前模式，不回显原始非法值。
- 代码直接构造的零值 Mode 沿用 legacy，保持旧 bootstrap 注入构造器兼容。新模式与 LEGACY_WORKER.enabled=true 互斥，Load 和直接 RuntimePlan 都验证，不能同时启动旧队列 worker 和新角色。
- bootstrap 在配置加载后、MySQL/Redis/worker/global binding/storage health/HTTP 之前解析角色计划并执行装配检查。当前业务程序仅 legacy 可启动；合法但未装配的新模式返回 `ErrRuntimeNotAssembled`，非法/冲突模式返回 `ErrRuntimeConfig`。不静默降级为旧 router，不运行部分新角色。
- 更新默认 legacy 的模板、README、[运行模式配置与启动检查](mq-runtime-modes.md)、MQ 配置说明和阶段清单。将后续拆为 P3.5.3.2 MQ 运行参数/受控依赖构造，P3.5.3.3 生命周期/角色能力核验；开始前仍可细分。

### 自审与验证

- 角色检查证明 api 不选择持久化/隔离/Outbox worker，worker 不选择 API/publisher，all 选择两类角色，legacy 只按既有开关选择旧 worker。角色计划不解释 transport 凭证或主机配置；仓储和 MQ 的可用性校验留给各自构造器与装配层。
- 测试默认与文件模式、大小写配置键、完全省略 YAML 的环境模式、覆盖/显式 legacy、空值/未知/错误类型/秘密字符串脱敏、重复加载，以及新旧 worker 冲突与直接零值配置。
- bootstrap 注入工厂记录调用，api/worker/all、非法模式和冲突配置均只有配置加载事件；MySQL、Redis、legacy worker 和 HTTP 工厂都不运行，既有全局依赖身份不变。没有触发连接构造器、消费订阅或健康任务。
- 原默认 legacy 的完整 bootstrap 生命周期、worker 启用/防重入/停止、缺失 factory、MySQL 离线 API 行为和迁移 CLI 回归继续通过。未通过改测试预期来容忍新角色误启动或吞掉错误。

环境：Windows amd64、Go 1.27.1、GCC/race。沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE，配置测试仅使用临时文件/模板；无 Docker、开发配置/.env 或真实数据操作。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./config . -run 'Test(Runtime\|BootstrapRuntime)' -count=1 -v` | 专项通过；config=2.571s、主包=2.897s |
| `go test -mod=readonly -race ./... -count=1` | 最终全仓库通过；主包=10.050s，config=2.546s，rabbitmq=6.508s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

本轮未改 AMQP 方法、确认状态机或测试 fixture，没有重复运行 Docker MQ 故障矩阵，也不计为本轮实测。启动拒绝的行为由直接执行 bootstrap、记录实际 factory 调用的测试覆盖。

### 后续边界

角色声明不表示角色服务已实现。当前主程序仅 legacy；api/worker/all 在外部连接前明确失败。consumer 参数/数量上限/后台恢复、MQ 资源装配、角色所需 schema/依赖核验、进程信号与优雅退出、整体 readiness、HTTP 202、业务事务和 Outbox 仍按后续小点接入。独立迁移命令可读取合法模式，但不会使用业务 bootstrap 的装配检查或新增 MQ 网络依赖。

建议 commit message：`feat(runtime): add validated mode plans and reject unassembled roles`

下一小点：**P3.5.3.2：MQ 运行参数与受控依赖构造**。开始前按 producer/consumer 选取、worker 上限和构造/失败清理范围继续细分；未完成角色不得静默降级。本轮不继续下一小点、不自动提交。

## P3.5.3.2.1：消费者有界参数与按角色生成 MQ 依赖计划

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查。代码基线为用户提交的 P3.5.3.1 `6ab1588`。将 P3.5.3.2 再拆成参数与纯依赖计划、实际连接创建与失败清理两点；本轮不创建 MQ 会话或启用新模式。

### 改动

- RABBITMQ 增加 consumerWorkers（默认 4，上限 32）、consumerPrefetch（默认 16，1—256）和独立 consumerOperationTimeout（默认 5s，正字符串不超过 60s）。三个字段支持省略 YAML 时的显式环境覆盖、空值拒绝及严格类型校验；共用已有 connect/write/close/heartbeat，发布者仍使用 publishTimeout。
- 新增 `RabbitMQConfig.Consumer()` 的纯构造结果，校验消费者直接 Go 配置、URI/vhost/传输期限与管理凭证。将 DialConsumer 原检查提取为 `ValidateConsumerOptions`，连接层和配置层复用相同无 I/O 校验；拨号/拓扑/关闭行为不变，没有 broker ACK timeout 等未生效配置。
- 新增 `Config.MQRuntime()`：legacy 不构造 MQ 输入；api 仅 command publisher；worker 仅持久化/隔离 consumer groups；all 为两者。持久化组按 consumerWorkers、隔离审计另固定 1 个独立 session，使用相同 prefetch 与消费者期限；最大消费者连接输入数量为 33。
- 新模式必需依赖缺失返回明确错误，不使用旧链路/default 账号降级。all 的后项校验失败返回空计划；计划只包含构造输入，没有连接、订阅、worker、Outbox 或 readiness。直接 Go 配置只构造选中依赖，Load 仍校验文件所有支持字段。
- ConsumerConfig/MQRuntimePlan 的 fmt/GoString 及 groups 嵌套格式隐藏凭证，错误仅有脱敏 sentinel 与已知字段/角色信息。计划不修改源配置、旧全局 config.Conf 或模式启动检查。
- 更新模板、README、[消费者参数与 MQ 依赖计划](mq-runtime-parameters.md)、MQ 配置/运行模式文档和阶段拆分。现有 api/worker/all 仍在业务 bootstrap 外部连接前明确失败。

### 自审与验证

- 核对 consumerWorkers=1—32、prefetch=1—256 与 audit=1 的数量边界；这是准入上限，不是测得的吞吐容量。consumerOperationTimeout 与发布期限独立，不冒充 DB 事务期限或 broker consumer ACK timeout。
- 测试默认值、1/32 workers 和 1/256 prefetch、消费者操作时间上界、所有 MQ 环境字段映射、重复加载、空覆盖/负数/浮点/null/布尔/无单位时间/溢出等拒绝和错误脱敏；直接 Go 构造也不可绕过消费者核心校验。
- 对各模式验证 publisher/group/固定队列选择、持久化 4 个及审计 1 个实例输入、新旧角色互斥、source/global 不变。worker 不构造 command publisher，api 不构造 consumer；缺失依赖和 all 的部分失败不返回半份计划。
- 启用两个本机 TCP listener 作为 AMQP/management 地址，Load、Consumer 和 api/worker/all MQRuntime 执行后 Accept 只超时，未接到任何连接。格式化消费者设置、计划及 consumers 列表不泄漏测试凭证。
- 原默认 legacy bootstrap、模式未装配拒绝、迁移 CLI 和 MQ 传输库回归继续通过；未改变测试预期或放开默认单元测试访问开发配置。

环境：Windows amd64、Go 1.27.1、GCC/race，沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE；仅临时文件/本机 listener，未启动 Docker 或业务服务、未访问开发配置/.env 或共享数据。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./config -run 'Test(MQRuntime\|RabbitMQEnvironment)' -count=1 -v` | 专项通过，2.286s |
| `go test -mod=readonly -race ./... -count=1` | 最终全仓库通过；主包=10.112s、config=2.632s、rabbitmq=6.501s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

本轮只抽取原消费者配置校验，没有变动 AMQP 方法/通知/确认状态机；纯构造零 I/O 由 listener 观测覆盖，没有重复 Docker 故障矩阵或将旧结果计为本轮实测。

### 后续边界

MQRuntimePlan 是输入描述，不是资源管理器、服务实例或角色就绪证明。实际 producer/consumer 连接创建、部分失败回收、总体构造/退出上限和生命周期所有权在下一小点；consumer 自动恢复、进程退出、主程序能力检查、readiness、HTTP 202、业务事务/审计/Outbox 继续按后续阶段实现。当前主程序仅 legacy，新模式启动拒绝保留。

建议 commit message：`feat(config): add bounded consumer settings and role-specific MQ plans`

下一小点：**P3.5.3.2.2：按角色受控创建 MQ 连接与失败清理**。开始前根据实例数量、总构造期限、无订阅的准备阶段与部分失败的回收继续细分；未完成业务 consumer 不得提前消费/ACK。本轮不继续下一小点、不自动提交。

## P3.5.3.2.2：按角色准备独立 MQ 连接、有界失败回滚与关闭

状态：2026-10-15（Australia/Sydney）完成实现、自审、全仓库 race、静态检查与隔离 Docker 实测。代码基线为用户提交的 P3.5.3.2.1 `3b5be6c`。本轮覆盖连接构造和显式资源所有权，不装配业务角色或启用新运行模式。

### 改动

- 新增 application 包的 `PrepareMQ` / `MQResources`，重新从 Config 验证 MQ 输入后才调用角色工厂。legacy 不创建 MQ；api 一个异步 publisher Supervisor；worker 按 consumerWorkers 创建独立持久化连接并另建一个隔离审计连接；all 合并两者。消费者最大 33 个，不借用其他 worker/publisher 连接。
- 消费者只通过 DialConsumer 建连并只读验证版本/vhost/拓扑；准备没有 QoS/Consume/Next/ACK/Reject、队列修复或迁移。固定队列、prefetch 和连接能力以列表副本返回，调用方修改副本不会改内部清理清单。
- MQOptions 提供整次 SetupTimeout（默认 30s）与整组 CloseTimeout（默认 5s），均正值且不超过 60s；是 Go 装配输入，不增加 YAML/env 字段。所有 consumer 共用总 deadline，并保留各 session 更短的 I/O 期限。
- publisher lifetime 由资源组拥有；准备期间的 watcher 转发 setup 取消，成功移交前停止并 join watcher。setup context 取消/超时不能在成功返回后意外停止 publisher 恢复或关闭 idle consumer；调用方必须显式 Close。
- 部分失败返回 nil 资源，回收所有已返回 handle（包括 handle+error），nil/typed nil 被拒绝；回滚独立于已取消的 setup context。Close 先停能力访问/取消 publisher，再共享一个总期限并发关闭所有 handle，全部 join；expired context 也触达全部 child Close，清理错误与原始错误都保留。并发等待有各自期限，关闭失败可重试，不假称超时 owner 已退出。
- Error/fmt 隐藏原始 transport/factory 错误和凭证，errors.Is 保留类别/取消/期限。文档说明解包后原始 cause 不应记录，成功准备不等于 MQ/接受 readiness。
- 更新 README、参数/模式/阶段说明、测试说明，新增 [MQ 连接准备与失败清理](mq-runtime-resources.md)。业务 bootstrap 原 guard 不变；未完成的 api/worker/all 仍在连接前明确拒绝。

### 自审与验证

- 10 个单元测试组覆盖选中/未选中工厂、legacy 缺省兼容、33 消费者上限、源配置/全局不变、列表所有权、坏模式/新旧冲突/缺配置/选项/工厂缺失的零 I/O 拒绝。
- 每一资源构造阶段注入 handle+error，全部旧 handle 与出错返回 handle 均关闭；nil/typed nil 不逃逸。总 setup deadline 不按 worker 重置，失败回滚不继承取消；工厂成功与 setup 取消竞争时不移交半份资源。
- 成功准备后的 setup 取消和期限不影响 publisher lifetime；另用自有本机 TCP peer 拒绝实际 AMQP 握手，证明 api 返回未就绪的异步 publisher，并在 setup 取消后继续恢复，Close 等待结束。
- 多连接共用 close deadline，expired context 仍关闭所有资源；并发 Close 合并且等待者能提前取消，所有 closer goroutine join，失败可重试，关闭异常不吞掉或泄漏凭证。
- 真实 broker 四种模式的 app 连接数（含 fixture 控制连接）依次为 1/2/4/5，证明 publisher 与 2 个持久化/1 个审计消费者独立建连。取消成功 setup 后 publisher 仍 WaitReady、消费者仍 Verify；Close 后只剩控制连接。
- 主/隔离各预放一条持久消息，所有阶段管理 API consumers 列表为空，两个队列 ready=1、unacknowledged=0、consumers=0。仅 fixture 最后读取/ACK，完整核对规范命令身份/hash/正文/attempt 和审计原文/持久属性，未提前领取或丢失责任。
- 代理透传第一（worker）/前二（all）连接，再让后续握手缺少实际 AMQP 回复。回滚前观察到已建立连接，总 8s 到期释放真实阻塞握手，所有已创建连接归零到控制基线；没有订阅/消息领取或拓扑修复。

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Engine 28.4.0、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE。真实资源全部由独立 fixture 创建/核验标签/清理；不访问开发 .env/config、不运行业务主程序或修改共享数据。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./application -count=1 -v` | 首轮 8 组专项通过，2.372s；新增离线/并发组纳入最终全仓库回归 |
| `go test -mod=readonly -race ./... -count=1` | 全部通过；主包=9.926s，application=2.720s，config=3.117s，rabbitmq=6.830s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 集成标签编译通过 |
| `TestMQPreparationIntegration`（完整命令见资源文档） | 6 个真实场景全部通过；suite=60.51s、包=63.135s，两个实际握手超时均=8.02s |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQSessionIntegration$' -count=1 -v -timeout=4m` | 共享代理原有 5 场景全部通过；suite=22.78s、包=25.366s |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

最终准备测试 runId=`791a28d50e1dc08372083147f7a28fe5`，cleanup 成功。首次 sandbox 无 Docker pipe 权限，按环境要求提升后执行；前一真实 run=`ff6b529dcc215d38d31b8769e0434bb3` 的四角色通过，但两个 2s 超时场景无法在约 5s 的管理统计窗口中观察回滚前连接，测试失败且 cleanup 成功。将仅真实场景的期限改为 8s 后完整复测通过；产品默认和单元测试毫秒期限未放宽。

共享 sessionProxy 新增默认关闭的 stallAfter 开关，后续连接的上行握手被吸收，已有 ACK 丢帧/下行故障分支不变；代理关闭仍 join 全部转发 goroutine。原 session 故障 suite 复测 runId=`52c3cdaed53e3d1615a40acd22397435`，cleanup 成功。本轮未改 AMQP 库实现，未重复其余 publisher/supervisor/consumer Docker suites，也不把旧结果计为本轮实测。

### 后续边界

当前实现是可显式调用的 MQ 依赖准备层，未接入业务 bootstrap，也未实现整体 readiness、进程信号/服务任务退出、consumer 自动恢复、HTTP 202、业务提交后 ACK、隔离审计或 Outbox。P3.5.3.2 现阶段参数与资源构造范围收口，P3/P3.5.3 整体尚未验收。

建议 commit message：`feat(runtime): prepare role-specific MQ resources with bounded rollback`

下一小点：**P3.5.3.3：生命周期装配与角色能力核验**。开始前按角色能力、service lifetime/退出与恢复范围继续细分，未完成业务角色不能提前订阅或静默降级。本轮不继续下一小点、不自动提交。

## P3.5.3.3.1：MQ 资源托管生命周期与自动有界关闭

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查和隔离 Docker 实测。代码基线为用户提交的 P3.5.3.2.2 `86de2f9`。将 P3.5.3.3 先拆成 MQ 资源 lifetime owner、角色能力门禁与服务退出装配；本轮只完成前一小点。

### 改动

- 新增 `application.StartMQLifecycle` / `MQLifecycle`，调用既有 PrepareMQ 建立 idle 连接，再由一个 goroutine 持有整组资源的关闭责任。lifetime 取消或 Close 请求触发自动关闭，Done/Wait 提供完成通知及可观察结果；不启动业务订阅/任务/进程信号。
- 保持 PrepareMQ 的 setup-only 语义：成功准备后的总 setup deadline 不会结束新的 lifetime；StartMQLifecycle 的显式 lifetime 取消才自动关闭。Wait 的 observer 取消不影响连接，Close 的 observer 即使已取消仍请求停止，并允许后续用新 observer join。
- 自动关闭继承 MQResources 的整体 CloseTimeout，使用独立 context；取消的 lifetime/等待者不能夺走清理预算。已停止的 owner 不再通过访问器提供能力，消费者列表仍为副本，不暴露内部清理清单。
- 关闭结果在 Done 之前发布且保持不可变，多等待者读取通过 channel 同步，无可变共享错误竞态。自动尝试失败明确报告；后续 Close 可在本次剩余预算内重试资源清理，但保留首次失败，不将已发生的异常擦掉。
- 检查准备交接的最后一次 lifetime 取消；半完成、handle+error 或取消与成功竞争时返回 nil owner，并独立回滚资源。fmt/Error 脱敏，errors.Is 保留构造/清理/取消/期限信息。
- 共用原 MQOptions 的纯校验，不改变默认值或添加 YAML/env 字段；AMQP 库和 broker 规则不变。更新 README、阶段/模式/资源/测试说明，新增 [MQ 托管生命周期](mq-lifecycle.md)。

### 自审与验证

- 9 个单元测试组验证四模式的自动取消、重复关闭与无效输入拒绝；nil/错误/取消交接、回滚错误传播和凭证脱敏；observer 与资源 lifetime 独立；准备期限结束后的连接继续存活。
- 独立关闭预算覆盖 lifetime deadline、已取消 Close observer；短观察者先返回但唯一 owner 保持清理责任，之后可再次 join。受控真实超时使所有 child Close 返回期限错误，owner Done 只在全部调用 join 后关闭；Wait 保留失败，显式 Close 可重试。
- 8 个并发 Close 合并至同一个 owner/资源组关闭尝试，所有 child 只关闭一次，无新监听 goroutine或结果竞态。首次专项在 deadline 用例中发现测试在 owner 启动后修改 fake.Close 的 race；改为交接之前配置 fake，最终专项/全仓库 race 均通过。
- 真实 suite 保留原 6 个角色连接准备/后续握手超时场景，并增加 4 个 managed 场景：api、worker、all 取消自动关闭，all 的 expired Close observer 先返回后由 owner 独立完成。每次 Wait 加入后 app 连接回到唯一 fixture 控制连接，清理无错误。
- 真实主/隔离队列各预存一条持久消息，整个准备/托管/回滚过程中 /api/consumers 为空、ready=1/unacknowledged=0/consumers=0；最终只用控制连接读取/ACK，完整命令身份/hash/正文/attempt 和审计原文/持久属性不变。
- 自审明确区分连接 owner 与业务 task owner：真实业务装配先停接单/领取、限时 drain 提交与 ACK，再取消 MQ lifetime；本轮不将 signal context 直接接到 MQ 以冒充在途事务优雅退出。新模式 guard、既有 legacy bootstrap 和全局依赖保持原行为。

环境：Windows amd64、Go 1.27.1、GCC/race、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE；真实资源使用独立 Docker fixture，标签核验后清理本轮容器/卷/网络，不读取开发 .env/config、不运行主程序或修改共享数据。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./application -run '^TestMQLifecycle' -count=1 -v` | 修正测试交接 race 后 8 组专项通过，2.362s；后加准备期限/回滚清理错误覆盖纳入最终全仓库回归 |
| `go test -mod=readonly -race ./... -count=1` | 全部通过；主包=10.342s，application=3.330s，config=2.993s，rabbitmq=6.901s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 集成标签编译通过 |
| `TestMQPreparationIntegration`（完整命令见生命周期文档） | 10 个真实场景全部通过；suite=79.90s、包=82.594s，managed 四场景=3.95/5.00/5.00/5.01s |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

本轮真实 runId=`69d68b6f1e37aca6d69b65763c5cc31b`，cleanup 成功。仍使用约 5s 管理统计观察窗口，两个原握手超时各=8.02s。没有变更共享 proxy/AMQP 库，没有重复其余 MQ Docker suites，也不把旧结果计为本轮实测。

### 后续边界

StartMQLifecycle 是显式可用的连接托管层，未接入业务 bootstrap；主程序仍只运行 legacy。Done 关闭且 Wait 返回错误时不能宣称所有 child 后台 owner 已退出；后续重试也不保证消除各 session 已缓存的关闭错误。本轮不覆盖实际业务在途事务/进程信号退出、角色能力核验、consumer 自动恢复、readiness、HTTP 202、隔离审计或 Outbox。P3.5.3.3/P3 整体尚未验收。

建议 commit message：`feat(runtime): own MQ lifetimes and join bounded shutdown`

下一小点：**P3.5.3.3.2：角色能力门禁与服务退出装配**。开始前按能力核验、停止准入/在途处理和进程信号继续细分，未完成业务角色不能提前订阅或静默降级。本轮不继续下一小点、不自动提交。

## P3.5.3.3.2.1：必需角色构造器与激活前服务实例检查

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查。代码基线为用户提交的 P3.5.3.3.1 `cb9ed0c`。将 P3.5.3.3.2 拆为本轮的角色构造器/实例检查、下一小点的受控构造/激活/分阶段退出编排；本轮不激活新业务角色。

### 改动

- 新增 application.RuntimeFactories.Check，从 Config 重新解析模式、先验证新旧 worker 冲突，再检查所选业务角色的非空构造函数。api 要求 API，worker 要求持久化/隔离审计/Outbox，all 要求四者；legacy 沿用旧 bootstrap，不要求现代服务构造器。
- 缺失构造器一次报告完整且稳定排序的角色列表，返回空 RuntimeAssembly，不交付半份结果。新增 RuntimeAssemblyError.Mode/MissingRoles 供 errors.As 读取，列表返回副本；共享 ErrRuntimeNotAssembled 与主包旧 sentinel 保持同一身份。
- 成功检查只保留选中工厂函数值和私有模式/角色快照。外部不能修改内部选择，Roles 返回副本，未选中工厂不要求存在、不被保留或执行；Check 不检查 MQ URI、schema 或依赖在线状态。
- 新增 RuntimeService 生命周期接口（Start 的 setup/lifetime、StopAdmission、Drain、Close、Done/Err）及 RuntimeFactory/RoleDependencies 的构造契约。只定义业务服务与 MQ 依赖的明确边界，不提供 API/消费者/Outbox 实现，不运行生命周期方法。
- RuntimeAssembly.ValidateServices 提供激活前第二次纯检查，拒绝缺失/nil/typed nil、额外角色和未检查的零值装配。精确校验服务集合，不调用任何服务方法，不改调用方 map 或关闭 handle；失败时回滚责任仍由后续执行器承担。
- bootstrap 配置加载后使用共享角色检查，错误包含具体缺失角色；保留新模式执行器兜底拒绝，不能只注册工厂就落入旧 router。未装配模式仍在 DB/Redis/worker/global binding/health/HTTP 之前失败；旧 legacy 行为不变。
- 更新 README、阶段清单、运行模式实际错误示例及生命周期后续说明，新增 [角色构造器与服务实例检查](runtime-assembly.md)。

### 自审与验证

- 7 个 application 专项测试组覆盖四模式选择、只要求选中工厂、旧 worker/直接零值兼容、全部/逐个缺失及部分 API 已注册时 worker 仍缺失、稳定角色列表、函数快照/角色和错误列表副本、源配置与旧全局不变。
- 非法 mode/新旧 worker 冲突先返回 ErrRuntimeConfig，不被缺失构造器掩盖。服务集合逐角色验证 missing/nil/typed nil，拒绝额外/未知服务和未检查的空装配；原 map/清理 handle 不变。
- 所有测试构造器和六个生命周期方法均设为 panic，证明两层检查不隐式执行构造/Start/停止/Drain/Close/Done/Err；两个独占本机 TCP listener 检查 AMQP/management 地址零连接。错误格式化不泄漏测试凭证、非法 mode 或未知 map key。
- 实际 bootstrap 专项复核 errors.Is 对原/共享 sentinel 兼容，errors.As 获取准确模式及完整缺失列表；调用记录仍只有 config，外部工厂与旧全局状态均不受影响。原 legacy 生命周期、关闭、MySQL 离线和迁移 CLI 回归继续通过。
- 自审区分结构检查与业务证明：构造函数非空、实例实现接口不证明业务语义或 readiness。ValidateServices 不冻结调用方 map，后续执行器必须控制集合、回滚所有已返回 handle，并在任何激活之前完成检查；本轮没有把生命周期接口声明当作已经完成退出编排。

环境：Windows amd64、Go 1.27.1、GCC/race，沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE。仅纯配置/本机 listener/受控 peer；没有 Docker、开发 .env/config、业务主程序启动或共享数据操作。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./application . -run 'Test(RuntimeAssembly\|BootstrapRuntime)' -count=1 -v` | 首轮专项通过；application=2.410s、主包=2.853s；增强 bootstrap 结构化诊断与 map 不变断言纳入最终全仓库回归 |
| `go test -mod=readonly -race ./... -count=1` | 全部通过；主包=9.969s，application=2.451s，config=2.867s，rabbitmq=6.511s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含集成标签代码的静态检查 |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

本轮未更改 MQ 网络/AMQP/确认状态机或 Docker fixture，零 I/O 由实际 listener 与禁止调用工厂/方法的测试证明；没有重复 Docker 故障 suites 或把前轮结果计作本轮实测。

### 后续边界

主程序仍只运行 legacy。现代服务默认构造器集合为空，受控服务构造、部分失败回滚、激活顺序、停止准入/事务 drain/资源关闭编排、进程信号、consumer 自动恢复和整体 readiness 尚未完成；HTTP 202、业务提交后 ACK、隔离审计与 Outbox 继续按对应阶段接入。P3.5.3.3.2/P3 整体尚未验收。

建议 commit message：`feat(runtime): validate required role factories and service assemblies`

下一小点：**P3.5.3.3.2.2：受控服务构造、激活与分阶段退出编排**。开始前按构造回滚、生命周期和进程信号范围继续细分；未完成业务角色不能提前订阅或静默降级。本轮不继续下一小点、不自动提交。

## P3.5.3.3.2.2：受控服务构造、激活与分阶段退出执行器

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查与隔离 Docker 实测。代码基线为用户提交的 P3.5.3.3.2.1 `b4420bd`。本轮完成可显式调用的现代角色执行器及其失败/退出编排，未提供生产业务角色或接线默认主程序。

### 改动

- 新增 application.RunRuntime / RuntimeOptions。所选角色构造器与 MQ 参数在外部依赖前验证；拒绝 legacy 误走现代执行器。统一 StartupTimeout 覆盖 MQ 准备、全部服务构造及激活工作，另设独立 DrainTimeout/CloseTimeout，复用既有 MQOptions，不添加 YAML/env 字段。
- 持有 PrepareMQ 的显式资源组，核验实际模式、publisher、消费者队列/数量/prefetch/非空能力，API 仅获 publisher，持久化/隔离只获对应连接组，Outbox 无 MQ 输入。服务不持有整个共享 MQ 资源组。
- 所有服务构造完成并 ValidateServices 后才 Start；激活顺序 Outbox→隔离→持久化→API。收集每个非空返回 handle，包括 handle+error；拒绝 nil/typed nil 和可比较实例的跨角色复用，重复 handle 只清理一次。实例自身是否暗中共享资源仍由工厂契约约束。
- Start 使用独立 setup/lifetime，成功后清除 setup 控制；监视稳定、非空 Done，只在关闭后读取 Err。异常退出（包括 nil Err）、nil Done 或 Done 发送值会失败并停止全组；启动中后台角色提前退出不继续激活 API。不自动重建服务或重放命令。
- 正常取消先同步停止全部已尝试激活角色的准入，再共享期限并发 Drain；期间保留 role lifetime 与 MQ 通路，所有 Drain 返回后才取消 role lifetime。随后并发关闭全部已构造服务，最后在同一关闭预算内关闭 MQ，期限已耗尽也触达每个 MQ child Close。
- 构造失败跳过激活/Drain，仍关闭全部已返回对象；Start 前即记录 attempted，失败 Start 也 StopAdmission/Drain。回滚和关闭使用独立 context，不继承已取消的启动/进程 context；所有调用 join，不丢弃 goroutine 伪装完成。回调适配器必须遵守期限、非阻塞访问器等接口契约。
- errors.Is 保留启动、drain、服务关闭、MQ 关闭与取消/期限的独立原因；Error/fmt 只含固定阶段/已知角色，不输出原始依赖错误。成功激活后正常取消且清理成功返回 nil；启动中取消则报启动失败并回滚。
- 更新 README、阶段清单、角色/模式/生命周期说明和测试说明，新增 [受控服务执行与退出](runtime-execution.md)。RunRuntime 直接持有资源，避免进程取消通过单独 MQLifecycle 提前切断 drain 通路；默认 bootstrap 未注册业务构造器，新模式仍拒绝。

### 回归中修复的既有关闭竞态

全仓库首轮在 TestConsumerChannelBrokerCancelAndMalformedDelivery 的 consumer_cancel 分支出现 ErrSessionClose；独立 20 轮复现 4 次（含坏 tag 分支）。owner.Done 在停止准入时关闭，可能早于 AMQP reader 发布连接 closed 状态；已经退役/中断的 transport 被 Close 再次写 graceful-close 握手，形成伪关闭失败。

连接内核在 Close 首次进入锁时记录 retired；此前已退役的 owner 不再发新 AMQP 握手，仍 join 关闭工作、abort transport 并等待自有在途调用归零。健康 owner 保持原 CloseDeadline、关闭黑洞期限和错误行为。新增确定性 peer 回归先停止准入但保留 socket，断言 Close 不写新握手且确实关闭 transport；该测试与原 broker cancel/坏投递分支连续 50 轮全部通过，未放宽测试预期。

### 自审与验证

- 11 组执行器专项验证三种现代模式角色输入、全部构造先于激活/API 最后启动、全部准入停止先于 drain、role lifetime/MQ 在 drain 期间有效、所有服务 Close 返回后才关闭 MQ，以及服务生命周期恰好调用一次。
- 逐构造器 handle+error、逐激活部分失败、nil/typed nil/实例复用、异常退出/Done 违约、整个启动工作期限、两个整组关闭阶段期限、成功返回与取消竞争、多个独立失败保留/脱敏、坏 MQ 快照和部分返回均有可执行测试。构造器与回调均为受控注入，未读取旧全局 manager。
- 真实 RuntimeExecution suite 的 fixture 角色不订阅或执行业务事务；验证 process 已取消后的 Drain 中实际 Publish 仍获得 accepted/confirm，消费者实际 Verify 仍成功。构造器 handle+error、部分激活失败、drain 超时后所有角色关闭并回到 broker 唯一控制连接。
- 主/隔离各预放一条持久原文，drain 另发布一条规范命令。全部过程无订阅、无 unacknowledged，最后仅控制连接读取/ACK，完整核对原文、持久属性、规范命令身份/hash/正文/attempt，未把 fixture ACK 冒充数据库提交后 ACK。
- 共享连接关闭修复另实测原 session 5 个健康/确认与关闭响应黑洞场景，以及 ConsumerChannel 5 个 prefetch/手动 ACK、未确认重投/旧句柄、可靠隔离、publisher 背压隔离、ACK 本地成功但丢帧后重投场景，全部通过。

环境：Windows amd64、Go 1.27.1、GCC/race、Docker Engine 28.4.0、RabbitMQ 4.3.6 / amqp091-go v1.15.0。沿用仓库 GOCACHE 与不存在的 ICHAT_CONFIG_FILE；只使用本轮独立 Docker project/vhost/账号/端口/卷，标签核验后清理，不读取开发 .env/config、不运行业务主程序或修改共享数据。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./application -run '^TestRunRuntime' -count=1 -timeout=30s` | 最终专项通过，11 组，2.577s |
| `go test -mod=readonly -race ./messaging/rabbitmq -run '^TestConsumer(RetiredClose\|ChannelBrokerCancelAndMalformedDelivery)' -count=50 -timeout=60s` | 关闭竞态与确定性无写入回归 50 轮全部通过，2.542s |
| `go test -mod=readonly -race ./... -count=1` | 关闭修复后最终全部通过；主包=10.757s，application=3.008s，config=3.211s，rabbitmq=6.969s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过，包含集成标签代码 |
| `go test -mod=readonly -tags=integration ./tests/testenv -run '^$'` | 集成标签编译通过 |
| `TestRabbitMQSessionIntegration` + `TestRuntimeExecutionIntegration`（完整命令见执行器文档） | 9 个真实场景全部通过；session=23.16s，runtime=39.88s，包=65.639s |
| `TestRabbitMQConsumerChannelIntegration` | 5 个真实场景全部通过；suite=45.81s，包=48.765s |
| gofmt、`git diff --check`、新增文件空白检查 | 通过 |

真实 runId：session=`fd89bfefef5eb5acd0a14c8549b60619`、runtime=`e8d3805b2bfabb08dcd25d0d970fdf18`、consumer channel=`0941bba2c2e6111fce4ead5cb279817e`；三轮 cleanup 均成功。首次实测因 Docker 未运行在资源发现阶段失败（run=`e587fab6c7e23ed98d29b5b93b46408d`，尚未创建 fixture）。后台启动 Desktop 后曾遇 WSL 0x800705aa 资源不足，后续引擎恢复并确认版本后完成实测；未终止其他应用/WSL、重置数据或修改系统内存配置。

### 后续边界

P3.5.3.3.2 的结构检查与受控执行基础范围收口；主程序仍只运行 legacy，现代业务工厂未实现/注册。真实业务在途事务、COMMIT 不确定、隔离审计/Outbox、consumer 自动恢复和整体 readiness 未由本轮 fixture 证明。回调必须遵守期限；Close 报错/超时时不能假称其内部全部任务已退出。P3.5.3.3/P3 整体尚未验收。

建议 commit message：`feat(runtime): orchestrate role startup and phased shutdown`

下一小点：**P3.5.3.3.3：进程信号与运行入口接线**。开始前按入口、信号和 legacy 兼容范围继续细分；缺失现代角色仍须在连接前拒绝，不能用 fixture 冒充生产处理器。本轮不继续下一小点、不自动提交。

## P3.5.3.3.3.1：进程入口分流与现代运行时信号

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查和集成标签编译。代码基线为用户提交的 P3.5.3.3.2.2 `47e4802`，开始时工作区干净。将 P3.5.3.3.3 拆为本轮入口分流/现代信号，以及下一小点的 legacy HTTP/WebSocket/cron 退出适配与信号接入。

### 改动

- `main → run → runProcess` 只加载一次配置，先调用共享 RuntimeFactories.Check，再选择 legacy 或 application.RunRuntime。缺失角色、非法模式、新旧 worker 冲突先于外部 I/O、旧全局绑定和信号注册失败。
- 现代分支只要求现代 runner/notifier 和所选工厂，纯校验 MQ 配置后使用 signal.NotifyContext 注册 os.Interrupt/SIGTERM，将同一配置、工厂和默认执行参数交给现有执行器。入口不自行准备 MQ/执行工厂，不把进程取消直接接到 MQ owner。
- 执行器返回前一直保留信号订阅，重复取消合并，不提前退出或抢走独立 drain/close 预算；返回后 defer stop 注销。入口原样返回执行错误，不通过 errors.Is(context.Canceled) 抹掉启动取消或同时存在的清理失败，main 继续对非 nil 错误退出 1。
- 原 bootstrap 提取接收已加载配置的 bootstrapLegacy，保留原加载入口与独立的新模式拒绝 fence；旧 HTTP、worker、健康探测、全局恢复及 Redis/MySQL 关闭顺序不变。legacy 不要求现代 runner/notifier，也不注册现代信号。
- 默认生产业务工厂依然为空，新模式依然在连接前报告完整缺失列表；未注册 fixture/no-op 服务，也未将旧 router/worker 当成现代实现。
- 更新 README、阶段勾选、模式/装配/执行器/连接生命周期及测试说明，新增 [进程入口分流与现代信号](runtime-process-entry.md)。未新增依赖、配置键或改变 Go/MQ 版本。

### 自审与验证

- 6 个入口测试组覆盖三现代模式的精确配置/工厂/参数/context 转交、仅要求所选工厂且不要求旧依赖、默认及部分缺失角色、非法模式/冲突、MQ 配置与注入边界错误、无效信号 context 的 stop handle 释放。
- 可控信号 context 取消后，fixture runner 阻塞在清理屏障；验证入口不提前返回、不提前注销。重复取消后仍等待同一次执行，清理成功返回 nil，清理失败与 context.Canceled 的组合错误完整保留。该 runner 仅验证入口调用契约，不是生产业务角色或执行器内部阶段证明。
- 保留默认生产 application.RunRuntime，在全部角色工厂可用且 signal context 预取消时实际执行；返回 context.Canceled 并注销信号 handle，没有连接 MQ 或调用任何角色工厂。
- 零值/显式 legacy 通过实际 bootstrapLegacy 装配受控 SQL driver 和禁止联网的 Redis client；配置仅加载一次，worker 停止时存储仍可用，最后 Redis/MySQL 关闭且旧全局完整恢复。原 bootstrap 生命周期、MySQL 离线、cron 等回归继续通过。
- 自审区分现代和旧 HTTP 所有权：legacy 当前 Gin.Run 没有停止入口，HTTP Shutdown 也不会代管 hijacked WebSocket。不能全局接收信号后只取消一个无监听者的 context，更不能借机提前关闭存储。因此 legacy 保留原 OS 退出行为，下一小点提供其完整停止/等待适配后再注册信号。
- 信号平台边界核对固定 Go 1.27.1 本地 os/signal 源码/说明：Windows Ctrl+C/Break 可取消 context，但控制台关闭/注销/关机仍可能由 OS 强制终止，不承诺完整 drain。默认测试只注入信号 context，没有向宿主/其他进程发送实际 OS 信号。

环境：Windows amd64、Go 1.27.1、GCC/race。沿用仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE，不读取开发 .env/config、不启用业务主程序、不访问共享数据。本轮没有改 AMQP/业务服务，不重跑 Docker，不把前轮真实 broker 结果计为本轮实测。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race . -run 'Test(RunProcess\|Bootstrap)' -count=1 -v -timeout=90s` | 首轮全部通过，主包=2.985s |
| `go test -mod=readonly -race . -run '^TestRunProcess' -count=20 -timeout=60s` | 6 组入口专项最终连续 20 轮通过，3.027s；清理屏障使用幂等关闭 |
| `go test -mod=readonly -race ./... -count=1` | 最终全部通过；主包=10.282s，application=2.810s，config=2.918s，rabbitmq=6.877s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过，包含集成标签代码 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库集成标签编译通过，未执行真实集成场景 |
| gofmt、`git diff --check`、新增文件空白/文档链接检查 | 全部通过 |

### 后续边界

P3.5.3.3.3.1 范围收口，P3.5.3.3.3/P3 整体尚未验收。主程序已经具备现代执行器接线，但现代业务工厂仍缺失，可运行模式仍仅 legacy；本轮不证明实际操作系统信号投递、legacy 优雅退出、真实业务 COMMIT/ACK、consumer 自动恢复或 readiness。

建议 commit message：`feat(runtime): dispatch process modes and wire modern shutdown signals`

下一小点：**P3.5.3.3.3.2：legacy HTTP/WebSocket/cron 的退出适配与信号接入**。开始前可按停止准入、活动会话/任务 join 和整体期限继续细分；必须在业务任务退出后再关闭存储。本轮不继续下一小点、不自动提交。

## P3.5.3.3.3.2.1：Legacy HTTP/WebSocket 请求所有权与退出适配

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查和集成标签编译。代码基线为用户提交的 P3.5.3.3.3.1 `7b8d55a`，开始时工作区干净。将 legacy 退出拆为本轮 HTTP/WebSocket 请求 owner，以及后续 cron 任务/所有权和整体退出接线；本轮不提前为 legacy 接收进程信号。

### 改动

- 新增纯构造的 legacyHTTPServer，调用方显式交付 listener 后才 Serve。使用私有请求清单持有每个已准入处理器，覆盖普通 HTTP、hijacked WebSocket、同用户被替换但仍在退出的旧会话；不借用只保留当前会话的用户全局表。
- StopAdmission 同步关业务准入，新请求获得 503/no-store；取消所有已登记的 hijacked 请求，普通在途 HTTP context 保留至 drain。取消与 socket I/O 均在登记锁外执行，不等待处理器或启动逐请求 watcher。
- Drain 将同一调用方预算用于 net/http.Shutdown 和显式请求等待。只有处理器完整返回后才移除记录；service.Chat 已有的订阅、reader/writer/心跳 join 和 Offline 也因此被包含。net/http accept loop 结束或 socket 已关闭均不充当业务任务完成证明。
- Close 取消全部请求，关闭 owned hijacked socket 并调用 net/http.Close 后继续等待。过期 observer 仍请求停止，超时保留 owner/活动记录，后续新 observer 和并发 Close 可继续等待；Close 失败不能授权上层提前释放共享存储。
- Hijack 前后核对停止边界，关闭竞争中刚得到的 socket，取消请求并拒绝交付。底层 handle+error、nil connection/buffer 坏返回均清理已交付 handle；停止后再 Serve 时由 net/http 关闭交付 listener。
- wrapper 透传 Unwrap、Hijacker、Flusher/FlushError、Pusher；自审核对固定 Gin 1.10.0 的 Stream/responseWriter 后保留条件式 CloseNotifier，仅底层支持时公开该能力，不创建虚假通知或额外等待任务。退出错误 fmt 脱敏，errors.Is 保留独立 cause。
- 更新 README、阶段勾选、入口和测试说明，新增 [Legacy HTTP 生命周期](legacy-http-lifecycle.md)。未改原 bootstrap/Gin.Run/cron/进程信号行为，没有注册现代角色或修改协议/配置/依赖版本。

### 自审与验证

- 独占 loopback HTTP 服务中阻塞一个普通请求，StopAdmission 后新请求被拒绝而旧 context 仍有效；Drain 等待原请求成功完成。强制关闭场景先耗尽 drain，再以已取消 observer Close，验证仍取消请求/中断连接，但处理器清理屏障未释放时不能报告完成；释放后 8 个并发 Close 全部等待成功。
- 真实 Gin/Gorilla/service.Chat 创建同 UID 旧/新连接及另一用户连接，各自实际收到推送帧。旧连接 reader 被可控日志屏障阻塞，替换后订阅虽已关闭仍保留 owner；停止后新连接和另一用户的订阅/socket 都关闭，Drain/Close 依然超时，直到旧 reader 释放。再次 Close 成功，两个当前用户各 Offline 一次，旧连接未清掉替换会话状态。
- 受控 Hijacker 分别在调用前、底层返回前制造停止，证明不会调用已停止的底层或遗留晚返回 socket；net.Pipe 客户端读到 EOF。部分 handle 返回和坏快照同样核对 EOF，未以 fake 回调记录代替物理 socket 关闭证据。
- 边界覆盖 nil handler/listener/context、构造前后无任务、空闲/重复关闭、关闭后再 Serve 的 listener 所有权、可选 CloseNotifier/不支持 Push/Hijack/Flush 的行为和 ResponseController unwrap，以及多个独立错误保留/格式化凭证脱敏。
- 自审明确三层结束通知：Serve 结束只说明 accept loop 停止，socket 关闭只说明 transport 中断，显式请求清单清空才说明已准入处理器及其托管任务返回。future legacy owner 仍须同时 join Serve，管理 cron/健康探测并在任务退出后关闭存储；不能把本轮独立适配器当作默认程序已支持信号优雅退出。

环境：Windows amd64、Go 1.27.1、GCC/race，沿用仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE。测试仅使用独占 loopback 临时端口、net.Pipe 和受控订阅，不读取开发 .env/config、不启用业务主程序、不访问共享 DB/Redis、不发送 OS 信号。本轮没有 Docker，不复用旧真实 broker 结果冒充本轮实测。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race . -run '^TestLegacyHTTP' -count=1 -v -timeout=45s` | 首轮 5 组通过，3.058s；后加 writer 能力/部分 socket 边界纳入最终专项 |
| `go test -mod=readonly -race . -run '^TestLegacyHTTP' -count=30 -timeout=60s` | 最终 7 组专项连续 30 轮全部通过，6.430s |
| `go test -mod=readonly -race ./... -count=1` | 最终全部通过；主包=9.903s，application=2.696s，config=2.789s，rabbitmq=6.592s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库集成标签编译通过；未执行实际集成场景 |
| gofmt、`git diff --check`、新增文件空白/文档链接检查 | 全部通过 |

### 后续边界

本轮 P3.5.3.3.3.2.1 范围收口，P3.5.3.3.3.2/P3 整体尚未验收。适配器显式可调用但尚未替换默认 Gin.Run；legacy 继续原 OS 信号行为。真实 Redis Pub/Sub、实际在途 DB 事务、cron 注册锁释放、整体信号退出不由受控订阅/日志屏障证明，现代生产业务角色仍缺失。

建议 commit message：`feat(runtime): own legacy HTTP and WebSocket shutdown lifecycles`

下一小点：**P3.5.3.3.3.2.2：legacy cron 的有界停止、任务等待与所有权释放，以及整体退出接线**。开始前可按 worker 和入口继续细分；必须在任务退出后再关闭存储。本轮不继续下一小点、不自动提交。

## P3.5.3.3.3.2.2.1：Legacy cron 任务停止、等待与注册释放

状态：2026-10-15（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译和隔离 Docker 实测。代码基线为用户提交的 P3.5.3.3.3.2.1 `b742248`，开始时工作区干净。将 P3.5.3.3.3.2.2 拆为本轮 worker owner，以及下一小点的 HTTP/worker/健康探测/存储统一生命周期和信号接入。

### 改动

- 新增 startLegacyWorker/legacyCronWorker，先纯校验 schedule、job/release 期限与非空依赖，再沿用无 TTL 的 AcquireLegacyWorker。成功 setup 与独立 job lifetime 分离；注册结果未知仍不补偿 DEL/自动重试。
- 同步准入 gate 拒绝停止后的排队 callback，running gate 跳过重叠；每次 SyncMsg 使用默认 30s context（正值、≤60s）。defer 在错误/panic 后归还 gate，后续排程可继续；仍使用 OwnedMessages 的逐条所有权检查与原子前缀确认。
- StopAdmission 不执行 RPC/等待任务；Drain 停止调度并等待全部 callback，不取消在途任务、不释放锁；Close 停止/取消剩余任务并请求清理。过期 observer 也请求停止，但不是任务已经结束的证明。
- 唯一 manager 只调用一次 cron.Stop，等其全部 callback 返回，再用独立 Background/default 5s ReleaseTimeout 进行一次比较 token 的释放。短等待者不会丢弃清理责任，多个 Close/Wait 共享 Done 发布前固定的结果；释放失败/超时不自动重试或擦掉错误。
- setup 在已知 token 交接前取消时返回 handle+startup error，并自动请求关闭闲置 worker；调用方继续 join handle。nil/typed nil、非法排程/期限、预取消均在 acquire 前拒绝。错误 fmt 只含固定类别，errors.Is 保留 cause，panic 原值不写入兼容日志。
- 默认 legacy startWorker 工厂已复用新 owner，仍保持旧 stop 的真实等待契约：独立 30s Drain 后取消/等待并释放，错误仍按旧接口记录日志；既有 bootstrap 先停止 worker 后恢复全局/关闭存储的顺序不变。未替换默认 Gin.Run 或为 legacy 注册进程信号。
- 更新 README、开发勾选、入口/HTTP/恢复/测试说明，新增 [Legacy worker 生命周期](legacy-worker-lifecycle.md)。原注册 Lua、数据 schema、MQ 与配置/依赖版本不变；不新增 YAML/env。

### 自审与验证

- 7 组默认测试使用真实 cron 短排程，而非手工调用正在运行的 job 伪装 join；验证 drain 不取消任务/不释放锁、Close 即使 observer 过期仍取消工作、被取消的 job 清理屏障未结束前 manager 不能释放注册，以及已停止的排队 callback 不再读数据。
- 顺序测试覆盖受控 commit → acknowledgement → Drain 返回 → Close/Release；job deadline、panic 后继续执行和始终不重叠；8 个并发 Close 的同一结果、独立 release deadline、保留首次错误/不重复释放；观察者取消不停止 worker、成功 setup 取消后 job lifetime 保留、已知 token 的交接取消自动回滚，以及输入/凭证脱敏边界。
- 实际 suite 使用独立 MySQL/Redis 和既有真实消息仓储。在真实 DB 提交后暂停 job：graceful 仍能完成确认，Drain 后注册仍 Busy，Close 才释放；forced Close observer 过期后任务仍被取消，但 job 返回前注册仍 Busy。释放屏障后原始前缀保留，新 owner 重放不增加消息行并清空前缀。
- 实际 stale_release 场景仅替换本轮 fixture token：旧 owner 的比较释放明确返回 ErrLegacyWorkerOwnership，不删除 successor；重复 Close 不重试 Release，首次错误保持。fixture 按自身 token 比较恢复，不使用共享 Redis/无条件 DEL。
- 实际 default_factory_stop 验证默认生产工厂接线：注册存在时竞争失败，重复 stop 后新 owner 可取得锁，旧 stop 再次调用不删除新 owner，Redis PING/MySQL SELECT 仍成功；worker 不拥有共享存储的关闭责任。
- 自审核对固定 cron v3.0.0 的 Stop/JobWaiter：只能在确认全部 callback 返回后释放 token，取消和超时不是数据库事务已结束的证据。默认 30s 是工作参数而非生产容量承诺；取消后确认失败可能暂停原 owner，旧人工恢复边界保留。兼容 stop 对不遵守 context 的回调继续等待，不能宣称整个进程有无条件 35s 上限。

环境：Windows amd64、Go 1.27.1、GCC/race。沿用仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE；默认测试只用受控 store/owner，不读开发 .env/config、不访问真实 DB/Redis。实际场景用 NewLegacyFixture 的独立 Docker project/schema/账号/随机 loopback 端口，标签核验后清理；未运行业务主程序、发送 OS 信号或修改共享数据。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race . -run '^TestLegacyWorker' -count=1 -v -timeout=45s` | 首轮 7 组通过，3.127s |
| `go test -mod=readonly -race . -run '^TestLegacyWorker' -count=50 -timeout=60s` | 最终 7 组连续 50 轮通过，7.565s |
| `go test -mod=readonly -race ./... -count=1` | 全部通过；主包=10.593s，application=2.795s，config=2.902s，rabbitmq=6.640s，无 race；最后增强的 setup 清理断言由最终 50 轮专项覆盖 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库集成标签编译通过 |
| `go test -mod=readonly -race -tags=integration . -run '^TestLegacyWorkerLifecycleIntegration$' -count=1 -v -timeout=5m` | 4 个真实场景全部通过，suite=21.78s、包=24.801s |
| gofmt、`git diff --check`、新增文件空白/文档链接检查 | 全部通过 |

实际 runId=`370d5ba80381122022e07f792fe4025f`，cleanup 成功。强制取消后的重放出现预期 MySQL duplicate-key 日志，随后完成既有内容一致性校验，最终行数仍为 1。首次 sandbox 无 Docker 配置/pipe 权限，在资源发现阶段失败（run=`88ac99b19f2bb145509296a7ddc693a3`，未创建 fixture）；提升后完成上述实测，没有自动审批拒绝或环境重置。本轮未改 MQ，不重复旧 MQ Docker suites，也不将旧结果计为本轮实测。

### 后续边界

P3.5.3.3.3.2.2.1 范围收口；默认启用的 legacy worker 已使用新生命周期，但 HTTP/Gin 与进程信号仍保持原行为。实际测试证明的是旧链路 MySQL 提交/Redis 确认与注册释放，不是 MQ 消费者提交后 ACK、端到端恢复或完整服务器信号退出。P3.5.3.3.3.2/P3 尚未整体验收。

建议 commit message：`feat(runtime): drain legacy cron jobs before releasing ownership`

下一小点：**P3.5.3.3.3.2.2.2：legacy HTTP/worker/健康探测/存储的统一生命周期与信号接入**。先停止各方准入，保留 drain 机会，确认 HTTP 和 job 全部 join 后再恢复全局并关闭存储；不能把 Close observer 超时误当作任务退出。本轮不继续下一小点、不自动提交。

## P3.5.3.3.3.2.2.2：Legacy 统一生命周期与进程信号

状态：2026-10-16（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译和隔离 Docker 实测。代码基线为用户提交的 P3.5.3.3.3.2.2.1 `ff425b7`，开始时工作区干净。本轮完成既定统一接线范围，不再拆分或推进下一项，不自动提交。

### 改动

- 默认 main 经 runProcess 分流到 runLegacyRuntime；legacy 与 modern 均在所选依赖纯检查通过后注册 os.Interrupt/SIGTERM。现代业务工厂仍为空，缺失角色继续在信号注册和连接前拒绝。旧 bootstrap/serve 保留兼容用途，默认 legacy 已使用 managed HTTP，不再调用 Gin.Run。
- 新 lifecycle owner 在共同 startup context 内创建惰性 MySQL pool、Redis、旧全局绑定、健康探测、HTTP、listener 和可选 worker。全部成功才启动 Serve；工厂部分返回 handle+error、交接取消和 typed nil 均受控处理，失败由同一 owner 回滚全部已交付资源。未启用 worker 时无需其工厂或参数。
- 停止时先同步停止 HTTP/worker 准入，再用共同 Background/DrainTimeout 并行 drain；随后用另一个共同 CloseTimeout 关闭 HTTP/worker/健康探测并等待 accept loop。默认 startup/drain/close 为 30s/10s/10s，保留原 PORT 语义，不新增配置键。
- 健康探测新增 StopContext/Done：过期 observer 仍发起取消并立即撤销 readiness，正在运行的 probe 由 owner 等待。HTTP 新增 RequestsDone，区分 accept loop 返回与请求/WebSocket handler 全部退出。
- Result 发布有界退出尝试，Wait/Done 发布实际最终清理。关闭期限耗尽仍有任务时返回 pending，但同一 owner 继续持有 globals/Redis/MySQL，待所有实际完成信号后收集 worker 释放结果、恢复全局并关闭存储。晚到错误由 Wait 返回，已发布 Result 不改写；不重复释放 worker 注册。
- 正常激活后的取消和成功清理返回 nil；启动失败、意外 Serve 返回（包括 nil/http.ErrServerClosed）、独立清理错误均保留 errors.Is，格式化只输出固定阶段，避免泄露 cause 中的凭证。新增 [Legacy 统一生命周期与进程退出](legacy-runtime-lifecycle.md)，同步入口、组件、恢复和测试文档。

### 自审与验证

- 默认测试验证全部 StopAdmission 先于任一 Drain、各阶段参与者共享同一 context、可选 worker、12 类部分启动回滚、未启用 Serve 的 listener 清理、预检查零 I/O/零信号注册、启动期限、异常 accept-loop、多个独立错误及格式化脱敏。
- pending 测试用受控屏障验证 Result 超时并不关闭池或恢复全局；过期 Close observer 仍请求停止，全部任务真正结束后才清理。晚到 worker 释放错误保留在 Wait，Result 保持不变。健康探测专项覆盖过期 observer 与实际 probe join。
- 实际 signal_graceful 场景通过 runProcess 注入 NotifyContext 取消，使用默认 Gin/JWT/WebSocket/真实 Redis 订阅及 worker。在真实 MySQL COMMIT 后阻塞 job，退出先关闭 WebSocket，而 worker drain 期间仍可使用存储且保留注册；放行后完成确认、释放、关池和信号注销。
- 实际 forced_pending_after_commit 场景使用 40ms drain/close；有界 Result 返回时 globals/池/注册仍在。放行 job 后完成实际清理，原记录由新 owner 幂等重放，最终消息行数仍为 1、队列前缀清空。
- 实际 busy_startup_rolls_back 场景验证已有 worker 锁导致启动失败且 Serve 未激活，listener/health/客户端全部回收，原持有者不受影响。mysql_offline_still_serves_liveness 仅停止本轮 fixture MySQL，实际默认装配仍提供 live=200、storage-ready=503，并能退出清理。
- 首轮专项中两个失败来自测试对未交付 Redis client 的关闭断言；改为只检查已转移所有权的 client，生产关闭顺序无需改动。最终全仓库与 30 轮专项通过。Docker 实测编译后补充了 typed-nil listener 防护及专项用例；该防护不改变实测使用的非 nil listener 路径，并由最终全仓库与 30 轮测试覆盖。

环境：Windows amd64、Go 1.27.1、GCC/race；沿用仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE。默认测试不读取开发 .env/config、不连接真实依赖。实际场景由 NewLegacyFixture 创建独立 Docker project/schema/账号/随机 loopback 端口，核验标签后清理；JWT 使用 fixture runId 派生的测试值。未运行业务主程序或向宿主进程发送 OS 信号。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race . ./health -run 'Test(LegacyRuntime\|RunProcessLegacy\|StorageStop)' -count=30 -timeout=60s` | 最终连续 30 轮通过，主包=5.575s、health=1.333s |
| `go test -mod=readonly -race ./... -count=1` | 全部通过；主包=10.107s、application=2.676s、config=2.896s、rabbitmq=6.515s、health=1.466s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库集成标签编译通过 |
| `go test -mod=readonly -race -tags=integration . -run '^TestLegacyRuntimeIntegration$' -count=1 -v -timeout=5m` | 4 个真实场景全部通过，suite=25.21s、包=28.204s |
| gofmt、`git diff --check`、新增文件空白/文档链接检查 | 全部通过 |

实际 runId=`3f80ab3438942c545b560f5aacbd6580`，cleanup 成功。强制取消后重放的 duplicate-key 日志属于既有幂等校验路径，最终消息行数仍为 1。首次 Docker engine 未运行，资源创建前因缺少 dockerDesktopLinuxEngine pipe 失败（run=`20fce38c50ac78e39539afd8dfb1bbd1`）；隐藏启动 Docker Desktop 后 server=28.4.0，完成上述实测。无自动审批拒绝、共享数据变更、进程强杀或环境重置。本轮未改 MQ，不重复旧 MQ Docker suites，也不将旧结果计为本轮实测。

### 后续边界

P3.5.3.3 的基础装配与入口范围收口；P3 尚未整体验收。信号测试证明注入取消后的真实装配行为，未实测 OS 信号投递。callback 必须遵守 context；实际任务未退出时不能凭 observer 超时提前关闭存储，也不能对任意不响应的 callback/closer 承诺硬退出上限。默认 main 收到 pending 错误仍会按既有逻辑退出 1，可能留下注册，继续按旧人工恢复步骤处理；嵌入者可保留 handle 等待最终清理。

默认现代业务工厂仍为空，api/worker/all 继续拒绝。HTTP 202、业务消费者提交后 ACK、审计、Outbox 和端到端恢复仍按对应后续阶段实施。

建议 commit message：`feat(runtime): wire legacy process signals and phased cleanup`

下一项：**P3.5.4：接受 readiness 与持续拓扑核验**。开始前按状态来源、拓扑复核与生命周期范围细分，拓扑不匹配必须保持未就绪。本轮不继续下一项、不自动提交。

## P3.5.4.1：Publisher 已就绪连接的周期只读拓扑复核

状态：2026-10-18（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译和隔离 Docker 实测。代码基线为用户提交的 legacy 统一生命周期 `8b334c2`，开始时工作区干净。将 P3.5.4 分为本轮 publisher 周期复核，以及下一小点接受状态汇总与运行时/健康接口接线；P3.5.4 整体保持未完成。本轮不自动提交或继续下一小点。

### 改动

- SupervisorOptions 新增 Go 装配参数 TopologyCheckInterval，默认 10s、范围 10ms—60s，零值不能关闭检查。应用配置沿用 DefaultSupervisorOptions，无新增 YAML/env；纯 ValidateSupervisorOptions 在网络 I/O 前拒绝非法间隔。
- 复核由原有唯一恢复 owner 串行执行，安装 ready 后开始计时，检查成功结束后重新计时，避免 ticker 积压/重叠。成功保持原 epoch 与准入，不创建新 publisher，不发布探测命令。
- PublisherSession 保留 startup 只读核验回调，后续在同一实际 AMQP connection 上复用 verifyNewConnection。复核 broker 版本/vhost、management 中的 feature flags、队列/交换机/有效策略/quorum 在线成员/绑定，再在独立临时 channel 执行 passive 查询并重新读取管理契约；不调用 EnsureLocal 或修复/删除拓扑。
- 检查整体受 Session.OperationTimeout 限制，活动检查进入 connection owner 的 beginCall/endCall 计数。watchdog 在取消/期限耗尽时中止 socket，打断 AMQP RPC。任意核验失败、management 不可用/超时均退休当前 epoch，撤销 Status/WaitReady 和后续 Publish 准入；公开错误只保留固定 sentinel。
- 旧检查与旧发布全部 quiesce 后才沿用 backoff 创建新 epoch；无法 join 时保持原 failed/cleanup 边界。检查与恢复同属一个 owner，晚到成功不能复活已退休/关闭实例。已有 accepted 不撤销，尚未完整确认的发布仍可 unknown，不自动重发。
- 新增[周期拓扑复核](mq-topology-monitor.md)，更新 supervisor、README、测试说明与开发阶段清单。默认现代业务工厂仍为空，legacy 接受健康接口继续 503；不将传输就绪直接解释为业务接受就绪。

### 自审与验证

- 新增 6 组 supervisor 默认测试：多轮成功保持 epoch/准入且没有探测发布、空闲失败立即撤销准入、原始错误脱敏、旧调用屏障阻止替代连接、未修复持续未就绪、修复后新 epoch 且无重发、deadline 后返回 nil 仍失败、Close 取消并 join 检查、旧 session 晚到成功隔离，以及间隔上下界纯校验。
- 新增 3 组 session 默认测试使用受控真实 AMQP 对端：初始化 context 不控制交接后的复核；重复健康检查不会遗留 watchdog 或消耗 publisher tag；预取消/nil observer 不退休健康 session，关闭后不可复用。channel.open 响应黑洞由 60ms watchdog 中止 socket，调用 quiesce；已发送但丢失 confirm 的发布在复核失败后仍返回 unknown，不误报 accepted/确定未入队。
- 新增真实 idle_quarantine_binding_drift 场景：先得到一个 accepted，再移除本轮隔离队列绑定，之后没有新发布；周期检查仍主动撤销就绪。绑定持续缺失、不自动修复；恢复绑定后新 epoch 就绪，原 accepted 的 command/attempt 内容保留，队列没有自动重发的额外消息。
- 新增真实 management_response_blackhole 场景：本轮 loopback 反向代理只丢弃管理响应，独立 AMQP 控制连接仍可查询队列；操作期限耗尽后停止接单，之后新连接核验也保持未就绪。恢复代理后新 epoch 就绪，队列没有巡检消息。
- 自审核对复核与 Publish 的共享连接所有权：失败会中止整个 epoch，不能只把健康布尔值改为 false 而继续使用未经验证的原 publisher。原 accepted 的持久责任保留，未决发布遵循既有 unknown；恢复路径没有补偿重发。周期轮询存在发现窗口，不等于每次请求前的原子拓扑快照，也不是生产容量承诺。

环境：Windows amd64、Go 1.27.1、GCC/race；沿用仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE。默认测试只使用 fake/自有 net.Pipe AMQP 对端，不读取开发 .env/config 或连接真实依赖。真实测试使用 StartWithRabbitMQ 的独立 Docker project、vhost、随机账号/端口，按归属标签清理；未运行业务主程序或修改共享数据。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./messaging/rabbitmq -run 'Test(Supervisor\|SessionTopology)' -count=1 -timeout=45s` | 首轮全部通过，2.727s |
| `go test -mod=readonly -race ./messaging/rabbitmq -run 'Test(Supervisor\|SessionTopology)' -count=10 -timeout=60s` | 连续 10 轮通过，8.013s |
| `go test -mod=readonly -race ./... -count=1` | 全部通过；主包=11.595s、application=3.261s、config=3.708s、rabbitmq=7.439s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库集成标签编译通过 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRabbitMQTopologyMonitorIntegration$' -count=1 -v -timeout=5m` | 2 个真实场景全部通过，suite=27.18s、包=29.839s |
| gofmt、`git diff --check`、新增文件空白/文档链接检查 | 全部通过 |

实际 runId=`6b537a6f0ae42e1cc95e0bdc701c0300`，broker=4.3.6、client=v1.15.0，cleanup 成功。首次 Docker engine 未运行，在资源创建前发现阶段失败（run=`befa1392734347d395ba389d9f43d6e5`，缺少 dockerDesktopLinuxEngine pipe）；隐藏启动 Docker Desktop 后 server=28.4.0，完成上述真实测试。无自动审批拒绝、环境重置、共享数据修改或进程强杀。本轮没有重复旧 broker 重启/ACK 丢帧/消费者 Docker suites，不将历史结果计为本轮实测。

### 后续边界

P3.5.4.1 范围收口，P3.5.4/P3 尚未整体验收。Status/WaitReady 仍是时点观察，拓扑变化在检查间隔内存在发现窗口；management 故障保守退休 AMQP 尚可用的 publisher。默认 10s 是有界装配值，性能压测、生产三节点成员契约仍待对应阶段。消费者的周期核验/恢复和业务接受状态不能由本轮 publisher 结果推断。

建议 commit message：`feat(rabbitmq): continuously verify publisher topology`

下一小点：**P3.5.4.2：接受就绪状态汇总及运行时/健康接口接线**。区分传输就绪、业务接受能力、退出准入与消费者状态；未装配的现代业务角色不能借健康检查宣称 ready。HTTP 202 与业务消费者仍按 P3.6/P4 实施。本轮不继续下一小点、不自动提交。

## P3.5.4.2：接受就绪状态汇总及运行时/健康接口接线

状态：2026-10-18（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译和隔离 Docker 实测。代码基线为用户提交的 publisher 周期拓扑复核 `96884d1`，开始时工作区干净。本轮完成既定就绪汇总及接线范围，不继续下一项、不自动提交。

### 改动

- 新增 health.RuntimeHealth/RuntimeStatus 和固定 ReadinessState 枚举。状态按值返回，不含依赖错误/URL；未知值保持 503，nil/typed nil capability 视为未配置。owner 的 String/GoString 不输出 context 或内部资源。
- RunRuntime 为每次现代运行创建独立只读 Health，随 RoleDependencies 提供给选中工厂。构造及部分激活期间始终 starting；所有 Start/Done/最终 setup 检查通过才发布绑定。进程取消立即撤销观察，回滚/退出也在任何 StopAdmission/Drain 前永久停止 owner，不提前取消服务/MQ lifetime。
- API 就绪要求显式 RuntimeAcceptance 能力：新接受 handler 的 AcceptanceReady、包含有时效 Redis 鉴权状态的 AuthenticationReady，以及 publisher 当前 State=ready、Ready=true、非零 epoch。Start 成功、JWT key 已配置或仅有 publisher 都不能代替业务能力；缺失/false 均未就绪。
- 消费者就绪单独要求所有本地消费连接 Done 非 nil 且未关闭，并由持久化/隔离角色显式 ConsumerReady 声明订阅/业务/存储状态。接受不受 MySQL 或本地消费者单独故障阻塞；worker 模式接受未配置、api 模式消费者未配置，不推断远程实例。
- 健康读取只采样本地缓存/Status/Done，不调用 Verify、WaitReady、Publish、订阅或依赖探测。角色能力方法也必须非阻塞且不能回读聚合状态。MQ 组关闭、进程取消或任一业务服务退出均不能保留 ready；停止标记在返回前复核。
- router.RegisterHealth 作为显式现代适配入口，接入 /health/ready/accept 并新增独立 /health/ready/consumers；保留 live/storage 语义，所有 ready 响应 no-store，公开原因均固定。旧 Router 始终传 nil，因此即使传入现代 mode，旧业务路由也不能虚报确认式接受。
- 新增[运行时就绪状态](runtime-readiness.md)，同步清单、运行时/组件/README/测试说明。P3.5.4 拓扑巡检与健康装配基础范围收口，默认现代业务工厂仍为空，legacy 接受健康接口仍为 503；生产 202、Redis 鉴权状态来源和消费事务仍在后续业务角色中实现。

### 自审与验证

- 新增 8 组 application 默认测试及 2 组 router 默认测试，覆盖模式/构造/激活门禁、缺失业务能力、鉴权与 publisher 状态/epoch、消费者独立故障、nil/typed nil、MQ 关闭、进程取消前后、drain 期间保留 MQ、并发观察、启动失败/服务意外退出、context 格式化脱敏、HTTP 固定原因/缓存/legacy 拒绝；原健康接口回归继续执行。
- 实际 suite 使用生产 RunRuntime、Gin loopback HTTP、真实 MQ 独立连接和空队列订阅；fixture 角色显式声明测试能力，没有生产 accepted handler、JWT 黑名单或业务事务。共享有时效探测缓存分别监测本轮 Redis/MySQL，健康 HTTP 本身不联网。
- redis_auth_connection_failure_and_recovery：关闭本轮 Redis 探测客户端，接受 HTTP 变为 503/AUTHENTICATION_UNAVAILABLE，消费者/live 保持 200；新建本轮客户端后恢复。没有停止 Redis 服务，也不将此场景描述为 Redis 服务重启或实际 JWT 校验验收。
- publisher_topology_retirement_and_new_epoch：移除本轮发布绑定，实际 mandatory 发布不能 accepted，接受 HTTP 503 而消费者仍 200；恢复绑定后 publisher 新 epoch 和接受 HTTP 恢复，未自动修复拓扑。
- consumer_connection_fault_does_not_block_acceptance：关闭本轮实际消费 channel/session，消费者 HTTP 503，接受 HTTP 仍 200，区分持久责任可继续进入 MQ 与本地处理暂时不可用。
- mysql_offline_accepts_until_process_drain：仅按标签核验后停止本轮 MySQL；storage/consumers=503 而 accept/live=200。取消进程并停在 drain 屏障时 accept/consumers=503/RUNTIME_STOPPING，role lifetime 和 publisher 仍可用；放行屏障后正常关闭所有资源。关闭期间 MySQL EOF 日志为预期故障现象。
- 首次集成标签编译发现测试误导入 go-redis/v9；修正为仓库既有 github.com/go-redis/redis/v8 v8.11.5，没有修改依赖。该次失败发生在编译阶段，没有创建 Docker 资源。随后 vet、标签编译和真实 suite 全部通过。最终增加 formatter 与 typed-nil/已关闭 MQ 的专项用例，最终全仓库及 30 轮专项均覆盖；formatter 不改变先前真实场景的汇总行为。

环境：Windows amd64、Go 1.27.1、GCC/race；沿用仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE。默认测试使用注入配置和受控依赖，不读取开发 .env/config、不连接真实服务。实际测试使用 StartWithRabbitMQ 的独立 project/vhost/随机账号/端口，核验标签后清理；未运行业务主程序、迁移/重置共享数据或发送 OS 信号。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./application ./router ./health -run 'Test(RuntimeHealth\|HealthEndpoints)' -count=1 -v -timeout=45s` | 首轮通过，application=2.491s、router=2.669s；health 此选择器无匹配测试 |
| `go test -mod=readonly -race ./application ./router -run 'Test(RuntimeHealth\|HealthEndpoints)' -count=30 -timeout=60s` | 最终连续 30 轮通过，application=3.694s、router=3.524s |
| `go test -mod=readonly -race ./... -count=1` | 最终全部通过；主包=10.459s、application=2.500s、config=2.448s、rabbitmq=6.806s、router=2.015s，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库集成标签编译通过 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestRuntimeReadinessIntegration$' -count=1 -v -timeout=5m` | 4 个真实场景全部通过，suite=26.30s、包=29.050s |
| gofmt、`git diff --check`、新增文件空白/文档链接检查 | 全部通过 |

实际 runId=`2a15f0a91b5a14945b858d9851584f0b`，broker=4.3.6、client=v1.15.0，cleanup 成功；没有自动审批拒绝、共享数据变更或环境重置。Docker 本轮已运行，无需启动/重启宿主引擎。只停止本轮 fixture MySQL，按归属正常回收；未重复上轮周期管理响应黑洞、broker 重启或 ACK 丢帧 suites，不将历史结果计为本轮实测。

### 后续边界

P3.5.4 的巡检与就绪装配基础范围收口，P3 尚未整体验收。健康接口是时点观察，不代替某次发送的完整确认判定；生产角色必须实现真实 handler、鉴权缓存新鲜度与消费状态，不能以 fixture 代替装配。默认 api/worker/all 仍缺少生产业务工厂并在 I/O 前拒绝；legacy 不支持 MQ accepted。消费者自动恢复/周期核验、事务提交后 ACK、Outbox 与端到端验收仍归后续阶段。

建议 commit message：`feat(runtime): compose acceptance and consumer readiness`

下一项：**P3.6：新 DTO 与 HTTP 202 接受入口**。开始前按鉴权/规范化/发布结果映射及 API 装配范围细分；未实现消费者时仅在隔离测试环境积压，不能启用缺失业务角色或退回旧 Redis 成功语义。本轮不继续下一项、不自动提交。

## P3.6.1：独立确认式 HTTP 发送适配与客户端协议探测

状态：2026-10-18（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译和隔离 Docker 实测。代码基线为用户提交的就绪汇总 `0a29b62`，开始时工作区干净。将 P3.6 分为本轮独立 handler/路由/客户端协议适配，以及下一小点生产 API 生命周期、鉴权探测与运行时装配；默认入口不切换。本轮不继续下一小点、不自动提交。

### 改动

- 新增 service.SubmitCommand，仅依赖 CommandPublisher 和有界操作 timeout；只从已鉴权 uint uid 取发送者。读取前限制 16 KiB，复用 DecodeRequest/Normalize，严格校验类型、未知/重复字段、UTF-8/代理项、UUID/ID/枚举/内容及时间；不做 DB 权限/冲突预查、Redis 入队或缓存/通知。
- 新增 router.RegisterSubmission，显式为现代 engine 注册 /auth/sendMessage，并以同一父 deadline 覆盖鉴权和提交。新增 JwtBearerAuth，仅接受 Authorization: Bearer；没有 header 时不调用旧 ExtractToken/FormValue 读取正文，query/form/raw token 均拒绝。旧 JwtAuth 和旧 Router 保持原调用路径。
- 新增共享 SubmissionResponse/Submission DTO 及规范 attempt ID 检查。只有 accepted+nil error、匹配逻辑 commandId、有效 attempt 及非零 connection/channel epoch/tag 才返回 HTTP 202。not_published+busy 为 429/Retry-After:1，明确 Return 为 503/PUBLISH_UNROUTABLE，NACK 为 503 依赖错误，unknown/矛盾/不完整结果保守 503/PUBLISH_UNKNOWN。
- 每个 HTTP 请求只调用一次 publisher，不自动重试。错误分类依据实际 Outcome，不将 unknown+busy 错映射为 429；已知完整 accepted 也不因写响应前取消而降级。返回规范逻辑身份和可用 attempt，原始错误/URL/非法结果字段不外泄，不包含 seq/persisted/delivered。
- cmd/loadtest 新增显式 -mode acceptance，使用已有请求文件、sender-id 与环境变量 token；只做接受协议探测，不创建账号/登录/WS，不混用旧 200 指标。默认一次，-attempts=1–100 为同一原键/原内容的显式重放；文件始终保留，输出不能覆盖同一文件。不跟随 redirect，响应限长，只有完整 202 且身份/hash 匹配才计 accepted；未知/丢失/旧 200 不清除原请求，先前 accepted 不倒退。
- 新增[确认式发送适配](command-submission.md)，同步协议、开发清单、README、测试和压测指南。P3.6/P3 保持未完成；本轮没有注册生产 API/后台角色、修改开发配置/依赖、启用业务主程序或切换共享数据。

### 自审与验证

- 新增 6 组 handler、2 组路由、3 组客户端默认测试。验证严格 JSON 与身份、规范 key/hash 和逐次新 attempt、同键异内容延后冲突决策、正文/NUL/时间保留、16 KiB 边界/未知长度流、无身份零读取、错误 Content-Type、Bearer-only/黑名单/Redis/鉴权 deadline 拒绝。
- 结果矩阵覆盖 202、429、not_published、Return、NACK、unknown、unknown+busy、accepted+error、错 commandId/缺 attempt/非法 attempt、零 epoch/tag/未知 Outcome 等矛盾结果；发布只调用一次，HTTP reason 固定，不回显诊断。预取消、父期限、调用后取消的 unknown 与已知 accepted 分别验证；正文读取被取消不误报需要修改输入的 400。
- 客户端验证已有文件在 unknown、响应丢失、旧 HTTP 200、坏 JSON/过大响应、错身份/缺 attempt 等情况下仍保留，已知 accepted 不倒退；明确 pre-publish/未接受分类独立，redirect 不转发 token/body，非法配置/输入/输出别名在网络 I/O 前拒绝。
- 实际 confirmed_202 场景运行新 Gin 路由、真实 JWT/Redis 黑名单与 publisher，读回持久信封核对发送者、规范身份/hash、内容/NUL/时间和 attempt。auth_and_input 场景证明伪造发送者、重复/超大请求、非法/被撤销 JWT、Redis 客户端关闭均无队列消息。
- 实际 lost_confirm 场景由本轮 TCP 代理丢弃真实 broker confirm；首请求等待时额外请求返回 429/Retry-After，首请求到期返回 unknown，而控制连接可读到原文。新 epoch 恢复本身不重发；原键显式重试返回 202，逻辑身份不变、物理 attempt 更新。
- 实际 real_return_plus_ack 场景只移除本轮主绑定，真实 Return+ACK 映射为 503 unroutable，不误报 accepted；fixture 恢复绑定，应用不修复。mysql_offline 场景按标签核验只停止本轮 MySQL，实际确认式 HTTP 仍返回 202，信封可读回。
- 自审区分输入/响应不确定性和业务终态：400/401/429 不证明先前 unknown 尝试没有入队；客户端不换键、不删除原文件。handler 不提供整个 HTTP server 的慢头/慢正文硬期限，后续 API owner 必须配置 ingress timeout 并 join 活动请求；publisher/鉴权调用须遵守 context，不丢弃后台 goroutine。

环境：Windows amd64、Go 1.27.1、GCC/race；沿用仓库 GOCACHE 和不存在的 ICHAT_CONFIG_FILE。默认测试只用注入依赖与自有 loopback HTTP，不读取开发 .env/config、不访问真实依赖。真实测试使用 StartWithRabbitMQ 独立 project/vhost/随机账号/端口，按标签回收；JWT key 使用本轮 runId 派生测试值。未运行业务 main、迁移/重置共享数据或发送 OS 信号。

| 命令/检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./service -run '^TestSubmitCommand' -count=1 -timeout=45s` | 首轮通过，2.585s |
| `go test -mod=readonly -race ./service ./router ./cmd/loadtest -run 'Test(SubmitCommand\|SubmissionRoute\|AcceptanceProbe)' -count=30 -timeout=60s` | 全部连续 30 轮通过，service=4.700s、router=2.819s、loadtest=6.347s |
| `go test -mod=readonly -race ./service -run '^TestSubmitCommand' -count=30 -timeout=60s` | 最后补充恰好 16 KiB/正文读取取消回归后，全部 handler 组 30 轮通过，3.677s |
| `go test -mod=readonly -race ./... -count=1` | 全部通过；主包=11.567s、application=2.866s、service=2.629s、router=2.203s、loadtest=2.291s、rabbitmq=7.657s，无 race；最后新增边界用例由专项覆盖，生产代码未再改动 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库集成标签编译通过 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestCommandSubmissionIntegration$' -count=1 -v -timeout=5m` | 5 个真实场景全部通过，suite=27.10s、包=31.545s |
| gofmt、`git diff --check`、新增文件空白/文档链接检查 | 全部通过 |

实际 runId=`213d870e4b6041eac5b4081dc700cccf`，broker=4.3.6、client=v1.15.0，cleanup 成功；本轮无失败测试、自动审批拒绝、引擎重启、共享数据变更或环境重置。真实 HTTP NACK、完整 API owner/慢客户端和业务事务尚未实测；不将上轮/历史结果计为本轮验证。fixture 控制连接的读取/ACK 仅为核对传输，不是业务提交后 ACK。

### 后续边界

P3.6.1 范围收口，P3.6/P3 尚未整体验收。当前是可显式注册的真实确认式路由及协议探测，默认 legacy 入口没有切换，现代生产工厂仍为空。接受就绪能力、HTTP server/活动请求生命周期、真实鉴权监控源需由下一小点装配；业务消费者、Outbox、状态/同步恢复与端到端验收仍按后续阶段实施。新协议探测不提供吞吐/落库/送达或自动恢复指标。

建议 commit message：`feat(api): add confirmed command submission and protocol probe`

下一小点：**P3.6.2：生产 API 角色的受控 HTTP 生命周期、Redis 鉴权探测与运行时装配**。完整管理 ingress 期限/停止准入/drain/存储归属，选中未完成后台角色仍须在 I/O 前拒绝。未完成 consumer 时仅在独立测试环境积压，本轮不继续下一小点、不自动提交。

## P3.6.2：生产 API 生命周期、鉴权探测与运行时装配

状态：2026-10-18（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译和隔离 Docker 实测。代码基线为用户提交的 P3.6.1 `7ec70eb`，开始时工作区干净。本轮完整完成既定范围，不再拆分或推进下一项，不自动提交。

### 实现

- 新增 apiService / apiAssembly：构造 idle，Start 才创建独立、延迟连接的 MySQL/Redis、真实 TokenService、两个监控器及 HTTP；不迁移 schema 或绑定旧全局。
- 独立 Gin engine 只提供新发送和四个健康路由。复用受控 HTTP transport owner 的活动请求跟踪，给新 API 配置停止准入的固定 JSON 503；默认 legacy 行为保持。
- 增加头/正文读取、写、空闲和头大小上限，请求 deadline 包含 Redis 鉴权及发布。真实 TCP 慢头/慢正文受到服务器期限约束，不因 handler deadline 无法中断正文读取而泄漏连接。
- 鉴权探针实际 GET 每实例随机 key，Redis.Nil 视为健康，其余错误保持未就绪；复用有新鲜度的探针缓存，健康请求无网络 I/O，每次发送仍实际检查黑名单。Redis 断连可在同一客户端上恢复。
- 单一 cleanup owner 管理所有部分启动、正常退出和意外 accept loop 退出。停止准入不取消在途发布；Drain 不释放存储；强制关闭时取消工作并停止探针，只有 HTTP 请求、探针、accept loop 全部真实退出后才关闭一次客户端。
- Close observer 过期返回 pending；同一 owner 继续保留资源并允许再次等待，Done 表示实际清理完成。执行器独立预算可能先结束并关闭共享 MQ，API 不会把 observer 超时当作已 join。
- 默认 runProcess 为显式 api 模式补入真实生产工厂，纯角色/MQ/API 校验后才注册信号和联网；已有注入工厂仍保留。worker/all 完整报告缺失后台角色。配置默认 legacy 未切换，没有启用业务主程序或修改开发数据。
- 新增 [API 生命周期](api-runtime-lifecycle.md)，更新 README、开发清单、进程/模式/装配/执行器/就绪/发送及测试说明。P3.6 范围收口；P3 整体验收留待下一项核对。

### 验证与自审

- 默认专项覆盖 idle 构造、成功 setup 取消隔离、Redis GET 失败恢复、停止准入保留已进入的发布、正常 drain 不关闭客户端、关闭 deadline 后请求和探针继续持有存储、各阶段 handle+error/nil/typed nil 回滚、启动中关闭、意外 listener 退出和关闭后拒绝重启。
- 配置测试覆盖模式/监听/HTTP 期限/大小、JWT、Redis 地址与池、MySQL、依赖/角色输入，证明纯检查不调用 opener。进程测试证明默认 API 工厂实际选择、idle 服务关闭、worker/all 后台缺失以及配置失败先于信号/执行器。
- 实际 Docker suite 从 defaultProcessDependencies 的真实 API 工厂进入 RunRuntime，没有 fixture/no-op 业务服务。receipt barrier 只在实际 publisher 完整确认后延迟返回，不制造接受结果。测试使用可控进程 context，没有向宿主投递 OS 信号。
- 5 个场景：Redis 初始离线仍 live、恢复接受、再断连撤销接受后同客户端恢复；真实 confirm 后进程取消仍 drain 成功返回 202；强制关闭超时后进程报告 shutdown failure，挂起 handler 仍持有可用 Redis/SQL，解除屏障后真实 join/关闭；端口占用回滚；核验归属后停止本次 MySQL，storage 503 而 accept/live 200、实际发送 202。
- 202 的 commandId / publishAttemptId 与真实队列信封匹配；强制退出时已确认命令仍在队列。没有业务 consumer，最终控制 Get/ACK 只用于 fixture 核验，不代表事务提交后 ACK。旧全局快照未变化。
- 初次编译发现既有 RedisTokenBlacklist 构造器接收具体 *redis.Client，改为 API 内部注入 token factory，保留既有鉴权公开 API；集成测试初次标签编译修正 AMQP Dial 参数和扁平信封字段。之后全部检查通过。首次 Docker 后加强进程错误、信号注销及具体客户端已关闭断言，重新实测通过。

固定 Go 1.27.1；GOROOT 清空、GOCACHE 指向仓库 .gocache；ICHAT_CONFIG_FILE 指向不存在的单元配置文件。没有读取开发 .env/config，也没有更改依赖或配置文件。

```powershell
go test -mod=readonly -race . -run 'Test(API|RunProcess|LegacyHTTP)' -count=1 -timeout=60s
go test -mod=readonly -race . -run 'Test(API|RunProcess|LegacyHTTP)' -count=30 -timeout=120s
go test -mod=readonly -race ./... -count=1 -timeout=90s
go vet -mod=readonly -tags=integration ./...
go test -mod=readonly -tags=integration ./... -run '^$'
go test -mod=readonly -race -tags=integration . -run '^TestProductionAPIIntegration$' -count=1 -v -timeout=7m
```

结果：专项 PASS（3.288s）；30 次专项 PASS（14.292s）；全仓库 race 全绿（主包 11.889s、rabbitmq 7.477s，其余包通过或无测试）；vet / 标签编译通过。Docker 首轮 PASS（主包 39.468s），加强断言后最终 PASS（主包 26.345s，suite 23.43s）。

两轮独立 runId：`f732ab8bee55924c96a23a10ea3a469f`、`154d7f88f6ea0837332cda78e69d908f`；RabbitMQ 4.3.6 / amqp091-go v1.15.0。每轮 fixture 清理均成功，Redis 故障仅作用于本次 loopback 代理连接，MySQL 控制及最终清理均先检查项目/run 标签，没有访问共享环境。

### 边界与下一项

P3.6 的确认式 handler、客户端协议探测和生产 API 装配范围完成。API 当前没有新登录、WebSocket、状态/同步/历史适配；默认配置仍为 legacy，未完成 consumer 时仅在独立测试环境积压。真实 broker NACK 的 HTTP 路径、OS 信号投递、吞吐/容量、业务提交后 ACK、隔离/Outbox、三节点与端到端恢复不在本轮验收范围。

下一项：**P3 总体验收核对与文档收口**。核对已有证据与阶段要求，区分库级、生产 API 和后续业务消费范围，再进入 P4；本轮不继续下一项、不自动提交。

## P3：总体验收核对与文档收口

状态：2026-10-18（Australia/Sydney）完成阶段核对、专项补强、全仓库随机顺序 race、静态检查、标签编译和集中 Docker 回归。基线为用户提交的生产 API 生命周期 `2c9c3bb`，开始时工作区干净。P3 基础设施、可靠 publisher 与接受入口范围收口；本轮不继续 P4、不自动提交。

### 核对与补齐

- 新增 [P3 验收对照](mq-reliability-acceptance.md)，逐项映射 P3.1—P3.6 的实现、可执行测试和核心断言；五项验收条件分别有直接证据。
- 明确 F05—F38 中完成/部分完成/后续责任。confirm 丢失后的稳定身份与新 attempt 不等于唯一业务落库；HTTP 响应丢失的协议探测不等于完整客户端恢复；单节点重启不等于三节点容灾；fixture Get/ACK 不等于事务提交后 ACK。P4—P8 的剩余验证继续保留。
- F09 原测试直接覆盖写失败后的旧 ACK 隔离，确认超时退休由另一测试验证。本轮将 TestPublisherLateAckCannotReachAnotherSlot 扩为 write_failure / confirm_timeout 两个时序：首发布 unknown、旧 channel 只关闭一次，新发布使用同 tag=1、不同 channel epoch；注入旧 ACK 后不能完成新请求，新结果还必须实际消费自己的 ACK，避免仅靠即时未完成检查。
- 更新 MQ 配置、拓扑、publisher、supervisor、消费传输、资源/lifecycle、legacy/现代运行时和 API 文档中过期的“现代工厂为空”“未接入 HTTP 202”“下一项 P3”状态。保留历史实施记录，不将历史结果改写成本轮验证；同步 README、测试入口和阶段清单。
- 修正 topology 包的过期注释；没有改变业务行为、依赖、配置默认值或共享数据。显式 api 继续只提供新发送/健康，worker/all 继续拒绝缺失的后台业务角色，默认 legacy 保持。

### 本轮验证

使用固定 Go 1.27.1、仓库 .gocache、不存在的 ICHAT_CONFIG_FILE，清空不匹配的 GOROOT。没有读取开发 .env/config、运行业务 main、执行迁移或重置共享数据。

| 命令 / 检查 | 最终结果 |
| --- | --- |
| `go test -mod=readonly -race ./... -count=2 -shuffle=on -timeout=120s` | 补强回归后的最终两轮全绿；主包 17.285s、rabbitmq 11.807s，其余包通过或无测试；无 race |
| `go test -mod=readonly -race ./messaging/rabbitmq -run '^TestPublisher(ACKFirstDrainsOriginalReturn\|LateAckCannotReachAnotherSlot\|ConfirmTimeoutAndConcurrentClose)$' -count=30 -timeout=60s` | 加强“必须消费自己的 ACK”断言后 30 轮通过，6.377s |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 通过 |
| `go test -mod=readonly -race -tags=integration -p 1 . ./tests/testenv -run '^Test(RabbitMQ.*Integration\|MQPreparationIntegration\|RuntimeExecutionIntegration\|RuntimeReadinessIntegration\|CommandSubmissionIntegration\|ProductionAPIIntegration)$' -count=1 -shuffle=on -timeout=20m -json` | 最终 13 个 suite 全部通过，0 fail / 0 skip；主包 31.432s、testenv 628.496s |
| gofmt / diff 空白 / 本地文档链接 / 验收测试名 | 检查通过；测试名定位实际 Go 声明，不以说明文字代替可执行用例 |

集中集成实际 suite、耗时和 runId：

| suite | 秒 | 独立 runId |
| --- | --- | --- |
| ProductionAPI | 29.32 | `7d248ad8dae770d0ce346a21a2a2dde5` |
| RabbitMQSupervisor | 46.34 | `84c7aac9b0c5fdafa033a0ef219e5460` |
| CommandSubmission | 25.85 | `72fac4f2cf329fd4a6495ebe064883c8` |
| RabbitMQTopologyMonitor | 26.72 | `ef865efa0eede2395cf41198ef6b2dfa` |
| RabbitMQTopology | 213.63 | `ce12dcb4a2e981e258b46a33e8ffe356` |
| RabbitMQPublisher | 21.47 | `05888f2ac00f2d8a2d97e01a6c6f240a` |
| RabbitMQEnvironment | 45.44 | `c70dfd7ea93afb3274830f40e55b6c9e` / `bd7083f77a7b2927d25cc294e9129593` |
| RuntimeExecution | 39.10 | `8617dea971f16f333f9864db076eaadc` |
| RabbitMQConsumerChannel | 44.66 | `8494e0c263572e8633cb809686b2ce58` |
| MQPreparation | 79.26 | `cda5962e58ac2779a77d87d7b17f2c9f` |
| RuntimeReadiness | 25.89 | `17b460c51eb0009c760b1a71a8860815` |
| RabbitMQConsumerSession | 35.43 | `3a79f279c147f692647fcc6ecd05cc00` |
| RabbitMQSession | 22.04 | `ec3907222a5afcf6010b22ab77c09283` |

集成 shuffle seed：主包 `1792317420874514800`，testenv `1792317458738256100`。本地 JSON 日志保留在忽略的 `p3-acceptance-integration-final.log`；原失败日志为 `p3-acceptance-integration.log`。表格和结论来自本轮最终日志，不引用此前通过代替本轮实测。

Docker 初次未运行：第一轮 13 个 suite 都在创建资源前的项目查询处遇到缺失 dockerDesktopLinuxEngine pipe，整轮失败（主包 1.978s、testenv 3.484s），不计为通过。随后隐藏启动已安装 Docker Desktop，确认 server 28.4.0 后重跑全部 suite，得到上述最终通过。无自动审批拒绝，没有引擎重置或共享资源清理。

最终实际核验 broker 4.3.6 / amqp091-go v1.15.0。拓扑 suite 保留 broker 默认约 180 秒 DLX 重试间隔，没有为缩短验收放宽契约；supervisor 重启仍使用既有 fixture 原端口 override。所有 fixture Close 成功，完成后按本轮 14 个 runId 额外只读核对容器、卷、网络均为 0；没有遗留当前任务资源。集中范围为 MQ/API，未重跑整个仓库的 legacy/MySQL 业务集成，不记录吞吐或端到端性能基线。

### 阶段结论与下一项

P3 基础设施、可靠 publisher 和新接受 API 范围验收收口。P3 对重投的证明止于稳定命令身份、物理 attempt 隔离和 broker 中原文保留；唯一业务 message/seq/Outbox、消费权限拒绝、隔离审计及完整客户端恢复仍须后续接线与故障演练。

下一项：**P4.1：MQ delivery 到 P2 幂等事务的持久化消费适配**。开始前按 wire 校验、事务结果、ACK/重投和角色生命周期拆成可审查小点；隔离及 Outbox 等必需角色未完成时继续保留 worker/all 门禁。

建议 commit message：`test(mq): close P3 acceptance and cover late ACK after timeout`

## P4.1.1：单条 MQ delivery 到 P2 事务及提交后 ACK

状态：2026-10-19（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译及独立 Docker 实测。基线为用户提交的 P3 收口 `067cc9a`，开始时工作区干净。按上一轮交接要求，将 P4.1 拆为本轮单条处理，以及下一小点受控 worker/角色生命周期；P4.1/P4 整体保持未完成，不自动提交。

### 实现与边界

- 新增 `workers.PersistenceHandler`，构造只捕获具体 `MessageRepository.PersistCommand`，不联网、启动 goroutine 或占有共享池。Handle 接收实际 ConsumerDelivery，要求有 deadline 的 context，事务与 ACK 共用期限。
- 严格复用 P2 JSON/信封校验，重新核对业务身份/hash、版本、字段、时间与 attempt；核对持久标记、JSON ContentType、空 encoding、主 exchange/routing key 及 AMQP MessageID/CorrelationID。原 body/headers 保持不变，redelivered 不参与幂等身份。
- 直接执行 P2 完整权限/seq/message/命令终态/Outbox 事务；不拆开写入或追加缓存/通知。applied、replayed、业务 rejected 与无原结果的 ID_CONFLICT 在仓储成功结束后均可 ACK。
- 释放责任前复核终态 disposition、请求身份/hash、时间与消息/会话/拒绝字段；零值、部分结果、冲突附原结果和占位拒绝均不 ACK。SQL/回滚错误、COMMIT 未知和 ACK 错误返回空 execution，保留 errors.Is/As；常用错误格式不打印正文、SQL 或凭证。
- ACK 只调用原 handle，一次、单条、multiple=false；不使用任意 tag，不重发 ACK，不做 Next/Reject/本地重试或主动隔离。调用者遇错应停止领取，原 channel owner 受控关闭后通过新 session 幂等恢复，不能立即 requeue 热循环。
- 新增 [持久化消费适配](mq-persistence-consumer.md)，同步阶段清单、README、测试入口及消费传输/P3 验收后续说明。生产 factories、默认 legacy 配置和 worker/all 缺失角色门禁均未变。

### 默认测试与自审

- wire 错误均不调用仓储/ACK，包括非法/超大/重复/未知 JSON、无效 UTF-8、版本/身份/hash/规范字段错误、属性/route/attempt 不匹配；失败不修改原文。
- 覆盖规范命令/context 原样传入、applied/replayed/conflict 及 persisted/rejected、部分/矛盾终态拒绝、仓储尚未完成时禁止 ACK、每次调用最多一次仓储/ACK、错误分类与安全日志。
- 覆盖 COMMIT 未知、仓储返回结果同时带错误、ACK 不确定、nil/无期限/预取消，以及仓储成功后取消仍不尝试 ACK。底层具体传输 tag/multiple、复制/旧 handle 隔离继续复用 P3 已验证的 ConsumerDelivery 实现。

### 实际 MySQL/RabbitMQ 验证

新增 `TestPersistenceConsumerIntegration` 的 8 个顺序场景，使用本轮单一独立 MySQL/Redis/RabbitMQ fixture。可靠 schema 显式迁移，identity 表仅创建测试用户/群/关系；写入前检查数据库等于本轮 `ichat_test_<runId>`，不读取开发 .env/config。

- 文本/图片/音频和群消息真实落库；新 publishAttempt 重放和同 key 跨目标异 hash 冲突不增加业务行、seq 或 Outbox。
- 已确认接受后移除群成员，新命令提交 rejected + 单个 command.result、无 message；此前 persisted 命令仍重放原终态。
- 两个独立消费 channel 竞争同身份，恰好一次 applied、一次 replayed，各自通过原 delivery ACK。
- `before_commit` / `after_commit` 使用实际测试子进程、本适配器、真实 SQL 事务和消费 session；仅 integration 测试 driver 在实际 Commit 前/成功后设置 stdout 屏障。父进程独立 SELECT 分别看到 0/1 条 message 后强杀进程并 wait；原 publishAttempt 重投，分别 applied/replayed；最终唯一 message/seq/两个事件。生产构建没有崩溃控制接口。
- 实际 COMMIT 成功后 driver 向调用方返回 EOF，适配器识别 commit_unknown、不 ACK；关闭旧 channel 后原 delivery 重投，复用既有终态。此为实际提交后的调用结果注入，不是 TCP 层 COMMIT 响应丢包演练。
- 归属核验后停止本轮 MySQL，处理报错且原 delivery 未 ACK；恢复数据库、更新可能变化的测试端口、等待 Ping 成功后，由新 session 恢复原 attempt 并落库。没有自动重连业务循环或 circuit breaker。
- 坏 JSON 无业务处置；关闭旧 channel 后 body/自定义 headers 原样重投，末尾继续保留在本轮队列中由 fixture 整体清理，不用控制 ACK 假装隔离审计。
- 成功场景均在关闭消费者后以控制连接验证主队列为空，排除仅靠本地 ACK 返回值判断；最终核对每个 conversation.last_seq、消息数和最大 seq 一致，无 seq 洞。

固定 Go 1.27.1，清空不匹配 GOROOT，GOCACHE 为仓库 .gocache，ICHAT_CONFIG_FILE 为不存在的单元配置。没有更改依赖/配置、运行业务 main 或操作共享开发数据。

| 命令 / 检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./workers -count=1 -timeout=60s` | PASS，2.005s |
| `go test -mod=readonly -race ./workers -count=30 -timeout=60s` | PASS，2.803s |
| `go test -mod=readonly -race ./... -count=1 -timeout=120s` | 全仓库通过，无 race；主包 11.176s、rabbitmq 7.055s、workers 1.770s，其余包通过或无测试 |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库标签编译通过，不代表运行全部集成测试 |
| `go test -mod=readonly -race -tags=integration ./workers -run '^TestPersistenceConsumerIntegration$' -count=1 -v -timeout=9m` | 最终 PASS，8 个场景全部通过，suite 29.49s、包 31.942s |

最终通过 runId：`d52bed4f4ed6c79395d65d0c3df029d9`，实际 RabbitMQ 4.3.6 / amqp091-go v1.15.0。两轮先前失败 runId 为 `00c5a39cabcd2acdeeba856503b873f3`、`57af490409aa3d1f3d4a83fbafcfc5a4`：首次夹具将真实 `relation` 写为 `relations`，且数据库重启未等待就绪，前置失败留下队列消息导致后续错误；修正为前置失败即终止 suite、惰性 SQL 连接等待恢复。第二轮 8 个场景通过，最终 seq 断言发现恢复连接池错误归到子测试 cleanup；改由 fixture 持有到父测试结束，第三轮完整通过。失败轮次不计为成功。

Docker 在沙箱内不可访问，所需测试和只读核验通过自动审批在沙箱外执行；无自动审批拒绝。三轮 fixture Close 均无清理错误，完成后按三个 runId 只读查询容器/卷/网络，三轮各类资源均为 0。测试中的 MySQL EOF/断连日志来自本轮预期停机注入，不是新的业务重试策略。

### 下一项

P4.1.1 单条适配范围完成；不宣称生产 worker、自动恢复/隔离或端到端完成。下一小点：**P4.1.2：顺序 worker 的受控准入、停止/drain/取消/join、角色生命周期及就绪适配**。独立消费者传输已允许多 session，但本轮不装配生产循环，不启用 worker/all；隔离/Outbox 等必需角色齐备前继续门禁。持续数据库故障的暂停/退避、可靠隔离转移、隔离审计/重放、真实 ACK 丢帧和已落库数据的组合验证留待后续阶段。

建议 commit message：`feat(worker): persist MQ deliveries before acknowledging`

## P4.1.2：顺序 worker 生命周期、就绪与运行时角色工厂

状态：2026-10-19（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译及独立 Docker 实测。基线为用户提交的 P4.1.1 `dbef91f`，开始时工作区干净。本轮完成 P4.1.2，P4.1 的既定两步范围收口；P4 整体未验收，不自动提交或推进下一项。

### 实现与所有权

- 新增 `workers.PersistenceService`，闲置构造时只校验并复制输入，复用具体 P2 仓储/P4.1.1 handler；不探测、订阅、迁移或启动任务。固定 1—32 个独立消费者、prefetch 1—256，拒绝 nil/typed nil、失效/重复 session Done 和无效期限。
- Start 在共同 setup 下执行真实存储探测、依次打开全部订阅和最终新鲜探测；全部成功后才启用每连接一个顺序消费循环。setup 成功后取消不影响独立 lifetime。每次处理共用一个有界事务/ACK context，不为事务创建可丢弃的后台 goroutine。
- 同步准入门禁覆盖停止与 Next 返回的竞争；停止后收到的 delivery 不处理、不 ACK。健康空队列超时继续等候，业务终态正常 ACK 后继续。任一处理/传输/存储/新鲜度错误或受控任务 panic 使整组停止领取，不重试、立即 requeue 或重开连接。
- StopAdmission 不做 RPC，也不取消已准入事务；Drain 等待 setup、工作循环及探测真实 join，保留提交/ACK 机会。Close 才取消剩余处理；observer 过期明确 pending，后续可等同一个 owner。部分启动 handle+error 和晚返回 handle 统一保留，工作/探测未返回前不关闭订阅。
- 所有订阅共用清理预算并发 Close，Done 仅在自有工作和 Close 调用真实返回后关闭；并发重复 Close 不重复清理。订阅关闭退役其专用 session，MQResources 继续持有整组，服务不关闭共享 SQL 或 MQ group。外部 SQL owner 必须等服务 Done，不能把 observer 超时当作已 join。
- ConsumerReady 只读本地状态/订阅 Done/lifetime/最近成功 SQL Ping；有效年龄为 ProbeInterval + 2*ProbeTimeout，迟到成功不能恢复停止实例。默认 Go 参数为 poll=1s、process=5s、probe interval=2s/timeout=1s、close=5s，均限制正值且≤60s；本轮不增加 YAML/env。
- 新增 `application.NewPersistenceFactory`，把执行器的主队列消费者适配到真实服务，拒绝 publisher/其他队列；静态验证 RuntimeService/RuntimeConsumerReadiness。仅提供可注入工厂，默认进程后台工厂、legacy 配置和 worker/all 门禁保持，隔离/Outbox 缺失不能用 fixture 服务代替。
- 新增 [持久化生命周期](mq-persistence-lifecycle.md)，同步 P4 清单、README、单条处理、运行时/健康文档及测试入口。所有外部 I/O 都由显式 Start 或 integration fixture 触发。

### 默认验证与自审

- 覆盖成功 setup 取消隔离、处理准入及提交/ACK drain、空队列等待、晚 delivery、多个 worker 失败后不取消已准入 sibling、无自动重开、partial/nil/typed nil/handle+error/panic 启动回滚与第二次探测失败。
- 覆盖启动中 Close、处理/探测/closer 不响应取消时 pending 及真实 join、并发 Stop/Drain/Close、关闭前闲置及禁止重启、lifetime 取消、处理期限和 handler/probe panic 的清理及错误脱敏。
- SQL 连接计数证明工厂/构造无 I/O；服务关闭后共享 pool 仍可 Ping。工厂拒绝空/错误/重复消费者、取消及坏配置；只提供 persistence 工厂时仍完整报告 quarantine/Outbox 缺失。
- 20 轮首批专项发现工厂直接把 `(*PersistenceService)(nil), error` 转成了非 nil RuntimeService 接口；修正为错误时显式返回 nil interface。之后专项 30 轮通过，最终增加期限/panic/lifetime 用例后再次 30 轮全部通过。此失败不计为通过。
- 自审补上最终启动探测，避免慢订阅打开后把早期 Ping 当作新鲜状态；准入同时检查所有订阅 Done；多错误 Unwrap 不包含 nil cause。

### 实际 MySQL/RabbitMQ 集成

`TestPersistenceLifecycleIntegration` 使用本轮独立 schema/vhost/随机账号和端口，可靠表显式迁移，身份写入前核对 schema；不读开发 .env/config。六场景均通过：

1. 真实 COMMIT 成功后 driver 屏障尚未返回，停止准入后 work context 保持有效，Drain observer 超时仍 pending；释放后单条 ACK，未处理的预取原 attempt 由新 session 接手。
2. 强制 Close 后已提交数据和未返回 callback 继续持有，observer 超时后共享 SQL 仍可 Ping、Done 未关闭；释放并 join 后原 attempt 重投，只 replay 既有 message/seq/两个事件。
3. 两连接顺序循环处理 8 个身份各 2 次物理发布；最终强化为先观察 broker messages=16，再消费并等待 messages=0，逐身份唯一消息/终态/事件，不凭首份落库提前停止。
4. loopback 代理实际解析并丢弃一个 basic.ack，本地方法成功，服务正常 drain/close；原 attempt redelivered，仓储 replay，不增加消息/seq/Outbox。本轮将真实消费 ACK 丢帧与业务持久化组合验证。
5. 实际 `NewPersistenceFactory` 经 RunRuntime 激活两连接持久化角色，真实 COMMIT 屏障中进程 context 取消，服务/聚合就绪撤销，释放后事务/ACK、服务/MQ 顺序退出，共享 SQL 保留。quarantine/Outbox 为明确的测试占位服务，不具备审计/通知，不代表默认生产装配完整。
6. 归属核验后停止本轮 MySQL，空队列也因实际 Ping 故障退出；MQ 继续接受；恢复数据库并等待可用后，旧服务仍停，新建 session/服务才处理积压。更新可能变化的测试端口，最后核对各 conversation 的消息数、最大 seq 和 last_seq 一致。

SQL 屏障仅在 integration driver 中，Ping 明确转发到实际 MySQL；生产没有控制接口。没有投递宿主 OS 信号、运行业务 main、启用默认 worker/all 或实现自动恢复。测试回调违反取消时没有丢弃 goroutine；pool 清理由真实服务退出后执行，运行时失败路径也等待执行器返回。

固定 Go 1.27.1，GOROOT 清空，GOCACHE 为仓库 .gocache，ICHAT_CONFIG_FILE 指向不存在的路径。命令与最终结果：

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./workers ./application -run '^TestPersistence(Service\|Factory)' -count=30 -timeout=90s` | 最终 PASS，workers 5.655s、application 2.791s |
| `go test -mod=readonly -race ./... -count=1 -timeout=120s` | 全仓库 PASS，无 race；主包 10.837s、application 3.076s、rabbitmq 7.057s、workers 1.824s；之后只增加专项测试/强化集成断言，生产代码未再变化 |
| `go vet -mod=readonly -tags=integration ./...` | 最终通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库标签编译通过；不代表重跑所有集成 suite |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceLifecycleIntegration$' -count=1 -v -timeout=9m` | 最终 PASS，6 个场景无 fail/skip，suite 41.30s、包 44.064s |

首轮通过 runId `2bdcb2b57a198a02fb2618edff2c488f`（suite 31.18s、包 33.913s）；强化队列正数基线及失败清理等待后，最终通过 runId `65e649448a1b6278b8a3aaff566d7487`。实际 RabbitMQ 4.3.6 / amqp091-go v1.15.0。数据库故障恢复的 fixture Ping 由 10ms 调整为 200ms 条件轮询，减少未就绪握手日志，不修改 worker 的故障策略或弱化断言。

两轮 fixture Close 成功，结束后按各自 runId 只读核对 Docker 容器/卷/网络均为 0。Docker 测试和只读核验通过自动审批在沙箱外执行，无审批拒绝。没有改依赖、共享配置/数据或默认入口行为。

### 阶段结论与下一项

P4.1 持久化消费适配及受控角色范围完成；持续数据库故障的自动暂停/退避/恢复、可靠隔离转移、隔离审计/受控重放及 Outbox 仍未完成，P4/P5 及端到端验收继续保留。默认后台装配在必需角色齐备前继续拒绝。

下一项：**P4.2.1：消费失败分类与有界本地重试**。区分业务终态、技术坏消息、事务可重试错误和基础设施故障，保留原业务身份、COMMIT 未知及恢复责任；不能因重试次数耗尽把正常积压当坏消息，也不能通过立即 requeue 制造重投风暴。

建议 commit message：`feat(worker): manage persistence consumer lifecycle and runtime role`

## P4.2.1：消费失败分类与有界本地事务重试

状态：2026-10-19（Australia/Sydney）完成实现、自审、专项/全仓库 race、静态检查、集成标签编译及独立 Docker 故障验证。基线为用户提交的 P4.1.2 `13de715`，开始时工作区干净。本轮完成 P4.2.1；P4 整体、自动恢复及隔离仍未验收，不自动提交或推进下一项。

### 实现与责任边界

- 新增 `ClassifyPersistenceFailure`：按固定责任阶段、类型及 MySQL 错误号遍历完整单/多错误树，区分 malformed、事务可重试、基础设施、提交/回滚未知、取消、ACK、技术及未知错误。不匹配错误文本，不只用 errors.As 取得第一个 MySQL 错误。生命周期阶段包装不改变底层 delivery 类别；混合连接/约束/未知错误禁止因同时存在 1213/1205 而重试。
- `MessageRepository.ExecuteCommand` 增加 `ErrCommandRollbackUnknown`，defer 回滚失败时保留原事务错误和回滚原因并返回空 execution；只忽略精确 sql.ErrTxDone，组合/包装中的其他失败不被隐藏。COMMIT 未知标记及业务写入/锁顺序保持。
- Handler 只在上一笔仓储事务已返回后，对 MySQL 1213/1205 有限重试。每次重用原 command、delivery、处理 context 和 ACK handle；不重新发布、不改业务 ID/hash、不领取下一条。最终安全终态后仍只有一次单条 ACK；ACK 错误不进入事务重试。
- 新增 `PersistenceRetryOptions`、`NewPersistenceHandlerWithRetry` 和 `PersistenceOptions.Retry`，工厂纯校验传递；完整零值沿用默认。默认最多 3 次（含首次）、BaseDelay=25ms、MaxDelay=250ms，最多 5 次、等待上限≤5s；指数上限的一半至上限之间随机正等待。MaxAttempts=1 禁用后续尝试，不新增 YAML/env。
- 全部事务/等待/ACK 共用原 ProcessingTimeout，不刷新 deadline、不丢弃后台 goroutine。次数耗尽只附 `ErrPersistenceRetriesExhausted`，类别仍是事务冲突；`PersistenceError.Attempts` 记录仓储调用数，保留 errors.Is/As 和安全日志。中途变为连接、技术、提交/回滚未知或取消立即停止；不把基础设施故障或耗尽当作坏消息。
- 单条失败仍交由 P4.1.2 服务停止整组领取并受控关闭。StopAdmission/Drain 保留已准入退避，强制 Close 才取消等待；自动暂停/恢复、主动隔离及审计尚未实现。任何类别均不单独授权 ACK/Reject/隔离，默认 legacy/后台工厂门禁保持。
- 新增 [失败分类与本地重试](mq-persistence-retry.md)，同步 README、阶段清单、单条/生命周期文档及测试入口。MySQL 事务边界复核使用官方 InnoDB 错误处理和错误号文档，客户端错误类型核对本机固定 go-sql-driver/mysql v1.8.1 源码，未升级依赖。

### 默认测试与自审

- 覆盖错误树混合优先级、未知文字/错误号、服务包装、提交/回滚未知及 ACK 阶段保护；sql.ErrTxDone+EOF 的组合不能掩盖回滚不确定性。
- 覆盖两次事务冲突后成功、相同 command/context、完整预算、每次事务返回后退避、最终单次 ACK、次数上限/禁用、抖动范围、耗尽保持原类别/原因及 Attempts。
- 覆盖处理中途类别变化、退避取消/期限、失败 execution 为空、ACK 即使携带 1213 也不重试、工厂拒绝坏策略及安全错误格式。所有测试默认无外部 I/O。
- 自审将分类应用到完整错误树，防止多个 PersistenceError 只读取第一个而漏掉 ACK；保留服务包装下的耗尽类别。等待参数使用准确的 BaseDelay 名称，避免把抖动的指数基数误称为实际最小等待。
- 字段重命名工具漏掉 integration 标签内一个结构体字面量，生命周期回归的标签编译实际捕获未知字段（尚未创建 fixture）；修正引用后回归通过。未把该次编译失败计为测试通过。

### 真实 MySQL/RabbitMQ 验证

新增 `TestPersistenceRetryIntegration`，复用独立 fixture，向本轮 schema 添加测试锁表。integration driver 在真实 message INSERT 后执行同一事务内的测试锁 SQL；真实 1213/1205 来自 MySQL，生产没有故障开关。只对本次测试连接设置 innodb_lock_wait_timeout=1，不修改生产或全局参数。

1. blocker 修改 127 行形成较重事务，worker 修改测试锁行后构造循环等待；实际 1213 选择 worker 回滚。仓储调用 2 次、实际回滚 1 次、提交 1 次，原 delivery 最终 ACK，唯一业务集合。
2. 实际 1205 发生在消息插入之后，整笔回滚再第二次完整事务成功；此前 seq 不被消耗，没有部分 message/Outbox。
3. 每次均真实锁超时，3 次调用、3 次回滚、0 提交；错误保持 transaction_retryable/exhausted，原 delivery 仍 busy。解除锁并关闭后原 publishAttempt 重投成功，无提前处置。
4. 实际 Rollback 成功后 driver 返回 EOF，保留真实 1205 与 rollback_unknown，只调用一次；原 attempt 随后恢复。本例为回滚调用结果注入，不是实际 TCP 响应丢包。
5. 实际 COMMIT 成功后 driver 返回 1213，仍为 commit_unknown 且一次调用；原 attempt 重放既有终态，不因错误号可重试而重复事务。本例同样是实际提交后的调用结果注入。
6. 为本轮下一 seq 增加临时 CHECK，得到实际 3819；一次调用/回滚、不重试或 ACK。删除约束后原 attempt 可恢复；DDL 仅作用于本轮自有 schema。
7. 托管服务在真实 1205 后进入退避，停止准入后 Drain observer 为 pending，处理 context 保留，最终第二次事务成功并单次 ACK。
8. 同样在真实 1205 后强制 Close，退避被取消；一次调用/回滚、0 提交，无 ACK，原 attempt 由新 session/仓储恢复。

既有 `TestPersistenceConsumerIntegration` 增加 COMMIT 未知及真实数据库离线的“一次尝试及类别”断言，同轮 8 场景继续验证媒体/群权限、并发幂等、提交前/后实际子进程强杀与原文保留。另回归 `TestPersistenceLifecycleIntegration` 的 6 个场景，覆盖多连接、实际 ACK 丢帧、pending/join、RunRuntime drain 及数据库探测故障。运行时隔离/Outbox 仍为明确的测试占位角色，不冒充生产完整装配。

默认及真实测试均核对规范终态、唯一 message/两个业务事件，最终按每会话消息数/最大 seq/last_seq 检查无洞。没有重置共享数据、运行默认 main、投递宿主信号或启用 worker/all。

固定 Go 1.27.1，GOROOT 清空，仓库 .gocache，不存在的 ICHAT_CONFIG_FILE。主要命令与结果：

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./workers ./database ./application -run 'Test(Persistence(Retry\|Failure\|Factory)\|Command(HandlerFailure\|ReplayRollback))' -count=30 -timeout=90s` | 最后 30 轮通过，workers 3.173s、database 2.738s、application 2.754s |
| `go test -mod=readonly -race ./... -count=1 -timeout=120s` | 最终全仓库通过，无 race；主包 11.032s、application 2.833s、database 2.271s、rabbitmq 6.842s、workers 1.840s |
| `go vet -mod=readonly -tags=integration ./...` / `go test -mod=readonly -tags=integration ./... -run '^$'` | 通过；标签编译不代表运行所有集成测试 |
| `go test -mod=readonly -race -tags=integration -p 1 ./tests/testenv ./workers -run '^Test(PersistenceRetryIntegration\|PersistenceConsumerIntegration)$' -count=1 -v -timeout=12m` | 两个 suite 共 16 个场景全部通过，无 fail/skip；retry suite 32.77s/包 36.498s，consumer suite 29.23s/包 31.622s |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceLifecycleIntegration$' -count=1 -v -timeout=9m` | 6 个场景全部通过，suite 40.55s/包 43.228s |

首轮新 retry suite 7 场景已通过，runId `23483f4156ee9bb90b125e0a5456ba75`，suite 33.61s/包 36.474s。加入实际退避强制关闭后，最终 retry runId `6114b046e53975eb0afe36da42dab646`；consumer 回归 runId `aad161d68d696980010ee96b5a3a8bde`；lifecycle 回归 runId `5aa92a5585936e40e3caa737e4d1f8be`。实际 RabbitMQ 4.3.6 / amqp091-go v1.15.0；驱动 v1.8.1 固定未变。首轮夹具的慢 SQL 日志不是性能基线，数据库 EOF 日志来自明确的停机注入。

全部 fixture Close 成功；Docker 测试及资源查询通过自动审批在沙箱外执行，没有审批拒绝。已按四个 runId 只读核对专属容器/卷/网络，四轮各类资源均为 0，不访问开发 .env/config 或共享命名空间。

### 下一项

P4.2.1 范围完成。下一项：**P4.2.2：数据库故障暂停、退避、恢复及消费者监督**。开始前按监督状态、连接替换和退出所有权拆分；数据库持续故障不能把正常积压隔离为坏消息，恢复不能造成立即 requeue/重连风暴，过期连接/任务不能重新获得就绪。可靠隔离转移、审计/受控重放、Outbox 和端到端恢复继续保留，默认后台装配在角色齐备前仍拒绝。

建议 commit message：`feat(worker): classify persistence failures and bound transaction retries`

## P4.2.2.1：独立持久化监督与数据库故障恢复（2026-10-19）

### 范围与拆分

按阶段文档的要求，在实现前将 P4.2.2 拆成独立监督器（本项）、生产连接替换/运行时接线（P4.2.2.2）、监督恢复的运行时退出验收（P4.2.2.3）。本项完成独立库和显式资源来源的真实恢复验证，不把整个 P4.2.2 标为完成。

- 新增 `workers.PersistenceSupervisor`、参数/状态和 `PersistenceConsumerFactory` / `PersistenceConsumerGroup` 契约。构造纯本地，Start 后一个 owner 串行管理数据库探测、每轮资源获取、子服务激活、退役及退避，没有并存消费轮次。
- 数据库连接故障撤销就绪、停止领取，保留已准入事务/ACK；旧子服务 join 并关闭订阅，再调用整轮 group Close。所有释放调用真实返回后才可恢复；部分 factory handle+error、晚返回 group 和 cleanup panic/错误均保留清理责任。
- 数据库持续不可用时不获取新消费者，仅做有界真实 Ping 和可取消指数抖动等待；默认基数 1s、上限 30s，每次等待在本次上限一半至上限之间。失败计数封顶 32，只有连续 ready 达到默认 1min 后，下次故障才重置，清理耗时不算稳定时间。
- 恢复要求完整错误树都是明确基础设施证据；允许 storage 探测自身超时。提交/回滚未知须同时有纯连接故障原因，之后靠新投递幂等恢复。畸形/约束/未知分支、重试耗尽、ACK 失败、处理取消及当前订阅故障均终止，不自动反复重投，不 ACK/Reject/隔离。
- 首次明确数据库离线可使 Start 成功进入受管但不可就绪状态，setup 自身取消/超时或确定启动错误仍失败；Start 返回后取消 setup 不影响 lifetime。状态读取无 I/O，Generation 统计资源获取轮次而非 SQL 探测；已恢复的故障保留安全类别，不污染最终正常关闭的 Err。
- StopAdmission 同步撤销准入、取消 setup/probe/backoff；Drain 保留已准入 work，Close 才取消剩余工作。observer 超时返回 Pending，Done 只在自有回调全部返回后关闭；不会因 probe/factory/SQL/closer 忽略 context 而丢弃 owner 或关闭共享 pool。
- 新增 [持久化监督器说明](mq-persistence-supervisor.md)，同步主清单、README、消费/生命周期/重试/验收及测试入口。未新增 YAML/env、迁移、依赖或生产故障开关。

### 验证与自审

默认测试覆盖初始离线的五次递增/封顶抖动等待、探测成功前零 factory 调用、旧 SQL/ACK 与整轮 closer 的真实 join、停止/强制取消/pending、late probe/factory、部分获取和 cleanup 错误/panic、完整错误树恢复门槛、并发重复关闭、构造无 I/O、禁止重启及日志脱敏。

首轮单次专项通过后，30 轮压力测试实际发现停止通知与控制 context 取消之间的竞态：owner 可能把正常 drain 当成异常退役。修正为同一停止边界下取消和发布通知，并由 owner 读取受锁保护的停止状态。之后 30 轮通过；扩大退避封顶覆盖后最终 30 轮继续通过。自审同时确保清理耗时不重置连续失败退避，终止状态不被晚 Close 覆盖。

独立 Docker suite 的 4 个实际场景：

1. 两个真实消费者激活后，仅停止本轮 MySQL；等待至少两次恢复失败，旧组释放且 factory 次数仍为 1。MQ 接受四个业务身份各两个物理发布，先观察积压 8，再启动 MySQL；相同 sql.DB 上新一轮两连接自动处理至 broker 0，每个身份只有一份 message/终态/两个事件。
2. 实际 COMMIT 成功后 integration driver 一次性返回 EOF，触发提交未知；旧轮次退役、抖动等待并自动恢复，新投递 replay 规范终态，两个事务调用而非两个成功业务写入。该项是 driver 返回结果注入，不是 TCP 丢包。
3. 本轮 schema 增加临时 CHECK，产生实际 MySQL 3819；一笔事务即终止监督器，不自动换轮。删除测试约束后，用新 session 核对原 publishAttempt 的 redelivered 并完整处理，未丢原消息。
4. 初始数据库离线时 Start 进入受管 backoff，零消费者获取；MQ 接受一条命令后关闭监督器。数据库恢复后旧监督器仍 stopped，原 publishAttempt 仍是未被领取过的首次投递，显式新处理器恢复它。

所有场景最终核对每会话消息数、最大 seq 和 last_seq 无洞，并证明监督器关闭后共享 SQL 可用。容器重启可改变随机宿主端口，只有测试 connector 更新连接地址；监督器全程持有同一个 pool，不把该测试适配冒充生产数据库切换能力。

| 检查 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./workers -run '^TestPersistenceSupervisor' -count=30 -timeout=90s` | 最终 30 轮通过，3.964s，无 race |
| `go test -mod=readonly -race ./... -count=1 -timeout=120s` | 全仓库通过，无 race；主包 11.197s、application 2.882s、rabbitmq 6.924s、workers 1.958s |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库标签编译通过；不代表执行全部集成测试 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceSupervisorIntegration$' -count=1 -v -timeout=9m` | 最终 4 场景通过，无 fail/skip，suite 62.32s/包 65.032s |

固定 Go 1.27.1，清空 GOROOT，使用仓库 .gocache 和不存在的 ICHAT_CONFIG_FILE。首轮 Docker 命令因 Desktop Linux 引擎未运行而失败，runId `fad4c42ddbc4dae327a65d43aa6e3401`，夹具未创建；后台启动已安装的 Docker Desktop 后重跑。第一次实际四场景 runId `706162e905320c53fae9937904bf158b`，suite 63.90s/包 66.666s 通过；将测试辅助恢复探测降为 200ms 并移除多余 restart 后，最终 runId `87b181fe95cd698b650ae781d6be80e1` 如上通过。实际 RabbitMQ 4.3.6 / amqp091-go v1.15.0，数据库 EOF 日志对应明确停库/恢复窗口，非性能基线。

全部 fixture Close 成功；按三个 runId 只读核对容器/卷/网络均为 0。Docker 启动、测试和资源查询获自动审批，无审批拒绝；未读开发 .env/config、修改共享数据或运行默认 main。

### 下一项

**P4.2.2.2：生产消费者连接替换与运行时接线**。当前 NewPersistenceFactory 仍构造单次服务，MQResources/聚合 readiness 仍引用初始 session；下一项必须提供可替换连接所有权、只读拓扑核验和部分失败清理，再接入监督器。group Close 必须真实 join，不能把仅表示退役的 Done 或有界观察超时当作清理完成。P4.2.2.3 随后验证运行时交叉退出及共享 SQL 最终释放；隔离/Outbox 未齐备前默认 worker/all 门禁继续保留。

建议 commit message：`feat(worker): supervise persistence recovery after database outages`

## P4.2.2.2：生产消费者连接替换与运行时接线（2026-10-19）

### 实现与边界

- 新增显式 `application.NewSupervisedPersistenceFactory(sharedPool, options)`，接入真实 PersistenceSupervisor；原 NewPersistenceFactory 保留单轮行为。工厂构造及 idle claim 不做 SQL/MQ I/O，不创建后台任务，不关闭共享 SQL。
- PrepareMQ 保持初始连接准备/核验行为，成功后保存受控持久化连接来源；runtimeRoleInputs 仅向持久化角色传递 PersistenceConsumers。一个来源只可 claim 一次，在 MQ 资源锁下转移初始持久化 session 的清理条目，publisher/隔离归属保持。默认进程后台工厂、legacy、YAML/env、依赖及迁移未切换；隔离/Outbox 缺失门禁保持。
- 首次借用复核初始连接的拓扑，后续按同一已验证配置新建各 worker 独立连接，再逐项 Verify；共用 setup deadline，不按连接延长。任一步失败保留部分 group，包括 handle+error、迟到 handle；typed nil、重复连接或缺少真实 join 能力拒绝。
- 新增 ConsumerSession.WaitClosed：Close owner 完成后才取得最终 idle 快照，等待已准入传输调用。group Close 一次并发 Close/WaitClosed 所有连接，真实返回后才移除活动组；来源 Close 是同一清理 owner 的有界观察，observer 超时不丢弃获取/关闭任务或允许下一轮。
- 清理错误/panic 保留并阻止替换，关闭调用失败后仍执行 join；来源最终关闭继续报告已退役组的清理错误。group/source 常用错误格式只显示固定阶段，errors.Is/As 保留原原因。直接来源观察也不回显私有 child cause。
- MQResources.Consumers 与 runtimeHealth 在 claim 后读取当前完整组及原隔离连接；暂停/部分准备/退役均不可 ready，旧 RoleDependencies.Consumers 只作为构造快照，不再参与后续连接判定。健康读取不做 I/O，子服务 SQL/订阅新鲜度和 runtime 停止门禁继续生效。
- 增加受控连接准备错误边界，允许连接/拓扑暂时不可用、明确订阅退役/receive unavailable、尚未准入 delivery 的已知订阅 setup 失败退避换轮。处理/ACK/约束/畸形/未知并列分支仍否决恢复，不重新发布、Reject/requeue 或重试 ACK。停止引起的纯连接准备取消不误报为运行故障。
- 新增 [运行时连接来源说明](mq-persistence-connections.md)，同步主清单、README、监督/生命周期/重试、角色装配、MQ 资源、消费者 session、聚合健康、测试入口与 P3 验收的后续边界。初始 broker 离线仍使 PrepareMQ 失败；本项不增加离线启动或消费者周期 MQ 拓扑巡检。

### 默认验证与自审

默认专项覆盖角色隔离、单一纯 claim、idle 工厂及缺失角色门禁、初始复核/后续全新连接、当前快照与聚合就绪、部分失败/typed nil/重复连接、所有步骤共用期限、迟到 factory、关闭 observer 超时与真正 join、清理错误保留/脱敏、连接阶段与处理/ACK 的完整错误树，以及纯 setup 取消和禁止旧轮次复活。

自审补上三项边界：WaitClosed 不能在等待 closeDone 之前缓存 idle；纯连接准备取消不能被当成 graceful shutdown 的新错误；清理错误不能因 active 已清空而被最终 MQ Close 隐藏。group 与 source 的直接错误格式也独立验证不打印底层凭证，保持 errors.Is 诊断。

### 实际 MySQL/RabbitMQ 验证

`TestPersistenceConnectionsIntegration` 使用生产 NewSupervisedPersistenceFactory → RunRuntime 和独立 fixture，隔离/Outbox 为明确测试占位角色。四场景：

1. 实际停启本轮 MySQL，暂停期间 runtime 保持运行，三身份各两个 MQ 发布先形成积压 6，且监督器仍为第一轮；恢复后自动使用第二轮连接消费至 0。初始 session 均已退役，聚合 ready 恢复，隔离仍为原连接，关闭后共享 SQL 可 Ping。
2. 主动关闭一个空闲消费 session，旧持久化组整体退出后新建；原 Done 持续关闭，当前健康及真实业务处理恢复，隔离不重建。
3. SQL 暂停后接受一条命令，删除本轮主队列绑定；SQL 恢复后至少两次替换尝试均拒绝漂移，不修复绑定、不消费积压、不结束 runtime。人工恢复本轮绑定后恢复处理。
4. 专属 loopback 代理保留初始两主一隔离连接，下一轮只放通首个连接，实际黑洞后续 AMQP 握手。管理 API 先观察到部分连接，下一轮前确认回到仅隔离连接的基线；解除故障后全组恢复并处理真实消息。

每个场景都等待独立 SQL 可见性，在 runtime 退出 join 后直接 RPC 检查队列为空，连接回到 fixture 两个控制/publisher owner 基线；最终核对 message/规范终态/两个 Outbox 唯一及每会话消息数=最大 seq=last_seq。测试 SQL connector 仅更新容器重启后的随机宿主端口，生产 pool 归属保持。

首轮 `fc1100b30414c7ed22c15b1af42d1e3e` 因首个管理采样未显示两个已知连接而在场景前失败；改为等待基线。第二轮 `1f726815a611fcd4a5d47f6fb231281a` 的停库/聚合恢复已通过，但健康消费后的旧队列零统计使测试过早检查 SQL，第二场景断言为 0/0/0；补上真实事务可见性等待及退出后直接 broker 检查。这两次不计为完整通过。

随后 `a8bfe5e89c4f47af789aed5fe8b632ab` 四场景全部通过，suite 89.94s/包 92.802s。最终生产连接 suite `0e3bfccd3e427d99f00d9c192bf7bbcf` 四场景全部通过，suite 89.93s；同轮回归原单轮工厂 lifecycle suite `ce73aad812236947198b287cf7190432` 六场景全部通过，suite 42.63s；两 suite 共 10 场景、包 135.352s，无 fail/skip。

主要检查：

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./application ./workers ./messaging/rabbitmq -run 'Test(PersistenceConsumerSource\|SupervisedPersistence\|Persistence(Connection\|Supervisor)\|ConsumerWaitClosed)' -count=30 -timeout=90s` | 30 轮专项通过，无 race |
| `go test -mod=readonly -race ./... -count=1 -timeout=120s` | 全仓库通过，无 race |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库标签编译通过，不代表执行全部集成测试 |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistence(Connections\|Lifecycle)Integration$' -count=1 -v -timeout=12m` | 最终 10 个实际场景通过，135.352s |

固定 Go 1.27.1，GOROOT 清空，仓库 .gocache 和不存在的 ICHAT_CONFIG_FILE；RabbitMQ 4.3.6 / amqp091-go v1.15.0，未升级依赖。上述 SQL EOF 日志对应明确停库/重启窗口，不是性能指标。未读开发 .env/config、操作共享数据、运行默认 main 或提交代码。

全部 fixture Close 成功。按五个 runId 只读核对容器/卷/网络各项均为 0；测试与资源查询通过自动审批，无审批拒绝。

### 下一项

**P4.2.2.3：监督恢复的运行时退出验收**。集中覆盖连接获取/替换、SQL/ACK、探测及 group join 与 drain/强制退出/pending 的交叉故障；明确 RunRuntime 有界观察返回后共享 pool 仍须持有到监督器 Done。P4.2.2 总项继续保持未完成，可靠隔离、审计/重放、Outbox 与完整客户端恢复仍待后续阶段。

建议 commit message：`feat(worker): wire supervised persistence consumer replacement`

## P4.2.2.3：监督恢复的运行时退出验收（2026-10-20）

### 范围与修正

- 新增 [监督恢复退出验收](mq-persistence-shutdown.md)，明确 RunRuntime 有界观察返回、监督器真正 Done、MQ 退役/清理及共享 SQL pool 最终释放的不同责任。只有外部 owner 在监督器 join 后释放其 pool；pending 返回不能丢弃 SQL/ACK/probe/dial/group owner。
- 默认测试调用真实 runRuntime、NewSupervisedPersistenceFactory、连接来源及监督器，仅 SQL/AMQP 边缘和尚未实现的 companion 角色使用本地夹具。覆盖恢复订阅 setup 中正常取消、迟到探测/获取、旧组 join 阻塞、observer 到期后 pool/lease 保留，以及停止后不复活。
- 首轮默认验收真实复现恢复中订阅 setup 取消被误报为 shutdown 失败：错误被包装为 Start/subscription，而此前正常取消判定只支持 storage/连接准备。收紧到有明确责任的纯取消：只有 Start/context、Start/subscription、Start/storage 及原 storage probe 纯取消可在停止路径结束；处理、ACK、Close/清理超时、混合 EOF 和未知原因仍保留。首轮该场景失败，不计为通过；修正后 30 轮通过。
- 新增 workers 阶段判定回归，保证扩展启动取消规则不吞掉提交后处理、ACK 和清理责任。未改变事务、ACK 次数、投递身份、处理期限、重试预算或连接恢复策略。
- 原集成 helper 保留默认清洁退出行为，另支持有界 runtime observer 返回后继续等待实际 supervisor Done，确保 pending 测试不会先清理 fixture pool。所有故障屏障仅存在于测试，生产无故障控制接口。

### 实际 MySQL/RabbitMQ 六场景

`TestPersistenceShutdownIntegration` 运行生产显式监督工厂 → RunRuntime，使用独立 schema/vhost；隔离/Outbox 继续为明确占位角色：

1. 先完成真实消费连接替换，再在实际 COMMIT 成功后的 driver 回调处阻塞。正常停止后 work context 保持有效，短 Drain observer 为 pending；释放后完成原 ACK，队列实际为空，没有下一轮。
2. 同样在第二轮实际 COMMIT 后保留不响应取消的回调；RunRuntime 预算到期返回 ErrRuntimeShutdown/ErrPersistenceSupervisorPending/deadline，监督器 Done 未关闭、SQL InUse>0 且同 pool 可 Ping。释放后原 publishAttempt redelivered，幂等 replay 规范终态；最终由明确外部 owner Close pool，之后 Ping 被拒绝。
3. 专属 loopback 代理放通新组首个连接，黑洞第二个实际 AMQP 握手。观察部分认证连接后停止，取消并完整清理该组；runtime 正常退出，解除代理故障也不重连。已 accepted 原 attempt 保持首次未领取状态，显式新处理器成功处理。
4. 真实 MySQL Ping 成功后 driver 返回屏障忽略取消。Runtime observer 返回 pending 时仍持有 SQL lease，pool 可用；解除后 supervisor 真正 Done，晚成功没有更新就绪或获取下一轮，外部 owner 才关闭 pool。
5. 第二轮连接通过真实代理丢弃消费 ACK；本地处理成功且数据库已提交。正常退出后原 attempt 重投，由新处理器 replay，message/规范终态/两个 Outbox 均唯一。
6. 核验归属后仅停启本轮 MySQL，在至少两次恢复失败的 backoff 中关闭 runtime；MQ 接受的积压保持。SQL 恢复后旧监督器不复活、Generation 保持 1；显式新处理器核对原 attempt 的首次投递后持久化。

COMMIT/Ping 屏障都是实际 SQL 成功后的 driver 返回屏障，非 SQL TCP 丢包；ACK 丢帧及 AMQP 握手黑洞是实际网络代理控制。未运行默认 main、发送宿主进程信号、启用完整 worker/all 或验证实际客户端。每场景退出后直接 RPC 检查主队列为空，连接回到两个 fixture 控制/publisher owner 基线；逐命令核对唯一业务集合，并最终核对每会话消息数=最大 seq=last_seq。

### 命令与结果

固定 Go 1.27.1，GOROOT 清空，仓库 .gocache 和不存在的 ICHAT_CONFIG_FILE。依赖未升级，无 YAML/env、迁移或默认装配切换。

| 命令 | 结果 |
| --- | --- |
| `go test -mod=readonly -race ./application ./workers -run '^TestPersistenceShutdown' -count=30 -timeout=90s` | 30 轮通过，无 race；application 11.310s、workers 1.664s |
| `go test -mod=readonly -race ./... -count=1 -timeout=120s` | 全仓库通过；主包 10.843s、application 3.448s、rabbitmq 7.270s、workers 2.122s |
| `go vet -mod=readonly -tags=integration ./...` | 通过 |
| `go test -mod=readonly -tags=integration ./... -run '^$'` | 全仓库标签编译通过，不代表执行所有集成 suite |
| `go test -mod=readonly -race -tags=integration ./tests/testenv -run '^TestPersistenceShutdownIntegration$' -count=1 -v -timeout=10m` | 6 场景全部通过，无 fail/skip；suite 60.99s/包 63.682s |

实际 RabbitMQ 4.3.6 / amqp091-go v1.15.0，fixture runId `96c8f10f786a6adcdc69ee3b5d6dde14`。数据库 EOF 日志来自最后一项明确停库/恢复窗口；测试 connector 的地址更新仅处理 Docker 随机宿主端口。

fixture Close 成功，按本 runId 只读核对容器/卷/网络均为 0。Docker 测试及资源查询通过自动审批，无审批拒绝；未读开发 .env/config 或操作共享数据，没有自动提交代码。

### 下一项

P4.2.2.1—P4.2.2.3 均完成，勾选 P4.2.2；P4 整体和隔离/审计/重放/Outbox 保持未完成。下一项 **P4.2.3：可靠隔离转移**，按原始信封与稳定 transport identity、目标可靠发布、原 delivery 处置及故障验收分步实施。只有目标发布 confirm 且无 return 才 ACK 原消息，目标故障或确认未知必须保留原投递责任。默认缺失后台角色门禁保持。

建议 commit message：`test(worker): verify supervised persistence shutdown ownership`
