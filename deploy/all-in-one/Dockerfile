# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS api-build
WORKDIR /src/apps/api
COPY apps/api/go.mod apps/api/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY apps/api ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/lanqin-api ./cmd/server

FROM node:20-bookworm-slim AS web-build
WORKDIR /src
COPY pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/web/package.json apps/web/package.json
RUN corepack enable && corepack prepare pnpm@10.28.2 --activate
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile --filter lanqin-email-web...
COPY apps/web apps/web
RUN pnpm --dir apps/web run build

FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
  --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
  rm -f /etc/apt/apt.conf.d/docker-clean && \
  apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata sqlite3 supervisor nginx \
    postfix postfix-sqlite \
    dovecot-core dovecot-imapd dovecot-pop3d dovecot-lmtpd dovecot-sqlite ssl-cert \
    rspamd

COPY --from=api-build /out/lanqin-api /usr/local/bin/lanqin-api
COPY --from=web-build /src/apps/web/dist /usr/share/nginx/html

COPY deploy/all-in-one/supervisord.conf /etc/supervisor/conf.d/lanqin.conf
COPY deploy/all-in-one/nginx.conf /etc/nginx/sites-enabled/default
COPY deploy/all-in-one/entrypoint.sh /entrypoint.sh

COPY deploy/postfix/main.cf /etc/postfix/main.cf
COPY deploy/postfix/master.cf /etc/postfix/master.cf
COPY deploy/postfix/sqlite-*.cf /etc/postfix/
COPY deploy/dovecot/dovecot.conf /etc/dovecot/dovecot.conf
COPY deploy/dovecot/dovecot-sql.conf.ext /etc/dovecot/dovecot-sql.conf.ext
COPY deploy/rspamd/local.d/ /etc/rspamd/local.d/
COPY deploy/rspamd/sync-dkim.sh /usr/local/bin/lanqin-rspamd-sync-dkim

RUN chmod +x /entrypoint.sh /usr/local/bin/lanqin-rspamd-sync-dkim && mkdir -p /run/nginx /run/rspamd /var/mail/vhosts /data /var/lib/rspamd/dkim

EXPOSE 80 443 25 465 587 993 995
CMD ["/entrypoint.sh"]
