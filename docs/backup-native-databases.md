# PostgreSQL / MySQL 原生数据库备份阶段（实验性）

`create-online` 已增加 PostgreSQL/MySQL 的应用级一致读取、邮件引用采集和 age 加密归档；`import-native` 可将经过认证的归档导入**独立空库**，保留未激活标记。已经在隔离 PostgreSQL 16.15、MySQL 8.4.13 上实际验证数据库、合成草稿/附件、DKIM、公开 CLI 及失败回滚。

**原生数据库恢复安装现已接通，但仍属实验性，尚未完成 Linux 生产式恢复验收。** `install-native` 会验证暂存副本、准备明确指定的新数据库、映射运行时路径并暂停可重放任务；经显式维护确认后，调用 Linux 专用目录 journal 切换和数据库激活。它既支持独立 `restore` 生成的暂存目录，也支持由原实例状态 DSN、Web 任务 ID、受保护恢复根及 master key 解析的已验证 Web 任务。原状态数据库 DSN 与新库目标 DSN 必须不同。任何步骤都不会自动停止/启动服务、改部署 DSN 或删除旧目录。`activate-native`/`rollback-native` 用于中断后的明确恢复；失败时保留旧树、新树、journal 和新库 guard。API 只在 active 记录与新 data/mail runtime roots 配对时允许启动。管理端新建备份已支持 SQLite/PostgreSQL/MySQL；Web 恢复仍只下载、解密、检查和隔离暂存，不触发覆盖安装。整个 Issue #34 继续保持未完成。

## 不停服采集与版本格式

- 创建独立数据库连接，不调用 `App.New`、迁移或业务 worker，不占用 API 业务连接池。使用只读 `REPEATABLE READ` 事务；数据库行导出、后续邮件/附件引用读取、草稿重建和 DKIM 重建都使用**同一个事务快照**，避免另启 `mysqldump` 进程拿到不同读取版本。
- 校验当前应用 schema version、已知表及列信息，拒绝额外/缺失表、未知触发器。MySQL 要求 InnoDB；PostgreSQL 拒绝非持久表、分区表及 RLS 等当前未适配组合。以有界查询持有表元数据/访问共享锁，业务 DML 可继续；不支持采集期间部署 DDL/迁移。
- 长时间只读事务可能保留 PostgreSQL 旧版本/WAL 或 MySQL undo，并延迟 DDL；仍需监控容量和负载。这是逻辑引用闭包，不是数据库与 Maildir 在同一物理瞬间的原子快照，也不包含 Postfix spool、部署密钥或上游远端全部 IMAP 邮件。
- 数据库文件为 `data/lanqin-native.jsonl`，每行一个受限结构记录：版本化 schema header、有类型的行值、行计数与完整内容摘要 footer。整数使用字符串保留 64 位精度，文本 UTF-8 校验后 Base64 编码，二进制严格 Base64；时间保持精度，不执行归档提供的 SQL 或 DDL。
- 总字节、总行数、每行及单元格都有上限；默认数据库格式上限为 10 GiB、1,000,000 行、128 MiB/记录、64 MiB/单元格，仍受整次采集 `--max-bytes` 更小的限制。先按数据库行宽查询限制大文本分配。超过限制失败，不截断或静默丢行。
- `backup.queue.v1`、每日计划、任务历史、上传检查点和 OAuth 临时 state 从导出的逻辑行流中排除，源库保持不变。地点凭据及业务记录保留，外部凭据的主密钥仍需独立保存。未激活的导入库不能再次冒充可正常采集的 live 来源。
- 加密层继续使用同一 age X25519 + tar 容器。原生 manifest 为 `version=2, scope=online-native-data, driver=mysql|postgres`，采集清单为 `<driver>-reference-closure` version 2；旧 SQLite manifest version 1 保持兼容，旧读者应拒绝未知原生格式，而不是误当 SQLite 打开。

## 显式 CLI 备份

只通过**显式环境变量引用**读取数据库连接配置；不把 DSN、用户名或密码放在命令行，不自动读取部署 `.env`，不使用 `app.LoadConfig`。以下变量值由操作者在受保护的进程环境中提供。

```text
./lanqin-backup create-online --driver postgres --dsn-env BACKUP_SOURCE_DSN --data-dir /data --maildir-root /var/mail/vhosts --workspace /private/capture-work --recipient age1... --out /backups/native.age
```

