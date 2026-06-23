#!/bin/sh
set -eu

: "${LANQIN_PUBLIC_HOSTNAME:=mail.example.com}"
: "${LANQIN_DATA_DIR:=/data}"
: "${LANQIN_DB_PATH:=/data/lanqin.db}"
: "${LANQIN_ADDR:=127.0.0.1:8080}"
: "${LANQIN_SMTP_HOST:=127.0.0.1}"
: "${LANQIN_SMTP_PORT:=25}"
: "${LANQIN_MAILDIR_ROOT:=/var/mail/vhosts}"
: "${LANQIN_TLS_CERT_FILE:=}"
: "${LANQIN_TLS_KEY_FILE:=}"

export LANQIN_DATA_DIR LANQIN_DB_PATH LANQIN_ADDR LANQIN_SMTP_HOST LANQIN_SMTP_PORT LANQIN_MAILDIR_ROOT

addgroup --system --gid 5000 vmail 2>/dev/null || true
adduser --system --uid 5000 --gid 5000 --home /var/mail/vhosts --no-create-home vmail 2>/dev/null || true
mkdir -p /data /var/mail/vhosts /var/lib/rspamd/dkim /run/rspamd /var/spool/postfix /var/run/dovecot
chown -R 5000:5000 /var/mail/vhosts
if id _rspamd >/dev/null 2>&1; then
  chown -R _rspamd:_rspamd /run/rspamd /var/lib/rspamd 2>/dev/null || true
elif id rspamd >/dev/null 2>&1; then
  chown -R rspamd:rspamd /run/rspamd /var/lib/rspamd 2>/dev/null || true
fi

AUTH_POLICY_NONCE_FILE="${LANQIN_AUTH_POLICY_NONCE_FILE:-/data/dovecot-auth-policy-nonce}"
mkdir -p "$(dirname "$AUTH_POLICY_NONCE_FILE")"
if [ ! -s "$AUTH_POLICY_NONCE_FILE" ]; then
  od -An -tx1 -N32 /dev/urandom | tr -d ' \n' > "$AUTH_POLICY_NONCE_FILE"
fi
chmod 600 "$AUTH_POLICY_NONCE_FILE" 2>/dev/null || true
AUTH_POLICY_HASH_NONCE="$(cat "$AUTH_POLICY_NONCE_FILE")"

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

postconf -e "myhostname = ${LANQIN_PUBLIC_HOSTNAME}"
postconf -e "myorigin = ${LANQIN_PUBLIC_HOSTNAME}"
postconf -e "smtpd_tls_cert_file = ${TLS_CERT}"
postconf -e "smtpd_tls_key_file = ${TLS_KEY}"
postconf -e "virtual_transport = lmtp:inet:127.0.0.1:24"
postconf -e "smtpd_sasl_path = inet:127.0.0.1:12345"
postconf -e "milter_mail_macros = i {mail_addr} {client_addr} {client_name} {auth_authen}"
postconf -e "smtpd_milters = inet:127.0.0.1:11332"
postconf -e "non_smtpd_milters = inet:127.0.0.1:11332"
sed -i "s#^ssl_cert = <.*#ssl_cert = <${TLS_CERT}#" /etc/dovecot/dovecot.conf
sed -i "s#^ssl_key = <.*#ssl_key = <${TLS_KEY}#" /etc/dovecot/dovecot.conf
sed -i "s#^auth_policy_hash_nonce = .*#auth_policy_hash_nonce = ${AUTH_POLICY_HASH_NONCE}#" /etc/dovecot/dovecot.conf

# Rspamd DKIM keys are exported after API seed/migrations create the SQLite DB.
/usr/local/bin/lanqin-api >/tmp/lanqin-api-bootstrap.log 2>&1 &
bootstrap_pid=$!
for i in $(seq 1 60); do
  if [ -f "$LANQIN_DB_PATH" ]; then
    users_count="$(sqlite3 "$LANQIN_DB_PATH" "SELECT COALESCE(COUNT(1),0) FROM users;" 2>/dev/null || echo 0)"
    domains_count="$(sqlite3 "$LANQIN_DB_PATH" "SELECT COALESCE(COUNT(1),0) FROM domains;" 2>/dev/null || echo 0)"
    if [ "${users_count:-0}" -gt 0 ] && [ "${domains_count:-0}" -gt 0 ]; then
      break
    fi
  fi
  sleep 1
done
kill "$bootstrap_pid" 2>/dev/null || true
wait "$bootstrap_pid" 2>/dev/null || true

/usr/local/bin/lanqin-rspamd-sync-dkim --once || true

postfix check
exec /usr/bin/supervisord -c /etc/supervisor/conf.d/lanqin.conf
