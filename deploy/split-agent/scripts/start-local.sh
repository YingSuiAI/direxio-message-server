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
verify_control_identity() {
  [ -f "$env_file" ] && [ ! -L "$env_file" ] || die ".env was replaced during startup"
  [ -f "$manifest" ] && [ ! -L "$manifest" ] || die ".manifest was replaced during startup"
  [ "$(stat -c '%d:%i:%u' "$env_file")" = "$env_identity" ] || die ".env identity changed during startup"
  [ "$(stat -c '%d:%i:%u' "$manifest")" = "$manifest_identity" ] || die ".manifest identity changed during startup"
  [ "$(stat -c '%a' "$env_file")" = 400 ] || die ".env permissions changed during startup"
  [ "$(stat -c '%a' "$manifest")" = 400 ] || die ".manifest permissions changed during startup"
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
manifest_agent_id=$(read_pair "$manifest" agent_instance_id)
manifest_message_id=$(read_pair "$manifest" message_instance_id)
manifest_generation=$(read_pair "$manifest" account_generation)
manifest_http_bind=$(read_pair "$manifest" message_http_bind)
manifest_https_bind=$(read_pair "$manifest" message_https_bind)
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "manifest stack identity is invalid"
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
[ "$(read_pair "$env_file" DIREXTALK_AGENT_INSTANCE_ID)" = "$manifest_agent_id" ] || die ".env Agent identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_INSTANCE_ID)" = "$manifest_message_id" ] || die ".env message-server identity differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_ACCOUNT_GENERATION)" = "$manifest_generation" ] || die ".env account generation differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTP_BIND)" = "$manifest_http_bind" ] || die ".env HTTP bind differs from manifest"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTPS_BIND)" = "$manifest_https_bind" ] || die ".env HTTPS bind differs from manifest"

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
message_root=$(cd "$script_dir/../../.." && pwd -P)
agent_root=$(cd "$message_root/../dirextalk-agent" && pwd -P)
[ "$(read_pair "$env_file" DIREXTALK_AGENT_BUILD_CONTEXT)" = "$agent_root" ] || die ".env Agent build context is not the sibling Agent repository"
[ "$(read_pair "$env_file" DIREXTALK_MESSAGE_BUILD_CONTEXT)" = "$message_root" ] || die ".env message build context is not this repository"

master_key=$(read_pair "$manifest" core_secret_master_key_path)
[ "$master_key" = "$out/core-secret-master-key" ] || die "manifest master-key path is outside the environment directory"
[ -f "$master_key" ] && [ ! -L "$master_key" ] || die "Agent master-key file is missing or symlinked"
[ "$(stat -c '%a' "$master_key")" = 400 ] || die "Agent master-key file must be mode 0400"
[ "$(stat -c '%s' "$master_key")" = 32 ] || die "Agent master-key file must contain 32 raw bytes"
[ "$(stat -c '%d' "$master_key")" = "$(read_pair "$manifest" core_secret_master_key_device)" ] || die "Agent master-key device changed"
[ "$(stat -c '%i' "$master_key")" = "$(read_pair "$manifest" core_secret_master_key_inode)" ] || die "Agent master-key inode changed"
[ "$(stat -c '%u' "$master_key")" = "$(read_pair "$manifest" core_secret_master_key_uid)" ] || die "Agent master-key owner changed"
[ "$(stat -c '%u' "$master_key")" = "$current_uid" ] || die "Agent master-key is not owned by the startup user"

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

command -v docker >/dev/null 2>&1 || die "docker is required"
command -v ss >/dev/null 2>&1 || die "ss is required for host-port ownership checks"
require_free_host_ports() {
  local port listeners status
  for port in "$manifest_http_bind" "$manifest_https_bind"; do
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

compose=(docker compose --project-name "$stack_name" --env-file "$env_file" -f "$stack_dir/compose.yaml" -f "$stack_dir/compose.local.yaml")
verify_control_identity
"${compose[@]}" config --quiet
require_free_host_ports
require_fresh_stack

verify_control_identity
"$script_dir/build-local.sh" "$env_file" agent
verify_control_identity
"$script_dir/build-local.sh" "$env_file" message-server

# Recheck immediately before creating resources so a build-time race cannot
# redirect startup into a same-name replacement stack or occupied host port.
verify_control_identity
require_free_host_ports
require_fresh_stack
"${compose[@]}" up -d --no-build --wait message-server

verify_control_identity
for service in agent message-server; do
  container=$(docker ps -q \
    --filter "label=com.docker.compose.project=$stack_name" \
    --filter "label=com.docker.compose.service=$service")
  [ -n "$container" ] && [ "${container#*$'\n'}" = "$container" ] || die "$service does not have exactly one running container"
  state=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}|{{ index .Config.Labels "com.docker.compose.service" }}|{{ .State.Health.Status }}' "$container")
  [ "$state" = "$stack_name|$service|healthy" ] || die "$service health ownership check failed: $state"
done

printf 'fresh split stack is healthy: %s\n' "$stack_name"