MySQL 使用 `--driver mysql`，其他路径语义不变。SQLite 继续使用 `--database /data/lanqin.db`；`--database` 和 `--dsn-env` 按引擎互斥。输出仍须不存在，私有工作区须预先存在且位于源目录之外。使用原部署工作目录和真实 DataDir/MaildirRoot，不猜测数据库绝对路径的另一含义。

源账户只需读取所有应用表及必要 schema 元数据；备份不授予任意服务管理或容器权限。MySQL 使用独立 UTC 会话，避免连接端 `parseTime` 设置改变导出表示；目标导入使用严格 SQL mode，拒绝静默截断或非法日期。连接 DSN 本身应配置该部署所需 TLS，命令不新增跳过证书验证的开关。

完成时输出安全统计及可信 `sha256=` 收据；原生数据库明文与草稿/附件暂存清理成功后才发布最终密文。归档可通过已有 S3/WebDAV/Drive 上传器传输，云端对象命名和密文收据不变。

## 隔离空库导入（不激活）

提前创建**与旧生产库不同**的、空的数据库和专用目标账户。目标账户需在该新库内创建应用 schema、写入数据、管理 FK/约束；不要复用指向旧生产库的 DSN。显式设置 `BACKUP_EMPTY_TARGET_DSN` 后执行：

```text
./lanqin-backup import-native --driver postgres --target-dsn-env BACKUP_EMPTY_TARGET_DSN --in /backups/native.age --identity-file /offline/recovery-key.txt --expected-sha256 <independently-trusted-64-hex> --workspace /private/import-work --confirm-empty-target
```

导入步骤：

1. 完整解密/age 认证，同流校验可信密文摘要、归档路径/链接/数量/体积、原生行格式及 footer。错密钥、损坏密文或格式不符时不连接目标写入。
2. 检查之前认证的采集清单及所有实际文件摘要、数量、大小、身份，拒绝额外、缺失或被修改的 payload。把清单中的数据库 SHA-256 继续绑定到实际导入读取，不能只凭一个 `READY` 文件名信任暂存。
3. 连接明确的新目标库，校验同引擎/同版本系列（PostgreSQL major，MySQL major.minor）并获取原有 schema advisory lock。必须没有已有基础表；**不会清空旧表、删库或覆盖数据**。
4. 只用应用代码中的已知 schema 建表，然后核对归档列名、类型、可空性、生成列、长度、精度、collation 和时间精度。归档不提供可执行建表语句。
5. 在单个可回滚的数据事务中，通过绑定参数导入所有非生成列。PostgreSQL 在事务内暂时移除由**目标应用新建 schema**取得的 FK，验证后原样重建；MySQL 仅在该专属连接暂时关闭 FK 检查，并逐一检查全部父子引用后才提交，再恢复检查。不全局修改数据库设置。
6. 第二次读取仍要求相同完整摘要与 EOF；所有引用、唯一性及迁移版本均有效后，在同一事务写入 `backup.native.restore.pending.v1`，再提交。任一行/引用/摘要失败回滚，不留下部分导入邮件。
7. 输出 `{driver,rows,sourceSHA256,reportSHA256,activated:false}`。清理本次实际创建、身份匹配的私有明文目录；清理失败明确报错，并提示核对目标后再重试。原加密文件和旧生产库保留。

对于已经由 Web 下载、认证解密并成功隔离暂存的原生任务，可直接用原实例任务记录导入，不重复提供恢复私钥：

```text
./lanqin-backup import-native --driver postgres --target-dsn-env BACKUP_NEW_DATABASE_DSN --web-job bkj_0123456789abcdef0123456789abcdef --web-state-dsn-env BACKUP_ORIGINAL_STATE_DSN --web-restore-root /srv/lanqin/backups/restore --web-recorded-root /backups/restore --workspace /private/import-work --confirm-empty-target
```

`BACKUP_ORIGINAL_STATE_DSN` 是原实例同引擎任务数据库，`BACKUP_NEW_DATABASE_DSN` 是独立新空库；同时在受保护环境提供原 `LANQIN_BACKUP_SECRET_KEY`。三个必需 Web 参数与 `--in/--identity-file/--expected-sha256` 互斥。可选 `--web-recorded-root` 指定任务记录中的原 Linux 根，`--web-restore-root` 指定操作员当前私有实际根，支持容器挂载映射到宿主机；不映射时记录根和实际根仍须相同。导入器核验加密任务、已完成/ready、scope/driver、记录根和认证报告摘要，再复制并重新校验实际 dump 与 payload 后导入；不会直接信任 `READY` 或浏览器提供的路径。原暂存保留，临时副本清理后只留下 guarded 新库。DSN 环境变量名称必须不同、连接值不能相同，并须确保两者实际指向不同数据库，不能仅更换用户或连接参数来规避隔离。

