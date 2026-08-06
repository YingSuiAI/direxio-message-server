#!/usr/bin/env bash
# Receipt-bound Agent runtime update adapter. The host updater supplies the
# canonical target and minimum message-server versions from its authenticated,
# persisted release plan; repository, Compose path, services, image reference,
# and health order remain code-owned.
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
compose_file=$(cd "$script_dir/.." && pwd -P)/compose.yaml

die() { printf 'split-agent update: %s\n' "$*" >&2; exit 1; }
negative() { printf 'split-agent update: %s\n' "$*" >&2; exit 3; }
usage() { printf 'usage: %s OUTPUT_DIR target_version minimum_server_version\n' "${0##*/}" >&2; exit 2; }
read_pair() {
  local file=$1 key=$2 count value
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {n++} END {print n+0}' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {print substr($0,length(wanted)+2); exit}' "$file")
  [ -n "$value" ] || die "$file contains an empty $key"
  printf '%s' "$value"
}
semver_ge() {
  local left=${1#v} right=${2#v} l1 l2 l3 r1 r2 r3
  IFS=. read -r l1 l2 l3 <<<"$left"; IFS=. read -r r1 r2 r3 <<<"$right"
  [ "$l1" -gt "$r1" ] || { [ "$l1" -eq "$r1" ] && { [ "$l2" -gt "$r2" ] || { [ "$l2" -eq "$r2" ] && [ "$l3" -ge "$r3" ]; }; }; }
}
canonical_version() { printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; }

[ "$#" -eq 3 ] || usage
out=$(readlink -m -- "$1")
target_version=$2
minimum_server_version=$3
canonical_version "$target_version" || usage
canonical_version "$minimum_server_version" || usage
[ -d "$out" ] && [ ! -L "$out" ] && [ "$(stat -c '%a' "$out")" = 700 ] || die 'OUTPUT_DIR must be a mode-0700 non-symlink directory'
env_file=$out/.env; manifest=$out/.manifest; receipt=$out/.cleanup-receipt
for file in "$env_file" "$manifest" "$receipt"; do
  [ -f "$file" ] && [ ! -L "$file" ] && [ "$(stat -c '%a' "$file")" = 400 ] || die "invalid protected control file: $file"
  [ "$(stat -c '%u' "$file")" = "$(id -u)" ] || die "control file owner mismatch: $file"
done
grep -Fqx '# dirextalk-split-cleanup-receipt-v1' "$receipt" || die 'cleanup receipt version is unsupported'
[ "$(read_pair "$receipt" state)" = complete ] || die 'cleanup receipt is incomplete'
[ "$(read_pair "$receipt" control.env_identity)" = "$(stat -c '%d:%i:%u' "$env_file")" ] || die '.env identity differs from receipt'
[ "$(read_pair "$receipt" control.manifest_identity)" = "$(stat -c '%d:%i:%u' "$manifest")" ] || die 'manifest identity differs from receipt'
[ "$(read_pair "$receipt" control.env_sha256)" = "$(sha256sum "$env_file" | awk '{print $1}')" ] || die '.env digest differs from receipt'
[ "$(read_pair "$receipt" control.manifest_sha256)" = "$(sha256sum "$manifest" | awk '{print $1}')" ] || die 'manifest digest differs from receipt'
[ "$(read_pair "$manifest" compose_mode)" = production ] || negative 'Agent release updates apply only to production stacks'
stack=$(read_pair "$manifest" stack_name)
[ "$stack" = "$(read_pair "$receipt" stack_name)" ] || die 'stack identity mismatch'

container_count=$(read_pair "$receipt" container.count)
message_id=''
declare -A old_ids=()
for ((i=0;i<container_count;i++)); do
  service=$(read_pair "$receipt" "container.$i.service")
  id=$(read_pair "$receipt" "container.$i.id")
  case "$service" in
    message-server) message_id=$id ;;
    agent|extension-runner|core-runner) old_ids[$service]=$id ;;
  esac
