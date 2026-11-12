# 受控部署维护协调器（实验性）

`lanqin-backup maintain` 在部署者明确提供、预先审查的计划上，协调停止本仓库 Compose 应用服务、确认停止、安装已验证恢复、离线检查、持久提交及服务重建。它在宿主机或有相应项目权限的受控维护环境中运行；Web API 不持有 Docker socket、任意命令执行或宿主机路径权限。

当前 CLI 可读取独立 `restore` 的 `online-data/online-native-data` 暂存，或现有已验证 Web 恢复任务；旧 `offline-data` 只支持原有隔离验证，不冒充带完整引用映射的自动安装输入。不需要把恢复私钥交给 Docker，也不从浏览器接收服务名、命令或目标路径。实验性的 [主机 helper 交接](backup-maintenance-host.md) 已接通：Web 重新认证后只提交已完成恢复任务与预批准计划，由固定 Unix socket 对端执行有限维护流程。协调器、模拟控制器、root socket 和复制的分层测试不是实际 Docker/邮件服务完整恢复验收，整个 Issue #34 继续保持打开。

## 支持边界

- 只支持明确的 `all-in-one`（`lanqin-email`）或 `stack`（`api/postfix/dovecot/rspamd/web/nginx`）服务集合。MySQL/PostgreSQL 容器保留运行，不删除数据库、容器卷或旧数据树。
- 只支持本仓库标准容器目录 `/data`、`/var/mail/vhosts`、`/var/lib/rspamd/dkim`。宿主机 targets 是对应的**真实绑定源目录**，不是容器中的 bind mount 挂载点；其他布局使用原手动安装流程，不能假设已经适配。
- 所有项目容器的 project/service/working-directory 标签、挂载、运行态和环境须可验证；额外未知服务、影子挂载、未识别状态或配置修改均拒绝。操作员仍须保证没有 Compose 项目之外的写入者访问这些目标。
- **默认仍要求 Postfix 队列可证为空。** 默认部署的 spool 没有持久化，强制重建会移除容器内队列。省略 `preservation`，或其 `quarantineQueues` 为 `false` 时，非空队列仍拒绝安装。显式 `quarantineQueues: true` 才允许在全部写入者停止、队列和部署资产被加密保全且完整验证后，隔离 pending 邮件再重建；不会自动投递或重放这些邮件。未知结构、链接、截断、变化或预算超限始终拒绝，详见 [维护保全](backup-preservation.md)。
- 本机 Docker default context，绝对 Docker CLI 路径；不继承隐藏的 Docker/Compose host/context 覆盖，不自动拉镜像或隐式构建。计划应用镜像须在本地且 ID 保持不变。原生数据库验证的 helper 服务有 `build:` 时明确拒绝自动路径，需先采用已构建的不可变本地镜像配置。
- 变更只在 Linux 启用；Windows 可审查计划和运行明确标注的状态机/文件模拟测试，不能用其证明 Linux ownership、fsync、进程组、原子不覆盖 rename 或 Docker 部署已验证。

## 准备计划

参考 [配置模板](../deploy/maintenance-plan.example.json)。它故意使用无效 UID/GID 与占位摘要，不能原样执行。六项服务身份必须全部显式填入实际数字；不要猜测 DKIM 用户或套用其他镜像的 UID。

在当前维护执行用户所有的私有目录中准备独立 state/workspace，均位于 live targets 之外；stateDir 每次操作独立，日志和原/新环境包含敏感部署信息，不能挂给 Web 或公开文件服务。Linux 计划文件须 `0600`，私有目录须 `0700`；关键配置和父目录不能由不可信用户修改。

普通 CLI 直接安装的恢复暂存树必须由实际安装操作者的 UID 所有，并保留仅属主可访问的权限。若 API 以非 root UID 创建暂存，直接给 root（UID 0）安装仍会拒绝；`webRecordedRoot` 的路径映射不改变属主要求。受限 [主机 helper](backup-maintenance-host.md) 另有显式 API UID 的安全复制边界，在批准根中核对身份与内容后建立新的 root-owned 副本再安装；这不是放宽 `materializePrivateInfo`、开放目录权限或仅凭 root 可读取就直接采用 API-owned 输入。

计划包含：

