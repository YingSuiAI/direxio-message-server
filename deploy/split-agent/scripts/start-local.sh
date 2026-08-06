#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ENV_FILE" >&2
  exit 2
}

die() {
  echo "split-stack start: $*" >&2
  exit 1
}

[ "$#" -eq 1 ] || usage
[ "${DIREXTALK_SPLIT_FIXTURE_MODE:-false}" != true ] || die "fixture mode is forbidden for production start-local"
env_input=$1
case "$env_input" in
  /*) env_file=$(readlink -m -- "$env_input") ;;
  *) env_file=$(readlink -m -- "$(pwd -P)/$env_input") ;;
esac
out=$(dirname -- "$env_file")
manifest=$out/.manifest
current_uid=$(id -u)

[ -d "$out" ] && [ ! -L "$out" ] || die "environment directory must be a regular non-symlink directory"
[ "$(stat -c '%a' "$out")" = 700 ] || die "environment directory must be mode 0700"
[ "$(stat -c '%u' "$out")" = "$current_uid" ] || die "environment directory must be owned by the startup user"
for control_file in "$env_file" "$manifest"; do
  [ -f "$control_file" ] && [ ! -L "$control_file" ] || die "missing regular control file: $control_file"
  [ "$(stat -c '%a' "$control_file")" = 400 ] || die "control file must be mode 0400: $control_file"
  [ "$(stat -c '%u' "$control_file")" = "$current_uid" ] || die "control file must be owned by the startup user: $control_file"
done

env_identity=$(stat -c '%d:%i:%u' "$env_file")
manifest_identity=$(stat -c '%d:%i:%u' "$manifest")
env_digest=$(sha256sum -- "$env_file" | awk '{print $1}')
manifest_digest=$(sha256sum -- "$manifest" | awk '{print $1}')
verify_control_identity() {
  [ -f "$env_file" ] && [ ! -L "$env_file" ] || die ".env was replaced during startup"
  [ -f "$manifest" ] && [ ! -L "$manifest" ] || die ".manifest was replaced during startup"
  [ "$(stat -c '%d:%i:%u' "$env_file")" = "$env_identity" ] || die ".env identity changed during startup"
  [ "$(stat -c '%d:%i:%u' "$manifest")" = "$manifest_identity" ] || die ".manifest identity changed during startup"
  [ "$(stat -c '%a' "$env_file")" = 400 ] || die ".env permissions changed during startup"
  [ "$(stat -c '%a' "$manifest")" = 400 ] || die ".manifest permissions changed during startup"
  [ "$(sha256sum -- "$env_file" | awk '{print $1}')" = "$env_digest" ] || die ".env content changed during startup"
  [ "$(sha256sum -- "$manifest" | awk '{print $1}')" = "$manifest_digest" ] || die ".manifest content changed during startup"
  for bound_file in "$message_tls_cert" "$message_tls_key" "$image_attestation"; do
    [ -f "$bound_file" ] && [ ! -L "$bound_file" ] || die "bound deployment file was replaced during startup: $bound_file"
  done
  [ "$(stat -c '%d:%i:%u:%a' -- "$message_tls_cert")" = "$message_tls_cert_identity" ] || die "message TLS certificate identity changed during startup"
  [ "$(stat -c '%d:%i:%u:%a' -- "$message_tls_key")" = "$message_tls_key_identity" ] || die "message TLS key identity changed during startup"
  [ "$(stat -c '%d:%i:%u:%a' -- "$image_attestation")" = "$image_attestation_identity" ] || die "image attestation identity changed during startup"
  [ "$(sha256sum -- "$message_tls_cert" | awk '{print $1}')" = "$message_tls_cert_digest" ] || die "message TLS certificate changed during startup"
  [ "$(sha256sum -- "$message_tls_key" | awk '{print $1}')" = "$message_tls_key_digest" ] || die "message TLS key changed during startup"
  [ "$(sha256sum -- "$image_attestation" | awk '{print $1}')" = "$image_attestation_digest" ] || die "image attestation changed during startup"
  [ "$(stat -c '%d:%i:%u:%g:%a' -- "$runner_apparmor_profile_path")" = "$runner_apparmor_profile_identity" ] || die "runner AppArmor profile identity changed during startup"
  [ "$(stat -c '%d:%i:%u:%g:%a' -- "$runner_apparmor_manager_path")" = "$runner_apparmor_manager_identity" ] || die "runner AppArmor manager identity changed during startup"
  [ "$(sha256sum -- "$runner_apparmor_profile_path" | awk '{print $1}')" = "$runner_apparmor_profile_sha256" ] || die "runner AppArmor profile changed during startup"
  [ "$(sha256sum -- "$runner_apparmor_manager_path" | awk '{print $1}')" = "$runner_apparmor_manager_sha256" ] || die "runner AppArmor manager changed during startup"
  "$runner_apparmor_manager_path" verify >/dev/null || die "runner AppArmor profile is no longer loaded and exact"
}

read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

stack_name=$(read_pair "$manifest" stack_name)
compose_mode=$(read_pair "$manifest" compose_mode)
manifest_agent_id=$(read_pair "$manifest" agent_instance_id)
manifest_message_id=$(read_pair "$manifest" message_instance_id)
manifest_generation=$(read_pair "$manifest" account_generation)
manifest_http_bind=$(read_pair "$manifest" message_http_bind)
manifest_https_bind=$(read_pair "$manifest" message_https_bind)
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "manifest stack identity is invalid"
case "$compose_mode" in local|production) ;; *) die "manifest compose mode is invalid" ;; esac
printf '%s\n' "$manifest_agent_id" | grep -Eq '^[0-9a-f-]{36}$' || die "manifest Agent instance ID is invalid"
printf '%s\n' "$manifest_message_id" | grep -Eq '^[0-9a-f-]{36}$' || die "manifest message-server instance ID is invalid"
[ "$manifest_agent_id" != "$manifest_message_id" ] || die "manifest instance identities must differ"
printf '%s\n' "$manifest_generation" | grep -Eq '^[1-9][0-9]*$' || die "manifest account generation is invalid"
for host_port in "$manifest_http_bind" "$manifest_https_bind"; do
  printf '%s\n' "$host_port" | grep -Eq '^[1-9][0-9]{3,4}$' || die "manifest host port is invalid: $host_port"
  [ "$host_port" -ge 1024 ] && [ "$host_port" -le 65535 ] || die "manifest host port is outside [1024,65535]: $host_port"
done
[ "$manifest_http_bind" != "$manifest_https_bind" ] || die "manifest HTTP and HTTPS ports must differ"

[ "$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)" = "$stack_name" ] || die ".env stack identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_SPLIT_COMPOSE_MODE)" = "$compose_mode" ] || die ".env compose mode differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_AGENT_INSTANCE_ID)" = "$manifest_agent_id" ] || die ".env Agent identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_INSTANCE_ID)" = "$manifest_message_id" ] || die ".env message-server identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_ACCOUNT_GENERATION)" = "$manifest_generation" ] || die ".env account generation differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTP_BIND)" = "$manifest_http_bind" ] || die ".env HTTP bind differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTPS_BIND)" = "$manifest_https_bind" ] || die ".env HTTPS bind differs from manifest"

bind_runner_manifest_value() {
  local env_key=$1 manifest_key=$2 env_value manifest_value
  env_value=$(read_pair "$env_file" "$env_key")
  manifest_value=$(read_pair "$manifest" "$manifest_key")
  [ "$env_value" = "$manifest_value" ] || die "$env_key differs from its manifest runner binding"
  printf '%s' "$env_value"
}

runner_host_prepared=$(bind_runner_manifest_value DIREXTALK_RUNNER_HOST_PREPARED runner_host_prepared)
core_extension_enabled=$(bind_runner_manifest_value DIREXTALK_CORE_EXTENSION_ENABLED core_extension_enabled)
core_workload_enabled=$(bind_runner_manifest_value DIREXTALK_CORE_WORKLOAD_ENABLED core_workload_enabled)
runner_apparmor_profile=$(bind_runner_manifest_value DIREXTALK_RUNNER_APPARMOR_PROFILE runner.apparmor.profile)
runner_apparmor_profile_path=$(bind_runner_manifest_value DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH runner.apparmor.path)
runner_apparmor_profile_sha256=$(bind_runner_manifest_value DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256 runner.apparmor.sha256)
runner_apparmor_manager_path=$(bind_runner_manifest_value DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH runner.apparmor.manager_path)
runner_apparmor_manager_sha256=$(bind_runner_manifest_value DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256 runner.apparmor.manager_sha256)
case "$runner_host_prepared" in true|false) ;; *) die "runner_host_prepared must be exactly true or false" ;; esac
case "$core_extension_enabled" in true|false) ;; *) die "core_extension_enabled must be exactly true or false" ;; esac
case "$core_workload_enabled" in true|false) ;; *) die "core_workload_enabled must be exactly true or false" ;; esac

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
if [ "$compose_mode" = local ]; then
  message_root=$(cd "$script_dir/../../.." && pwd -P)
  agent_root=$(cd "$message_root/../dirextalk-agent" && pwd -P)
  [ "$(read_pair "$env_file" DIREXTALK_AGENT_BUILD_CONTEXT)" = "$agent_root" ] || die ".env Agent build context is not the sibling Agent repository"
  [ "$(read_pair "$env_file" DIREXTALK_MESSAGE_BUILD_CONTEXT)" = "$message_root" ] || die ".env message build context is not this repository"
fi

master_key=$(read_pair "$manifest" core_secret_master_key_path)
[ "$master_key" = "$out/core-secret-master-key" ] || die "manifest master-key path is outside the environment directory"
[ -f "$master_key" ] && [ ! -L "$master_key" ] || die "Agent master-key file is missing or symlinked"
[ "$(stat -c '%a' "$master_key")" = 400 ] || die "Agent master-key file must be mode 0400"
[ "$(stat -c '%s' "$master_key")" = 32 ] || die "Agent master-key file must contain 32 raw bytes"
[ "$(stat -c '%d' "$master_key")" = "$(read_pair "$manifest" core_secret_master_key_device)" ] || die "Agent master-key device changed"
[ "$(stat -c '%i' "$master_key")" = "$(read_pair "$manifest" core_secret_master_key_inode)" ] || die "Agent master-key inode changed"
[ "$(stat -c '%u' "$master_key")" = "$(read_pair "$manifest" core_secret_master_key_uid)" ] || die "Agent master-key owner changed"
[ "$(stat -c '%u' "$master_key")" = "$current_uid" ] || die "Agent master-key is not owned by the startup user"

verify_bound_deployment_file() {
  local label=$1 env_key=$2 manifest_prefix=$3 expected_path=$4 path
  path=$(read_pair "$env_file" "$env_key")
  [ "$path" = "$expected_path" ] || die "$label path is outside the environment directory"
  [ "$path" = "$(read_pair "$manifest" "${manifest_prefix}_path")" ] || die "$label path differs from manifest"
  [ -f "$path" ] && [ ! -L "$path" ] || die "$label is missing or symlinked"
  [ "$(stat -c '%a' -- "$path")" = 400 ] || die "$label must be mode 0400"
  [ "$(stat -c '%d' -- "$path")" = "$(read_pair "$manifest" "${manifest_prefix}_device")" ] || die "$label device changed"
  [ "$(stat -c '%i' -- "$path")" = "$(read_pair "$manifest" "${manifest_prefix}_inode")" ] || die "$label inode changed"
  [ "$(stat -c '%u' -- "$path")" = "$(read_pair "$manifest" "${manifest_prefix}_uid")" ] || die "$label owner changed"
  [ "$(stat -c '%u' -- "$path")" = "$current_uid" ] || die "$label is not owned by the startup user"
  [ "$(sha256sum -- "$path" | awk '{print $1}')" = "$(read_pair "$manifest" "${manifest_prefix}_sha256")" ] || die "$label content differs from manifest"
  printf '%s' "$path"
}

message_tls_cert=$(verify_bound_deployment_file "message TLS certificate" DIREXTALK_MESSAGE_TLS_CERT_FILE message_tls_cert "$out/message-tls-external-cert.pem")
message_tls_key=$(verify_bound_deployment_file "message TLS key" DIREXTALK_MESSAGE_TLS_KEY_FILE message_tls_key "$out/message-tls-external-key.pem")
image_attestation=$(verify_bound_deployment_file "image attestation" DIREXTALK_IMAGE_ATTESTATION_FILE image_attestation "$out/image-attestation")
message_tls_cert_identity=$(stat -c '%d:%i:%u:%a' -- "$message_tls_cert")
message_tls_key_identity=$(stat -c '%d:%i:%u:%a' -- "$message_tls_key")
image_attestation_identity=$(stat -c '%d:%i:%u:%a' -- "$image_attestation")
message_tls_cert_digest=$(sha256sum -- "$message_tls_cert" | awk '{print $1}')
message_tls_key_digest=$(sha256sum -- "$message_tls_key" | awk '{print $1}')
image_attestation_digest=$(sha256sum -- "$image_attestation" | awk '{print $1}')
[ "$compose_mode" = local ] || [ -s "$image_attestation" ] || die "production image attestation is empty"

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
  'DIREXTALK_MESSAGE_POSTGRES_VOLUME:resource.volume.message_postgres'
  'DIREXTALK_MESSAGE_CONFIG_VOLUME:resource.volume.message_config'
  'DIREXTALK_MESSAGE_DATA_VOLUME:resource.volume.message_data'
  'DIREXTALK_MESSAGE_PLUGINS_VOLUME:resource.volume.message_plugins'
  'DIREXTALK_AGENT_POSTGRES_VOLUME:resource.volume.agent_postgres'
  'DIREXTALK_AGENT_SECRET_VOLUME:resource.volume.agent_secrets'
  'DIREXTALK_AGENT_CONFIG_VOLUME:resource.volume.agent_config'
  'DIREXTALK_AGENT_CORE_DATA_VOLUME:resource.volume.agent_core_data'
  'DIREXTALK_AGENT_SOCKET_VOLUME:resource.volume.agent_extension_socket'
  'DIREXTALK_AGENT_INSTALL_VOLUME:resource.volume.agent_extension_install'
  'DIREXTALK_AGENT_STAGING_VOLUME:resource.volume.agent_extension_staging'
  'DIREXTALK_AGENT_WORKSPACE_VOLUME:resource.volume.agent_extension_workspaces'
  'DIREXTALK_AGENT_RUNNER_WORKSPACE_VOLUME:resource.volume.agent_runner_workspaces'
  'DIREXTALK_AGENT_RUNNER_STATE_VOLUME:resource.volume.agent_runner_state'
  'DIREXTALK_AGENT_KNOWLEDGE_CONTENT_VOLUME:resource.volume.agent_knowledge_content'
  'DIREXTALK_AGENT_KNOWLEDGE_MOUNT_VOLUME:resource.volume.agent_knowledge_mount'
  'DIREXTALK_AGENT_QDRANT_VOLUME:resource.volume.agent_qdrant'
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
  [ "$actual" = "$expected" ] || die "$env_key differs from its manifest target"
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

validate_cgroup_parent() {
  local name=$1 value=$2
  printf '%s\n' "$value" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?\.slice$' || die "$name must be a safe systemd slice name"
}

validate_runner_uid() {
  local name=$1 value=$2
  case "$name:$value" in
    DIREXTALK_CORE_EXTENSION_RUNNER_UID:65531|DIREXTALK_CORE_WORKLOAD_RUNNER_UID:65530) ;;
    DIREXTALK_CORE_EXTENSION_RUNNER_UID:* ) die "$name is fixed at 65531 for the bundled runner image" ;;
    DIREXTALK_CORE_WORKLOAD_RUNNER_UID:* ) die "$name is fixed at 65530 for the bundled runner image" ;;
    *) die "$name has an unsupported runner identity" ;;
  esac
}

validate_target_write_access() {
  local name=$1 path=$2 expected_uid=$3 expected_gid=$4 metadata status owner_uid owner_gid mode permissions acl
  if metadata=$(stat -c '%u %g %a' -- "$path" 2>/dev/null); then
    :
  else
    status=$?
    die "$name metadata check failed (status $status): $path"
  fi
  read -r owner_uid owner_gid mode <<<"$metadata"
  [ -n "$owner_uid" ] && [ -n "$owner_gid" ] && [ -n "$mode" ] || die "$name metadata is incomplete: $path"
  permissions=$((8#$mode))
  if [ "$owner_uid" = "$expected_uid" ] && (( permissions & 0200 )); then
    (( permissions & 0002 )) && die "$name must not be world-writable: $path"
    return 0
  fi
  if [ "$owner_gid" = "$expected_gid" ] && (( permissions & 0020 )); then
    (( permissions & 0002 )) && die "$name must not be world-writable: $path"
    return 0
  fi
  (( permissions & 0002 )) && die "$name must not be world-writable: $path"
  if command -v getfacl >/dev/null 2>&1; then
    if acl=$(getfacl -cp -- "$path" 2>/dev/null); then
      if printf '%s\n' "$acl" | awk -F: -v uid="$expected_uid" -v gid="$expected_gid" '
        $1 == "mask" && $2 == "" { mask = $3 }
        $1 == "user" && $2 == uid && $3 ~ /w/ { user_write = 1 }
        $1 == "group" && $2 == gid && $3 ~ /w/ { group_write = 1 }
        END { if ((user_write || group_write) && mask ~ /w/) exit 0; exit 1 }
      '; then
        return 0
      fi
    fi
  fi
  die "$name is not writable by runner UID/GID $expected_uid:$expected_gid: $path"
}

validate_delegated_cgroup_root() {
  local name=$1 value=$2 marker=$3 parent=$4 expected_owner=$5 fs_type owner canonical controllers subtree required
  case "$value" in
    /sys/fs/cgroup|/sys/fs/cgroup/|/sys/fs/cgroup/system.slice|/sys/fs/cgroup/user.slice|/sys/fs/cgroup/global.slice)
      die "$name must be a per-stack delegated subtree, not the cgroup root or a system/user/global slice" ;;
  esac
  case "$value" in
    *"$marker"*) ;;
    *) die "$name must contain the fresh stack identity $marker" ;;
  esac
  case "$value" in
    *"${parent%.slice}"*) ;;
    *) die "$name must be beneath the delegated parent identity ${parent}: $value" ;;
  esac
  [ -d "$value" ] || die "$name must already exist as a delegated cgroup-v2 directory: $value"
  canonical=$(readlink -f -- "$value" 2>/dev/null || true)
  [ "$canonical" = "$value" ] || die "$name must be a canonical path without symlink indirection: $value"
  fs_type=$(stat -fc '%T' "$value" 2>/dev/null || true)
  [ "$fs_type" = cgroup2fs ] || die "$name is not on a cgroup-v2 filesystem: $value"
  [ -f "$value/cgroup.controllers" ] && [ -r "$value/cgroup.controllers" ] || \
    die "$name controllers file is missing or unreadable: $value"
  controllers=$(tr '\n' ' ' <"$value/cgroup.controllers" 2>/dev/null || true)
  [ -n "$controllers" ] || die "$name has no delegated controllers: $value"
  for required in cpu memory pids; do
    printf ' %s ' "$controllers" | grep -Fq " $required " || die "$name does not expose controller $required: $value"
  done
  [ -f "$value/cgroup.subtree_control" ] || die "$name subtree control is missing: $value"
  [ -f "$value/cgroup.procs" ] || die "$name process control is missing: $value"
  subtree=$(tr '\n' ' ' <"$value/cgroup.subtree_control" 2>/dev/null || true)
  for required in cpu memory pids; do
    printf ' %s ' "$subtree" | grep -Fq " $required " || die "$name has not enabled controller $required in subtree_control: $value"
  done
  owner=$(stat -c '%u:%g' "$value" 2>/dev/null || true)
  [ "$owner" = "$expected_owner:$expected_owner" ] || die "$name must be owned by runner UID/GID $expected_owner, got $owner: $value"
  validate_target_write_access "$name delegated root" "$value" "$expected_owner" "$expected_owner"
  validate_target_write_access "$name subtree control" "$value/cgroup.subtree_control" "$expected_owner" "$expected_owner"
  validate_target_write_access "$name process control" "$value/cgroup.procs" "$expected_owner" "$expected_owner"
}

runner_unit_property() {
  local unit=$1 property=$2 value
  value=$(systemctl show "$unit" --property="$property" --value 2>/dev/null || true)
  [ -n "$value" ] || die "$unit has no $property property"
  printf '%s' "$value"
}

validate_runner_delegate() {
  local role=$1 unit=$2 delegate delegate_controllers controller
  local -a controller_items
  delegate=$(runner_unit_property "$unit" Delegate)
  [ "$delegate" = yes ] || die "$role runner Delegate property is not enabled"
  delegate_controllers=$(runner_unit_property "$unit" DelegateControllers)
  read -r -a controller_items <<<"$delegate_controllers"
  [ "${#controller_items[@]}" -eq 3 ] || \
    die "$role runner DelegateControllers must contain exactly cpu memory pids"
  for controller in "${controller_items[@]}"; do
    case "$controller" in
      cpu|memory|pids) ;;
      *) die "$role runner DelegateControllers contains unsupported controller $controller" ;;
    esac
  done
  for controller in cpu memory pids; do
    printf ' %s ' "$delegate_controllers" | grep -Fq " $controller " || \
      die "$role runner DelegateControllers is missing $controller"
  done
}

require_empty_cgroup_procs() {
  local label=$1 path=$2 contents read_status
  if contents=$(tr -d '[:space:]' <"$path" 2>/dev/null); then
    :
  else
    read_status=$?
    die "$label process control read failed (status $read_status)"
  fi
  [ -z "$contents" ] || die "$label has an unexpected direct process"
}

validate_runner_control_group() {
  local name=$1 value=$2 parent=$3 unit=$4
  printf '%s\n' "$value" | grep -Eq '^/[^/[:space:]][^[:space:]]*$' || die "$name is not an absolute ControlGroup path"
  case "$value" in
    *'//'|*'/../'*|*'/./'*) die "$name is not canonical" ;;
    *"/$parent/$unit") ;;
    *) die "$name is not bound to exact parent/unit: $value" ;;
  esac
}

validate_runner_parent_process_control() {
  local role=$1 root=$2 procs=$3 control_group=$4 owner=$5 mode=$6 canonical fs_type
  [ "$root" = "/sys/fs/cgroup${control_group%/*}" ] || die "$role runner parent slice root differs from ControlGroup parent"
  [ "$procs" = "$root/cgroup.procs" ] || die "$role runner parent process control path is not exact"
  [ -d "$root" ] && [ ! -L "$root" ] || die "$role runner parent slice root is missing or symlinked"
  canonical=$(readlink -f -- "$root" 2>/dev/null || true)
  [ "$canonical" = "$root" ] || die "$role runner parent slice root is not canonical"
  fs_type=$(stat -fc '%T' "$root" 2>/dev/null || true)
  [ "$fs_type" = cgroup2fs ] || die "$role runner parent slice root is not cgroup-v2"
  [ "$(stat -c '%u:%g' -- "$root")" = 0:0 ] || die "$role runner parent slice directory is not root-owned"
  [ -f "$procs" ] && [ ! -L "$procs" ] || die "$role runner parent process control is missing or symlinked"
  [ "$(stat -c '%u:%g' -- "$procs")" = "$owner" ] || die "$role runner parent process control owner differs"
  [ "$(stat -c '%a' -- "$procs")" = "$mode" ] || die "$role runner parent process control mode differs"
}

validate_root_owned_asset() {
  local name=$1 path=$2 current parent mode permissions
  [ -f "$path" ] && [ ! -L "$path" ] || die "$name must be a root-owned immutable regular file"
  [ "$(stat -c '%u:%g' -- "$path")" = 0:0 ] || die "$name must be root-owned"
  mode=$(stat -c '%a' -- "$path")
  permissions=$((8#$mode))
  (( (permissions & 18) == 0 )) || die "$name must not be group/world writable"
  current=${path%/*}
  while :; do
    [ -d "$current" ] && [ ! -L "$current" ] || die "$name parent must be a regular directory: $current"
    [ "$(stat -c '%u:%g' -- "$current")" = 0:0 ] || die "$name parent must be root-owned: $current"
    mode=$(stat -c '%a' -- "$current")
    permissions=$((8#$mode))
    (( (permissions & 18) == 0 )) || die "$name parent must not be group/world writable: $current"
    [ "$current" = / ] && break
    parent=${current%/*}
    [ -n "$parent" ] || parent=/
    current=$parent
  done
}

verify_runner_host_binding() {
  local role=$1 unit=$2 user=$3 parent=$4 uid=$5 root=$6 control_group=$7 fragment=$8 fragment_hash=$9
  local actual_fragment actual_hash actual_control_group active_state sub_state enabled main_pid keeper_owner
  local parent_root=${10} parent_procs=${11} parent_procs_owner=${12} parent_procs_mode=${13}
  command -v systemctl >/dev/null 2>&1 || die "systemctl is required for runner host identity verification"
  command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required for runner template verification"
  validate_root_owned_asset DIREXTALK_RUNNER_PREP_HELPER_PATH "$runner_prep_helper_path"
  actual_hash=$(sha256sum -- "$runner_prep_helper_path" | awk '{print $1}')
  [ "$actual_hash" = "$runner_prep_helper_hash" ] || die "runner preparation helper hash differs from manifest"
  [ "$unit" = "dirextalk-${role}-runner@${stack_name}.service" ] || die "$role runner unit is not stack-bound"
  actual_fragment=$(runner_unit_property "$unit" FragmentPath)
  [ "$actual_fragment" = "$fragment" ] || die "$role runner FragmentPath differs from manifest"
  [ -f "$actual_fragment" ] && [ ! -L "$actual_fragment" ] || die "$role runner template is missing or symlinked"
  [ "$(stat -c '%u:%g' -- "$actual_fragment")" = 0:0 ] || die "$role runner template is not root-owned"
  [ "$(stat -c '%a' -- "$actual_fragment")" = 644 ] || die "$role runner template mode is not 0644"
  actual_hash=$(sha256sum -- "$actual_fragment" | awk '{print $1}')
  [ "$actual_hash" = "$fragment_hash" ] || die "$role runner template hash differs from manifest"
  [ "$(runner_unit_property "$unit" User)" = "$user" ] || die "$role runner User property differs"
  [ "$(runner_unit_property "$unit" Group)" = "$user" ] || die "$role runner Group property differs"
  [ "$(runner_unit_property "$unit" Slice)" = "$parent" ] || die "$role runner Slice property differs"
  validate_runner_delegate "$role" "$unit"
  [ "$(runner_unit_property "$unit" DelegateSubgroup)" = keeper ] || die "$role runner DelegateSubgroup differs"
  actual_control_group=$(runner_unit_property "$unit" ControlGroup)
  [ "$actual_control_group" = "$control_group" ] || die "$role runner ControlGroup differs from manifest"
  validate_runner_control_group "$role runner ControlGroup" "$actual_control_group" "$parent" "$unit"
  validate_runner_parent_process_control "$role" "$parent_root" "$parent_procs" "$actual_control_group" "$parent_procs_owner" "$parent_procs_mode"
  active_state=$(runner_unit_property "$unit" ActiveState)
  sub_state=$(runner_unit_property "$unit" SubState)
  [ "$active_state" = active ] && [ "$sub_state" = running ] || die "$role runner unit is not active/running"
  enabled=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  [ "$enabled" = enabled ] || die "$role runner unit is not persistently enabled"
  [ "$root" = "/sys/fs/cgroup${actual_control_group}" ] || die "$role runner root is not bound to ControlGroup"
  validate_delegated_cgroup_root "$role runner root" "$root" "$stack_name" "$parent" "$uid"
  [ -d "$root/keeper" ] && [ ! -L "$root/keeper" ] || die "$role runner keeper subgroup is missing"
  keeper_owner=$(stat -c '%u:%g' -- "$root/keeper")
  [ "$keeper_owner" = "$uid:$uid" ] || die "$role runner keeper owner differs"
  require_empty_cgroup_procs "$role runner delegated root" "$root/cgroup.procs"
  main_pid=$(runner_unit_property "$unit" MainPID)
  printf '%s\n' "$main_pid" | grep -Eq '^[1-9][0-9]*$' || die "$role runner MainPID is invalid"
  grep -Fxq "$main_pid" "$root/keeper/cgroup.procs" || die "$role runner MainPID is not held in keeper subgroup"
}

verify_local_docker_identity() {
  local driver security_options docker_status
  command -v docker >/dev/null 2>&1 || die "docker is required"
  [ -z "${DOCKER_HOST:-}" ] || die "DOCKER_HOST must be unset for the local rootful daemon"
  case "${DOCKER_CONTEXT:-default}" in
    ''|default) ;;
    *) die "DOCKER_CONTEXT must be unset or default for the local rootful daemon" ;;
  esac
  if docker_context_endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker default context inspection failed (status $docker_status)"
  fi
  case "$docker_context_endpoint" in
    unix:///*) ;;
    *) die "Docker default context must use a local Unix socket" ;;
  esac
  docker_context_socket=${docker_context_endpoint#unix://}
  [ -S "$docker_context_socket" ] || die "Docker default context socket is unavailable"
  docker_context_canonical=$(readlink -f -- "$docker_context_socket" 2>/dev/null || true)
  [ "$docker_context_canonical" = /run/docker.sock ] || die "Docker default context socket is not the local rootful socket"
  docker_machine_id=$(cat /etc/machine-id 2>/dev/null | tr -d '[:space:]' || true)
  [ "$docker_machine_id" = "$runner_prep_machine_id" ] || die "host machine-id differs from runner-prep manifest"
  if docker_engine_id=$(docker info --format '{{.ID}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker Engine ID query failed (status $docker_status)"
  fi
  [ "$docker_engine_id" = "$runner_prep_docker_engine_id" ] || die "Docker Engine ID differs from runner-prep manifest"
  if driver=$(docker info --format '{{.CgroupDriver}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker CgroupDriver query failed (status $docker_status)"
  fi
  [ "$driver" = systemd ] || die "Docker Engine must use the systemd cgroup driver for delegated runner subtrees (got ${driver:-unknown})"
  if security_options=$(docker info --format '{{json .SecurityOptions}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker SecurityOptions query failed (status $docker_status)"
  fi
  [ -n "$security_options" ] || die "Docker SecurityOptions query returned no value"
  case "$security_options" in
    *rootless*) die "rootful Docker is required (rootless security option detected)" ;;
  esac
}

extension_cgroup_root=$(bind_runner_manifest_value DIREXTALK_EXTENSION_CGROUP_ROOT runner.extension.root)
core_runner_cgroup_root=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_CGROUP_ROOT runner.core.root)
extension_cgroup_parent=$(bind_runner_manifest_value DIREXTALK_EXTENSION_CGROUP_PARENT runner.extension.parent)
core_runner_cgroup_parent=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_CGROUP_PARENT runner.core.parent)
extension_runner_uid=$(bind_runner_manifest_value DIREXTALK_CORE_EXTENSION_RUNNER_UID runner.extension.uid)
workload_runner_uid=$(bind_runner_manifest_value DIREXTALK_CORE_WORKLOAD_RUNNER_UID runner.core.uid)
extension_runner_unit=$(bind_runner_manifest_value DIREXTALK_EXTENSION_RUNNER_UNIT runner.extension.unit)
core_runner_unit=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_UNIT runner.core.unit)
extension_runner_user=$(bind_runner_manifest_value DIREXTALK_EXTENSION_RUNNER_USER runner.extension.user)
core_runner_user=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_USER runner.core.user)
extension_fragment_path=$(bind_runner_manifest_value DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH runner.extension.fragment_path)
core_fragment_path=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_FRAGMENT_PATH runner.core.fragment_path)
extension_fragment_hash=$(bind_runner_manifest_value DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256 runner.extension.fragment_sha256)
core_fragment_hash=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256 runner.core.fragment_sha256)
runner_prep_helper_path=$(bind_runner_manifest_value DIREXTALK_RUNNER_PREP_HELPER_PATH runner.helper.path)
runner_prep_helper_hash=$(bind_runner_manifest_value DIREXTALK_RUNNER_PREP_HELPER_SHA256 runner.helper.sha256)
runner_prep_machine_id=$(bind_runner_manifest_value DIREXTALK_RUNNER_PREP_MACHINE_ID runner.machine_id)
runner_prep_docker_engine_id=$(bind_runner_manifest_value DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID runner.docker_engine_id)
extension_control_group=$(bind_runner_manifest_value DIREXTALK_EXTENSION_CONTROL_GROUP runner.extension.control_group)
core_control_group=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_CONTROL_GROUP runner.core.control_group)
extension_parent_root=$(bind_runner_manifest_value DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT runner.extension.parent_root)
core_parent_root=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT runner.core.parent_root)
extension_parent_procs=$(bind_runner_manifest_value DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS runner.extension.parent_procs)
core_parent_procs=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS runner.core.parent_procs)
extension_parent_procs_owner=$(bind_runner_manifest_value DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER runner.extension.parent_procs_owner)
core_parent_procs_owner=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER runner.core.parent_procs_owner)
extension_parent_procs_mode=$(bind_runner_manifest_value DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE runner.extension.parent_procs_mode)
core_parent_procs_mode=$(bind_runner_manifest_value DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE runner.core.parent_procs_mode)
[ "$runner_apparmor_profile" = dirextalk-runner-userns ] || die "runner AppArmor profile name is not repository-fixed"
[ "$runner_apparmor_profile_path" = /etc/apparmor.d/dirextalk-runner-userns ] || die "runner AppArmor profile path is not repository-fixed"
[ "$runner_apparmor_manager_path" = "$script_dir/manage-runner-apparmor.sh" ] || die "runner AppArmor manager path is not the repository entrypoint"
printf '%s\n' "$runner_apparmor_profile_sha256" | grep -Eq '^[0-9a-f]{64}$' || die "runner AppArmor profile SHA-256 is invalid"
printf '%s\n' "$runner_apparmor_manager_sha256" | grep -Eq '^[0-9a-f]{64}$' || die "runner AppArmor manager SHA-256 is invalid"
for runner_apparmor_asset in "$runner_apparmor_profile_path" "$runner_apparmor_manager_path"; do
  [ -f "$runner_apparmor_asset" ] && [ ! -L "$runner_apparmor_asset" ] || die "runner AppArmor asset is missing or symlinked: $runner_apparmor_asset"
  [ "$(stat -c '%u:%g' -- "$runner_apparmor_asset")" = 0:0 ] || die "runner AppArmor asset is not root-owned: $runner_apparmor_asset"
  runner_apparmor_mode=$((8#$(stat -c '%a' -- "$runner_apparmor_asset")))
  (( (runner_apparmor_mode & 18) == 0 )) || die "runner AppArmor asset is group/world writable: $runner_apparmor_asset"
done
[ "$(stat -c '%a' -- "$runner_apparmor_profile_path")" = 644 ] || die "runner AppArmor installed profile mode is not 0644"
[ "$(sha256sum -- "$runner_apparmor_profile_path" | awk '{print $1}')" = "$runner_apparmor_profile_sha256" ] || die "runner AppArmor installed profile hash differs from manifest"
[ "$(sha256sum -- "$runner_apparmor_manager_path" | awk '{print $1}')" = "$runner_apparmor_manager_sha256" ] || die "runner AppArmor manager hash differs from manifest"
runner_apparmor_profile_identity=$(stat -c '%d:%i:%u:%g:%a' -- "$runner_apparmor_profile_path")
runner_apparmor_manager_identity=$(stat -c '%d:%i:%u:%g:%a' -- "$runner_apparmor_manager_path")
"$runner_apparmor_manager_path" verify >/dev/null || die "runner AppArmor loaded-profile verification failed"
[ "$(read_pair "$manifest" runner.extension.group)" = "$extension_runner_user" ] || die "extension runner group manifest differs"
[ "$(read_pair "$manifest" runner.core.group)" = "$core_runner_user" ] || die "Core runner group manifest differs"
[ "$(read_pair "$manifest" runner.extension.uid)" = "$extension_runner_uid" ] || die "extension runner UID manifest differs"
[ "$(read_pair "$manifest" runner.core.uid)" = "$workload_runner_uid" ] || die "Core runner UID manifest differs"
[ "$(read_pair "$manifest" runner.extension.gid)" = "$extension_runner_uid" ] || die "extension runner GID manifest differs"
[ "$(read_pair "$manifest" runner.core.gid)" = "$workload_runner_uid" ] || die "Core runner GID manifest differs"
[ "$(read_pair "$manifest" runner.extension.parent)" = "$extension_cgroup_parent" ] || die "extension runner parent manifest differs"
[ "$(read_pair "$manifest" runner.core.parent)" = "$core_runner_cgroup_parent" ] || die "Core runner parent manifest differs"
[ "$(read_pair "$manifest" runner.extension.root)" = "$extension_cgroup_root" ] || die "extension runner root manifest differs"
[ "$(read_pair "$manifest" runner.core.root)" = "$core_runner_cgroup_root" ] || die "Core runner root manifest differs"
validate_runner_uid DIREXTALK_CORE_EXTENSION_RUNNER_UID "$extension_runner_uid"
validate_runner_uid DIREXTALK_CORE_WORKLOAD_RUNNER_UID "$workload_runner_uid"
validate_cgroup_parent DIREXTALK_EXTENSION_CGROUP_PARENT "$extension_cgroup_parent"
validate_cgroup_parent DIREXTALK_CORE_RUNNER_CGROUP_PARENT "$core_runner_cgroup_parent"
validate_delegated_cgroup_root DIREXTALK_EXTENSION_CGROUP_ROOT "$extension_cgroup_root" "$stack_name" "$extension_cgroup_parent" "$extension_runner_uid"
validate_delegated_cgroup_root DIREXTALK_CORE_RUNNER_CGROUP_ROOT "$core_runner_cgroup_root" "$stack_name" "$core_runner_cgroup_parent" "$workload_runner_uid"

[ "$runner_host_prepared" = true ] || die "runner host was not prepared by prepare-runner-cgroups.sh"
verify_runner_host_binding extension "$extension_runner_unit" "$extension_runner_user" \
  "$extension_cgroup_parent" "$extension_runner_uid" "$extension_cgroup_root" \
  "$extension_control_group" "$extension_fragment_path" "$extension_fragment_hash" \
  "$extension_parent_root" "$extension_parent_procs" "$extension_parent_procs_owner" "$extension_parent_procs_mode"
verify_runner_host_binding core "$core_runner_unit" "$core_runner_user" \
  "$core_runner_cgroup_parent" "$workload_runner_uid" "$core_runner_cgroup_root" \
  "$core_control_group" "$core_fragment_path" "$core_fragment_hash" \
  "$core_parent_root" "$core_parent_procs" "$core_parent_procs_owner" "$core_parent_procs_mode"

command -v ss >/dev/null 2>&1 || die "ss is required for host-port ownership checks"
verify_local_docker_identity
require_free_host_ports() {
  local port listeners status ports=("$manifest_http_bind")
  [ "$message_tls_mode" = edge-terminated ] || ports+=("$manifest_https_bind")
  for port in "${ports[@]}"; do
    if listeners=$(ss -H -ltn "sport = :$port"); then
      [ -z "$listeners" ] || die "host port is already in use: $port"
    else
      status=$?
      die "host port inspection failed for $port (status $status)"
    fi
  done
}
require_fresh_stack() {
  local name status existing
  for name in "${networks[@]}"; do
    if docker network inspect "$name" >/dev/null 2>&1; then
      die "fresh stack network already exists: $name"
    else
      status=$?
      [ "$status" -eq 1 ] || die "Docker network inspection failed for $name (status $status)"
    fi
  done
  for name in "${volumes[@]}"; do
    if docker volume inspect "$name" >/dev/null 2>&1; then
      die "fresh stack volume already exists: $name"
    else
      status=$?
      [ "$status" -eq 1 ] || die "Docker volume inspection failed for $name (status $status)"
    fi
  done
  if existing=$(docker ps -aq --filter "label=com.docker.compose.project=$stack_name"); then
    [ -z "$existing" ] || die "fresh stack containers already exist for project $stack_name"
  else
    status=$?
    die "Docker container inspection failed (status $status)"
  fi
}

message_tls_mode=$(read_pair "$manifest" message_tls_mode)
case "$message_tls_mode" in
  local|external|edge-terminated) ;;
  *) die "manifest message TLS mode is invalid" ;;
esac
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_TLS_MODE)" = "$message_tls_mode" ] || die ".env message TLS mode differs from manifest"
compose=(docker compose --project-name "$stack_name" --env-file "$env_file" -f "$stack_dir/compose.yaml")
if [ "$message_tls_mode" != edge-terminated ]; then
  compose+=(-f "$stack_dir/compose.direct-tls.yaml")
fi
if [ "$compose_mode" = local ]; then
  compose+=(-f "$stack_dir/compose.local.yaml")
fi
verify_control_identity
"${compose[@]}" config --quiet
require_free_host_ports
require_fresh_stack

if [ "$compose_mode" = local ]; then
  verify_control_identity
  "$script_dir/build-local.sh" "$env_file" agent
  verify_control_identity
  "$script_dir/build-local.sh" "$env_file" message-server
else
  verify_control_identity
  "$script_dir/verify-production-tls.sh" "$env_file"
  "${compose[@]}" pull --policy always
  verify_control_identity
  "$script_dir/verify-production-images.sh" "$env_file" "$image_attestation"
fi

# Recheck immediately before creating resources so a build-time race cannot
# redirect startup into a same-name replacement stack or occupied host port.
verify_control_identity
verify_runner_host_binding extension "$extension_runner_unit" "$extension_runner_user" \
  "$extension_cgroup_parent" "$extension_runner_uid" "$extension_cgroup_root" \
  "$extension_control_group" "$extension_fragment_path" "$extension_fragment_hash" \
  "$extension_parent_root" "$extension_parent_procs" "$extension_parent_procs_owner" "$extension_parent_procs_mode"
verify_runner_host_binding core "$core_runner_unit" "$core_runner_user" \
  "$core_runner_cgroup_parent" "$workload_runner_uid" "$core_runner_cgroup_root" \
  "$core_control_group" "$core_fragment_path" "$core_fragment_hash" \
  "$core_parent_root" "$core_parent_procs" "$core_parent_procs_owner" "$core_parent_procs_mode"
verify_local_docker_identity
require_free_host_ports
require_fresh_stack

journal_receipt=$out/.cleanup-receipt
journal_identity=
write_start_journal() {
  local tmp env_sha256 manifest_sha256 extension_main_pid core_main_pid index
  [ ! -e "$journal_receipt" ] && [ ! -L "$journal_receipt" ] || die "cleanup journal already exists"
  env_sha256=$(sha256sum -- "$env_file" | awk '{print $1}')
  manifest_sha256=$(sha256sum -- "$manifest" | awk '{print $1}')
  extension_main_pid=$(runner_unit_property "$extension_runner_unit" MainPID)
  core_main_pid=$(runner_unit_property "$core_runner_unit" MainPID)
  tmp=$(mktemp "$out/.cleanup-receipt.XXXXXX") || die "cannot create cleanup journal"
  {
    printf '%s\n' '# dirextalk-split-cleanup-receipt-v1'
    printf 'stack_name=%s\nstate=starting\n' "$stack_name"
    printf 'control.env_identity=%s\ncontrol.manifest_identity=%s\n' "$env_identity" "$manifest_identity"
    printf 'control.env_sha256=%s\ncontrol.manifest_sha256=%s\n' "$env_sha256" "$manifest_sha256"
    printf 'host.machine_id=%s\ndocker.engine_id=%s\n' "$docker_machine_id" "$docker_engine_id"
    printf 'docker.context_endpoint=%s\ndocker.context_socket=%s\n' "$docker_context_endpoint" "$docker_context_canonical"
    printf 'container.count=0\nnetwork.count=0\nvolume.count=0\n'
    printf 'planned.network.count=%s\n' "${#networks[@]}"
    index=0
    for name in "${networks[@]}"; do printf 'planned.network.%s.name=%s\n' "$index" "$name"; index=$((index + 1)); done
    printf 'planned.volume.count=%s\n' "${#volumes[@]}"
    index=0
    for name in "${volumes[@]}"; do printf 'planned.volume.%s.name=%s\n' "$index" "$name"; index=$((index + 1)); done
    printf 'runner.extension.unit=%s\nrunner.extension.control_group=%s\nrunner.extension.main_pid=%s\nrunner.extension.fragment_path=%s\nrunner.extension.fragment_sha256=%s\n' \
      "$extension_runner_unit" "$extension_control_group" "$extension_main_pid" "$extension_fragment_path" "$extension_fragment_hash"
    printf 'runner.core.unit=%s\nrunner.core.control_group=%s\nrunner.core.main_pid=%s\nrunner.core.fragment_path=%s\nrunner.core.fragment_sha256=%s\n' \
      "$core_runner_unit" "$core_control_group" "$core_main_pid" "$core_fragment_path" "$core_fragment_hash"
  } >"$tmp" || { rm -f -- "$tmp"; die "cannot write cleanup journal"; }
  chmod 400 -- "$tmp" || { rm -f -- "$tmp"; die "cannot protect cleanup journal"; }
  if ! ln -- "$tmp" "$journal_receipt"; then
    rm -f -- "$tmp"
    die "cleanup journal was replaced before atomic creation"
  fi
  rm -f -- "$tmp"
  journal_identity=$(stat -c '%d:%i:%u' "$journal_receipt")
}

write_start_journal
startup_receipt_complete=false
capture_incomplete_receipt() {
  local receipt=$journal_receipt tmp ids id status data raw_name name service project actual_id planned_name
  local network_id network_name network_project volume_name volume_project fingerprint_json fingerprint
  local env_sha256 manifest_sha256 record index
  local -a container_records=() network_records=() volume_records=()
  command -v jq >/dev/null 2>&1 || return 1
  command -v sha256sum >/dev/null 2>&1 || return 1
  [ -f "$receipt" ] && [ ! -L "$receipt" ] || return 1
  [ "$(stat -c '%d:%i:%u' "$receipt")" = "$journal_identity" ] || return 1
  if ! ids=$(docker ps -aq --filter "label=com.docker.compose.project=$stack_name"); then
    return 1
  fi
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    printf '%s\n' "$id" | grep -Eq '^[0-9a-f]{64}$' || return 1
    if data=$(docker inspect "$id" 2>/dev/null); then
      :
    else
      status=$?
      [ "$status" -eq 1 ] || return 1
      continue
    fi
    actual_id=$(jq -r '.[0].Id // empty' <<<"$data")
    raw_name=$(jq -r '.[0].Name // empty' <<<"$data")
    service=$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")
    project=$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")
    [ "$actual_id" = "$id" ] || return 1
    case "$raw_name" in /*) name=${raw_name#/} ;; *) return 1 ;; esac
    [ "$project" = "$stack_name" ] || return 1
    printf '%s\n' "$name" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || return 1
    printf '%s\n' "$service" | grep -Eq '^[a-z0-9][a-z0-9_-]*$' || return 1
    container_records+=("$id|$name|$service|$project")
  done <<<"$ids"
  for planned_name in "${networks[@]}"; do
    if data=$(docker network inspect "$planned_name" 2>/dev/null); then
      :
    else
      status=$?
      [ "$status" -eq 1 ] || return 1
      continue
    fi
    network_id=$(jq -r '.[0].Id // empty' <<<"$data")
    network_name=$(jq -r '.[0].Name // empty' <<<"$data")
    network_project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$data")
    printf '%s\n' "$network_id" | grep -Eq '^[0-9a-f]{64}$' || return 1
    [ "$network_name" = "$planned_name" ] && [ "$network_project" = "$stack_name" ] || return 1
    network_records+=("$network_id|$network_name|$network_project")
  done
  for planned_name in "${volumes[@]}"; do
    if data=$(docker volume inspect "$planned_name" 2>/dev/null); then
      :
    else
      status=$?
      [ "$status" -eq 1 ] || return 1
      continue
    fi
    volume_name=$(jq -r '.[0].Name // empty' <<<"$data")
    volume_project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$data")
    if ! fingerprint_json=$(jq -c -e '
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
      return 1
    fi
    fingerprint=$(printf '%s' "$fingerprint_json" | sha256sum | awk '{print $1}')
    [ "$volume_name" = "$planned_name" ] && [ "$volume_project" = "$stack_name" ] || return 1
    printf '%s\n' "$fingerprint" | grep -Eq '^[0-9a-f]{64}$' || return 1
    volume_records+=("$volume_name|$volume_project|$fingerprint")
  done
  env_sha256=$(read_pair "$receipt" control.env_sha256)
  manifest_sha256=$(read_pair "$receipt" control.manifest_sha256)
  tmp=$(mktemp "$out/.cleanup-receipt.XXXXXX") || return 1
  if ! {
    printf '%s\n' '# dirextalk-split-cleanup-receipt-v1'
    printf 'stack_name=%s\nstate=incomplete\n' "$stack_name"
    printf 'control.env_identity=%s\ncontrol.manifest_identity=%s\n' "$(read_pair "$receipt" control.env_identity)" "$(read_pair "$receipt" control.manifest_identity)"
    printf 'control.env_sha256=%s\ncontrol.manifest_sha256=%s\n' "$env_sha256" "$manifest_sha256"
    printf 'host.machine_id=%s\ndocker.engine_id=%s\n' "$(read_pair "$receipt" host.machine_id)" "$(read_pair "$receipt" docker.engine_id)"
    printf 'docker.context_endpoint=%s\ndocker.context_socket=%s\n' "$(read_pair "$receipt" docker.context_endpoint)" "$(read_pair "$receipt" docker.context_socket)"
    printf 'container.count=%s\n' "${#container_records[@]}"
    index=0
    for record in "${container_records[@]}"; do IFS='|' read -r id name service project <<<"$record"; printf 'container.%s.id=%s\ncontainer.%s.name=%s\ncontainer.%s.service=%s\ncontainer.%s.project=%s\n' "$index" "$id" "$index" "$name" "$index" "$service" "$index" "$project"; index=$((index + 1)); done
    printf 'network.count=%s\n' "${#network_records[@]}"
    index=0
    for record in "${network_records[@]}"; do IFS='|' read -r network_id network_name network_project <<<"$record"; printf 'network.%s.id=%s\nnetwork.%s.name=%s\nnetwork.%s.project=%s\n' "$index" "$network_id" "$index" "$network_name" "$index" "$network_project"; index=$((index + 1)); done
    printf 'volume.count=%s\n' "${#volume_records[@]}"
    index=0
    for record in "${volume_records[@]}"; do IFS='|' read -r volume_name volume_project fingerprint <<<"$record"; printf 'volume.%s.name=%s\nvolume.%s.project=%s\nvolume.%s.fingerprint_sha256=%s\n' "$index" "$volume_name" "$index" "$volume_project" "$index" "$fingerprint"; index=$((index + 1)); done
    for role in extension core; do
      printf 'runner.%s.unit=%s\nrunner.%s.control_group=%s\nrunner.%s.main_pid=%s\nrunner.%s.fragment_path=%s\nrunner.%s.fragment_sha256=%s\n' "$role" "$(read_pair "$receipt" runner.$role.unit)" "$role" "$(read_pair "$receipt" runner.$role.control_group)" "$role" "$(read_pair "$receipt" runner.$role.main_pid)" "$role" "$(read_pair "$receipt" runner.$role.fragment_path)" "$role" "$(read_pair "$receipt" runner.$role.fragment_sha256)"
    done
  } >"$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  chmod 400 -- "$tmp" || { rm -f -- "$tmp"; return 1; }
  [ "$(stat -c '%d:%i:%u' "$receipt")" = "$journal_identity" ] || { rm -f -- "$tmp"; return 1; }
  mv -- "$tmp" "$receipt" || { rm -f -- "$tmp"; return 1; }
  journal_identity=$(stat -c '%d:%i:%u' "$receipt")
}
on_start_failure() {
  local status=$?
  trap - EXIT
  if [ "$startup_receipt_complete" != true ]; then
    if ! capture_incomplete_receipt; then
      printf 'split-stack start: partial startup identity capture was uncertain; preserving %s\n' "$journal_receipt" >&2
    fi
  fi
  exit "$status"
}
trap on_start_failure EXIT
if [ "$compose_mode" = production ]; then
  "${compose[@]}" up -d --no-build --pull never --wait message-server
else
  "${compose[@]}" up -d --no-build --wait message-server
fi

verify_control_identity
for service in agent extension-runner core-runner message-server; do
  container=$(docker ps -q \
    --filter "label=com.docker.compose.project=$stack_name" \
    --filter "label=com.docker.compose.service=$service")
  [ -n "$container" ] && [ "${container#*$'\n'}" = "$container" ] || die "$service does not have exactly one running container"
  state=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}|{{ index .Config.Labels "com.docker.compose.service" }}|{{ .State.Health.Status }}' "$container")
  [ "$state" = "$stack_name|$service|healthy" ] || die "$service health ownership check failed: $state"
done

# Persist the exact objects created by this fresh start.  Cleanup consumes this
# receipt instead of resolving mutable Compose names again.  Docker volumes do
# not expose a stable object ID through the local-volume API, so their strongest
# available generation binding is a digest of the complete inspect metadata.
write_cleanup_receipt() {
  local receipt_state=${1:-complete}
  local receipt=$out/.cleanup-receipt tmp container_ids container_id inspect_line
  local raw_name container_name container_service container_project
  local network_data network_id network_name network_project
  local volume_data volume_name volume_project fingerprint_json fingerprint status
  local extension_main_pid core_main_pid env_sha256 manifest_sha256 record index
  local -a container_records=()
  local -a network_records=()
  local -a volume_records=()

  case "$receipt_state" in complete|incomplete) ;; *) die "cleanup receipt state is invalid" ;; esac
  command -v jq >/dev/null 2>&1 || die "jq is required to persist cleanup object identities"
  command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required to persist cleanup volume identities"
  [ -f "$receipt" ] && [ ! -L "$receipt" ] || die "cleanup journal is missing before receipt finalization"
  [ "$(stat -c '%d:%i:%u' "$receipt")" = "$journal_identity" ] || die "cleanup journal identity changed before receipt finalization"

  if container_ids=$(docker ps -aq --filter "label=com.docker.compose.project=$stack_name"); then
    :
  else
    die "container identity inspection failed after startup"
  fi
  if [ "$receipt_state" = complete ]; then
    [ -n "$container_ids" ] || die "no Compose containers were created after startup"
  fi
  while IFS= read -r container_id; do
    [ -n "$container_id" ] || continue
    printf '%s\n' "$container_id" | grep -Eq '^[0-9a-f]{64}$' || die "Compose container ID is not a full immutable ID"
    if inspect_line=$(docker inspect -f '{{.Id}}|{{.Name}}|{{ index .Config.Labels "com.docker.compose.service" }}|{{ index .Config.Labels "com.docker.compose.project" }}' "$container_id" 2>/dev/null); then
      :
    else
      status=$?
      [ "$receipt_state" = incomplete ] && [ "$status" -eq 1 ] && continue
      die "Compose container identity inspection failed (status $status)"
    fi
    IFS='|' read -r container_id_check raw_name container_service container_project <<<"$inspect_line"
    [ "$container_id_check" = "$container_id" ] || die "Compose container identity changed during receipt capture"
    case "$raw_name" in
      /*) container_name=${raw_name#/} ;;
      *) die "Compose container name is not canonical" ;;
    esac
    printf '%s\n' "$container_name" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || die "Compose container name is invalid"
    printf '%s\n' "$container_service" | grep -Eq '^[a-z0-9][a-z0-9_-]*$' || die "Compose container service label is invalid"
    [ "$container_project" = "$stack_name" ] || die "Compose container project label changed during receipt capture"
    container_records+=("$container_id|$container_name|$container_service|$container_project")
  done <<<"$container_ids"
  if [ "$receipt_state" = complete ]; then
    [ "${#container_records[@]}" -gt 0 ] || die "no Compose container identities were captured"
  fi

  for network_name in "${networks[@]}"; do
    if network_data=$(docker network inspect "$network_name" 2>/dev/null); then
      :
    else
      status=$?
      [ "$status" -eq 1 ] || die "Compose network identity inspection failed (status $status): $network_name"
      network_data=
    fi
    if [ -z "$network_data" ]; then
      [ "$receipt_state" = incomplete ] && continue
      die "Compose network disappeared during receipt capture: $network_name"
    fi
    network_id=$(jq -r '.[0].Id // empty' <<<"$network_data")
    network_name=$(jq -r '.[0].Name // empty' <<<"$network_data")
    network_project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$network_data")
    printf '%s\n' "$network_id" | grep -Eq '^[0-9a-f]{64}$' || die "Compose network ID is not a full immutable ID"
    printf '%s\n' "$network_name" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || die "Compose network name is invalid"
    [ "$network_project" = "$stack_name" ] || die "Compose network is not owned by this project: $network_name"
    network_records+=("$network_id|$network_name|$network_project")
  done

  for volume_name in "${volumes[@]}"; do
    if volume_data=$(docker volume inspect "$volume_name" 2>/dev/null); then
      :
    else
      status=$?
      [ "$status" -eq 1 ] || die "Compose volume identity inspection failed (status $status): $volume_name"
      volume_data=
    fi
    if [ -z "$volume_data" ]; then
      [ "$receipt_state" = incomplete ] && continue
      die "Compose volume disappeared during receipt capture: $volume_name"
    fi
    volume_name=$(jq -r '.[0].Name // empty' <<<"$volume_data")
    volume_project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$volume_data")
    if ! fingerprint_json=$(jq -c -e '
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
    ' <<<"$volume_data"); then
      die "Compose volume identity metadata is incomplete: $volume_name"
    fi
    fingerprint=$(printf '%s' "$fingerprint_json" | sha256sum | awk '{print $1}')
    [ -n "$volume_name" ] || die "Compose volume name is unavailable"
    [ "$volume_project" = "$stack_name" ] || die "Compose volume is not owned by this project: $volume_name"
    printf '%s\n' "$fingerprint" | grep -Eq '^[0-9a-f]{64}$' || die "Compose volume fingerprint is invalid: $volume_name"
    volume_records+=("$volume_name|$volume_project|$fingerprint")
  done

  extension_main_pid=$(runner_unit_property "$extension_runner_unit" MainPID)
  core_main_pid=$(runner_unit_property "$core_runner_unit" MainPID)
  printf '%s\n' "$extension_main_pid" | grep -Eq '^[1-9][0-9]*$' || die "extension runner MainPID is invalid during receipt capture"
  printf '%s\n' "$core_main_pid" | grep -Eq '^[1-9][0-9]*$' || die "Core runner MainPID is invalid during receipt capture"
  env_sha256=$(sha256sum -- "$env_file" | awk '{print $1}')
  manifest_sha256=$(sha256sum -- "$manifest" | awk '{print $1}')

  tmp=$(mktemp "$out/.cleanup-receipt.XXXXXX") || die "cannot create cleanup receipt"
  if ! {
    printf '%s\n' '# dirextalk-split-cleanup-receipt-v1'
    printf 'stack_name=%s\n' "$stack_name"
    printf 'state=%s\n' "$receipt_state"
    printf 'control.env_identity=%s\n' "$env_identity"
    printf 'control.manifest_identity=%s\n' "$manifest_identity"
    printf 'control.env_sha256=%s\n' "$env_sha256"
    printf 'control.manifest_sha256=%s\n' "$manifest_sha256"
    printf 'host.machine_id=%s\n' "$docker_machine_id"
    printf 'docker.engine_id=%s\n' "$docker_engine_id"
    printf 'docker.context_endpoint=%s\n' "$docker_context_endpoint"
    printf 'docker.context_socket=%s\n' "$docker_context_canonical"
    printf 'container.count=%s\n' "${#container_records[@]}"
    index=0
    for record in "${container_records[@]}"; do
      IFS='|' read -r container_id container_name container_service container_project <<<"$record"
      printf 'container.%s.id=%s\n' "$index" "$container_id"
      printf 'container.%s.name=%s\n' "$index" "$container_name"
      printf 'container.%s.service=%s\n' "$index" "$container_service"
      printf 'container.%s.project=%s\n' "$index" "$container_project"
      index=$((index + 1))
    done
    printf 'network.count=%s\n' "${#network_records[@]}"
    index=0
    for record in "${network_records[@]}"; do
      IFS='|' read -r network_id network_name network_project <<<"$record"
      printf 'network.%s.id=%s\n' "$index" "$network_id"
      printf 'network.%s.name=%s\n' "$index" "$network_name"
      printf 'network.%s.project=%s\n' "$index" "$network_project"
      index=$((index + 1))
    done
    printf 'volume.count=%s\n' "${#volume_records[@]}"
    index=0
    for record in "${volume_records[@]}"; do
      IFS='|' read -r volume_name volume_project fingerprint <<<"$record"
      printf 'volume.%s.name=%s\n' "$index" "$volume_name"
      printf 'volume.%s.project=%s\n' "$index" "$volume_project"
      printf 'volume.%s.fingerprint_sha256=%s\n' "$index" "$fingerprint"
      index=$((index + 1))
    done
    printf 'runner.extension.unit=%s\n' "$extension_runner_unit"
    printf 'runner.extension.control_group=%s\n' "$extension_control_group"
    printf 'runner.extension.main_pid=%s\n' "$extension_main_pid"
    printf 'runner.extension.fragment_path=%s\n' "$extension_fragment_path"
    printf 'runner.extension.fragment_sha256=%s\n' "$extension_fragment_hash"
    printf 'runner.core.unit=%s\n' "$core_runner_unit"
    printf 'runner.core.control_group=%s\n' "$core_control_group"
    printf 'runner.core.main_pid=%s\n' "$core_main_pid"
    printf 'runner.core.fragment_path=%s\n' "$core_fragment_path"
    printf 'runner.core.fragment_sha256=%s\n' "$core_fragment_hash"
  } >"$tmp"; then
    rm -f -- "$tmp"
    die "cannot write cleanup receipt"
  fi
  chmod 400 -- "$tmp" || { rm -f -- "$tmp"; die "cannot protect cleanup receipt"; }
  if ! mv -- "$tmp" "$receipt"; then
    rm -f -- "$tmp"
    die "cleanup receipt could not be atomically finalized"
  fi
  [ -f "$receipt" ] && [ ! -L "$receipt" ] || die "cleanup receipt is not a regular file"
  [ "$(stat -c '%a' "$receipt")" = 400 ] || die "cleanup receipt mode changed during creation"
}

verify_control_identity
verify_local_docker_identity
write_cleanup_receipt complete
startup_receipt_complete=true
trap - EXIT

printf 'fresh split stack is healthy: %s\n' "$stack_name"
