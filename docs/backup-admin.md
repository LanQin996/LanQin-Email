# 管理端备份地点与归档上传（Issue #34）

超级管理员可在后台「备份地点」新增多个 S3 / WebDAV 地点，并进行编辑、启停、删除。每个地点独立配置；相同 endpoint / bucket、不同账户或前缀可以共存。简体中文、繁体中文及英文均使用现有界面翻译系统。

**已接通既有密文归档的后台异步上传。** 管理员可以选择归档与多个地点、查询逐地点结果、取消任务、分页查看历史及下载脱敏报告；不把配置保存或上传成功冒充为新建备份成功。新建在线备份、连接测试、定时计划、恢复切换及 Google Drive OAuth 仍未完成。通用不停服设计见 [在线备份](backup-online.md)。

## 配置主密钥

在 API 进程的受保护环境中设置 `LANQIN_BACKUP_SECRET_KEY`，值必须是 **Base64 编码的 32 字节随机密钥**，例如通过 `openssl rand -base64 32` 生成。不要使用站点密码、S3 Secret 或 age 恢复私钥替代。

- 主密钥只从环境读取，不接受管理端修改、不写入数据库、不通过 API 返回。未配置/格式不正确不会阻止邮件应用启动，但无法新增、编辑或删除地点。
- 全部地点记录（包括地址、ID、名称及凭据）使用标准 AES-256-GCM、独立随机 nonce 和固定用途 AAD 加密，保存在已有 `system_settings` 的 `backup.destinations.v1` 项。没有新增数据表或修改现有表结构。
- 不能只把数据库内的 endpoint 改成另一个地址以复用旧凭据：认证会失败；地址及凭据属于同一加密记录。管理员通过 API 更换地址时也必须重新填写凭据。
- 主密钥与 age 离线恢复私钥用途不同。SQLite 快照内的加密配置不是主密钥副本；整站恢复后要重新使用这些地点凭据，仍需原始 `LANQIN_BACKUP_SECRET_KEY`。在独立可信位置离线保存它，不要连同配置数据库一起暴露。
- **尚无主密钥轮换工具。** 不要直接更换或丢弃已有密钥。错误/缺失密钥或被篡改密文会使已有配置读取失败（`503`），不会清空后悄悄覆盖。需恢复原密钥或另行设计可审计迁移。
- Compose 的 `env_file` 会传递所配置的环境；仅向需要的 API 进程提供该密钥。此变更没有改写 `deploy/.env`、重启服务或触碰生产数据。

## 接口与凭据语义

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

连接参数复用 CLI 校验：HTTPS、普通 S3 bucket、无查询字符串/URL 用户信息、无越界前缀、拒绝私网 IP 字面量。**保存成功不是连接成功。** 配置接口不访问远端；后台上传使用上传引擎的 DNS 固定、公网地址验证、证书校验及不跟随重定向策略。

## 发布归档与异步任务

API 额外配置 `LANQIN_BACKUP_ARTIFACT_DIR` 为预先存在的私有目录（容器内外路径按挂载对应）；发布端需要写权限，API 仅需读权限，可只读挂载。不要放在 Web 静态目录，也不要放入需要递归采集的 data/mail/dkim 源目录，避免把旧备份再次打包。不配置时不启动上传 worker，邮箱功能继续运行。API 从不接收浏览器传入的本地路径，也不直接上传当前运行中的数据库文件。

操作员先用现有 `create` 得到真实的离线加密归档及可信 SHA-256 收据，再使用同一 `LANQIN_BACKUP_SECRET_KEY` 的受控环境执行：

```text
./tmp/lanqin-backup publish --catalog /backups/published --in /backups/sample.age --expected-sha256 <trusted-64-hex-receipt>
```

发布复制并校验实际字节，独占创建随机 ID，先发布密文再发布 HMAC 认证元数据，不覆盖现有文件。目录 HMAC 使用主密钥派生的独立用途密钥；更换主密钥会使原目录认证失败。该检查只证明发布内容与可信收据一致、有 age 格式头；没有恢复私钥时，不能证明密文能解密、包含本应用备份或已经过恢复验证。CLI 产生的真归档另有加解密往返测试。

- 默认每个归档上限 1 GiB；最多选择 16 个地点，最多 16 个待执行任务。worker 每次执行一个父任务，内部最多两个并发上传，每地点最多三次尝试。单任务最多一小时；每次尝试仍受上传引擎的 20 分钟上限约束。
- 入队立即返回 `202`，文件复制、哈希和网络传输都在后台，期间不占用业务数据库连接。凭据仅在执行前按地点解析到内存，不写进进程环境，不复制到任务记录，不返回客户端。
- 任务固定归档收据及地点配置指纹。执行前若地点删除、停用、地址或凭据改变，那个地点失败，其余仍执行；不能让排队任务悄悄上传到修改后的地址。
- 任务和队列使用现有 `system_settings`，按独立用途 AAD 加密并使用 CAS 更新；不新增表。每行保留 MySQL TEXT 容量余量，任务创建大小更严格以预留结果空间。
- 跨实例队列采用 30 秒租约、5 秒续租及 fencing token。失效任务标记 `interrupted`，**不自动重放**，避免重启后重复上传。取消通过数据库标志传播；通常一秒内开始终止请求，不承诺撤销已被远端接受的 PUT。
- 上传成功仅表示远端接受 PUT，未做独立下载/解密恢复演练。一个地点失败时父任务为部分成功，不更新失败地点的最近成功时间。重试使用不同随机对象名，断线或取消可能留下远端孤立对象；当前不自动删除。
- 历史每页 20 条，当前不自动清理历史；管理员应监控数据库、密文目录及上传暂存空间容量。目录扫描上限 1000 个条目，超限报错而不是静默截断；当前没有保留策略。禁止将目录和明文暂存区置于 Web 静态目录。

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
