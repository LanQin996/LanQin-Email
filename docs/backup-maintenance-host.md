# Web 到主机维护 helper 的受控交接（实验性）

管理端可把**已成功验证并隔离暂存的在线恢复任务**交给 Linux 主机上的 root helper，按部署者预先批准的计划执行 [维护协调器](backup-maintenance.md)。API 不持有 Docker socket、root 可写 spool、任意命令或目标路径权限；它只能经固定 Unix socket 提交计划 ID、模板摘要、恢复任务 ID、认证报告摘要及当前管理员 ID。

这是具有停服和覆盖效果的维护入口，不是重新下载/解密按钮。只有真实超级管理员会话可访问，提交前必须再次校验当前密码，启用 TOTP 时还须使用未重放的验证码，并复用账户/IP 限流。普通系统设置权限或开放 API Token 不允许发起、列举或查询此入口。

**实验性的真实 HTTP API 到 root helper、all-in-one SQLite Docker 邮件覆盖恢复与回滚已在隔离环境通过。** 第 24 轮实际从 MinIO/WebDAV 上传和认证发现，再经 WebDAV ready 恢复任务、管理员重新认证和固定 Unix socket 执行正式维护；不是浏览器点击、注入 helper 或生产部署。其他数据库/部署组合、Google/商业云账号及浏览器仍待验收，见 [真实存储演练](backup-real-storage-validation.md) 和 [交付状态](backup-progress.md)。

## 固定配置与权限

默认 `LANQIN_BACKUP_MAINTENANCE_SOCKET` 为空，API 不联系 helper，计划列表返回 `{available:false,items:[]}`。启用时只给 API 一个固定 Unix socket 路径；root helper 的私有配置、状态、批准模板及目标库/保全私钥不通过 Web 配置，不向 API 提供其写权限。

helper 配置示例只展示结构，路径、真实 API UID 和模板摘要必须由部署者核对；不能直接原样运行：

```json
{
  "version": 1,
  "socket": "/run/lanqin-maintenance/helper.sock",
  "stateDir": "/srv/lanqin-host-maintenance",
  "apiUid": 10001,
  "plans": [
    {
      "id": "restore-local-sqlite",
      "template": {
        "path": "/private/approved-maintenance-plan.json",
        "sha256": "<trusted-template-file-sha256>"
      },
      "source": {
        "driver": "sqlite",
        "dbPath": "/srv/lanqin/data/lanqin.db",
        "restoreRoot": "/srv/lanqin/backups/restore",
        "recordedRoot": "/backups/restore"
      }
    }
  ]
}
```

