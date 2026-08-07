#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Remove only host runner instances left by a failed provision/start preflight.
# Docker objects must be proven absent before any systemd mutation is allowed.

usage() { echo "usage: $0 OUTPUT_DIR" >&2; exit 2; }
die() { echo "split-stack provision-failure cleanup: $*" >&2; exit 1; }
expected_negative() { echo "split-stack provision-failure cleanup: $*" >&2; exit 3; }
[ "$#" -eq 1 ] || usage
script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)

case "$1" in
  /*) out=$(readlink -m -- "$1") ;;
  *) out=$(readlink -m -- "$(pwd -P)/$1") ;;
esac
[ "$out" != / ] || die "refusing to clean the filesystem root"
[ -d "$out" ] && [ ! -L "$out" ] || die "output directory must be a regular non-symlink directory"
[ "$(stat -c '%a' "$out")" = 700 ] || die "output directory must be mode 0700"
[ "$(stat -c '%u' "$out")" = "$(id -u)" ] || die "output directory owner differs"
out_identity=$(stat -c '%d:%i:%u:%g:%a' "$out") || die "output directory identity query failed"
env_file=$out/.env
manifest=$out/.manifest
for control in "$env_file" "$manifest"; do
  [ -f "$control" ] && [ ! -L "$control" ] || die "missing regular control file: $control"
  [ "$(stat -c '%a' "$control")" = 400 ] || die "control file must be mode 0400: $control"
  [ "$(stat -c '%u' "$control")" = "$(id -u)" ] || die "control file owner differs: $control"
done
env_identity=$(stat -c '%d:%i:%u:%g:%a' "$env_file") || die ".env identity query failed"
manifest_identity=$(stat -c '%d:%i:%u:%g:%a' "$manifest") || die "manifest identity query failed"
env_hash=$(sha256sum -- "$env_file" | awk '{print $1}') || die ".env digest query failed"
manifest_hash=$(sha256sum -- "$manifest" | awk '{print $1}') || die "manifest digest query failed"
revalidate_controls() {
  [ "$(stat -c '%d:%i:%u:%g:%a' "$out" 2>/dev/null)" = "$out_identity" ] || die "output directory identity changed"
  [ "$(stat -c '%d:%i:%u:%g:%a' "$env_file" 2>/dev/null)" = "$env_identity" ] || die ".env identity changed"
  [ "$(stat -c '%d:%i:%u:%g:%a' "$manifest" 2>/dev/null)" = "$manifest_identity" ] || die "manifest identity changed"
  [ "$(sha256sum -- "$env_file" 2>/dev/null | awk '{print $1}')" = "$env_hash" ] || die ".env content changed"
  [ "$(sha256sum -- "$manifest" 2>/dev/null | awk '{print $1}')" = "$manifest_hash" ] || die "manifest content changed"
}
if [ -e "$out/.cleanup-receipt" ] || [ -L "$out/.cleanup-receipt" ]; then
  expected_negative "a cleanup receipt exists; use cleanup-local.sh"
fi

read_pair() {
  local file=$1 key=$2 count value
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {n++} END{print n+0}' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {print substr($0,length(wanted)+2); exit}' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

stack_name=$(read_pair "$manifest" stack_name)
grep -Fqx '# dirextalk-split-manifest-v1' "$manifest" || die "manifest version is unsupported"
stack_nonce=$(read_pair "$manifest" stack_nonce)
[ "$stack_nonce" = "${stack_name#d-}" ] || die "manifest stack nonce does not bind to stack identity"
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "manifest stack identity is invalid"
manifest_machine=$(read_pair "$manifest" runner.machine_id)
manifest_engine=$(read_pair "$manifest" runner.docker_engine_id)
manifest_agent=$(read_pair "$manifest" agent_instance_id)
manifest_message=$(read_pair "$manifest" message_instance_id)
manifest_generation=$(read_pair "$manifest" account_generation)
printf '%s\n' "$manifest_machine" | grep -Eq '^[0-9a-f]{32}$' || die "manifest machine-id is invalid"
printf '%s\n' "$manifest_engine" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || die "manifest Docker Engine ID is invalid"
printf '%s\n' "$manifest_agent" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' || die "manifest Agent instance ID is invalid"
printf '%s\n' "$manifest_message" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' || die "manifest message-server instance ID is invalid"
[ "$manifest_agent" != "$manifest_message" ] || die "manifest instance identities must differ"
printf '%s\n' "$manifest_generation" | grep -Eq '^[1-9][0-9]*$' || die "manifest account generation is invalid"

env_stack=$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)
[ "$env_stack" = "$stack_name" ] || die ".env stack identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_RUNNER_PREP_MACHINE_ID)" = "$manifest_machine" ] || die ".env machine-id differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID)" = "$manifest_engine" ] || die ".env Engine ID differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_AGENT_INSTANCE_ID)" = "$manifest_agent" ] || die ".env Agent identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_INSTANCE_ID)" = "$manifest_message" ] || die ".env message-server identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_ACCOUNT_GENERATION)" = "$manifest_generation" ] || die ".env account generation differs from manifest"

network_keys=(message_private message_public message_database agent_private agent_database agent_caller agent_egress)
volume_keys=(postgres message_config message_data message_plugins agent_secrets agent_config agent_core_data agent_extension_socket agent_extension_install agent_extension_staging agent_runner_workspaces agent_runner_state agent_knowledge_content agent_knowledge_mount capability_authority capability_shared capability_private core_runner_socket core_runner_installs core_runner_workspaces core_runner_state)
networks=() volumes=()
for key in "${network_keys[@]}"; do networks+=("$(read_pair "$manifest" "resource.network.$key")"); done
for key in "${volume_keys[@]}"; do volumes+=("$(read_pair "$manifest" "resource.volume.$key")"); done
for resource in "${networks[@]}" "${volumes[@]}"; do
  printf '%s\n' "$resource" | grep -Eq "^${stack_name}-[A-Za-z0-9_.-]+$" || die "resource is outside the fresh stack namespace: $resource"
done

runner_units=(extension core)
runner_names=("$(read_pair "$manifest" runner.extension.unit)" "$(read_pair "$manifest" runner.core.unit)")
runner_parents=("$(read_pair "$manifest" runner.extension.parent)" "$(read_pair "$manifest" runner.core.parent)")
runner_groups=("$(read_pair "$manifest" runner.extension.control_group)" "$(read_pair "$manifest" runner.core.control_group)")
runner_fragments=("$(read_pair "$manifest" runner.extension.fragment_path)" "$(read_pair "$manifest" runner.core.fragment_path)")
runner_hashes=("$(read_pair "$manifest" runner.extension.fragment_sha256)" "$(read_pair "$manifest" runner.core.fragment_sha256)")
runner_parent_roots=("$(read_pair "$manifest" runner.extension.parent_root)" "$(read_pair "$manifest" runner.core.parent_root)")
runner_parent_procs=("$(read_pair "$manifest" runner.extension.parent_procs)" "$(read_pair "$manifest" runner.core.parent_procs)")
runner_parent_procs_owners=("$(read_pair "$manifest" runner.extension.parent_procs_owner)" "$(read_pair "$manifest" runner.core.parent_procs_owner)")
runner_parent_procs_modes=("$(read_pair "$manifest" runner.extension.parent_procs_mode)" "$(read_pair "$manifest" runner.core.parent_procs_mode)")
env_runner_units=("$(read_pair "$env_file" DIREXTALK_EXTENSION_RUNNER_UNIT)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_UNIT)")
env_runner_parents=("$(read_pair "$env_file" DIREXTALK_EXTENSION_CGROUP_PARENT)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_CGROUP_PARENT)")
env_runner_groups=("$(read_pair "$env_file" DIREXTALK_EXTENSION_CONTROL_GROUP)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_CONTROL_GROUP)")
env_runner_roots=("$(read_pair "$env_file" DIREXTALK_EXTENSION_CGROUP_ROOT)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_CGROUP_ROOT)")
env_runner_fragments=("$(read_pair "$env_file" DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_FRAGMENT_PATH)")
env_runner_hashes=("$(read_pair "$env_file" DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256)")
env_runner_parent_roots=("$(read_pair "$env_file" DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT)")
env_runner_parent_procs=("$(read_pair "$env_file" DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS)" "$(read_pair "$env_file" DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS)")
env_network_keys=(DIREXTALK_MESSAGE_PRIVATE_NETWORK DIREXTALK_MESSAGE_PUBLIC_NETWORK DIREXTALK_MESSAGE_DATABASE_NETWORK DIREXTALK_AGENT_PRIVATE_NETWORK DIREXTALK_AGENT_DATABASE_NETWORK DIREXTALK_AGENT_CALLER_NETWORK DIREXTALK_AGENT_EGRESS_NETWORK)
env_volume_keys=(DIREXTALK_POSTGRES_VOLUME DIREXTALK_MESSAGE_CONFIG_VOLUME DIREXTALK_MESSAGE_DATA_VOLUME DIREXTALK_MESSAGE_PLUGINS_VOLUME DIREXTALK_AGENT_SECRET_VOLUME DIREXTALK_AGENT_CONFIG_VOLUME DIREXTALK_AGENT_CORE_DATA_VOLUME DIREXTALK_AGENT_SOCKET_VOLUME DIREXTALK_AGENT_INSTALL_VOLUME DIREXTALK_AGENT_STAGING_VOLUME DIREXTALK_AGENT_RUNNER_WORKSPACE_VOLUME DIREXTALK_AGENT_RUNNER_STATE_VOLUME DIREXTALK_AGENT_KNOWLEDGE_CONTENT_VOLUME DIREXTALK_AGENT_KNOWLEDGE_MOUNT_VOLUME DIREXTALK_CAPABILITY_AUTHORITY_VOLUME DIREXTALK_CAPABILITY_SHARED_VOLUME DIREXTALK_CAPABILITY_PRIVATE_VOLUME DIREXTALK_CORE_RUNNER_SOCKET_VOLUME DIREXTALK_CORE_RUNNER_INSTALL_VOLUME DIREXTALK_CORE_RUNNER_WORKSPACE_VOLUME DIREXTALK_CORE_RUNNER_STATE_VOLUME)
for i in 0 1; do
  role=${runner_units[$i]}; unit=${runner_names[$i]}
  [ "${env_runner_units[$i]}" = "$unit" ] || die "$role runner unit differs between .env and manifest"
  [ "${env_runner_parents[$i]}" = "${runner_parents[$i]}" ] || die "$role runner parent differs between .env and manifest"
  [ "${env_runner_groups[$i]}" = "${runner_groups[$i]}" ] || die "$role runner ControlGroup differs between .env and manifest"
  [ "${env_runner_roots[$i]}" = "/sys/fs/cgroup${runner_groups[$i]}" ] || die "$role runner root differs from ControlGroup"
  [ "${env_runner_fragments[$i]}" = "${runner_fragments[$i]}" ] || die "$role runner fragment path differs between .env and manifest"
  [ "${env_runner_hashes[$i]}" = "${runner_hashes[$i]}" ] || die "$role runner fragment hash differs between .env and manifest"
  [ "${env_runner_parent_roots[$i]}" = "${runner_parent_roots[$i]}" ] || die "$role runner parent root differs between .env and manifest"
  [ "${env_runner_parent_procs[$i]}" = "${runner_parent_procs[$i]}" ] || die "$role runner parent process control differs between .env and manifest"
  [ "${runner_parent_roots[$i]}" = "/sys/fs/cgroup${runner_groups[$i]%/*}" ] || die "$role runner parent root differs from ControlGroup parent"
  [ "${runner_parent_procs[$i]}" = "${runner_parent_roots[$i]}/cgroup.procs" ] || die "$role runner parent process control path is not exact"
  case "$role:${runner_parent_procs_owners[$i]}" in extension:65531:65531|core:65530:65530) ;; *) die "$role runner parent process control owner is invalid" ;; esac
  [ "${runner_parent_procs_modes[$i]}" = 644 ] || die "$role runner parent process control mode is invalid"
  printf '%s\n' "$unit" | grep -Eq "^dirextalk-${role}-runner@${stack_name}\.service$" || die "$role runner unit is not stack-bound"
  printf '%s\n' "${runner_parents[$i]}" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*\.slice$' || die "$role runner parent is invalid"
  printf '%s\n' "${runner_groups[$i]}" | grep -Eq '^/[^/[:space:]][^[:space:]]*$' || die "$role runner ControlGroup is invalid"
  case "${runner_groups[$i]}" in
    */"${runner_parents[$i]}"/"$unit") ;;
    *) die "$role runner ControlGroup is not derived from its Slice and unit" ;;
  esac
  printf '%s\n' "${runner_hashes[$i]}" | grep -Eq '^[0-9a-f]{64}$' || die "$role runner fragment hash is invalid"
  [ -f "${runner_fragments[$i]}" ] && [ ! -L "${runner_fragments[$i]}" ] || die "$role runner fragment is missing or symlinked"
  [ "$(stat -c '%u:%g' "${runner_fragments[$i]}")" = 0:0 ] || die "$role runner fragment is not root-owned"
  [ "$(stat -c '%a' "${runner_fragments[$i]}")" = 644 ] || die "$role runner fragment mode is not 0644"
  [ "$(sha256sum -- "${runner_fragments[$i]}" | awk '{print $1}')" = "${runner_hashes[$i]}" ] || die "$role runner fragment hash changed"
