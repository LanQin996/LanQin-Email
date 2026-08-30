# syntax=docker/dockerfile:1.7

FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && apt-get install -y --no-install-recommends postfix postfix-sqlite ca-certificates rsyslog ssl-cert
COPY main.cf /etc/postfix/main.cf
COPY master.cf /etc/postfix/master.cf
COPY sqlite-*.cf /etc/postfix/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 25
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=6 \
  CMD postfix status >/dev/null 2>&1 || exit 1
CMD ["/entrypoint.sh"]
