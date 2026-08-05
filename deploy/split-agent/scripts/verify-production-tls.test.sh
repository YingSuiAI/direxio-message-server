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
chmod 400 "$cert"

env_file=$tmp_dir/.env
cat >"$env_file" <<EOF
DIREXTALK_MESSAGE_TLS_MODE=external
DIREXTALK_MESSAGE_TLS_CERT_FILE=$cert
DIREXTALK_MESSAGE_TLS_KEY_FILE=$key
DIREXTALK_MESSAGE_SERVER_NAME=message.example.com
EOF
chmod 400 "$env_file"

"$script_dir/verify-production-tls.sh" "$env_file" >/dev/null

# The gate binds the certificate identity and server name, not just parseability.
sed -i 's/DIREXTALK_MESSAGE_SERVER_NAME=message.example.com/DIREXTALK_MESSAGE_SERVER_NAME=other.example.com/' "$env_file"
if "$script_dir/verify-production-tls.sh" "$env_file" >/dev/null 2>&1; then
  echo "wrong TLS server name unexpectedly passed production gate" >&2
  exit 1
fi
sed -i 's/DIREXTALK_MESSAGE_SERVER_NAME=other.example.com/DIREXTALK_MESSAGE_SERVER_NAME=message.example.com/' "$env_file"

second_cert=$tmp_dir/second.crt
second_key=$tmp_dir/second.key
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj '/C=CN/ST=Beijing/L=Beijing/O=Dirextalk/CN=message.example.com' \
  -addext 'subjectAltName=DNS:message.example.com' \
  -keyout "$second_key" -out "$second_cert" >/dev/null 2>&1
chmod 400 "$second_key" "$second_cert"
sed -i "s#DIREXTALK_MESSAGE_TLS_KEY_FILE=.*#DIREXTALK_MESSAGE_TLS_KEY_FILE=$second_key#" "$env_file"
if "$script_dir/verify-production-tls.sh" "$env_file" >/dev/null 2>&1; then
  echo "mismatched TLS certificate/key unexpectedly passed production gate" >&2
  exit 1
fi
sed -i "s#DIREXTALK_MESSAGE_TLS_KEY_FILE=.*#DIREXTALK_MESSAGE_TLS_KEY_FILE=$key#" "$env_file"

chmod 600 "$cert"
if "$script_dir/verify-production-tls.sh" "$env_file" >/dev/null 2>&1; then
  echo "mode-0600 TLS certificate unexpectedly passed production gate" >&2
  exit 1
fi
chmod 400 "$cert"
chmod 600 "$env_file"
if "$script_dir/verify-production-tls.sh" "$env_file" >/dev/null 2>&1; then
  echo "mode-0600 environment unexpectedly passed production gate" >&2
  exit 1
fi
chmod 400 "$env_file"

sed -i 's/DIREXTALK_MESSAGE_TLS_MODE=external/DIREXTALK_MESSAGE_TLS_MODE=local/' "$env_file"
if "$script_dir/verify-production-tls.sh" "$env_file" >/dev/null 2>&1; then
  echo "local self-signed TLS unexpectedly passed production gate" >&2
  exit 1
fi

printf 'production TLS checks verified\n'
