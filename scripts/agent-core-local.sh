#!/bin/sh
# Start the Agent Core and Message Server as two isolated Compose projects.
# The script intentionally has no chat/model probe: this lane proves topology,
# authenticated Core readiness, and Message Server health only.
set -eu

usage() {
  echo "usage: $0 up|down|verify|rotate [RUN_DIR]" >&2
  exit 2
}

action=${1:-}
case "$action" in up|down|verify|rotate) ;; *) usage ;; esac

script_dir=$(CDPATH= cd -- "$(dirname "$0")" && pwd -P)
server_root=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
agent_root=${DIREXTALK_AGENT_ROOT:-$(CDPATH= cd -- "$server_root/../agent" && pwd -P)}
run_dir=${2:-${DIREXTALK_AGENT_CORE_RUN_DIR:-$server_root/.run.agent-core-local}}
run_dir=$(mkdir -p "$run_dir" && CDPATH= cd -- "$run_dir" && pwd -P)
stack_id_file=$run_dir/.stack-id
if [ -n "${STACK_ID:-}" ]; then
  stack_id=$STACK_ID
  case "$stack_id" in
    [a-z0-9][a-z0-9_-]*) ;;
    *) echo "STACK_ID must start with lowercase alphanumeric and contain only [a-z0-9_-]" >&2; exit 2 ;;
  esac
  [ "${#stack_id}" -le 24 ] || { echo "STACK_ID must be at most 24 characters" >&2; exit 2; }
elif [ -r "$stack_id_file" ]; then
  stack_id=$(tr -d '\r\n' < "$stack_id_file")
else
  stack_id=$(openssl rand -hex 12)
  printf '%s\n' "$stack_id" > "$stack_id_file"
  chmod 0400 "$stack_id_file"
fi
[ -n "$stack_id" ] || { echo "empty stack identity" >&2; exit 2; }

agent_compose=$agent_root/deploy/container/compose.local.yaml
agent_bootstrap=$agent_root/deploy/container/scripts/bootstrap-local.sh
server_compose=$server_root/docker-compose.agent-core-local.yml
agent_project=${DIREXTALK_AGENT_CORE_PROJECT:-dirextalk-agent-core-$stack_id}
server_project=${DIREXTALK_MESSAGE_SERVER_PROJECT:-dirextalk-message-server-$stack_id}
agent_image=${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-dirextalk-agent-core:local}
runner_image=${DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE:-dirextalk-extension-runner:local}
postgres_image=${DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:-docker.io/library/postgres:18-alpine}
tls_server_name=${P2P_AGENT_CORE_SERVER_NAME:-core.local}

agent_env=$run_dir/agent-compose.env
server_env=$run_dir/server.env

agent_network="dirextalk-agent-private-$stack_id"
agent_egress_network="dirextalk-agent-egress-$stack_id"
caller_network="dirextalk-agent-caller-$stack_id"
server_network="dirextalk-message-server-private-$stack_id"
public_network="dirextalk-message-server-public-$stack_id"

agent_volume_postgres="dirextalk-agent-postgres-$stack_id"
agent_volume_socket="dirextalk-agent-socket-$stack_id"
agent_volume_staging="dirextalk-agent-staging-$stack_id"
agent_volume_workspaces="dirextalk-agent-workspaces-$stack_id"
agent_volume_install="dirextalk-agent-install-$stack_id"
agent_volume_state="dirextalk-agent-state-$stack_id"
agent_volume_secret="dirextalk-agent-secret-$stack_id"
agent_volume_pgsecret="dirextalk-agent-pgsecret-$stack_id"
agent_volume_config="dirextalk-agent-config-$stack_id"
server_volume_postgres="dirextalk-message-postgres-$stack_id"
server_volume_config="dirextalk-message-config-$stack_id"
server_volume_data="dirextalk-message-data-$stack_id"
server_volume_material="dirextalk-message-agent-core-material-$stack_id"

