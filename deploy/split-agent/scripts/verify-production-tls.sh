#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ENV_FILE" >&2
  exit 2
}

die() {
  echo "production TLS gate: $*" >&2
  exit 1
}

[ "$#" -eq 1 ] || usage
env_file=$1
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "environment file must be a regular non-symlink file"

read_env_value() {
  local key=$1 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$env_file")
  [ "$count" -eq 1 ] || die "environment file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$env_file")
  [ -n "$value" ] || die "$key is empty"
  printf '%s' "$value"
}

mode=$(read_env_value DIREXTALK_MESSAGE_TLS_MODE)
[ "$mode" = external ] || die "production requires DIREXTALK_MESSAGE_TLS_MODE=external; self-signed local TLS is not trusted"
cert=$(read_env_value DIREXTALK_MESSAGE_TLS_CERT_FILE)
key=$(read_env_value DIREXTALK_MESSAGE_TLS_KEY_FILE)
server_name=$(read_env_value DIREXTALK_MESSAGE_SERVER_NAME)
case "$cert:$key" in
  /*:/*) ;;
  *) die "TLS certificate and key paths must be absolute" ;;
esac
[ -f "$cert" ] && [ ! -L "$cert" ] || die "TLS certificate must be a regular non-symlink file"
[ -f "$key" ] && [ ! -L "$key" ] || die "TLS private key must be a regular non-symlink file"
key_mode=$(stat -c '%a' "$key")
cert_mode=$(stat -c '%a' "$cert")
[ "$key_mode" = 400 ] || die "TLS private key must be mode 0400"
case "$cert_mode" in
  400|440|444) ;;
  *) die "TLS certificate must not be group/world writable (use mode 0400, 0440, or 0444)" ;;
esac

openssl x509 -in "$cert" -noout >/dev/null 2>&1 || die "TLS certificate cannot be parsed"
openssl pkey -in "$key" -noout >/dev/null 2>&1 || die "TLS private key cannot be parsed"
openssl x509 -in "$cert" -checkend 604800 -noout >/dev/null 2>&1 || die "TLS certificate expires within seven days"
openssl x509 -in "$cert" -checkhost "$server_name" -noout >/dev/null 2>&1 || die "TLS certificate SAN/CN does not match $server_name"

cert_pub=$(openssl x509 -in "$cert" -pubkey -noout | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')
key_pub=$(openssl pkey -in "$key" -pubout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')
[ -n "$cert_pub" ] && [ "$cert_pub" = "$key_pub" ] || die "TLS certificate and private key do not match"

printf 'production TLS certificate, key, identity, and expiry checks passed\n'
