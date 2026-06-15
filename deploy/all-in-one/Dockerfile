# syntax=docker/dockerfile:1.7

FROM golang:1.22-bookworm AS api-build
WORKDIR /src/apps/api
COPY apps/api/go.mod apps/api/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY apps/api ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/lanqin-api ./cmd/server

FROM node:20-bookworm-slim AS web-build
WORKDIR /src/apps/web
COPY apps/web/package.json apps/web/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --prefer-offline --no-audit || npm install --prefer-offline --no-audit
COPY apps/web ./
RUN npm run build

FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
  --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
  rm -f /etc/apt/apt.conf.d/docker-clean && \
  apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata sqlite3 supervisor nginx \
    postfix postfix-sqlite \
    dovecot-core dovecot-imapd dovecot-lmtpd dovecot-sqlite ssl-cert \
    opendkim opendkim-tools

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
COPY deploy/opendkim/opendkim.conf /etc/opendkim.conf
COPY deploy/opendkim/TrustedHosts /etc/opendkim/TrustedHosts

RUN chmod +x /entrypoint.sh && mkdir -p /run/nginx /var/mail/vhosts /data /etc/opendkim/keys

EXPOSE 80 443 25 587 993
CMD ["/entrypoint.sh"]
