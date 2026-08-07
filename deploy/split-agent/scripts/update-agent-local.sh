#!/usr/bin/env bash
# Receipt-bound Agent runtime update adapter. The host updater supplies the
# canonical target and minimum message-server versions from its authenticated,
# persisted release plan; repository, Compose path, services, image reference,
# and health order remain code-owned.
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
compose_file=$stack_dir/compose.yaml
production_compose_file=$stack_dir/compose.production.yaml
production_image_gate=$script_dir/verify-production-images.sh

die() { printf 'split-agent update: %s\n' "$*" >&2; exit 1; }
negative() { printf 'split-agent update: %s\n' "$*" >&2; exit 3; }
usage() { printf 'usage: %s OUTPUT_DIR target_version minimum_server_version\n' "${0##*/}" >&2; exit 1; }
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
test_fixture=${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}
case "$test_fixture" in true|false) ;; *) usage ;; esac
required_owner=0
required_group=0
if [ "$test_fixture" = true ]; then required_owner=$(id -u); required_group=$(id -g); fi
canonical_version "$target_version" || usage
canonical_version "$minimum_server_version" || usage
[ -d "$out" ] && [ ! -L "$out" ] && [ "$(stat -c '%a' "$out")" = 700 ] || die 'OUTPUT_DIR must be a mode-0700 non-symlink directory'
[ "$(stat -c '%u' "$out")" = "$required_owner" ] || die 'OUTPUT_DIR owner mismatch'
dir_identity=$(stat -c '%d:%i:%u:%g:%a' "$out")
verify_output_dir_identity() {
  [ -d "$out" ] && [ ! -L "$out" ] && [ "$(stat -c '%d:%i:%u:%g:%a' "$out")" = "$dir_identity" ] || die 'OUTPUT_DIR identity changed during Agent update'
}
durable_replace() {
  local source=$1 destination=$2
  verify_output_dir_identity
  sync -f "$source" || return 1
  verify_output_dir_identity
  mv -f -- "$source" "$destination" || return 1
  sync -f "$destination" || return 1
  sync -f "$out" || return 1
}
durable_remove() {
  local path=$1
  verify_output_dir_identity
  rm -f -- "$path" || return 1
  sync -f "$out" || return 1
}
shopt -s nullglob
failure_records=("$out"/.agent-update-failure.*)
shopt -u nullglob
for failure_record in "${failure_records[@]}"; do
  [ -f "$failure_record" ] && [ ! -L "$failure_record" ] || die 'Agent update failure evidence is not a regular protected file'
  [ "$(stat -c '%u:%a' "$failure_record")" = "$(id -u):400" ] || die 'Agent update failure evidence owner or mode is invalid'
done
[ "${#failure_records[@]}" -eq 0 ] || die 'unfinished Agent update failure evidence requires explicit operator recovery'
env_file=$out/.env; manifest=$out/.manifest; receipt=$out/.cleanup-receipt; attestation=$out/image-attestation
for file in "$env_file" "$manifest" "$receipt" "$attestation"; do
  [ -f "$file" ] && [ ! -L "$file" ] && [ "$(stat -c '%a' "$file")" = 400 ] || die "invalid protected control file: $file"
  [ "$(stat -c '%u' "$file")" = "$(id -u)" ] || die "control file owner mismatch: $file"
done
command -v flock >/dev/null 2>&1 || die 'flock is required'
agent_journal=$out/.agent-update.transaction
lock_file=$out/.agent-update.lock
if [ -e "$lock_file" ] || [ -L "$lock_file" ]; then
  [ -f "$lock_file" ] && [ ! -L "$lock_file" ] || die 'Agent update lock must be a regular non-symlink file'
else
  old_umask=$(umask)
  umask 077
  if ! (set -o noclobber; : >"$lock_file") 2>/dev/null; then
    umask "$old_umask"
    die 'could not atomically create the Agent update lock'
  fi
  umask "$old_umask"
  chmod 600 -- "$lock_file" || die 'could not protect the Agent update lock'
  chown "$required_owner:$required_group" -- "$lock_file" || die 'could not bind the Agent update lock owner'
