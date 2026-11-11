# 独立云端恢复索引（实验性）

`discover` 不依赖本地任务历史或密文目录。它列举选定 S3/WebDAV/Drive 地点中的恢复索引，验证原备份主密钥派生的独立用途 HMAC 后，保存新的私有索引文件。之后 `fetch` 重新验证该索引，再对实际下载的密文核对 SHA-256 和字节数；`restore` 仍使用管理员离线持有的 age 私钥完成全流认证及格式检查。

这不是仅凭远端 `.age` 文件名或云端摘要自动信任备份。没有认证索引的旧归档仍需要独立可信的原上传收据。云存储可以删除文件、索引或隐瞒列表；已观察到的认证索引不能证明列表完整、最新，也不能证明密文现在仍可下载或成功解密。

## 发布索引

管理端上传使用已有 `LANQIN_BACKUP_SECRET_KEY`，在密文上传成功后完整回读，再发布不可覆盖的认证索引并回读校验。原始 `state=succeeded` 表示密文上传结果；`index_state=succeeded|failed` 单独表示索引结果。索引失败不能改写密文已经上传的事实，但任务会明确报告部分完成，不据此清理该地点的旧备份。

独立 CLI 必须显式选择包含原主密钥的环境变量，值为 Base64 编码的 32 字节；不要把值写到命令行或配置 JSON：

```text
./lanqin-backup upload --config /secure/destinations.json --in /backups/mail.age --expected-sha256 <trusted-sha256> --index-key-env BACKUP_INDEX_KEY --spool-dir /private/spool
```

未指定 `--index-key-env` 的旧 CLI 上传保持原行为，不发布独立发现索引。恢复索引与 age 加密各有职责：索引主密钥用于来源认证，归档创建时的备份密码或 age 私钥用于解密；都要在云端之外独立保管。更换实例的凭据库密钥不会让新密钥验证旧索引，需使用原索引主密钥进行发现。

已取得的密码归档可以使用原备份密码和独立可信 SHA-256 收据在其他服务器通过 `restore --password-file` 离线解密，不依赖原主密钥。**仅输入备份密码不能让新实例自动发现全部旧备份**：远端索引验证仍需原主密钥，下载仍需云端凭据，Drive 还受原文件夹可见性和授权范围限制。修改备份密码也不改变旧归档所需的历史密码。

若密文已成功而索引发布失败，或要将有独立可信收据的旧对象纳入发现，可显式重试发布，命令会再次完整回读密文，不重新上传密文。管理端「继续任务」只继续尚未成功的密文传输；所有密文均成功而仅索引失败时不展示该入口，API 也拒绝空重试。混合失败任务可以继续其余密文，但不会悄悄清除旧索引错误：

```text
./lanqin-backup publish-index --config /secure/destinations.json --destination r2 --object-key lanqin/lanqin-0123456789abcdef0123456789abcdef.age --expected-sha256 <independently-trusted-sha256> --bytes <trusted-ciphertext-bytes> --index-key-env BACKUP_INDEX_KEY
```

发布不能从同一不可信对象的未认证 metadata 自动取得 trusted SHA。已有索引只在认证后的地点、原地点 ID、对象、SHA、字节数一致时被视为幂等成功，不覆盖原索引。旧报告未记录原 Drive 地点 ID 时，不应猜测该 ID 来给旧媒体生成认证索引。

## 管理端独立发现与恢复

管理端「备份地点」中的「从云端发现备份」不依赖本实例上传历史或本地密文目录。先在部署环境保留原索引主密钥，再配置并授权对应云端地点；界面不会接收索引密钥、任意 endpoint 或对象路径。选择地点后入队后台发现任务，可在任务详情查看进度、取消和重新打开结果。

Google Drive 默认读取当前授权文件夹；若要找回历史副本，可明确填写原文件夹 ID。该文件夹必须对当前 Google 授权可见，且其中索引须通过原主密钥认证；输入原 ID 不会修改地点配置，也不能绕过 `drive.file` 的授权范围。

发现最多 128 条认证索引，成功后每页最多 32 条。结果按任务独立加密保存，失败、取消、租约过期或中断不会开放部分列表；发现不更新最近上传成功时间，不触发保留清理。列表是观察到的历史收据，仍不证明远端文件当前可用或列表完整、最新。

