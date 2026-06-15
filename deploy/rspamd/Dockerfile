# syntax=docker/dockerfile:1.7

FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && apt-get install -y --no-install-recommends rspamd sqlite3 ca-certificates
COPY local.d/ /etc/rspamd/local.d/
COPY sync-dkim.sh /usr/local/bin/lanqin-rspamd-sync-dkim
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/lanqin-rspamd-sync-dkim && mkdir -p /run/rspamd /var/lib/rspamd/dkim
EXPOSE 11332 11334
CMD ["/entrypoint.sh"]