fi
[ "$(stat -c '%u:%g:%a' -- "$lock_file")" = "$required_owner:$required_group:600" ] || die 'Agent update lock owner or mode is invalid'
lock_identity=$(stat -c '%d:%i:%u:%a' -- "$lock_file")
exec 9<>"$lock_file"
[ "$(stat -c '%d:%i:%u:%a' -- "$lock_file")" = "$lock_identity" ] || die 'Agent update lock changed before acquisition'
flock -n 9 || die 'another Agent update owns this run directory'
[ "$(stat -c '%d:%i:%u:%a' -- "$lock_file")" = "$lock_identity" ] || die 'Agent update lock changed after acquisition'
[ ! -e "$agent_journal" ] && [ ! -L "$agent_journal" ] || die 'unfinished Agent update journal requires explicit operator recovery'
[ -f "$production_compose_file" ] && [ ! -L "$production_compose_file" ] || die 'production Compose override is unavailable'
[ -x "$production_image_gate" ] && [ ! -L "$production_image_gate" ] || die 'production image gate is unavailable'
command -v docker >/dev/null 2>&1 || die 'docker is required'
command -v jq >/dev/null 2>&1 || die 'jq is required'
command -v sync >/dev/null 2>&1 || die 'sync is required'
grep -Fqx '# dirextalk-split-cleanup-receipt-v1' "$receipt" || die 'cleanup receipt version is unsupported'
[ "$(read_pair "$receipt" state)" = complete ] || die 'cleanup receipt is incomplete'
[ "$(read_pair "$receipt" control.env_identity)" = "$(stat -c '%d:%i:%u' "$env_file")" ] || die '.env identity differs from receipt'
[ "$(read_pair "$receipt" control.manifest_identity)" = "$(stat -c '%d:%i:%u' "$manifest")" ] || die 'manifest identity differs from receipt'
[ "$(read_pair "$receipt" control.env_sha256)" = "$(sha256sum "$env_file" | awk '{print $1}')" ] || die '.env digest differs from receipt'
[ "$(read_pair "$receipt" control.manifest_sha256)" = "$(sha256sum "$manifest" | awk '{print $1}')" ] || die 'manifest digest differs from receipt'
[ "$(read_pair "$env_file" DIREXTALK_RELEASE_CATALOG_ORIGIN)" = https://imadmin.dirextalk.ai ] || die 'release catalog origin differs from the canonical deployment origin'
[ "$(read_pair "$manifest" compose_mode)" = production ] || negative 'Agent release updates apply only to production stacks'
stack=$(read_pair "$manifest" stack_name)
[ "$stack" = "$(read_pair "$receipt" stack_name)" ] || die 'stack identity mismatch'
[ "$(read_pair "$env_file" DIREXTALK_IMAGE_ATTESTATION_FILE)" = "$attestation" ] || die 'image attestation path is outside the receipt-bound run directory'
[ "$(read_pair "$manifest" image_attestation_path)" = "$attestation" ] || die 'image attestation path differs from manifest'
[ "$(stat -c '%d' "$attestation")" = "$(read_pair "$manifest" image_attestation_device)" ] || die 'image attestation device differs from manifest'
[ "$(stat -c '%i' "$attestation")" = "$(read_pair "$manifest" image_attestation_inode)" ] || die 'image attestation inode differs from manifest'
[ "$(stat -c '%u' "$attestation")" = "$(read_pair "$manifest" image_attestation_uid)" ] || die 'image attestation owner differs from manifest'
[ "$(sha256sum "$attestation" | awk '{print $1}')" = "$(read_pair "$manifest" image_attestation_sha256)" ] || die 'image attestation digest differs from manifest'
[ "$(sed -n '1p' "$attestation")" = '# dirextalk-image-attestation-v2' ] || die 'image attestation version is unsupported'

env_identity=$(stat -c '%d:%i:%u:%a' "$env_file"); env_sha=$(sha256sum "$env_file" | awk '{print $1}')
manifest_identity=$(stat -c '%d:%i:%u:%a' "$manifest"); manifest_sha=$(sha256sum "$manifest" | awk '{print $1}')
receipt_identity=$(stat -c '%d:%i:%u:%a' "$receipt"); receipt_sha=$(sha256sum "$receipt" | awk '{print $1}')
attestation_identity=$(stat -c '%d:%i:%u:%a' "$attestation"); attestation_sha=$(sha256sum "$attestation" | awk '{print $1}')
manifest_machine_id=$(read_pair "$manifest" runner.machine_id)
manifest_engine_id=$(read_pair "$manifest" runner.docker_engine_id)
receipt_machine_id=$(read_pair "$receipt" host.machine_id)
receipt_engine_id=$(read_pair "$receipt" docker.engine_id)
receipt_endpoint=$(read_pair "$receipt" docker.context_endpoint)
receipt_socket=$(read_pair "$receipt" docker.context_socket)
[ "$manifest_machine_id" = "$receipt_machine_id" ] || die 'manifest/receipt machine identity mismatch'
[ "$manifest_engine_id" = "$receipt_engine_id" ] || die 'manifest/receipt Docker Engine identity mismatch'
[ "$receipt_socket" = /run/docker.sock ] || die 'cleanup receipt Docker socket is not canonical'
verify_control_identity() {
  verify_output_dir_identity
  [ "$(stat -c '%d:%i:%u:%a' "$env_file")" = "$env_identity" ] || die '.env identity changed during Agent update'
  [ "$(sha256sum "$env_file" | awk '{print $1}')" = "$env_sha" ] || die '.env contents changed during Agent update'
  [ "$(stat -c '%d:%i:%u:%a' "$manifest")" = "$manifest_identity" ] || die 'manifest identity changed during Agent update'
  [ "$(sha256sum "$manifest" | awk '{print $1}')" = "$manifest_sha" ] || die 'manifest contents changed during Agent update'
  [ "$(stat -c '%d:%i:%u:%a' "$receipt")" = "$receipt_identity" ] || die 'cleanup receipt identity changed during Agent update'
  [ "$(sha256sum "$receipt" | awk '{print $1}')" = "$receipt_sha" ] || die 'cleanup receipt contents changed during Agent update'
  [ "$(stat -c '%d:%i:%u:%a' "$attestation")" = "$attestation_identity" ] || die 'image attestation identity changed during Agent update'
  [ "$(sha256sum "$attestation" | awk '{print $1}')" = "$attestation_sha" ] || die 'image attestation contents changed during Agent update'
  [ "$(stat -c '%d:%i:%u:%a' -- "$lock_file")" = "$lock_identity" ] || die 'Agent update lock changed while held'
}
verify_host_docker_identity() {
  local current_endpoint endpoint_socket
  [ -z "${DOCKER_HOST:-}" ] || die 'DOCKER_HOST must be unset for the local rootful Docker daemon'
  case "${DOCKER_CONTEXT:-default}" in ''|default) ;; *) die 'DOCKER_CONTEXT must be unset or default' ;; esac
  [ -f /etc/machine-id ] && [ ! -L /etc/machine-id ] || die 'host machine-id is unavailable'
  [ "$(stat -c '%u:%g' -- /etc/machine-id)" = 0:0 ] || die 'host machine-id is not root-owned'
  printf '%s\n' "$receipt_machine_id" | grep -Eq '^[0-9a-f]{32}$' || die 'cleanup receipt machine identity is invalid'
  printf '%s\n' "$receipt_engine_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || die 'cleanup receipt Docker Engine identity is invalid'
  [ "$(tr -d '[:space:]' </etc/machine-id)" = "$receipt_machine_id" ] || die 'host machine identity changed during Agent update'
  case "$receipt_endpoint" in unix:///*) ;; *) die 'cleanup receipt is not bound to a local Docker endpoint' ;; esac
  current_endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null) || die 'Docker context inspection failed'
  [ "$current_endpoint" = "$receipt_endpoint" ] || die 'Docker context endpoint changed during Agent update'
  endpoint_socket=${current_endpoint#unix://}
  [ -S "$endpoint_socket" ] || die 'bound local Docker socket is unavailable'
  [ "$(readlink -f -- "$endpoint_socket")" = "$receipt_socket" ] || die 'Docker context socket changed during Agent update'
  [ "$(docker info --format '{{.ID}}' 2>/dev/null)" = "$receipt_engine_id" ] || die 'Docker Engine identity changed during Agent update'
}
verify_control_identity
verify_host_docker_identity

