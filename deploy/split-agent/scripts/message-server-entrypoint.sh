#!/bin/sh
set -eu

# The database URL is intentionally read only from a mounted secret file. Do
# not replace this with an external interpolation command: that command would
# receive the complete DSN in its argv and make it observable through process
# inspection. The POSIX shell parameter expansions and printf below are
# builtins, so the DSN remains in this process until the message-server reads
# the generated file.

die() {
  printf 'message-server entrypoint: %s\n' "$*" >&2
  exit 1
}

template=${MESSAGE_SERVER_CONFIG_TEMPLATE:-/etc/dirextalk-message-server/message-server.yaml}
secret=${MESSAGE_SERVER_DATABASE_URL_FILE:-/run/secrets/message_database_url}
output=${MESSAGE_SERVER_RUNTIME_CONFIG:-/tmp/message-server.yaml}
binary=${MESSAGE_SERVER_BINARY:-/usr/bin/dirextalk-message-server}

[ -r "$template" ] || die "config template is not readable: $template"
[ -r "$secret" ] || die "database URL secret is not readable: $secret"
[ "$output" != "$template" ] || die "runtime config must be separate from the template"

tls_mode=${MESSAGE_SERVER_TLS_MODE:?set MESSAGE_SERVER_TLS_MODE}
https_flag=false
cert_flag=false
key_flag=false
for arg in "$@"; do
  case "$arg" in
    --https-bind-address) https_flag=true ;;
    --tls-cert) cert_flag=true ;;
    --tls-key) key_flag=true ;;
    --https-bind-address=*|--tls-cert=*|--tls-key=*) die "TLS listener flags must use the canonical separate-argument form" ;;
  esac
done
case "$tls_mode" in
  edge-terminated)
    [ "$https_flag:$cert_flag:$key_flag" = false:false:false ] || die "edge-terminated mode forbids direct TLS listener flags"
    ;;
  local|external)
    [ "$https_flag:$cert_flag:$key_flag" = true:true:true ] || die "$tls_mode mode requires the direct-TLS Compose overlay"
    ;;
  *) die "MESSAGE_SERVER_TLS_MODE must be local, external, or edge-terminated" ;;
esac

# Command substitution strips trailing newlines.  Reject any other
# non-printable byte so the value cannot break the generated YAML scalar.
db_url=$(cat "$secret")
[ -n "$db_url" ] || die "database URL secret is empty"
case "$db_url" in
  *[![:print:]]*) die "database URL secret contains a non-printable byte" ;;
esac

count=0
umask 077
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    *__DIREXTALK_DB_DSN__*)
      count=$((count + 1))
      prefix=${line%%__DIREXTALK_DB_DSN__*}
      suffix=${line#*__DIREXTALK_DB_DSN__}
      printf '%s%s%s\n' "$prefix" "$db_url" "$suffix"
      ;;
    *)
      printf '%s\n' "$line"
      ;;
  esac
done <"$template" >"$output"

[ "$count" -eq 1 ] || die "expected exactly one database URL marker, found $count"
chmod 0400 "$output"

exec "$binary" --config "$output" "$@"
