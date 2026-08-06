#!/usr/bin/env bash
# Shared receipt-bound lifecycle implementation for the three Agent runtime
# containers.  The public wrappers below deliberately do not use Compose:
# message-server, Postgres, Qdrant, networks, and volumes are outside scope.

set -euo pipefail

agent_runtime_die() {
  printf 'split-agent runtime: %s\n' "$*" >&2
  exit 1
}

agent_runtime_usage() {
  printf 'usage: %s OUTPUT_DIR\n' "$1" >&2
  exit 2
}

agent_runtime_read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" \
    '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' \
    "$file")
  [ "$count" -eq 1 ] || agent_runtime_die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" \
    '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' \
    "$file")
  [ -n "$value" ] || agent_runtime_die "$file has an empty $key entry"
  printf '%s' "$value"
}

agent_runtime_file_identity() {
  local path=$1 label=$2 identity
  [ -f "$path" ] && [ ! -L "$path" ] || agent_runtime_die "$label is missing or symlinked"
  identity=$(stat -c '%d:%i:%u' -- "$path") || agent_runtime_die "$label metadata inspection failed"
  printf '%s' "$identity"
}

agent_runtime_verify_control() {
  local path=$1 label=$2 expected_identity=$3 expected_digest=$4 current_identity current_digest
  current_identity=$(agent_runtime_file_identity "$path" "$label")
  [ "$current_identity" = "$expected_identity" ] || agent_runtime_die "$label identity changed"
  [ "$(stat -c '%a' -- "$path")" = 400 ] || agent_runtime_die "$label mode changed"
  [ "$(stat -c '%u' -- "$path")" = "$agent_runtime_uid" ] || agent_runtime_die "$label owner changed"
  current_digest=$(sha256sum -- "$path" | awk '{print $1}')
  [ "$current_digest" = "$expected_digest" ] || agent_runtime_die "$label contents changed"
}

agent_runtime_verify_controls() {
  agent_runtime_verify_control "$agent_runtime_env_file" .env \
    "$agent_runtime_env_identity" "$agent_runtime_env_digest"
  agent_runtime_verify_control "$agent_runtime_manifest" .manifest \
    "$agent_runtime_manifest_identity" "$agent_runtime_manifest_digest"
  agent_runtime_verify_control "$agent_runtime_receipt" .cleanup-receipt \
    "$agent_runtime_receipt_identity" "$agent_runtime_receipt_digest"
}

agent_runtime_verify_host() {
  local endpoint socket canonical machine engine
  [ -z "${DOCKER_HOST:-}" ] || agent_runtime_die "DOCKER_HOST must be unset"
  case "${DOCKER_CONTEXT:-default}" in
    ''|default) ;;
    *) agent_runtime_die "DOCKER_CONTEXT must be unset or default" ;;
  esac
  if endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null); then
    :
  else
    agent_runtime_die "Docker context inspection failed"
  fi
  [ "$endpoint" = "$agent_runtime_context_endpoint" ] || agent_runtime_die "Docker context endpoint changed"
  case "$endpoint" in
    unix:///*) ;;
    *) agent_runtime_die "Docker context is not a local Unix endpoint" ;;
  esac
  socket=${endpoint#unix://}
  [ -S "$socket" ] || agent_runtime_die "Docker context socket is unavailable"
  canonical=$(readlink -f -- "$socket" 2>/dev/null || true)
  [ "$canonical" = "$agent_runtime_context_socket" ] || agent_runtime_die "Docker context socket identity changed"

  [ -f /etc/machine-id ] && [ ! -L /etc/machine-id ] || agent_runtime_die "host machine-id is unavailable"
  [ "$(stat -c '%u:%g' /etc/machine-id 2>/dev/null || true)" = 0:0 ] || agent_runtime_die "host machine-id is not root-owned"
  machine=$(tr -d '[:space:]' </etc/machine-id 2>/dev/null || true)
  printf '%s\n' "$machine" | grep -Eq '^[0-9a-f]{32}$' || agent_runtime_die "host machine-id is invalid"
  [ "$machine" = "$agent_runtime_machine_id" ] || agent_runtime_die "host machine-id changed"

  if engine=$(docker info --format '{{.ID}}' 2>/dev/null); then
    :
  else
    agent_runtime_die "Docker Engine ID query failed"
  fi
  [ "$engine" = "$agent_runtime_engine_id" ] || agent_runtime_die "Docker Engine ID changed"
}