container_count=$(read_pair "$receipt" container.count)
message_id=''
declare -A old_ids=()
declare -A receipt_ids=()
for ((i=0;i<container_count;i++)); do
  service=$(read_pair "$receipt" "container.$i.service")
  id=$(read_pair "$receipt" "container.$i.id")
  [ -z "${receipt_ids[$service]:-}" ] || die "receipt contains duplicate service $service"
  receipt_ids[$service]=$id
  case "$service" in
    message-server) message_id=$id ;;
    agent|extension-runner|core-runner) old_ids[$service]=$id ;;
  esac
done
[ -n "$message_id" ] && [ "${#old_ids[@]}" -eq 3 ] || die 'receipt lacks the fixed message/Agent services'
message_data=$(docker inspect "$message_id" 2>/dev/null) || die 'recorded message-server container is unavailable'
[ "$(jq -r '.[0].Id // empty' <<<"$message_data")" = "$message_id" ] || die 'message-server container identity changed'
[ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$message_data")" = "$stack" ] || die 'message-server container project changed'
[ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$message_data")" = message-server ] || die 'message-server container service changed'
[ "$(jq -r '.[0].State.Status // empty' <<<"$message_data")" = running ] || die 'message-server container is not running'
[ "$(jq -r '.[0].State.Health.Status // empty' <<<"$message_data")" = healthy ] || die 'message-server container is not healthy'
message_image_id=$(jq -r '.[0].Image // empty' <<<"$message_data")
server_version=$(docker image inspect "$message_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null) || die 'message-server image version inspection failed'
canonical_version "$server_version" || die 'running message-server version is not canonical'
semver_ge "$server_version" "$minimum_server_version" || negative "target requires message-server $minimum_server_version (running $server_version)"

current_ref=$(read_pair "$env_file" DIREXTALK_AGENT_IMAGE_IMMUTABLE)
printf '%s\n' "$current_ref" | grep -Eq '^(docker\.io/)?dirextalk/agent@sha256:[0-9a-f]{64}$' || die 'current Agent image is not the fixed immutable repository'
[ "$(read_pair "$attestation" image.DIREXTALK_AGENT_IMAGE_IMMUTABLE)" = "$current_ref" ] || die 'Agent image differs from image attestation'
current_attested_revision=$(read_pair "$attestation" agent_source_revision)
printf '%s\n' "$current_attested_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'attested Agent revision is invalid'
current_image_id=''
for service in agent extension-runner core-runner; do
  data=$(docker inspect "${old_ids[$service]}" 2>/dev/null) || die "recorded $service container is unavailable"
  [ "$(jq -r '.[0].Id // empty' <<<"$data")" = "${old_ids[$service]}" ] || die "$service container identity changed"
  [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$current_ref" ] || die "$service does not use the protected Agent image reference"
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")" = "$stack" ] || die "$service container project changed"
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")" = "$service" ] || die "$service container service changed"
  [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] || die "$service container is not running"
  [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ] || die "$service container is not healthy"
  observed=$(jq -r '.[0].Image // empty' <<<"$data")
  [ -z "$current_image_id" ] && current_image_id=$observed
  [ "$observed" = "$current_image_id" ] || die 'Agent runtime containers do not use one image ID'