done
for i in "${!network_keys[@]}"; do [ "$(read_pair "$env_file" "${env_network_keys[$i]}")" = "${networks[$i]}" ] || die "network name differs between .env and manifest"; done
for i in "${!volume_keys[@]}"; do [ "$(read_pair "$env_file" "${env_volume_keys[$i]}")" = "${volumes[$i]}" ] || die "volume name differs between .env and manifest"; done

command -v docker >/dev/null 2>&1 || die "docker is required"
command -v systemctl >/dev/null 2>&1 || die "systemctl is required"
[ -z "${DOCKER_HOST:-}" ] || die "DOCKER_HOST must be unset for local cleanup"
case "${DOCKER_CONTEXT:-default}" in ''|default) ;; *) die "DOCKER_CONTEXT must be unset or default" ;; esac

log_file=$out/.provision-failure-cleanup.log
if [ -e "$log_file" ]; then
  [ -f "$log_file" ] && [ ! -L "$log_file" ] || die "failure log is not a regular file"
  [ "$(stat -c '%u:%a' "$log_file")" = "$(id -u):400" ] || die "failure log identity or mode changed"
  chmod 600 -- "$log_file"
else
  : >"$log_file"
  chmod 600 -- "$log_file"
fi
protect_failure_log() {
  local status=$?
  if ! chmod 400 -- "$log_file" 2>/dev/null; then
    echo "split-stack provision-failure cleanup: failure log mode could not be protected" >&2
    [ "$status" -ne 0 ] || status=1
  fi
  trap - EXIT
  exit "$status"
}
trap protect_failure_log EXIT
log() { printf '%s\n' "$*" >>"$log_file"; }

