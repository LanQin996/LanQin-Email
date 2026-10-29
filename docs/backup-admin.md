# 管理端备份地点（Issue #34，配置阶段）

超级管理员可在后台「备份地点」新增多个 S3 / WebDAV 地点，并进行编辑、启停、删除。每个地点独立配置；相同 endpoint / bucket、不同账户或前缀可以共存。简体中文、繁体中文及英文均使用现有界面翻译系统。

**当前只是管理端配置闭环。** 它还没有连接 [CLI 上传引擎](backup-storage.md)，不能在页面发起备份、测试云连接、查看成功历史、设置定时计划或恢复；Google Drive OAuth 也未实现。页面明确展示这一边界，不制造「最近成功」或「已开启计划」的状态。

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

连接参数复用 CLI 校验：HTTPS、普通 S3 bucket、无查询字符串/URL 用户信息、无越界前缀、拒绝私网 IP 字面量。**保存成功不是连接成功。** 当前保存接口不访问远端；实际联网时仍须使用上传引擎的 DNS 固定、公网地址验证、证书校验及不跟随重定向策略。

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
