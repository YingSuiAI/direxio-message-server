#!/bin/sh
# Two-stack Agent Core acceptance orchestrator.
#
# This is intentionally separate from agent-core-local.sh.  It owns one
# explicit STACK_ID at a time, keeps all generated metadata under an isolated
# run directory, enables both production runner profiles, and never prints
# secret contents.  It does not remove resources outside the exact Compose
# projects/networks/volumes derived from STACK_ID.
set -eu

usage() {
  echo "usage: $0 prepare|config|up|status|verify|down|cleanup [STACK_ID]" >&2
  exit 2
}

action=${1:-}
case "$action" in prepare|config|up|status|verify|down|cleanup) ;; *) usage ;; esac

die() { echo "agent-core-e2e: $*" >&2; exit 1; }

validate_text() {
  value=$1
  [ "$(printf '%s' "$value" | wc -l)" -eq 0 ] || die "value contains a newline"
  printf '%s' "$value" | LC_ALL=C grep -q '[[:cntrl:]]' && die "value contains control characters" || true
}

validate_path_value() {
  value=$1
  validate_text "$value"
  case "$value" in
    /*) ;;
    *) die "path must be absolute" ;;
  esac
  [ "$value" != "/" ] || die "path must not be filesystem root"
  case "$value" in
    *'//'*) die "path must be clean" ;;
    *'/../'*|*/..|../*) die "path must not contain parent traversal" ;;
  esac
}

validate_cgroup_parent_value() {
  value=$1
  validate_text "$value"
  case "$value" in *.slice) ;; *) die "cgroup parent must be a safe .slice name" ;; esac
  case "$value" in *[!A-Za-z0-9_.-]*) die "cgroup parent must be a safe .slice name" ;; esac
}

script_dir=$(CDPATH= cd -- "$(dirname "$0")" && pwd -P)
server_root=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
agent_root=${DIREXTALK_AGENT_ROOT:-$(CDPATH= cd -- "$server_root/../agent" && pwd -P)}
agent_compose=$agent_root/deploy/container/compose.local.yaml
server_compose=$server_root/docker-compose.agent-core-local.yml
server_e2e_override=$server_root/docker-compose.agent-core-e2e.yml

stack_id=${2:-${STACK_ID:-}}
[ -n "$stack_id" ] || { echo "STACK_ID is required" >&2; exit 2; }
case "$stack_id" in
  [a-z0-9]*) ;;
  *) echo "STACK_ID must start with lowercase alphanumeric and contain only [a-z0-9_-]" >&2; exit 2 ;;
esac
case "$stack_id" in *[!a-z0-9_-]*) echo "STACK_ID must start with lowercase alphanumeric and contain only [a-z0-9_-]" >&2; exit 2 ;; esac
[ "${#stack_id}" -le 24 ] || { echo "STACK_ID must be at most 24 characters" >&2; exit 2; }

run_root=${DIREXTALK_E2E_RUN_ROOT:-$server_root/.run.agent-core-e2e}
validate_text "$run_root"
run_root=$(mkdir -p "$run_root" && CDPATH= cd -- "$run_root" && pwd -P)
[ "$run_root" != "/" ] || die "refusing filesystem root as run root"
validate_path_value "$run_root"
run_dir=$run_root/$stack_id
[ ! -L "$run_dir" ] || { echo "refusing symlinked run directory" >&2; exit 1; }
case "$run_dir" in "$run_root"/*) ;; *) echo "invalid run directory" >&2; exit 1 ;; esac

agent_stack_name=dirextalk-agent-e2e-$stack_id
agent_project=$agent_stack_name
server_project=dirextalk-message-server-e2e-$stack_id
agent_network="${agent_stack_name}-private"
agent_egress_network="${agent_stack_name}-egress"
caller_network="${agent_stack_name}-caller"
server_network="dirextalk-message-e2e-private-$stack_id"
public_network="dirextalk-message-e2e-public-$stack_id"

agent_env=$run_dir/agent/.env
server_env=$run_dir/server.env
metadata=$run_dir/metadata.env

compose() {
  project=$1; env_file=$2; file=$3
  shift 3
  if [ "$project" = "$server_project" ]; then
    COMPOSE_PROFILES=extensions,core-runner \
      docker compose --profile extensions --profile core-runner -p "$project" --env-file "$env_file" -f "$file" -f "$server_e2e_override" "$@"
  else
    COMPOSE_PROFILES=extensions,core-runner \
      docker compose --profile extensions --profile core-runner -p "$project" --env-file "$env_file" -f "$file" "$@"
  fi
}

validate_image_ref() {
  value=$1
  validate_text "$value"
  printf '%s' "$value" | grep -Eq '^[A-Za-z0-9._-]+(:[0-9]{1,5})?(/[A-Za-z0-9._-]+)*@sha256:[0-9a-f]{64}$' ||
    die "all image references must be immutable digests"
  case "$value" in *'//'*) die "all image references must use clean paths" ;; esac
  name=${value%@sha256:*}
  case "$name" in *'/./'*|*/./|*'/../'*|*/..|../*) die "all image references must use clean paths" ;; esac
  hostport=${name%%/*}
  case "$hostport" in
    *:*)
      port=${hostport##*:}
      awk -v p="$port" 'BEGIN { exit !(p + 0 >= 1 && p + 0 <= 65535) }' || die "invalid image registry port"
      ;;
  esac
}

validate_secret_ref() {
  file=$1
  validate_path_value "$file"
  [ -n "$file" ] || die "secret file reference is empty"
  [ -f "$file" ] && [ ! -L "$file" ] || die "secret file reference is not a regular file"
}

assert_project_ownership() {
  project=$1; env_file=$2; file=$3
  [ -f "$env_file" ] || return 0
  config=$(compose "$project" "$env_file" "$file" config --format json)
  containers=$(compose "$project" "$env_file" "$file" ps -aq)
  for id in $containers; do
    owner=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$id" 2>/dev/null || true)
    [ "$owner" = "$project" ] || die "refusing cleanup: container ownership mismatch"
  done
  for kind in networks volumes; do
    for name in $(printf '%s' "$config" | jq -r ".${kind} // {} | to_entries[] | select(.value.external != true) | .value.name"); do
      [ -n "$name" ] || continue
      case "$kind" in
        networks) docker_kind=network ;;
        volumes) docker_kind=volume ;;
        *) die "unsupported Docker resource kind" ;;
      esac
      resources=$(docker "$docker_kind" ls --format '{{.Name}}') || die "refusing cleanup: cannot list $kind"
      resource_match=$(printf '%s\n' "$resources" | grep -Fxc "$name" || true)
      [ "$resource_match" -eq 1 ] || continue
      owner=$(docker "$docker_kind" inspect --format '{{index .Labels "com.docker.compose.project"}}' "$name") ||
        die "refusing cleanup: cannot inspect $kind"
      if [ -n "$owner" ]; then
        [ "$owner" = "$project" ] || die "refusing cleanup: $kind ownership mismatch"
      else
        die "refusing cleanup: $kind has no Compose ownership label"
      fi
    done
  done
}

validate_agent_manifest() {
  agent_dir=$run_dir/agent
  manifest=$agent_dir/.manifest
  [ -f "$manifest" ] && [ ! -L "$manifest" ] || die "missing Agent bootstrap manifest"
  [ "$(stat -c '%a' "$manifest" 2>/dev/null || echo 0)" = 400 ] || die "Agent manifest mode is unsafe"
  [ "$(sed -n '1p' "$manifest")" = '# dirextalk-bootstrap-manifest-v1' ] || die "invalid Agent bootstrap manifest"
  for name in .env config.yaml; do
    count=$(awk -v n="$name" '$2 == n { count++ } END { print count + 0 }' "$manifest")
    [ "$count" -eq 1 ] || die "invalid Agent manifest entry count"
    expected=$(awk -v n="$name" '$2 == n { print $1 }' "$manifest")
    printf '%s' "$expected" | grep -Eq '^[0-9a-f]{64}$' || die "missing Agent manifest hash"
    actual=$(sha256sum "$agent_dir/$name" | awk '{print $1}')
    [ "$actual" = "$expected" ] || die "Agent runtime manifest tampered"
  done
}

require_env_line() {
  key=$1
  expected=$2
  validate_text "$expected"
  [ "$(grep -Fxc "${key}=${expected}" "$agent_env")" -eq 1 ] || die "Agent bootstrap value mismatch for $key"
}

write_server_env() {
  [ -r "$run_dir/agent/instance-id" ] || die "missing Agent instance identity; run prepare"
  instance_id=$(tr -d '\r\n' < "$run_dir/agent/instance-id")
  [ -n "$instance_id" ] || die "empty Agent instance identity"
  validate_text "$instance_id"
  printf '%s' "$instance_id" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-8[0-9a-f]{3}-[0-9a-f]{12}$' ||
    die "invalid Agent instance identity"
  token_file=${DIREXTALK_AGENT_SERVICE_TOKEN_FILE:-$run_dir/agent/service-token}
  ca_file=${DIREXTALK_AGENT_TLS_CERT_FILE:-$run_dir/agent/tls-cert}
  message_password_file=${DIREXTALK_MESSAGE_POSTGRES_PASSWORD_FILE:-$run_dir/message-postgres-password}
  message_pgpass_file=${DIREXTALK_MESSAGE_POSTGRES_PGPASS_FILE:-$run_dir/message-postgres-pgpass}
  [ "$message_pgpass_file" != "$message_password_file" ] || die "PostgreSQL password and pgpass files must be distinct"
  validate_secret_ref "$token_file"
  validate_secret_ref "$ca_file"
  if [ ! -e "$message_password_file" ]; then
    umask 077
    openssl rand -hex 24 > "$message_password_file"
    chmod 0400 "$message_password_file"
  fi
  validate_secret_ref "$message_password_file"
  umask 077
  message_pgpass_tmp=$message_pgpass_file.tmp.$$
  awk -v db="${DIREXTALK_MESSAGE_POSTGRES_DB:-dirextalk_message_server}" \
    -v user="${DIREXTALK_MESSAGE_POSTGRES_USER:-dirextalk_message_server}" \
    'NR == 1 {
       if ($0 == "" || $0 ~ /[^A-Za-z0-9._~-]/) exit 1
       printf "postgres:5432:%s:%s:%s\n", db, user, $0
       next
     }
     { exit 1 }
     END { if (NR != 1) exit 1 }' \
    "$message_password_file" > "$message_pgpass_tmp" ||
    die "Message Server PostgreSQL password must be one pgpass-safe line"
  mv -f -- "$message_pgpass_tmp" "$message_pgpass_file"
  chmod 0400 "$message_pgpass_file"
  validate_secret_ref "$message_pgpass_file"
  message_image=${DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE:-${DIREXTALK_MESSAGE_SERVER_IMAGE:-}}
  postgres_image=${DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:-${DIREXTALK_POSTGRES_IMAGE:-}}
  core_image=${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}
  runner_image=${DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE:-}
  core_runner_image=${DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE:-}
  validate_image_ref "$message_image"
  validate_image_ref "$postgres_image"
  validate_image_ref "$core_image"
  validate_image_ref "$runner_image"
  validate_image_ref "$core_runner_image"
  cat > "$server_env" <<EOF
DIREXTALK_MESSAGE_SERVER_IMAGE=$message_image
DIREXTALK_POSTGRES_IMAGE=$postgres_image
DIREXTALK_MESSAGE_SERVER_PROJECT=$server_project
DIREXTALK_AGENT_CALLER_NETWORK_NAME=$caller_network
P2P_AGENT_CORE_SERVER_NAME=${P2P_AGENT_CORE_SERVER_NAME:-core.local}
P2P_AGENT_CORE_EXPECTED_INSTANCE_ID=$instance_id
DIREXTALK_AGENT_SERVICE_TOKEN_FILE=$token_file
DIREXTALK_AGENT_TLS_CERT_FILE=$ca_file
DIREXTALK_MESSAGE_POSTGRES_PASSWORD_FILE=$message_password_file
DIREXTALK_MESSAGE_POSTGRES_PGPASS_FILE=$message_pgpass_file
DIREXTALK_MESSAGE_SERVER_PRIVATE_NETWORK=$server_network
DIREXTALK_MESSAGE_SERVER_PUBLIC_NETWORK=$public_network
DIREXTALK_MESSAGE_POSTGRES_VOLUME=dirextalk-message-e2e-postgres-$stack_id
DIREXTALK_MESSAGE_SERVER_CONFIG_VOLUME=dirextalk-message-e2e-config-$stack_id
DIREXTALK_MESSAGE_SERVER_DATA_VOLUME=dirextalk-message-e2e-data-$stack_id
DIREXTALK_MESSAGE_AGENT_CORE_MATERIAL_VOLUME=dirextalk-message-e2e-material-$stack_id
EOF
  chmod 0400 "$server_env"
  cat > "$metadata" <<EOF
STACK_ID=$stack_id
AGENT_PROJECT=$agent_project
SERVER_PROJECT=$server_project
AGENT_COMPOSE=$agent_compose
SERVER_COMPOSE=$server_compose
CALLER_NETWORK=$caller_network
EOF
  chmod 0400 "$metadata"
}

prepare() {
  mkdir -p "$run_dir"
  [ ! -e "$run_dir/agent" ] || [ -d "$run_dir/agent" ] || die "Agent bootstrap path is not a directory"
  bootstrap=$agent_root/deploy/container/scripts/bootstrap-local.sh
  [ -x "$bootstrap" ] || die "missing Agent bootstrap script"
  core_image=${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}
  runner_image=${DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE:-}
  postgres_image=${DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:-}
  core_runner_image=${DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE:-}
  validate_image_ref "$core_image"
  validate_image_ref "$runner_image"
  validate_image_ref "$postgres_image"
  validate_image_ref "$core_runner_image"
  tls_name=${P2P_AGENT_CORE_SERVER_NAME:-core.local}
  validate_text "$tls_name"
  extension_enabled=${DIREXTALK_E2E_CORE_EXTENSION_ENABLED:-true}
  workload_enabled=${DIREXTALK_E2E_CORE_WORKLOAD_ENABLED:-true}
  extension_cgroup=${DIREXTALK_E2E_EXTENSION_CGROUP_ROOT:-$run_dir/cgroup/extension}
  workload_cgroup=${DIREXTALK_E2E_CORE_RUNNER_CGROUP_ROOT:-$run_dir/cgroup/core-runner}
  extension_parent=${DIREXTALK_E2E_EXTENSION_CGROUP_PARENT:-${agent_stack_name}-extension.slice}
  workload_parent=${DIREXTALK_E2E_CORE_RUNNER_CGROUP_PARENT:-${agent_stack_name}-core-runner.slice}
  validate_path_value "$extension_cgroup"
  validate_path_value "$workload_cgroup"
  validate_cgroup_parent_value "$extension_parent"
  validate_cgroup_parent_value "$workload_parent"
  [ "$extension_parent" != "$workload_parent" ] || die "runner cgroup parents must be distinct"
  DIREXTALK_CORE_EXTENSION_ENABLED="$extension_enabled" \
    DIREXTALK_CORE_WORKLOAD_ENABLED="$workload_enabled" \
    DIREXTALK_AGENT_STACK_NAME="dirextalk-agent-e2e-$stack_id" \
    DIREXTALK_EXTENSION_CGROUP_ROOT="$extension_cgroup" \
    DIREXTALK_CORE_RUNNER_CGROUP_ROOT="$workload_cgroup" \
    DIREXTALK_EXTENSION_CGROUP_PARENT="$extension_parent" \
    DIREXTALK_CORE_RUNNER_CGROUP_PARENT="$workload_parent" \
    DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE="$core_runner_image" \
    "$bootstrap" "$run_dir/agent" "$core_image" "$runner_image" "$postgres_image" "$tls_name" >/dev/null
  grep -Fqx "core_extension_enabled: $extension_enabled" "$run_dir/agent/config.yaml" ||
    die "existing bootstrap config has a different extension enablement; use a new STACK_ID"
  grep -Fqx "core_workload_enabled: $workload_enabled" "$run_dir/agent/config.yaml" ||
    die "existing bootstrap config has a different workload enablement; use a new STACK_ID"
  validate_agent_manifest
  require_env_line DIREXTALK_AGENT_STACK_NAME "$agent_stack_name"
  require_env_line DIREXTALK_AGENT_NETWORK_NAME "$agent_network"
  require_env_line DIREXTALK_AGENT_EGRESS_NETWORK_NAME "$agent_egress_network"
  require_env_line DIREXTALK_AGENT_CALLER_NETWORK_NAME "$caller_network"
  require_env_line DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE "$core_runner_image"
  require_env_line DIREXTALK_EXTENSION_CGROUP_ROOT "$extension_cgroup"
  require_env_line DIREXTALK_CORE_RUNNER_CGROUP_ROOT "$workload_cgroup"
  require_env_line DIREXTALK_EXTENSION_CGROUP_PARENT "$extension_parent"
  require_env_line DIREXTALK_CORE_RUNNER_CGROUP_PARENT "$workload_parent"
  write_server_env
  echo "prepared isolated Agent Core stack $stack_id" >&2
}

require_prepared() {
  [ -r "$agent_env" ] && [ -r "$server_env" ] || die "stack is not prepared; run prepare first"
  validate_agent_manifest
}

cgroup_root_from_env() {
  key=$1
  value=$2
  validate_path_value "$value"
  case "$value" in
    /sys/fs/cgroup/*) ;;
    *) die "$key must be an existing delegated cgroup-v2 subtree under /sys/fs/cgroup" ;;
  esac
  [ -d "$value" ] && [ ! -L "$value" ] || die "$key must be an existing directory"
  fs_type=$(stat -fc '%T' "$value" 2>/dev/null || true)
  [ "$fs_type" = cgroup2fs ] || die "$key is not a cgroup-v2 directory"
  owner=$(stat -c '%u' "$value" 2>/dev/null || true)
  mode=$(stat -c '%a' "$value" 2>/dev/null || true)
  [ "$owner" = "$3" ] || die "$key must be owned by runner UID $3"
  [ -n "$mode" ] || die "$key mode is unavailable"
  [ $((0$mode & 18)) -eq 0 ] || die "$key must not be group/world writable"
  # The host-side orchestrator normally runs as the deployment user, while
  # the delegated subtree is intentionally owned by the unprivileged runner
  # UID.  Testing `-w` here would therefore reject every correctly-owned
  # subtree.  Verify the runner-owned control files and their owner-write bit
  # instead; the runner itself is the process that will exercise them.
  for control in cgroup.procs cgroup.subtree_control; do
    [ -e "$value/$control" ] || die "$key $control is unavailable"
    control_owner=$(stat -c '%u' "$value/$control" 2>/dev/null || true)
    control_mode=$(stat -c '%a' "$value/$control" 2>/dev/null || true)
    [ "$control_owner" = "$3" ] || die "$key $control must be owned by runner UID $3"
    [ -n "$control_mode" ] && [ $((0$control_mode & 128)) -ne 0 ] ||
      die "$key $control must be owner-writable"
  done
  # cgroupfs pseudo-files report a zero inode size even when they contain
  # delegated controller names, so `-s` is not a valid capability check.
  controllers=$(cat "$value/cgroup.controllers" 2>/dev/null || true)
  [ -n "$controllers" ] || die "$key has no delegated controllers"
}

preflight_cgroups() {
  extension_cgroup=${DIREXTALK_E2E_EXTENSION_CGROUP_ROOT:-}
  workload_cgroup=${DIREXTALK_E2E_CORE_RUNNER_CGROUP_ROOT:-}
  [ -n "$extension_cgroup" ] && [ -n "$workload_cgroup" ] ||
    die "up requires DIREXTALK_E2E_EXTENSION_CGROUP_ROOT and DIREXTALK_E2E_CORE_RUNNER_CGROUP_ROOT"
  [ "$extension_cgroup" != "$workload_cgroup" ] || die "runner cgroup roots must be distinct"
  cgroup_root_from_env DIREXTALK_E2E_EXTENSION_CGROUP_ROOT "$extension_cgroup" 65531
  cgroup_root_from_env DIREXTALK_E2E_CORE_RUNNER_CGROUP_ROOT "$workload_cgroup" 65530
}

verify_cgroup_parent_mapping() {
  parent=$1
  root=$2
  root_real=$(realpath -e -- "$root" 2>/dev/null || true)
  [ -n "$root_real" ] || die "delegated cgroup root cannot be resolved"
  matches=$(find /sys/fs/cgroup -type d -name "$parent" -print 2>/dev/null || true)
  count=$(printf '%s\n' "$matches" | sed '/^$/d' | wc -l)
  [ "$count" -eq 1 ] || die "cgroup parent must resolve to exactly one host slice"
  slice_real=$(realpath -e -- "$matches" 2>/dev/null || true)
  case "$root_real" in "$slice_real"/*) ;; *) die "delegated cgroup root is outside its configured parent slice" ;; esac
}

wait_service() {
  project=$1; env_file=$2; file=$3; service=$4
  deadline=$(( $(date +%s) + ${DIREXTALK_E2E_HEALTH_TIMEOUT_SECONDS:-180} ))
  while :; do
    container=$(compose "$project" "$env_file" "$file" ps -q "$service" || true)
    if [ -n "$container" ]; then
      status=$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)
      health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)
      case "$status:$health" in
        running:healthy|running:none) return 0 ;;
        exited:*|dead:*) die "$service did not stay running" ;;
      esac
    fi
    [ "$(date +%s)" -lt "$deadline" ] || die "timed out waiting for $service"
    sleep 2
  done
}

wait_volume_socket() {
  volume=$1
  socket_name=$2
  deadline=$(( $(date +%s) + ${DIREXTALK_E2E_HEALTH_TIMEOUT_SECONDS:-180} ))
  while :; do
    mountpoint=$(docker volume inspect --format '{{.Mountpoint}}' "$volume" 2>/dev/null || true)
    if [ -n "$mountpoint" ] && [ -S "$mountpoint/$socket_name" ]; then
      return 0
    fi
    [ "$(date +%s)" -lt "$deadline" ] || die "timed out waiting for runner socket $socket_name"
    sleep 2
  done
}

config() {
  require_prepared
  compose "$agent_project" "$agent_env" "$agent_compose" config --quiet
  compose "$server_project" "$server_env" "$server_compose" config --quiet
  echo "compose configuration valid for isolated stack $stack_id" >&2
}

up() {
  require_prepared
  config
  preflight_cgroups
  stack_started=0
  cleanup_partial_up() {
    status=$?
    if [ "$status" -ne 0 ] && [ "$stack_started" -eq 1 ]; then
      down >/dev/null 2>&1 || true
    fi
    trap - EXIT
    exit "$status"
  }
  trap cleanup_partial_up EXIT INT TERM

  # Keep the dependency fence visible: database/secret-init, then migration,
  # then both isolated runners, then Core, and only then Message Server.
  compose "$agent_project" "$agent_env" "$agent_compose" up -d postgres
  stack_started=1
  wait_service "$agent_project" "$agent_env" "$agent_compose" postgres
  compose "$agent_project" "$agent_env" "$agent_compose" up --abort-on-container-exit --exit-code-from migrate migrate
  compose "$agent_project" "$agent_env" "$agent_compose" up -d extension-runner core-runner
  wait_service "$agent_project" "$agent_env" "$agent_compose" extension-runner
  wait_service "$agent_project" "$agent_env" "$agent_compose" core-runner
  wait_volume_socket "${agent_stack_name}-extension-socket" extension-runner.sock
  wait_volume_socket "${agent_stack_name}-core-runner-socket" runner.sock
  compose "$agent_project" "$agent_env" "$agent_compose" up -d core
  wait_service "$agent_project" "$agent_env" "$agent_compose" core
  expected_instance_id=$(tr -d '\r\n' < "$run_dir/agent/instance-id")
  compose "$agent_project" "$agent_env" "$agent_compose" exec -T core \
    /usr/local/bin/dirextalk-agent healthcheck \
    --expect-instance-id "$expected_instance_id" \
    --require-capability agent.info \
    --require-capability mcp \
    --require-capability skill \
    --require-capability conversation.extensions \
    --require-capability workload.core_runner
  compose "$server_project" "$server_env" "$server_compose" up -d
  wait_service "$server_project" "$server_env" "$server_compose" message-server
  extension_parent=$(awk -F= '$1 == "DIREXTALK_EXTENSION_CGROUP_PARENT" { print $2 }' "$agent_env")
  workload_parent=$(awk -F= '$1 == "DIREXTALK_CORE_RUNNER_CGROUP_PARENT" { print $2 }' "$agent_env")
  extension_root=$(awk -F= '$1 == "DIREXTALK_EXTENSION_CGROUP_ROOT" { print $2 }' "$agent_env")
  workload_root=$(awk -F= '$1 == "DIREXTALK_CORE_RUNNER_CGROUP_ROOT" { print $2 }' "$agent_env")
  verify_cgroup_parent_mapping "$extension_parent" "$extension_root"
  verify_cgroup_parent_mapping "$workload_parent" "$workload_root"
  status
  trap - EXIT INT TERM
}

status() {
  require_prepared
  compose "$agent_project" "$agent_env" "$agent_compose" ps
  compose "$server_project" "$server_env" "$server_compose" ps
  container=$(compose "$server_project" "$server_env" "$server_compose" ps -q message-server || true)
  if [ -n "$container" ]; then
    port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "8008/tcp") 0).HostPort}}' "$container" 2>/dev/null || true)
    [ -n "$port" ] && echo "message-server loopback port: $port" >&2 || true
  fi
}

verify() {
  require_prepared
  assert_project_ownership "$server_project" "$server_env" "$server_compose"
  assert_project_ownership "$agent_project" "$agent_env" "$agent_compose"
  for service in postgres extension-runner core-runner core; do
    wait_service "$agent_project" "$agent_env" "$agent_compose" "$service"
  done
  wait_service "$server_project" "$server_env" "$server_compose" message-server
  extension_parent=$(awk -F= '$1 == "DIREXTALK_EXTENSION_CGROUP_PARENT" { print $2 }' "$agent_env")
  workload_parent=$(awk -F= '$1 == "DIREXTALK_CORE_RUNNER_CGROUP_PARENT" { print $2 }' "$agent_env")
  extension_root=$(awk -F= '$1 == "DIREXTALK_EXTENSION_CGROUP_ROOT" { print $2 }' "$agent_env")
  workload_root=$(awk -F= '$1 == "DIREXTALK_CORE_RUNNER_CGROUP_ROOT" { print $2 }' "$agent_env")
  verify_cgroup_parent_mapping "$extension_parent" "$extension_root"
  verify_cgroup_parent_mapping "$workload_parent" "$workload_root"
  expected_instance_id=$(tr -d '\r\n' < "$run_dir/agent/instance-id")
  compose "$agent_project" "$agent_env" "$agent_compose" exec -T core \
    /usr/local/bin/dirextalk-agent healthcheck \
    --expect-instance-id "$expected_instance_id" \
    --require-capability agent.info \
    --require-capability model.profile \
    --require-capability conversation \
    --require-capability mcp \
    --require-capability skill \
    --require-capability task \
    --require-capability confirmation \
    --require-capability workload.core_runner
  container=$(compose "$server_project" "$server_env" "$server_compose" ps -q message-server || true)
  [ -n "$container" ] || die "verify requires an already running message-server; run up first"
  running=$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)
  [ "$running" = running ] || die "verify requires an already running message-server; run up first"
  port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "8008/tcp") 0).HostPort}}' "$container" 2>/dev/null || true)
  [ -n "$port" ] || die "verify could not resolve the message-server loopback port"
  owner_file=${DIREXTALK_E2E_OWNER_SECRET_FILE:-}
  [ -n "$owner_file" ] || die "verify requires DIREXTALK_E2E_OWNER_SECRET_FILE"
  api_key_file=${DIREXTALK_E2E_DEEPSEEK_API_KEY_FILE:-}
  flow=${DIREXTALK_E2E_FLOW:-full}
  case "$flow" in full|deepseek|extensions|workload) ;; *) die "DIREXTALK_E2E_FLOW must be full, deepseek, extensions, or workload" ;; esac
  runner=${DIREXTALK_AGENT_CORE_E2E_BIN:-}
  if [ -n "$runner" ]; then
    [ -x "$runner" ] || die "DIREXTALK_AGENT_CORE_E2E_BIN must be executable"
    command_args="$runner"
  else
    command_args="go run ./cmd/agent-core-e2e"
  fi
  # Secret values are never interpolated; only caller-owned file paths cross
  # this boundary. The driver prints only its sanitized JSON summary.
  set -- $command_args "$flow" --base-url "http://127.0.0.1:$port" --owner-secret-file "$owner_file"
  if [ -n "$api_key_file" ]; then set -- "$@" --deepseek-api-key-file "$api_key_file"; fi
  if [ -n "${DIREXTALK_E2E_EXTENSION_SECRET_INPUT_FILE:-}" ]; then set -- "$@" --extension-secret-input-file "$DIREXTALK_E2E_EXTENSION_SECRET_INPUT_FILE"; fi
  if [ -n "${DIREXTALK_E2E_WORKLOAD_PLAN_FILE:-}" ]; then set -- "$@" --workload-plan-file "$DIREXTALK_E2E_WORKLOAD_PLAN_FILE"; fi
  # Re-check both host slice mappings immediately before executing the driver;
  # a delegated subtree must not drift between healthcheck and acceptance.
  verify_cgroup_parent_mapping "$extension_parent" "$extension_root"
  verify_cgroup_parent_mapping "$workload_parent" "$workload_root"
  "$@"
}

down() {
  require_prepared
  assert_project_ownership "$server_project" "$server_env" "$server_compose"
  assert_project_ownership "$agent_project" "$agent_env" "$agent_compose"
  compose "$server_project" "$server_env" "$server_compose" down --volumes --remove-orphans
  compose "$agent_project" "$agent_env" "$agent_compose" down --volumes --remove-orphans
  echo "stopped exact isolated stack $stack_id" >&2
}

cleanup() {
  down
  case "$run_dir" in "$run_root"/*) rm -rf -- "$run_dir" ;; *) die "refusing cleanup outside run root" ;; esac
  echo "removed exact isolated run directory for $stack_id" >&2
}

case "$action" in
  prepare) prepare ;;
  config) config ;;
  up) up ;;
  status) status ;;
  verify) verify ;;
  down) down ;;
  cleanup) cleanup ;;
esac