revalidate_controls
endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null) || die "Docker context inspection failed"
case "$endpoint" in
  unix:///*) context_socket=${endpoint#unix://} ;;
  *) die "Docker context is not a local Unix endpoint" ;;
esac
context=$(docker context show 2>/dev/null) || die "Docker current-context query failed"
[ "$context" = default ] || die "Docker current context must be default"
[ -S "$context_socket" ] || die "Docker socket is unavailable"
socket_canonical=$(readlink -f -- "$context_socket" 2>/dev/null) || die "Docker socket canonicalization failed"
[ "$socket_canonical" = /run/docker.sock ] || die "Docker socket identity changed"
[ "$(stat -Lc '%u' "$context_socket" 2>/dev/null)" = 0 ] || die "Docker socket is not root-owned"
socket_identity=$(stat -Lc '%d:%i:%u:%g' "$context_socket" 2>/dev/null) || die "Docker socket identity query failed"
context_endpoint=$endpoint
[ -f /etc/machine-id ] && [ ! -L /etc/machine-id ] || die "host machine-id is unavailable"
[ "$(stat -c '%u:%g' /etc/machine-id 2>/dev/null)" = 0:0 ] || die "host machine-id is not root-owned"
machine_identity=$(stat -c '%d:%i:%u:%g' /etc/machine-id 2>/dev/null) || die "host machine-id identity query failed"
machine=$(cat /etc/machine-id 2>/dev/null | tr -d '[:space:]') || die "host machine-id read failed"
[ "$machine" = "$manifest_machine" ] || die "host machine-id differs from manifest"
engine=$(docker --context default info --format '{{.ID}}' 2>/dev/null) || die "Docker Engine identity query failed"
[ "$engine" = "$manifest_engine" ] || die "Docker Engine ID differs from manifest"

