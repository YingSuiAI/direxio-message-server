#!/usr/bin/env bash
set -euo pipefail

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
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "missing regular .env"
[ -f "$manifest" ] && [ ! -L "$manifest" ] || die "missing regular .manifest"
[ "$(stat -c '%a' "$env_file")" = 400 ] || die ".env must be mode 0400"
[ "$(stat -c '%a' "$manifest")" = 400 ] || die ".manifest must be mode 0400"

read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

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
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "manifest stack identity is not a 128-bit generated namespace"
[ "$stack_nonce" = "${stack_name#d-}" ] || die "manifest stack nonce does not bind to stack identity"
printf '%s\n' "$manifest_agent_id" | grep -Eq '^[0-9a-f-]{36}$' || die "manifest Agent instance ID is invalid"
printf '%s\n' "$manifest_message_id" | grep -Eq '^[0-9a-f-]{36}$' || die "manifest message-server instance ID is invalid"
[ "$manifest_agent_id" != "$manifest_message_id" ] || die "manifest instance identities must differ"
printf '%s\n' "$manifest_generation" | grep -Eq '^[1-9][0-9]*$' || die "manifest account generation is invalid"
for host_port in "$manifest_http_bind" "$manifest_https_bind"; do
  printf '%s\n' "$host_port" | grep -Eq '^[1-9][0-9]{3,4}$' || die "manifest host port is invalid"
  [ "$host_port" -ge 1024 ] && [ "$host_port" -le 65535 ] || die "manifest host port is outside the unprivileged range"
done
[ "$manifest_http_bind" != "$manifest_https_bind" ] || die "manifest host ports must differ"
[ "$manifest_client_base_url" = "http://localhost:$manifest_http_bind" ] || die "manifest client URL is not derived from its HTTP host port"
[ "$manifest_master_key_path" = "$out/core-secret-master-key" ] || die "manifest Agent master-key path is not bound to the output directory"
[ -f "$manifest_master_key_path" ] && [ ! -L "$manifest_master_key_path" ] || die "Agent master-key file is missing or symlinked"
[ "$(stat -c '%a' "$manifest_master_key_path")" = 400 ] || die "Agent master-key file must be mode 0400"
[ "$(stat -c '%s' "$manifest_master_key_path")" = 32 ] || die "Agent master-key file must contain exactly 32 raw bytes"
[ "$(stat -c '%d' "$manifest_master_key_path")" = "$manifest_master_key_device" ] || die "Agent master-key device changed after provisioning"
[ "$(stat -c '%i' "$manifest_master_key_path")" = "$manifest_master_key_inode" ] || die "Agent master-key inode changed after provisioning"
[ "$(stat -c '%u' "$manifest_master_key_path")" = "$manifest_master_key_uid" ] || die "Agent master-key owner changed after provisioning"
[ "$manifest_master_key_uid" = "$(id -u)" ] || die "Agent master-key is not owned by the cleanup user"

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

resource_names=()
verify_pair() {
  local kind=$1 pair=$2 env_key manifest_key expected actual
  env_key=${pair%%:*}
  manifest_key=${pair#*:}
  expected=$(read_pair "$manifest" "$manifest_key")
  actual=$(read_pair "$env_file" "$env_key")
  [ "$actual" = "$expected" ] || die "$env_key was edited outside the manifest target"
  [ "$actual" = "$stack_name-${expected#"$stack_name-"}" ] || die "$env_key is not derived from the manifest stack identity"
  resource_names+=("$kind:$actual")
}
for pair in "${network_pairs[@]}"; do
  verify_pair network "$pair"
done
for pair in "${volume_pairs[@]}"; do
  verify_pair volume "$pair"
done

command -v docker >/dev/null 2>&1 || die "docker is required for exact-target cleanup"

inspect_resource() {
  local kind=$1 name=$2 data actual project
  if ! data=$(docker "$kind" inspect "$name" 2>/dev/null); then
    return 0
  fi
  actual=$(jq -r '.[0].Name // empty' <<<"$data")
  [ "$actual" = "$name" ] || die "$kind inspect name mismatch for $name"
  project=$(jq -r '.[0].Labels["com.docker.compose.project"] // empty' <<<"$data")
  [ "$project" = "$stack_name" ] || die "$kind $name is not owned by Compose project $stack_name"
}
for item in "${resource_names[@]}"; do
  inspect_resource "${item%%:*}" "${item#*:}"
done

containers=$(docker ps -aq --filter "label=com.docker.compose.project=$stack_name") || die "container ownership inspection failed"
for container in $containers; do
  container_project=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$container" 2>/dev/null || true)
  [ "$container_project" = "$stack_name" ] || die "container $container has a mismatched Compose project label"
done

compose_file=$(cd "$(dirname "$0")/.." && pwd -P)/compose.yaml
[ -f "$compose_file" ] || die "split compose file is missing"
compose_args=(--project-name "$stack_name" --env-file "$env_file" -f "$compose_file" down --remove-orphans)
if [ "$purge" = true ]; then
  compose_args+=(--volumes)
fi
printf 'cleaning exact Compose project %s (purge=%s)\n' "$stack_name" "$purge"
docker compose "${compose_args[@]}"
printf 'split-stack cleanup complete; generated files remain at %s for audit\n' "$out"
