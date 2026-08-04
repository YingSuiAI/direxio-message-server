#!/bin/sh
set -eu

script_dir=$(cd "$(dirname -- "$0")" && pwd -P)
entrypoint=$script_dir/message-server-entrypoint.sh
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-message-entrypoint.XXXXXX")
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

template=$tmp_dir/template.yaml
secret=$tmp_dir/database-url
runtime=$tmp_dir/runtime.yaml
binary=$tmp_dir/fake-message-server
observed=$tmp_dir/observed
dsn='postgresql://user:p@ss&pipe|word@db:5432/message?sslmode=disable'

printf '%s\n' 'global:' '  database:' '    connection_string: __DIREXTALK_DB_DSN__' >"$template"
printf '%s\n' "$dsn" >"$secret"
cat >"$binary" <<'EOF'
#!/bin/sh
set -eu
[ "${1:-}" = --config ]
[ -n "${2:-}" ]
[ "${3:-}" = --http-bind-address ]
[ "${4:-}" = :18008 ]
[ "${5:-}" = --https-bind-address ]
[ "${6:-}" = :18448 ]
[ "${7:-}" = --tls-cert ]
[ "${8:-}" = /etc/dirextalk-message-server/server.crt ]
[ "${9:-}" = --tls-key ]
[ "${10:-}" = /etc/dirextalk-message-server/server.key ]
[ "$#" -eq 10 ]
config=$2
case "$(cat "$config")" in
  *'connection_string: postgresql://user:p@ss&pipe|word@db:5432/message?sslmode=disable'*) ;;
  *) exit 1 ;;
esac
case "$(cat "$config")" in
  *__DIREXTALK_DB_DSN__*) exit 1 ;;
esac
if env | grep -F 'postgresql://' >/dev/null 2>&1; then
  exit 1
fi
printf '%s\n' ok >"$MESSAGE_SERVER_ENTRYPOINT_TEST_OBSERVED"
EOF
chmod 700 "$binary"

MESSAGE_SERVER_CONFIG_TEMPLATE=$template \
MESSAGE_SERVER_DATABASE_URL_FILE=$secret \
MESSAGE_SERVER_RUNTIME_CONFIG=$runtime \
MESSAGE_SERVER_BINARY=$binary \
MESSAGE_SERVER_ENTRYPOINT_TEST_OBSERVED=$observed \
  "$entrypoint" \
    --http-bind-address :18008 \
    --https-bind-address :18448 \
    --tls-cert /etc/dirextalk-message-server/server.crt \
    --tls-key /etc/dirextalk-message-server/server.key

[ "$(cat "$observed")" = ok ]
[ "$(stat -c '%a' "$runtime")" = 400 ]
printf 'message-server entrypoint secret-file substitution verified\n'