compose() {
  project=$1
  env_file=$2
  file=$3
  shift 3
  docker compose -p "$project" --env-file "$env_file" -f "$file" "$@"
}

set_agent_names() {
  base_env=$run_dir/agent/.env
  [ -f "$base_env" ] || return 1
  [ -e "$agent_env" ] && chmod u+w "$agent_env" || true
  cp "$base_env" "$agent_env"
  chmod u+w "$agent_env"
  cat >> "$agent_env" <<EOF
DIREXTALK_AGENT_NETWORK_NAME=$agent_network
DIREXTALK_AGENT_EGRESS_NETWORK_NAME=$agent_egress_network
DIREXTALK_AGENT_CALLER_NETWORK_NAME=$caller_network
DIREXTALK_AGENT_POSTGRES_VOLUME=$agent_volume_postgres
DIREXTALK_AGENT_SOCKET_VOLUME=$agent_volume_socket
DIREXTALK_AGENT_STAGING_VOLUME=$agent_volume_staging
DIREXTALK_AGENT_WORKSPACE_VOLUME=$agent_volume_workspaces
DIREXTALK_AGENT_INSTALL_VOLUME=$agent_volume_install
DIREXTALK_AGENT_RUNNER_STATE_VOLUME=$agent_volume_state
DIREXTALK_AGENT_SECRET_VOLUME=$agent_volume_secret
DIREXTALK_AGENT_POSTGRES_SECRET_VOLUME=$agent_volume_pgsecret
DIREXTALK_AGENT_CONFIG_VOLUME=$agent_volume_config
EOF
  chmod 0400 "$agent_env"
}