agent_runtime_image_id() {
  local image=$1 image_id
  if image_id=$(docker image inspect "$image" --format '{{.Id}}' 2>/dev/null); then
    :
  else
    agent_runtime_die "Docker image identity inspection failed for $image"
  fi
  printf '%s\n' "$image_id" | grep -Eq '^sha256:[0-9a-f]{64}$' || agent_runtime_die "Docker image identity is invalid for $image"
  printf '%s' "$image_id"
}

agent_runtime_verify_target() {
  local role=$1 id=${agent_runtime_ids[$1]} name=${agent_runtime_names[$1]}
  local service=${agent_runtime_services[$1]} data replacement actual_id raw_name actual_name actual_service project image image_id
  if data=$(docker inspect "$id" 2>/dev/null); then
    :
  else
    if replacement=$(docker inspect "$name" 2>/dev/null); then
      actual_id=$(jq -r '.[0].Id // empty' <<<"$replacement")
      agent_runtime_die "same-name replacement detected for $role ($name, id $actual_id)"
    fi
    agent_runtime_die "exact recorded $role container is unavailable ($id)"
  fi
  if ! jq -e 'type == "array" and length == 1' <<<"$data" >/dev/null; then
    agent_runtime_die "$role container inspect returned malformed JSON"
  fi
  image_id=$(agent_runtime_image_id "$agent_runtime_expected_image")
  actual_id=$(jq -r '.[0].Id // empty' <<<"$data")
  raw_name=$(jq -r '.[0].Name // empty' <<<"$data")
  actual_name=${raw_name#/}
  actual_service=$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")
  project=$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")
  image=$(jq -r '.[0].Config.Image // empty' <<<"$data")
  agent_runtime_target_status[$role]=$(jq -r '.[0].State.Status // empty' <<<"$data")
  agent_runtime_target_health[$role]=$(jq -r '.[0].State.Health.Status // "none"' <<<"$data")
  agent_runtime_target_image_id[$role]=$(jq -r '.[0].Image // empty' <<<"$data")
  [ "$actual_id" = "$id" ] || agent_runtime_die "$role container ID identity changed"
  [ "$actual_name" = "$name" ] || agent_runtime_die "$role container name identity changed"
  [ "$actual_service" = "$service" ] || agent_runtime_die "$role container service identity changed"
  [ "$project" = "$agent_runtime_stack" ] || agent_runtime_die "$role container project identity changed"
  [ "$image" = "$agent_runtime_expected_image" ] || agent_runtime_die "$role container Config.Image identity changed"
  printf '%s\n' "${agent_runtime_target_image_id[$role]}" | grep -Eq '^sha256:[0-9a-f]{64}$' || agent_runtime_die "$role container image ID is invalid"
  [ "${agent_runtime_target_image_id[$role]}" = "$image_id" ] || agent_runtime_die "$role container image ID identity changed"
  [ -n "${agent_runtime_target_status[$role]}" ] || agent_runtime_die "$role container state is unavailable"
}

agent_runtime_refresh_targets() {
  local role
  for role in agent extension-runner core-runner; do
    agent_runtime_verify_target "$role"
  done
}

agent_runtime_before_mutation() {
  agent_runtime_verify_controls
  agent_runtime_verify_host
  agent_runtime_refresh_targets
  agent_runtime_require_known_states
}

agent_runtime_is_stopped() {
  case "$1" in
    created|exited) return 0 ;;
    *) return 1 ;;
  esac
}

agent_runtime_is_running() {
  [ "$1" = running ]
}

agent_runtime_is_active() {
  case "$1" in
    running|restarting) return 0 ;;
    *) return 1 ;;
  esac
}

agent_runtime_require_known_states() {
  local role status
  for role in agent extension-runner core-runner; do
    status=${agent_runtime_target_status[$role]}
    if ! agent_runtime_is_stopped "$status" && ! agent_runtime_is_active "$status"; then
      agent_runtime_die "$role container has an unknown state: $status"
    fi
  done
}