inspect_absent() {
  local kind=$1 name=$2 err status
  err=$(mktemp "$out/.cleanup-failure-inspect.XXXXXX") || die "cannot allocate inspect workspace"
  if docker --context default "$kind" inspect "$name" >/dev/null 2>"$err"; then
    rm -f -- "$err"
    expected_negative "Docker $kind exists for failed stack: $name"
  else
    status=$?
  fi
  if [ "$status" -eq 1 ]; then
    case "$kind" in
      network)
        if grep -Eiq '^(Error response from daemon: )?(network .+ not found|no such network: .+)$' "$err"; then
          rm -f -- "$err"
          return 0
        fi
        ;;
      volume)
        if grep -Eiq '^(Error response from daemon: )?(get .+: no such volume|no such volume: .+)$' "$err"; then
          rm -f -- "$err"
          return 0
        fi
        ;;
      *) die "unsupported Docker object kind: $kind" ;;
    esac
  fi
  cat "$err" >&2
  rm -f -- "$err"
  die "Docker $kind absence check failed (status $status): $name"
}

verify_docker_absent() {
  local containers prefix_containers name
  if ! containers=$(docker --context default ps -aq --filter "label=com.docker.compose.project=$stack_name"); then
    die "Docker container absence check failed"
  fi
  [ -z "$containers" ] || expected_negative "Docker containers exist for failed stack"
  if ! prefix_containers=$(docker --context default ps -aq --filter "name=^/${stack_name}-"); then
    die "Docker stack-prefix container absence check failed"
  fi
  [ -z "$prefix_containers" ] || expected_negative "Docker stack-prefix containers exist for failed stack"
  for name in "${networks[@]}"; do inspect_absent network "$name"; done
  for name in "${volumes[@]}"; do inspect_absent volume "$name"; done
}
verify_docker_absent

