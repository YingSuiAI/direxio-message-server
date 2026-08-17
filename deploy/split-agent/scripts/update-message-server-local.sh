#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
die() { printf 'split message-server update: %s\n' "$*" >&2; exit 1; }
negative() { printf 'split message-server update: %s\n' "$*" >&2; exit 3; }
usage() { printf 'usage: %s OUTPUT_DIR target_version\n' "${0##*/}" >&2; exit 2; }
canonical_version() { printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; }
semver_ge() {
  local left=${1#v} right=${2#v} l1 l2 l3 r1 r2 r3
  IFS=. read -r l1 l2 l3 <<<"$left"; IFS=. read -r r1 r2 r3 <<<"$right"
  [ "$l1" -gt "$r1" ] || { [ "$l1" -eq "$r1" ] && { [ "$l2" -gt "$r2" ] || { [ "$l2" -eq "$r2" ] && [ "$l3" -ge "$r3" ]; }; }; }
}
read_pair() {
  local file=$1 key=$2 count value
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {n++} END {print n+0}' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {print substr($0,length(wanted)+2); exit}' "$file")
  [ -n "$value" ] || die "$file contains an empty $key"
  printf '%s' "$value"
}

[ "$#" -eq 2 ] || usage
out=$(readlink -m -- "$1")
target_version=$2
canonical_version "$target_version" || usage
required_owner=0
[ "${DIREXTALK_MESSAGE_SERVER_UPDATE_TEST_FIXTURE:-false}" != true ] || required_owner=$(id -u)
[ -d "$out" ] && [ ! -L "$out" ] && [ "$(stat -c '%a:%u' "$out")" = "700:$required_owner" ] || die 'OUTPUT_DIR must be a protected deployment directory'
env_file=$out/.env; manifest=$out/.manifest; receipt=$out/.cleanup-receipt
for file in "$env_file" "$manifest" "$receipt"; do
  [ -f "$file" ] && [ ! -L "$file" ] && [ "$(stat -c '%a:%u' "$file")" = "400:$required_owner" ] || die "invalid protected control file: $file"
done
command -v docker >/dev/null 2>&1 || die 'docker is required'
command -v jq >/dev/null 2>&1 || die 'jq is required'
command -v flock >/dev/null 2>&1 || die 'flock is required'
lock=$out/.message-server-update.lock
umask 077
: >>"$lock"; chmod 600 "$lock"; exec 9<>"$lock"; flock -n 9 || die 'another message-server update is running'
[ "$(read_pair "$manifest" compose_mode)" = production ] || negative 'message-server updates apply only to production stacks'
[ "$(read_pair "$receipt" state)" = complete ] || die 'cleanup receipt is incomplete'
stack=$(read_pair "$manifest" stack_name)
image=$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_IMAGE)
current_version=$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_VERSION)
canonical_version "$current_version" || die 'recorded message-server version is invalid'
[ "$image" = "docker.io/dirextalk/message-server:$current_version" ] || die 'message-server image does not match its recorded version tag'
target_image="docker.io/dirextalk/message-server:$target_version"
local_image_ref=${DIREXTALK_MESSAGE_SERVER_LOCAL_IMAGE_REF:-}
[ "$(id -u)" -eq 0 ] || [ -z "$local_image_ref" ] || [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_TEST_FIXTURE:-false}" = true ] || die 'local image apply requires root'
[[ "$local_image_ref" != *$'\n'* ]] || die 'local image ref contains a newline'

container_count=$(read_pair "$receipt" container.count)
message_id=
message_index=
for ((index=0; index<container_count; index++)); do
  if [ "$(read_pair "$receipt" "container.$index.service")" = message-server ]; then
    message_id=$(read_pair "$receipt" "container.$index.id"); message_index=$index
  fi