assert_project_ownership() {
  project=$1
  env_file=$2
  file=$3
  if [ "${TEST_OWNERSHIP_GUARD_FAIL:-0}" = 1 ]; then
    echo "ownership guard test failure" >&2
    return 1
  fi
  containers=$(compose "$project" "$env_file" "$file" ps -aq)
  for id in $containers; do
    owner=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$id")
    [ "$owner" = "$project" ] || { echo "refusing cleanup: container $id belongs to $owner" >&2; return 1; }
  done
  config=$(compose "$project" "$env_file" "$file" config --format json)
  printf '%s' "$config" | jq -r '.networks // {} | to_entries[] | select(.value.external != true) | .value.name' | while IFS= read -r name; do
    [ -z "$name" ] && continue
    owner=$(docker network inspect --format '{{index .Labels "com.docker.compose.project"}}' "$name" 2>/dev/null || true)
    [ "$owner" = "$project" ] || { echo "refusing cleanup: network $name belongs to ${owner:-unknown}" >&2; exit 1; }
  done
  printf '%s' "$config" | jq -r '.volumes // {} | to_entries[] | select(.value.external != true) | .value.name' | while IFS= read -r name; do
    [ -z "$name" ] && continue
    owner=$(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$name" 2>/dev/null || true)
    [ "$owner" = "$project" ] || { echo "refusing cleanup: volume $name belongs to ${owner:-unknown}" >&2; exit 1; }
  done
}

safe_down() {
  project=$1; env_file=$2; file=$3
  [ -f "$env_file" ] || return 0
  assert_project_ownership "$project" "$env_file" "$file" || return 1
  compose "$project" "$env_file" "$file" down --volumes --remove-orphans
}

refresh_after_rotation() {
  # Equivalent to the Agent rotate-local contract, but with the explicit
  # per-run project so a rotation cannot recreate another run's resources.
  compose "$agent_project" "$agent_env" "$agent_compose" up --no-deps --force-recreate --abort-on-container-exit --exit-code-from secret-init secret-init
  compose "$agent_project" "$agent_env" "$agent_compose" up -d --force-recreate core
  wait_agent_ready
  if [ "${ROTATION_INJECT_FAILURE_ONCE:-0}" = 1 ] && [ ! -e "$rotation_root/injected" ]; then
    : > "$rotation_root/injected"
    echo "injected rotation failure after Core readiness" >&2
    return 1
  fi
  compose "$server_project" "$server_env" "$server_compose" up --no-deps --force-recreate --abort-on-container-exit --exit-code-from agent-core-secret-init agent-core-secret-init
  compose "$server_project" "$server_env" "$server_compose" up -d --force-recreate message-server
  wait_healthy "$server_project" "$server_env" "$server_compose" message-server
  probe_server_public
}

rotation_root=$run_dir/rotations
rotation_current=$rotation_root/current

set_rotation_paths() {
  chmod u+w "$agent_env" 2>/dev/null || true
  cat >> "$agent_env" <<EOF
DIREXTALK_SERVICE_TOKEN_FILE=$rotation_current/service-token
DIREXTALK_TLS_CERT_FILE=$rotation_current/tls-cert
DIREXTALK_TLS_KEY_FILE=$rotation_current/tls-key
EOF
  chmod 0400 "$agent_env"
  chmod u+w "$server_env" 2>/dev/null || true
  sed -i "s|^DIREXTALK_AGENT_SERVICE_TOKEN_FILE=.*|DIREXTALK_AGENT_SERVICE_TOKEN_FILE=$rotation_current/service-token|; s|^DIREXTALK_AGENT_TLS_CERT_FILE=.*|DIREXTALK_AGENT_TLS_CERT_FILE=$rotation_current/ca-cert|" "$server_env"
  chmod 0400 "$server_env"
}

validate_generation() {
  generation_path=$1
  for artifact in service-token tls-cert tls-key ca-cert; do
    [ -f "$generation_path/$artifact" ] || { echo "incomplete rotation generation" >&2; return 1; }
  done
  [ "$(wc -c < "$generation_path/service-token")" -eq 43 ] || { echo "invalid rotation token length" >&2; return 1; }
  openssl x509 -in "$generation_path/tls-cert" -noout -checkend 60 >/dev/null 2>&1 || return 1
  openssl x509 -in "$generation_path/ca-cert" -noout -checkend 60 >/dev/null 2>&1 || return 1
  cmp "$generation_path/tls-cert" "$generation_path/ca-cert" >/dev/null || { echo "rotation CA/cert mismatch" >&2; return 1; }
  chmod 0400 "$generation_path/service-token" "$generation_path/tls-cert" "$generation_path/tls-key" "$generation_path/ca-cert"
}

swap_rotation_pointer() {
  target=$1
  [ -d "$rotation_root/$target" ] || return 1
  rm -f "$rotation_root/current.next"
  ln -s "$target" "$rotation_root/current.next"
  mv -Tf "$rotation_root/current.next" "$rotation_current"
}

ensure_initial_generation() {
  mkdir -p "$rotation_root"
  if [ -L "$rotation_current" ]; then
    set_rotation_paths
    return
  fi
  generation="generation-$(date +%s)-$(openssl rand -hex 4)"
  mkdir "$rotation_root/$generation"
  cp "$run_dir/agent/service-token" "$rotation_root/$generation/service-token"
  cp "$run_dir/agent/tls-cert" "$rotation_root/$generation/tls-cert"
  cp "$run_dir/agent/tls-key" "$rotation_root/$generation/tls-key"
  cp "$run_dir/agent/tls-cert" "$rotation_root/$generation/ca-cert"
  validate_generation "$rotation_root/$generation"
  swap_rotation_pointer "$generation"
  set_rotation_paths
}

rotate_stack() {
  [ -r "$agent_env" ] && [ -r "$server_env" ] || { echo "rotation requires an existing stack run directory" >&2; return 1; }
  ensure_initial_generation
  generation="generation-$(date +%s)-$(openssl rand -hex 4)"
  stage=$rotation_root/$generation
  mkdir "$stage"
  token=""
  while [ "$(printf '%s' "$token" | wc -c)" -ne 43 ]; do
    token=$(openssl rand -base64 32 | tr -d '=+/\n' | cut -c1-43)
  done
  printf '%s' "$token" > "$stage/service-token"
  chmod 0400 "$stage/service-token"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$stage/tls-key" -out "$stage/tls-cert" -days 365 \
    -subj "/CN=$tls_server_name" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "subjectAltName=DNS:$tls_server_name,IP:127.0.0.1" >/dev/null 2>&1
  cp "$stage/tls-cert" "$stage/ca-cert"
  validate_generation "$stage"
  previous=$(readlink "$rotation_current")
  # Promotion is the only authority change. Keep the old pointer until all
  # downstream secret-init/recreate/readiness work succeeds.
  rotation_active=1
  rotation_restore() {
    [ "$rotation_active" = 1 ] || return 0
    swap_rotation_pointer "$previous" || true
    set_rotation_paths || true
    refresh_after_rotation || true
    rotation_active=0
  }
  trap 'exit_status=$?; if [ "$rotation_active" = 1 ]; then rotation_restore; fi; exit "$exit_status"' EXIT INT TERM
  swap_rotation_pointer "$generation"
  set_rotation_paths
  if refresh_after_rotation; then
    rotation_active=0
    trap - EXIT INT TERM
    echo "coordinated Agent Core/Message Server rotation passed" >&2
    return 0
  fi
  echo "rotation failed; rollback trap restoring prior generation" >&2
  return 1
}

wait_healthy() {
  project=$1
  env_file=$2
  file=$3
  service=$4
  deadline=$(( $(date +%s) + ${HEALTH_TIMEOUT_SECONDS:-180} ))
  container=$(compose "$project" "$env_file" "$file" ps -q "$service")
  [ -n "$container" ] || { echo "missing container for $service" >&2; return 1; }
  while :; do
    status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container")
    case "$status" in
      healthy) return 0 ;;
      unhealthy|exited|dead) docker inspect --format '{{json .State.Health}}' "$container" >&2; return 1 ;;
    esac
    [ "$(date +%s)" -lt "$deadline" ] || { echo "$service did not become healthy (last=$status)" >&2; return 1; }
    sleep 2
  done
}

