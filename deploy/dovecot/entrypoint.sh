#!/bin/sh
set -eu
: "${LANQIN_TLS_CERT_FILE:=}"
: "${LANQIN_TLS_KEY_FILE:=}"
addgroup --system --gid 5000 vmail 2>/dev/null || true
adduser --system --uid 5000 --gid 5000 --home /var/mail/vhosts --no-create-home vmail 2>/dev/null || true
mkdir -p /var/mail/vhosts
chown -R 5000:5000 /var/mail/vhosts
TLS_CERT=/etc/ssl/certs/ssl-cert-snakeoil.pem
TLS_KEY=/etc/ssl/private/ssl-cert-snakeoil.key
if [ -n "$LANQIN_TLS_CERT_FILE" ] || [ -n "$LANQIN_TLS_KEY_FILE" ]; then
  if [ -f "$LANQIN_TLS_CERT_FILE" ] && [ -f "$LANQIN_TLS_KEY_FILE" ]; then
    TLS_CERT="$LANQIN_TLS_CERT_FILE"
    TLS_KEY="$LANQIN_TLS_KEY_FILE"
  else
    echo "warning: LANQIN_TLS_CERT_FILE/LANQIN_TLS_KEY_FILE not readable; using snakeoil localhost certificate" >&2
  fi
fi
sed -i "s#^ssl_cert = <.*#ssl_cert = <${TLS_CERT}#" /etc/dovecot/dovecot.conf
sed -i "s#^ssl_key = <.*#ssl_key = <${TLS_KEY}#" /etc/dovecot/dovecot.conf
exec dovecot -F
