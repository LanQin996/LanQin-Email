# LanQin Email Docker 部署说明

## 默认：单容器部署

```bash
cd deploy
cp .env.example .env
# 修改 LANQIN_PUBLIC_HOSTNAME / LANQIN_ADMIN_EMAIL / LANQIN_ADMIN_PASSWORD
docker compose up -d --build
```

默认只启动一个业务容器：

```text
lanqin-email
```

容器内部包含：

- Go API
- Web 静态站点
- Nginx
- Postfix
- Dovecot
- OpenDKIM

常用日志：

```bash
docker compose logs -f lanqin-email
```

## 使用 GitHub Actions 构建好的镜像

仓库已添加 `.github/workflows/docker.yml`：

- pull request：检查前端 shadcn 规则、前端构建、后端测试。
- push 到 `main` / `master`：检查通过后发布 Docker 镜像到 GHCR。
- push tag，例如 `v1.0.0`：发布对应 tag 镜像。

默认发布镜像：

```text
ghcr.io/lanqin996/lanqin-email:latest
ghcr.io/lanqin996/lanqin-email-api:latest
ghcr.io/lanqin996/lanqin-email-web:latest
ghcr.io/lanqin996/lanqin-email-postfix:latest
ghcr.io/lanqin996/lanqin-email-dovecot:latest
ghcr.io/lanqin996/lanqin-email-opendkim:latest
```

单容器部署时，确认 `.env` 里的 `LANQIN_IMAGE` 是你的 GHCR 镜像：

```env
LANQIN_IMAGE=ghcr.io/lanqin996/lanqin-email:latest
```

然后在服务器执行：

```bash
cd deploy
docker compose pull
docker compose up -d
```

如果镜像是私有的，服务器需要先登录 GHCR：

```bash
echo "<github_token>" | docker login ghcr.io -u <github_user> --password-stdin
```

## 可选：多容器调试部署

如果需要分别查看 Postfix / Dovecot / OpenDKIM 日志，可以使用保留的 stack 编排：

```bash
cd deploy
docker compose -f docker-compose.stack.yml up -d --build
```

## DNS

进入 Web 管理后台后，在域名管理中查看每个域名需要配置的：

- MX
- SPF TXT
- DKIM TXT
- DMARC TXT

配置完成后点击“检测”。

## 邮件服务边界

- Postfix 读取 `/data/lanqin.db` 中的 `domains`、`mailboxes`、`aliases`。
- Dovecot 读取同一个 SQLite 数据库进行邮箱认证，并使用 `/var/mail/vhosts` 作为 Maildir 根目录。
- OpenDKIM 启动时从 SQLite 导出域名 DKIM 私钥到容器内 `/etc/opendkim/keys`。
- Go API 是 Webmail 和管理后台唯一入口；浏览器不直接连接 SMTP/IMAP。
- Go API 会读取 `LANQIN_MAILDIR_ROOT=/var/mail/vhosts`，周期扫描 Maildir，把 Postfix/Dovecot 入站邮件同步成 Webmail 索引。

## 生产注意

- 建议在服务器或边缘网关配置 HTTPS。
- 云厂商通常默认封禁 25 端口，需要单独申请解封。
- SQLite 适合 V1 单机部署；多节点部署前迁移到 PostgreSQL，并把 Postfix/Dovecot maps 改为 PostgreSQL。

