# P6.3.2：发送模拟器磁盘存储

`tests/clientsim.OpenFileStore(ctx, directory)` 将[发送状态核心](client-sender-simulator.md)的 SnapshotStore 接到真实本地文件。使用已有私有目录，不自动创建目录、替换坏日志或删除旧数据。P6.3.2 已完成，P6.3.3 [HTTP 恢复](client-sender-http.md)已完成，下一项 P6.3.4 通知与生产 API 交叉恢复验收。

## 所有权与使用

目录内仅使用固定名称：

| 文件 | 用途 |
| --- | --- |
| sender.lock | 长期保留的操作系统锁文件；Close 或进程终止释放锁，不删除文件 |
| sender.journal | 追加式完整快照日志；创建时写格式头并 Sync |

先打开 FileStore，再将它独占借给一个 OpenSender。停止旧 Sender 的全部调用后关闭 FileStore，才能交给新 owner。FileStore 自有 root、锁和日志句柄；Sender 不关闭借入存储。旧 FileStore 关闭后，其迟到调用无法写入新 owner 的日志。

```go
store, err := clientsim.OpenFileStore(ctx, directory)
if err != nil {
    return err
}
defer store.Close() // 调用方还应处理返回错误。
sender, err := clientsim.OpenSender(ctx, authenticatedUser, store)
```

Windows 使用现有固定版本 x/sys 的 LockFileEx，独占、立即失败；同进程第二次打开也竞争同一锁。操作系统在句柄关闭或进程终止后释放锁，极端情况下释放有延迟，因此 Open 不按 PID 删除“过期锁”，只返回可识别的 ErrStoreBusy。[Microsoft LockFileEx](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex)

Darwin、DragonFly、FreeBSD、Linux、NetBSD 和 OpenBSD 实现使用非阻塞 flock；本轮只完成 Linux/amd64 交叉编译，没有实际 Unix 运行验收。其他平台明确返回 ErrStoreUnsupported。目录由调用者专用，活动期间不能由外部替换、截断或编辑这些文件；固定文件名通过 os.Root 打开，现有非普通文件/符号链接拒绝。没有使用重命名覆盖或遗留 PID 文件作为互斥依据。

## 提交与恢复格式

日志以 `ICHAT-SENDLOG-1\n` 开头，每条记录封装整个 version=1 发送快照：

| 部分 | 字段 |
| --- | --- |
| 48 字节头 | SNP1、uint32 大端长度、uint64 大端连续编号、前 16 字节的 SHA-256 |
| payload | 原始快照字节，最多 16 MiB |
| 40 字节尾 | 头及 payload 的 SHA-256、COMMIT01 标记 |

Save 检查 context、容量和当前文件长度，按最多 32 KiB 的块写入下一条记录，完整写完后调用 File.Sync，只有 Sync 成功才更新内存快照并返回 nil。固定 Go 1.27.1 Windows 源码的 Sync/Fsync 路径最终调用 FlushFileBuffers；普通 Write 成功本身不能代替该刷新。[Microsoft FlushFileBuffers](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers)

一旦开始写入，短写、写后错误、取消、Sync 错误或 panic 都可能留下已提交/未提交的结果：当前 FileStore 和 Sender 停止继续操作，关闭后重新加载。已知 Sync 成功不因随后 context 取消而抹除；真正发送 HTTP 前仍应检查取消。

重启逐条扫描，检查头校验、长度、编号、完整 payload 校验和提交标记，保留最新完整记录。头也有独立校验，避免损坏长度将一条完整记录伪装为未完成尾部。检查到合法前缀的末尾未完成记录时，仅截断该尾部并 Sync，成功后才返回可用存储。已完成记录损坏、错误头/编号/标记、空文件、未知格式或超限文件整份拒绝，不回退到更早记录。

文件层验证封装，OpenSender 继续验证快照中的用户、原请求、身份/hash、状态与完整消息；即使最新记录校验正确但快照语义损坏，也不回退到旧业务状态。恢复 inFlight、保留原键/正文及终态的规则沿用 P6.3.1。

每次 Load/Save 还核对当前文件长度，检测到外部长度变化时停止使用。Load 返回独立字节副本。Close 等同步读写实际返回，先关闭日志，再关闭锁和 root；重复 Close 保留原错误，不因观察者退出或错误而默默丢弃结果。文件错误默认格式化隐藏路径/底层诊断，仍可 errors.Is 检查原因。

## 容量和验证边界

日志累计上限 256 MiB，单快照 16 MiB，核心最多 512 条逻辑记录。旧完整快照继续保留，本项没有自动压缩、归档或删除；达到任何上限明确失败，保留已有内容，不能借此生成新业务键。读取按记录及 32 KiB 块处理，并在 I/O 之间检查 context；同步系统调用本身不由后台 goroutine 提前返回。

Windows 本地目录的真实子进程测试覆盖强杀后的锁释放与跨进程读取。它验证进程崩溃恢复，未模拟机器断电、存储控制器故障、网络文件系统或实际前端；本项也没有接入真实 HTTP/MQ/SQL。

```powershell
go test -mod=readonly -p=2 -race ./tests/clientsim -count=30 -shuffle=on -timeout=120s
go test -mod=readonly -p=2 -race ./tests/clientsim -run '^TestFileStoreCrossProcess' -count=1 -v -timeout=90s
go test -mod=readonly -p=2 -race ./... -count=1 -timeout=120s
go vet -mod=readonly -p=2 -tags=integration ./...
go test -mod=readonly -p=2 -tags=integration ./... -run '^$'
```

每轮真实子进程验收为一个竞争锁场景加八个强杀点：queued、submitting、accepted、persisted、rejected、半写、Sync 前、Sync 后但尚未返回。核对原请求/稳定 ID/hash/计数、终态 DTO 和原键重试；已知终态禁止重发。半写仅回收候选尾部，写完或 Sync 后未返回的结果按未知提交恢复。

磁盘故障测试用真实文件加受控 I/O 包装，区分写前失败、实际半写、完整写后报错、实际 Sync 后报错及 panic；另外覆盖坏完整记录/旧记录、坏最新业务快照、恢复 read/truncate/Sync 错误、取消、用户隔离、Close 等待及关闭错误保持。包装注入不等同真实硬盘故障。所有测试只使用 t.TempDir，子进程强杀后均 Wait 加入，目录由测试框架清理。运行证据见[实施进度](message-reliability-mq-progress.md)。