wait_agent_ready() {
  wait_healthy "$agent_project" "$agent_env" "$agent_compose" core
  compose "$agent_project" "$agent_env" "$agent_compose" exec -T core /usr/local/bin/dirextalk-agent healthcheck >/dev/null
}

probe_server_public() {
  container=$(compose "$server_project" "$server_env" "$server_compose" ps -q message-server)
  [ -n "$container" ] || return 1
  binding=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$container")
  [ "$binding" != "{}" ] && [ "$binding" != "null" ] || { echo "Message Server has no host port binding" >&2; return 1; }
  http_port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "8008/tcp") 0).HostPort}}' "$container")
  [ -n "$http_port" ] || { echo "Message Server HTTP binding is empty" >&2; return 1; }
  network_binding=$(docker inspect --format '{{json (index .NetworkSettings.Ports "8008/tcp")}}' "$container")
  [ "$network_binding" != "null" ] && [ "$network_binding" != "[]" ] || { echo "Message Server NetworkSettings binding is empty" >&2; return 1; }
  curl --fail --silent --show-error "http://127.0.0.1:$http_port/_p2p/health" >/dev/null
  echo "Message Server public probe passed on exact container $container port $http_port" >&2
}

write_server_env() {
  instance_file=$run_dir/agent/instance-id
  [ -r "$instance_file" ] || { echo "missing Agent bootstrap instance-id" >&2; exit 1; }
  instance_id=$(tr -d '\r\n' < "$instance_file")
  [ -n "$instance_id" ] || { echo "empty Agent bootstrap instance-id" >&2; exit 1; }
  [ -e "$server_env" ] && chmod u+w "$server_env" || true
  cat > "$server_env" <<EOF
DIREXTALK_MESSAGE_SERVER_IMAGE=${DIREXTALK_MESSAGE_SERVER_IMAGE:-dirextalk/message-server:agent-core-local}
DIREXTALK_POSTGRES_IMAGE=${DIREXTALK_POSTGRES_IMAGE:-docker.io/library/postgres:18-alpine}
DIREXTALK_MESSAGE_SERVER_PROJECT=$server_project
DIREXTALK_AGENT_CALLER_NETWORK_NAME=$caller_network
P2P_AGENT_CORE_SERVER_NAME=$tls_server_name
P2P_AGENT_CORE_EXPECTED_INSTANCE_ID=$instance_id
DIREXTALK_AGENT_SERVICE_TOKEN_FILE=$run_dir/agent/service-token
DIREXTALK_AGENT_TLS_CERT_FILE=$run_dir/agent/tls-cert
DIREXTALK_MESSAGE_SERVER_PRIVATE_NETWORK=$server_network
DIREXTALK_MESSAGE_SERVER_PUBLIC_NETWORK=$public_network
DIREXTALK_MESSAGE_POSTGRES_VOLUME=$server_volume_postgres
DIREXTALK_MESSAGE_SERVER_CONFIG_VOLUME=$server_volume_config
DIREXTALK_MESSAGE_SERVER_DATA_VOLUME=$server_volume_data
DIREXTALK_MESSAGE_AGENT_CORE_MATERIAL_VOLUME=$server_volume_material
EOF
  chmod 0400 "$server_env"
}