done
[ -n "$message_id" ] || die 'cleanup receipt lacks message-server'
data=$(docker inspect "$message_id" 2>/dev/null) || die 'recorded message-server container is unavailable'
[ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$image" ] || die 'running message-server does not use its recorded version tag'
old_image_id=$(jq -r '.[0].Image // empty' <<<"$data")
running_version=$(docker image inspect "$old_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null) || die 'running message-server version inspection failed'
[ "$running_version" = "$current_version" ] || die 'running message-server version differs from its receipt'
semver_ge "$current_version" "$target_version" && negative "message-server $target_version is not newer than running $current_version"

if [ -n "$local_image_ref" ]; then
  target_identity=$(docker image inspect "$local_image_ref" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || die 'local message-server image is unavailable'
else
  docker pull "$target_image" >/dev/null || die 'message-server target version pull failed'
  target_identity=$(docker image inspect "$target_image" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || die 'message-server target version inspection failed'
fi
IFS='|' read -r pulled_version target_image_id target_revision <<<"$target_identity"
[ "$pulled_version" = "$target_version" ] || die "target image is $pulled_version, expected $target_version"
printf '%s\n' "$target_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'target revision label is invalid'
[ "$(docker run --rm --entrypoint /usr/bin/dirextalk-message-server "$target_image_id" --version)" = "$target_version" ] || die 'message-server binary version mismatch'

compose=(docker compose --env-file "$env_file" -f "$stack_dir/compose.yaml" -f "$stack_dir/compose.production.yaml" --project-name "$stack")
rollback_needed=false
rollback_message_server() {
  local status=$? rollback_id rollback_receipt attempts data
  [ "$rollback_needed" = true ] || return "$status"
  trap - EXIT
  printf 'split message-server update: restoring previous local image after failed apply\n' >&2
  if DIREXTALK_MESSAGE_SERVER_IMAGE="$image" \
      "${compose[@]}" up -d --no-deps --force-recreate --no-build --pull never message-server >/dev/null 2>&1; then
    attempts=${DIREXTALK_MESSAGE_SERVER_UPDATE_HEALTH_ATTEMPTS:-60}
    while [ "$attempts" -gt 0 ]; do
      rollback_id=$("${compose[@]}" ps -q message-server 2>/dev/null || true)
      data=$(docker inspect "$rollback_id" 2>/dev/null || true)
      if [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$old_image_id" ] && [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ]; then
        rollback_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
        awk -F= -v service_index="$message_index" -v id="$rollback_id" \
          '$1==("container." service_index ".id") {$0=$1 "=" id} {print}' "$receipt" >"$rollback_receipt" &&
          chmod 400 "$rollback_receipt" && mv -f "$rollback_receipt" "$receipt"
        return "$status"
      fi
      attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1
    done
  fi
  printf 'split message-server update: previous local image restoration failed\n' >&2
  return 1
}
trap rollback_message_server EXIT
if [ -n "$local_image_ref" ]; then
  docker image tag "$target_image_id" "$target_image" >/dev/null || die 'could not bind local message-server image to the target version tag'
fi
rollback_needed=true
DIREXTALK_MESSAGE_SERVER_IMAGE="$target_image" \
  "${compose[@]}" up -d --no-deps --force-recreate --no-build --pull never message-server >/dev/null || die 'message-server recreate failed'
attempts=${DIREXTALK_MESSAGE_SERVER_UPDATE_HEALTH_ATTEMPTS:-60}
while [ "$attempts" -gt 0 ]; do
  new_id=$("${compose[@]}" ps -q message-server 2>/dev/null || true)
  if [ -n "$new_id" ]; then
    data=$(docker inspect "$new_id" 2>/dev/null || true)
    if [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$target_image_id" ] && [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ]; then break; fi
  fi
  attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1
done
[ "$attempts" -gt 0 ] || die 'updated message-server did not become healthy'
[ "$(docker exec "$new_id" /usr/bin/dirextalk-message-server --version)" = "$target_version" ] || die 'running message-server binary version mismatch'

new_env=$(mktemp "$out/.env.XXXXXX")
awk -F= -v image="$target_image" -v version="$target_version" -v revision="$target_revision" '
  $1=="DIREXTALK_MESSAGE_SERVER_IMAGE" {$0=$1 "=" image; image_seen=1}
  $1=="DIREXTALK_MESSAGE_SERVER_VERSION" {$0=$1 "=" version; version_seen=1}
  $1=="DIREXTALK_MESSAGE_SOURCE_REVISION" {$0=$1 "=" revision; revision_seen=1}
  {print}
  END {if (!image_seen || !version_seen || !revision_seen) exit 1}
' "$env_file" >"$new_env" || die 'could not update expected message-server version'
chmod 400 "$new_env"
new_env_identity=$(stat -c '%d:%i:%u' "$new_env"); new_env_sha=$(sha256sum "$new_env" | awk '{print $1}')
new_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
awk -F= -v service_index="$message_index" -v id="$new_id" -v identity="$new_env_identity" -v digest="$new_env_sha" '
  $1=="control.env_identity" {$0=$1 "=" identity}
  $1=="control.env_sha256" {$0=$1 "=" digest}
  $1==("container." service_index ".id") {$0=$1 "=" id}
  {print}
' "$receipt" >"$new_receipt"
chmod 400 "$new_receipt"
mv -f "$new_env" "$env_file"
mv -f "$new_receipt" "$receipt"
rollback_needed=false
trap - EXIT
if [ "$old_image_id" != "$target_image_id" ] && ! docker ps -aq --filter "ancestor=$old_image_id" | grep -q .; then docker image rm "$old_image_id" >/dev/null 2>&1 || true; fi
printf 'split message-server update passed: version=%s image=%s revision=%s\n' "$target_version" "$target_image" "$target_revision"