- 显式项目目录/名称、Docker CLI 绝对路径、完整 Compose 文件列表与每个文件的 SHA-256。
- 当前完整环境文件及 SHA-256。不能通过重复制 `.env.example` 覆盖生产 `.env` 来准备计划。
- 宿主机 data/mail/dkim targets、六项 UID/GID、标准容器 runtime roots、私有安装 workspace 和独立 stateDir。
- `staged` 与独立可信 `expectedReportSHA256`；`restore` 会输出在线归档的 `report-sha256` 收据。仅有文件名 `READY` 或事后不可信文件的哈希不构成来源证明。
- 或以 `webJob/webRestoreRoot/webRecordedRoot` 取代 staged/report。SQLite 提供原 `webStateDb`；原生引擎提供 `webStateDsnEnv`，主密钥由受保护的进程环境读取，不放进计划 JSON。

实际路径和摘要必须在操作前核对。计划哈希会绑定整个操作；执行中不得编辑计划、Compose 文件、输入环境或重新指向目录。预检失败不进入安装；安装仍会对独立复制后的报告、数据库与 payload 再次验证。

## SQLite 服务组读取

默认数据目录和文件仍为 `0700/0600`。当目标 API 与 Postfix 使用不同 UID 时，可由部署者在 root 批准计划中显式设置 `sqliteGroupRead: true`，并把 `owners.data.gid` 填为实际容器 Postfix 的主 GID（正整数）。该策略仅支持 SQLite；不会从归档或 Web 请求推断、接受读取组，也不会改变 PostgreSQL/MySQL 的连接授权。

策略只作用于待发布的新 live 副本：data 根为 setgid `2750`，`lanqin.db` 为 `0640`；运行中的固定 WAL/SHM 须继承同一批准组及 `0640`。其他 data 目录/文件仍私有，原暂存、旧树及回滚恢复的旧权限均不改。读取组没有目录或数据库写权限，但能读取完整 SQLite 数据库，应按敏感数据访问授权选择服务组，不能使用广泛共享组或世界可读权限代替。

初次真实预检会检查 Postfix 账户身份与批准组是否相符，不符合时在停服前拒绝；省略策略时，只接受可使用原私有属主权限的读取身份。不要仅依据附加组成员判断有效权限：Postfix 降权可能清除附加组。安装 journal 和计划哈希均绑定本次策略；安装后校验以及冷恢复的 `resume-start` 会重新核对 live 根、主库与存在的 WAL/SHM 的类型、属主/组、权限和单链接约束，漂移或不匹配时拒绝，不自动 chmod 修复。

配置模板故意不启用该策略，仍保留六项 `-1` 身份占位，不能原样执行。手动 `install` 使用同义的 `--sqlite-group-read`，操作说明见 [恢复指南](backup-restore.md)。该机制已在隔离容器验证新 SMTP 投递、API/附件和 IMAP 读取，以及 API 写事务后的 sidecar 组/权限和无关 UID 拒绝；随后全新 ext4 VM 已由正式计划完成 all-in-one SQLite 覆盖恢复与回滚，没有安装后的手工权限补救。它不替代其他部署拓扑、真实云或生产验收，详见 [真实邮件演练](backup-real-mail-validation.md)。

## 可选队列与部署资产保全

[计划模板](../deploy/maintenance-plan.example.json) 不启用 `preservation`，保留原有空队列保护及故意无效的身份/摘要占位。需要本地保全时，再向已经审查的计划显式添加以下可选字段；公钥须替换为有效值：

```json
{
  "preservation": {
    "recipient": "age1replace-with-a-real-x25519-public-key",
    "identityEnv": "LANQIN_MAINTENANCE_PRESERVATION_IDENTITY",
    "quarantineQueues": false,
    "assets": []
  }
}
```

`identityEnv` 是维护进程的环境变量名，值为与公钥匹配的 age X25519 私钥；不放进计划 JSON、服务 `.env` 或 Docker/Compose 子进程。结构预检不读取私钥值。真正修改数据、环境或强制重建前，维护 gate 取得当前快照，完整回读解密验证实际密文及每个 payload，并再次核对源快照；变化或验证失败不继续。

保全会包含固定计划/容器/镜像绑定、计划列出的 Compose 文件、当前环境，以及存在时的 next 环境副本。`assets` 只额外指定固定摘要的普通部署文件，可选择 TLS/服务私钥，不扫描目录，也不把 age 恢复私钥一并装进包。额外资产最多 24 个、每个最多 8 MiB；单个容器 spool 最多 64 MiB，**每份 spool 与部署资产合计 payload 最多 256 MiB、160 项**，自动收集的材料也占预算。

