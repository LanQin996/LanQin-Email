#!/bin/sh
set -eu

: "${LANQIN_DB_PATH:=/data/lanqin.db}"
: "${LANQIN_RSPAMD_DKIM_DIR:=/var/lib/rspamd/dkim}"
: "${LANQIN_RSPAMD_DKIM_SYNC_SECONDS:=60}"

chown_dkim_dir() {
  if id _rspamd >/dev/null 2>&1; then
    chown -R _rspamd:_rspamd "$LANQIN_RSPAMD_DKIM_DIR" 2>/dev/null || true
  elif id rspamd >/dev/null 2>&1; then
    chown -R rspamd:rspamd "$LANQIN_RSPAMD_DKIM_DIR" 2>/dev/null || true
  fi
}

sync_keys() {
  mkdir -p "$LANQIN_RSPAMD_DKIM_DIR"
  if [ ! -f "$LANQIN_DB_PATH" ]; then
    chown_dkim_dir
    return 0
  fi

  sqlite3 -separator '|' "$LANQIN_DB_PATH" "SELECT name, dkim_selector, dkim_private_key FROM domains WHERE status='active';" 2>/dev/null | while IFS='|' read -r domain selector private_key; do
    [ -n "$domain" ] || continue
    [ -n "$selector" ] || selector="lanqin"
    keyfile="$LANQIN_RSPAMD_DKIM_DIR/${domain}.${selector}.key"
    tmpfile="${keyfile}.tmp"
    printf '%s' "$private_key" | base64 -d > "$tmpfile"
    chmod 0640 "$tmpfile"
    mv "$tmpfile" "$keyfile"
  done

  chown_dkim_dir
}

if [ "${1:-}" = "--once" ]; then
  sync_keys
  exit 0
fi

while true; do
  sync_keys || true
  sleep "$LANQIN_RSPAMD_DKIM_SYNC_SECONDS"
done
