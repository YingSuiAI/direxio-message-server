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
