#!/bin/sh
set -eu

mkdir -p /run/rspamd /var/lib/rspamd/dkim
if id _rspamd >/dev/null 2>&1; then
  chown -R _rspamd:_rspamd /run/rspamd /var/lib/rspamd 2>/dev/null || true
elif id rspamd >/dev/null 2>&1; then
  chown -R rspamd:rspamd /run/rspamd /var/lib/rspamd 2>/dev/null || true
fi

/usr/local/bin/lanqin-rspamd-sync-dkim --once || true
/usr/local/bin/lanqin-rspamd-sync-dkim &
if id _rspamd >/dev/null 2>&1; then
  exec rspamd -f -u _rspamd -g _rspamd
elif id rspamd >/dev/null 2>&1; then
  exec rspamd -f -u rspamd -g rspamd
fi
exec rspamd -f --insecure
