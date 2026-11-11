# 管理端备份地点与归档上传（Issue #34）

超级管理员可在后台「备份地点」新增多个 S3 / WebDAV 地点，并进行编辑、启停、删除。每个地点独立配置；相同 endpoint / bucket、不同账户或前缀可以共存。简体中文、繁体中文及英文均使用现有界面翻译系统。

**已接通后台新建 SQLite、PostgreSQL、MySQL 在线备份、执行计划、Drive OAuth 及既有归档异步上传。** 管理员必须自行设置并确认备份密码，才能新建或启用计划备份；还可选择多个地点、观察阶段、取消任务、查看历史及下载脱敏报告。每地点新增默认关闭的 [远端保留策略](backup-retention.md)，经确认后可保留最近 1..100 份。[Web 恢复向导](backup-web-restore.md) 已接通已确认副本下载、重新认证、解密及隔离验证；SQLite 使用维护窗口 [CLI 安装/回滚](backup-restore.md)，PostgreSQL/MySQL 使用 [原生空库导入与维护安装](backup-native-databases.md)。原生 Web 任务可直接交给 CLI 验证、导入和安装，无需再次提交解密凭据，不从 Web 自动切换生产；自动维护协调和生产邮件恢复验收仍未完成。通用实现见 [在线备份](backup-online.md)，验收边界见 [交付状态](backup-progress.md)。

## 配置主密钥

在 API 进程的受保护环境中设置 `LANQIN_BACKUP_SECRET_KEY`，值必须是 **Base64 编码的 32 字节随机密钥**，例如通过 `openssl rand -base64 32` 生成。不要使用站点密码、S3 Secret 或 age 恢复私钥替代。

- 主密钥只从环境读取，不接受管理端修改、不写入数据库、不通过 API 返回。未配置/格式不正确不会阻止邮件应用启动，但无法新增、编辑或删除地点。
- 全部地点记录（包括地址、ID、名称及凭据）使用标准 AES-256-GCM、独立随机 nonce 和固定用途 AAD 加密，保存在已有 `system_settings` 的 `backup.destinations.v1` 项。没有新增数据表或修改现有表结构。
- 不能只把数据库内的 endpoint 改成另一个地址以复用旧凭据：认证会失败；地址及凭据属于同一加密记录。管理员通过 API 更换地址时也必须重新填写凭据。
- 主密钥与 age 离线恢复私钥用途不同。数据库快照内的加密配置不是主密钥副本；整站恢复后要重新使用这些地点凭据，仍需原始 `LANQIN_BACKUP_SECRET_KEY`。在独立可信位置离线保存它，不要连同配置数据库一起暴露。
- **尚无主密钥轮换工具。** 不要直接更换或丢弃已有密钥。错误/缺失密钥或被篡改密文会使已有配置读取失败（`503`），不会清空后悄悄覆盖。需恢复原密钥或另行设计可审计迁移。
- Compose 的 `env_file` 会传递所配置的环境；仅向需要的 API 进程提供该密钥。此变更没有改写 `deploy/.env`、重启服务或触碰生产数据。

## 接口与凭据语义

### 管理员自设备份密码与历史公钥归档

超级管理员必须在备份设置中输入并确认自己的备份密码；未设置时不能新建备份或启用计划，也不会自动生成密码。`LANQIN_BACKUP_RECIPIENT` 即使保留旧值也不能启用后台新备份。新密码为 8–128 个 Unicode 字符（按码点计数），不能全为空白或包含控制字符；首尾空格按原样保留。归档使用标准 age scrypt 密码加密，服务端为自动任务保存的密码通过既有主密钥加密后落库，读取接口仅返回已设置状态与不透明配置标识，不回显密码。

8–128 字符限制仅用于新设置或修改的密码；旧规则下已保存的密码继续可读、可执行备份及验证旧密码，历史归档恢复也不受新长度限制影响。字符按 Unicode 码点计数，不按 UTF-8 字节或浏览器 UTF-16 长度计数，不进行 Unicode 归一化。

已有备份密码时，修改密码须验证当前备份密码，并使用最新配置 revision；这只影响之后创建的归档，**旧备份不会自动改密，仍需创建当时的旧密码或对应 age 私钥**。密码更换后，绑定旧配置的排队采集任务拒绝执行，旧计划到期禁用并要求重新确认。请将历次密码保存在独立密码管理器中，应用不保存可供选择的密码历史。

