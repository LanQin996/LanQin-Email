# 通用不停服备份路线（Issue #34）

## 约束与结论

用户要求整个备份过程不停服，且不能依赖特定云平台、云盘 API、LVM、Btrfs 或 ZFS。普通 ext4 上也必须能工作。此前将存储级原子快照视为前提不适合这个需求，已撤销该设计前提。

采用应用级在线采集：SQLite 一致导出 + 邮件/附件引用闭包 + 完整性验证 + 本地公钥加密 + 多地点异步上传。它不是整个磁盘同一物理时刻的快照，也不保证备份期间新增的数据全部包含。整个过程不停止 API、SMTP、IMAP/POP3 或后台任务，不主动冻结文件系统，不请求容器控制权限。

## 已接通的在线采集链路

新增独立 `create-online` CLI，调用 `app.PrepareOnlineBackup`，不调用 `app.New` 或启动后台服务，也不读取未显式指定的部署环境/主机秘密。只支持 SQLite；外部数据库直接拒绝，不尝试连接 DSN。

`apps/api/internal/backup/online.SnapshotSQLite` 使用现代 SQLite 驱动的 Online Backup API：

1. 单独创建 `mode=ro` 连接，不借用应用数据库池；要求源已经是 WAL（本应用的 SQLite 初始化已如此配置），不在备份时切换源日志模式。
2. 在独立连接上固定 WAL 只读事务，每步最多复制 64 页，步间让出时间；业务及租约心跳写入不会反复重启复制。BUSY/LOCKED 有界等待，最多五分钟或调用者更短的 deadline。不暂停写入，但只读快照持有期间可能延迟 checkpoint 截断并增加 WAL 空间，需监控容量。
3. 写入私有临时目录；只将新副本转为独立 DELETE 日志模式，关闭并 `quick_check`，检查大小、同步文件后以非覆盖方式发布。
4. 失败、取消、超限均不发布最终数据库；不直接复制正在运行的 `.db/-wal/-shm` 文件，不把 `immutable=1` 用在活库上。

合成 SQLite 测试占住业务池唯一连接，并在导出步骤间执行三个真实事务；验证写入能提交、副本没有撕裂事务、源仍为 WAL、副本不依赖旁文件。还覆盖取消、复制中取消、大小上限、并发目标占位及拒绝非 WAL 源。

同包的 `CollectDKIM` 已实现从该关闭副本重建活动域名的 DKIM 文件：默认 selector 与部署脚本一致，校验路径、公私钥匹配及 RSA 私钥有效性，私有暂存、独占创建，失败清理本轮自建文件，不覆盖既有键。合成测试验证源库随后轮换密钥不影响导出副本，并覆盖路径穿越、重复名称、错公钥、损坏私钥、数量上限及拒绝活跃 WAL 源。它不会访问或改写运行中的 rspamd 目录。

这些原语已接入 CLI 及管理端新建在线备份任务，另保留上传既有归档入口。管理端需要显式配置公钥和私有工作区，详见 [管理端配置](backup-admin.md)。归档可解密暂存不等于生产覆盖恢复已完成。

## 文件协调器的边界与实现

必须先解决本仓库真实写入顺序，不能简单递归复制三个目录就称为一致备份：

- `maildir_write.go` 先落盘、原子重命名，再更新 `messages.raw_path`；重写邮件会删除旧路径。已读/星标也会移动文件。冻结的数据库可能暂时引用刚被移走或删掉的路径。
- 草稿更新先修改 `messages` 的主题/正文及附件元数据，再调用 `rewriteMessageMaildir`。即使旧 `raw_path` 仍存在，旧 MIME 内容也可能与冻结数据库不同；不能仅用文件存在、大小或“同一个 Message-ID”作为内容一致的证据。协调器必须对草稿按冻结记录及附件重建或做等价内容校验，并覆盖写入中间状态。
- 邮件删除先删除 MIME 和附件，再删除数据库记录。发现缺失引用不能直接忽略，也不能为了通过检查从副本删掉邮件记录。
- `maildir_sync.go` 定期扫描 `new/cur`，刚投递、尚未建索引的邮件以及未注册收件人的邮件也必须有明确捕获规则；只遍历 `messages` 会漏信。
- DKIM 同步脚本从 `domains` 的私钥字段派生 `<domain>.<selector>.key`。优先从冻结数据库重建当前业务所需键，而不是把异步更新中的 DKIM 目录误当成同一版本。域名、selector、密钥编码必须校验，不允许数据库内容变成任意写入路径。