verify_unit() {
  local i=$1 role=${runner_units[$1]} unit=${runner_names[$1]} state fragment control slice enabled enabled_status active active_status
  state=$(systemctl show "$unit" --property=LoadState --value 2>/dev/null) || die "$role runner load-state query failed"
  case "$state" in
    loaded|not-found) ;;
    *) die "$role runner load state is unsafe: $state" ;;
  esac
  if enabled=$(systemctl is-enabled "$unit" 2>/dev/null); then
    enabled_status=0
  else
    enabled_status=$?
  fi
  case "$enabled_status:$enabled" in
    0:enabled|1:disabled|1:masked|4:not-found) ;;
    *) die "$role runner enablement query failed or returned an unsafe state: $enabled" ;;
  esac
  if active=$(systemctl is-active "$unit" 2>/dev/null); then
    active_status=0
  else
    active_status=$?
  fi
  case "$active_status:$active" in
    0:active)
      [ "$state" = loaded ] || die "$role runner is active without a loaded unit"
      ;;
    3:inactive|3:failed|4:unknown)
      case "$enabled_status:$enabled" in
        1:disabled|1:masked|4:not-found) return 1 ;;
        0:enabled) [ "$state" = loaded ] || die "$role runner is enabled without a loaded unit" ;;
      esac
      ;;
    *) die "$role runner activity query failed or returned an unsafe state: $active" ;;
  esac
  fragment=$(systemctl show "$unit" --property=FragmentPath --value 2>/dev/null) || die "$role runner FragmentPath query failed"
  [ "$fragment" = "${runner_fragments[$i]}" ] || die "$role runner FragmentPath changed"
  [ -f "$fragment" ] && [ ! -L "$fragment" ] || die "$role runner fragment was replaced"
  [ "$(stat -c '%u:%g' "$fragment")" = 0:0 ] || die "$role runner fragment owner changed"
  [ "$(stat -c '%a' "$fragment")" = 644 ] || die "$role runner fragment mode changed"
  [ "$(sha256sum -- "$fragment" | awk '{print $1}')" = "${runner_hashes[$i]}" ] || die "$role runner fragment hash changed"
  control=$(systemctl show "$unit" --property=ControlGroup --value 2>/dev/null) || die "$role runner ControlGroup query failed"
  [ "$control" = "${runner_groups[$i]}" ] || die "$role runner ControlGroup changed"
  slice=$(systemctl show "$unit" --property=Slice --value 2>/dev/null) || die "$role runner Slice query failed"
  [ "$slice" = "${runner_parents[$i]}" ] || die "$role runner Slice changed"
  return 0
}

