#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)

usage() {
  echo "usage: $0 [--purge] OUTPUT_DIR" >&2
  exit 2
}

die() {
  echo "split-stack cleanup: $*" >&2
  exit 1
}

purge=false
case "${1:-}" in
  --purge)
    purge=true
    shift
    ;;
esac
[ "$#" -eq 1 ] || usage

out_input=$1
case "$out_input" in
  /*) out=$(readlink -m -- "$out_input") ;;
  *) out=$(readlink -m -- "$(pwd -P)/$out_input") ;;
esac
[ "$out" != "/" ] || die "refusing to clean the filesystem root"
[ -d "$out" ] && [ ! -L "$out" ] || die "output directory must be a regular non-symlink directory"
[ "$(stat -c '%a' "$out")" = 700 ] || die "output directory must be mode 0700"
env_file=$out/.env
manifest=$out/.manifest
receipt=$out/.cleanup-receipt
for control_file in "$env_file" "$manifest" "$receipt"; do
  [ -f "$control_file" ] && [ ! -L "$control_file" ] || die "missing regular control file: $control_file"
  [ "$(stat -c '%a' "$control_file")" = 400 ] || die "control file must be mode 0400: $control_file"
  [ "$(stat -c '%u' "$control_file")" = "$(id -u)" ] || die "control file must be owned by the cleanup user: $control_file"
done

read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

validate_https_manifest_binding() {
  local tls_mode=$1 server_name=$2 client_base_url=$3 status
  if printf '%s\n' "$server_name" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$'; then
    :
  else
    status=$?
    if [ "$status" -eq 1 ]; then
      die "$tls_mode manifest server name is invalid"
    fi
    die "$tls_mode manifest server-name validation infrastructure failure (grep status $status)"
  fi
  [ "$client_base_url" = "https://$server_name" ] || die "$tls_mode manifest client URL is not bound to its server name"
}

grep -Fqx '# dirextalk-split-cleanup-receipt-v1' "$receipt" || die "cleanup receipt version is unsupported"

stack_name=$(read_pair "$manifest" stack_name)
stack_nonce=$(read_pair "$manifest" stack_nonce)
manifest_agent_id=$(read_pair "$manifest" agent_instance_id)
manifest_message_id=$(read_pair "$manifest" message_instance_id)
manifest_generation=$(read_pair "$manifest" account_generation)
manifest_master_key_path=$(read_pair "$manifest" core_secret_master_key_path)
manifest_master_key_device=$(read_pair "$manifest" core_secret_master_key_device)
manifest_master_key_inode=$(read_pair "$manifest" core_secret_master_key_inode)
manifest_master_key_uid=$(read_pair "$manifest" core_secret_master_key_uid)
manifest_http_bind=$(read_pair "$manifest" message_http_bind)
manifest_https_bind=$(read_pair "$manifest" message_https_bind)
manifest_client_base_url=$(read_pair "$manifest" message_client_base_url)
manifest_tls_mode=$(read_pair "$manifest" message_tls_mode)
manifest_server_name=$(read_pair "$manifest" message_server_name)
manifest_machine_id=$(read_pair "$manifest" runner.machine_id)
manifest_engine_id=$(read_pair "$manifest" runner.docker_engine_id)
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "manifest stack identity is not a 128-bit generated namespace"
[ "$stack_nonce" = "${stack_name#d-}" ] || die "manifest stack nonce does not bind to stack identity"
printf '%s\n' "$manifest_agent_id" | grep -Eq '^[0-9a-f-]{36}$' || die "manifest Agent instance ID is invalid"
printf '%s\n' "$manifest_message_id" | grep -Eq '^[0-9a-f-]{36}$' || die "manifest message-server instance ID is invalid"
[ "$manifest_agent_id" != "$manifest_message_id" ] || die "manifest instance identities must differ"
printf '%s\n' "$manifest_generation" | grep -Eq '^[1-9][0-9]*$' || die "manifest account generation is invalid"
printf '%s\n' "$manifest_machine_id" | grep -Eq '^[0-9a-f]{32}$' || die "manifest machine-id is invalid"
printf '%s\n' "$manifest_engine_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || die "manifest Docker Engine ID is invalid"
for host_port in "$manifest_http_bind" "$manifest_https_bind"; do
  printf '%s\n' "$host_port" | grep -Eq '^[1-9][0-9]{3,4}$' || die "manifest host port is invalid"
  [ "$host_port" -ge 1024 ] && [ "$host_port" -le 65535 ] || die "manifest host port is outside the unprivileged range"
done
[ "$manifest_http_bind" != "$manifest_https_bind" ] || die "manifest host ports must differ"
case "$manifest_tls_mode" in
  local)
    [ "$manifest_server_name" = localhost ] || die "local TLS manifest server name must be localhost"
    [ "$manifest_client_base_url" = "http://localhost:$manifest_http_bind" ] || die "local manifest client URL is not derived from its HTTP host port"
    ;;
  external|edge-terminated)
    validate_https_manifest_binding "$manifest_tls_mode" "$manifest_server_name" "$manifest_client_base_url"
    ;;
  *) die "manifest TLS mode is invalid" ;;
esac
[ "$manifest_master_key_path" = "$out/core-secret-master-key" ] || die "manifest Agent master-key path is not bound to the output directory"
[ -f "$manifest_master_key_path" ] && [ ! -L "$manifest_master_key_path" ] || die "Agent master-key file is missing or symlinked"
[ "$(stat -c '%a' "$manifest_master_key_path")" = 400 ] || die "Agent master-key file must be mode 0400"
[ "$(stat -c '%s' "$manifest_master_key_path")" = 32 ] || die "Agent master-key file must contain exactly 32 raw bytes"
[ "$(stat -c '%d' "$manifest_master_key_path")" = "$manifest_master_key_device" ] || die "Agent master-key device changed after provisioning"
[ "$(stat -c '%i' "$manifest_master_key_path")" = "$manifest_master_key_inode" ] || die "Agent master-key inode changed after provisioning"
[ "$(stat -c '%u' "$manifest_master_key_path")" = "$manifest_master_key_uid" ] || die "Agent master-key owner changed after provisioning"
[ "$manifest_master_key_uid" = "$(id -u)" ] || die "Agent master-key is not owned by the cleanup user"

env_identity=$(stat -c '%d:%i:%u' "$env_file")
manifest_identity=$(stat -c '%d:%i:%u' "$manifest")
receipt_identity=$(stat -c '%d:%i:%u' "$receipt")
receipt_stack=$(read_pair "$receipt" stack_name)
receipt_state=$(read_pair "$receipt" state)
receipt_env_identity=$(read_pair "$receipt" control.env_identity)
receipt_manifest_identity=$(read_pair "$receipt" control.manifest_identity)
receipt_env_sha256=$(read_pair "$receipt" control.env_sha256)
receipt_manifest_sha256=$(read_pair "$receipt" control.manifest_sha256)
receipt_machine_id=$(read_pair "$receipt" host.machine_id)
receipt_engine_id=$(read_pair "$receipt" docker.engine_id)
receipt_context_endpoint=$(read_pair "$receipt" docker.context_endpoint)
receipt_context_socket=$(read_pair "$receipt" docker.context_socket)
[ "$receipt_stack" = "$stack_name" ] || die "cleanup receipt stack identity differs from manifest"
case "$receipt_state" in
  complete|incomplete|starting) ;;
  *) die "cleanup receipt state is unsupported" ;;
esac
[ "$receipt_env_identity" = "$env_identity" ] || die "cleanup receipt was not created for this .env"
[ "$receipt_manifest_identity" = "$manifest_identity" ] || die "cleanup receipt was not created for this manifest"
[ "$receipt_env_sha256" = "$(sha256sum -- "$env_file" | awk '{print $1}')" ] || die "cleanup receipt .env digest differs"
[ "$receipt_manifest_sha256" = "$(sha256sum -- "$manifest" | awk '{print $1}')" ] || die "cleanup receipt manifest digest differs"
[ "$receipt_machine_id" = "$manifest_machine_id" ] || die "cleanup receipt machine-id differs from manifest"
[ "$receipt_engine_id" = "$manifest_engine_id" ] || die "cleanup receipt Engine ID differs from manifest"
printf '%s\n' "$receipt_machine_id" | grep -Eq '^[0-9a-f]{32}$' || die "cleanup receipt machine-id is invalid"
printf '%s\n' "$receipt_engine_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || die "cleanup receipt Engine ID is invalid"
case "$receipt_context_endpoint" in
  unix:///*) ;;
  *) die "cleanup receipt context endpoint is not a local Unix endpoint" ;;
esac
[ "$receipt_context_socket" = /run/docker.sock ] || die "cleanup receipt context socket is not the local rootful socket"

env_stack=$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)
env_agent_id=$(read_pair "$env_file" DIREXTALK_AGENT_INSTANCE_ID)
env_message_id=$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_INSTANCE_ID)
env_generation=$(read_pair "$env_file" DIREXTALK_ACCOUNT_GENERATION)
env_http_bind=$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTP_BIND)
env_https_bind=$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTPS_BIND)
env_client_base_url=$(read_pair "$env_file" DIREXTALK_MESSAGE_CLIENT_BASE_URL)
env_master_key_path=$(read_pair "$env_file" DIREXTALK_CORE_SECRET_MASTER_KEY_FILE)
[ "$env_stack" = "$stack_name" ] || die ".env stack identity differs from the manifest"
[ "$env_agent_id" = "$manifest_agent_id" ] || die ".env Agent instance identity differs from the manifest"
[ "$env_message_id" = "$manifest_message_id" ] || die ".env message-server identity differs from the manifest"
[ "$env_generation" = "$manifest_generation" ] || die ".env account generation differs from the manifest"
[ "$env_http_bind" = "$manifest_http_bind" ] || die ".env HTTP host port differs from the manifest"
[ "$env_https_bind" = "$manifest_https_bind" ] || die ".env HTTPS host port differs from the manifest"
[ "$env_client_base_url" = "$manifest_client_base_url" ] || die ".env client URL differs from the manifest"
[ "$env_master_key_path" = "$manifest_master_key_path" ] || die ".env master-key path differs from the manifest"

network_pairs=(
  'DIREXTALK_MESSAGE_PRIVATE_NETWORK:resource.network.message_private'
  'DIREXTALK_MESSAGE_PUBLIC_NETWORK:resource.network.message_public'
  'DIREXTALK_MESSAGE_DATABASE_NETWORK:resource.network.message_database'
  'DIREXTALK_AGENT_PRIVATE_NETWORK:resource.network.agent_private'
  'DIREXTALK_AGENT_DATABASE_NETWORK:resource.network.agent_database'
  'DIREXTALK_AGENT_CALLER_NETWORK:resource.network.agent_caller'
  'DIREXTALK_AGENT_EGRESS_NETWORK:resource.network.agent_egress'
)
volume_pairs=(
  'DIREXTALK_POSTGRES_VOLUME:resource.volume.postgres'
  'DIREXTALK_MESSAGE_CONFIG_VOLUME:resource.volume.message_config'
  'DIREXTALK_MESSAGE_DATA_VOLUME:resource.volume.message_data'
  'DIREXTALK_MESSAGE_PLUGINS_VOLUME:resource.volume.message_plugins'
  'DIREXTALK_AGENT_SECRET_VOLUME:resource.volume.agent_secrets'
  'DIREXTALK_AGENT_CONFIG_VOLUME:resource.volume.agent_config'
  'DIREXTALK_AGENT_CORE_DATA_VOLUME:resource.volume.agent_core_data'
  'DIREXTALK_AGENT_SOCKET_VOLUME:resource.volume.agent_extension_socket'
  'DIREXTALK_AGENT_INSTALL_VOLUME:resource.volume.agent_extension_install'
  'DIREXTALK_AGENT_STAGING_VOLUME:resource.volume.agent_extension_staging'
  'DIREXTALK_AGENT_RUNNER_WORKSPACE_VOLUME:resource.volume.agent_runner_workspaces'
  'DIREXTALK_AGENT_RUNNER_STATE_VOLUME:resource.volume.agent_runner_state'
  'DIREXTALK_AGENT_KNOWLEDGE_CONTENT_VOLUME:resource.volume.agent_knowledge_content'
  'DIREXTALK_AGENT_KNOWLEDGE_MOUNT_VOLUME:resource.volume.agent_knowledge_mount'
  'DIREXTALK_CAPABILITY_AUTHORITY_VOLUME:resource.volume.capability_authority'
  'DIREXTALK_CAPABILITY_SHARED_VOLUME:resource.volume.capability_shared'
  'DIREXTALK_CAPABILITY_PRIVATE_VOLUME:resource.volume.capability_private'
  'DIREXTALK_CORE_RUNNER_SOCKET_VOLUME:resource.volume.core_runner_socket'
  'DIREXTALK_CORE_RUNNER_INSTALL_VOLUME:resource.volume.core_runner_installs'
  'DIREXTALK_CORE_RUNNER_WORKSPACE_VOLUME:resource.volume.core_runner_workspaces'
  'DIREXTALK_CORE_RUNNER_STATE_VOLUME:resource.volume.core_runner_state'
)
networks=()
volumes=()
bind_resource() {
  local pair=$1 env_key manifest_key expected actual
  env_key=${pair%%:*}
  manifest_key=${pair#*:}
  expected=$(read_pair "$manifest" "$manifest_key")
  actual=$(read_pair "$env_file" "$env_key")
  [ "$actual" = "$expected" ] || die "$env_key was edited outside the manifest target"
  case "$actual" in
    "$stack_name"-*) ;;
    *) die "$env_key is outside the fresh stack namespace" ;;
  esac
  printf '%s' "$actual"
}
for pair in "${network_pairs[@]}"; do
  networks+=("$(bind_resource "$pair")")
done
for pair in "${volume_pairs[@]}"; do
  volumes+=("$(bind_resource "$pair")")
done

if [ "$receipt_state" = starting ]; then
  planned_network_count=$(read_pair "$receipt" planned.network.count)
  planned_volume_count=$(read_pair "$receipt" planned.volume.count)
  [ "$planned_network_count" -eq "${#networks[@]}" ] || die "startup journal planned network count differs from manifest"
  [ "$planned_volume_count" -eq "${#volumes[@]}" ] || die "startup journal planned volume count differs from manifest"
  for ((index = 0; index < planned_network_count; index++)); do
    [ "$(read_pair "$receipt" "planned.network.$index.name")" = "${networks[index]}" ] || \
      die "startup journal planned network differs from manifest"
  done
  for ((index = 0; index < planned_volume_count; index++)); do
    [ "$(read_pair "$receipt" "planned.volume.$index.name")" = "${volumes[index]}" ] || \
      die "startup journal planned volume differs from manifest"
  done
  "$script_dir/recover-starting-cleanup-receipt.sh" "$out"
  if [ "$purge" = true ]; then
    exec "$0" --purge "$out"
  fi
  exec "$0" "$out"
fi

container_count=$(read_pair "$receipt" container.count)
network_count=$(read_pair "$receipt" network.count)
volume_count=$(read_pair "$receipt" volume.count)
for count_pair in "container:$container_count" "network:$network_count" "volume:$volume_count"; do
  count_name=${count_pair%%:*}
  count_value=${count_pair#*:}
  if [ "$receipt_state" = complete ]; then
    printf '%s\n' "$count_value" | grep -Eq '^[1-9][0-9]{0,3}$' || die "cleanup receipt $count_name count is invalid"
  else
    printf '%s\n' "$count_value" | grep -Eq '^[0-9][0-9]{0,3}$' || die "cleanup receipt $count_name count is invalid"
  fi
done
if [ "$receipt_state" = complete ]; then
  [ "$network_count" -eq "${#networks[@]}" ] || die "cleanup receipt network count differs from manifest"
  [ "$volume_count" -eq "${#volumes[@]}" ] || die "cleanup receipt volume count differs from manifest"
else
  [ "$network_count" -le "${#networks[@]}" ] || die "incomplete cleanup receipt has too many networks"
  [ "$volume_count" -le "${#volumes[@]}" ] || die "incomplete cleanup receipt has too many volumes"
fi

container_ids=()
container_names=()
container_services=()
container_projects=()
for ((index = 0; index < container_count; index++)); do
  container_ids[index]=$(read_pair "$receipt" "container.$index.id")
  container_names[index]=$(read_pair "$receipt" "container.$index.name")
  container_services[index]=$(read_pair "$receipt" "container.$index.service")
  container_projects[index]=$(read_pair "$receipt" "container.$index.project")
  printf '%s\n' "${container_ids[index]}" | grep -Eq '^[0-9a-f]{64}$' || die "cleanup receipt container ID is not a full immutable ID"
  printf '%s\n' "${container_names[index]}" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || die "cleanup receipt container name is invalid"
  printf '%s\n' "${container_services[index]}" | grep -Eq '^[a-z0-9][a-z0-9_-]*$' || die "cleanup receipt container service is invalid"
  [ "${container_projects[index]}" = "$stack_name" ] || die "cleanup receipt container project is not this stack"
done

network_ids=()
network_receipt_names=()
network_projects=()
for ((index = 0; index < network_count; index++)); do
  network_ids[index]=$(read_pair "$receipt" "network.$index.id")
  network_receipt_names[index]=$(read_pair "$receipt" "network.$index.name")
  network_projects[index]=$(read_pair "$receipt" "network.$index.project")
  printf '%s\n' "${network_ids[index]}" | grep -Eq '^[0-9a-f]{64}$' || die "cleanup receipt network ID is not a full immutable ID"
  printf '%s\n' "${network_receipt_names[index]}" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || die "cleanup receipt network name is invalid"
  [ "${network_projects[index]}" = "$stack_name" ] || die "cleanup receipt network project is not this stack"
  if [ "$receipt_state" = complete ]; then
    [ "${network_receipt_names[index]}" = "${networks[index]}" ] || die "cleanup receipt network name differs from manifest"
  else
    network_known=false
    for planned_name in "${networks[@]}"; do
      [ "${network_receipt_names[index]}" = "$planned_name" ] && network_known=true
    done
    [ "$network_known" = true ] || die "incomplete cleanup receipt network is outside the planned namespace"
  fi
done

volume_names=()
volume_projects=()
volume_fingerprints=()
for ((index = 0; index < volume_count; index++)); do
  volume_names[index]=$(read_pair "$receipt" "volume.$index.name")
  volume_projects[index]=$(read_pair "$receipt" "volume.$index.project")
  volume_fingerprints[index]=$(read_pair "$receipt" "volume.$index.fingerprint_sha256")
  printf '%s\n' "${volume_names[index]}" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || die "cleanup receipt volume name is invalid"
  [ "${volume_projects[index]}" = "$stack_name" ] || die "cleanup receipt volume project is not this stack"
  if [ "$receipt_state" = complete ]; then
    [ "${volume_names[index]}" = "${volumes[index]}" ] || die "cleanup receipt volume name differs from manifest"
  else
    volume_known=false
    for planned_name in "${volumes[@]}"; do
      [ "${volume_names[index]}" = "$planned_name" ] && volume_known=true
    done
    [ "$volume_known" = true ] || die "incomplete cleanup receipt volume is outside the planned namespace"
  fi
  printf '%s\n' "${volume_fingerprints[index]}" | grep -Eq '^[0-9a-f]{64}$' || die "cleanup receipt volume fingerprint is invalid"
done

runner_units=(extension core)
runner_unit_names=("$(read_pair "$receipt" runner.extension.unit)" "$(read_pair "$receipt" runner.core.unit)")
runner_control_groups=("$(read_pair "$receipt" runner.extension.control_group)" "$(read_pair "$receipt" runner.core.control_group)")
runner_main_pids=("$(read_pair "$receipt" runner.extension.main_pid)" "$(read_pair "$receipt" runner.core.main_pid)")
runner_fragment_paths=("$(read_pair "$receipt" runner.extension.fragment_path)" "$(read_pair "$receipt" runner.core.fragment_path)")
runner_fragment_hashes=("$(read_pair "$receipt" runner.extension.fragment_sha256)" "$(read_pair "$receipt" runner.core.fragment_sha256)")
runner_parent_roots=("$(read_pair "$manifest" runner.extension.parent_root)" "$(read_pair "$manifest" runner.core.parent_root)")
runner_parent_procs=("$(read_pair "$manifest" runner.extension.parent_procs)" "$(read_pair "$manifest" runner.core.parent_procs)")
runner_parent_procs_owners=("$(read_pair "$manifest" runner.extension.parent_procs_owner)" "$(read_pair "$manifest" runner.core.parent_procs_owner)")
runner_parent_procs_modes=("$(read_pair "$manifest" runner.extension.parent_procs_mode)" "$(read_pair "$manifest" runner.core.parent_procs_mode)")
for ((index = 0; index < 2; index++)); do
  role=${runner_units[index]}
  printf '%s\n' "${runner_unit_names[index]}" | grep -Eq "^dirextalk-${role}-runner@${stack_name}\.service$" || die "$role runner unit is not stack-bound"
  printf '%s\n' "${runner_control_groups[index]}" | grep -Eq '^/[^/[:space:]][^[:space:]]*$' || die "$role runner ControlGroup is invalid"
  printf '%s\n' "${runner_main_pids[index]}" | grep -Eq '^[1-9][0-9]*$' || die "$role runner MainPID is invalid"
  printf '%s\n' "${runner_fragment_hashes[index]}" | grep -Eq '^[0-9a-f]{64}$' || die "$role runner fragment hash is invalid"
  [ -f "${runner_fragment_paths[index]}" ] && [ ! -L "${runner_fragment_paths[index]}" ] || die "$role runner fragment is missing or symlinked"
  [ "$(stat -c '%u:%g' "${runner_fragment_paths[index]}")" = 0:0 ] || die "$role runner fragment is not root-owned"
  [ "$(stat -c '%a' "${runner_fragment_paths[index]}")" = 644 ] || die "$role runner fragment mode is not 0644"
  [ "$(sha256sum -- "${runner_fragment_paths[index]}" | awk '{print $1}')" = "${runner_fragment_hashes[index]}" ] || die "$role runner fragment hash changed"
  [ "${runner_parent_roots[index]}" = "/sys/fs/cgroup${runner_control_groups[index]%/*}" ] || die "$role runner parent root is not receipt-bound"
  [ "${runner_parent_procs[index]}" = "${runner_parent_roots[index]}/cgroup.procs" ] || die "$role runner parent process control path is not exact"
  printf '%s\n' "${runner_parent_procs_owners[index]}" | grep -Eq '^[0-9]+:[0-9]+$' || die "$role runner parent process control owner is invalid"
  [ "${runner_parent_procs_modes[index]}" = 644 ] || die "$role runner parent process control mode is invalid"
done

command -v docker >/dev/null 2>&1 || die "docker is required for exact-target cleanup"
command -v jq >/dev/null 2>&1 || die "jq is required for exact-target cleanup"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required for exact-target cleanup"
command -v systemctl >/dev/null 2>&1 || die "systemctl is required to stop the exact runner units"

inspection_error_file=$(mktemp "$out/.cleanup-inspect.XXXXXX") || die "cannot allocate Docker inspect error workspace"
chmod 600 "$inspection_error_file"
cleanup_inspection_workspace() {
  rm -f -- "$inspection_error_file"
}
trap cleanup_inspection_workspace EXIT

verify_control_receipt() {
  [ -f "$receipt" ] && [ ! -L "$receipt" ] || die "cleanup receipt was replaced"
  [ "$(stat -c '%d:%i:%u' "$receipt")" = "$receipt_identity" ] || die "cleanup receipt identity changed"
  [ "$(stat -c '%a' "$receipt")" = 400 ] || die "cleanup receipt permissions changed"
  [ "$(stat -c '%u' "$receipt")" = "$(id -u)" ] || die "cleanup receipt owner changed"
  [ "$(stat -c '%d:%i:%u' "$env_file")" = "$env_identity" ] || die ".env identity changed before cleanup mutation"
  [ "$(stat -c '%d:%i:%u' "$manifest")" = "$manifest_identity" ] || die ".manifest identity changed before cleanup mutation"
  [ "$(sha256sum -- "$env_file" | awk '{print $1}')" = "$receipt_env_sha256" ] || die ".env contents changed before cleanup mutation"
  [ "$(sha256sum -- "$manifest" | awk '{print $1}')" = "$receipt_manifest_sha256" ] || die ".manifest contents changed before cleanup mutation"
}

verify_local_docker_identity() {
  local endpoint socket canonical machine engine
  [ -z "${DOCKER_HOST:-}" ] || die "DOCKER_HOST must be unset for local Docker cleanup"
  case "${DOCKER_CONTEXT:-default}" in
    ''|default) ;;
    *) die "DOCKER_CONTEXT must be unset or default for local Docker cleanup" ;;
  esac
  endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null || true)
  [ "$endpoint" = "$receipt_context_endpoint" ] || die "Docker local context endpoint changed"
  case "$endpoint" in
    unix:///*) ;;
    *) die "Docker context is not a local Unix endpoint" ;;
  esac
  socket=${endpoint#unix://}
  [ -S "$socket" ] || die "Docker local context socket is unavailable"
  canonical=$(readlink -f -- "$socket" 2>/dev/null || true)
  [ "$canonical" = "$receipt_context_socket" ] || die "Docker local context socket identity changed"
  [ -f /etc/machine-id ] && [ ! -L /etc/machine-id ] || die "host machine-id is unavailable"
  [ "$(stat -c '%u:%g' /etc/machine-id 2>/dev/null || true)" = 0:0 ] || die "host machine-id is not root-owned"
  machine=$(cat /etc/machine-id 2>/dev/null | tr -d '[:space:]' || true)
  printf '%s\n' "$machine" | grep -Eq '^[0-9a-f]{32}$' || die "host machine-id is invalid"
  [ "$machine" = "$receipt_machine_id" ] || die "host machine-id changed since startup"
  engine=$(docker info --format '{{.ID}}' 2>/dev/null || true)
  [ "$engine" = "$receipt_engine_id" ] || die "Docker Engine ID changed since startup"
}

volume_fingerprint() {
  local data=$1 fingerprint_json
  if fingerprint_json=$(jq -c -e '
    .[0] as $v |
    if (($v | has("Name")) and (($v.Name | type) == "string") and (($v.Name | length) > 0) and
        ($v | has("Driver")) and (($v.Driver | type) == "string") and (($v.Driver | length) > 0) and
        ($v | has("Scope")) and (($v.Scope | type) == "string") and (($v.Scope | length) > 0) and
        ($v | has("CreatedAt")) and (($v.CreatedAt | type) == "string") and (($v.CreatedAt | length) > 0) and
        ($v | has("Mountpoint")) and (($v.Mountpoint | type) == "string") and (($v.Mountpoint | length) > 0) and
        ($v | has("Labels")) and (($v.Labels == null) or (($v.Labels | type) == "object")) and
        ($v | has("Options")) and (($v.Options == null) or (($v.Options | type) == "object")))
    then {Name:$v.Name, Driver:$v.Driver, Scope:$v.Scope, CreatedAt:$v.CreatedAt,
          Mountpoint:$v.Mountpoint,
          Labels:($v.Labels // {} | to_entries | sort_by(.key) | from_entries),
          Options:($v.Options // {} | to_entries | sort_by(.key) | from_entries)}
    else error("volume identity metadata is incomplete")
    end
  ' <<<"$data"); then
    :
  else
    die "Docker volume fingerprint extraction failed"
  fi
  printf '%s' "$fingerprint_json" | sha256sum | awk '{print $1}'
}

inspect_object() {
  local kind=$1 target=$2 data status
  local -a inspect_command=()
  case "$kind" in
    container) inspect_command=(docker inspect "$target") ;;
    network|volume) inspect_command=(docker "$kind" inspect "$target") ;;
    *) die "unsupported Docker inspect kind: $kind" ;;
  esac
  : >"$inspection_error_file"
  if data=$("${inspect_command[@]}" 2>"$inspection_error_file"); then
    if ! jq -e 'type == "array" and length == 1' <<<"$data" >/dev/null; then
      printf 'split-stack cleanup: Docker %s inspect returned malformed JSON for %s\n' "$kind" "$target" >&2
      return 2
    fi
    printf '%s' "$data"
    return 0
  else
    status=$?
  fi
  if [ "$status" -eq 1 ] && grep -Eiq 'no such (object|container|network|volume)|not found' "$inspection_error_file"; then
    return 1
  fi
  printf 'split-stack cleanup: Docker %s inspect failed for %s (status %s)\n' "$kind" "$target" "$status" >&2
  return 2
}

inspect_container_exact() {
  local index=$1 id=${container_ids[$1]} data actual_id raw_name actual_name service project
  if data=$(inspect_object container "$id"); then
    :
  else
    status=$?
    [ "$status" -eq 1 ] && return 1
    die "container identity inspection failed for ${container_names[index]} (status $status)"
  fi
  actual_id=$(jq -r '.[0].Id // empty' <<<"$data")
  raw_name=$(jq -r '.[0].Name // empty' <<<"$data")
  service=$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")
  project=$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")
  actual_name=${raw_name#/}
  [ "$actual_id" = "$id" ] || die "Compose container ID changed for ${container_names[index]}"
  [ "$actual_name" = "${container_names[index]}" ] || die "Compose container name changed for ${container_names[index]}"
  [ "$service" = "${container_services[index]}" ] || die "Compose container service changed for ${container_names[index]}"
  [ "$project" = "${container_projects[index]}" ] || die "Compose container project changed for ${container_names[index]}"
}

verify_no_container_replacement() {
  local index=$1 id candidate_ids candidate data actual_name project status
  if candidate_ids=$(docker ps -aq --filter "label=com.docker.compose.project=$stack_name"); then
    :
  else
    die "container replacement inspection failed"
  fi
  while IFS= read -r candidate; do
    [ -n "$candidate" ] || continue
    [ "$candidate" = "${container_ids[index]}" ] && continue
    if data=$(inspect_object container "$candidate"); then
      actual_name=$(jq -r '.[0].Name // empty' <<<"$data")
      project=$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")
      actual_name=${actual_name#/}
      [ "$project" = "$stack_name" ] || die "container replacement has an unexpected Compose project"
      [ "$actual_name" != "${container_names[index]}" ] || die "Compose container same-name replacement detected: ${container_names[index]}"
    else
      status=$?
      [ "$status" -eq 1 ] || die "container replacement identity inspection failed (status $status)"
    fi
  done <<<"$candidate_ids"
}

inspect_network_exact() {
  local index=$1 id=${network_ids[$1]} name=${network_receipt_names[$1]} data actual_id actual_name project replacement replacement_id replacement_name replacement_project status
  if data=$(inspect_object network "$id"); then
    :
  else
    status=$?
    [ "$status" -eq 1 ] || die "network identity inspection failed for $name (status $status)"
    if replacement=$(inspect_object network "$name"); then
      replacement_id=$(jq -r '.[0].Id // empty' <<<"$replacement")
      [ "$replacement_id" = "$id" ] || die "Compose network identity changed for $name"
      replacement_name=$(jq -r '.[0].Name // empty' <<<"$replacement")
      replacement_project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$replacement")
      [ "$replacement_name" = "$name" ] && [ "$replacement_project" = "${network_projects[index]}" ] || die "Compose network identity changed for $name"
      return 0
    else
      status=$?
      [ "$status" -eq 1 ] || die "network replacement inspection failed for $name (status $status)"
    fi
    return 1
  fi
  actual_id=$(jq -r '.[0].Id // empty' <<<"$data")
  actual_name=$(jq -r '.[0].Name // empty' <<<"$data")
  project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$data")
  [ "$actual_id" = "$id" ] || die "Compose network ID changed for $name"
  [ "$actual_name" = "$name" ] || die "Compose network name changed for $name"
  [ "$project" = "${network_projects[index]}" ] || die "Compose network project changed for $name"
}

inspect_volume_exact() {
  local index=$1 name=${volume_names[$1]} data actual_name project fingerprint status
  if data=$(inspect_object volume "$name"); then
    :
  else
    status=$?
    [ "$status" -eq 1 ] && return 1
    die "volume identity inspection failed for $name (status $status)"
  fi
  actual_name=$(jq -r '.[0].Name // empty' <<<"$data")
  project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$data")
  fingerprint=$(volume_fingerprint "$data")
  [ "$actual_name" = "$name" ] || die "Compose volume name changed for $name"
  [ "$project" = "${volume_projects[index]}" ] || die "Compose volume project changed for $name"
  [ "$fingerprint" = "${volume_fingerprints[index]}" ] || die "Compose volume fingerprint changed for $name"
}

verify_runner_unit_exact() {
  local index=$1 role=${runner_units[$1]} unit=${runner_unit_names[$1]}
  local load_state active_state sub_state fragment fragment_hash control_group main_pid enabled
  enabled=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  case "$enabled" in
    disabled|masked|not-found|'') return 1 ;;
    enabled) ;;
    *) die "$role runner unit enablement state is unsafe: $enabled" ;;
  esac
  load_state=$(systemctl show "$unit" --property=LoadState --value 2>/dev/null || true)
  case "$load_state" in
    not-found|'') return 1 ;;
    loaded) ;;
    *) die "$role runner unit load state is unsafe: $load_state" ;;
  esac
  active_state=$(systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)
  case "$active_state" in
    inactive|failed|deactivating) ;;
    active) ;;
    *) die "$role runner unit active state is unsafe: $active_state" ;;
  esac
  fragment=$(systemctl show "$unit" --property=FragmentPath --value 2>/dev/null || true)
  [ "$fragment" = "${runner_fragment_paths[index]}" ] || die "$role runner FragmentPath changed"
  [ -f "$fragment" ] && [ ! -L "$fragment" ] || die "$role runner fragment was replaced"
  fragment_hash=$(sha256sum -- "$fragment" | awk '{print $1}')
  [ "$fragment_hash" = "${runner_fragment_hashes[index]}" ] || die "$role runner fragment hash changed"
  if [ "$active_state" = active ]; then
    sub_state=$(systemctl show "$unit" --property=SubState --value 2>/dev/null || true)
    [ "$sub_state" = running ] || die "$role runner unit sub-state is unsafe: $sub_state"
    control_group=$(systemctl show "$unit" --property=ControlGroup --value 2>/dev/null || true)
    [ "$control_group" = "${runner_control_groups[index]}" ] || die "$role runner ControlGroup changed"
    main_pid=$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)
    [ "$main_pid" = "${runner_main_pids[index]}" ] || die "$role runner MainPID changed"
  fi
}

verify_runner_unit_disabled() {
  local index=$1 role=${runner_units[$1]} unit=${runner_unit_names[$1]} active enabled
  active=$(systemctl is-active "$unit" 2>/dev/null || true)
  [ "$active" != active ] || die "$role runner unit remained active after disable --now"
  enabled=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  case "$enabled" in
    disabled|masked|not-found|'') ;;
    *) die "$role runner unit remained enabled after disable --now: $enabled" ;;
  esac
}

before_mutation() {
  verify_control_receipt
  verify_local_docker_identity
}

# Read-only preflight over every recorded object.  A missing recorded object
# is tolerated for idempotent cleanup; a same-name replacement is rejected by
# the exact network/volume checks and cannot be selected by a later mutation.
verify_local_docker_identity
for ((index = 0; index < container_count; index++)); do
  if ! inspect_container_exact "$index"; then
    verify_no_container_replacement "$index"
  fi
done
for ((index = 0; index < network_count; index++)); do
  if inspect_network_exact "$index"; then
    :
  else
    status=$?
    [ "$status" -eq 1 ] || die "network preflight inspection failed (status $status)"
  fi
done
for ((index = 0; index < volume_count; index++)); do
  if inspect_volume_exact "$index"; then
    :
  else
    status=$?
    [ "$status" -eq 1 ] || die "volume preflight inspection failed (status $status)"
  fi
done

# Remove only the exact recorded containers.  `docker compose down` and
# project/name-only stop paths are intentionally forbidden here.
for ((index = 0; index < container_count; index++)); do
  if inspect_container_exact "$index"; then
    before_mutation
    inspect_container_exact "$index" || die "recorded container disappeared before removal: ${container_names[index]}"
    docker container rm -f "${container_ids[index]}" >/dev/null || die "exact container removal failed: ${container_names[index]}"
  else
    verify_no_container_replacement "$index"
  fi
done

# The runner host units are also exact receipt-bound objects.  Stop only the
# two stack-specific units after their cgroup/control-group identity is
# revalidated; never stop a broad slice or same-name replacement.
for ((index = 0; index < 2; index++)); do
  if verify_runner_unit_exact "$index"; then
    before_mutation
    verify_runner_unit_exact "$index" || die "recorded ${runner_units[index]} runner changed before stop"
    systemctl disable --now "${runner_unit_names[index]}" || die "exact ${runner_units[index]} runner disable failed"
    verify_runner_unit_disabled "$index"
  fi
done

# The profile is host-global, so removal is attempted only after every exact
# stack container is gone. The manager returns 3 when another stopped/running
# container or process still references it; that is an expected shared-host
# state, while identity or Docker/AppArmor failures remain fatal.
if apparmor_cleanup_output=$("$script_dir/manage-runner-apparmor.sh" remove 2>&1); then
  :
else
  apparmor_cleanup_status=$?
  case "$apparmor_cleanup_status" in
    3) printf '%s\n' "$apparmor_cleanup_output" >&2 ;;
    *) die "runner AppArmor cleanup failed (status $apparmor_cleanup_status): $apparmor_cleanup_output" ;;
  esac
fi

for ((index = 0; index < network_count; index++)); do
  if inspect_network_exact "$index"; then
    before_mutation
    inspect_network_exact "$index" || die "recorded network disappeared before removal: ${network_receipt_names[index]}"
    docker network rm "${network_ids[index]}" >/dev/null || die "exact network removal failed: ${network_receipt_names[index]}"
  fi
done

if [ "$purge" = true ]; then
  for ((index = 0; index < volume_count; index++)); do
    if inspect_volume_exact "$index"; then
      before_mutation
      inspect_volume_exact "$index" || die "recorded volume disappeared before purge: ${volume_names[index]}"
      docker volume rm "${volume_names[index]}" >/dev/null || die "exact volume purge failed: ${volume_names[index]}"
    fi
  done
fi

printf 'split-stack cleanup complete; generated files remain at %s for audit (purge=%s)\n' "$out" "$purge"