agent_runtime_stop_one() {
  local role=$1 id=${agent_runtime_ids[$1]}
  agent_runtime_before_mutation
  agent_runtime_is_active "${agent_runtime_target_status[$role]}" || return 0
  if docker container stop "$id" >/dev/null; then
    :
  else
    agent_runtime_die "exact $role stop failed"
  fi
  agent_runtime_before_mutation
  agent_runtime_is_stopped "${agent_runtime_target_status[$role]}" || agent_runtime_die "$role did not reach a stopped state"
}

agent_runtime_wait_healthy() {
  local role=$1 attempts=$agent_runtime_health_timeout_seconds status health
  while [ "$attempts" -gt 0 ]; do
    agent_runtime_verify_controls
    agent_runtime_verify_host
    agent_runtime_refresh_targets
    agent_runtime_require_known_states
    status=${agent_runtime_target_status[$role]}
    health=${agent_runtime_target_health[$role]}
    if [ "$status" = running ] && [ "$health" = healthy ]; then
      return 0
    fi
    case "$health" in
      healthy|starting|unhealthy|none) ;;
      *) agent_runtime_die "$role has an unknown health state while settling: $health" ;;
    esac
    attempts=$((attempts - 1))
    [ "$attempts" -gt 0 ] && sleep 1
  done
  agent_runtime_die "$role health check timed out"
}

agent_runtime_start_one() {
  local role=$1 id=${agent_runtime_ids[$1]} status
  agent_runtime_before_mutation
  status=${agent_runtime_target_status[$role]}
  if agent_runtime_is_stopped "$status"; then
    if docker container start "$id" >/dev/null; then
      :
    else
      agent_runtime_die "exact $role start failed"
    fi
  elif ! agent_runtime_is_active "$status"; then
    agent_runtime_die "$role is neither stopped nor active before start"
  fi
  agent_runtime_wait_healthy "$role"
}

agent_runtime_stop() {
  local role all_stopped=true
  agent_runtime_refresh_targets
  agent_runtime_require_known_states
  for role in agent extension-runner core-runner; do
    if ! agent_runtime_is_stopped "${agent_runtime_target_status[$role]}"; then
      all_stopped=false
    fi
  done
  [ "$all_stopped" = true ] && return 3
  for role in agent extension-runner core-runner; do
    agent_runtime_stop_one "$role"
  done
  agent_runtime_before_mutation
  for role in agent extension-runner core-runner; do
    agent_runtime_is_stopped "${agent_runtime_target_status[$role]}" || agent_runtime_die "$role remains active after stop"
  done
  return 0
}

agent_runtime_restart() {
  local role
  agent_runtime_refresh_targets
  agent_runtime_require_known_states
  # Restart always crosses an exact stop boundary, including an already
  # healthy runtime.  A mixed known state is recovered through the same
  # controlled sequence, then the runners are proven healthy before Agent.
  for role in agent extension-runner core-runner; do
    agent_runtime_stop_one "$role"
  done
  agent_runtime_start_one extension-runner
  agent_runtime_start_one core-runner
  agent_runtime_start_one agent
  return 0
}

