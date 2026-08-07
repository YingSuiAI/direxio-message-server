#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

die() {
  echo "split PostgreSQL init: $*" >&2
  exit 1
}

read_secret() {
  local label=$1 path=$2 value
  [ -f "$path" ] && [ ! -L "$path" ] || die "$label is not a regular non-symlink file"
  IFS= read -r value <"$path" || [ -n "$value" ] || die "$label is empty"
  [ -n "$value" ] || die "$label is empty"
  printf '%s' "$value"
}

[ "${POSTGRES_USER:-}" = dirextalk_cluster_admin ] || die "POSTGRES_USER must be the protected cluster administrator"
secret_dir=${POSTGRES_INIT_SECRET_DIR:-/run/secrets}
message_password=$(read_secret "message PostgreSQL password" "$secret_dir/message_postgres_password")
agent_password=$(read_secret "Agent PostgreSQL password" "$secret_dir/agent_postgres_password")
printf '%s\n' "$message_password" | grep -Eq '^[0-9a-f]{48}$' || die "message PostgreSQL password has an invalid fresh-state format"
printf '%s\n' "$agent_password" | grep -Eq '^[0-9a-f]{48}$' || die "Agent PostgreSQL password has an invalid fresh-state format"
[ "$message_password" != "$agent_password" ] || die "application database passwords must differ"

psql --username "$POSTGRES_USER" --dbname postgres --set=ON_ERROR_STOP=1 <<SQL
CREATE ROLE dirextalk_message_server LOGIN PASSWORD '$message_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION;
CREATE ROLE dirextalk_agent LOGIN PASSWORD '$agent_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION;

CREATE DATABASE dirextalk_message_server OWNER dirextalk_message_server;
CREATE DATABASE dirextalk_agent OWNER dirextalk_agent;

REVOKE CONNECT, TEMPORARY ON DATABASE postgres FROM PUBLIC;
REVOKE CONNECT ON DATABASE template1 FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE dirextalk_message_server FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE dirextalk_agent FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE dirextalk_message_server TO dirextalk_message_server;
GRANT CONNECT, TEMPORARY ON DATABASE dirextalk_agent TO dirextalk_agent;
SQL
unset message_password agent_password

psql --username "$POSTGRES_USER" --dbname dirextalk_message_server --set=ON_ERROR_STOP=1 <<'SQL'
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO dirextalk_message_server;
SQL

psql --username "$POSTGRES_USER" --dbname dirextalk_agent --set=ON_ERROR_STOP=1 <<'SQL'
CREATE EXTENSION vector;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO dirextalk_agent;
SQL
