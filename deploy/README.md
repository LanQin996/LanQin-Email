# LanQin Email Docker 部署说明

## 最简单部署：单容器镜像版

服务器上不需要源码构建，只要 `docker-compose.yml` 和 `.env` 即可。

```bash
cd deploy
cp .env.example .env
# 修改 LANQIN_PUBLIC_HOSTNAME / LANQIN_PUBLIC_BASE_URL / LANQIN_ADMIN_EMAIL / LANQIN_ADMIN_PASSWORD
docker compose pull
docker compose up -d
```

也可以使用脚本：

```bash
cd deploy
bash install.sh
```

第一次执行会生成 `.env` 并提示你修改配置；修改完成后再次执行 `bash install.sh`。

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
- Rspamd

常用命令：

```bash
# 查看日志
docker compose logs -f lanqin-email

# 更新镜像并重启
docker compose pull
docker compose up -d

# 停止
docker compose down
```

## GHCR 镜像权限

默认镜像：

```text
ghcr.io/lanqin996/lanqin-email:latest
ghcr.io/lanqin996/lanqin-email-api:latest
ghcr.io/lanqin996/lanqin-email-web:latest
ghcr.io/lanqin996/lanqin-email-postfix:latest
ghcr.io/lanqin996/lanqin-email-dovecot:latest
ghcr.io/lanqin996/lanqin-email-rspamd:latest
```

如果拉取时报：

```text
unauthorized
```

说明 GHCR Package 还是私有，二选一：

1. 到 GitHub Packages 把镜像改成 Public。
2. 在服务器登录 GHCR：

```bash
echo "<github_token>" | docker login ghcr.io -u <github_user> --password-stdin
```

## 本地源码构建

如果你是在完整源码仓库里本机构建，使用 build override：

```bash
cd deploy
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

这样会使用 `deploy/all-in-one/Dockerfile` 构建单容器镜像。

## 可选：多容器调试部署

如果需要分别查看 Postfix / Dovecot / Rspamd 日志，可以使用 stack 编排。

拉取镜像版：

```bash
cd deploy
docker compose -f docker-compose.stack.yml up -d
```

源码构建版：

```bash
cd deploy
docker compose -f docker-compose.stack.yml -f docker-compose.stack.build.yml up -d --build
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
- 第三方客户端可使用 IMAP SSL `993`、POP3 SSL `995`、SMTP SSL `465` 或 Submission `587`。
- Rspamd 通过 milter 接入 Postfix，负责 DKIM 签名和垃圾邮件标记。
- Rspamd 会周期性从 SQLite 导出域名 DKIM 私钥到容器内 `/var/lib/rspamd/dkim`。
- Go API 是 Webmail 和管理后台入口；浏览器不直接连接 SMTP/IMAP/POP3。
- Go API 会读取 `LANQIN_MAILDIR_ROOT=/var/mail/vhosts`，周期扫描 Maildir，把 Postfix/Dovecot 入站邮件同步成 Webmail 索引。
- 第三方客户端可通过 LanQin API 提供的 SMTP `465/587` 发信；Webmail/API 和第三方客户端的“已发送”都由 API 写入，外发投递进入发送队列并由 API worker relay/retry，客户端后续 IMAP APPEND 到 Sent 会按 `Message-ID` 去重。
- 用户可在个人邮箱管理中接入外部 IMAP 账号；本地存储模式会同步到 LanQin，远端直连模式每次从远端读取。启用前必须配置 `LANQIN_EXTERNAL_IMAP_SECRET_KEY`，默认不允许连接 localhost / 内网 / link-local IMAP 主机。Gmail / Outlook OAuth2 需要在对应控制台配置回调地址：`/api/external-imap-oauth/gmail/callback` 或 `/api/external-imap-oauth/outlook/callback`。
- send-as v1 支持本人邮箱、启用的别名转发 source 指向本人邮箱，或数据库表 `send_as_grants` 中显式授权的地址。

## 邮件客户端 TLS 证书

Web 站点可以由宿主机 Nginx / 宝塔反代到容器 `80`，但 SMTP/IMAP/POP3 端口不会使用 Web 反代的证书。
如果第三方客户端连接 `993/995` 时提示证书是 `localhost`，说明 Dovecot 仍在使用容器自带的测试证书。LanQin API 的 SMTP `465/587` submission 不会使用自签测试证书；启用前必须配置可读的真实证书。

生产环境请把域名证书挂载进容器，并在 `.env` 指向证书文件：

```env
LANQIN_TLS_CERT_FILE=/certs/fullchain.pem
LANQIN_TLS_KEY_FILE=/certs/privkey.pem
LANQIN_SUBMISSION_ADDR=:587
LANQIN_SUBMISSION_TLS_ADDR=:465
```

单容器示例：

```yaml
services:
  lanqin-email:
    volumes:
      - ./data:/data
      - ./mail:/var/mail/vhosts
      - ./dkim:/var/lib/rspamd/dkim
      - /etc/letsencrypt:/etc/letsencrypt:ro
```

证书域名必须覆盖 `LANQIN_PUBLIC_HOSTNAME`。更新后执行：

```bash
docker compose up -d --force-recreate
```

## SMTP 发信排查

单容器部署时，Webmail 发信默认提交给同容器内的 Postfix：

```env
LANQIN_SMTP_HOST=127.0.0.1
LANQIN_SMTP_PORT=25
LANQIN_SMTP_REQUIRE_TLS=false
```

Split stack 使用 `docker-compose.stack.yml` 时，API 容器默认会把 `LANQIN_SMTP_HOST` 覆盖为 `postfix`，让 Webmail 和 SMTP 提交都 relay 到 Postfix service。只有改用外部 SMTP 时才需要在 `.env` 明确填写 `LANQIN_STACK_SMTP_HOST` / `LANQIN_STACK_SMTP_PORT`。

如果发送队列里出现 relay 失败，通常是 Postfix 会话被中断或外部 SMTP 配置错误。优先检查：

```bash
docker compose exec lanqin-email supervisorctl status
docker compose exec lanqin-email postconf -M smtp/inet
# SMTP 提交 465/587 由 LanQin API 提供，不再由 Postfix 监听。
docker compose exec lanqin-email sqlite3 /data/lanqin.db "select key,value from system_settings where key like 'smtp%' order by key;"
docker compose exec lanqin-email sqlite3 /data/lanqin.db "select status,attempt_count,last_error from send_queue order by created_at desc limit 10;"
docker compose logs --tail=200 lanqin-email
```

确认后台“系统设置”里没有把本机 Postfix 的 `SMTP Require TLS` 打开；本机 `127.0.0.1:25` 必须保持 TLS=false。

## 生产注意

- 建议在服务器或边缘网关配置 HTTPS。
- 云厂商通常默认封禁 25 端口，需要单独申请解封。
- SQLite 适合 V1 单机部署；多节点部署前迁移到 PostgreSQL，并把 Postfix/Dovecot maps 改为 PostgreSQL。

