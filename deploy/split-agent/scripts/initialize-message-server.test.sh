#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
initializer=$script_dir/initialize-message-server.sh
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-message-init.XXXXXX")
trap 'rm -rf -- "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin" "$tmp_dir/external"

cat >"$tmp_dir/bin/generate-keys" <<'EOF'
#!/bin/sh
set -eu
case "$1" in
  -private-key) printf 'matrix-key\n' >"$2" ;;
  -tls-cert) printf 'local-cert\n' >"$2"; [ "$3" = -tls-key ]; printf 'local-key\n' >"$4" ;;
  *) exit 2 ;;
esac
EOF
cat >"$tmp_dir/bin/generate-config" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' 'global:' '  database:' '    connection_string: __DIREXTALK_DB_DSN__' '  well_known_client_name: "http://invalid"'
EOF
cat >"$tmp_dir/bin/capability-init" <<'EOF'
#!/bin/sh
set -eu
for dir in "$@"; do mkdir -p "$dir"; done
EOF
chmod 755 "$tmp_dir/bin/"*
: >"$tmp_dir/registration-secret"
: >"$tmp_dir/external/server.crt"
: >"$tmp_dir/external/server.key"

run_init() {
  local root=$1 mode=$2 deployment=$3
  mkdir -p "$root"
  MESSAGE_CONFIG_DIR=$root/config \
  MESSAGE_DATA_DIR=$root/data \
  MESSAGE_REGISTRATION_SECRET_FILE=$tmp_dir/registration-secret \
  MESSAGE_EXTERNAL_TLS_CERT_FILE=$tmp_dir/external/server.crt \
  MESSAGE_EXTERNAL_TLS_KEY_FILE=$tmp_dir/external/server.key \
  MESSAGE_GENERATE_KEYS_BINARY=$tmp_dir/bin/generate-keys \
  MESSAGE_GENERATE_CONFIG_BINARY=$tmp_dir/bin/generate-config \
  MESSAGE_CAPABILITY_INITIALIZER=$tmp_dir/bin/capability-init \
  MESSAGE_CAPABILITY_AUTHORITY_DIR=$root/capability-authority \
  MESSAGE_CAPABILITY_SHARED_DIR=$root/capability \
  MESSAGE_CAPABILITY_PRIVATE_DIR=$root/capability-private \
  MESSAGE_SERVER_TLS_MODE=$mode \
  MESSAGE_DEPLOYMENT_MODE=$deployment \
  MESSAGE_SERVER_NAME=message.example.com \
  MESSAGE_CLIENT_BASE_URL=https://message.example.com \
  MESSAGE_LOCAL_BOOTSTRAP_ENABLED=false \
    "$initializer"
}

edge_root=$tmp_dir/edge
run_init "$edge_root" edge-terminated production
[ ! -e "$edge_root/config/server.crt" ]
[ ! -e "$edge_root/config/server.key" ]
grep -Fq 'well_known_client_name: "https://message.example.com"' "$edge_root/config/message-server.yaml"

if run_init "$tmp_dir/edge-local" edge-terminated local >/dev/null 2>&1; then
  echo 'edge-terminated local initialization unexpectedly passed' >&2
  exit 1
fi
if run_init "$tmp_dir/local-production" local production >/dev/null 2>&1; then
  echo 'local TLS production initialization unexpectedly passed' >&2
  exit 1
fi

printf 'message-server edge-terminated initialization wrapper verified\n'
