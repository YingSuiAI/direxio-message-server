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
current_uid=$(id -u) || die "cannot determine current UID"
env_identity=$(stat -c '%d %i %u %a' -- "$env_file") || die "environment file metadata could not be read"
env_metadata=$(stat -c '%u %a' -- "$env_file") || die "environment file metadata could not be read"
env_type=$(stat -c '%F' -- "$env_file") || die "environment file type could not be read"
read -r env_uid env_mode <<<"$env_metadata"
[ "$env_type" = "regular file" ] || die "environment file must be a regular file"
[ "$env_uid" = "$current_uid" ] || die "environment file must be owned by the provisioning user"
[ "$env_mode" = 400 ] || die "environment file must have mode 0400"

read_env_value() {
  local key=$1 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$env_file")
  [ "$count" -eq 1 ] || die "environment file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$env_file")
  [ -n "$value" ] || die "$key is empty"
  printf '%s' "$value"
}

mode=$(read_env_value DIREXTALK_MESSAGE_TLS_MODE)
cert=$(read_env_value DIREXTALK_MESSAGE_TLS_CERT_FILE)
key=$(read_env_value DIREXTALK_MESSAGE_TLS_KEY_FILE)
server_name=$(read_env_value DIREXTALK_MESSAGE_SERVER_NAME)
client_base_url=$(read_env_value DIREXTALK_MESSAGE_CLIENT_BASE_URL)
printf '%s\n' "$server_name" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$' || \
  die "DIREXTALK_MESSAGE_SERVER_NAME must be a DNS host name without a scheme, port, or wildcard"
[ "$client_base_url" = "https://$server_name" ] || die "DIREXTALK_MESSAGE_CLIENT_BASE_URL must be the canonical https:// server URL"
case "$cert:$key" in
  /*:/*) ;;
  *) die "TLS certificate and key paths must be absolute" ;;
esac
case "$cert:$key" in
  *'//'*) die "TLS certificate and key paths must be clean absolute paths" ;;
  *'..'*) die "TLS certificate and key paths must not contain parent traversal" ;;
  *[[:space:]]*) die "TLS certificate and key paths must not contain whitespace" ;;
  *[[:cntrl:]]*) die "TLS certificate and key paths must not contain control bytes" ;;
esac
[ -f "$cert" ] && [ ! -L "$cert" ] || die "TLS certificate must be a regular non-symlink file"
[ -f "$key" ] && [ ! -L "$key" ] || die "TLS private key must be a regular non-symlink file"
key_mode=$(stat -c '%a' "$key")
cert_mode=$(stat -c '%a' "$cert")
[ "$key_mode" = 400 ] || die "TLS private key must be mode 0400"
[ "$cert_mode" = 400 ] || die "TLS certificate must be mode 0400"
cert_identity=$(stat -c '%d %i %u %a' -- "$cert") || die "TLS certificate identity could not be read"
key_identity=$(stat -c '%d %i %u %a' -- "$key") || die "TLS private key identity could not be read"
cert_type=$(stat -c '%F' -- "$cert") || die "TLS certificate type could not be read"
key_type=$(stat -c '%F' -- "$key") || die "TLS private key type could not be read"
cert_uid=$(awk '{print $3}' <<<"$cert_identity")
cert_mode=$(awk '{print $4}' <<<"$cert_identity")
key_uid=$(awk '{print $3}' <<<"$key_identity")
key_mode=$(awk '{print $4}' <<<"$key_identity")
case "$cert_type:$key_type" in
  regular\ file:regular\ file|regular\ empty\ file:regular\ empty\ file|regular\ file:regular\ empty\ file|regular\ empty\ file:regular\ file) ;;
  *) die "TLS certificate or private key type is invalid" ;;
esac
[ "$cert_uid" = "$current_uid" ] || die "TLS certificate owner is invalid"
[ "$key_uid" = "$current_uid" ] || die "TLS private key owner is invalid"

if [ "$mode" = edge-terminated ]; then
  [ ! -s "$cert" ] || die "edge-terminated mode forbids a message-server TLS certificate"
  [ ! -s "$key" ] || die "edge-terminated mode forbids a message-server TLS private key"
  [ "$(stat -c '%d %i %u %a' -- "$cert")" = "$cert_identity" ] || die "TLS certificate placeholder identity changed during verification"
  [ "$(stat -c '%d %i %u %a' -- "$key")" = "$key_identity" ] || die "TLS private-key placeholder identity changed during verification"
  [ "$(stat -c '%d %i %u %a' -- "$env_file")" = "$env_identity" ] || die "environment file identity changed during verification"
  printf 'production edge-terminated TLS contract checks passed\n'
  exit 0
fi
[ "$mode" = external ] || die "production requires DIREXTALK_MESSAGE_TLS_MODE=external or edge-terminated; self-signed local TLS is not trusted"

openssl x509 -in "$cert" -noout >/dev/null 2>&1 || die "TLS certificate cannot be parsed"
openssl pkey -in "$key" -noout >/dev/null 2>&1 || die "TLS private key cannot be parsed"
openssl x509 -in "$cert" -checkend 604800 -noout >/dev/null 2>&1 || die "TLS certificate expires within seven days"
if cert_host_check=$(openssl x509 -in "$cert" -checkhost "$server_name" -noout 2>/dev/null); then
  case "$cert_host_check" in
    *" does match certificate") ;;
    *) die "TLS certificate SAN/CN does not match $server_name" ;;
  esac
else
  die "TLS certificate SAN/CN could not be checked"
fi

cert_pub=$(openssl x509 -in "$cert" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')
key_pub=$(openssl pkey -in "$key" -pubout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')
[ -n "$cert_pub" ] && [ "$cert_pub" = "$key_pub" ] || die "TLS certificate and private key do not match"

[ -f "$cert" ] && [ ! -L "$cert" ] || die "TLS certificate was replaced during verification"
[ -f "$key" ] && [ ! -L "$key" ] || die "TLS private key was replaced during verification"
[ "$(stat -c '%d %i %u %a' -- "$cert")" = "$cert_identity" ] || die "TLS certificate identity changed during verification"
[ "$(stat -c '%d %i %u %a' -- "$key")" = "$key_identity" ] || die "TLS private key identity changed during verification"
[ "$(stat -c '%d %i %u %a' -- "$env_file")" = "$env_identity" ] || die "environment file identity changed during verification"

printf 'production TLS certificate, key, identity, and expiry checks passed\n'
