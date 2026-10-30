# Issue #34 交付状态（2026-10-31）

**尚未完成整个 Issue，不能关闭，也未部署本轮改动。** 下表区分实现、模拟服务验证和生产验收，避免把阶段完成当作全部完成。

| 能力 | 实现状态 | 已验证 / 未验证 |
| --- | --- | --- |
| 多个 S3、WebDAV、Drive 地点 | 已实现，独立配置及加密凭据 | SQLite/HTTP、前端、本地 TLS；未用真实云账号 |
| 不停服 SQLite、Maildir、附件、草稿、DKIM 采集 | 已实现，引用闭包而非整盘原子快照 | 真实文件/SQLite、并发变更、CLI 加解密；未做生产 Linux 恢复 |
| 手动任务、取消、历史、阶段、逐地点结果 | 已实现 | 真实 API/SQLite；成功云响应为注入 uploader，错误路径为本地模拟目标 |
| 每地点每日时间/IANA 时区/独立启停 | 已实现 | 假时钟驱动真实 SQLite 入队、CAS、防重启洪峰、DST；浏览器保存与下次时间 |
| Drive OAuth、PKCE、一次性 state、刷新 | 已实现，申请 `drive.file` | 本地 TLS token/folder API、串绑/重放/刷新冲突/日志脱敏；真实 Google 同意未验收 |
| Drive 上传与文件校验 | 已实现，resumable session + 单次完整 PUT | 本地 TLS、多地点隔离、MD5/大小/归属校验；无跨进程续传 |
| 云端密文下载 `fetch` | 已实现 S3/WebDAV/Drive | 本地 TLS → 真实 age 解密/SQLite/邮件暂存往返；真实供应商待验收 |
| 错误密钥拒绝、非覆盖隔离恢复 | 已实现 | 全流认证、可信收据、越界/链接/资源限制测试 |
| Web 云端列表/恢复向导 | **未实现** | 当前使用可信上传报告 + CLI fetch/restore |
| 维护窗口 CLI 三目录覆盖、路径/UID/GID/INTERNALDATE 映射、旧发送暂停、回滚 | 已实现，见 [恢复指南](backup-restore.md) | 真实 Linux `/tmp` 合成目录和公开 CLI install/rollback 通过；真实邮件服务收发及生产恢复未验收 |
| 自动保留策略/远端清理 | **未实现** | 不自动删除任何远端备份，不宣称会清除最后成功版本 |
| 分片/跨进程断点续传 | **未实现** | 中断可留远端孤立对象；Drive 308 不推进已上传 offset |
| PostgreSQL/MySQL 数据备份恢复 | **未实现** | 仅元数据持久化有契约测试；无测试 DSN 时仍跳过，不能声称跨库通过 |

## 每日计划的行为

- 每地点配置 `HH:MM` 和 IANA 时区，开启或编辑后从下一次未来触发开始。计划关闭不影响手动备份，也不取消已经入队的任务；需要取消时在任务列表明确取消。
- 固定一个 30 秒扫描 worker，只入队，不在定时器里执行采集/网络。使用同一个容量 16 的后台队列，满队列不消费触发点，后续扫描重试。
- 计划游标、任务及队列在同一 DB 事务 CAS 提交；每天的请求 ID 按计划代、地点、当地日期确定，多实例并发不重复创建。错过多天只补最近一轮，不回放整个历史。
- DST 重复小时取最早实际时间；缺失时间取当天第一个不早于目标的实际分钟。计划变更生成新代，明确改变的计划不是旧计划重复任务。
- 地点删除、停用、凭据授权代或公钥变化，到期时禁用旧计划并记录安全错误，不自动改为新的上传目标。正常 Drive 短期令牌刷新不改变授权代，因此不会误禁用计划。
- 在线导出的私有数据库副本排除备份计划、任务/租约及未完成 OAuth state，源库不变。邮件发送业务状态仍保留，生产恢复的暂停/激活还需后续实现。

## Google Drive 的部署要求

部署者配置 `LANQIN_BACKUP_DRIVE_CLIENT_ID`、`LANQIN_BACKUP_DRIVE_CLIENT_SECRET` 及 HTTPS 站点根 `LANQIN_PUBLIC_BASE_URL`；在自己的 Google OAuth Web 客户端注册：

```text
https://your-mail.example/api/admin/backups/drive/oauth/callback
```

实例还需原有 `LANQIN_BACKUP_SECRET_KEY` 加密凭据库。界面不接收网盘密码、refresh token 或恢复私钥；配置缺失时授权按钮关闭并明确提示。新建地点后点击授权，由 Google 同意页完成；每次授权创建独立应用备份文件夹，重新授权不删除旧目录，也不自动导入旧账户内容。

授权采用管理员/地点/配置代绑定、十分钟单次 state、PKCE S256 和离线授权；令牌进入加密凭据库，授权码、state、session URL 和 provider body 不返回日志/错误。每个 Drive 上传任务前刷新一次令牌，刷新 CAS 合并无关地点更新，不用新授权的 token 配旧文件夹。

