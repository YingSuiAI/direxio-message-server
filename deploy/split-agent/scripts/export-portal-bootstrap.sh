#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 OUTPUT_DIR DESTINATION_FILE" >&2
  exit 2
}

die() {
  echo "portal bootstrap export: $*" >&2
  exit 1
}

[ "$#" -eq 2 ] || usage
out_input=$1
destination_input=$2
case "$out_input" in
  /*) out=$(readlink -m -- "$out_input") ;;
  *) out=$(readlink -m -- "$(pwd -P)/$out_input") ;;
esac
case "$destination_input" in
  /*) destination=$(readlink -m -- "$destination_input") ;;
  *) destination=$(readlink -m -- "$(pwd -P)/$destination_input") ;;
esac
[ -d "$out" ] && [ ! -L "$out" ] || die "output directory must be a regular non-symlink directory"
env_file=$out/.env
manifest=$out/.manifest
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "output directory is not a provisioned split stack"
[ -f "$manifest" ] && [ ! -L "$manifest" ] || die "output directory has no immutable stack manifest"
[ ! -e "$destination" ] || die "destination already exists; choose a fresh audit path"

read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}
stack_name=$(read_pair "$manifest" stack_name)
env_stack=$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)
[ "$stack_name" = "$env_stack" ] || die ".env stack identity differs from the manifest"
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "stack identity is not a generated immutable namespace"

destination_parent=${destination%/*}
[ -n "$destination_parent" ] || destination_parent=/
if [ -e "$destination_parent" ]; then
  [ -d "$destination_parent" ] && [ ! -L "$destination_parent" ] || die "destination parent must be a regular non-symlink directory"
  [ "$destination_parent" != "/" ] || die "refusing to write credentials directly under the filesystem root"
  [ "$(stat -c '%a' "$destination_parent")" = 700 ] || die "destination parent must be mode 0700"
else
  mkdir -p "$destination_parent"
  chmod 700 "$destination_parent"
fi
tmp_file=$(mktemp "$destination_parent/.portal-bootstrap.XXXXXX")
chmod 400 "$tmp_file"
cleanup() {
  rm -f "$tmp_file"
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || die "docker is required"
container=$(docker compose --project-name "$stack_name" --env-file "$env_file" \
  -f "$(cd "$(dirname "$0")/.." && pwd -P)/compose.yaml" ps -q message-server 2>/dev/null || true)
[ -n "$container" ] || die "message-server is not running for this stack"
project=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$container" 2>/dev/null || true)
[ "$project" = "$stack_name" ] || die "message-server container ownership does not match the manifest"
docker cp "$container:/var/dirextalk-message-server/p2p/bootstrap.json" "$tmp_file" >/dev/null 2>&1 || die "portal bootstrap credentials are not available"
chmod 400 "$tmp_file"
jq -e 'type == "object" and (.access_token | type == "string" and length > 0) and (.agent_token | type == "string" and length > 0) and (.password | type == "string" and length > 0) and (.owner_user_id | type == "string" and length > 0)' "$tmp_file" >/dev/null || die "portal bootstrap JSON is incomplete"
mv -- "$tmp_file" "$destination"
trap - EXIT
chmod 400 "$destination"
printf 'portal bootstrap credentials exported to %s (contents were not printed)\n' "$destination"
