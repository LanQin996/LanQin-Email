# 真实存储服务到 Web API、主机 helper 与邮件恢复演练

2026-11-04，第 24 轮全新隔离 VM 实际通过了 **一处 MinIO S3-compatible + 一处 WsgiDAV、all-in-one SQLite、真实 HTTP API 到 root helper** 的闭环。不是注入 uploader、下载函数或 helper 响应，也没有通过修改数据库伪造成功任务。本轮只增加演练材料和文档，没有修改生产 Go 代码或重新运行 Go 全量；此前代码检查和第 23 轮独立 CLI 邮件演练仍是各自范围的证据。

VM 使用 WHPX、Linux、Docker 29.9.2、Compose 5.6.1 和全新 ext4 虚拟磁盘，没有 NIC、宿主文件共享或宿主端口转发。存储服务只监听客体内的私网 loopback 地址，客户端使用明确测试 CA 验证 HTTPS，并由生产网络策略显式批准固定主机、端口及 IP；没有关闭证书校验或启用全局私网访问。

## 实际完成的闭环

1. 当前邮件 API 创建两个独立地点，凭据进入应用加密配置。通过 `POST /api/admin/backups/snapshots` 创建一项快照，返回后由真实后台任务采集、age 加密并上传两处服务。两份结果均成功，生产完整密文回读与认证恢复索引发布成功。
2. 两地点分别配置每日 `05:16 UTC`。等待实际时钟触发后，各自生成不同任务 ID、密文及成功索引收据；下次时间推进到次日 `05:16 UTC`。这是实际定时任务，不是伪造时钟或直接写入队列。
3. 分别提交 S3 和 WebDAV 发现任务，经真实服务列表及认证索引读取，各返回两项完整备份：手动与定时产生的备份。使用服务端安全分页 DTO 精确绑定原地点、对象、SHA-256 和字节数。
4. 从 S3 认证发现项真实下载，age 解密后得到成功且 ready 的 SQLite 暂存。WebDAV 先使用错误私钥，任务失败且未发布 ready；立刻通过 API、附件及 IMAPS 验证 live 邮件原文与 INTERNALDATE 未变。随后使用正确私钥，从 WebDAV 完成真实下载和成功 ready 暂存。
5. 当前管理员密码重新认证后，向 `POST /api/admin/backups/maintenance/requests` 提交批准计划与已认证任务引用。API 经固定 Unix socket 联系真实 root helper；helper 独立验证、复制和调用正式维护协调器，没有把 Docker socket 或 root state 挂给 API。
6. host 状态与实际 core 收据均成功：core 为 `healthy/committed=true/needsRecovery=false`。旧 Cookie 调用 `/api/me` 得到 401；重新登录后，原恢复任务查询得到 404，但同一已知 host ID 的持久状态仍可读取。这证明状态查询不依赖已被恢复过程排除的旧任务记录。
7. 恢复后的原文 MIME、296 字节二进制附件和实际 IMAP INTERNALDATE 与基线一致；快照后邮件不在新的活动收件箱，旧定时发送为 `cancelled`。恢复后接受的新 SMTP 邮件经授权附件下载及明确测试 CA 的 IMAPS 验证可读。
8. 使用 helper 生成的精确维护计划执行显式回滚，`rollbackCompleted/originalServicesRestored=true` 且 `needsRecovery=false`。旧树中的快照后邮件重新可读，恢复后新接受的邮件保留在 failed Maildir 中。

最终结果为 `success:true`，客体任务结果 0、完成标记与宿主 QEMU 正常退出相符。IMAP UIDVALIDITY 改变，不承诺 UID 稳定。测试结束已停止 helper、存储进程、邮件容器及拥有的 Docker daemon；没有提交、推送或部署到现有服务。

## 服务来源与当前运行身份

MinIO 使用官方仓库固定 release tag 的源码构建。官方二进制下载渠道返回 410、匿名镜像渠道返回 401 后，没有寻找或读取其他账户凭据；改用固定 commit、记录归档 SHA-256 并保留构建收据。WebDAV 使用官方 PyPI 分发的 WsgiDAV 和 Cheroot，并记录每个依赖分发的来源及摘要。

| 材料 | 本轮身份 |
| --- | --- |
| MinIO tag / commit | `RELEASE.2025-09-07T16-13-09Z` / `07c3a429bfed433e49018cb0f78a52145d4bedeb` |
| MinIO 源码归档 SHA-256 | `8819e3e7817e46b7b3798f8f200ead208562e571563c2e040352378031abe9f2` |
| MinIO 客体二进制 SHA-256 | `53ad55c16b965fee6b39247c99b3726ddff59b11217ee6a7359d723b8a38befd` |
| WsgiDAV 4.4.0 wheel SHA-256 | `bd8ac483c63e11e9be3a17dcd5158da76c181fdfe6f5ce7066213b956002e344` |
| Cheroot 11.1.2 wheel SHA-256 | `0f6c0ba05c00fbc869fb46b1de4ec2384e1d85418ae963d3bc10ae83b688dbfa` |
| 存储输入归档 SHA-256 | `f59674bd2e15af4949e04578d7ad7d64b6bb36dd0b42a08a271fe5337235ee9a` |
| 当前邮件派生镜像 | `sha256:e294254cf68c81d93fee66223e330a3bb868e3fbfc80e9902c2dc59a22764d13` |