成功导入并不自动处理 live 文件路径、属主、发送任务或会话。新库含原业务状态，API 启动会在启动 worker 前检查未激活标记并拒绝启用。不要直接删除标记、挂载暂存树或手工把全部发送改回排队；必须继续执行下面的原生维护安装。

创建 schema 在 MySQL 不能与整次数据导入合为一个全局事务；失败可能留下**空的应用 schema**。该新库仍应保留供核对，不能因为报错就直接重复导入到非空库。工具不自动删除失败库，后续重试要由操作者明确准备另一个空目标。

## 原生维护安装、激活与回滚

原生安装必须由部署操作员在维护窗口执行。先停止所有 API、SMTP/IMAP 写入者及 worker，准备三个已存在、非挂载点的目标目录（data/mail/dkim），并为新数据库设置与旧生产库分离的 DSN 环境变量。以下示例在宿主机/受控 helper 的命名空间使用 `/srv/lanqin/data`、`/srv/lanqin/mail`、`/srv/lanqin/dkim`；部署容器内 runtime roots 仍是 `/data`、`/var/mail/vhosts`，本项目 DKIM 挂到 `/var/lib/rspamd/dkim`。目标服务 UID/GID 和数据库连接需在该执行环境下明确可用。独立归档流程先运行 `restore` 得到暂存目录和可信 `report-sha256` 收据，再用 `import-native` 导入空的新库。之后：

```text
./lanqin-backup install-native --driver postgres --target-dsn-env BACKUP_NEW_DATABASE_DSN --staged /private/recovery --workspace /private/install-work --runtime-data-root /data --runtime-mail-root /var/mail/vhosts --data-target /srv/lanqin/data --mail-target /srv/lanqin/mail --dkim-target /srv/lanqin/dkim --expected-report-sha256 <trusted-report-sha256> --journal-dir /private/restore-journal --data-uid <api-uid> --data-gid <api-gid> --mail-uid <mail-uid> --mail-gid <mail-gid> --dkim-uid <dkim-uid> --dkim-gid <dkim-gid> --confirm-maintenance
```

Web 任务完成上述空库导入后，可将 `--staged/--expected-report-sha256` 替换成同一组 `--web-job/--web-state-dsn-env/--web-restore-root`，其他必填安装参数不变。完整命令见 [Web 维护交接](backup-web-restore.md)。导入和安装使用同一个新目标库和同一 Web 恢复任务；目标库原导入 dump 摘要必须与这次已认证暂存一致。

镜像内的 `/usr/local/bin/lanqin-backup` 可用于 keygen、采集、fetch、空库导入等独立操作；这不授权或实现 `docker exec` 一键覆盖。Compose 容器里的 `/data`、`/var/mail/vhosts`、`/var/lib/rspamd/dkim` 通常是 bind mount，安装器不能重命名挂载点。必须在能访问实际宿主机目标目录及其父目录的受控维护环境切换。Web handoff 可由操作员用 `--web-recorded-root /backups/restore --web-restore-root /srv/lanqin/backups/restore` 显式映射记录根到实际私有根，仍绑定同一已认证任务及实际报告/payload；不能改写任务元数据绕过绑定。服务重建挂载前后均须核对 DB/文件配对。

CLI 输出的 JSON receipt 记录 journal、transaction 和数据库状态。目录切换后激活新库；操作员必须单独、明确地将部署 DSN 指向该新库，再启动服务并检查邮件读取、附件、DKIM 和受控收发。旧数据库和 `.rollback-<transaction>` 目录保留。不可把目标路径或新 DSN 指向旧生产库；工具只会连接显式指定的新库，不负责保存或切换部署配置。

若安装/激活中断，先保持所有服务停止，并按 receipt 检查 journal 与所有目录。`activate-native` 和 `rollback-native` 均只接受 driver、新库 DSN 环境变量、原三个 targets、journal 及维护确认，不接收 staged/Web 任务/UID 参数。例如：

```text
./lanqin-backup rollback-native --driver postgres --target-dsn-env BACKUP_NEW_DATABASE_DSN --data-target /srv/lanqin/data --mail-target /srv/lanqin/mail --dkim-target /srv/lanqin/dkim --journal <receipt-journal> --confirm-maintenance
```

回滚先在新库重新写入启动 guard，再恢复旧目录；失败树不会自动删除。回滚后由操作员保持/恢复旧 DSN，再启动旧服务。不得从不确定的目录或数据库状态直接启动服务。

