# LanQin Email

LanQin Email 是一个自建邮箱 Webmail MVP：React/Vite + shadcn 风格组件前端，Go + SQLite 后端，默认使用单容器集成 Postfix、Dovecot、Rspamd。

## 快速开发

### 后端

```bash
cd apps/api
go mod tidy
go test ./...
go run ./cmd/server
```

默认管理员：

- 邮箱：`admin@lanqin.local`
- 密码：`ChangeMe123!`

生产环境请通过 `LANQIN_ADMIN_PASSWORD` 覆盖。

### 前端

```bash
cd apps/web
npm install
npm run dev
```

前端默认代理 `/api` 到 `http://localhost:8080`。

### Web UI 规则

`apps/web` 的业务页面和业务组件必须使用官方 shadcn/ui 组件源码。新增 UI primitive 前先执行：

```bash
cd apps/web
npx shadcn@latest add <component>
npm run check:shadcn
```

详细规则见 `apps/web/SHADCN_RULES.md`。`npm run check:shadcn` 是提交前的实际检查入口。

## Docker 部署

推荐单容器部署。服务器只需要 `deploy/docker-compose.yml` 和 `.env`，不需要源码构建：

```bash
cd deploy
cp .env.example .env
# 修改 .env 里的域名和管理员密码
docker compose pull
docker compose up -d
```

这个容器内部集成：API、Web、Nginx、Postfix、Dovecot、Rspamd。

如果 GHCR 镜像是私有的，先登录：

```bash
echo "<github_token>" | docker login ghcr.io -u <github_user> --password-stdin
```

本地源码构建：

```bash
cd deploy
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

如需多容器调试版：

```bash
cd deploy
docker compose -f docker-compose.stack.yml -f docker-compose.stack.build.yml up -d --build
```

真实公网收发前需要正确配置 MX/SPF/DKIM/DMARC，并确认云厂商开放 25/587/993 端口。

## V1 能力

- 管理员/普通用户登录
- 多域名、邮箱账号、别名管理
- DNS 记录展示和检测
- Webmail：文件夹、邮件列表、阅读、写信、附件、搜索、已读、星标、移动、删除
- 开发环境本地投递：给系统内邮箱发送会直接写入对方 Inbox，便于无公网邮件栈验证

## 当前收发说明

- 本地开发：系统内邮箱互发可直接使用；未配置 `LANQIN_SMTP_HOST` 时，外部收件人不会真正投递到公网。
- 服务器部署：`deploy/.env.example` 默认使用 `LANQIN_SMTP_HOST=127.0.0.1`，发件会交给同容器内 Postfix。
- 收件同步：Postfix/Dovecot 收到的 Maildir 邮件会由 API 的 Maildir worker 同步到 SQLite 后展示在 Webmail。
- Maildir worker 通过 `LANQIN_MAILDIR_ROOT` 和 `LANQIN_MAILDIR_SCAN_SECONDS` 控制，默认服务器路径为 `/var/mail/vhosts`。