邮件运行依赖仍来自已核验的旧公开基础镜像，当前 API、备份 CLI、前端和部署配置由工作树构建覆盖，不能把基础发布版本等同于当前源码。context 与宿主 CLI 的输入摘要和公开基础镜像身份见 [独立 CLI 演练](backup-real-mail-validation.md)；本轮精确输入列表另由 `execution.json` 冻结。

## 复现步骤与公开 API

以下表格记录本轮历史公钥模式演练的输入，不代表新密码要求已经完成同样的服务验收。当前后台新建及计划备份须先通过 `POST /api/admin/backups/encryption` 自设并确认密码，使用返回的密码配置代绑定快照/计划；不能再靠环境公钥启用。恢复新密码归档用 `encryptionMode=password` 与 `backupPassword`，历史公钥归档仍用原私钥。当前操作指南见 [管理端配置](backup-admin.md)。

先准备独立 Linux Docker 环境、合成邮箱、私有工作区、受信测试 CA，以及真实 S3-compatible/WebDAV 服务。配置当前 API 的公钥、加密凭据主密钥、恢复根和精确网络策略；root helper 另有私有、固定摘要的批准模板。SQLite 主组读取按 [维护计划](backup-maintenance.md#sqlite-服务组读取) 显式批准，不从 HTTP 请求接受组或路径。

按以下公开 API 顺序操作，所有改变状态的请求只提交一次，轮询使用 GET：

| 步骤 | API 与关键核验 |
| --- | --- |
| 地点 | `GET /api/admin/backups/destinations` 取得 revision；`POST /api/admin/backups/destinations` 创建各地点后重新 GET 取 ID 和新 revision |
| 手动快照 | `GET /api/admin/backups/capabilities` 核对公钥；`POST /api/admin/backups/snapshots` 绑定地点版本和公钥，GET job 核验全部地点成功及索引状态 |
| 每日计划 | `POST /api/admin/backups/destinations/{id}/schedule`，绑定当前计划/地点 revision、公钥和 `HH:MM`/IANA 时区；等待真实 due 后核验独立任务和次日游标 |
| 认证发现 | `POST /api/admin/backups/discoveries`；成功且 ready 后 `GET /api/admin/backups/jobs/{id}/discovery`，只使用返回的游标和条目 ID |
| 隔离恢复 | `POST /api/admin/backups/restores`，提交发现任务/条目、地点、可信摘要、当前密码与 age 私钥；先证明错钥失败且 live 未变，再证明正确任务成功 ready |
| 覆盖交接 | `GET /api/admin/backups/maintenance/plans`；重新认证后 `POST /api/admin/backups/maintenance/requests`，只选择批准计划，不传主机路径或命令 |
| 失联与登录 | POST 前固定 nonce 并推导 host ID；失联后只 GET 已知 ID，重新登录，验证旧会话和旧任务失效但持久 host 状态仍可读 |
| 收发与回滚 | 验证实际 SMTP、授权附件与 IMAPS 内容/INTERNALDATE；使用原生成计划显式 `maintain-recover --operation rollback`，核验旧树及新邮件隔离副本 |

详细请求 DTO 见 [API](API.md)、[Web 恢复](backup-web-restore.md) 与 [主机 helper](backup-maintenance-host.md)。日程示例应按本次测试环境选择未来 due，不能照搬已过去的 `05:16`；本轮没有通过倒拨系统时钟或直接造 job 达成定时成功。

本机工作区 `E:\Projet\LanQin-Email` 的关键证据集中为：

```text
tmp/issue34-docker-vm/run24/execution.json
tmp/issue34-docker-vm/run24/serial.log
tmp/issue34-docker-vm/run24/web-storage-evidence/result.json
tmp/issue34-docker-vm/run24/web-storage-evidence/commands.log
tmp/issue34-storage-assets-ul9tzpwt/sources.json
tmp/issue34-storage-assets-ul9tzpwt/minio-build-source.json
tmp/issue34-web-storage-integration.py
tmp/issue34-web-backup-protocol.py
tmp/issue34-storage-services.py
tmp/issue34-web-storage-task.sh
```

这些 ignored `tmp` 文件是本次工作区的演练输入和证据，不随仓库 clone 分发，也没有接入通用 CI 邮件/云服务集成任务。重放需要对应输入、依赖缓存及新的空客体磁盘；不能重用旧维护 journal 或把本地收据路径当作生产路径。

## 未被本轮证明的要求

这是实际自建服务软件的隔离联调，强于此前合成 TLS 协议测试，但不是 Google OAuth/Drive API、商业 AWS/R2/B2 或第三方 WebDAV 的真实账户联调。本轮只有一个 S3 和一个 WebDAV 实例，尚未完成 Issue 中至少 3 个 S3、2 个 WebDAV 与 Drive 共存的完整真实服务样例，也不覆盖真实大文件中断/撤销授权矩阵。

客户端是调用生产路由的 HTTP 测试工具，不是浏览器点击；没有新增 Portal、移动布局、键盘或页面提交断言。还未覆盖 stack、PostgreSQL/MySQL Docker 维护、非空队列自动重导入及完整生产运维。当前归档为 `age(tar(...))`，不压缩；失败通知、独立连接测试入口等原计划缺口不能用本次成功演练替代。完整要求核对见 [交付状态](backup-progress.md)。