`install-native`、`activate-native`、`rollback-native` 仅在 Linux 可用且要求 `--confirm-maintenance`。它们不会替代发行版/Compose 的协调停服、配置备份或实际服务验收。Windows 上运行的 PG/MySQL 数据库测试不证明 Linux 权限、fsync、原子不覆盖 rename 或全站服务恢复。

默认私有区峰值需要包含完整解密数据；`import-native` 清理临时树，只保留新库，文件目录安装将继续从独立可信归档验证后准备。Windows 下必须配置适当的目录 ACL，Unix mode 位不等于 Windows DACL。当前只有 Linux 备份/恢复服务目标；Windows 原生数据库往返是代码/协议验证，不等于 Linux 全站恢复验收。

## 已验证与 CI

2026-11-01 在工作区 `tmp` 的独立数据目录启动 PostgreSQL **16.15**、MySQL **8.4.13**，均只绑定本机临时端口，使用合成账户/邮件；测试后已关闭，没有安装系统服务、覆盖已有配置或访问已有用户数据库。MySQL 使用 `--no-defaults/--no-monitor`；PostgreSQL 使用显式新集群和私有测试目录 ACL。

- 两种引擎的原 `TestExternalDatabaseContract` 实际通过，包含备份地点、队列、每日计划、保留策略、恢复入队和续传检查点；此轮不再把这些契约标为“没有 DSN 未验证”。
- `TestNativeDatabaseBackupRestoreContract` 在两种引擎上执行真实只读快照、采集、age 加解密、空库导入、公开 `create-online/import-native` 子进程，验证中文草稿、真实二进制附件及 DKIM，原冻结事务期间另一连接写入能提交且冻结引用不前移。
- 覆盖错密钥不建表、非空目标拒绝、修改附件被拒绝、无效 FK 图导致整次行事务回滚、运行态上传会话不导出、原生暂存清理和启动未激活库被拒绝。格式单测还覆盖数值精度、非法 UTF-8、截断、尾随/篡改记录、恶意 identifier、资源上限及取消。
- CI 已增加独立 source/target/CLI/失败图/Web 导入数据库，并构建公开 CLI 执行往返。CI 仍采用 PostgreSQL 16 / MySQL 8.4 服务；**本地没有 Docker，因此未运行 Actions 容器编排本身**，不能声称远端 CI 已绿。新增 Web 空库导入与维护安装交接本轮验证结果见 [交付状态](backup-progress.md)。

手动测试需分别设置 `LANQIN_TEST_NATIVE_POSTGRES_{SOURCE,TARGET,CLI,BAD,WEB}_DSN` 或 `LANQIN_TEST_NATIVE_MYSQL_{SOURCE,TARGET,CLI,BAD,WEB}_DSN`，并将 `LANQIN_TEST_BACKUP_CLI` 指向本轮构建的真实 CLI。SOURCE 必须是专属空测试库，TARGET/CLI/BAD/WEB 各为独立空库；不要将测试 DSN 指向已有业务库。无 DSN 时会跳过对应引擎或 Web 子进程测试，不能把跳过算作该路径验证。

```text
go test ./internal/app -run '^TestExternalDatabaseContract$|^TestNativeDatabaseBackupRestoreContract$' -count=1 -timeout=5m -v
```

剩余工作：自动化部署协调/受控激活仍未接入 Web；需要在 Linux CI/隔离主机执行公开 native maintenance CLI 子进程测试，并完成真实邮件服务与至少一个真实云供应商的恢复演练。Web restore 当前仅隔离暂存，不是全覆盖切换。本阶段没有提交、推送、部署或向 GitHub 发布进度消息。

本轮整合验证：`go vet ./...`、全量 `go test ./... -count=1 -timeout=10m` 通过（app 包 354.800 秒）。PostgreSQL/MySQL 的真实合同、公开归档/Web `import-native` 子进程、认证任务和容器到宿主路径映射均通过；激活核心在 Windows 上使用真实数据库及模拟发布目录验证。Linux 公开维护 CLI E2E 需要由 Linux runner 执行，尚无远端结果，不能将本地合同通过当成该 Linux 生命周期已运行。

Linux/amd64、`CGO_ENABLED=0` 的 API/备份 CLI 交叉构建通过。`gofmt` 检查报告 53 个既有 CRLF 文件，换行归一化后无差异，`git diff --check` 通过。前端 shadcn、i18n、lint、改动文件 Prettier 和 Vite 构建通过；完整 `pnpm --dir apps/web run check` 仍在既有 23 个未修改文件的全仓 Prettier 阶段中断。