bootstrap_agent() {
  # Keep bootstrap independent of either repository's current working
  # directory. Its atomic manifest validation owns reuse/incomplete-output
  # handling; this orchestrator always invokes it.
  (CDPATH= cd /tmp && "$agent_bootstrap" "$run_dir/agent" "$agent_image" "$runner_image" "$postgres_image" "$tls_server_name")
}

cleanup() {
  status=$?
  if [ "${KEEP:-0}" = 1 ]; then
    echo "KEEP=1: preserving Compose projects, volumes, and networks" >&2
    exit "$status"
  fi
  if [ -f "$server_env" ]; then
    safe_down "$server_project" "$server_env" "$server_compose" >/dev/null 2>&1 || true
  fi
  if [ -f "$agent_env" ]; then
    safe_down "$agent_project" "$agent_env" "$agent_compose" >/dev/null 2>&1 || true
  fi
  echo "cleaned exact test Compose projects, volumes, and networks" >&2
  exit "$status"
}

if [ "$action" = down ]; then
  [ -f "$server_env" ] && safe_down "$server_project" "$server_env" "$server_compose"
  [ -f "$agent_env" ] && safe_down "$agent_project" "$agent_env" "$agent_compose"
  exit 0
fi

if [ "$action" = rotate ]; then
  set_agent_names
  write_server_env
  rotate_stack
  exit $?
fi

trap cleanup EXIT INT TERM
bootstrap_agent
set_agent_names
write_server_env
ensure_initial_generation

# Validate both projects before creating anything, then build only the local
# Message Server image. Core image references remain explicit bootstrap inputs.
compose "$agent_project" "$agent_env" "$agent_compose" config --quiet
compose "$server_project" "$server_env" "$server_compose" config --quiet
if [ "${SKIP_SERVER_BUILD:-0}" != 1 ]; then
  compose "$server_project" "$server_env" "$server_compose" build message-server
fi

compose "$agent_project" "$agent_env" "$agent_compose" up -d
wait_agent_ready

compose "$server_project" "$server_env" "$server_compose" up -d
wait_healthy "$server_project" "$server_env" "$server_compose" message-server
probe_server_public
echo "Agent Core readiness and Message Server health passed" >&2

if [ "$action" = verify ]; then
  echo "topology verification passed; DeepSeek/chat probe intentionally skipped" >&2
fi
