# 加密备份：在线采集、离线快照与多地点上传（Issue #34）

## 当前状态与边界

这是实验性的**SQLite 在线数据采集 / 离线副本导出 / 验证后解密到暂存区**，不是已完成的整站灾备功能。

- 已实现：独立 CLI、age X25519 公钥加密、加密格式版本、SQLite 完整性预检、资源限制、全流认证、可信收据校验及非覆盖暂存恢复；M2 首批增加现有密文的多个 S3 / WebDAV 地点上传、有限并发和独立结果。上传说明见 [多地点上传](backup-storage.md)。
- 普通采集、上传和隔离恢复命令不控制服务；`maintain` 仅按显式固定部署计划协调维护。现有 `create` 仍只读取准备好的离线副本，不可指向生产运行目录。
- 管理端已实现 [新建 SQLite/PostgreSQL/MySQL 在线备份、多地点配置、加密凭据库与既有归档异步上传](backup-admin.md)，仅允许真正超级管理员操作；支持阶段、逐地点结果、取消、历史及报告。新建与计划备份必须由管理员在后台自行设置并确认备份密码，并配置私有工作区；旧环境公钥不再启用后台新备份，未设置时不自动生成密码。
- 已接通 `create-online` CLI：独立 SQLite 在线导出、Maildir 引用定位及受控复制、附件采集、冻结草稿重建、DKIM 重建、未索引候选隔离、有限重试及加密发布。不依赖云盘快照或特殊文件系统。命令、范围和限制见 [在线备份](backup-online.md)。
- 已接通每地点独立每日计划、Google Drive OAuth/刷新/上传，以及 `fetch` 云端密文下载 CLI（S3/WebDAV/Drive）。真实 Google 授权和生产云服务仍未验收，不能将本地 TLS 模拟测试当成供应商联调。
- 已实现显式维护窗口的 [CLI 目录安装与回滚](backup-restore.md)，包括路径映射、UID/GID、INTERNALDATE 和运行态暂停；仅 Linux 可切换，合成数据验收不等于真实生产邮件服务恢复。
- 新增 [固定部署计划的维护协调 CLI](backup-maintenance.md)，串联停服证明、Postfix 队列保护、数据/环境事务、首次启动前提交及健康恢复。默认要求队列为空；只有显式启用并确认 [维护保全](backup-preservation.md)，才允许验证加密副本后隔离 pending 邮件，不自动重放。Web 可经 [固定 Unix socket 主机 helper](backup-maintenance-host.md) 重新认证后提交预批准计划；隔离 all-in-one SQLite 的真实 HTTP API、存储与邮件恢复/回滚已通过，范围见 [真实存储演练](backup-real-storage-validation.md)。
- 新增 `maintenance-host` 与 `maintenance-host-resolve` [主机维护服务和显式状态解除命令](backup-maintenance-host.md#请求标识失联和恢复)。前者仅 Linux root 接受批准 API UID 的有限请求；后者须停 daemon、取得主机锁，并验证原记录和生成计划，不执行服务控制或重放安装。
- 新增 `preservation-verify` / `preservation-restore` [保全包验证与隔离恢复 CLI](backup-preservation.md#验证与隔离恢复)。它们只处理独立的维护证据，不控制服务、应用 `.env` 或重投递队列；维护保全不使每日云备份自动包含部署资产。
- 已接通默认关闭的 [远端保留策略](backup-retention.md)：按地点保留最近成功上传的副本，新旧密文回读校验及强 ETag 条件删除；真实供应商仍待验收。
- 已接通 [Web 恢复验证及维护交接](backup-web-restore.md)：从已确认成功上传记录选副本，重新认证后后台下载/解密/完整验证到私有暂存，按认证任务和清单摘要交接给 CLI 安装器。
- 已接通 [分片、加密检查点和跨进程继续](backup-resume.md)：S3 multipart、Drive 分块，以及 WebDAV 完整响应核对；明确取消可关闭已知 S3 未完成会话。
- 已接通实验性的 [PostgreSQL/MySQL 同引擎逻辑快照、独立空库导入及维护安装/激活/回滚](backup-native-databases.md)，真实双库及公开导入 CLI 往返通过；Linux 原生维护生命周期仍待实际 runner 验收。
- 已接通部署者显式授权的 [内网 NAS 白名单](backup-storage.md#明确授权内网-nas)，默认仍为公网 HTTPS，不放宽 TLS 或 Drive 固定来源。
- 已增加 [独立云端认证恢复索引](backup-discovery.md)、`discover`、索引绑定的 `fetch` 和管理端后台发现→分页选择→重新认证恢复。新实例无需上传历史或本地密文目录；本地协议、真实 API/数据库/age 与界面静态验证不替代真实供应商验收。
- 尚未完成：压缩/增量/去重、独立连接测试入口、备份失败主动通知、Postfix 队列自动导入/去重/重投递、完整部署资产自动应用，以及 Google/商业云账号、完整实例/部署矩阵、浏览器和生产验收。真实 MinIO/WebDAV 到 HTTP API、root helper 与 all-in-one SQLite Docker 邮件闭环已通过，不扩称这些剩余组合也已验收；显式维护保全仍是本地隔离证据，详见 [交付状态](backup-progress.md)。
- 原 SQLite version 1 不冒充原生数据库格式：MySQL/PostgreSQL 使用显式 version 2 原生归档，跨数据库类型迁移不在计划内。
- 生产目标平台为 Linux；Windows 上的合成测试不等于 Linux 服务恢复验证。Windows 的 Unix mode 位不代表私有 ACL，真实密钥和明文暂存目录必须另行设置合适的 ACL。

**不要把旧 `create` 指向活目录，或把恢复目录直接挂给邮件容器。** 在线采集使用独立的 `create-online` 命令；先在隔离环境用合成数据演练，当前没有生产 Linux 恢复验收。

## 一致性与备份范围

旧 `create` 命令的输入必须是管理员事先准备好的、保持不变的离线副本：

```text
offline-snapshot/
  data/
    lanqin.db
    attachments/...
    ...其他 data 持久化文件
  mail/...
  dkim/...
```

顶层只允许这三个目录，必须全部存在；链接和特殊文件不支持。SQLite 必须已经正常关闭、完成 checkpoint，无 `-wal`、`-shm` 或 `-journal` 旁文件，并通过 `quick_check`。

**这些检查无法证明原服务已经停写。** `--confirm-offline-snapshot` 是旧命令的操作者确认，不是自动协调器。`create-online` 另走应用级引用闭包采集；单独在线复制 SQLite 仍不保证与 Maildir 一致，也不能将在线归档理解为跨文件物理原子快照。

文件名、数据库、附件和 DKIM 内容都位于密文内。但该副本**不包含**宿主机 `.env` / 外部主密钥、Postfix spool、证书等未放入指定目录的部署资产或远程模式 IMAP 邮件。没有这些内容时不能承诺整站完整恢复。工具不自动抓取进程环境、凭据目录或其他宿主机文件。

部署者可在维护窗口另外启用 [队列与显式部署资产保全](backup-preservation.md)，生成独立 scope 的本地加密包；这不改变上述日常 data/mail/dkim 归档范围，不自动上传云端，也不把保全包中的环境或队列应用到服务。

## 格式与密钥

格式为 `age(tar(manifest + data + mail + dkim))`，使用 `filippo.io/age`，不自行实现密码算法。第一版不压缩。加密清单 `lanqin-backup.json` 为 `version=1`、`driver=sqlite`；旧命令使用 `scope=offline-data`，在线命令使用 `scope=online-data`，后者还要求有效的 `data/backup-online.json` 完成清单。旧版只理解离线 scope 的恢复器会拒绝在线归档，不应绕过检查。

后台新备份使用管理员自设密码的 age scrypt 加密；密码通过 `LANQIN_BACKUP_SECRET_KEY` 加密保存以供计划任务使用。该主密钥负责配置加密及云端索引认证，不是备份恢复密码。历史密码归档仍需当时密码，不会随改密自动更新。

历史公钥归档及原始 CLI 的显式 X25519 公钥模式仍兼容，不会自动回退到环境公钥。该模式恢复私钥由管理员离线保管，不需留在邮件服务器上。CLI 不接受命令行私钥值，也不将私钥打印到终端。丢失历史密码或私钥不能通过重置站点密码恢复旧备份。下文公钥命令是独立 CLI 的显式选择，不是后台新备份的启用步骤。

age 的认证加密可以检测密文被修改/截断，**不等于签名证明发送者身份**：知道公钥的人仍然可以生成一份全新的密文。因此：

- 导出生成密文 SHA-256 收据；恢复必须提供从**独立可信位置**取得的 `--expected-sha256`，校验实际解密的同一条输入流，而非先读一次再换文件。
- 不能把同一个不可信云端同时提供的文件和 SHA-256 当成来源认证。手动上传要求既有可信收据；支持独立发现的认证索引通过原备份主密钥派生的独立用途 HMAC 验证，仍需在云端之外保管该主密钥。没有认证索引的旧对象继续使用独立可信原收据，不能仅凭远端列表自动认证。
- age 的最终认证帧必须读到 EOF；tar 的 EOF 本身不代表认证完成。尾部额外明文、密文截断及尾部认证失败均拒绝发布。

## CLI 演练

在源码根目录构建（Go 版本以 `apps/api/go.mod` 为准）：

```text
go -C apps/api build -o ../../tmp/lanqin-backup ./cmd/backup
```

Windows 的构建输出可加 `.exe` 后缀。以下命令以 Linux、已准备好的隔离离线快照为例；私钥、密文、恢复目录均放在源快照之外，输出父目录需预先存在且权限合适：

```text
./tmp/lanqin-backup keygen --identity-file /secure/offline/recovery-key.txt
```

仅标准输出公钥（例如 `age1...`），私钥文件独占创建，不会覆盖同名文件。将私钥安全转移离线保存后，备份机只需公钥：

```text
./tmp/lanqin-backup create --snapshot /staging/offline-snapshot --recipient age1... --out /backups/sample.age --confirm-offline-snapshot
```

命令会输出密文 SHA-256 收据，必须与备份分开保管在可信位置。输出文件只有在导出完成、关闭并同步后才以不覆盖现有文件的方式发布；文件系统需支持同目录硬链接。不支持时失败，不退化为覆盖式重命名。

```text
./tmp/lanqin-backup restore --in /backups/sample.age --identity-file /secure/offline/recovery-key.txt --expected-sha256 <trusted-64-hex-receipt> --out /staging/restore-attempt
```

`--out` 必须是不存在的新目录，不接受已有空目录，更不允许覆盖运行目录。成功结果为：

```text
restore-attempt/
  READY
  snapshot/
    data/...
    mail/...
    dkim/...
```

失败时可能留下私有的 `.partial` 暂存目录；**无 READY 就不能视为恢复成功**。不会递归删除用户目录或自动重新使用失败目录。检查失败原因后由操作者处置，重新尝试使用新目录。

归档链接、特殊设备、重复路径和越界路径被拒绝。恢复文件默认 `0600`、目录 `0700`（Unix），不会重放归档的 UID/GID、可执行位或特殊权限；原始 tar 元数据不是执行授权。生产所有权映射、Maildir 验证、停写切换、回滚以及暂停旧发送任务由后续阶段实现。

默认数据总量上限 10 GiB、归档条目上限 100000，可用 `--max-bytes` / `--max-entries` 显式调整，两者必须为正。中断、空间不足和 I/O 错误不能产出可用的最终备份/READY。失败暂存区及私钥目录均需由管理员维护容量和访问权限。

维护保全包使用不同的 `maintenance-preservation` scope，不能交给普通 `restore` 冒充 data/mail/dkim 恢复。取得独立可信的密文收据和私钥文件后，只做完整验证或恢复到不存在的隔离证据目录：

```text
./tmp/lanqin-backup preservation-verify --in /private/preservation.age --identity-file /offline/preservation-key.txt --expected-sha256 <trusted-cipher-sha256>
./tmp/lanqin-backup preservation-restore --in /private/preservation.age --identity-file /offline/preservation-key.txt --expected-sha256 <trusted-cipher-sha256> --out /private/new-preservation-evidence
```

两条命令均核验完整密文和 payload；`preservation-verify` 不写明文文件，`preservation-restore` 只生成私有 manifest、顺序编号的 opaque payload 和最后的 `READY`，不按 manifest 中的来源说明写回原路径或展开 spool。默认上限为总 payload 256 MiB、160 项，可用 `--max-bytes/--max-entries` 收紧。实际部署资产选择、队列隔离确认及所有阶段证据保留规则见 [维护保全](backup-preservation.md)。

## 多备份地点约定（M2 / M3，已实现能力与剩余验收）

备份地点按**独立实例列表**管理，不是 S3、WebDAV、Google Drive 各一个全局配置。可以同时添加多个 S3 地点，例如名为 Cloudflare R2、Backblaze B2、S3 3 的三个独立配置；这些名称仅为配置示例，不表示已完成供应商兼容性验证。

以下保留完整设计要求，不表示每项入口均已完成；独立连接测试 UI/API 仍未实现，实际多目标和部署验收范围以 [完成核对](backup-progress.md#原-issue-与-m0m4-完成核对) 为准。

- 每个地点使用稳定、唯一的 `destination_id`，独立保存名称、存储类型、连接配置、凭据引用、启用状态、计划、时区、保留策略及执行记录。不得以存储类型作为唯一键，也不得仅凭 endpoint 或 bucket 判定重复；同一服务的不同账户、桶或前缀均可分别配置。
- S3 地点分别设置 endpoint、region、bucket、prefix、寻址方式及凭据；凭据加密存储，API 和界面不回显秘密，不允许通过一个地点覆盖另一个地点的凭据。WebDAV、Drive 同样允许多个实例；共享授权时，删除地点不能顺带撤销其他地点仍在使用的授权。
- 支持对单个地点手动备份、对选定地点批量备份，以及各地点独立启停定时计划。未开启计划仍可手动备份。地点停用、取消任务和删除配置应明确区分；删除配置不默认删除远端备份。
- 同一次批量任务中，快照范围和密码配置相同的地点可复用一次一致性快照及同一密文，分别创建上传子任务；不同加密配置需要独立加密产物。不同计划触发的任务不默认视为同一快照。
- 上传采用全局有界并发和逐地点有界重试，防止多个慢目标拖垮邮箱服务。每个地点分别记录排队、运行、成功、失败、取消以及最近成功时间；一个地点失败不取消其他地点，也不能把“部分成功”显示为全部成功。保留策略只处理该地点已确认完整的备份，失败不能触发删除最后一份成功备份。
- 管理界面采用“备份地点”列表，展示名称、类型、计划状态、最近成功时间及当前任务状态，并提供新增、编辑、连接测试、手动备份、历史和停用等操作。恢复时先选地点，再选该地点的完整备份。界面继续遵守 shadcn、移动端和三语言要求。

验收至少覆盖：同时配置 **3 个 S3、2 个 WebDAV 和 Google Drive 地点**；相同类型及同一 endpoint 的不同配置能够共存；凭据和计划互不覆盖；批量执行中一个目标失败、其他目标仍成功；失败目标不更新最近成功时间；关闭计划后仍能手动执行；重试、保留策略和取消不会误操作其他地点。所有地点配置、测试连接、备份和恢复接口均由服务端校验超级管理员权限，新增自定义远端地址时还需防范 SSRF 和凭据跨目标泄漏。

此节是完整设计与后续验收约定。当前 CLI 已实现 SQLite 在线采集、三种存储的多地点上传、云端下载，以及维护窗口目录安装/回滚；配置文件只保存环境变量引用。管理端已接通新建备份、加密凭据库、Drive OAuth、每日计划、上传、历史、取消、脱敏报告、默认关闭的远端保留策略及 Web 隔离恢复验证，并可经受限 root helper 自动交接批准维护计划。真实 1 个 MinIO S3 + 1 个 WebDAV 的手动/日程、认证发现和下载，以及 HTTP API 到 helper 的 all-in-one SQLite 邮件覆盖/回滚已通过；**尚不能据此认定 3S3+2DAV+Drive、其他部署矩阵、浏览器或生产验收完成**。`disabled` 表示地点停用，与独立每日计划开关不同。

## 后续里程碑

1. 验证已接入的管理端新建备份在真实 Linux 部署中的资源影响；补齐生产恢复权限/路径/IMAP 元数据映射及邮件服务恢复演练。不要求云盘 API、LVM、Btrfs 或 ZFS。
2. 增加签名或独立可信目录、压缩、完整范围清单与部署恢复包。
3. 存储适配层：WebDAV、S3、Google Drive OAuth，按上述多地点实例模型实现，保持统一格式、独立目标状态和有界重试。
4. 管理员任务接口、UI（三语言）、日程、保留策略与可回滚覆盖恢复。只允许真正超级管理员，不挂载不受限 Docker socket。
5. PostgreSQL/MySQL 一致性导出 / 恢复及 all-in-one / 分体部署矩阵；最后再评估多端同步。

参考：[age 项目](https://github.com/FiloSottile/age)、[Go 库文档](https://pkg.go.dev/filippo.io/age)。这不是对应用侧新代码已经完成安全审计的声明。
