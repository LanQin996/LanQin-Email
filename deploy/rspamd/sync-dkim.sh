#!/bin/sh
set -eu

: "${LANQIN_DB_PATH:=/data/lanqin.db}"
: "${LANQIN_DB_DRIVER:=sqlite}"
: "${LANQIN_DB_HOST:=}"
: "${LANQIN_DB_PORT:=}"
: "${LANQIN_DB_NAME:=}"
: "${LANQIN_DB_USER:=}"
: "${LANQIN_DB_PASSWORD:=}"
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
	rows_file="$(mktemp)"
	chmod 0600 "$rows_file"
	trap 'rm -f "$rows_file"' EXIT HUP INT TERM
	case "$(printf '%s' "$LANQIN_DB_DRIVER" | tr '[:upper:]' '[:lower:]')" in
	'' | sqlite | sqlite3)
		if [ ! -f "$LANQIN_DB_PATH" ]; then
			rm -f "$rows_file"
			trap - EXIT HUP INT TERM
			chown_dkim_dir
			return 0
		fi
		sqlite3 -separator '|' "$LANQIN_DB_PATH" "SELECT name,dkim_selector,dkim_private_key FROM domains WHERE status='active';" >"$rows_file"
		;;
	mysql)
		MYSQL_PWD="$LANQIN_DB_PASSWORD" mysql --protocol=TCP --host="$LANQIN_DB_HOST" --port="$LANQIN_DB_PORT" --user="$LANQIN_DB_USER" --database="$LANQIN_DB_NAME" --batch --raw --skip-column-names --execute="SELECT CONCAT_WS('|',name,dkim_selector,dkim_private_key) FROM domains WHERE status='active'" >"$rows_file"
		;;
	pg | pgsql | postgres | postgresql)
		PGPASSWORD="$LANQIN_DB_PASSWORD" psql --host="$LANQIN_DB_HOST" --port="$LANQIN_DB_PORT" --username="$LANQIN_DB_USER" --dbname="$LANQIN_DB_NAME" --no-align --tuples-only --field-separator='|' --command="SELECT name,dkim_selector,dkim_private_key FROM domains WHERE status='active'" >"$rows_file"
		;;
	*)
		echo "error: unsupported LANQIN_DB_DRIVER=$LANQIN_DB_DRIVER" >&2
		return 1
		;;
	esac

	while IFS='|' read -r domain selector private_key; do
		[ -n "$domain" ] || continue
		[ -n "$selector" ] || selector="lanqin"
		keyfile="$LANQIN_RSPAMD_DKIM_DIR/${domain}.${selector}.key"
		tmpfile="${keyfile}.tmp"
		printf '%s' "$private_key" | base64 -d >"$tmpfile"
		chmod 0640 "$tmpfile"
		mv "$tmpfile" "$keyfile"
	done <"$rows_file"

	rm -f "$rows_file"
	trap - EXIT HUP INT TERM

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
