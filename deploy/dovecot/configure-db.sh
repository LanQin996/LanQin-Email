#!/bin/sh
set -eu

: "${LANQIN_DB_DRIVER:=sqlite}"
: "${LANQIN_DB_HOST:=}"
: "${LANQIN_DB_PORT:=}"
: "${LANQIN_DB_NAME:=}"
: "${LANQIN_DB_USER:=}"
: "${LANQIN_DB_PASSWORD:=}"

require_external_config() {
	for name in LANQIN_DB_HOST LANQIN_DB_PORT LANQIN_DB_NAME LANQIN_DB_USER LANQIN_DB_PASSWORD; do
		eval "value=\${$name:-}"
		if [ -z "$value" ]; then
			echo "error: $name is required for LANQIN_DB_DRIVER=$LANQIN_DB_DRIVER" >&2
			exit 1
		fi
		clean="$(printf '%s' "$value" | tr -d '\r\n')"
		if [ "$clean" != "$value" ]; then
			echo "error: $name must not contain newlines" >&2
			exit 1
		fi
		case "$value" in
		*[[:space:]]*)
			echo "error: $name must not contain whitespace in Dovecot SQL configuration" >&2
			exit 1
			;;
		esac
	done
}

write_config() {
	driver="$1"
	password_query="$2"
	user_query="$3"
	{
		printf 'driver = %s\n' "$driver"
		if [ "$driver" = sqlite ]; then
			printf 'connect = /data/lanqin.db\n'
		else
			printf 'connect = host=%s port=%s dbname=%s user=%s password=%s\n' "$LANQIN_DB_HOST" "$LANQIN_DB_PORT" "$LANQIN_DB_NAME" "$LANQIN_DB_USER" "$LANQIN_DB_PASSWORD"
		fi
		printf 'default_pass_scheme = BLF-CRYPT\n'
		printf 'password_query = %s\n' "$password_query"
		printf 'user_query = %s\n' "$user_query"
	} >/etc/dovecot/dovecot-sql.conf.ext
	chmod 0600 /etc/dovecot/dovecot-sql.conf.ext
}

case "$(printf '%s' "$LANQIN_DB_DRIVER" | tr '[:upper:]' '[:lower:]')" in
'' | sqlite | sqlite3)
	write_config sqlite \
		"SELECT address AS user, password_hash AS password FROM mailboxes WHERE lower(address)=lower('%u') AND status='active'" \
		"SELECT '/var/mail/vhosts/' || d.name || '/' || m.local_part AS home, 'maildir:/var/mail/vhosts/' || d.name || '/' || m.local_part || '/Maildir' AS mail, 5000 AS uid, 5000 AS gid, '*:storage=' || CAST(m.quota_mb AS TEXT) || 'M' AS quota_rule FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(substr('%u', instr('%u', '@') + 1)) AND m.local_part=lower(CASE WHEN instr(substr('%u', 1, instr('%u', '@') - 1), '+') > 0 THEN substr(substr('%u', 1, instr('%u', '@') - 1), 1, instr(substr('%u', 1, instr('%u', '@') - 1), '+') - 1) ELSE substr('%u', 1, instr('%u', '@') - 1) END) AND m.status='active' AND d.status='active' UNION SELECT '/var/mail/vhosts/' || lower(substr('%u', instr('%u', '@') + 1)) || '/__unregistered__' AS home, 'maildir:/var/mail/vhosts/' || lower(substr('%u', instr('%u', '@') + 1)) || '/__unregistered__/Maildir' AS mail, 5000 AS uid, 5000 AS gid, '*:storage=1024M' AS quota_rule WHERE EXISTS (SELECT 1 FROM system_settings WHERE key='catchAllEnabled' AND value='true') AND EXISTS (SELECT 1 FROM domains WHERE name=lower(substr('%u', instr('%u', '@') + 1)) AND status='active') AND NOT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(substr('%u', instr('%u', '@') + 1)) AND m.local_part=lower(CASE WHEN instr(substr('%u', 1, instr('%u', '@') - 1), '+') > 0 THEN substr(substr('%u', 1, instr('%u', '@') - 1), 1, instr(substr('%u', 1, instr('%u', '@') - 1), '+') - 1) ELSE substr('%u', 1, instr('%u', '@') - 1) END) AND m.status='active')"
	;;
mysql)
	require_external_config
	write_config mysql \
		"SELECT address AS user,password_hash AS password FROM mailboxes WHERE lower(address)=lower('%u') AND status='active'" \
		"SELECT CONCAT('/var/mail/vhosts/',d.name,'/',m.local_part) AS home,CONCAT('maildir:/var/mail/vhosts/',d.name,'/',m.local_part,'/Maildir') AS mail,5000 AS uid,5000 AS gid,CONCAT('*:storage=',CAST(m.quota_mb AS CHAR),'M') AS quota_rule FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=LOWER(SUBSTRING_INDEX('%u','@',-1)) AND m.local_part=LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%u','@',1),'+',1)) AND m.status='active' AND d.status='active' UNION SELECT CONCAT('/var/mail/vhosts/',LOWER(SUBSTRING_INDEX('%u','@',-1)),'/__unregistered__') AS home,CONCAT('maildir:/var/mail/vhosts/',LOWER(SUBSTRING_INDEX('%u','@',-1)),'/__unregistered__/Maildir') AS mail,5000 AS uid,5000 AS gid,'*:storage=1024M' AS quota_rule WHERE EXISTS (SELECT 1 FROM system_settings WHERE \`key\`='catchAllEnabled' AND value='true') AND EXISTS (SELECT 1 FROM domains WHERE name=LOWER(SUBSTRING_INDEX('%u','@',-1)) AND status='active') AND NOT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=LOWER(SUBSTRING_INDEX('%u','@',-1)) AND m.local_part=LOWER(SUBSTRING_INDEX(SUBSTRING_INDEX('%u','@',1),'+',1)) AND m.status='active')"
	;;
pg | pgsql | postgres | postgresql)
	require_external_config
	write_config pgsql \
		"SELECT address AS user,password_hash AS password FROM mailboxes WHERE lower(address)=lower('%u') AND status='active'" \
		"SELECT '/var/mail/vhosts/' || d.name || '/' || m.local_part AS home,'maildir:/var/mail/vhosts/' || d.name || '/' || m.local_part || '/Maildir' AS mail,5000 AS uid,5000 AS gid,'*:storage=' || CAST(m.quota_mb AS TEXT) || 'M' AS quota_rule FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(split_part('%u','@',2)) AND m.local_part=lower(split_part(split_part('%u','@',1),'+',1)) AND m.status='active' AND d.status='active' UNION SELECT '/var/mail/vhosts/' || lower(split_part('%u','@',2)) || '/__unregistered__' AS home,'maildir:/var/mail/vhosts/' || lower(split_part('%u','@',2)) || '/__unregistered__/Maildir' AS mail,5000 AS uid,5000 AS gid,'*:storage=1024M' AS quota_rule WHERE EXISTS (SELECT 1 FROM system_settings WHERE key='catchAllEnabled' AND value='true') AND EXISTS (SELECT 1 FROM domains WHERE name=lower(split_part('%u','@',2)) AND status='active') AND NOT EXISTS (SELECT 1 FROM mailboxes m JOIN domains d ON d.id=m.domain_id WHERE d.name=lower(split_part('%u','@',2)) AND m.local_part=lower(split_part(split_part('%u','@',1),'+',1)) AND m.status='active')"
	;;
*)
	echo "error: unsupported LANQIN_DB_DRIVER=$LANQIN_DB_DRIVER" >&2
	exit 1
	;;
esac
