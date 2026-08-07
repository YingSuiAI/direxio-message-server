#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

die() {
  echo "split PostgreSQL entrypoint: $*" >&2
  exit 1
}

source_dir=/run/secrets
materialized_dir=/run/dirextalk-postgres-secrets
official_entrypoint=/usr/local/bin/docker-entrypoint.sh

[ "$(id -u)" -eq 0 ] || die "secret materialization requires container root"
[ -x "$official_entrypoint" ] && [ ! -L "$official_entrypoint" ] || die "official PostgreSQL entrypoint is unavailable"
postgres_uid=$(id -u postgres) || die "official postgres user is unavailable"
postgres_gid=$(id -g postgres) || die "official postgres group is unavailable"

[ -d "$materialized_dir" ] && [ ! -L "$materialized_dir" ] || die "materialized secret tmpfs is unavailable"
materialized_metadata=$(stat -c '%u:%g:%a' -- "$materialized_dir") || die "materialized secret tmpfs metadata is unavailable"
case "$materialized_metadata" in
  0:0:700|"0:$postgres_gid:750") ;;
  *) die "materialized secret tmpfs has unsafe ownership or mode" ;;
esac
chown "0:$postgres_gid" -- "$materialized_dir"
chmod 0750 -- "$materialized_dir"

materialize_secret() {
  local name=$1 source target source_identity
  source=$source_dir/$name
  target=$materialized_dir/$name
  [ -f "$source" ] && [ ! -L "$source" ] || die "$name source is not a regular non-symlink file"
  [ "$(stat -c '%a' -- "$source")" = 400 ] || die "$name source must be mode 0400"
  source_identity=$(stat -c '%d:%i:%u:%g:%a' -- "$source") || die "$name source metadata is unavailable"

  if [ -e "$target" ] || [ -L "$target" ]; then
    [ -f "$target" ] && [ ! -L "$target" ] || die "$name materialized target is unsafe"
    [ "$(stat -c '%u:%g:%a' -- "$target")" = "$postgres_uid:$postgres_gid:400" ] || \
      die "$name materialized target has unsafe ownership or mode"
  fi

  [ "$(stat -c '%d:%i:%u:%g:%a' -- "$source")" = "$source_identity" ] || die "$name source identity changed before materialization"
  install -o "$postgres_uid" -g "$postgres_gid" -m 0400 -- "$source" "$target"
  [ "$(stat -c '%d:%i:%u:%g:%a' -- "$source")" = "$source_identity" ] || die "$name source identity changed during materialization"
  [ -f "$target" ] && [ ! -L "$target" ] || die "$name materialized target is not a regular non-symlink file"
  [ "$(stat -c '%u:%g:%a' -- "$target")" = "$postgres_uid:$postgres_gid:400" ] || \
    die "$name materialized target ownership or mode is incorrect"
  cmp -s -- "$source" "$target" || die "$name materialized content differs from its source"
  [ "$(stat -c '%d:%i:%u:%g:%a' -- "$source")" = "$source_identity" ] || die "$name source identity changed during verification"
}

materialize_secret postgres_admin_password
materialize_secret message_postgres_password
materialize_secret agent_postgres_password

[ "${POSTGRES_PASSWORD_FILE:-}" = "$materialized_dir/postgres_admin_password" ] || \
  die "POSTGRES_PASSWORD_FILE must use the materialized admin secret"
[ "${POSTGRES_INIT_SECRET_DIR:-}" = "$materialized_dir" ] || \
  die "POSTGRES_INIT_SECRET_DIR must use the materialized secret directory"

# Restore the official image's startup umask before it creates the PostgreSQL
# 18 version parent and drops privileges. Materialized files already have
# explicit private modes.
umask 022
exec "$official_entrypoint" "$@"