agent_runtime_main() {
  local operation=$1 out_input=$2
  local current_uid out
  [ "$operation" = stop ] || [ "$operation" = restart ] || agent_runtime_usage "${0##*/}"
  case "$out_input" in
    /*) out=$(readlink -m -- "$out_input") ;;
    *) out=$(readlink -m -- "$(pwd -P)/$out_input") ;;
  esac
  [ "$out" != / ] || agent_runtime_die "refusing to use the filesystem root"
  [ -d "$out" ] && [ ! -L "$out" ] || agent_runtime_die "OUTPUT_DIR must be a regular non-symlink directory"
  [ "$(stat -c '%a' -- "$out")" = 700 ] || agent_runtime_die "OUTPUT_DIR must be mode 0700"
  current_uid=$(id -u)
  agent_runtime_uid=$current_uid
  agent_runtime_env_file=$out/.env
  agent_runtime_manifest=$out/.manifest
  agent_runtime_receipt=$out/.cleanup-receipt
  for path in "$agent_runtime_env_file" "$agent_runtime_manifest" "$agent_runtime_receipt"; do
    [ -f "$path" ] && [ ! -L "$path" ] || agent_runtime_die "missing protected control file: $path"
    [ "$(stat -c '%a' -- "$path")" = 400 ] || agent_runtime_die "control file must be mode 0400: $path"
    [ "$(stat -c '%u' -- "$path")" = "$current_uid" ] || agent_runtime_die "control file owner mismatch: $path"
  done

  agent_runtime_env_identity=$(stat -c '%d:%i:%u' -- "$agent_runtime_env_file")
  agent_runtime_manifest_identity=$(stat -c '%d:%i:%u' -- "$agent_runtime_manifest")
  agent_runtime_receipt_identity=$(stat -c '%d:%i:%u' -- "$agent_runtime_receipt")
  agent_runtime_env_digest=$(sha256sum -- "$agent_runtime_env_file" | awk '{print $1}')
  agent_runtime_manifest_digest=$(sha256sum -- "$agent_runtime_manifest" | awk '{print $1}')
  agent_runtime_receipt_digest=$(sha256sum -- "$agent_runtime_receipt" | awk '{print $1}')
  grep -Fqx '# dirextalk-split-cleanup-receipt-v1' "$agent_runtime_receipt" || agent_runtime_die "cleanup receipt version is unsupported"

  # Bind the current provisioning controls to the identities and digests that
  # start-local captured in the immutable cleanup receipt.  Establishing only
  # a process-local baseline here would accept a file replaced before this
  # wrapper started.
  [ "$(agent_runtime_read_pair "$agent_runtime_receipt" control.env_identity)" = "$agent_runtime_env_identity" ] || agent_runtime_die "cleanup receipt was not created for this .env"
  [ "$(agent_runtime_read_pair "$agent_runtime_receipt" control.manifest_identity)" = "$agent_runtime_manifest_identity" ] || agent_runtime_die "cleanup receipt was not created for this manifest"
  [ "$(agent_runtime_read_pair "$agent_runtime_receipt" control.env_sha256)" = "$agent_runtime_env_digest" ] || agent_runtime_die "cleanup receipt .env digest differs"
  [ "$(agent_runtime_read_pair "$agent_runtime_receipt" control.manifest_sha256)" = "$agent_runtime_manifest_digest" ] || agent_runtime_die "cleanup receipt manifest digest differs"

  agent_runtime_stack=$(agent_runtime_read_pair "$agent_runtime_manifest" stack_name)
  [ "$agent_runtime_stack" = "$(agent_runtime_read_pair "$agent_runtime_receipt" stack_name)" ] || agent_runtime_die "receipt stack identity differs from manifest"
  printf '%s\n' "$agent_runtime_stack" | grep -Eq '^d-[a-z2-7]{26}$' || agent_runtime_die "stack identity is invalid"
  [ "$(agent_runtime_read_pair "$agent_runtime_receipt" state)" = complete ] || agent_runtime_die "cleanup receipt is not complete"
  agent_runtime_machine_id=$(agent_runtime_read_pair "$agent_runtime_manifest" runner.machine_id)
  agent_runtime_engine_id=$(agent_runtime_read_pair "$agent_runtime_manifest" runner.docker_engine_id)
  [ "$agent_runtime_machine_id" = "$(agent_runtime_read_pair "$agent_runtime_receipt" host.machine_id)" ] || agent_runtime_die "receipt machine-id differs from manifest"
  [ "$agent_runtime_engine_id" = "$(agent_runtime_read_pair "$agent_runtime_receipt" docker.engine_id)" ] || agent_runtime_die "receipt Engine ID differs from manifest"
  printf '%s\n' "$agent_runtime_machine_id" | grep -Eq '^[0-9a-f]{32}$' || agent_runtime_die "machine-id binding is invalid"
  printf '%s\n' "$agent_runtime_engine_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || agent_runtime_die "Engine ID binding is invalid"
  agent_runtime_context_endpoint=$(agent_runtime_read_pair "$agent_runtime_receipt" docker.context_endpoint)
  agent_runtime_context_socket=$(agent_runtime_read_pair "$agent_runtime_receipt" docker.context_socket)
  case "$agent_runtime_context_endpoint" in unix:///*) ;; *) agent_runtime_die "receipt Docker endpoint is not local" ;; esac
  [ "$agent_runtime_context_socket" = /run/docker.sock ] || agent_runtime_die "receipt Docker socket is not rootful local socket"

  agent_runtime_compose_mode=$(agent_runtime_read_pair "$agent_runtime_manifest" compose_mode)
  case "$agent_runtime_compose_mode" in
    local) agent_runtime_expected_image=$(agent_runtime_read_pair "$agent_runtime_env_file" DIREXTALK_AGENT_IMAGE_LOCAL) ;;
    production) agent_runtime_expected_image=$(agent_runtime_read_pair "$agent_runtime_env_file" DIREXTALK_AGENT_IMAGE_IMMUTABLE) ;;
    *) agent_runtime_die "compose mode is invalid" ;;
  esac
  case "$agent_runtime_expected_image" in
    *$'\n'*) agent_runtime_die "Agent image ref contains a newline" ;;
  esac

  declare -A agent_runtime_ids=() agent_runtime_names=() agent_runtime_services=()
  declare -A agent_runtime_target_status=() agent_runtime_target_health=() agent_runtime_target_image_id=()
  local container_count index id name service project role found_agent=false found_extension=false found_core=false
  container_count=$(agent_runtime_read_pair "$agent_runtime_receipt" container.count)
  printf '%s\n' "$container_count" | grep -Eq '^[1-9][0-9]{0,3}$' || agent_runtime_die "cleanup receipt container count is invalid"
  for ((index = 0; index < container_count; index++)); do
    id=$(agent_runtime_read_pair "$agent_runtime_receipt" "container.$index.id")
    name=$(agent_runtime_read_pair "$agent_runtime_receipt" "container.$index.name")
    service=$(agent_runtime_read_pair "$agent_runtime_receipt" "container.$index.service")
    project=$(agent_runtime_read_pair "$agent_runtime_receipt" "container.$index.project")
    printf '%s\n' "$id" | grep -Eq '^[0-9a-f]{64}$' || agent_runtime_die "receipt container ID is not full length"
    printf '%s\n' "$name" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || agent_runtime_die "receipt container name is invalid"
    printf '%s\n' "$service" | grep -Eq '^[a-z0-9][a-z0-9_-]*$' || agent_runtime_die "receipt container service is invalid"
    [ "$project" = "$agent_runtime_stack" ] || agent_runtime_die "receipt container project is outside this stack"
    case "$service" in
      agent)
        [ "$found_agent" = false ] || agent_runtime_die "receipt contains duplicate agent container"
        found_agent=true; agent_runtime_ids["agent"]=$id; agent_runtime_names["agent"]=$name; agent_runtime_services["agent"]=$service ;;
      extension-runner)
        [ "$found_extension" = false ] || agent_runtime_die "receipt contains duplicate extension-runner container"
        found_extension=true; agent_runtime_ids["extension-runner"]=$id; agent_runtime_names["extension-runner"]=$name; agent_runtime_services["extension-runner"]=$service ;;
      core-runner)
        [ "$found_core" = false ] || agent_runtime_die "receipt contains duplicate core-runner container"
        found_core=true; agent_runtime_ids["core-runner"]=$id; agent_runtime_names["core-runner"]=$name; agent_runtime_services["core-runner"]=$service ;;
    esac
  done
  [ "$found_agent" = true ] && [ "$found_extension" = true ] && [ "$found_core" = true ] || agent_runtime_die "receipt does not contain exactly the three Agent runtime containers"

  command -v docker >/dev/null 2>&1 || agent_runtime_die "docker is required"
  command -v jq >/dev/null 2>&1 || agent_runtime_die "jq is required"
  command -v sha256sum >/dev/null 2>&1 || agent_runtime_die "sha256sum is required"
  agent_runtime_health_timeout_seconds=${DIREXTALK_AGENT_RUNTIME_HEALTH_TIMEOUT_SECONDS:-60}
  printf '%s\n' "$agent_runtime_health_timeout_seconds" | grep -Eq '^[1-9][0-9]{0,3}$' || agent_runtime_die "health timeout must be 1..999 seconds"
  agent_runtime_verify_controls
  agent_runtime_verify_host
  case "$operation" in
    stop) agent_runtime_stop ;;
    restart) agent_runtime_restart ;;
  esac
}
