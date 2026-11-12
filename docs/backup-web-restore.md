# Web 恢复验证与维护安装交接

管理端已接通成功上传记录选副本，或从 [云端认证索引独立发现](backup-discovery.md#管理端独立发现与恢复) 后选择条目 → 管理员重新认证 → 后台下载 → age 全流认证解密 → 数据库格式与在线清单验证 → 私有隔离暂存。独立发现不要求本实例存在上传历史或本地密文目录。SQLite 会检查数据库完整性；PostgreSQL/MySQL 暂存检查 typed dump，空库导入时再核对数据库 schema、版本和全部外键。**下载和验证阶段不会停止服务或覆盖生产目录**；正式切换须另行明确提交给维护窗口安装器或预批准的主机 helper。原生 Web 任务的 CLI 导入和安装交接均不需要再次输入恢复私钥。

这还不是整个 Issue #34 完成：真实 HTTP API 从 MinIO/WebDAV 到主机 helper、all-in-one SQLite 邮件覆盖与回滚已在隔离环境通过，但 Google/商业云、完整实例数量、stack/原生数据库 Docker 矩阵及浏览器流程仍缺验收。[原生数据库 CLI](backup-native-databases.md) 已接通逻辑快照/加密/独立空库导入及受控安装；Web 到固定批准计划的 [主机 helper 自动交接](backup-maintenance-host.md) 现已实验性接通，上传的跨进程继续见 [续传说明](backup-resume.md)。

操作者可将已验证任务配置给 [维护协调 CLI](backup-maintenance.md)，由显式、固定摘要的部署计划控制本仓库 Compose 服务。默认或未确认隔离时仍只允许可证为空的 Postfix 队列；显式配置 `preservation.quarantineQueues: true` 后，须先通过 [维护保全](backup-preservation.md) 的完整加密验证，才能隔离 pending 邮件后替换容器，不自动投递或回放这些邮件。没有 Web API 直接控制 Docker 的入口；协调器代码/故障测试不等于真实服务恢复验收。

也可在成功且 ready 的恢复任务中，经当前密码/TOTP 再认证选择预批准主机计划，提交给固定 Unix socket helper 自动执行上述维护流程。API 只发送批准计划 ID/摘要和认证任务引用，root helper 独立验证来源、跨 UID 复制为自身私有输入，并在自己的持久记录中追踪。服务停止和会话撤销可能中断页面访问；应保留预计算请求 ID，重新登录后查询，不自动重复 POST。发生 `needsRecovery` 时须完成原生成计划的人工受控恢复，再停 helper 使用 `maintenance-host-resolve` 核验并解除隔离；不是 Web 任意命令/路径接口。具体部署、安全状态与分段验证边界见 [主机维护交接](backup-maintenance-host.md)。

维护保全是额外的主机侧计划：实际私钥由计划 `identityEnv` 指定的受保护环境提供，不沿用 Web 任务结束后已清空的内存私钥，也不传给 Docker。它不改变每日云备份的 `.env`/队列范围；保全包只允许 `preservation-verify` 或 `preservation-restore` 做校验和私有隔离恢复，不将资产或队列自动应用到服务。所有阶段的包与收据都应保留，具体预算和边界见 [维护保全](backup-preservation.md)。

## 配置与使用

在原有 `LANQIN_BACKUP_SECRET_KEY` 之外，设置 `LANQIN_BACKUP_RESTORE_DIR` 为预先存在的私有、可写目录。路径须与 live data、数据库父目录、Maildir、备份工作区和密文目录双向不重叠；Unix 应为 `0700`，Windows 另行设置私有 ACL。不要挂到静态资源、用户可读目录或公开文件服务器。

管理端打开成功/部分成功任务的详情，点击「恢复此备份」，选择尚未被清理、且配置仍匹配的地点副本。先核对发布时间、文件大小及本实例保存的可信 SHA-256；选择创建该归档时的备份密码或 age X25519 私钥，再填写当前管理员密码，开启两步认证时还须输入**未使用过**的 TOTP，并明确确认将解密凭据提交给当前邮件服务器进行此次验证。当前站点登录密码与备份密码是不同字段，改过备份密码后仍须使用旧归档当时的历史密码。

浏览器输入在提交时清空，不进入 React Query、浏览器持久存储或报告。此次恢复输入的备份密码或私钥仅暂存在当前实例的单任务内存通道，由现有受限 worker 消费，不写恢复任务数据库或环境变量；这与自动备份设置中加密保存的新归档密码是两个用途。任务结束清空可变字节缓冲区，但不宣称 Go/浏览器的不可变字符串具备可证明的内存擦除能力。正常 HTTPS 部署保证传输保护；不要在不可信明文 HTTP 上提交真实解密凭据。

所有恢复路由要求真实超级管理员会话，不对普通设置权限或开放 API Token 放行。重新认证复用密码/TOTP 校验、TOTP 防重放及账户/IP 限流。HTTP 只完成校验和原子入队/占租约，不在请求里下载、解密或复制文件。

## 任务与验证

- 恢复与快照/上传/保留清理共用跨进程租约。为了不持久化私钥，恢复需要立即获得空闲队列；存在活动或待执行任务时返回 `409 backup_restore_busy`，由管理员稍后重新提交。
- 请求只接受原任务 ID、地点 ID、可信摘要、地点 revision、幂等 nonce、认证及所选模式的备份密码或私钥；从独立发现恢复时还提交 `discoveryEntryId`，原对象、字节数及 Drive 身份由认证分页解析。不接受 endpoint、对象名、下载路径、生产路径或 shell 命令。source、所选页、地点和 lease 在同一事务 CAS 校验。
- 下载限制为收据中的准确大小，最大 1 GiB。独立目录内真实下载后，再通过 `backup.Restore` 对**实际读入的同一密文**校验全流 age 认证、可信摘要、路径/链接/条目/解压体积。SQLite 另检查数据库完整性，原生归档另检查受限行格式及完整 footer。原始云错误正文不会进入界面或报告。
- `online-data` 调用 SQLite 安装器检查完整采集清单、各 payload 大小/摘要、数据库引用、受支持 schema/触发器及运行态暂停规则。`online-native-data` 通过版本化原生格式检查清单、native dump 和每个 payload，driver 必须与归档一致；新库导入、路径映射及发送/会话暂停在后续原生安装阶段完成。
- 错误密钥、篡改、取消、过期租约或失败不会发布 ready。正常失败只清理本次实际创建且身份匹配的目录；清理失败给出独立错误，不能冒充已清理。崩溃可能留下私有残留目录，需要操作者核对，不自动猜测归属清理。
- 单任务最多一小时、下载最多二十分钟，解压最多 1 GiB/100,000 项。恢复根达到 8 个条目后拒绝新恢复，避免无界保留明文；它不是所有磁盘空间的预留保证。峰值需考虑密文、原暂存和验证副本，约为单份数据的数倍；保留的成功暂存需在维护安装/演练后由操作者明确核对处置。
- 重启后原有租约恢复把任务标记中断，**不自动重放私钥操作**；相同 nonce 只返回原任务，重新执行须新任务和重新认证/提交私钥。任务结果包含安全的 manifest/来源/ready 信息，不返回服务器绝对路径、工作目录名或私钥。

恢复数据仍含旧会话/发送状态，但在线暂存有启动阻止标记，不能直接作为 live 数据挂载。正式安装器会在自己的独立副本里再次验证、重映射并暂停运行态；Web 验证不是激活许可。

## 维护窗口安装交接

SQLite 实例使用 `install`，PostgreSQL/MySQL 实例使用 `install-native`。两者均需操作者先停止全部写入者，并显式提供原任务数据库、原恢复根和 Web 恢复任务 ID；在受保护的环境中提供原 `LANQIN_BACKUP_SECRET_KEY`，不把密钥放在命令行。原生流程另需要**不同的**新目标数据库 DSN；不得把 Web 状态库 DSN 当成新目标。其他 target/runtime/UID/GID/journal 参数仍遵守 [恢复指南](backup-restore.md) 和 [原生恢复指南](backup-native-databases.md)，不能从浏览器输入推断。

```sh
./lanqin-backup install \
  --web-job bkj_0123456789abcdef0123456789abcdef \
  --web-state-db /srv/lanqin/data/lanqin.db \
  --web-restore-root /srv/lanqin/backups/restore \
  --web-recorded-root /backups/restore \
  --workspace /private/install-work \
  --runtime-data-root /data --runtime-mail-root /var/mail/vhosts \
  --data-target /srv/lanqin/data \
  --mail-target /srv/lanqin/mail \
  --dkim-target /srv/lanqin/dkim \
  --journal-dir /private/restore-journals \
  --data-uid "$DATA_UID" --data-gid "$DATA_GID" \
  --mail-uid "$MAIL_UID" --mail-gid "$MAIL_GID" \
  --dkim-uid "$DKIM_UID" --dkim-gid "$DKIM_GID" \
  --confirm-maintenance
```

`--web-job`、`--web-state-db`、`--web-restore-root` 必须一起使用，与 `--staged` 互斥。CLI 使用独立只读 SQLite 连接，不调用 `App.New`、迁移或业务 worker。它检查加密任务记录、成功/ready、原 root、受控子目录、在线清单的原认证摘要；随后安装器在**复制后的实际清单字节**上再次核对同一摘要，防止交接到复制间的修改。清单认证不能省略各 payload 的重新校验。

以上从宿主机维护环境运行：`--web-recorded-root /backups/restore` 是原容器任务记录中的 Linux 绝对根，只用于与已认证任务比较；`--web-restore-root /srv/lanqin/backups/restore` 是当前执行环境中同一恢复挂载的私有实际路径。映射由部署操作员显式提供，不来自浏览器；仍校验原任务目录名、报告摘要和实际复制后的 payload。省略 `--web-recorded-root` 时沿用原规则，实际根与记录根必须精确一致。不要为了映射改写任务记录或移动未知目录。

后续仍是原来的 prepare → 保存旧目录 → durable journal → 受控逐目录切换 → 失败回滚；工具不自动启动服务。回滚继续使用原 live targets 和保存的 journal。旧 `--staged` 手动模式保持兼容；它依赖操作者独立可信的 CLI 解密验证，不用 Web 的数据库记录冒充认证。

原生 Web 任务先导入**预先创建的独立空库**。`BACKUP_ORIGINAL_STATE_DSN` 指向原实例任务数据库，`BACKUP_NEW_DATABASE_DSN` 指向新空库；在受保护环境提供这两个值及原 `LANQIN_BACKUP_SECRET_KEY`，不在命令行写 DSN 或密钥。下面的 `import-native` 不需要 `--in`、`--identity-file` 或 `--expected-sha256`：

```sh
./lanqin-backup import-native \
  --driver postgres \
  --target-dsn-env BACKUP_NEW_DATABASE_DSN \
  --web-job bkj_0123456789abcdef0123456789abcdef \
  --web-state-dsn-env BACKUP_ORIGINAL_STATE_DSN \
  --web-restore-root /srv/lanqin/backups/restore \
  --web-recorded-root /backups/restore \
  --workspace /private/import-work \
  --confirm-empty-target
```

导入器从加密任务记录取得可信清单/密文收据，复制到独立私有工作区并重新核对实际文件后，才连接新空库建表、参数化导入并写未激活 guard。原 Web 暂存保留；本次复制的临时明文在结束时清理。非空目标拒绝，导入或清理报错后先核对目标状态，不直接重复导入。成功 receipt 的 `activated` 仍为 `false`。

然后在维护窗口使用**同一 Web 任务与同一个新目标库**安装文件：

```sh
./lanqin-backup install-native \
  --driver postgres \
  --target-dsn-env BACKUP_NEW_DATABASE_DSN \
  --web-job bkj_0123456789abcdef0123456789abcdef \
  --web-state-dsn-env BACKUP_ORIGINAL_STATE_DSN \
  --web-restore-root /srv/lanqin/backups/restore \
  --web-recorded-root /backups/restore \
  --workspace /private/install-work \
  --runtime-data-root /data --runtime-mail-root /var/mail/vhosts \
  --data-target /srv/lanqin/data --mail-target /srv/lanqin/mail \
  --dkim-target /srv/lanqin/dkim \
  --journal-dir /private/restore-journals \
  --data-uid "$API_UID" --data-gid "$API_GID" \
  --mail-uid "$MAIL_UID" --mail-gid "$MAIL_GID" \
  --dkim-uid "$DKIM_UID" --dkim-gid "$DKIM_GID" \
  --confirm-maintenance
```

`--web-job`、`--web-state-dsn-env`、`--web-restore-root` 必须一起使用。`import-native` 的 Web 参数与 `--in/--identity-file/--expected-sha256` 互斥；`install-native` 的 Web 参数与 `--staged/--expected-report-sha256` 互斥。容器到宿主机可显式提供上述可选 `--web-recorded-root` 映射。两种命令均从原始同引擎 Web 状态数据库读取已完成任务，仅接受 scope/driver、任务状态、记录根和认证报告摘要全部匹配的任务。报告摘要随后由导入器或安装器在实际私有根的独立副本上重新验证；Web 原始暂存文件不会直接安装或跳过 payload/dump 检查。状态库和新目标库的环境变量名称必须不同，实际 DSN 值不能相同，且操作员必须确保它们确实指向不同数据库；换用户名、参数或别名仍指向同一个库不构成隔离。

激活后由操作员将部署 DSN 明确切至新库；工具不更改部署连接配置。回滚原生恢复必须使用 `rollback-native`，同时传入该**新库**的 DSN、原三个 targets、保存的 journal 和维护确认；回滚先阻止新库启动再恢复旧目录。核对后由操作员切回旧 DSN，不能仅回滚文件就启动仍指向新库的服务。

## 当前验证

- 2026-11-04，第 24 轮使用真实 MinIO、WsgiDAV/Cheroot、当前生产 HTTP API、root Unix socket helper 和 all-in-one SQLite 邮件容器，完成两地点上传/完整回读/认证发现、两个实际 UTC 日程、两种存储的正确 age ready 恢复，以及错钥失败/live 不变。随后实际 API 重新认证提交正式维护，旧会话 401、旧任务 404，新登录可查持久 host 状态；恢复后 SMTP、授权附件和 TLS IMAP 内容/INTERNALDATE 通过，正式回滚恢复旧树且保留新接受邮件。没有注入网络下载或 helper 响应，也没有直接造 job；客户端是 HTTP 演练工具，不是浏览器。详细边界见 [真实存储演练](backup-real-storage-validation.md)。本轮没有生产代码变更或重跑 Go/前端检查。
- 以下为此前自动测试与界面分层证据，保留各自执行范围，不因新服务演练升级为更多浏览器/供应商已验收：
- 自动测试使用真实合成在线采集/age/SQLite/完整安装器验证；覆盖错误密钥、密文篡改、失败清理、取消、重启不重放、来源变化、并发互斥、工作区重叠和保留数量上限。下载在 app 测试中注入；三种存储的网络协议和下载另有本地 TLS 测试，不能把两者合称真实云账号端到端测试。
- HTTP 测试验证匿名/普通设置用户拒绝、管理员密码、TOTP 必须存在且防重放、错误限流。原生数据库合同已在隔离 PostgreSQL 16.15/MySQL 8.4.13 上覆盖 Web 快照、重新认证恢复、实际 age/dump/payload 验证和任务认证交接；云上传/下载为注入响应，不是供应商联调。
- 本地生产前端连接真实 API/SQLite，合成下载响应经过实际 age 解密、SQLite 和安装器校验，观察到下载 → 已验证暂存；刷新后可重新查看来源版本/范围/时间、保存安全报告。错误密码时输入清空，已打开弹窗/已有错误/下载状态下跨标签切换三种语言即时更新；实际 `innerWidth=390`、页面宽度 `390`，表单和结果无横向溢出。不是生产云服务或完整 Linux 邮件服务恢复验收。
- Web → 维护交接测试验证原 root/主密钥/已认证任务、清单篡改拒绝，以及复制后的实际清单摘要传入安装器；CLI 参数/调用顺序测试使用模拟 switch，不将 Windows 单测当成 Linux 实际目录覆盖验收。

Web 独立发现整合后 `go vet ./...` 和 `go test ./... -count=1 -timeout=10m` 全量通过（app 包 320.519 秒），两种真实隔离数据库的发现元数据、Web 空库导入与容器/宿主路径映射合同亦通过。前端完整 `pnpm --dir apps/web run check` 的 shadcn、1764 项三语言检查、10 个语言测试及 ESLint 通过，仍在原有 23 文件 Prettier 阶段中断，具体清单见 [管理端记录](backup-admin.md)；单独 TypeScript/Vite 构建及改动文件 Prettier 通过。三语言 React HTML/SSR 的合成状态断言通过，不是浏览器移动端/Portal/键盘或真实 API 交互。原始 `gofmt -l .` 列出 53 个既有 CRLF 文件，换行归一化后实际差异为零。最终范围见 [交付状态](backup-progress.md)。

独立合成 API/前端代理与临时浏览器标签已关闭，临时 Go 预览入口已移除；本轮没有提交、推送、部署、停止生产服务或发布 GitHub 评论。

Linux/amd64、`CGO_ENABLED=0` 的 API 与备份 CLI 基线交叉构建通过。原生公开 Web 导入→Linux 文件切换→激活→回滚的测试会由 CI 的 Linux runner 执行，目前没有该工作流的远端结果；交叉构建和 Windows 模拟目录测试均不替代实际 Linux 切换或完整邮件服务恢复验收。
