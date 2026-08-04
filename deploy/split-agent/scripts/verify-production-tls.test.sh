#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-tls-gate.XXXXXX")
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

cert=$tmp_dir/server.crt
key=$tmp_dir/server.key
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj '/C=CN/ST=Beijing/L=Beijing/O=Dirextalk/CN=message.example.com' \
  -addext 'subjectAltName=DNS:message.example.com' \
  -keyout "$key" -out "$cert" >/dev/null 2>&1
chmod 400 "$key"
chmod 444 "$cert"

env_file=$tmp_dir/.env
cat >"$env_file" <<EOF
DIREXTALK_MESSAGE_TLS_MODE=external
DIREXTALK_MESSAGE_TLS_CERT_FILE=$cert
DIREXTALK_MESSAGE_TLS_KEY_FILE=$key
DIREXTALK_MESSAGE_SERVER_NAME=message.example.com
EOF
chmod 400 "$env_file"

"$script_dir/verify-production-tls.sh" "$env_file" >/dev/null

sed -i 's/DIREXTALK_MESSAGE_TLS_MODE=external/DIREXTALK_MESSAGE_TLS_MODE=local/' "$env_file"
if "$script_dir/verify-production-tls.sh" "$env_file" >/dev/null 2>&1; then
  echo "local self-signed TLS unexpectedly passed production gate" >&2
  exit 1
fi

printf 'production TLS checks verified\n'