done
current_version=$(docker image inspect "$current_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null) || die 'current Agent version inspection failed'
canonical_version "$current_version" || die 'current Agent image version is not canonical'
current_revision=$(docker image inspect "$current_image_id" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || die 'current Agent revision inspection failed'
[ "$current_revision" = "$current_attested_revision" ] || die 'running Agent revision differs from image attestation'
semver_ge "$current_version" "$target_version" && negative "Agent $target_version is not newer than running $current_version"

target_tag=docker.io/dirextalk/agent:$target_version
verify_control_identity
verify_host_docker_identity
docker pull "$target_tag" >/dev/null || die 'Agent image pull failed'
target_identity=$(docker image inspect "$target_tag" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null) || die 'target Agent image inspection failed'
target_label=${target_identity%%|*}; rest=${target_identity#*|}; target_id=${rest%%|*}; rest=${rest#*|}; target_revision=${rest%%|*}; digests=${rest#*|}
[ "$target_label" = "$target_version" ] || die 'target Agent image version label mismatch'
printf '%s\n' "$target_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'target Agent image revision label is invalid'
target_ref=$(printf '%s\n' "$digests" | awk '$0 ~ /^(docker\.io\/)?dirextalk\/agent@sha256:[0-9a-f]{64}$/ {print; exit}')
[ -n "$target_ref" ] || die 'target Agent image has no immutable repository digest'

new_attestation=''
new_manifest=''
new_env=''
new_receipt=''
final_receipt=''
runner_preparation=''
mutation_started=false
failure_phase=preflight
prepare_runner_cgroups() {
  local image_ref=$1 compose_env=$2 helper expected_hash key expected observed count=0
  verify_control_identity
  verify_host_docker_identity
  helper=$(read_pair "$compose_env" DIREXTALK_RUNNER_PREP_HELPER_PATH) || return 1
  expected_hash=$(read_pair "$compose_env" DIREXTALK_RUNNER_PREP_HELPER_SHA256) || return 1
  if [ "${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}" != true ]; then
    [ "$(readlink -f -- "$helper" 2>/dev/null)" = "$script_dir/prepare-runner-cgroups.sh" ] || return 1
    [ "$(stat -c '%u' -- "$helper" 2>/dev/null)" = 0 ] || return 1
  fi
  [ -x "$helper" ] && [ ! -L "$helper" ] || return 1
  [ "$(sha256sum -- "$helper" | awk '{print $1}')" = "$expected_hash" ] || return 1
  runner_preparation=$(mktemp "$out/.runner-preparation.XXXXXX") || return 1
  if ! DIREXTALK_AGENT_IMAGE_IMMUTABLE=$image_ref "$helper" "$stack" >"$runner_preparation"; then
    rm -f "$runner_preparation"
    runner_preparation=''
    return 1
  fi
  chmod 400 "$runner_preparation" || { rm -f "$runner_preparation"; runner_preparation=''; return 1; }
  for key in \
    DIREXTALK_EXTENSION_CGROUP_ROOT DIREXTALK_CORE_RUNNER_CGROUP_ROOT \
    DIREXTALK_EXTENSION_CGROUP_PARENT DIREXTALK_CORE_RUNNER_CGROUP_PARENT \
    DIREXTALK_CORE_EXTENSION_RUNNER_UID DIREXTALK_CORE_WORKLOAD_RUNNER_UID \
    DIREXTALK_EXTENSION_RUNNER_UNIT DIREXTALK_CORE_RUNNER_UNIT \
    DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH DIREXTALK_CORE_RUNNER_FRAGMENT_PATH \
    DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256 DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256 \
    DIREXTALK_RUNNER_APPARMOR_PROFILE DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH \
    DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256 DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH \
    DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256 DIREXTALK_RUNNER_PREP_HELPER_PATH \
    DIREXTALK_RUNNER_PREP_HELPER_SHA256 DIREXTALK_RUNNER_PREP_MACHINE_ID \
    DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID DIREXTALK_EXTENSION_CONTROL_GROUP \
    DIREXTALK_CORE_RUNNER_CONTROL_GROUP DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT \
    DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS \
    DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER \
    DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE \
    DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE; do
    expected=$(read_pair "$compose_env" "$key") || { rm -f "$runner_preparation"; runner_preparation=''; return 1; }
    observed=$(read_pair "$runner_preparation" "$key") || { rm -f "$runner_preparation"; runner_preparation=''; return 1; }
    [ "$observed" = "$expected" ] || { rm -f "$runner_preparation"; runner_preparation=''; return 1; }
    count=$((count+1))
  done
  [ "$(awk 'NF && $0 !~ /^[[:space:]]*#/ {n++} END {print n+0}' "$runner_preparation")" -eq "$count" ] || { rm -f "$runner_preparation"; runner_preparation=''; return 1; }
  rm -f "$runner_preparation"
  runner_preparation=''
}
normalize_runner_volumes() {
  local image_ref=$1 compose_env=$2 service
  for service in \
    extension-socket-init extension-runner-storage-init \
    core-runner-socket-init core-runner-storage-init; do
    verify_control_identity
    verify_host_docker_identity
    DIREXTALK_AGENT_IMAGE_IMMUTABLE=$image_ref docker compose --env-file "$compose_env" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
      run --rm --no-deps --pull never -T --interactive=false "$service" >/dev/null || return 1
  done
}
wait_services() {
  local expected=$1 expected_ref=$2 compose_env=$3 service id data attempts
  shift 3
  verify_control_identity
  verify_host_docker_identity
  for service in "$@"; do
    attempts=${DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS:-60}
    while [ "$attempts" -gt 0 ]; do
      id=$(docker compose --env-file "$compose_env" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" ps -q "$service" 2>/dev/null) || return 1
      if [ -n "$id" ]; then
        data=$(docker inspect "$id" 2>/dev/null) || return 1
        if [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$expected" ] && \
           [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$expected_ref" ] && \
           [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")" = "$stack" ] && \
           [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")" = "$service" ] && \
           [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] && \
           [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ]; then break; fi
        [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" != unhealthy ] || return 1
      fi
      attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1
    done
    [ "$attempts" -gt 0 ] || return 1
  done
  verify_control_identity
  verify_host_docker_identity
}

cleanup_previous_image() {
  local service id data all_ids project observed_service observed_ref status bound_id refs ref resolved_id retain_image=false removable=false
  local -a fixed_refs=()
  verify_control_identity
  verify_host_docker_identity
  [ -f "$agent_journal" ] && [ ! -L "$agent_journal" ] && [ "$(stat -c '%u:%a' "$agent_journal")" = "$required_owner:400" ] || return 1
  [ "$(read_pair "$agent_journal" state)" = cleanup-pending ] || return 1
  [ "$(read_pair "$agent_journal" old_image_ref)" = "$current_ref" ] || return 1
  [ "$(read_pair "$agent_journal" old_image_id)" = "$current_image_id" ] || return 1
  for service in agent extension-runner core-runner; do
    id=$(docker compose --env-file "$env_file" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" ps -q "$service") || return 1
    [ "$id" = "${new_ids[$service]}" ] || return 1
    data=$(docker inspect "$id" 2>/dev/null) || return 1
    [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$target_id" ] || return 1
    [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$target_ref" ] || return 1
    [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")" = "$stack" ] || return 1
    [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")" = "$service" ] || return 1
    [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] || return 1
    [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ] || return 1
  done
  all_ids=$(docker ps -aq --no-trunc) || return 1
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    data=$(docker inspect "$id" 2>/dev/null) || return 1
    [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$current_image_id" ] || continue
    observed_ref=$(jq -r '.[0].Config.Image // empty' <<<"$data")
    project=$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")
    observed_service=$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")
    bound_id=''
    [ -z "$observed_service" ] || bound_id=$(journal_bound_id "$observed_service" 2>/dev/null || true)
    status=$(jq -r '.[0].State.Status // empty' <<<"$data")
    removable=false
    if [ "$project" = "$stack" ] && [ "$observed_ref" = "$current_ref" ] && [ "$status" != running ] && [ "$bound_id" = "$id" ]; then
      case "$observed_service" in
        extension-socket-init|extension-runner-storage-init|core-runner-socket-init|core-runner-storage-init|agent-migrate) removable=true ;;
        agent|extension-runner|core-runner)
          [ "${old_ids[$observed_service]:-}" = "$id" ] && removable=true
          ;;
      esac
    fi
    if [ "$removable" = true ]; then
      verify_control_identity
      verify_host_docker_identity
      docker container rm "$id" >/dev/null || return 1
    else
      retain_image=true
    fi
  done <<<"$all_ids"
  verify_control_identity
  verify_host_docker_identity
  [ "$(docker image inspect "$current_ref" --format '{{.Id}}' 2>/dev/null)" = "$current_image_id" ] || return 1
  refs=$(docker image inspect "$current_image_id" --format '{{range .RepoTags}}{{println .}}{{end}}{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null) || return 1
  while IFS= read -r ref; do
    [ -n "$ref" ] || continue
    if printf '%s\n' "$ref" | grep -Eq '^(docker\.io/)?dirextalk/agent([:@])'; then
      fixed_refs+=("$ref")
    else
      retain_image=true
    fi
  done <<<"$refs"
  for ref in "${fixed_refs[@]}"; do
    verify_output_dir_identity
    verify_host_docker_identity
    resolved_id=$(docker image inspect "$ref" --format '{{.Id}}' 2>/dev/null) || return 1
    [ "$resolved_id" = "$current_image_id" ] || return 1
    docker image rm "$ref" >/dev/null || return 1
  done
  if [ "$retain_image" = true ]; then
    printf 'split-agent update: previous image ID retained because another container or foreign repository alias still uses it\n' >&2
    return 0
  fi
  verify_output_dir_identity
  verify_host_docker_identity
  if docker image inspect "$current_image_id" >/dev/null 2>&1; then
    docker image rm "$current_image_id" >/dev/null || return 1
  fi
}

render_receipt() {
  local source=$1 destination=$2 env_identity=$3 env_digest=$4 manifest_identity=$5 manifest_digest=$6
  local agent_id=$7 extension_id=$8 core_id=$9 state=${10}
  case "$state" in cleanup-pending|complete) ;; *) return 1 ;; esac
  python3 - "$source" "$destination" "$env_identity" "$env_digest" "$manifest_identity" "$manifest_digest" \
    "$agent_id" "$extension_id" "$core_id" "$state" <<'PY'
import pathlib,sys
source,dest,identity,digest,manifest_identity,manifest_digest,*rest=sys.argv[1:]
ids,state=rest[:3],rest[3]
mapping=dict(zip(("agent","extension-runner","core-runner"),ids))
lines=pathlib.Path(source).read_text().splitlines()
services={}
for line in lines:
    if line.startswith("container.") and ".service=" in line:
        key,value=line.split("=",1); services[key.split(".")[1]]=value
out=[]
for line in lines:
    if line.startswith("state="): line="state="+state
    elif line.startswith("control.env_identity="): line="control.env_identity="+identity
    elif line.startswith("control.env_sha256="): line="control.env_sha256="+digest
    elif line.startswith("control.manifest_identity="): line="control.manifest_identity="+manifest_identity
    elif line.startswith("control.manifest_sha256="): line="control.manifest_sha256="+manifest_digest
    elif line.startswith("container.") and ".id=" in line:
        key=line.split("=",1)[0]; index=key.split(".")[1]; service=services.get(index)
        if service in mapping: line=key+"="+mapping[service]
    out.append(line)
pathlib.Path(dest).write_text("\n".join(out)+"\n")
PY
}

write_agent_journal() {
  local state=$1 temp index journal_service journal_id
  case "$state" in prepared|cleanup-pending) ;; *) return 1 ;; esac
  verify_output_dir_identity
  temp=$(mktemp "$out/.agent-update.transaction.XXXXXX") || return 1
  {
    printf '%s\n' '# dirextalk-agent-update-transaction-v1'
    printf 'state=%s\nstack_name=%s\n' "$state" "$stack"
    printf 'old_image_ref=%s\nold_image_id=%s\n' "$current_ref" "$current_image_id"
    printf 'host_machine_id=%s\ndocker_engine_id=%s\ndocker_context_endpoint=%s\ndocker_context_socket=%s\n' \
      "$receipt_machine_id" "$receipt_engine_id" "$receipt_endpoint" "$receipt_socket"
    printf 'old.env_sha256=%s\nold.manifest_sha256=%s\nold.receipt_sha256=%s\nold.attestation_sha256=%s\n' \
      "$env_sha" "$manifest_sha" "$receipt_sha" "$attestation_sha"
    printf 'old.container.count=%s\n' "$container_count"
    for ((index=0; index<container_count; index++)); do
      journal_service=$(read_pair "$receipt" "container.$index.service")
      journal_id=$(read_pair "$receipt" "container.$index.id")
      printf 'old.container.%s.service=%s\nold.container.%s.id=%s\n' "$index" "$journal_service" "$index" "$journal_id"
    done
  } >"$temp" || { rm -f "$temp"; return 1; }
  chmod 400 "$temp" || { rm -f "$temp"; return 1; }
  verify_output_dir_identity
  durable_replace "$temp" "$agent_journal" || { rm -f "$temp"; return 1; }
}

journal_bound_id() {
  local wanted=$1 count index journal_service
  count=$(read_pair "$agent_journal" old.container.count) || return 1
  for ((index=0; index<count; index++)); do
    journal_service=$(read_pair "$agent_journal" "old.container.$index.service") || return 1
    if [ "$journal_service" = "$wanted" ]; then
      read_pair "$agent_journal" "old.container.$index.id"
      return
    fi
  done
  return 1
}

mark_agent_journal_cleanup_pending() {
  local temp
  verify_output_dir_identity
  temp=$(mktemp "$out/.agent-update.transaction.XXXXXX") || return 1
  awk -F= '$1=="state" {$0="state=cleanup-pending"} {print}' "$agent_journal" >"$temp" || { rm -f "$temp"; return 1; }
  chmod 400 "$temp" || { rm -f "$temp"; return 1; }
  verify_output_dir_identity
  durable_replace "$temp" "$agent_journal" || { rm -f "$temp"; return 1; }
}

verify_old_agent_runtime_unchanged() {
  local service data
  verify_control_identity
  verify_host_docker_identity
  for service in agent extension-runner core-runner; do
    data=$(docker inspect "${old_ids[$service]}" 2>/dev/null) || return 1
    [ "$(jq -r '.[0].Id // empty' <<<"$data")" = "${old_ids[$service]}" ] || return 1
    [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$current_image_id" ] || return 1
    [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$current_ref" ] || return 1
    [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")" = "$stack" ] || return 1
    [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")" = "$service" ] || return 1
    [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] || return 1
    [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ] || return 1
  done
  verify_control_identity
  verify_host_docker_identity
}

write_failure_evidence() {
  local status=$1 evidence receipt_identity receipt_digest
  verify_output_dir_identity
  receipt_identity=$(stat -c '%d:%i:%u' "$receipt" 2>/dev/null || true)
  receipt_digest=$(sha256sum "$receipt" 2>/dev/null | awk '{print $1}')
  [ -n "$receipt_identity" ] || receipt_identity=unavailable
  [ -n "$receipt_digest" ] || receipt_digest=unavailable
  evidence=$(mktemp "$out/.agent-update-failure.XXXXXX") || return 1
  if ! {
    printf '%s\n' '# dirextalk-agent-update-failure-v1'
    printf 'state=failed\n'
    printf 'recorded_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'phase=%s\n' "$failure_phase"
    printf 'exit_status=%s\n' "$status"
    printf 'stack_name=%s\n' "$stack"
    printf 'current_version=%s\n' "$current_version"
    printf 'current_image=%s\n' "$current_ref"
    printf 'target_version=%s\n' "$target_version"
    printf 'target_image=%s\n' "$target_ref"
    printf 'target_revision=%s\n' "$target_revision"
    printf 'cleanup_receipt_identity=%s\n' "$receipt_identity"
    printf 'cleanup_receipt_sha256=%s\n' "$receipt_digest"
  } >"$evidence"; then
    rm -f "$evidence"
    return 1
  fi
  chmod 400 "$evidence" || { rm -f "$evidence"; return 1; }
  sync -f "$evidence" || return 1
  sync -f "$out" || return 1
}

transaction_exit() {
  local status=$?
  trap - EXIT
  set +e
  if [ "$status" -ne 0 ] && [ "$mutation_started" = true ]; then
    write_failure_evidence "$status" || printf 'split-agent update: could not persist failure evidence\n' >&2
  fi
  [ -z "$new_env" ] || rm -f "$new_env"
  [ -z "$new_receipt" ] || rm -f "$new_receipt"
  [ -z "$final_receipt" ] || rm -f "$final_receipt"
  [ -z "$new_attestation" ] || rm -f "$new_attestation"
  [ -z "$new_manifest" ] || rm -f "$new_manifest"
  [ -z "$runner_preparation" ] || rm -f "$runner_preparation"
  if [ "$mutation_started" = false ]; then durable_remove "$agent_journal" || status=1; fi
  exit "$status"
}
trap transaction_exit EXIT

write_agent_journal prepared || die 'could not durably prepare Agent update journal'

stop_wrapper=$script_dir/stop-agent-local.sh
if [ "${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}" = true ] && [ -n "${DIREXTALK_AGENT_UPDATE_STOP_WRAPPER:-}" ]; then
  stop_wrapper=$DIREXTALK_AGENT_UPDATE_STOP_WRAPPER
fi
verify_control_identity
verify_host_docker_identity
failure_phase=runtime-stop
mutation_started=true
if "$stop_wrapper" "$out" >/dev/null; then
  :
else
  status=$?
  if [ "$status" -eq 3 ] && verify_old_agent_runtime_unchanged; then
    mutation_started=false
    negative 'Agent runtime stop reported an expected negative without changing the bound runtime'
  fi
  die 'Agent runtime stop failed'
fi
failure_phase=runner-preparation
prepare_runner_cgroups "$target_ref" "$env_file" || die 'target runner cgroup preparation failed'
failure_phase=volume-normalization
normalize_runner_volumes "$target_ref" "$env_file" || die 'target runner volume normalization failed'
failure_phase=storage-migration
verify_control_identity
verify_host_docker_identity
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref docker compose --env-file "$env_file" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
  run --rm --no-deps --pull never -T --interactive=false agent-migrate >/dev/null || die 'target Agent storage migration failed'
failure_phase=runner-start
verify_control_identity
verify_host_docker_identity
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref docker compose --env-file "$env_file" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
  up -d --no-deps --force-recreate --no-build --pull never extension-runner core-runner >/dev/null || die 'target runner start failed'
failure_phase=runner-health
wait_services "$target_id" "$target_ref" "$env_file" extension-runner core-runner || die 'target runner health check failed'
failure_phase=agent-start
verify_control_identity
verify_host_docker_identity
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref docker compose --env-file "$env_file" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
  up -d --no-deps --force-recreate --no-build --pull never agent >/dev/null || die 'target Agent start failed'
failure_phase=agent-health
wait_services "$target_id" "$target_ref" "$env_file" agent || die 'target Agent health check failed'

failure_phase=control-render
new_attestation=$(mktemp "$out/.image-attestation.XXXXXX")
awk -F= -v ref="$target_ref" -v revision="$target_revision" '
  $1=="agent_source_revision" {$0="agent_source_revision=" revision; revision_seen=1}
  $1=="image.DIREXTALK_AGENT_IMAGE_IMMUTABLE" {$0="image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=" ref; image_seen=1}
  {print}
  END {if (!revision_seen || !image_seen) exit 1}
' "$attestation" >"$new_attestation" || die 'could not prepare the target Agent attestation'
chmod 400 "$new_attestation"
new_manifest=$(mktemp "$out/.manifest.XXXXXX")
awk -F= -v device="$(stat -c '%d' "$new_attestation")" -v inode="$(stat -c '%i' "$new_attestation")" \
  -v uid="$(stat -c '%u' "$new_attestation")" -v digest="$(sha256sum "$new_attestation" | awk '{print $1}')" '
  $1=="image_attestation_device" {$0="image_attestation_device=" device; device_seen=1}
  $1=="image_attestation_inode" {$0="image_attestation_inode=" inode; inode_seen=1}
  $1=="image_attestation_uid" {$0="image_attestation_uid=" uid; uid_seen=1}
  $1=="image_attestation_sha256" {$0="image_attestation_sha256=" digest; digest_seen=1}
  {print}
  END {if (!device_seen || !inode_seen || !uid_seen || !digest_seen) exit 1}
' "$manifest" >"$new_manifest" || die 'could not prepare the target manifest'
chmod 400 "$new_manifest"
new_env=$(mktemp "$out/.env.XXXXXX")
awk -F= -v replacement="$target_ref" '$1=="DIREXTALK_AGENT_IMAGE_IMMUTABLE" {$0="DIREXTALK_AGENT_IMAGE_IMMUTABLE=" replacement; seen=1} {print} END {if (!seen) exit 1}' "$env_file" >"$new_env" || die 'could not prepare the target environment'
chmod 400 "$new_env"
new_env_identity=$(stat -c '%d:%i:%u' "$new_env"); new_env_sha=$(sha256sum "$new_env" | awk '{print $1}')
new_manifest_identity=$(stat -c '%d:%i:%u' "$new_manifest"); new_manifest_sha=$(sha256sum "$new_manifest" | awk '{print $1}')
declare -A new_ids=()
for service in agent extension-runner core-runner; do
  new_ids[$service]=$(docker compose --env-file "$env_file" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" ps -q "$service")
  printf '%s\n' "${new_ids[$service]}" | grep -Eq '^[0-9a-f]{64}$' || die 'new container identity is invalid'
done
new_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
render_receipt "$receipt" "$new_receipt" "$new_env_identity" "$new_env_sha" "$new_manifest_identity" "$new_manifest_sha" \
  "${new_ids[agent]}" "${new_ids[extension-runner]}" "${new_ids[core-runner]}" cleanup-pending
chmod 400 "$new_receipt"
final_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
render_receipt "$receipt" "$final_receipt" "$new_env_identity" "$new_env_sha" "$new_manifest_identity" "$new_manifest_sha" \
  "${new_ids[agent]}" "${new_ids[extension-runner]}" "${new_ids[core-runner]}" complete
chmod 400 "$final_receipt"

failure_phase=production-gates
"$production_image_gate" "$new_env" "$new_attestation" >/dev/null || die 'updated Agent failed the production image gate'
topology_json=$(mktemp "$out/.agent-update-topology.XXXXXX")
if ! docker compose --env-file "$new_env" -f "$compose_file" -f "$production_compose_file" --project-name "$stack" config --format json >"$topology_json" ||
   ! jq -e --arg ref "$target_ref" '
     .services.agent.image == $ref and
     .services["extension-runner"].image == $ref and
     .services["core-runner"].image == $ref and
     .services["extension-runner"].network_mode == "none" and
     .services["core-runner"].network_mode == "none" and
     .services.agent.user == "65532:65532" and
     .services["extension-runner"].user == "65531:65531" and
     .services["core-runner"].user == "65530:65530" and
     (.services.agent.build == null) and
     (.services["extension-runner"].build == null) and
     (.services["core-runner"].build == null)
   ' "$topology_json" >/dev/null; then
  rm -f "$topology_json"
  die 'updated Agent failed the production topology gate'
fi
rm -f "$topology_json"

failure_phase=control-commit
verify_output_dir_identity
durable_replace "$new_attestation" "$attestation" || die 'could not durably commit the target Agent attestation'
new_attestation=''
verify_output_dir_identity
durable_replace "$new_manifest" "$manifest" || die 'could not durably commit the target Agent manifest'
new_manifest=''
verify_output_dir_identity
durable_replace "$new_env" "$env_file" || die 'could not durably commit the target Agent environment'
new_env=''
if [ "${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}" = true ] && [ "${DIREXTALK_AGENT_UPDATE_FAIL_RECEIPT_COMMIT:-false}" = true ]; then
  die 'injected receipt commit failure'
fi
verify_output_dir_identity
durable_replace "$new_receipt" "$receipt" || die 'could not durably commit the cleanup-pending Agent receipt'
new_receipt=''

env_identity=$(stat -c '%d:%i:%u:%a' "$env_file"); env_sha=$(sha256sum "$env_file" | awk '{print $1}')
manifest_identity=$(stat -c '%d:%i:%u:%a' "$manifest"); manifest_sha=$(sha256sum "$manifest" | awk '{print $1}')
receipt_identity=$(stat -c '%d:%i:%u:%a' "$receipt"); receipt_sha=$(sha256sum "$receipt" | awk '{print $1}')
attestation_identity=$(stat -c '%d:%i:%u:%a' "$attestation"); attestation_sha=$(sha256sum "$attestation" | awk '{print $1}')
verify_control_identity
verify_host_docker_identity
mark_agent_journal_cleanup_pending || die 'could not commit Agent cleanup-pending journal state'
failure_phase=old-image-cleanup
cleanup_previous_image || die 'previous Agent image cleanup failed'
failure_phase=final-receipt-commit
verify_output_dir_identity
durable_replace "$final_receipt" "$receipt" || die 'could not durably commit the complete Agent receipt'
final_receipt=''

mutation_started=false
trap - EXIT
durable_remove "$agent_journal" || die 'could not durably remove the completed Agent update journal'
printf 'split-agent update passed: version=%s image=%s\n' "$target_version" "$target_ref"