启用保全不等于允许丢弃非空在线队列：只有显式将 `quarantineQueues` 设为 `true`，并通过加密保全、回读及当前源检查，才能隔离 pending 邮件后替换容器。这些邮件保留为证据，不会导入新 spool、去重或自动重新投递。该保全包也不会自动进入每日云备份。

私有 stateDir 每次操作最多保留 16 份 bundle，达到上限拒绝新快照，不自动删除旧包。**保留所有阶段的密文及收据，不能只留下最后一份**：后续快照中的当前环境可能已经切换为新值。隔离校验与恢复使用 `preservation-verify`、`preservation-restore`，不会应用配置或重放队列；完整参数和证据边界见 [维护保全](backup-preservation.md)。

## 查看、执行与状态

```text
./lanqin-backup maintain-plan --config /private/maintenance-plan.json
./lanqin-backup maintain --config /private/maintenance-plan.json --confirm-maintenance
./lanqin-backup maintain-status --config /private/maintenance-plan.json
```

`maintain-plan` 是不调用 Docker、不启动服务的结构/文件摘要审查，不证明当前服务或队列可安全停止。`maintain` 要求显式维护确认，再执行真实 Docker/容器预检；发现不支持组合时拒绝，不降级为只猜路径或只检查 API。

`maintain-status` 只读取已记录、与原计划哈希绑定的状态，不调用 Docker，也不证明服务此刻仍健康。即使源输入、环境或目标路径已经缺失，也可读取合法原日志用于诊断；实际恢复仍严格检查受控配置与当前目录状态。

正常流程：

```mermaid
flowchart LR
  A[固定计划与配置] --> B[停止并确认全部应用写入者]
  B --> C[空队列或显式验证保全后隔离并检查恢复副本]
  C --> D[保存旧树并安装新树]
  D --> E[离线数据与运行配置检查]
  E --> F[持久提交]
  F --> G[强制重建应用容器并检查健康]
```

宿主机目录被 rename 后，旧容器仍可能绑定旧 inode，因此新恢复必须重建而非仅 `start`。提交前准备失败且确定数据未变时，或已验证 rollback 把原目录 inode 放回时，可以启动原容器，保留其未持久化 spool。未知安装结果不启动任何一组数据。

**提交发生在第一个业务服务启动之前。** 启动后可能已接受新邮件，启动或健康失败不能自动把新数据库/目录退回旧快照。协调器会尝试停止应用写入者，保留新数据、旧树、环境副本和日志，报告 `needsRecovery`。不能把“已复制”或“已提交”单独当作已恢复健康。

## PostgreSQL / MySQL

原生恢复仍先在**独立空库**执行 `import-native`。计划中 `targetDsnEnv` 是维护环境可连接的新导入库，绝不是原生产库；恢复时保留该引用，以便 guard/激活/回滚。

另提供 `nextEnvironmentFile:{path,sha256}` 和 `runtimeDatabase:{host,port,database,user}`。前者为完整、私有、经审查的新运行环境副本，后者明确指定容器侧的新库身份。主机连接可能是 loopback 映射端口，容器连接可能是 `postgres:5432`；不能把一种 namespace 的 DSN 原样塞入另一种，也不能仅修改 API 的 `LANQIN_DATABASE_URL`。

API 的运行 DSN 和 Postfix/Dovecot/Rspamd 的 `LANQIN_DB_HOST/PORT/NAME/USER/PASSWORD` 必须一致。原生计划会在停服安装后，事务式切换真实环境文件，再渲染实际 Compose 配置并比较显式身份。私有原环境和回滚后的新环境均保留，journal 不存秘密值。

随后在无服务端口、无依赖启动、禁止拉镜像的 one-off 容器中，固定执行 `lanqin-backup verify-native`：只读检查该容器 namespace 中的实际数据库、已激活标记、schema/FK 和安装计划摘要配对，不调用 `App.New`、迁移、worker 或业务监听。通过后才允许提交并重建业务服务。

PostgreSQL 自定义 startup schema/role 参数、`PGOPTIONS/PGSERVICE/PGSERVICEFILE/PGHOSTADDR` 覆盖以及未适配的 Dovecot 连接串表示会拒绝，不能只比较原始字符串后猜测 API 和邮件组件连接同一库。需要外部密钥的功能仍依赖操作者提供与恢复数据对应的配置材料；健康检查不等于所有外部账户和真实 SMTP/IMAP 流程已验收。

## 显式恢复

保留原计划，不修改其哈希来适配眼前状态。读取 status，核对 stateDir、install journal、所有旧/新/failed 树和环境副本。恢复不重新执行 Install：

