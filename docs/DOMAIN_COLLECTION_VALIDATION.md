# 统一收件验证记录（2026-09-30）

## 已完成

- Go 全量测试：在 apps/api 执行 go test ./... -json -timeout=10m，通过（internal/app 263.453 秒）。随后针对最终的本地路由、SMTP submission、配额锁等修补重新执行 TestDomainCollection、TestSMTP、TestSubmission，全部通过；新增持久化恢复测试亦通过。
- go vet ./... 通过；所有本次修改的 Go 文件通过 gofmt 检查。
- 前端完整 check 已执行：shadcn 检查、1470 条翻译检查、10 个语言测试、全量 ESLint 通过；在 Prettier 阶段被下列未修改文件阻断。之后单独运行 build，TypeScript 和 Vite 构建通过。本次修改的五个前端文件单独 Prettier 检查通过。
- 浏览器使用 Playwright 和合成 API 响应：桌面 1440px、移动端 390px × 简体/繁体/英文配置保存；跨域搜索选择；已显示错误随语言切换；弹窗加载时切换语言；普通用户不显示配置入口。9 项场景均通过，无页面运行时异常。这不是连接真实后端的端到端验证。
- Go 集成测试使用隔离 SQLite、httptest 和合成邮件，覆盖路由不级联/不循环、共同目标与加号地址去重、别名、未知地址优先级、配额独立重试、最终失败、内容清理、附件与 BCC 隐私、原邮箱保留、Open API scopes、目标生命周期保护和审计。持久化恢复测试实际读取生成的 Maildir 文件；SMTP 导入测试模拟邮件服务写入文件，再调用真实导入器。
- 使用部署中的真实 SQLite 映射 SQL 进行查询测试，通过；使用 CI 版本 shfmt v3.10.0 对 configure-db.sh 执行 -d，通过（包含 shell 解析）。git diff --check 通过，OpenAPI JSON 解析通过。

## 未完成与环境限制

- TestExternalDatabaseContract 已执行，但 PostgreSQL/MySQL 子测试均因未配置 LANQIN_TEST_POSTGRES_DSN / LANQIN_TEST_MYSQL_DSN 跳过，不算跨数据库通过。测试中已接入统一收件的外键、幂等任务和真实数据库投递契约，供隔离服务环境运行。
- 本机没有 Docker、Postfix/Dovecot、PostgreSQL/MySQL 客户端与隔离实例，因此未完成真实 SMTP/IMAP 数量、Postfix 失败/重试/隐私验证及 Compose 数据库组合检查。未部署或开启生产规则；未覆盖 deploy/.env。生产验收清单和回滚步骤见 DOMAIN_COLLECTION.md。
- 全局 gofmt -l . 仍列出下列未修改文件；没有为消除检查输出而格式化无关代码。

## 未修改文件的格式检查输出

### 前端 Prettier

