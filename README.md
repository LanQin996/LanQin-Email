# LanQin Email

![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)
![React](https://img.shields.io/badge/React-18.3-61DAFB?logo=react)
![SQLite](https://img.shields.io/badge/SQLite-003B57?logo=sqlite)
![Docker](https://img.shields.io/badge/Docker-2496ED?logo=docker)
![Postfix](https://img.shields.io/badge/Postfix-5E3C2B?logo=maildotru)
![Rspamd](https://img.shields.io/badge/Rspamd-FFD045)

自建邮箱 Webmail 全栈方案。前端 React + shadcn/ui，后端 Go + SQLite，默认单容器集成 Postfix、Dovecot、Rspamd。

## 特性

- **Webmail 客户端** — 文件夹管理、邮件读写、附件、搜索、标签、星标、规则过滤
- **多域名/多邮箱** — 域名管理、DKIM 签名、DNS 记录检测、邮箱别名转发
- **双因素认证** — TOTP 两步登录（兼容 Google Authenticator / Authy）
- **管理员面板** — 用户/域名/邮箱/别名/邮件管理、系统设置、邮件模板
- **用户自助** — 开放注册、自助申请邮箱、黑名单、收件规则、联系人
- **单容器部署** — 一个容器跑通 API + Web + Nginx + Postfix + Dovecot + Rspamd
- **本地投递** — 开发环境系统内邮箱互发直接写入 Inbox，无需公网邮件栈

## 快速开始

### 开发

```bash
# 后端
cd apps/api
go mod tidy
go test ./...
go run ./cmd/server

# 前端（新终端）
cd apps/web
pnpm install
pnpm run dev
```

默认管理员：`admin@lanqin.local`，密码通过 `LANQIN_ADMIN_PASSWORD` 设置（不设置则启动时随机生成并输出到日志）。

### 部署

服务器只需要 Compose 文件和配置，不需要源码构建：

```bash
cd deploy
cp .env.example .env
# 修改 .env 里的域名和管理员密码
docker compose pull
docker compose up -d
```

单容器内部集成：API、Web、Nginx、Postfix、Dovecot、Rspamd。

## 架构

```
┌─────────────────────────────────────────────────┐
│                  Docker 容器                       │
│  ┌──────┐  ┌────────┐  ┌──────┐  ┌──────────┐  │
│  │ API  │  │  Web   │  │Nginx │  │ Postfix  │  │
│  │ Go   │  │ React  │  │反代  │  │  MTA     │  │
│  └──┬───┘  └────────┘  └──────┘  └────┬─────┘  │
│     │ SQLite                  Maildir  │        │
│     └──────────────────────────────────┘        │
│  ┌────────┐  ┌──────────┐                      │
│  │Dovecot │  │ Rspamd   │                      │
│  │  IMAP  │  │ 反垃圾   │                      │
│  └────────┘  └──────────┘                      │
└─────────────────────────────────────────────────┘
```

数据流：
1. **收件** → Postfix 接收 → Dovecot 写入 Maildir → API worker 同步到 SQLite → Webmail 展示
2. **发件** → Webmail 编辑 → API 构造 MIME → Postfix 投递
3. **反垃圾** → Rspamd 在 Postfix 投递前评分，标记 Spam 文件夹

## 能力

| 模块 | 功能 |
|------|------|
| 认证 | 登录/注册、会话管理、双因素 TOTP、Turnstile 人机验证 |
| 域名 | 多域名管理、DKIM 密钥生成、DNS 记录展示与检测 |
| 邮箱 | 邮箱账号管理、容量配额、密码同步 |
| Webmail | 文件夹、邮件列表、阅读、写信、附件、搜索、已读/未读、星标、移动、删除、标签 |
| 规则 | 收件规则（条件+动作）、发件人黑名单 |
| 联系人 | 个人通讯录管理 |
| 管理 | 用户/域名/邮箱/别名 CRUD、系统设置持久化、邮件模板编辑、SMTP 测试 |
| 清理 | 归档已读、清空回收站/垃圾邮件 |

## 收发说明

- **开发环境**：系统内邮箱互发直接投递到对方 Inbox。未配置 `LANQIN_SMTP_HOST` 时外部收件人不会真正投递。
- **服务器部署**：`.env` 默认 `LANQIN_SMTP_HOST=127.0.0.1`，发件交给同容器内 Postfix。
- **收件同步**：Postfix/Dovecot 收到 Maildir 后，API 的 Maildir worker 同步到 SQLite 后展示。
- **公网收发**：需要正确配置 MX/SPF/DKIM/DMARC，并确认云厂商开放 25/587/993 端口。

## 要求

- Go 1.22+
- Node.js 20+
- Docker & Docker Compose（部署）

## License

[MIT](./LICENSE)