从结果选择一条「恢复此备份」，重新输入当前管理员密码及 TOTP（已启用时），再选择该归档的历史备份密码或离线 age 私钥。浏览器仅提交不透明条目 ID 和已显示的摘要；服务端从认证分页记录取得对象、字节数、原地点身份及 Drive 文件夹，绑定当前地点配置、凭据与租约后入队。无上传历史的新实例也使用相同下载→全流认证→数据库/附件校验→私有暂存流程，随后通过现有维护 CLI 安装。Web 大小上限由 `restoreMaxBytes` 返回；未知或超限时不允许提交，应使用有界独立 CLI。

## CLI 新实例发现与下载

配置明确的原物理地点和新凭据。配置 ID/显示名称可以改变；S3 endpoint/bucket/prefix、WebDAV 集合或 Drive folder 必须与原索引物理绑定一致。下载仍使用当前配置的 endpoint 和网络策略，不信任索引提供任意 URL。

```text
./lanqin-backup discover --config /secure/destinations.json --destination r2 --index-key-env BACKUP_INDEX_KEY --out /private/discovered.json
./lanqin-backup fetch --config /secure/destinations.json --destination r2 --object-key lanqin/lanqin-0123456789abcdef0123456789abcdef.age --recovery-index-file /private/discovered.json --index-key-env BACKUP_INDEX_KEY --out /private/download.age
./lanqin-backup restore --in /private/download.age --identity-file /offline/recovery-key.txt --expected-sha256 <authenticated-sha256-from-discover> --out /private/recovered
```

`discover --out`、`fetch --out` 和 `restore --out` 均不能覆盖现有结果。`fetch` 的索引模式与 `--expected-sha256` 互斥，必须同时提供 `--recovery-index-file` 和 `--index-key-env`。发现文件是包含认证条目的有界 JSON；重新使用时逐条验证 HMAC、格式与地点绑定，不信任其未认证外层顺序或数量。它不是恢复私钥，也不包含凭据。

内网 NAS 需另显式提供 `--network-policy-env`，格式和 TLS 要求见 [网络策略](backup-storage.md#明确授权内网-nas)。停用地点可被明确选择用于只读恢复，不会开启自动备份、修改计划或触发删除。

## 存储后端与限制

- S3 使用受限 bucket/prefix 的分页列举；WebDAV 仅 Depth 1 读取集合直子项；Drive 仅查询明确 folder 和应用恢复索引属性。不会遍历用户的其他目录或把发现请求用于清理。
- 单次发现有时间、页数、条目及响应体上限；超限、坏签名、错密钥、冲突记录或不安全的路径会明确失败，不静默截断为完整成功。单索引最大 16 KiB，CLI bundle 最大 3 MiB、128 个条目。
- S3/WebDAV 的 sidecar 名为密文对象名加 `.index.json`；Drive 使用独立应用 JSON 文件。索引公开最少恢复元数据：随机对象 ID、字节数、密文 SHA、完成时间、原地点 ID 和物理地点哈希，不包含邮件主题、正文、文件名、凭据、恢复私钥或 endpoint 明文。
- Drive 继续使用 `drive.file`。新 OAuth Client/项目或重新授权后创建的新文件夹，不保证能访问旧 folder；操作员必须提供明确授权的原 folder 和有效令牌。认证索引保留原地点 ID，下载时用于核对媒体 appProperties，不将新配置 ID 冒充原绑定。
- 索引记录曾经验证的上传，不是远端存活证明。保留清理或手工删除可能让旧索引对应的数据不再可用；真正下载仍须通过大小、摘要及后续 age/数据库检查。历史索引不授权删除任何对象。

## 验证状态

格式、密钥隔离、跨新实例 ID、错误地点、索引篡改、重复/未知/尾随 JSON 以及 CLI 参数与下载绑定由针对性测试覆盖。三种协议在本地 TLS 合成服务中完成发布、幂等重试、发现、新实例 ID 绑定、下载实际文件及 age 恢复；坏索引、错误密钥、分页/响应上限、DAV XML/href、Drive 原身份与取消/租约 guard 测试通过，不等于真实供应商验收。索引整合全量 Go 测试与 vet 通过；前端静态检查/构建通过，但完整 check 仍受既有 Prettier 阻断，浏览器 QA 因工具无法识别 URL 而停止，没有可宣称通过的截图或 DOM 验证。

管理端和 CLI 均已接通独立发现；后台任务、分页、重新认证和无本地历史恢复由真实 API/数据库/age 合成样本验证，网络在 App 测试中注入，三种存储协议另用本地 TLS 验证。前端三语言静态检查、构建及实际 React HTML 的 SSR 验证不等于浏览器、移动端、Portal 或键盘操作验收。自动维护协调及真实供应商/邮件服务恢复仍需后续完成，最终命令及验收边界以 [交付状态](backup-progress.md) 为准。