verify_unit_disabled() {
  local i=$1 role=${runner_units[$1]} unit=${runner_names[$1]} active active_status enabled enabled_status
  if active=$(systemctl is-active "$unit" 2>/dev/null); then
    active_status=0
  else
    active_status=$?
  fi
  case "$active_status:$active" in
    3:inactive|3:failed|4:unknown) ;;
    0:active) die "$role runner remained active after cleanup" ;;
    *) die "$role runner activity postcondition query failed or returned an unsafe state: $active" ;;
  esac
  if enabled=$(systemctl is-enabled "$unit" 2>/dev/null); then
    enabled_status=0
  else
    enabled_status=$?
  fi
  case "$enabled_status:$enabled" in
    1:disabled|1:masked|4:not-found) ;;
    *) die "$role runner enablement postcondition failed: $enabled" ;;
  esac
}

cleanup_runner_apparmor() {
  local output status
  if output=$("$script_dir/manage-runner-apparmor.sh" remove 2>&1); then
    return 0
  else
    status=$?
  fi
  case "$status" in
    3) log "$output" ;;
    *) die "runner AppArmor cleanup failed (status $status): $output" ;;
  esac
}

unit_present=(false false)
for i in 0 1; do
  revalidate_controls
  if verify_unit "$i"; then
    unit_present[i]=true
  fi
done
if [ "${unit_present[0]}" = false ] && [ "${unit_present[1]}" = false ]; then
  cleanup_runner_apparmor
  expected_negative "runner units are already absent or disabled"
fi
for i in 0 1; do
  if [ "${unit_present[$i]}" = true ]; then
    revalidate_controls
    endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null) || die "Docker context revalidation failed"
    [ "$endpoint" = "$context_endpoint" ] || die "Docker context changed before runner cleanup"
    case "$endpoint" in
      unix:///*) context_socket=${endpoint#unix://} ;;
      *) die "Docker context stopped using a local Unix endpoint" ;;
    esac
    context=$(docker context show 2>/dev/null) || die "Docker current-context revalidation failed"
    [ "$context" = default ] || die "Docker current context changed before runner cleanup"
    [ "$(readlink -f -- "$context_socket" 2>/dev/null)" = /run/docker.sock ] || die "Docker socket canonical path changed before runner cleanup"
    [ "$(stat -Lc '%d:%i:%u:%g' "$context_socket" 2>/dev/null)" = "$socket_identity" ] || die "Docker socket identity changed before runner cleanup"
    [ "$(stat -c '%d:%i:%u:%g' /etc/machine-id 2>/dev/null)" = "$machine_identity" ] || die "host machine-id identity changed before runner cleanup"
    machine=$(cat /etc/machine-id 2>/dev/null | tr -d '[:space:]') || die "host machine-id revalidation failed"
    [ "$machine" = "$manifest_machine" ] || die "host machine-id changed before runner cleanup"
    engine=$(docker --context default info --format '{{.ID}}' 2>/dev/null) || die "Docker Engine revalidation failed"
    [ "$engine" = "$manifest_engine" ] || die "Docker Engine changed before runner cleanup"
    verify_docker_absent
    verify_unit "$i" || die "runner identity changed before cleanup"
    systemctl disable --now "${runner_names[$i]}" || die "exact ${runner_units[$i]} runner cleanup failed"
    verify_unit_disabled "$i"
    log "disabled ${runner_names[$i]}"
  fi
done
cleanup_runner_apparmor
chmod 400 -- "$log_file"
log_file_mode=$(stat -c '%a' "$log_file")
[ "$log_file_mode" = 400 ] || die "failure log mode could not be protected"
trap - EXIT
printf 'provision-failure cleanup complete; generated files and failure log remain at %s\n' "$out"