done
[ -n "$message_id" ] && [ "${#old_ids[@]}" -eq 3 ] || die 'receipt lacks the fixed message/Agent services'
message_data=$(docker inspect "$message_id" 2>/dev/null) || die 'recorded message-server container is unavailable'
[ "$(jq -r '.[0].Id // empty' <<<"$message_data")" = "$message_id" ] || die 'message-server container identity changed'
message_image_id=$(jq -r '.[0].Image // empty' <<<"$message_data")
server_version=$(docker image inspect "$message_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null) || die 'message-server image version inspection failed'
canonical_version "$server_version" || die 'running message-server version is not canonical'
semver_ge "$server_version" "$minimum_server_version" || negative "target requires message-server $minimum_server_version (running $server_version)"

current_ref=$(read_pair "$env_file" DIREXTALK_AGENT_IMAGE_IMMUTABLE)
printf '%s\n' "$current_ref" | grep -Eq '^(docker\.io/)?dirextalk/agent@sha256:[0-9a-f]{64}$' || die 'current Agent image is not the fixed immutable repository'
current_image_id=''
for service in agent extension-runner core-runner; do
  data=$(docker inspect "${old_ids[$service]}" 2>/dev/null) || die "recorded $service container is unavailable"
  [ "$(jq -r '.[0].Id // empty' <<<"$data")" = "${old_ids[$service]}" ] || die "$service container identity changed"
  [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$current_ref" ] || die "$service does not use the protected Agent image reference"
  observed=$(jq -r '.[0].Image // empty' <<<"$data")
  [ -z "$current_image_id" ] && current_image_id=$observed
  [ "$observed" = "$current_image_id" ] || die 'Agent runtime containers do not use one image ID'
done
current_version=$(docker image inspect "$current_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null) || die 'current Agent version inspection failed'
canonical_version "$current_version" || die 'current Agent image version is not canonical'
semver_ge "$current_version" "$target_version" && negative "Agent $target_version is not newer than running $current_version"

target_tag=docker.io/dirextalk/agent:$target_version
docker pull "$target_tag" >/dev/null || die 'Agent image pull failed'
target_identity=$(docker image inspect "$target_tag" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null) || die 'target Agent image inspection failed'
target_label=${target_identity%%|*}; rest=${target_identity#*|}; target_id=${rest%%|*}; digests=${rest#*|}
[ "$target_label" = "$target_version" ] || die 'target Agent image version label mismatch'
target_ref=$(printf '%s\n' "$digests" | awk '$0 ~ /^(docker\.io\/)?dirextalk\/agent@sha256:[0-9a-f]{64}$/ {print; exit}')
[ -n "$target_ref" ] || die 'target Agent image has no immutable repository digest'

old_env=$(mktemp "$out/.agent-update-old-env.XXXXXX")
rm -f "$old_env"
ln -- "$env_file" "$old_env" || die 'could not preserve the exact pre-update environment identity'
old_receipt=$(mktemp "$out/.agent-update-old-receipt.XXXXXX")
rm -f "$old_receipt"
ln -- "$receipt" "$old_receipt" || { rm -f "$old_env"; die 'could not preserve the exact pre-update receipt identity'; }
new_env=''
new_receipt=''
mutated=false
commit_complete=false
rollback() {
  local status=0
  [ "$mutated" = true ] || return 0
  DIREXTALK_AGENT_IMAGE_IMMUTABLE=$current_ref docker compose --env-file "$old_env" -f "$compose_file" --project-name "$stack" \
    up -d --no-deps --force-recreate extension-runner core-runner >/dev/null || status=1
  wait_services "$current_image_id" "$current_ref" extension-runner core-runner || status=1
  DIREXTALK_AGENT_IMAGE_IMMUTABLE=$current_ref docker compose --env-file "$old_env" -f "$compose_file" --project-name "$stack" \
    up -d --no-deps --force-recreate agent >/dev/null || status=1
  wait_services "$current_image_id" "$current_ref" agent || status=1
  return "$status"
}
wait_services() {
  local expected=$1 expected_ref=$2 service id data attempts
  shift 2
  for service in "$@"; do
    attempts=${DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS:-60}
    while [ "$attempts" -gt 0 ]; do
      id=$(docker compose --env-file "$env_file" -f "$compose_file" --project-name "$stack" ps -q "$service" 2>/dev/null) || return 1
      if [ -n "$id" ]; then
        data=$(docker inspect "$id" 2>/dev/null) || return 1
        if [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$expected" ] && \
           [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$expected_ref" ] && \
           [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] && \
           [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ]; then break; fi
        [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" != unhealthy ] || return 1
      fi
      attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1
    done
    [ "$attempts" -gt 0 ] || return 1
  done
}

restore_control_files() {
  local status=0
  if [ -e "$old_env" ]; then
    if [ -e "$env_file" ] && [ "$old_env" -ef "$env_file" ]; then
      rm -f "$old_env" || status=1
    else
      mv -f "$old_env" "$env_file" || status=1
    fi
  fi
  if [ -e "$old_receipt" ]; then
    if [ -e "$receipt" ] && [ "$old_receipt" -ef "$receipt" ]; then
      rm -f "$old_receipt" || status=1
    else
      mv -f "$old_receipt" "$receipt" || status=1
    fi
  fi
  return "$status"
}

transaction_exit() {
  local status=$? recovery_status=0
  trap - EXIT
  set +e
  if [ "$mutated" = true ] && [ "$commit_complete" = false ]; then
    rollback || recovery_status=1
    restore_control_files || recovery_status=1
    if [ "$recovery_status" -ne 0 ]; then
      printf 'split-agent update: update transaction failed and exact rollback did not complete\n' >&2
      status=1
    fi
  fi
  [ -z "$new_env" ] || rm -f "$new_env"
  [ -z "$new_receipt" ] || rm -f "$new_receipt"
  rm -f "$old_env" "$old_receipt"
  exit "$status"
}
trap transaction_exit EXIT

stop_wrapper=$script_dir/stop-agent-local.sh
if [ "${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}" = true ] && [ -n "${DIREXTALK_AGENT_UPDATE_STOP_WRAPPER:-}" ]; then
  stop_wrapper=$DIREXTALK_AGENT_UPDATE_STOP_WRAPPER
fi
if "$stop_wrapper" "$out" >/dev/null; then
  :
else
  status=$?
  [ "$status" -eq 3 ] && negative 'Agent runtime is already stopped'
  die 'Agent runtime stop failed'
fi
mutated=true
if ! DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref docker compose --env-file "$env_file" -f "$compose_file" --project-name "$stack" run --rm --no-deps agent-migrate >/dev/null ||
   ! DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref docker compose --env-file "$env_file" -f "$compose_file" --project-name "$stack" up -d --no-deps --force-recreate extension-runner core-runner >/dev/null ||
   ! wait_services "$target_id" "$target_ref" extension-runner core-runner ||
   ! DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref docker compose --env-file "$env_file" -f "$compose_file" --project-name "$stack" up -d --no-deps --force-recreate agent >/dev/null ||
   ! wait_services "$target_id" "$target_ref" agent; then
  die 'target Agent update failed'
fi

new_env=$(mktemp "$out/.env.XXXXXX")
awk -F= -v replacement="$target_ref" '$1=="DIREXTALK_AGENT_IMAGE_IMMUTABLE" {$0="DIREXTALK_AGENT_IMAGE_IMMUTABLE=" replacement} {print}' "$env_file" >"$new_env"
chmod 400 "$new_env"
new_env_identity=$(stat -c '%d:%i:%u' "$new_env"); new_env_sha=$(sha256sum "$new_env" | awk '{print $1}')
declare -A new_ids=()
for service in agent extension-runner core-runner; do
  new_ids[$service]=$(docker compose --env-file "$env_file" -f "$compose_file" --project-name "$stack" ps -q "$service")
  printf '%s\n' "${new_ids[$service]}" | grep -Eq '^[0-9a-f]{64}$' || die 'new container identity is invalid'
done
new_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
python3 - "$receipt" "$new_receipt" "$new_env_identity" "$new_env_sha" \
  "${new_ids[agent]}" "${new_ids[extension-runner]}" "${new_ids[core-runner]}" <<'PY'
import pathlib,sys
source,dest,identity,digest,*ids=sys.argv[1:]
mapping=dict(zip(("agent","extension-runner","core-runner"),ids))
lines=pathlib.Path(source).read_text().splitlines()
services={}
for line in lines:
    if line.startswith("container.") and ".service=" in line:
        key,value=line.split("=",1); services[key.split(".")[1]]=value
out=[]
for line in lines:
    if line.startswith("control.env_identity="): line="control.env_identity="+identity
    elif line.startswith("control.env_sha256="): line="control.env_sha256="+digest
    elif line.startswith("container.") and ".id=" in line:
        key=line.split("=",1)[0]; index=key.split(".")[1]; service=services.get(index)
        if service in mapping: line=key+"="+mapping[service]
    out.append(line)
pathlib.Path(dest).write_text("\n".join(out)+"\n")
PY
chmod 400 "$new_receipt"
mv -f "$new_env" "$env_file"
new_env=''
if [ "${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}" = true ] && [ "${DIREXTALK_AGENT_UPDATE_FAIL_RECEIPT_COMMIT:-false}" = true ]; then
  die 'injected receipt commit failure'
fi
mv -f "$new_receipt" "$receipt"
new_receipt=''
commit_complete=true
mutated=false
trap - EXIT
rm -f "$old_env" "$old_receipt"
printf 'split-agent update passed: version=%s image=%s\n' "$target_version" "$target_ref"
