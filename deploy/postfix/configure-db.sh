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
	postconf -e 'recipient_bcc_maps = sqlite:/etc/postfix/sqlite-collection-bcc.cf'
	;;
mysql)
	require_external_config
	write_mysql_map /etc/postfix/mysql-domains.cf "SELECT 1 FROM domains WHERE lower(name)=lower('%s') AND status='active'"
	write_mysql_map /etc/postfix/mysql-aliases.cf "SELECT destination FROM aliases WHERE enabled=1 AND (lower(source)=lower('%s') OR (lower(source)=(CONCAT(LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)),'@',LOWER(SUBSTRING_INDEX('%s','@',-1)))) AND NOT EXISTS (SELECT 1 FROM aliases exact_alias WHERE lower(exact_alias.source)=lower('%s') AND exact_alias.enabled=1))) UNION SELECT address FROM mailboxes WHERE lower(address)=(CONCAT(LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)),'@',LOWER(SUBSTRING_INDEX('%s','@',-1)))) AND status='active' UNION SELECT m.address FROM domain_collections c JOIN domains s ON s.id=c.domain_id JOIN mailboxes m ON m.id=c.target_mailbox_id JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id WHERE s.name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND s.status='active' AND m.status='active' AND d.status='active' AND u.disabled=0 AND NOT EXISTS (SELECT 1 FROM mailboxes known WHERE lower(known.address)=(CONCAT(LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)),'@',LOWER(SUBSTRING_INDEX('%s','@',-1)))) AND known.status='active') AND NOT EXISTS (SELECT 1 FROM aliases al WHERE (lower(al.source)=lower('%s') OR lower(al.source)=(CONCAT(LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)),'@',LOWER(SUBSTRING_INDEX('%s','@',-1))))) AND al.enabled=1)"
	write_mysql_map /etc/postfix/mysql-collection-bcc.cf "SELECT m.address FROM domain_collections c JOIN domains s ON s.id=c.domain_id JOIN mailboxes m ON m.id=c.target_mailbox_id JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id WHERE s.name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND s.status='active' AND m.status='active' AND d.status='active' AND u.disabled=0 AND lower(m.address)<>(CONCAT(LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)),'@',LOWER(SUBSTRING_INDEX('%s','@',-1))))"
	write_mysql_map /etc/postfix/mysql-mailboxes.cf "SELECT CONCAT('vhosts/',d.name,'/',m.local_part,'/Maildir/') FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND m.local_part=LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)) AND m.status='active' AND d.status='active' UNION SELECT CONCAT('vhosts/',LOWER(SUBSTRING_INDEX('%s','@',-1)),'/__unregistered__/Maildir/') WHERE EXISTS (SELECT 1 FROM system_settings WHERE \`key\`='catchAllEnabled' AND value='true') AND EXISTS (SELECT 1 FROM domains WHERE name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND status='active') AND NOT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=LOWER(SUBSTRING_INDEX('%s','@',-1)) AND m.local_part=LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%s','@',1),'+',1)) AND m.status='active')"
	postconf -e 'virtual_mailbox_domains = mysql:/etc/postfix/mysql-domains.cf'
	postconf -e 'virtual_mailbox_maps = mysql:/etc/postfix/mysql-mailboxes.cf'
	postconf -e 'virtual_alias_maps = mysql:/etc/postfix/mysql-aliases.cf'
	postconf -e 'recipient_bcc_maps = mysql:/etc/postfix/mysql-collection-bcc.cf'
	;;
pg | pgsql | postgres | postgresql)
	require_external_config
	write_pgsql_map /etc/postfix/pgsql-domains.cf "SELECT 1 FROM domains WHERE lower(name)=lower('%s') AND status='active'"
	write_pgsql_map /etc/postfix/pgsql-aliases.cf "SELECT destination FROM aliases WHERE enabled=1 AND (lower(source)=lower('%s') OR (lower(source)=(lower(split_part(split_part('%s','@',1),'+',1)) || '@' || lower(split_part('%s','@',2))) AND NOT EXISTS (SELECT 1 FROM aliases exact_alias WHERE lower(exact_alias.source)=lower('%s') AND exact_alias.enabled=1))) UNION SELECT address FROM mailboxes WHERE lower(address)=(lower(split_part(split_part('%s','@',1),'+',1)) || '@' || lower(split_part('%s','@',2))) AND status='active' UNION SELECT m.address FROM domain_collections c JOIN domains s ON s.id=c.domain_id JOIN mailboxes m ON m.id=c.target_mailbox_id JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id WHERE s.name=lower(split_part('%s','@',2)) AND s.status='active' AND m.status='active' AND d.status='active' AND u.disabled=0 AND NOT EXISTS (SELECT 1 FROM mailboxes known WHERE lower(known.address)=(lower(split_part(split_part('%s','@',1),'+',1)) || '@' || lower(split_part('%s','@',2))) AND known.status='active') AND NOT EXISTS (SELECT 1 FROM aliases al WHERE (lower(al.source)=lower('%s') OR lower(al.source)=(lower(split_part(split_part('%s','@',1),'+',1)) || '@' || lower(split_part('%s','@',2)))) AND al.enabled=1)"
	write_pgsql_map /etc/postfix/pgsql-collection-bcc.cf "SELECT m.address FROM domain_collections c JOIN domains s ON s.id=c.domain_id JOIN mailboxes m ON m.id=c.target_mailbox_id JOIN domains d ON d.id=m.domain_id JOIN users u ON u.id=m.user_id WHERE s.name=lower(split_part('%s','@',2)) AND s.status='active' AND m.status='active' AND d.status='active' AND u.disabled=0 AND lower(m.address)<>(lower(split_part(split_part('%s','@',1),'+',1)) || '@' || lower(split_part('%s','@',2)))"
	write_pgsql_map /etc/postfix/pgsql-mailboxes.cf "SELECT 'vhosts/' || d.name || '/' || m.local_part || '/Maildir/' FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(split_part('%s','@',2)) AND m.local_part=lower(split_part(split_part('%s','@',1),'+',1)) AND m.status='active' AND d.status='active' UNION SELECT 'vhosts/' || lower(split_part('%s','@',2)) || '/__unregistered__/Maildir/' WHERE EXISTS (SELECT 1 FROM system_settings WHERE key='catchAllEnabled' AND value='true') AND EXISTS (SELECT 1 FROM domains WHERE name=lower(split_part('%s','@',2)) AND status='active') AND NOT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(split_part('%s','@',2)) AND m.local_part=lower(split_part(split_part('%s','@',1),'+',1)) AND m.status='active')"
	postconf -e 'virtual_mailbox_domains = pgsql:/etc/postfix/pgsql-domains.cf'
	postconf -e 'virtual_mailbox_maps = pgsql:/etc/postfix/pgsql-mailboxes.cf'
	postconf -e 'virtual_alias_maps = pgsql:/etc/postfix/pgsql-aliases.cf'
	postconf -e 'recipient_bcc_maps = pgsql:/etc/postfix/pgsql-collection-bcc.cf'
	;;
*)
	echo "error: unsupported LANQIN_DB_DRIVER=$LANQIN_DB_DRIVER" >&2
	exit 1
	;;
esac