后台不再提供公钥模式。历史 age 公钥归档仍可用对应 `AGE-SECRET-KEY-...` 私钥恢复，原始 CLI 的显式 `--recipient` / 私钥恢复参数仍兼容，但不会自动回退到环境公钥。保留历史私钥，不要因设置新密码而丢弃；应用不会自动持有它或重新加密旧归档。

密码、age 私钥与 `LANQIN_BACKUP_SECRET_KEY` 分别承担归档解密、归档解密与凭据库/索引认证职责。**在另一台服务器离线解密已取得的密码归档，只需该归档的密码及独立可信 SHA-256 收据，不依赖原应用主密钥；从远端自动发现和验证旧索引仍需要原主密钥及云端访问权限。** 只记住备份密码，不代表新实例能自动发现所有旧备份，也不能解开原数据库内的地点凭据。见 [密码归档恢复](backup-restore.md#1-取回并验证) 与 [独立发现](backup-discovery.md)。

接口需要会话认证以及实际 `role=admin`，不向开放 API Token 增加备份接口。即使普通用户被授予全部系统设置权限，仍不能读取或修改地点。

列表响应：

```json
{
  "encryptionConfigured": true,
  "revision": "<opaque-current-revision>",
  "items": [
    {
      "id": "bkd_example",
      "name": "S3 example",
      "type": "s3",
      "disabled": false,
      "credentialsSet": true,
      "sessionTokenSet": false,
      "s3": {
        "endpoint": "https://s3.example.invalid",
        "region": "auto",
        "bucket": "mail-backups",
        "prefix": "lanqin",
        "pathStyle": true
      }
    }
  ]
}
```

初始空库 `revision` 为 `"0"`。所有写请求必须带上读取时的 revision；落库使用跨进程原子 compare-and-swap。陈旧页面返回 `409`，不覆盖他人的配置；删除最后一个地点仍保留加密空集合，旧 revision `"0"` 不会重新有效。

- 新增 `POST /api/admin/backups/destinations`：发送 `revision/name/type/disabled`，以及 `s3` 或 `webdav` 对象。ID 由服务端生成。
- `s3` 除列表中的参数外，写入时可带 `accessKeyId`、`secretAccessKey`、`sessionToken`、`clearSessionToken`。新增必须填写密钥对；编辑时两项同时留空表示保留，不能只替换一项。更换 endpoint 必须填写新密钥对，不带走旧 token；替换密钥对时旧 token 会清除，可同时提交新 token。未替换密钥对时 token 留空保留，`clearSessionToken: true` 显式删除 token。
- `webdav` 参数为 `url/username/password`。新增必须填写用户名和密码；同地址编辑时同时留空表示保留。改变 url 必须重新填写二者，不自动携带旧认证信息。
- 更新 `POST /api/admin/backups/destinations/{id}`，存储类型不可变；切换类型需要明确删除后新建，避免跨类型凭据误用。
- 删除 `DELETE /api/admin/backups/destinations/{id}`，body 只包含 `revision`。只删除地点配置，不发出任何远端删除请求。
- 写成功只返回 `{ok:true}`，客户端重新读取列表和新 revision。所有元数据响应 `Cache-Control: no-store`；既不回填密钥/用户名，也不返回数据库密文。

最多 64 个地点，单次写 body 最多 32 KiB，各凭据值最多 8192 字节。加密后集合最多 60 KiB，以兼容最小 MySQL TEXT 容量；超限拒绝整次更新，保留原配置。该上限不是“一种存储只能有一个”。

连接参数复用 CLI 校验：HTTPS、普通 S3 bucket、无查询字符串/URL 用户信息、无越界前缀，默认拒绝私网 IP 字面量。明确的内网 NAS/MinIO 可由部署者配置精确 host/port/IP 白名单，见 [网络策略](backup-storage.md#明确授权内网-nas)；不接受 Web/地点 JSON 自行放宽。**保存成功不是连接成功。** 配置接口不访问远端；后台上传使用上传引擎的 DNS 固定、当前网络策略、证书校验及不跟随重定向策略。

## 发布归档与异步任务

### 从云端独立发现

「从云端发现备份」可在没有本实例上传历史、没有本地密文目录时使用：先配置可解密原认证索引的原备份主密钥及相应云端地点凭据，再选择地点创建后台发现任务。Drive 可明确填原文件夹 ID，但该目录仍须对当前 Google 授权可见。发现不会修改地点的文件夹绑定或最近上传成功。

任务完成后显示有界分页的认证历史索引。选中一条后验证当前管理员密码/TOTP，并选择归档创建时的备份密码或 age 私钥解密；服务端从认证页解析对象和原身份，不接受浏览器自选对象路径。超过 `restoreMaxBytes` 的条目或无法确认上限时，界面拒绝提交并指向独立 CLI。发现或恢复均不自动覆盖生产，详见 [独立发现](backup-discovery.md) 与 [维护交接](backup-web-restore.md)。

### 启用管理端新建备份

除既有 `LANQIN_BACKUP_SECRET_KEY`、`LANQIN_BACKUP_ARTIFACT_DIR` 外，还需配置工作目录并设置备份密码：

- 在后台自行设置并确认备份密码；不要求 `LANQIN_BACKUP_RECIPIENT`，也不自动生成密码。
- 旧环境公钥或旧公钥模式记录不能代替该设置；未设置密码的旧计划不能继续生成新归档，需设置密码后重新确认计划。历史归档和恢复能力不因此改密或删除。
- `LANQIN_BACKUP_WORKSPACE`：预先存在的私有可写工作目录，真实路径必须在 data/mail 源根之外。Unix 权限只允许属主访问；Windows 需设置私有 ACL。认证归档目录也必须在源根之外，新建任务需要 API 对它有写权限；只有手动发布+上传时 API 可只读挂载。

浏览器只提交当前密码配置的 `expectedRecipient`（`password:<opaque-generation>`），不是密码本身或任意加密目标。地点 revision、加密配置标识和幂等 nonce 在打开确认窗时固定；配置变化返回 `409` 或在执行前失败，不能悄悄使用另一密码或公钥。每个父任务串行使用现有租约队列，采集最多三十分钟、SQLite 子步骤五分钟、全任务一小时；上传仍有限并发/重试。管理端目前将采集 payload 限为 896 MiB，并另设 1 GiB 密文硬上限，超过即失败而非生成残缺对象。加上明文、密文、认证目录副本及上传 spool，磁盘峰值可接近上限的 3–4 倍。

采集使用独立数据库连接/快照事务，加密、文件发布和网络传输不借用业务连接池，monitor 贯穿所有阶段续租与取消。只有加密、明文清理和认证目录发布成功后才把 artifact 附加到任务并开始上传。发布与 DB 附加不能跨文件系统原子提交；中断可能留下合法本地孤立密文，不能因任务失败就删除它或擅自重传。导出副本排除备份队列/租约/任务历史，以防旧备份任务自动重放；邮件发送状态在备份中保留，正式维护安装时暂停或取消可重放任务，再受控激活。

### 容器部署的可选备份挂载

API 和 all-in-one 镜像现在都构建并包含 `/usr/local/bin/lanqin-backup`。需使用包含该改动的新构建或版本；本地源码改变不等于已发布的 `latest` 镜像已经更新。默认 Compose 不新增备份挂载，`deploy/.env.example` 的旧公钥兼容项、工作区、归档/恢复目录和 Drive OAuth 客户端值均留空，部署者显式启用并在后台自设备份密码。

选用 `deploy/docker-compose.backup.yml`（all-in-one）或 `deploy/docker-compose.stack.backup.yml`（分体，仅 API 服务）。覆盖文件把宿主机 `deploy/backups/work`、`deploy/backups/artifacts`、`deploy/backups/restore` 分别挂到容器 `/backups/work`、`/backups/artifacts`、`/backups/restore`，独立于邮件源根；工作区和恢复区会包含私密明文。挂载声明 `create_host_path: false`，不会依赖 Compose 自动创建可能过宽的目录。

在 Linux 宿主机上先核对目标镜像中 API 运行账户的数字 UID/GID，再为**新建的专属目录**设置权限。下面从仓库/部署仓库根运行，`API_UID`、`API_GID` 由操作员填入实际值；不要猜测 Maildir 账户或照搬其他镜像的 UID。现有目录先核对身份和内容，不直接递归改权：

```sh
install -d -m 0700 -o "$API_UID" -g "$API_GID" \
  deploy/backups deploy/backups/work deploy/backups/artifacts deploy/backups/restore
```

在已有的受保护 `deploy/.env` 中设置 `LANQIN_BACKUP_SECRET_KEY`，然后由管理员在后台自行设置并确认备份密码；仅配置旧 `LANQIN_BACKUP_RECIPIENT` 不会启用新备份。选用的覆盖文件会设置三个 `/backups` 容器路径，不在 `.env` 放恢复私钥。需要 Drive 时，可在地点授权区配置共享 OAuth Client ID 和 Client Secret，加密保存后即时生效，无需编辑环境文件。若任一 Drive 客户端环境变量非空，则继续以环境配置优先，后台只读；部分环境参数缺失时不能混用数据库配置。不要为启用备份重新复制 `.env.example` 覆盖已有部署密码。

仅需要内网 NAS 时再设置 `LANQIN_BACKUP_NETWORK_ALLOWLIST_JSON`；空值保留公网默认。精确 JSON 示例和约束见 [网络策略](backup-storage.md#明确授权内网-nas)，不要将它替换成全网段/CIDR或允许任意私网的标志。分体部署只给 API 挂备份目录并授予该策略；其他邮件服务不需要这项访问配置。

先渲染检查所选组合，再在计划的部署变更窗口应用。以下为 all-in-one 默认数据库示例；使用 MySQL/PostgreSQL 时，在基础文件和备份文件之间保留已有的对应数据库覆盖文件。构建型部署也继续保留原构建覆盖文件和镜像配置：

```sh
docker compose --env-file deploy/.env \
  -f deploy/docker-compose.yml -f deploy/docker-compose.backup.yml config --quiet
docker compose --env-file deploy/.env \
  -f deploy/docker-compose.yml -f deploy/docker-compose.backup.yml up -d
```

分体部署使用以下组合；不要把 all-in-one 的 MySQL/PostgreSQL 覆盖文件直接叠到 stack，它们定义的应用服务名不同：

```sh
docker compose --env-file deploy/.env \
  -f deploy/docker-compose.stack.yml -f deploy/docker-compose.stack.backup.yml config --quiet
docker compose --env-file deploy/.env \
  -f deploy/docker-compose.stack.yml -f deploy/docker-compose.stack.backup.yml up -d
```

CI 配置检查覆盖 all-in-one 默认/MySQL/PostgreSQL 加备份文件及分体 stack 加备份文件；`config --quiet` 不启动服务，也不证明镜像构建、宿主机权限、真实上传或恢复已通过。正式恢复的目标目录切换必须在宿主机维护工具或受控离线 helper 中进行；运行中的 bind mount 挂载点不是可直接重命名的安装目标，镜像含 CLI 也不等于可以在运行容器内覆盖 live 挂载。

停用时先关闭备份计划，等待活动任务完成或明确取消，再移除相应备份覆盖文件和在线备份加密/路径配置，按原部署组合重新应用。保留原主密钥的离线备份。上述回退不会删除三个宿主机目录、已有远端副本或数据卷；不要用删除目录、`down -v` 或清空凭据库来代替关闭功能。成功恢复暂存及中断残留由操作员核对后单独处置，不自动递归清除。

## 执行周期与立即执行

本轮代码已扩展每地点计划，尚待本轮整体检查及部署结果汇总；此前真实服务验证仅覆盖每日计划，不能据此宣称小时、周或月的真实供应商长期运行均已验收。

- 每日：在所选 IANA 时区的 `HH:MM` 执行。旧计划没有 `period` 字段时继续按每日读取。
- 每小时：选择每小时的第 0–59 分钟；重复的本地小时只选较早一次。DST 缺失墙钟时间沿用向后推进到当天首个不早于目标时刻的规则。
- 每周：选择至少一个星期日到星期六及 `HH:MM`，允许一周多天。
- 每月：选择 1–31 日及 `HH:MM`；该月没有所选日期时取月底。时区完全跳过的日历日期不会被归一化为别日的额外任务。
- 固定间隔：每 N 分钟、小时或天，分别支持 5–527040 分钟、1–8760 小时、1–366 天。锚点由服务器保存计划时确定，首轮在一个完整间隔后触发；每 N 天是固定 `N×24` 小时，不随夏令时变成日历天。暂不支持每 N 秒，避免重型邮件快照形成高频任务。

「立即执行」复用手动在线快照接口，仅向当前地点创建一次任务，不要求先启用计划，也不改变下一次触发时间。地点本身仍需启用、配置可用，Drive 地点仍需完成授权；计划关闭不影响手动备份。

任务去重绑定地点、计划配置代及实际 UTC 触发点，同一天可以正确产生多轮小时/间隔任务。游标、任务与队列继续在同一事务中 CAS 提交；队列满不消费该次触发，错过多期只补最近一轮，不回放整个历史。

### 上传操作员已发布的归档

API 额外配置 `LANQIN_BACKUP_ARTIFACT_DIR` 为预先存在的私有目录（容器内外路径按挂载对应）；发布端需要写权限，API 仅需读权限，可只读挂载。不要放在 Web 静态目录，也不要放入需要递归采集的 data/mail/dkim 源目录，避免把旧备份再次打包。不配置时不启动上传 worker，邮箱功能继续运行。API 从不接收浏览器传入的本地路径，也不直接上传当前运行中的数据库文件。

操作员先用现有 `create` 得到真实的离线加密归档及可信 SHA-256 收据，再使用同一 `LANQIN_BACKUP_SECRET_KEY` 的受控环境执行：

```text
./tmp/lanqin-backup publish --catalog /backups/published --in /backups/sample.age --expected-sha256 <trusted-64-hex-receipt>
```

发布复制并校验实际字节，独占创建随机 ID，先发布密文再发布 HMAC 认证元数据，不覆盖现有文件。目录 HMAC 使用主密钥派生的独立用途密钥；更换主密钥会使原目录认证失败。该检查只证明发布内容与可信收据一致、有 age 格式头；没有恢复私钥时，不能证明密文能解密、包含本应用备份或已经过恢复验证。CLI 产生的真归档另有加解密往返测试。

- 默认每个归档上限 1 GiB；最多选择 16 个地点，最多 16 个待执行任务。worker 每次执行一个父任务，内部最多两个并发上传，每地点最多三次尝试。单任务最多一小时；每次尝试仍受上传引擎的 20 分钟上限约束。
- 入队立即返回 `202`，文件复制、哈希和网络传输都在后台，期间不占用业务数据库连接。账户凭据仅在执行前按地点解析到内存，不写进进程环境，不复制到任务记录，不返回客户端；上传会话另存为加密检查点，不在任务 DTO/报告中回显。
- 任务固定归档收据及地点配置指纹。执行前若地点删除、停用、地址或凭据改变，那个地点失败，其余仍执行；不能让排队任务悄悄上传到修改后的地址。
- 任务和队列使用现有 `system_settings`，按独立用途 AAD 加密并使用 CAS 更新；不新增表。每行保留 MySQL TEXT 容量余量，任务创建大小更严格以预留结果空间。
- 跨实例队列采用 30 秒租约、5 秒续租及 fencing token。失效任务标记 `interrupted`，**不自动重放**；已发布归档和检查点可用时，超级管理员可显式继续最多十次。取消通常一秒内开始终止请求，再有界尝试关闭已知 S3 未完成会话，不撤销已成功副本。服务器退出保留会话供继续。
- 新执行的管理端上传使用 [加密检查点与继续协议](backup-resume.md)，只有远端密文完整回读一致后才确认成功；旧版历史不追认这一验证，也不等于解密恢复演练。一个地点失败时父任务为部分成功，成功地点不重传、不回退其他地点最近成功。未知会话和孤立对象仍可能存在，不能盲删；明确边界及存储端权限见续传说明。
- 历史每页 20 条，当前不自动清理历史；管理员应监控数据库、密文目录及上传暂存空间容量。目录扫描上限 1000 个条目，超限报错而不是静默截断；远端保留策略不清理本地目录。禁止将目录和明文暂存区置于 Web 静态目录。

## 管理端新建备份验证（2026-10-30）

- 真实 HTTP/SQLite 覆盖新建 `202`、同 nonce 幂等、跨任务类型冲突、地点版本/预期公钥绑定、配置换钥拒绝、客户端路径字段拒绝，以及匿名、普通设置管理员拒绝访问。旧上传记录的空 kind/phase 仍兼容。
- 实际 Prepare→加密→认证目录发布→任务附加→上传接口链路，生成归档再实际解密恢复，验证源 DB 不变、导出副本没有可领取的备份任务；上传成功响应为注入 uploader，不是云端联调。覆盖长采集期间 `/api/me` 响应、取消、陈旧 token 阶段写入拒绝、配置私钥误填不回显和密文字节硬上限。
- 连续写入每个 SQLite 复制批次仍完成、生成固定读代副本，防自身续租造成导出饥饿。WAL 空间增长需按部署负载另行观察，不把合成测试当吞吐基准。
- 浏览器连接真实本地 API/SQLite/生产前端，实际新建归档并显示 SHA、公钥和采集邮件统计；两个 `.invalid` 目标故意失败以验证逐地点错误，不假报云端成功。已打开任务由简中切英文/繁中即时更新；390px 同源 iframe 实测 document/scroll 宽均 390，无横向溢出，不是真机触控验证。
- Go 全量 `go test ./... -count=1 -timeout=10m` 与 `go vet ./...` 通过，app 包约 255 秒；原始 gofmt 仍报告 54 个既有 CRLF 文件，规范化后无实际差异。PostgreSQL/MySQL 契约因无测试 DSN 跳过，在线采集仍只支持 SQLite。
- 前端完整 `check` 的 shadcn、i18n（1629 项/10 测试）、ESLint 通过，在上文/下文历史记录列出的 23 个未修改文件的 Prettier 阶段中断；单独 TypeScript/Vite 构建通过。未通过全仓格式化掩盖问题。
- 临时 UI 服务已关闭；辅助源码在忽略的 `apps/api/tmp/backup-capture-ui/main.go.txt`。未提交、推送、部署或改动线上服务；定时、Drive OAuth、云端自动下载及生产覆盖恢复仍未完成。

## 归档上传与在线采集原语验证（2026-10-29）

- SQLite 任务回归覆盖入队幂等、重复 claim、部分成功与独立最近成功时间、真实 `/api/me` 在长上传期间仍响应、运行中取消、排队取消、配置变更、过期租约、陈旧 worker fencing、分页及队列容量。新增退出中断持久化、不自动重放、发布后同大小密文被篡改时零远端尝试的测试。
- 浏览器发现任务完成与点击取消的竞态会误显示“配置已变更”；取消接口已改为终态幂等且不改写原结果，并补回归。该修正通过真实 SQLite 单测，修正后未重新进行浏览器取消验证。
- 真实本地 Go API、隔离 SQLite、真实加密归档及生产前端构建验证两个地点的异步入队、逐地点失败、历史及 JSON 报告下载；报告不含凭据、配置指纹或 fencing token。目标使用 `.invalid` 合成地址，无真实云端上传。
- 已打开任务的失败提示在简中、英文、繁中间即时切换；同源 iframe 提供 390px CSS 视口，表单实测文档宽与滚动宽均 390px，任务长摘要换行。不是手机真机测试。长传取消与部分成功使用后端注入 uploader 测试；S3/WebDAV 协议测试使用本地 TLS 服务器。
- `SnapshotSQLite` 与 DKIM 采集原语通过合成数据测试；完整 Maildir/附件在线协调器未完成，不作整站可恢复承诺。Go CLI 加密创建/发布/恢复往返及原上传协议测试通过。
- `go vet ./...`、Go 全量测试及新增针对性测试通过。一次全量重跑使用自设五分钟上限时在既有 Telegram 测试阶段超时；按默认十分钟上限重新运行全部通过（app 包 286 秒），日志位于忽略的 `tmp/issue34-api-full-test.log`。外部数据库契约因未配置 PostgreSQL/MySQL DSN 明确跳过。原始 `gofmt -l .` 仍列出 55 个既有 CRLF 文件；LF 规范化后无实际格式差异。
- 完整前端 `check` 的 shadcn、三语言及 ESLint 通过，仍在下方列出的 23 个未修改文件的 Prettier 阶段中断；单独 TypeScript / Vite 构建通过。临时本地验证服务已关闭，源码保留于忽略的 `apps/api/tmp/backup-jobs-ui/main.go.txt`；本轮未提交、推送或部署。

## 首批配置阶段验证记录（2026-10-29）

- SQLite 实际 HTTP 认证与 CRUD：3 个 S3 + 2 个 WebDAV、独立凭据、编辑保留、地址变更要求新凭据、删除隔离、错误/缺失主密钥、密文篡改、超限更新保留原数据、陈旧版本及双写 CAS。
- 权限测试覆盖匿名用户和只有 `admin.settings.view/update` 的用户对全部四条路由的拒绝。跨数据库契约测试也加入相同持久化/CAS 用例，但本机没有 Docker 或 PostgreSQL/MySQL 测试 DSN，尚未实际验证外部数据库。
- 使用真实本地 Go API + 生产构建前端 + 隔离 SQLite，通过浏览器保存 3 个 S3 和 2 个 WebDAV；验证重新加载后仍存在、仅停用一个地点、不回填凭据、更换地址错误及删除确认。没有连接真实云账号。
- 浏览器用第二标签切换语言，验证已打开弹窗和已有错误在 `zh-CN / en / zh-TW` 之间即时更新。首批移动视口覆盖调用后实际仍为 1280 像素，当时未完成移动端和待处理状态语言切换实测；后续布局验证已补齐，见下节。
- 前端 shadcn、三语言检查及 ESLint 通过，TypeScript / Vite 构建单独通过。完整 `pnpm --dir apps/web run check` 在 Prettier 阶段被 **23 个未修改文件**阻断，没有通过全仓库格式化掩盖。
- 后端 `go vet ./...` 和 `go test ./... -count=1` 通过；新增备份管理测试重复运行 3 次通过。原始 `gofmt -l .` 仍有 55 个既有 CRLF 文件，换行归一化后没有真实格式差异；未修改无关 Go 文件。PostgreSQL / MySQL 契约命令因未配置 DSN 跳过，不代表跨数据库验证通过。

前端格式阻断文件：

```text
src/components/auth-guard.tsx
src/components/auth-states.tsx
src/components/confirm-dialog.tsx
src/components/domain-collection-dialog.tsx
src/components/language-selector.tsx
src/components/mail-rule-utils.ts
src/components/mail-schedule-utils.ts
src/components/mail-statistics.tsx
src/components/telegram-settings-card.tsx
src/components/ui/dialog.tsx
src/components/ui/password-input.tsx
src/components/ui/sheet.tsx
src/components/ui/sidebar.tsx
src/components/ui/toast.tsx
src/components/ui/toaster.tsx
src/hooks/use-toast.ts
src/lib/diagnostic-messages.ts
src/lib/language.tsx
src/lib/permission-translations.ts
src/lib/utils.ts
src/pages/login.tsx
src/pages/not-found.tsx
src/pages/register.tsx
```

## 布局与图标调整验证（2026-10-29）

- 将嵌套卡片网格改为桌面「地点列表 + 详情」布局，移动端使用地点下拉选择器。操作集中到选中地点的编辑按钮、启用开关和更多菜单，删除仍需确认。
- 复用 Lucide 图标区分 S3 / WebDAV、启用 / 停用及端点、桶、前缀、区域、寻址方式；不按用户填写的地点名称猜测供应商品牌。未添加依赖、修改 UI 基础组件或放宽授权。
- 真实本地 API、隔离 SQLite、生产前端构建验证 3 个 S3 + 2 个 WebDAV 切换、新增后自动选中、删除当前合成测试项后刷新回退、更多菜单打开删除确认及取消。凭据不回填；未连接真实云账号。
- 使用同源测试 iframe 提供实际 **390 × 700** CSS 视口，读取确认 `innerWidth=390`、`documentElement.scrollWidth=390`，验证下拉选择、S3 / WebDAV 详情和编辑弹窗；这不是将桌面截图简单裁成手机宽度，也不等于真机触控测试。
- 测试服务只增加响应延迟以观察 UI，未模拟成功响应：验证提交期间控件禁用、`处理中...` 即时切换为英文、已有失败提示即时切换繁体，以及中文地点名称不随界面语言改变。
- 本次前端 shadcn、三语言、ESLint 及改动文件 Prettier 检查通过；TypeScript / Vite 构建单独通过。完整前端检查仍被上文 23 个未修改文件的 Prettier 格式问题阻断。
- 本次只修改前端布局、字典和本文档，没有重跑后端全量测试，也没有提交、推送、部署或修改生产服务。