现有协调器实现如下：

- 只修改已生成的 SQLite 私有副本，移除 `backup.queue.v1` 及 `backup.job.v1.*` 备份运行态/历史记录，并在清单中统计遗漏数量；地点凭据和其他业务数据保留。源库不受影响。该操作防止恢复副本后重放备份任务，不代表清除了邮件发送队列，不是 SQLite 自由页的安全擦除。

- 固定 SQLite 副本后读取全部附件引用，要求路径位于显式 DataDir 的 `attachments/<messageID>/<attachmentID>_...`，确认父邮件存在、字节数匹配，再复制至私有暂存区。
- 有界枚举 `domain/local/Maildir/new|cur` 及 Maildir++ 子文件夹，包含 catch-all；不扫描 `tmp`，不采集 Dovecot 索引。按同一邮箱内去掉 flags 的文件身份定位移动，不按 Message-ID、文件大小或跨用户猜测。多个候选拒绝。
- 清单捕获句柄级文件身份；复制同时核对扫描身份、前后 inode/FileID、大小、mtime 和二次读取 SHA-256，拒绝链接、特殊文件、越界路径及已有输出。普通已提交邮件业务内容不可编辑，移动/标志变化仅重命名；稳定投递任务的等价 MIME 重试不是跨文件物理快照保证。
- 草稿从冻结数据库及已采集附件复用 `BuildMIME` 重建，避开旧 MIME 滞后窗口。原始数据库不改写；重建位置和摘要记录在加密清单中。共享 MIME 生成器补充了合法引号/反斜杠文件名的 quoted-pair 转义，未改变既有路径 basename 安全规则。
- 未被冻结数据库引用的 MIME 作为候选保存在 `data/backup-unindexed/<hash>.eml`；清单保留来源映射。它可能是新投递，也可能处于删除窗口，恢复时必须显式核对，**不会自动导入活跃 Maildir**。
- 外部邮箱已有本地 MIME 按普通原文保全；无原始 MIME 的镜像记录仅保留数据库和已落盘附件缓存，统计 `external_cached_only`，不声称包含上游完整邮件，不请求上游服务。本地非草稿缺少原文引用则拒绝，让正常 backfill 完成后重试，不伪造原始邮件。
- Maildir mtime 是 IMAP INTERNALDATE，不是写入发生时间，不拿它与数据库快照开始时间排序；合法未来日期可以备份。原日期记录在加密恢复清单中；生产恢复阶段仍需验证映射并恢复相应 IMAP 元数据。[Dovecot Maildir 文档](https://doc.dovecot.org/2.4.5/core/config/mailbox_formats/maildir.html)

缺失引用或采集期间变化会丢弃本轮私有暂存区，从 SQLite 重新开始，最多三次；歧义、非法路径、密钥错误和资源超限不盲目重试。高频变更不能收敛时明确失败，不暂停投递、不发布残缺归档。总任务最多三十分钟，SQLite 子步骤最多五分钟；复制、哈希和网络不借用业务连接池。并发安全检查不是对任意外部进程恶意改写已提交文件的跨文件原子保证。

## CLI 演练

先用合成数据验证，以下为 Linux 命令示例，路径应替换为显式授权的源根和私有工作区。工作区须预先存在、位于 data/mail 源目录之外，Unix 权限仅属主可访问，例如 `0700`；Windows 必须另行配置私有 ACL。

```text
go -C apps/api build -o ../../tmp/lanqin-backup ./cmd/backup
./tmp/lanqin-backup create-online --database /data/lanqin.db --data-dir /data --maildir-root /var/mail/vhosts --workspace /private-backup-work --recipient age1... --out /backups/sample-online.age
```

`--recipient` 是 age 公钥，不是恢复私钥。源必须已使用 WAL，命令不会改变源日志模式或冻结文件系统。数据库中存储的相对路径按调用进程工作目录解释；应使用原部署的工作目录及配置根路径，不能凭猜测重定向相对引用。显式根目录的符号链接别名可以使用，根内的子链接仍拒绝。

默认总数据 10 GiB、全部文件/目录条目 100000、单个重建草稿 MIME 64 MiB、最多三轮，可用 `--max-bytes`、`--max-entries`、`--max-mime-bytes`、`--attempts` 调整（有硬上限）。文件及报告均计入预算。加密前后的临时空间应留足；信号退出不一定覆盖强制终止/断电，管理员须管理私有工作区遗留目录。

命令仅输出安全计数、范围提示及 `sha256=...` 收据，不输出源路径、邮件 ID、地址或密钥。私有明文清理成功后才非覆盖发布最终密文；采集/加密/清理失败不发布。最终标准输出写入失败可能发生在发布之后，不应以非零退出码就盲目删除已有文件。不要将密文输出或发布目录放入源目录。

使用现有 `publish` 命令将该密文和可信收据发布到认证目录后，可由管理端异步上传。也可先用原 `restore` 命令和离线恢复私钥，在全新目录解密演练。在线归档的 `scope=online-data`；恢复器在 READY 前检查版本化完成清单，未索引候选仍留在隔离区。完成清单不等于来源签名或恢复器重新证明业务一致性，独立可信收据仍必需。

恢复还需将原部署绝对路径安全映射到新根目录、恢复 Maildir 所有权、隔离待发送任务及提供回滚。数据库导出成功、归档上传成功或出现 READY 都不能替代隔离恢复演练。Postfix 队列、宿主机部署秘密和外部 IMAP 内容仍不在现有归档范围内。

参考：[SQLite Online Backup API](https://www.sqlite.org/backup.html)、[SQLite WAL](https://www.sqlite.org/wal.html)。

## CLI 阶段验证（2026-10-30）

- 使用真实 WAL SQLite 合成数据，业务池唯一连接保持占用，独立协调器仍完成采集；冻结后写入新草稿、移动原文、轮换源 DKIM，实际加密/解密暂存验证冻结正文、附件、原始 MIME、catch-all、DKIM 与逐文件摘要一致。
- 清单扫描后替换同大小文件并还原 mtime，FileID/inode 绑定仍拒绝第一轮；重新从 SQLite 开始第二轮后成功。附件并发删除同样重新采集，不把缺失记录悄悄从旧快照移除。
- 覆盖永久缺失、跨邮箱路径、根外附件、链接、歧义、条目/字节/MIME 上限、取消、显式根别名、未来 INTERNALDATE、重复 Close 和私有目录被替换时拒绝误删。Windows 实测链接和扫描后替换；仅“打开源句柄后重命名”场景因 Windows 共享删除限制跳过，不冒充 Unix 运行验证。
- 实际构建 CLI 子进程执行 keygen/create-online/restore/publish/错密钥恢复；SQLite 源保持 WAL，采集期间成功提交 45 次写事务；原文精确一致、候选隔离、错误密钥无 READY、私有工作区清空。重放脚本为忽略的 `tmp/online-cli-subprocess-smoke.py`，全部为合成数据，不使用线上邮件或云凭据。
- Linux/amd64 CLI 交叉编译通过，**不是 Linux 恢复运行验证**。PostgreSQL/MySQL 契约检查因未配置测试 DSN 跳过；在线采集仍明确只支持 SQLite。
- `go vet ./...`、`go test ./... -count=1 -timeout=10m` 全部通过（app 包约 237 秒）；完整输出保存在忽略的 `tmp/issue34-api-full-test.log`。原始 `gofmt -l .` 列出 54 个既有 CRLF 文件，LF 规范化后没有实际格式差异，未格式化无关文件。`git diff --check` 通过。本轮未修改前端，未重新运行前端检查。
- 本轮没有更改前端或部署配置，没有停止线上服务，没有提交、推送或发布标签；Issue #34 仍未完成全部范围。
