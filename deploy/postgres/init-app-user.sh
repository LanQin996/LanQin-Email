#!/bin/sh
set -eu

: "${POSTGRES_DB:?POSTGRES_DB is required}"
: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${LANQIN_POSTGRES_APP_USER:?LANQIN_POSTGRES_APP_USER is required}"
: "${LANQIN_POSTGRES_APP_PASSWORD:?LANQIN_POSTGRES_APP_PASSWORD is required}"

case "$POSTGRES_DB$POSTGRES_USER$LANQIN_POSTGRES_APP_USER" in
'' | *[!A-Za-z0-9_]*)
	echo "error: PostgreSQL database and user names may contain only letters, digits, and underscores" >&2
	exit 1
	;;
esac
case "$LANQIN_POSTGRES_APP_PASSWORD" in
'' | *[!A-Za-z0-9._~-]*)
	echo "error: LANQIN_POSTGRES_APP_PASSWORD may contain only letters, digits, dot, underscore, tilde, and hyphen" >&2
	exit 1
	;;
esac
if [ "$POSTGRES_USER" = "$LANQIN_POSTGRES_APP_USER" ]; then
	echo "error: PostgreSQL administrator and application must use separate accounts" >&2
	exit 1
fi

psql --set=ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
	--set=db_name="$POSTGRES_DB" --set=app_user="$LANQIN_POSTGRES_APP_USER" \
	--set=app_password="$LANQIN_POSTGRES_APP_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', :'app_user', :'app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app_user') \gexec
ALTER DATABASE :"db_name" OWNER TO :"app_user";
ALTER SCHEMA public OWNER TO :"app_user";
SQL