```text
./lanqin-backup maintain-recover --config /private/maintenance-plan.json --operation resume-start --confirm-maintenance
./lanqin-backup maintain-recover --config /private/maintenance-plan.json --operation rollback --confirm-maintenance
```

`resume-start` 校验当前已提交的数据/环境配对后重建，不要求业务数据仍与旧归档字节相等，也不重新暂停或重放新产生的邮件任务。`rollback` 需要明确维护确认，新产生的数据树会保留在 failed/quarantine 位置；若需要替换容器，当前 Postfix spool 仍须可证为空，或在原计划明确允许 `preservation.quarantineQueues: true` 的前提下重新通过保全 gate 后隔离。否则保留停止的容器，不丢弃其队列内容。

若崩溃发生在环境旧文件已保存而新文件尚未发布之间，只有带合法私有环境 journal 的显式 rollback 可以在项目锁内恢复环境，再继续停止和回滚；不会在取得锁前改环境。若安装返回收据尚未写入协调日志，必须检查真实 install journal 后，显式提供同一 stateDir 的 `--journal` 和 `--transaction`。不提供、结果矛盾或无法证明时保持停止并报告需要恢复，不猜测目录归属。

```text
./lanqin-backup maintain-recover --config /private/maintenance-plan.json --operation rollback --journal /srv/lanqin-maintenance/restore-001/restore-<transaction>.journal --transaction <transaction> --confirm-maintenance
```

不要删除状态文件来让 `maintain` 再跑，不使用 `down -v`、`rm`、`prune` 或清空旧树作为恢复手段。相同项目使用固定 OS 锁串行化，已经存在的操作不会自动重放。

## 当前验证

2026-11-04，全新 WHPX/Linux ext4 VM 使用真实 Docker 29.9.2、Compose 5.6.1 和当前工作树派生镜像，完成了 all-in-one SQLite CLI 的在线备份、隔离解密、正式 `maintain`、恢复后实际 SMTP/API 附件/IMAPS 收取，以及显式回滚。MIME 摘要与 INTERNALDATE 保持，旧定时发送被取消，新接受邮件保留在 failed 树；最新 Linux install/maintenance 全包亦通过。仅测试账户和数据参与，没有宿主数据共享或端口转发；stack、原生数据库 Docker、Web 实际点击流程和非空队列重导入仍未验收。准确范围见 [真实邮件演练](backup-real-mail-validation.md)。

以下为此前的模拟与 Linux syscall 分层验证，保留其原有证据边界，不将它们反推为额外部署拓扑已通过：

测试覆盖持久状态、锁、停止/安装/校验/重建/健康故障、取消、损坏状态、环境复制/rename/fsync、缺失目录与显式恢复、当前数据保护、SQLite 完整 payload 和 DB 摘要，以及 Compose 固定参数/角色身份/挂载/标签/空队列 tar 拒绝规则。本轮新增保全格式、CLI 和维护 gate 已进入最终 Windows 全量测试并通过；启用保全时在控制器预检前拒绝固定文件中的 age 恢复私钥，包含跨 64 KiB 边界回归。

本轮在相同隔离 Linux VM 中实际执行了 maintenance、preservation、`cmd/backup` 全包，均通过；私钥检查前移后再次重编并执行最新 maintenance 全包通过。Docker runner 仍为模拟，不能据此声称真实容器或邮件服务已验收。

Windows 上包含协调器的最近 Go 全量已通过，其后最后的镜像身份/运行渲染补丁也通过 maintenance、备份 CLI 和安装器受影响包复验；没有将该复验称为再次执行全仓测试。

在保全能力加入前，官方 QEMU 11.1 与 Alpine virt 内核的临时 Linux VM（无 NIC、无宿主挂载、1536 MiB RAM）曾实际执行 maintenance、install、`cmd/backup` 全包及 SQLite 真实维护 actions 回归，均 exit 0；不是仅有交叉编译。真实 Linux UID/属主检查、目录 rename 和 fsync 路径已运行，但 guest 只有 RAM rootfs/tmpfs，没有块设备，不能将 fsync 返回成功当作断电耐久性证明。controller/Compose runner 仍为模拟，未实测真实 Docker `cp`、SMTP/IMAP 收发或队列重放；Linux 原生数据库及 `verify-native` 的实际服务生命周期仍待验收。最终命令、本轮新增保全的验证范围和未验收部分见 [进度记录](backup-progress.md)。
