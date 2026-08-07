#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/initialize-postgres.sh
[ -x "$script" ] || { echo "initialize-postgres.sh must be executable" >&2; exit 1; }
bash -n "$script"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck "$script"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-postgres-init.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
mkdir -m 700 "$tmp/bin" "$tmp/secrets"
printf '%048x\n' 1 >"$tmp/secrets/message_postgres_password"
printf '%048x\n' 2 >"$tmp/secrets/agent_postgres_password"
chmod 400 "$tmp/secrets/"*

cat >"$tmp/bin/psql" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$POSTGRES_INIT_TEST_ARGS"
cat >>"$POSTGRES_INIT_TEST_SQL"
EOF
chmod 755 "$tmp/bin/psql"

POSTGRES_USER=dirextalk_cluster_admin \
POSTGRES_INIT_SECRET_DIR=$tmp/secrets \
POSTGRES_INIT_TEST_ARGS=$tmp/args \
POSTGRES_INIT_TEST_SQL=$tmp/sql \
PATH=$tmp/bin:$PATH \
  "$script"

[ "$(wc -l <"$tmp/args")" -eq 3 ]
if grep -Fq "$(cat "$tmp/secrets/message_postgres_password")" "$tmp/args" ||
   grep -Fq "$(cat "$tmp/secrets/agent_postgres_password")" "$tmp/args"; then
  echo "application password leaked into psql arguments" >&2
  exit 1
fi
grep -Fq 'CREATE ROLE dirextalk_message_server' "$tmp/sql"
grep -Fq 'CREATE ROLE dirextalk_agent' "$tmp/sql"
grep -Fq 'NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION' "$tmp/sql"
grep -Fq 'CREATE DATABASE dirextalk_message_server OWNER dirextalk_message_server' "$tmp/sql"
grep -Fq 'CREATE DATABASE dirextalk_agent OWNER dirextalk_agent' "$tmp/sql"
grep -Fq 'REVOKE CONNECT, TEMPORARY ON DATABASE dirextalk_message_server FROM PUBLIC' "$tmp/sql"
grep -Fq 'REVOKE CONNECT, TEMPORARY ON DATABASE dirextalk_agent FROM PUBLIC' "$tmp/sql"
grep -Fq 'CREATE EXTENSION vector' "$tmp/sql"
grep -Fq 'REVOKE ALL ON SCHEMA public FROM PUBLIC' "$tmp/sql"

chmod 600 "$tmp/secrets/agent_postgres_password"
cp "$tmp/secrets/message_postgres_password" "$tmp/secrets/agent_postgres_password"
chmod 400 "$tmp/secrets/agent_postgres_password"
if POSTGRES_USER=dirextalk_cluster_admin POSTGRES_INIT_SECRET_DIR=$tmp/secrets PATH=$tmp/bin:$PATH \
  POSTGRES_INIT_TEST_ARGS=$tmp/rejected.args POSTGRES_INIT_TEST_SQL=$tmp/rejected.sql "$script" >/dev/null 2>&1; then
  echo "identical application passwords were accepted" >&2
  exit 1
fi

echo "PostgreSQL fresh-state role and database initializer tests passed"