- apps/web/src/components/admin-config.ts
- apps/web/src/components/auth-guard.tsx
- apps/web/src/components/auth-states.tsx
- apps/web/src/components/confirm-dialog.tsx
- apps/web/src/components/mail-attachment.tsx
- apps/web/src/components/mail-content.ts
- apps/web/src/components/mail-html-frame.tsx
- apps/web/src/components/mail-rule-utils.ts
- apps/web/src/components/mail-schedule-utils.ts
- apps/web/src/components/protected-layout.tsx
- apps/web/src/components/telegram-settings-card.tsx
- apps/web/src/components/turnstile-box.tsx
- apps/web/src/components/ui/avatar.tsx
- apps/web/src/components/ui/badge.tsx
- apps/web/src/components/ui/button.tsx
- apps/web/src/components/ui/card.tsx
- apps/web/src/components/ui/checkbox.tsx
- apps/web/src/components/ui/dialog.tsx
- apps/web/src/components/ui/dropdown-menu.tsx
- apps/web/src/components/ui/input.tsx
- apps/web/src/components/ui/label.tsx
- apps/web/src/components/ui/password-input.tsx
- apps/web/src/components/ui/resizable.tsx
- apps/web/src/components/ui/scroll-area.tsx
- apps/web/src/components/ui/select.tsx
- apps/web/src/components/ui/separator.tsx
- apps/web/src/components/ui/sheet.tsx
- apps/web/src/components/ui/sidebar.tsx
- apps/web/src/components/ui/skeleton.tsx
- apps/web/src/components/ui/switch.tsx
- apps/web/src/components/ui/table.tsx
- apps/web/src/components/ui/textarea.tsx
- apps/web/src/components/ui/toast.tsx
- apps/web/src/components/ui/toaster.tsx
- apps/web/src/components/ui/tooltip.tsx
- apps/web/src/hooks/use-me.ts
- apps/web/src/hooks/use-mobile.tsx
- apps/web/src/hooks/use-toast.ts
- apps/web/src/index.css
- apps/web/src/lib/api-types.ts
- apps/web/src/lib/display-mode.ts
- apps/web/src/lib/language.tsx
- apps/web/src/lib/theme.ts
- apps/web/src/lib/utils.ts
- apps/web/src/lib/validation.ts
- apps/web/src/main.tsx
- apps/web/src/pages/login.tsx
- apps/web/src/pages/mail.tsx
- apps/web/src/pages/not-found.tsx
- apps/web/src/pages/profile.tsx
- apps/web/src/pages/register.tsx
- apps/web/src/vite-env.d.ts

### Go gofmt

- apps/api/cmd/server/main.go
- apps/api/internal/app/auth_handlers.go
- apps/api/internal/app/config.go
- apps/api/internal/app/delivery_queue_handlers.go
- apps/api/internal/app/delivery_queue_handlers_test.go
- apps/api/internal/app/dialect_sql.go
- apps/api/internal/app/dialect_sql_test.go
- apps/api/internal/app/dns_handlers.go
- apps/api/internal/app/external_imap.go
- apps/api/internal/app/external_schema_test.go
- apps/api/internal/app/hardening_test.go
- apps/api/internal/app/health.go
- apps/api/internal/app/health_test.go
- apps/api/internal/app/imap_metadata.go
- apps/api/internal/app/invite_group_test.go
- apps/api/internal/app/linuxdo_auth.go
- apps/api/internal/app/linuxdo_auth_test.go
- apps/api/internal/app/login_rate_limit.go
- apps/api/internal/app/login_rate_limit_test.go
- apps/api/internal/app/mail_translate.go
- apps/api/internal/app/mail_translate_test.go
- apps/api/internal/app/mailbox_quota_test.go
- apps/api/internal/app/mailbox_sharing.go
- apps/api/internal/app/maildir_health.go
- apps/api/internal/app/maildir_incremental_test.go
- apps/api/internal/app/maildir_ownership_other.go
- apps/api/internal/app/maildir_ownership_unix.go
- apps/api/internal/app/maildir_sync.go
- apps/api/internal/app/message_search.go
- apps/api/internal/app/mime.go
- apps/api/internal/app/open_api_extended.go
- apps/api/internal/app/outbound_guard_test.go
- apps/api/internal/app/permission_group_handlers.go
- apps/api/internal/app/permissions.go
- apps/api/internal/app/personal_handlers.go
- apps/api/internal/app/queue_leases.go
- apps/api/internal/app/queue_leases_test.go
- apps/api/internal/app/registration_invite_handlers.go
- apps/api/internal/app/registration_invite_test.go
- apps/api/internal/app/rule_forward.go
- apps/api/internal/app/send_queue.go
- apps/api/internal/app/session.go
- apps/api/internal/app/session_guard_test.go
- apps/api/internal/app/settings_handlers.go
- apps/api/internal/app/status_webhook.go
- apps/api/internal/app/telegram_notifications.go
- apps/api/internal/app/telegram_notifications_test.go
- apps/api/internal/app/template_handlers.go
- apps/api/internal/app/threading.go
- apps/api/internal/app/threading_test.go
- apps/api/internal/app/turnstile.go
- apps/api/internal/app/two_factor.go
- apps/api/internal/app/types.go
- apps/api/internal/app/util.go