- `template` 指向已审查、固定摘要的完整 [maintenance Config](../deploy/maintenance-plan.example.json)，其中不使用 Web 参数。`staged`/`expectedReportSHA256` 是模板占位输入，源目录此时可不存在；实际任务会生成独立 root-owned 输入及计划。模板本身的 state/workspace、部署固定文件和目录权限仍需满足维护配置校验。
- SQLite API 与 Postfix 使用不同 UID 时，部署者可在该 root 批准模板中显式设置 `sqliteGroupRead: true`，并将 `owners.data.gid` 核对为目标 Postfix 的实际主 GID；模板摘要及生成计划会绑定此策略。Web 只选择批准的计划，不能自行提供开关、UID/GID 或修改权限。data 根的 setgid `2750` 与主库/固定 WAL/SHM 的 `0640` 仅允许受信任服务组读取，不允许组写入；其他数据仍私有，冷恢复会拒绝权限漂移。完整边界见 [SQLite 服务组读取](backup-maintenance.md#sqlite-服务组读取)。
- SQLite source 使用显式 `dbPath`；PostgreSQL/MySQL source 使用 `dsnEnv` 环境变量名而不含 `dbPath`。`restoreRoot` 是 helper namespace 中固定的实际源根，`recordedRoot` 为原 API 任务记录中的容器根；不从请求接受或猜测目录映射。
- `apiUid` 必须显式写出，是 helper 实际看到的 Unix peer 数字 UID，不是浏览器用户 ID；0 是合法的显式值，不是省略字段的默认授权。容器部署须核对 user namespace/UID 映射，不能照抄示例。socket 所在父目录由 root 管理、API 不可改写；socket 本身为 `0666`，连接授权依赖内核 peer credentials 严格匹配 `apiUid`，不能把它当作任意用户均可执行维护。若 API 在主机 namespace 中确实以 UID 0 连接，同 UID 的进程都处于这项授权边界内；该 socket 不能将已是主机 root 的进程彼此隔离，也不能靠容器内显示 root 推断宿主 peer UID。
- API 端客户端反向检查 helper peer UID 必须为 0，不经 TCP、HTTP 代理或重定向寻找 helper。Windows 不支持生产 daemon；测试注入接口不是面向用户的绕过开关。
- API 数据库备份主密钥 `LANQIN_BACKUP_SECRET_KEY` 由 helper 在受保护环境读取，用于验证原任务记录；源/目标 DSN 及保全私钥也只按批准模板中的环境引用提供。不要把这些秘密值写入 helper JSON、批准模板、日志或 Web payload，也不要向 API 分享额外的 root 目标库/保全私钥。

在 root 所有、私有配置文件和目录准备完成后，由部署者单独监督启动：

```text
./lanqin-backup maintenance-host --config /private/maintenance-host.json --confirm-maintenance-service
```

此命令仅支持 Linux root。没有默认创建或启动系统服务，没有默认给 API 挂载 socket，更不会改写已有 `deploy/.env`。socket 的容器挂载与实际 Linux 权限须由部署者在隔离部署中验证，不能为解决连接问题改挂 Docker socket 或给 API 挂 root 私有 stateDir。

## 管理端与后台流程

计划列表只公开 `{id,templateSHA256,driver,queueQuarantine}`，无服务器路径、UID、DSN 或私钥。管理员选择与恢复任务 driver 匹配的批准计划，核对覆盖及可能的队列隔离提示，再提交密码/TOTP 和维护确认。Web 请求不包含路径、命令、恢复私钥或身份映射。

helper 会重新核验模板和原加密恢复任务、当前发起管理员、报告摘要及固定源根。API UID 所有的暂存不是直接授予 root 安装器的可信树：helper 使用受限、重新校验的复制过程，在自身私有 `stateDir/operation-<request-id>/` 下建立新的 root-owned `recovered`、`workspace`、`state` 和 `maintenance-plan.json`。复制后再次核对来源；原生数据库先导入批准的新空库并保留 guard，再进入维护协调。该专用复制不放宽普通 `install` 的 `materializePrivateInfo` 属主检查。

业务服务停止、队列处理、文件/环境切换、提交及健康恢复继续受 [协调器](backup-maintenance.md) 和可选 [保全 gate](backup-preservation.md) 限制。helper 接受排队、API 显示维护中，或源任务的 `READY` 都不代表生产恢复完成。

## 请求标识、失联和恢复

客户端在 POST 前固定 nonce，host ID 为 `SHA256(UTF8(user.id + ":" + nonce))` 的前 16 字节小写 hex，不带前缀。例如 `usr_synthetic-admin` 与 `550e8400-e29b-41d4-a716-446655440000` 对应 `d2eea1e29533371c9b88b0cf6d4529d2`。这只是稳定标识，不替代超级管理员认证。

相同 ID 和完全相同的请求只返回已有状态；同 ID 换内容，或换 nonce 重复提交同计划和同恢复任务，会拒绝。当前 helper 只允许一项活动操作；重启遇到未完成记录会改为 `needsRecovery`，不自动重放导入或停服。`needsRecovery` 会阻挡新请求，不能删状态文件或换 nonce 当成安全重试。

daemon 关闭时会先停止接收请求，等待已有 HTTP handler 结束，并取消、等待 worker 的有界清理退出，最后才释放主机锁。不能将停止进程的意图或 socket 断连当成旧操作已经清理完成；新 daemon/人工 resolve 仍须取得同一 host 锁。

停服/覆盖会使 API 暂时不可达，恢复安装还会撤销旧会话。POST 响应丢失时不要自动再次 POST；保存 nonce/预计算 ID，待 API 可达并重新登录后查询相同状态。当前超级管理员可查询已知 host ID，不依赖恢复后的新数据库仍保留旧任务或旧会话。helper 返回 `404` 仅表示该 ID 当前未查到，不表示操作已经安全失败、回滚或从未执行。

状态仅包括安全枚举：`queued/preparing/maintaining/succeeded/failed/needsRecovery`；phase 为 `queued/preparing/importing/maintenance/completed/interrupted`。错误码不会包含原始 Docker 输出、主机路径或 DSN。未知错误统一呈现 helper 不可用；不能靠错误文字推断目录切换结果。

需要人工恢复时，在宿主机核对该操作生成的 `maintenance-plan.json`、协调日志、安装 journal、所有新旧树和环境副本，再使用原有 `maintain-status`/显式 `maintain-recover`。当前没有 Web 端任意恢复、强制清锁或重新执行命令接口；也不能仅看到文件已复制就解除 helper 的待恢复状态。保留全部计划、状态与保全证据。

完成原计划的受控恢复后，先停止 daemon，确认它已完成 handler/worker 清理，再显式请求解除该 host 记录的隔离：

```text
./lanqin-backup maintenance-host-resolve --config /private/maintenance-host.json --request <32-hex-host-request-id> --confirm-maintenance-service
```

`maintenance-host-resolve` 仅支持 Linux root，须取得与 daemon 相同的 host 锁。它只读取固定 root 记录、批准模板、该操作的精确生成计划及 `ReadState`，不启动或停止服务、不重新复制、不导入数据库，也不重放 Install。只有协调状态无 `needsRecovery`/待处理动作，且证明为已提交的 `healthy`，或已验证恢复原服务且数据已回滚/确定未修改，才可把 host `needsRecovery` 转为相应的成功或失败终态。`READY` 文件、目录存在、手工改状态或仅“服务看起来在线”均不构成解除条件。

一个更窄的可放弃分支是：helper 在导入前的 `queued/preparing` 阶段中断，root 私有记录保留了该中断阶段，且该操作根本没有协调 `state.journal`。显式 resolve 可以将它记为 `failed/interrupted`，不会伪造已安装/已回滚的收据。若阶段已到 `importing/maintenance`，或存在未证明安全的 core 状态，则仍拒绝；操作员必须先核对新库 guard、原生成计划及服务/目录结果并完成受控恢复。不能删除记录、重新编号或换 nonce 绕过隔离；处理后重新启动 daemon 才恢复接收批准请求。

## 验证边界

第 24 轮在新 ext4 VM 中实际调用生产 HTTP 路由、root Unix socket 和 helper，完成 all-in-one SQLite 的正式覆盖维护与回滚。host 成功状态与 core `healthy/committed/needsRecovery=false` 对应；旧 Cookie 为 401、旧恢复任务为 404，新会话仍可读取同一已知 host ID。原 MIME/附件/INTERNALDATE 保持，快照后邮件从新收件箱消失，旧定时发送取消；新 SMTP 邮件可读，回滚后旧邮件回来、新邮件保留在 failed 树。此测试使用标准 all-in-one 的 API UID 0；非 root API 跨 UID 的边界由下述独立 Linux 测试验证，不能声称所有容器 UID 映射已经联调。

这次没有新增生产代码或重跑 Go 全量，也没有浏览器交互；真实存储是隔离自建的 MinIO/WsgiDAV，不是 Google/商业云账号。来源、版本、输入摘要及复现步骤见 [真实存储演练](backup-real-storage-validation.md)。以下保留此前分层测试的原范围：

App 自动测试使用真实 HTTP/会话认证、SQLite 中已完成的 age 恢复任务及注入 helper 客户端，覆盖密码/TOTP 防重放、限流、角色/API Token 拒绝、批准摘要/driver/ready 绑定、未知字段和敏感错误拒绝，以及响应丢失后的只读查询。这部分不调用 Docker，不能独自证明 root socket peer 权限或容器 UID 映射。

本轮 Windows 最终 Go 全量通过，之后 resolve 的小补丁也通过 host/CLI 定向复验和全量 vet。隔离 Linux VM 实际通过 root Unix socket、非批准 UID 1001 客户端拒绝、客户端拒绝非 root 假 helper，以及 shutdown 清理期间保持 host 锁的 host 全包（3.50 秒）；CLI 全包通过（4.77 秒）。另一次 handoff 全包复跑（0.68 秒）证明 UID 1001 源暂存经受限复制生成 root-owned 输入。最新 resolve/host 全包又在 VM 通过（3.59 秒）：使用真实协调器 `Run` 配合模拟服务动作生成状态日志，核验精确计划哈希和安全终态，不把它称为再次完成实际服务控制。

PostgreSQL/MySQL 的真实隔离 HOST 合同均通过并停止测试数据库，覆盖当前管理员身份、独立复制及受控空库导入，不执行服务控制；这不是 Linux 数据库部署或真实 Docker 协调。本轮前端完整检查在原 23 个文件 Prettier 阶段阻断；之后终态收起补丁的最新 shadcn、i18n（1792 项、10 个测试）、改动文件 ESLint/格式及 TypeScript/Vite 构建均通过。三语言实际 React SSR 共 126 断言通过，不是浏览器/真实 API/停服联调。各层结果及准确命令由 [进度记录](backup-progress.md) 汇总，不能把分段测试合称 Web 点击到真实邮件服务成功。

上述较早的 peer/复制测试 VM 无 NIC/宿主挂载，使用 RAM rootfs/tmpfs；不能把其 syscall 执行当作 Docker 或邮件部署验收。第 24 轮另用新 ext4 磁盘实际完成本文所列单一组合的服务闭环，仍不构成其他矩阵组合或块设备断电耐久证明。