**反向代理也必须对该 callback 去掉查询参数日志。** 应用已脱敏自己的请求日志，但无法控制 Nginx/CDN 等外层记录 `$request` / `$request_uri`。callback 日志宜只记录 `$uri`；不得把完整 code/state URL 收集进访问日志或错误上报。

Drive 使用固定 Google HTTPS origins、证书和公网地址验证，拒绝代理环境和重定向。上传返回的 session URL 仅限固定上传 origin/path，不持久化或回显。当前网络失败重试新对象/session，不是跨进程断点续传；上传成功仍不是下载解密验收。[Google 上传文档](https://developers.google.com/workspace/drive/api/guides/manage-uploads)、[OAuth 服务端流程](https://developers.google.com/identity/protocols/oauth2/web-server)。

## 云端下载到隔离恢复

从独立可信位置保管的上传报告取得地点、object key（Drive 为文件 ID）及密文 SHA-256，配置文件仍只放凭据环境变量引用：

```text
./tmp/lanqin-backup fetch --config /secure/destinations.json --destination r2 --object-key lanqin/lanqin-0123456789abcdef0123456789abcdef.age --expected-sha256 <trusted-64-hex> --out /staging/download.age
./tmp/lanqin-backup restore --in /staging/download.age --identity-file /offline/recovery-key.txt --expected-sha256 <trusted-64-hex> --out /staging/new-recovery
```

输出必须不存在。默认密文上限 1 GiB，可显式调至单 PUT 上限 5 GiB；下载有时限，不自动重试，失败只清除本次临时密文。停用地点可以显式读取，不会被自动启用。S3/WebDAV 只接受本应用生成的对象名及选定前缀；Drive 先验证父目录、地点和可信摘要元数据，再下载媒体。不会遍历或读取不相关云文件，不使用默认 AWS 凭据链。解密后可按恢复指南使用明确维护确认的 `install/rollback`；不是 Web 一键覆盖，也不自动启动服务。

独立 CLI 的 Drive 配置为 `type: "drive"`、`drive: {"folder_id":"<bound-folder>","access_token_env":"BACKUP_DRIVE_ACCESS"}`。该短期令牌须由操作者显式提供；CLI fetch 不读取应用加密凭据库，也不自动启动 OAuth 流程。不要把 bearer token 放入命令行或配置 JSON。

`fetch` 校验同一条下载流的可信 SHA-256 和 age 格式头，不持有恢复私钥、不做完整解密认证；第二条 `restore` 才校验 age 全流认证。文件和收据若都来自同一不可信存储，不构成来源认证。下载/暂存成功不表示已覆盖生产，旧发送任务、绝对路径、权限、IMAP 元数据、未索引候选与回滚还需人工/后续受控恢复流程处理。

## 本轮验证记录

- `go test ./... -count=1 -timeout=10m` 全量通过，app 包约 335 秒；`go vet ./...` 通过。原始 gofmt 仍列出 54 个既有 CRLF 文件，LF 规范化后无实际格式差异，未改无关文件。
- 前端完整 `pnpm --dir apps/web run check` 的 shadcn、i18n（1664 项、10 个测试）和全量 ESLint 通过，在既有 23 个文件的 Prettier 阶段中断；单独 TypeScript/Vite 构建通过，失败文件清单见 [管理端验证记录](backup-admin.md)。
- 本地真实 API/SQLite/生产前端保存每日 `23:58 Asia/Shanghai` 并显示正确下次触发；验证 Drive 地点无需手填 token，缺 OAuth 客户端时按钮禁用并明确提示。时间控件提交改为读取命名 FormData 字段，修复原生时间值更新时保存旧值的问题。浏览器未进行真实 Google 同意。
- 真实本地 TLS 测试覆盖 Drive 多目标隔离、回调到新目录绑定、刷新令牌轮换/CAS、错误授权不阻断 S3、恶意 session URL/元数据/校验值拒绝；日志测试证明 callback code/state/provider error 不入应用请求日志，handler 仍收到真实参数。
- 修复并回归了初次指纹检查与令牌刷新之间的重新授权竞态：worker 将排队指纹传给刷新函数，新授权不得与旧文件夹混用，测试确认不匹配时零 token 网络请求。
- 三种存储真实本地 TLS 下载到实际 age 解密/SQLite/邮件暂存往返通过；S3 大于 64 KiB 的归档未被错误响应上限截断。错误摘要、越界对象、Drive 父目录或归属错误、取消、超限和已有输出均拒绝。
- 假时钟驱动真实持久化队列验证每日去重、满队列不消费、错过多天只补一轮、DST、配置换钥及多实例 CAS。元数据 PostgreSQL/MySQL 契约仍因未配置隔离测试 DSN 而跳过。
- 临时 UI 服务已关闭，未提交、推送或部署，未改动线上服务。此记录不构成真实供应商或生产恢复验收，Issue 保持打开。
