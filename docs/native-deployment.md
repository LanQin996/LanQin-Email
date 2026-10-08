# 非 Docker 完整邮件服务部署（Debian 12 / SQLite）

本指南对应 [Issue #15](https://github.com/LanQin996/LanQin-Email/issues/15)，在宿主机运行 Nginx、LanQin API、Postfix、Dovecot 和 Rspamd，由 systemd 管理。入站链路为 Postfix → Rspamd → Dovecot LMTP → Maildir → API 同步；出站链路为 Webmail 或 API SMTP submission → API 队列 → 本机 Postfix → 收件服务器。

适用范围是**全新、专用的 Debian 12 主机和 SQLite 数据库**，配置基于仓库的 Debian bookworm 镜像与 Dovecot 2.3。不要直接覆盖已有邮件服务器、宝塔/Hestia 邮件服务或生产数据库的配置。Ubuntu、Debian 13 / Dovecot 2.4、MySQL/PostgreSQL 需要另行适配；外部数据库不能只修改 API 的 DSN，邮件映射和 DKIM 导出也必须同步修改。

本指南已对照仓库实现核查，尚未在真实 Debian 主机完成端到端验收。完成下文验收后再迁移正式 MX。

## 1. 域名、网络和工具

以下示例使用邮件域名 `example.com`、服务器主机名和网页地址 `mail.example.com`，请替换所有示例域名。需要固定公网 IP、可设置 PTR 的服务器、出入站 TCP 25，以及入站 80、443、465、587、993、995。仅使用 IMAP 时可不开放 995。保留 SSH 管理通道；下面不提供覆盖现有防火墙的命令。

- `mail.example.com` 的 A 记录指向服务器；只有 IPv6 路由、端口和 PTR 都可用时才添加 AAAA。
- 邮件主机记录必须使用 DNS-only，不能通过普通 Cloudflare HTTP 代理承载 SMTP/IMAP。
- 云厂商必须允许出入站 25；仅开放安全组不能解除运营商封禁。
- 建议先使用测试域名。正式域名的 MX 在验收通过后切换。

本文命令使用 **Bash**。软件安装、配置写入和服务管理命令以 root 执行；源码构建以普通用户执行。主机可只安装运行依赖，将同一版本的 API 二进制与 Web 静态文件在其他 Linux 构建机生成后上传。

在 Debian 12 安装依赖时，Postfix 安装向导选择 `Internet Site`，系统邮件名称填写 `mail.example.com`：

```bash
apt-get update
apt-get install -y ca-certificates curl git openssl sqlite3 nginx certbot \
  postfix postfix-sqlite dovecot-core dovecot-imapd dovecot-pop3d \
  dovecot-lmtpd dovecot-sqlite rspamd dnsutils swaks
# 配置完成前停止邮件监听，并在云防火墙限制邮件端口。
systemctl stop postfix dovecot rspamd
```

构建工具版本以所检出的源码为准：当前 `apps/api/go.mod` 要求 Go **1.25.14**，`apps/web/package.json` 使用 pnpm **10.28.2**，锁文件中的 Vite 8 要求 Node **20.19+ 或 22.12+**，建议使用 Node 24 LTS。Debian 默认的 Go/Node 软件包版本可能不足，请通过官方发行渠道安装并核验下载。运行阶段不需要 Go、Node 或 pnpm。

## 2. 构建同一版本的 API 与 Web

普通用户在源码根目录执行，先检出准备部署的发布 tag 或确定的提交；不要把不同版本的 API、Web 和邮件配置混用：

```bash
git clone https://github.com/LanQin996/LanQin-Email.git
cd LanQin-Email
# 首次构建前：git checkout <发布 tag 或提交>
git rev-parse HEAD
go version
node --version
corepack enable
corepack prepare pnpm@10.28.2 --activate
pnpm install --frozen-lockfile --filter lanqin-email-web...
pnpm --dir apps/web run check
mkdir -p build
(
  cd apps/api
  go mod download
  go vet ./...
  go test ./...
  CGO_ENABLED=0 go build -trimpath -o ../../build/lanqin-api ./cmd/server
)
```

前端产物是 `apps/web/dist/`。在 Linux 目标架构上构建 API，或明确设置匹配目标的 `GOOS=linux` 和 `GOARCH=amd64` / `arm64`；不能把 Windows 可执行文件装到 Linux。

后续 root 命令均假设已进入同一份源码根目录。记录 `git rev-parse HEAD`，保留旧产物和配置供升级回滚使用。

## 3. 用户、目录和 SQLite 权限

Dovecot 的 SQL user query 固定使用 UID/GID **5000**，API 写入的 Maildir 文件权限为 `0600`。API 也以 `vmail` 运行，保证 Webmail 与 IMAP 可以读取彼此创建的邮件。先确认 UID/GID 没有被其他账号占用：

```bash
getent passwd 5000 || true
getent group 5000 || true
getent passwd vmail || true
getent group vmail || true
```

全新服务器上应没有以上账号；若有结果，先核实身份，不能继续创建冲突账号或改动其他服务的 UID。确认后执行一次：

```bash
groupadd --gid 5000 vmail
useradd --uid 5000 --gid vmail --system --home-dir /var/mail/vhosts \
  --no-create-home --shell /usr/sbin/nologin vmail
install -d -o vmail -g postfix -m 2770 /data
install -d -o vmail -g vmail -m 0700 /data/attachments
install -d -o vmail -g vmail -m 0700 /var/mail/vhosts
install -d -o root -g root -m 0755 /opt/lanqin/web /etc/lanqin
install -d -o root -g postfix -m 0750 /etc/lanqin/tls
install -o root -g root -m 0755 build/lanqin-api /usr/local/bin/lanqin-api
cp -a apps/web/dist/. /opt/lanqin/web/
chown -R root:root /opt/lanqin/web
find /opt/lanqin/web -type d -exec chmod 0755 {} +
find /opt/lanqin/web -type f -exec chmod 0644 {} +
```

保留 `/data/lanqin.db` 和 `/var/mail/vhosts`，现有 SQLite 映射及 Dovecot 生成脚本使用这些固定路径。SQLite 采用 WAL，Postfix 需要访问数据库及其 `-wal` / `-shm` 文件。Postfix 降权时清除附加组，因此这里使用它的主组 `postfix`，不能依赖 `usermod -aG` 给 Postfix 增加数据库权限。`2770` 目录及 API 的 `UMask=0007` 使数据库文件和 WAL 继承该组；附件目录单独限制为 `0700`。`postfix` 组成员可访问数据库和 TLS 私钥，API 仅通过下文 systemd 单元取得该组权限；不能加入交互用户或无关服务。不要使用 `chmod 777` 或把数据库放入 Web 根目录。

## 4. 获取并安装真实 TLS 证书

确保 A/AAAA 解析正确且公网可访问 80。先停止 Nginx，使用 HTTP-01 获取证书；有其他站点时应改用既有 ACME 客户端或 DNS-01，不能停用已有生产站点：

```bash
systemctl stop nginx
certbot certonly --standalone -d mail.example.com
install -o root -g postfix -m 0644 \
  /etc/letsencrypt/live/mail.example.com/fullchain.pem /etc/lanqin/tls/fullchain.pem
install -o root -g postfix -m 0640 \
  /etc/letsencrypt/live/mail.example.com/privkey.pem /etc/lanqin/tls/privkey.pem
```

API、Postfix 和 Dovecot 使用同一份证书副本；Nginx 也引用该目录。不要全局放宽 `/etc/letsencrypt` 私钥权限。第 9 节配置自动续期和副本更新。

## 5. API 环境变量与 systemd

创建 `/etc/lanqin/lanqin.env`，owner 为 `root:root`、权限 `0600`，填写以下内容。API 只读取进程环境变量，**不会自动加载源码目录或 deploy 下的 `.env`**，这里由 systemd 的 `EnvironmentFile` 注入。文件不是 shell 脚本，不写 `export` 或命令替换；密码和密钥填写实际值。

```ini
LANQIN_ADDR=127.0.0.1:8080
LANQIN_DATA_DIR=/data
LANQIN_DB_DRIVER=sqlite
LANQIN_DB_PATH=/data/lanqin.db
LANQIN_PUBLIC_HOSTNAME=mail.example.com
LANQIN_PUBLIC_BASE_URL=https://mail.example.com
LANQIN_ADMIN_EMAIL=admin@example.com
LANQIN_ADMIN_PASSWORD=REPLACE_WITH_A_UNIQUE_STRONG_PASSWORD
LANQIN_ALLOW_INSECURE_HTTP=false
LANQIN_TRUSTED_PROXY_COUNT=1
LANQIN_OPEN_REGISTRATION=false
LANQIN_SMTP_HOST=127.0.0.1
LANQIN_SMTP_PORT=25
LANQIN_SMTP_REQUIRE_TLS=false
LANQIN_SUBMISSION_ADDR=:587
LANQIN_SUBMISSION_TLS_ADDR=:465
LANQIN_SUBMISSION_MAX_MESSAGE_MB=35
LANQIN_TLS_CERT_FILE=/etc/lanqin/tls/fullchain.pem
LANQIN_TLS_KEY_FILE=/etc/lanqin/tls/privkey.pem
LANQIN_MAILDIR_ROOT=/var/mail/vhosts
LANQIN_MAILDIR_SCAN_SECONDS=5
LANQIN_NOTIFICATION_SECRET_KEY=REPLACE_WITH_A_RANDOM_PERSISTENT_SECRET
LANQIN_EXTERNAL_IMAP_ENABLED=false
```

使用 `openssl rand -hex 32` 生成通知/TOTP 加密主密钥，妥善保存并备份；启用外部 IMAP 时另行生成 `LANQIN_EXTERNAL_IMAP_SECRET_KEY`。不要重复生成已有部署的密钥。管理员初始密码只用于首次建库，后续通过应用修改；已有数据库中的系统设置会覆盖部分环境配置，应同时在管理后台核对 SMTP、Maildir 和站点设置。

创建 `/etc/systemd/system/lanqin-api.service`：

```ini
[Unit]
Description=LanQin Email API and SMTP submission
Wants=network-online.target
After=network-online.target

[Service]
User=vmail
Group=vmail
SupplementaryGroups=postfix
WorkingDirectory=/opt/lanqin
EnvironmentFile=/etc/lanqin/lanqin.env
ExecStart=/usr/local/bin/lanqin-api
Restart=on-failure
RestartSec=5
TimeoutStopSec=30
UMask=0007
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/data /var/mail/vhosts

[Install]
WantedBy=multi-user.target
```

API 只获准绑定低端口，不以 root 运行。8080 只监听回环，Nginx 只代理 `/api/` 和健康检查；`/auth-policy` 不对公网代理，因此本方案不设置 `LANQIN_AUTH_POLICY_SECRET`。代理层数 `1` 对应直接连接的 Nginx；增加其他代理时必须重新核对真实客户端 IP 的信任链。

```bash
chown root:root /etc/lanqin/lanqin.env
chmod 0600 /etc/lanqin/lanqin.env
systemctl daemon-reload
systemctl enable --now lanqin-api
curl --fail --silent --show-error http://127.0.0.1:8080/readyz
stat -c '%U %G %a %n' /data /data/lanqin.db /data/lanqin.db-wal /data/lanqin.db-shm
```

只有 `/readyz` 返回 200 后继续配置邮件服务。数据库文件预期为 `vmail:postfix` 且没有 other 权限；WAL/SHM 可能在进程退出后消失。首次启动会自动迁移表、创建管理员域名、邮箱和 DKIM 材料。若 API 启动失败，用 `journalctl -u lanqin-api -n 100` 定位，不要清空数据库重新尝试。

## 6. Postfix：25 入站和本机投递

在专用新服务器复制同版本配置；下列命令覆盖目标文件：

```bash
install -o root -g root -m 0644 deploy/postfix/main.cf /etc/postfix/main.cf
install -o root -g root -m 0644 deploy/postfix/master.cf /etc/postfix/master.cf
for mapping in deploy/postfix/sqlite-*.cf; do
  install -o root -g postfix -m 0640 "$mapping" "/etc/postfix/$(basename "$mapping")"
done
postconf -e 'myhostname = mail.example.com'
postconf -e 'myorigin = $myhostname'
postconf -e 'mydestination = localhost'
postconf -e 'mynetworks = 127.0.0.0/8 [::1]/128'
postconf -e 'virtual_transport = lmtp:inet:127.0.0.1:24'
postconf -e 'smtpd_milters = inet:127.0.0.1:11332'
postconf -e 'non_smtpd_milters = inet:127.0.0.1:11332'
postconf -e 'smtpd_tls_cert_file = /etc/lanqin/tls/fullchain.pem'
postconf -e 'smtpd_tls_key_file = /etc/lanqin/tls/privkey.pem'
postconf -e 'message_size_limit = 36700160'
postfix check
```

`master.cf` 已将需要数据库和网络连接的进程设为不 chroot，避免 Debian 默认 chroot 环境看不到 `/data`。仅信任回环源地址，不能把公网/VPC 网段加入 `mynetworks`；本机进程属于受信投递方。保留 `reject_unauth_destination`、`recipient_bcc_maps` 和现有地址映射。邮件域名属于虚拟域，不能添加到 `mydestination`。

**Postfix 不监听 465/587，也不负责 SMTP AUTH**；这些端口由 API 完成认证、发件身份和配额检查后进入队列。Webmail relay 到本机 25 时 `SMTP Require TLS=false`，这是受限的回环连接，不是允许公网客户端明文认证。

## 7. Dovecot：IMAPS、POP3S 和本机 LMTP

```bash
install -o root -g root -m 0644 deploy/dovecot/dovecot.conf /etc/dovecot/dovecot.conf
LANQIN_DB_DRIVER=sqlite sh deploy/dovecot/configure-db.sh
policy_nonce=$(openssl rand -hex 32)
sed -i "s/__LANQIN_AUTH_POLICY_HASH_NONCE__/$policy_nonce/" /etc/dovecot/dovecot.conf
unset policy_nonce
```

生成脚本将 SQL 配置写入 `/etc/dovecot/dovecot-sql.conf.ext`，保留 `0600` 和 root ownership。它读取同一 `/data/lanqin.db`，返回 UID/GID 5000 及数据库中的邮箱配额。认证策略调用 API 以检查停用用户和协议限制，不要删除该调用。

编辑 `/etc/dovecot/dovecot.conf`，完成以下调整：

```text
# 替换原有 ssl / ssl_cert / ssl_key 值。
ssl = required
ssl_cert = </etc/lanqin/tls/fullchain.pem
ssl_key = </etc/lanqin/tls/privkey.pem
disable_plaintext_auth = yes
# 替换原有 /dev/stdout、/dev/stderr，使用宿主机日志。
log_path = syslog
info_log_path = syslog
# 加入策略失败拒绝；保留原有 URL、请求属性和随机 nonce。
auth_policy_reject_on_fail = yes
auth_policy_log_only = no
```

用下面的块替换已有 `service imap-login`、`service pop3-login` 和 `service lmtp`，关闭明文端口、保留 993/995 并限制 LMTP 到回环：

```text
service imap-login {
  inet_listener imap {
    port = 0
  }
  inet_listener imaps {
    port = 993
    ssl = yes
  }
}
service pop3-login {
  inet_listener pop3 {
    port = 0
  }
  inet_listener pop3s {
    port = 995
    ssl = yes
  }
}
service lmtp {
  inet_listener lmtp {
    address = 127.0.0.1
    port = 24
  }
}
```

删除 `service auth` 中的 `inet_listener postfix-auth` 子块（12345）；Dovecot 默认内部认证服务仍保留，API submission 不使用这个外部 socket。SQL auth-worker 保持 Debian/Dovecot 默认 root 身份以读取 `0600` 配置和数据库，不要改成 `vmail` 或对外暴露认证接口。

```bash
doveconf -n
```

确认没有配置错误、IMAP/POP3 明文监听、公网 LMTP 或 12345 监听。Dovecot 配置覆盖了发行版的顶层文件，原来的 `conf.d` 配置不会自动包含，不应重复追加其中的 SQL/userdb 设置。

## 8. Rspamd 和 DKIM 导出

```bash
install -d -o _rspamd -g _rspamd -m 0750 /var/lib/rspamd/dkim
install -o root -g root -m 0644 deploy/rspamd/local.d/dkim_signing.conf /etc/rspamd/local.d/dkim_signing.conf
install -o root -g root -m 0644 deploy/rspamd/local.d/actions.conf /etc/rspamd/local.d/actions.conf
install -o root -g root -m 0644 deploy/rspamd/local.d/worker-proxy.inc /etc/rspamd/local.d/worker-proxy.inc
install -o root -g root -m 0755 deploy/rspamd/sync-dkim.sh /usr/local/bin/lanqin-rspamd-sync-dkim
```

Debian 12 软件包使用 `_rspamd`；先用 `id _rspamd` 确认。编辑 `/etc/rspamd/local.d/worker-proxy.inc`，将 `bind_socket` 改为 `"127.0.0.1:11332"`。创建 `/etc/rspamd/local.d/worker-normal.inc`，内容为 `bind_socket = "127.0.0.1:11333";`；创建 `/etc/rspamd/local.d/worker-controller.inc`，内容为 `bind_socket = "127.0.0.1:11334";`。这些端口不开放到公网。

创建 `/etc/rspamd/local.d/sign_networks.map`，仅包含两行 `127.0.0.0/8` 和 `::1`，不要复用容器示例中宽泛的私网信任网段。现有 `actions.conf` 以垃圾邮件标记和 DKIM 签名为主，不直接拒收垃圾邮件；需要更严格策略时应独立测试后配置。

创建 `/etc/systemd/system/lanqin-dkim-sync.service`：

```ini
[Unit]
Description=Export LanQin DKIM keys for Rspamd
After=lanqin-api.service

[Service]
Type=oneshot
User=root
Environment=LANQIN_DB_DRIVER=sqlite
Environment=LANQIN_DB_PATH=/data/lanqin.db
ExecStart=/usr/local/bin/lanqin-rspamd-sync-dkim --once
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/data /var/lib/rspamd/dkim
```

导出程序需要读取 SQLite WAL，并把私钥属主改为 `_rspamd`，该独立任务以 root 执行，不向它注入 API 的密码或加密密钥。

创建 `/etc/systemd/system/lanqin-dkim-sync.timer`：

```ini
[Unit]
Description=Periodically export LanQin DKIM keys

[Timer]
OnBootSec=30s
OnUnitActiveSec=60s
Unit=lanqin-dkim-sync.service

[Install]
WantedBy=timers.target
```

```bash
systemctl daemon-reload
systemctl start lanqin-dkim-sync.service
systemctl enable --now lanqin-dkim-sync.timer
rspamadm configtest
systemctl enable --now rspamd dovecot postfix
```

应在 API 建库并返回 ready 后首次导出，再启动邮件服务。新增域名后等一次定时导出或手动启动该 service；确认 key 文件存在、owner 为 `_rspamd`、权限为 `0640`，不要输出私钥内容。定时任务失败查看 journal，不能只依据 timer 为 active 判定 DKIM 正常。

## 9. Nginx、HTTPS 和证书续期

在专用新服务器将 `deploy/all-in-one/nginx.conf` 安装为 `/etc/nginx/sites-available/lanqin`，保留其 CSP、安全头、`/api/` 代理、健康检查和 SPA `try_files`。调整主 server：

- 将 `listen 80;` 替换为 `listen 443 ssl;`，`server_name` 替换为 `mail.example.com`，`root` 替换为 `/opt/lanqin/web`。
- 添加 `ssl_certificate /etc/lanqin/tls/fullchain.pem;`、`ssl_certificate_key /etc/lanqin/tls/privkey.pem;`、`ssl_protocols TLSv1.2 TLSv1.3;`。
- 保留清空 `True-Client-IP` 的设置。直接面向公网的 Nginx 将 `X-Forwarded-For` 设置为 `$remote_addr`；`X-Real-IP` 仍为 `$remote_addr`，`X-Forwarded-Proto` 仍为 `$scheme`。
- 在 `/api/` 内添加 `proxy_buffering off;` 和 `proxy_read_timeout 300s;`，支持邮件事件流和较慢请求。

另加 HTTP server，提供 ACME webroot 和 HTTPS 跳转：

```nginx
server {
  listen 80;
  server_name mail.example.com;
  location ^~ /.well-known/acme-challenge/ {
    root /var/www/letsencrypt;
    try_files $uri =404;
  }
  location / {
    return 301 https://mail.example.com$request_uri;
  }
}
```

```bash
install -d -o root -g root -m 0755 /var/www/letsencrypt
ln -s /etc/nginx/sites-available/lanqin /etc/nginx/sites-enabled/lanqin
# 仅在此专用新服务器禁用发行版默认站点。
unlink /etc/nginx/sites-enabled/default
nginx -t
systemctl enable --now nginx
```

不使用仅托管静态文件的 `deploy/nginx/web.conf`：该文件没有 API 反向代理。也不能把 Vite 开发服务器用作生产 Web 服务。

初次证书使用 standalone，切换续期到 webroot，先演练：

```bash
certbot reconfigure --cert-name mail.example.com --webroot -w /var/www/letsencrypt
certbot renew --dry-run
```

Debian 的 Certbot 版本若不支持 `reconfigure`，使用 `certbot certonly --webroot -w /var/www/letsencrypt -d mail.example.com --cert-name mail.example.com --force-renewal` 完成一次更新并保存认证方式；避免反复强制签发触发 CA 限额。`dry-run` 不会默认执行 deploy hook，不能据此认定证书副本已更新。

创建可执行的 `/etc/letsencrypt/renewal-hooks/deploy/lanqin.sh`（root owner、`0700`）：

```bash
#!/bin/sh
set -eu
[ "${RENEWED_LINEAGE:-}" = /etc/letsencrypt/live/mail.example.com ] || exit 0
install -o root -g postfix -m 0644 "$RENEWED_LINEAGE/fullchain.pem" /etc/lanqin/tls/fullchain.pem.new
install -o root -g postfix -m 0640 "$RENEWED_LINEAGE/privkey.pem" /etc/lanqin/tls/privkey.pem.new
mv /etc/lanqin/tls/fullchain.pem.new /etc/lanqin/tls/fullchain.pem
mv /etc/lanqin/tls/privkey.pem.new /etc/lanqin/tls/privkey.pem
nginx -t
systemctl reload nginx
systemctl reload postfix
systemctl reload dovecot
```

API 在新 TLS 握手时重新读取证书，无需为了证书续期重启。用以下命令实际执行一次 hook，随后比对公网 443/465/587/993/995 的证书，而不是只检查磁盘文件：

```bash
RENEWED_LINEAGE=/etc/letsencrypt/live/mail.example.com \
  /etc/letsencrypt/renewal-hooks/deploy/lanqin.sh
systemctl enable --now certbot.timer
```

## 10. DNS 与完整验收

登录 `https://mail.example.com`，核对管理员账号、站点设置及 SMTP `127.0.0.1:25`、TLS=false、Maildir `/var/mail/vhosts`。创建合成测试邮箱 `alice@example.com` 和 `bob@example.com`；新增域名时确认 DKIM 导出成功。配置以下记录，值以后台显示的实际密钥和 IP 为准：

| 记录 | 示例或要求 |
| --- | --- |
| `example.com MX` | `10 mail.example.com.` |
| `mail.example.com A/AAAA` | 实际可用公网地址 |
| IP 的 PTR | `mail.example.com`，由服务器供应商设置，并能正向解析回该 IP |
| `example.com TXT` SPF | 仅本机发信时可使用 `v=spf1 mx -all`；已有发信服务需合并为一个 SPF 记录 |
| `mail.example.com TXT` HELO SPF | 按后台建议填写实际 IPv4/IPv6，不能保留示例 IP |
| `lanqin._domainkey.example.com TXT` | 后台生成的 DKIM 公钥，不能发布私钥 |
| `_dmarc.example.com TXT` | 先监控并确认 SPF/DKIM 对齐，再逐步收紧到 quarantine/reject |

检查服务及监听范围：

```bash
systemctl --no-pager --full status lanqin-api postfix dovecot rspamd lanqin-dkim-sync.timer
curl --fail --silent --show-error https://mail.example.com/readyz
ss -lntp
postconf -n
doveconf -n
rspamadm configtest
postmap -q example.com sqlite:/etc/postfix/sqlite-domains.cf
postmap -q alice@example.com sqlite:/etc/postfix/sqlite-mailboxes.cf
runuser -u postfix -- postmap -q example.com sqlite:/etc/postfix/sqlite-domains.cf
```

8080、24、11332/11333/11334 必须只监听回环；143、110、12345 不应监听。443/465/587/993/995 使用匹配域名的有效证书，25 可 STARTTLS；不要开放 Rspamd 控制台、数据库或 `/auth-policy`。验证策略 URL 在公网不返回 API JSON。

从**另一台非受信公网主机**验证 25 不能把第三方发件域转发给第三方收件域，不发送 DATA：

```bash
swaks --server mail.example.com --port 25 \
  --from sender@example.net --to recipient@example.org --quit-after RCPT
```

RCPT 必须被拒绝（例如 Relay access denied）；不能在同服务器或回环上做此检查，因为回环是允许 relay 的来源。再检查 465/587 未认证提交被拒绝、错误密码被拒绝、正常用户不能冒用其他邮箱发件、停用用户不能通过 SMTP/IMAP 登录。

用真实外部邮箱和合成正文测试完整链路：

1. 外部发到 `alice@example.com`：Postfix 接收、Dovecot LMTP 投递、Maildir 文件出现、Webmail 同步显示；使用 IMAPS 993 再读取同一封邮件。
2. Webmail 发到外部：API 队列 relay 成功、Postfix 队列最终处理完成、外部实际收到且 SPF/DKIM/DMARC 通过。API `relayed` 仅表示 Postfix 接受，不代表最终送达。
3. 邮件客户端经 465（隐式 TLS）和 587（STARTTLS）认证发信，确认已发送在 Webmail/IMAP 可见；如启用 POP3，再检查 995。
4. 同域、跨域、地址别名、加号地址、域名收集、配额及附件测试；确认未知地址按设置拒绝或兜底，BCC 目标没有出现在 To/Cc。共享邮箱和外部邮箱另按实际启用功能验证。
5. 在 Webmail 和 IMAP 分别标记已读、移动、删除，并检查另一端同步；确认 vmail 可以读取两端创建的 Maildir 文件。
6. 重启主机后重复健康、监听、收发和 DKIM 检查，并验证 ACME 续期 hook。

常用排查命令为 `journalctl -u lanqin-api -u dovecot -u rspamd -u lanqin-dkim-sync -n 100`、`journalctl --since '10 minutes ago' | grep postfix` 和 `postqueue -p`。根据主机日志配置，Postfix/Dovecot 也可能写入 `/var/log/mail.log`；不要把真实邮件、账号密码、DSN、密钥或完整数据库查询结果复制到公开 Issue。

## 11. 备份、升级与回滚

备份必须同时包括 `/data`（数据库及附件）、`/var/mail/vhosts`、`/var/lib/rspamd/dkim`、`/etc/lanqin`（加密主密钥和 TLS 副本）、ACME 配置、邮件/Nginx/systemd 配置以及 Postfix 未投递队列 `/var/spool/postfix`。备份含私密数据，限制访问并加密异地保存；制定保留策略并演练恢复。

为了得到数据库、附件和 Maildir 一致的快照，在维护窗口先停止接受新请求和投递，再停 API、Dovecot、Rspamd 及 DKIM timer；可从以下顺序开始安排：

```bash
systemctl stop nginx postfix lanqin-dkim-sync.timer lanqin-dkim-sync.service
systemctl stop lanqin-api dovecot rspamd
```

确认服务已退出后再做文件系统快照/备份，包括可能尚存的 SQLite WAL/SHM；不能在运行中只复制 `lanqin.db`。不停机的数据库备份需使用 SQLite backup API，并另行协调附件/Maildir 快照。恢复或维护完成后先启动 API，等待 `/readyz`，导出 DKIM，再启动 Rspamd、Dovecot、Postfix、Nginx 和 timer。

升级前记录旧提交并备份完整状态，在隔离环境构建和验证新版本；维护窗口安装同版本 API/Web/邮件配置，不运行容器 entrypoint，也不在宿主机运行 `supervisord`。API 启动会自动迁移数据库，**不能假设换回旧二进制即可回滚 schema**。需要退回时恢复配套数据库、Maildir、附件、密钥和配置的快照；明确快照后的新邮件与排队邮件如何保留/重放，避免丢信或重复发送。上线前确认回滚窗口和数据保留方案。
