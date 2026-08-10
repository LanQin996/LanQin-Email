#!/bin/sh
set -eu

: "${LANQIN_DB_DRIVER:=sqlite}"
: "${LANQIN_DB_HOST:=}"
: "${LANQIN_DB_PORT:=}"
: "${LANQIN_DB_NAME:=}"
: "${LANQIN_DB_USER:=}"
: "${LANQIN_DB_PASSWORD:=}"

reject_newlines() {
	clean="$(printf '%s' "$2" | tr -d '\r\n')"
	if [ "$clean" != "$2" ]; then
		echo "error: $1 must not contain newlines" >&2
		exit 1
	fi
}

require_external_config() {
	for name in LANQIN_DB_HOST LANQIN_DB_PORT LANQIN_DB_NAME LANQIN_DB_USER LANQIN_DB_PASSWORD; do
		eval "value=\${$name:-}"
		if [ -z "$value" ]; then
			echo "error: $name is required for LANQIN_DB_DRIVER=$LANQIN_DB_DRIVER" >&2
			exit 1
		fi
		reject_newlines "$name" "$value"
	done
}

write_mysql_map() {
	file="$1"
	query="$2"
	{
		printf 'user = %s\n' "$LANQIN_DB_USER"
		printf 'password = %s\n' "$LANQIN_DB_PASSWORD"
		printf 'hosts = %s:%s\n' "$LANQIN_DB_HOST" "$LANQIN_DB_PORT"
		printf 'dbname = %s\n' "$LANQIN_DB_NAME"
		printf 'query = %s\n' "$query"
	} >"$file"
	chown root:postfix "$file"
	chmod 0640 "$file"
}

write_pgsql_map() {
	file="$1"
	query="$2"
	{
		printf 'user = %s\n' "$LANQIN_DB_USER"
		printf 'password = %s\n' "$LANQIN_DB_PASSWORD"
		printf 'hosts = %s:%s\n' "$LANQIN_DB_HOST" "$LANQIN_DB_PORT"
		printf 'dbname = %s\n' "$LANQIN_DB_NAME"
		printf 'query = %s\n' "$query"
	} >"$file"
	chown root:postfix "$file"
	chmod 0640 "$file"
}

case "$(printf '%s' "$LANQIN_DB_DRIVER" | tr '[:upper:]' '[:lower:]')" in
'' | sqlite | sqlite3)
	postconf -e 'virtual_mailbox_domains = sqlite:/etc/postfix/sqlite-domains.cf'
	postconf -e 'virtual_mailbox_maps = sqlite:/etc/postfix/sqlite-mailboxes.cf'
	postconf -e 'virtual_alias_maps = sqlite:/etc/postfix/sqlite-aliases.cf'
	;;
mysql)
	require_external_config
	write_mysql_map /etc/postfix/mysql-domains.cf "SELECT 1 FROM domains WHERE lower(name)=lower('%s') AND status='active'"
	write_mysql_map /etc/postfix/mysql-aliases.cf "SELECT destination FROM aliases WHERE lower(source)=lower('%s') AND enabled=1 UNION SELECT address FROM mailboxes WHERE lower(address)=lower('%s') AND status='active'"
	write_mysql_map /etc/postfix/mysql-mailboxes.cf "SELECT CONCAT('vhosts/',d.name,'/',m.local_part,'/Maildir/') FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND m.local_part=LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)) AND m.status='active' AND d.status='active' UNION SELECT CONCAT('vhosts/',LOWER(SUBSTRING_INDEX('%s','@',-1)),'/__unregistered__/Maildir/') WHERE EXISTS (SELECT 1 FROM system_settings WHERE \`key\`='catchAllEnabled' AND value='true') AND EXISTS (SELECT 1 FROM domains WHERE name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND status='active') AND NOT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND m.local_part=LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)) AND m.status='active')"
	postconf -e 'virtual_mailbox_domains = mysql:/etc/postfix/mysql-domains.cf'
	postconf -e 'virtual_mailbox_maps = mysql:/etc/postfix/mysql-mailboxes.cf'
	postconf -e 'virtual_alias_maps = mysql:/etc/postfix/mysql-aliases.cf'
	;;
pg | pgsql | postgres | postgresql)
	require_external_config
	write_pgsql_map /etc/postfix/pgsql-domains.cf "SELECT 1 FROM domains WHERE lower(name)=lower('%s') AND status='active'"
	write_pgsql_map /etc/postfix/pgsql-aliases.cf "SELECT destination FROM aliases WHERE lower(source)=lower('%s') AND enabled=1 UNION SELECT address FROM mailboxes WHERE lower(address)=lower('%s') AND status='active'"
	write_pgsql_map /etc/postfix/pgsql-mailboxes.cf "SELECT 'vhosts/' || d.name || '/' || m.local_part || '/Maildir/' FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(split_part('%s','@',2)) AND m.local_part=lower(split_part(split_part('%s','@',1),'+',1)) AND m.status='active' AND d.status='active' UNION SELECT 'vhosts/' || lower(split_part('%s','@',2)) || '/__unregistered__/Maildir/' WHERE EXISTS (SELECT 1 FROM system_settings WHERE key='catchAllEnabled' AND value='true') AND EXISTS (SELECT 1 FROM domains WHERE name=lower(split_part('%s','@',2)) AND status='active') AND NOT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(split_part('%s','@',2)) AND m.local_part=lower(split_part(split_part('%s','@',1),'+',1)) AND m.status='active')"
	postconf -e 'virtual_mailbox_domains = pgsql:/etc/postfix/pgsql-domains.cf'
	postconf -e 'virtual_mailbox_maps = pgsql:/etc/postfix/pgsql-mailboxes.cf'
	postconf -e 'virtual_alias_maps = pgsql:/etc/postfix/pgsql-aliases.cf'
	;;
*)
	echo "error: unsupported LANQIN_DB_DRIVER=$LANQIN_DB_DRIVER" >&2
	exit 1
	;;
esac
