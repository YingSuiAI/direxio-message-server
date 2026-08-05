#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

# Guarded public-edge cutover.  This script is intentionally narrower than a
# deployment tool: it may stop/start one already-identified Caddy container
# and may create/remove only the new edge project's Caddy container.  It never
# runs `down`, removes a volume, or touches the message/Agent stack databases.

usage() {
  echo "usage: $0 NEW_STACK_ENV EDGE_ENV ACTIVE_EDGE_RECEIPT OUTPUT_RECEIPT" >&2
  exit 2
}

log() {
  printf 'edge cutover: %s\n' "$*" >&2
}

fail() {
  log "$*"
  return 1
}

[ "$#" -eq 4 ] || usage
stack_env=$1
edge_env=$2
active_receipt=$3
output_receipt=$4

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
default_stack_compose=$script_dir/../compose.yaml
default_edge_compose=$script_dir/../edge-compose.yaml
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-edge-cutover.XXXXXX")
scope_lock_file=''
scope_lock_fd=''
txn_dir=$(dirname -- "$active_receipt")/.cutover-edge-txn
txn_journal=$txn_dir/journal
cleanup() {
  if [ -n "${scope_lock_fd:-}" ]; then
    "$flock_bin" -u "$scope_lock_fd" 2>/dev/null || true
    eval "exec ${scope_lock_fd}>&-" 2>/dev/null || true
  fi
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT

docker_bin=${DIREXTALK_DOCKER_BIN:-docker}
curl_bin=${DIREXTALK_CURL_BIN:-curl}
jq_bin=${DIREXTALK_JQ_BIN:-jq}
flock_bin=${DIREXTALK_FLOCK_BIN:-flock}

bound_host_engine=''
bound_stack_env=''
bound_edge_env=''
bound_active_receipt=''
bound_stack_compose=''
bound_edge_compose=''
bound_caddyfile=''
bound_message_tls_cert=''
current_image_id=''
current_repo_digest=''
current_network_id=''
current_network_labels_hash=''
current_data_volume_fingerprint=''
current_config_volume_fingerprint=''
bound_output_parent=''
receipt_kind=''
receipt_operation=''
receipt_revision=''
candidate_image_id=''
candidate_repo_digest=''
candidate_network_id=''
candidate_network_labels_hash=''
message_network_id=''
message_network_labels_hash=''
candidate_data_volume_fingerprint=''
candidate_config_volume_fingerprint=''

require_regular_owned() {
  local file=$1 mode=${2:-}
  [ -f "$file" ] && [ ! -L "$file" ] || fail "file must be a regular non-symlink: $file" || return 1
  [ "$(stat -c '%u' "$file")" = "$(id -u)" ] || fail "file owner mismatch: $file" || return 1
  if [ -n "$mode" ]; then
    [ "$(stat -c '%a' "$file")" = "$mode" ] || fail "file mode must be $mode: $file" || return 1
  fi
}

require_dir_owned() {
  local dir=$1 mode=${2:-}
  [ -d "$dir" ] && [ ! -L "$dir" ] || fail "directory must be a regular non-symlink: $dir" || return 1
  [ "$(stat -c '%u' "$dir")" = "$(id -u)" ] || fail "directory owner mismatch: $dir" || return 1
  if [ -n "$mode" ]; then
    [ "$(stat -c '%a' "$dir")" = "$mode" ] || fail "directory mode must be $mode: $dir" || return 1
  fi
}

dir_binding() {
  local dir=$1
  require_dir_owned "$dir" 700 || return 1
  printf '%s|%s|%s|%s' \
    "$(stat -c '%d' "$dir")" "$(stat -c '%i' "$dir")" \
    "$(stat -c '%u' "$dir")" "$(stat -c '%a' "$dir")"
}

acquire_scope_lock() {
  local scope_dir
  scope_dir=$(dirname -- "$active_receipt")
  require_dir_owned "$scope_dir" 700 || return 1
  scope_lock_file=$scope_dir/.dirextalk-edge-public.lock
  if [ -e "$scope_lock_file" ] || [ -L "$scope_lock_file" ]; then
    require_regular_owned "$scope_lock_file" 600 || return 1
  else
    umask 077
    : >"$scope_lock_file" || return 1
    chmod 600 "$scope_lock_file" || return 1
  fi
  require_regular_owned "$scope_lock_file" 600 || return 1
  exec {scope_lock_fd}>>"$scope_lock_file" || return 1
  if ! "$flock_bin" -n "$scope_lock_fd"; then
    eval "exec ${scope_lock_fd}>&-" 2>/dev/null || true
    scope_lock_fd=''
    fail "another public-edge operation is in progress" || return 1
  fi
  if [ "${DIREXTALK_CUTOVER_TEST_BLOCK_AFTER_LOCK:-false}" = true ]; then
    local lock_ready=${DIREXTALK_CUTOVER_TEST_LOCK_READY_FILE:-}
    local lock_release=${DIREXTALK_CUTOVER_TEST_LOCK_RELEASE_FILE:-}
    [ -n "$lock_ready" ] && : >"$lock_ready"
    if [ -n "$lock_release" ]; then
      while [ ! -e "$lock_release" ]; do :; done
    fi
  fi
}

transaction_exists() {
  [ -e "$txn_dir" ] || [ -L "$txn_dir" ]
}

sha256_file() {
  sha256sum -- "$1" | awk '{print $1}'
}

file_binding() {
  local file=$1
  require_regular_owned "$file" || return 1
  printf '%s|%s|%s|%s|%s' \
    "$(stat -c '%d' "$file")" "$(stat -c '%i' "$file")" \
    "$(stat -c '%u' "$file")" "$(stat -c '%a' "$file")" "$(sha256_file "$file")"
}

host_engine_binding() {
  local machine_file=${DIREXTALK_MACHINE_ID_FILE:-/etc/machine-id}
  local host_name machine_id engine_id
  [ -f "$machine_file" ] && [ ! -L "$machine_file" ] || fail "machine identity file is unavailable" || return 1
  host_name=$(hostname 2>/dev/null || true)
  machine_id=$(tr -d '[:space:]' <"$machine_file") || return 1
  engine_id=$("$docker_bin" info --format '{{.ID}}' 2>/dev/null | tr -d '[:space:]') || return 1
  [ -n "$host_name" ] && [ -n "$machine_id" ] && [ -n "$engine_id" ] || fail "host or Docker Engine identity is empty" || return 1
  printf '%s|%s|%s' "$host_name" "$machine_id" "$engine_id"
}

image_inspect_json() {
  local image=$1 out=$2
  "$docker_bin" image inspect "$image" >"$out" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1' "$out" >/dev/null 2>&1
}

network_inspect_json() {
  local network=$1 out=$2
  "$docker_bin" network inspect "$network" >"$out" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1' "$out" >/dev/null 2>&1
}

volume_inspect_json() {
  local volume=$1 out=$2
  "$docker_bin" volume inspect "$volume" >"$out" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1' "$out" >/dev/null 2>&1
}

image_id_from_json() { "$jq_bin" -r '.[0].Id // .[0].ID // ""' "$1"; }
repo_digest_from_json() {
  "$jq_bin" -r --arg image "$1" '(.[0].RepoDigests // []) | map(select(type == "string" and contains("@sha256:"))) | map(select(split("@sha256:")[1] == ($image | split("@sha256:")[1]))) | .[0] // ""' "$2"
}
network_id_from_json() { "$jq_bin" -r '.[0].Id // .[0].ID // ""' "$1"; }
network_labels_hash() { "$jq_bin" -c '.[0].Labels // {} | to_entries | sort_by(.key)' "$1" | sha256sum | awk '{print $1}'; }
volume_fingerprint_from_json() {
  local file=$1 normalized
  if normalized=$("$jq_bin" -c -e '
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
  ' "$file"); then
    :
  else
    return 1
  fi
  printf '%s' "$normalized" | sha256sum | awk '{print $1}'
}

verify_hardened_candidate() {
  local file=$1
  "$jq_bin" -e '.[0].HostConfig.ReadonlyRootfs == true and ((.[0].HostConfig.CapDrop // []) | map(ascii_upcase) | index("ALL")) != null and ((.[0].HostConfig.SecurityOpt // []) | map(ascii_downcase) | any(. == "no-new-privileges:true" or . == "no-new-privileges")) and ((.[0].Config.Healthcheck.Test // []) | length) > 1 and ((.[0].Config.Healthcheck.Test // [""])[0] == "CMD-SHELL") and ((.[0].Config.Healthcheck.Test // [] | join(" ")) | test("wget|curl"))' "$file" >/dev/null 2>&1
}

# Read one literal KEY=VALUE entry without sourcing untrusted deployment
# files.  Duplicate keys are rejected; values are never echoed by this tool.
read_kv() {
  local file=$1 key=$2 count value
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && $0 !~ /^[[:space:]]*$/ && $1 == wanted { count++ } END { print count + 0 }' "$file") || return 1
  [ "$count" -eq 1 ] || fail "$file must contain exactly one $key entry" || return 1
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && $0 !~ /^[[:space:]]*$/ && $1 == wanted { print substr($0, length(wanted) + 2); exit }' "$file") || return 1
  [ -n "$value" ] || fail "$key is empty" || return 1
  printf '%s' "$value"
}

optional_kv() {
  local file=$1 key=$2 count value
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && $0 !~ /^[[:space:]]*$/ && $1 == wanted { count++ } END { print count + 0 }' "$file") || return 1
  [ "$count" -le 1 ] || fail "$file contains duplicate $key" || return 1
  [ "$count" -eq 1 ] || return 0
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && $0 !~ /^[[:space:]]*$/ && $1 == wanted { print substr($0, length(wanted) + 2); exit }' "$file") || return 1
  [ -n "$value" ] || fail "$key is empty" || return 1
  printf '%s' "$value"
}

valid_project() {
  printf '%s' "$1" | grep -Eq '^[a-z0-9][a-z0-9_-]{0,62}$'
}

valid_name() {
  printf '%s' "$1" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$'
}

valid_domain() {
  printf '%s' "$1" | grep -Eq '^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?))+$'
}

valid_container_id() {
  printf '%s' "$1" | grep -Eq '^[0-9a-f]{12,64}$' && [ "${1//0/}" ]
}

valid_full_container_id() {
  printf '%s' "$1" | grep -Eq '^[0-9a-f]{64}$' && [ "${1//0/}" ]
}

valid_image() {
  local value=$1 digest repository canonical
  printf '%s' "$value" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' || return 1
  case "$value" in
    *registry.invalid*|*@sha256:0000000000000000000000000000000000000000000000000000000000000000) return 1 ;;
  esac
  repository=${value%%@sha256:*}
  case "$repository" in
    docker.io/*) canonical=${repository#docker.io/} ;;
    */*)
      case "${repository%%/*}" in
        *.*|*:*|localhost) return 1 ;;
        *) canonical=$repository ;;
      esac
      ;;
    *) canonical="library/$repository" ;;
  esac
  canonical=${canonical%:*}
  case "$canonical" in
    dirextalk/message-server|dirextalk/agent|library/caddy) ;;
    *) return 1 ;;
  esac
  digest=${value##*@sha256:}
  [ "${digest//0/}" ]
}

valid_port() {
  printf '%s' "$1" | grep -Eq '^[0-9]+$' && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

inspect_container() {
  local id=$1 file=$2
  "$docker_bin" inspect "$id" >"$file" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1 and (.[0].Id | type == "string")' "$file" >/dev/null 2>&1 || return 1
}

container_id_from_inspect() {
  "$jq_bin" -r '.[0].Id' "$1"
}

container_health() {
  "$jq_bin" -r '.[0].State.Health.Status // ""' "$1"
}

container_image() {
  "$jq_bin" -r '.[0].Config.Image // ""' "$1"
}

verify_container_labels() {
  local file=$1 project=$2 service=$3
  "$jq_bin" -e --arg project "$project" --arg service "$service" '
    .[0].Config.Labels["com.docker.compose.project"] == $project and
    .[0].Config.Labels["com.docker.compose.service"] == $service
  ' "$file" >/dev/null 2>&1
}

verify_container_network_and_mounts() {
  local file=$1 network=$2 data_volume=$3 config_volume=$4
  "$jq_bin" -e --arg network "$network" --arg data "$data_volume" --arg config "$config_volume" '
    (.[0].NetworkSettings.Networks | keys) == [$network] and
    ([.[0].Mounts[] | select(.Destination == "/data" and .Type == "volume" and .Name == $data)] | length) == 1 and
    ([.[0].Mounts[] | select(.Destination == "/config" and .Type == "volume" and .Name == $config)] | length) == 1
  ' "$file" >/dev/null 2>&1
}

verify_container_ports() {
  local file=$1
  "$jq_bin" -e '
    ([.[0].HostConfig.PortBindings["80/tcp"][]?.HostPort] | sort | unique) == ["80"] and
    ([.[0].HostConfig.PortBindings["443/tcp"][]?.HostPort] | sort | unique) == ["443"]
  ' "$file" >/dev/null 2>&1
}

verify_healthy_running_container() {
  local file=$1
  "$jq_bin" -e '.[0].State.Status == "running" and .[0].State.Health.Status == "healthy"' "$file" >/dev/null 2>&1
}

verify_loopback_message_ports() {
  local file=$1 http_port=$2 https_port=$3
  "$jq_bin" -e --arg http "$http_port" --arg https "$https_port" '
    ([.[0].HostConfig.PortBindings["8008/tcp"][]? | {port: .HostPort, ip: (.HostIp // "")}] | length == 1 and .[0].port == $http and .[0].ip == "127.0.0.1") and
    ([.[0].HostConfig.PortBindings["8448/tcp"][]? | {port: .HostPort, ip: (.HostIp // "")}] | length == 1 and .[0].port == $https and .[0].ip == "127.0.0.1")
  ' "$file" >/dev/null 2>&1
}

verify_network_contains() {
  local network=$1 container_id=$2 expected_project=${3:-}
  local file
  file=$tmp_dir/network-$(printf '%s' "$network" | tr -c 'A-Za-z0-9_.-' '_').json
  "$docker_bin" network inspect "$network" >"$file" 2>/dev/null || return 1
  if [ -n "$expected_project" ]; then
    "$jq_bin" -e --arg network "$network" --arg container "$container_id" --arg project "$expected_project" '
      .[0].Name == $network and
      .[0].Labels["com.docker.compose.project"] == $project and
      .[0].Containers[$container] != null
    ' "$file" >/dev/null 2>&1 || return 1
  else
    "$jq_bin" -e --arg network "$network" --arg container "$container_id" '
      .[0].Name == $network and .[0].Containers[$container] != null
    ' "$file" >/dev/null 2>&1 || return 1
  fi
}

verify_old_identity() {
  local file=$1 expected_id=$2 project=$3 image=$4 network=$5 data_volume=$6 config_volume=$7
  valid_container_id "$expected_id" || return 1
  "$jq_bin" -e --arg id "$expected_id" --arg project "$project" --arg image "$image" \
    --arg network "$network" --arg data "$data_volume" --arg config "$config_volume" '
      .[0].Id == $id and
      .[0].Config.Image == $image and
      .[0].Config.Labels["com.docker.compose.project"] == $project and
      .[0].Config.Labels["com.docker.compose.service"] == "caddy" and
      (.[0].NetworkSettings.Networks | keys) == [$network] and
      ([.[0].Mounts[] | select(.Destination == "/data" and .Type == "volume" and .Name == $data)] | length) == 1 and
      ([.[0].Mounts[] | select(.Destination == "/config" and .Type == "volume" and .Name == $config)] | length) == 1 and
      ([.[0].HostConfig.PortBindings["80/tcp"][]?.HostPort] | sort | unique) == ["80"] and
      ([.[0].HostConfig.PortBindings["443/tcp"][]?.HostPort] | sort | unique) == ["443"] and
      .[0].State.Status == "running" and .[0].State.Health.Status == "healthy"
    ' "$file" >/dev/null 2>&1
}

verify_new_identity() {
  local file=$1 expected_id=$2 project=$3 image=$4 network=$5 data_volume=$6 config_volume=$7
  verify_old_identity "$file" "$expected_id" "$project" "$image" "$network" "$data_volume" "$config_volume"
}

verify_new_identity_without_health() {
  local file=$1 expected_id=$2 project=$3 image=$4 network=$5 data_volume=$6 config_volume=$7
  valid_container_id "$expected_id" || return 1
  "$jq_bin" -e --arg id "$expected_id" --arg project "$project" --arg image "$image" \
    --arg network "$network" --arg data "$data_volume" --arg config "$config_volume" '
      .[0].Id == $id and
      .[0].Config.Image == $image and
      .[0].Config.Labels["com.docker.compose.project"] == $project and
      .[0].Config.Labels["com.docker.compose.service"] == "caddy" and
      (.[0].NetworkSettings.Networks | keys) == [$network] and
      ([.[0].Mounts[] | select(.Destination == "/data" and .Type == "volume" and .Name == $data)] | length) == 1 and
      ([.[0].Mounts[] | select(.Destination == "/config" and .Type == "volume" and .Name == $config)] | length) == 1 and
      ([.[0].HostConfig.PortBindings["80/tcp"][]?.HostPort] | sort | unique) == ["80"] and
      ([.[0].HostConfig.PortBindings["443/tcp"][]?.HostPort] | sort | unique) == ["443"]
    ' "$file" >/dev/null 2>&1
}

compose_stack() {
  "$docker_bin" compose --env-file "$stack_env" -f "$stack_compose" "$@"
}

compose_edge() {
  "$docker_bin" compose --env-file "$edge_env" -f "$edge_compose" "$@"
}

read_and_validate_inputs() {
  require_regular_owned "$stack_env" 400 || return 1
  require_regular_owned "$edge_env" 400 || return 1
  require_regular_owned "$active_receipt" 400 || return 1
  output_parent=$(dirname -- "$output_receipt")
  require_dir_owned "$output_parent" 700 || return 1
  if transaction_exists; then
    [ -d "$txn_dir" ] && [ ! -L "$txn_dir" ] || fail "cutover transaction path is not a private directory" || return 1
  else
    [ ! -e "$output_receipt" ] && [ ! -L "$output_receipt" ] || fail "output receipt must be a new non-symlink path" || return 1
  fi
  bound_output_parent=$(dir_binding "$output_parent") || return 1

  stack_name=$(read_kv "$stack_env" DIREXTALK_SPLIT_STACK_NAME) || return 1
  message_network=$(read_kv "$stack_env" DIREXTALK_MESSAGE_PUBLIC_NETWORK) || return 1
  message_private_network=$(read_kv "$stack_env" DIREXTALK_MESSAGE_PRIVATE_NETWORK) || return 1
  agent_private_network=$(read_kv "$stack_env" DIREXTALK_AGENT_PRIVATE_NETWORK) || return 1
  message_http_port=$(read_kv "$stack_env" DIREXTALK_MESSAGE_HTTP_BIND) || return 1
  message_https_port=$(read_kv "$stack_env" DIREXTALK_MESSAGE_HTTPS_BIND) || return 1
  message_server_name=$(read_kv "$stack_env" DIREXTALK_MESSAGE_SERVER_NAME) || return 1
  message_tls_mode=$(read_kv "$stack_env" DIREXTALK_MESSAGE_TLS_MODE) || return 1
  message_tls_cert=$(read_kv "$stack_env" DIREXTALK_MESSAGE_TLS_CERT_FILE) || return 1
  message_image=$(read_kv "$stack_env" DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE) || return 1
  agent_image=$(read_kv "$stack_env" DIREXTALK_AGENT_IMAGE_IMMUTABLE) || return 1
  stack_compose=$(optional_kv "$stack_env" DIREXTALK_MESSAGE_COMPOSE_FILE) || return 1
  [ -n "${stack_compose:-}" ] || stack_compose=$default_stack_compose

  edge_project=$(read_kv "$edge_env" DIREXTALK_EDGE_STACK_NAME) || return 1
  edge_domain=$(read_kv "$edge_env" DIREXTALK_PUBLIC_DOMAIN) || return 1
  edge_network=$(read_kv "$edge_env" DIREXTALK_MESSAGE_PUBLIC_NETWORK) || return 1
  caddy_image=$(read_kv "$edge_env" DIREXTALK_CADDY_IMAGE_IMMUTABLE) || return 1
  caddy_data_volume=$(read_kv "$edge_env" DIREXTALK_CADDY_DATA_VOLUME) || return 1
  caddy_config_volume=$(read_kv "$edge_env" DIREXTALK_CADDY_CONFIG_VOLUME) || return 1
  caddyfile=$(read_kv "$edge_env" DIREXTALK_CADDYFILE) || return 1
  edge_compose=$(optional_kv "$edge_env" DIREXTALK_EDGE_COMPOSE_FILE) || return 1
  [ -n "${edge_compose:-}" ] || edge_compose=$default_edge_compose
  public_ca=$(optional_kv "$edge_env" DIREXTALK_CADDY_CA_FILE) || return 1
  well_known_path=$(optional_kv "$edge_env" DIREXTALK_WELL_KNOWN_PATH) || return 1
  [ -n "${well_known_path:-}" ] || well_known_path=/.well-known/matrix/server
  public_health_path=$(optional_kv "$edge_env" DIREXTALK_PUBLIC_HEALTH_PATH) || return 1
  [ -n "${public_health_path:-}" ] || public_health_path=/_p2p/health

  valid_project "$stack_name" || fail "invalid message stack project name" || return 1
  valid_project "$edge_project" || fail "invalid edge stack project name" || return 1
  [ "$stack_name" != "$edge_project" ] || fail "message and edge Compose projects must be distinct" || return 1
  valid_name "$message_network" && valid_name "$edge_network" || fail "invalid public network name" || return 1
  [ "$message_network" = "$edge_network" ] || fail "edge and message public network names differ" || return 1
  valid_name "$message_private_network" && valid_name "$agent_private_network" || fail "invalid private network name" || return 1
  valid_port "$message_http_port" && valid_port "$message_https_port" || fail "message host ports are invalid" || return 1
  [ "$message_http_port" != "$message_https_port" ] || fail "message HTTP and HTTPS host ports must differ" || return 1
  [ "$message_tls_mode" = external ] || fail "message TLS mode must be external" || return 1
  valid_domain "$edge_domain" || fail "public domain is invalid" || return 1
  [ "$message_server_name" = "$edge_domain" ] || fail "message TLS server name must equal public domain" || return 1
  valid_image "$message_image" || fail "message-server image is not an immutable digest" || return 1
  valid_image "$agent_image" || fail "Agent image is not an immutable digest" || return 1
  valid_image "$caddy_image" || fail "Caddy image is not an immutable digest" || return 1
  valid_name "$caddy_data_volume" && valid_name "$caddy_config_volume" || fail "invalid Caddy volume name" || return 1
  case "$well_known_path:$public_health_path" in
    /*:/*) ;;
    *) fail "public endpoint paths must be absolute" || return 1 ;;
  esac
  require_regular_owned "$stack_compose" || return 1
  require_regular_owned "$edge_compose" || return 1
  require_regular_owned "$caddyfile" || return 1
  [ -f "$message_tls_cert" ] && [ ! -L "$message_tls_cert" ] || fail "message TLS certificate must be a regular non-symlink file" || return 1
  [ "$(stat -c '%u' "$message_tls_cert")" = "$(id -u)" ] || fail "message TLS certificate owner mismatch" || return 1
  if [ -n "${public_ca:-}" ]; then
    require_regular_owned "$public_ca" || return 1
  fi
  grep -Eq '(^|[[:space:]])reverse_proxy[[:space:]]+message-server:(8008|8448)([[:space:]]|$)' "$caddyfile" || fail "Caddyfile must proxy to message-server" || return 1

  [ "$(sed -n '1p' "$active_receipt")" = '# dirextalk-edge-receipt-v1' ] || fail "active receipt header is invalid" || return 1
  receipt_kind=$(read_kv "$active_receipt" receipt_kind) || return 1
  [ "$receipt_kind" = adopted-edge-v1 ] || fail "active receipt is not a compliant adoption receipt" || return 1
  receipt_operation=$(read_kv "$active_receipt" operation_id) || return 1
  receipt_revision=$(read_kv "$active_receipt" revision) || return 1
  receipt_host_name=$(read_kv "$active_receipt" host_name) || return 1
  receipt_machine_id=$(read_kv "$active_receipt" machine_id) || return 1
  receipt_engine_id=$(read_kv "$active_receipt" docker_engine_id) || return 1
  owner_uid=$(read_kv "$active_receipt" owner_uid) || return 1
  [ "$owner_uid" = "$(id -u)" ] || fail "active receipt owner mismatch" || return 1
  receipt_edge_project=$(read_kv "$active_receipt" edge_stack_name) || return 1
  receipt_message_stack=$(read_kv "$active_receipt" message_stack_name) || return 1
  receipt_domain=$(read_kv "$active_receipt" domain) || return 1
  receipt_network=$(read_kv "$active_receipt" message_public_network) || return 1
  [ "$receipt_domain" = "$edge_domain" ] || fail "active receipt domain mismatch" || return 1
  valid_project "$receipt_message_stack" || fail "active receipt message stack is invalid" || return 1
  [ "$receipt_message_stack" != "$stack_name" ] || fail "fresh message stack must differ from the active receipt stack" || return 1
  valid_name "$receipt_network" || fail "active receipt public network is invalid" || return 1
  current_id=$(read_kv "$active_receipt" active_caddy_container_id) || return 1
  current_project=$(read_kv "$active_receipt" active_caddy_project) || return 1
  current_image=$(read_kv "$active_receipt" active_caddy_image) || return 1
  current_data_volume=$(read_kv "$active_receipt" active_caddy_data_volume) || return 1
  current_config_volume=$(read_kv "$active_receipt" active_caddy_config_volume) || return 1
  current_network=$(read_kv "$active_receipt" active_caddy_network) || return 1
  current_http_port=$(read_kv "$active_receipt" active_caddy_http_port) || return 1
  current_https_port=$(read_kv "$active_receipt" active_caddy_https_port) || return 1
  current_image_id=$(read_kv "$active_receipt" active_caddy_image_id) || return 1
  current_repo_digest=$(read_kv "$active_receipt" active_caddy_repo_digest) || return 1
  current_network_id=$(read_kv "$active_receipt" active_caddy_network_id) || return 1
  current_network_labels_hash=$(read_kv "$active_receipt" active_caddy_network_labels_sha256) || return 1
  current_data_volume_fingerprint=$(read_kv "$active_receipt" active_caddy_data_volume_fingerprint_sha256) || return 1
  current_config_volume_fingerprint=$(read_kv "$active_receipt" active_caddy_config_volume_fingerprint_sha256) || return 1
  valid_full_container_id "$current_id" || fail "active receipt Caddy container ID must be a full immutable ID" || return 1
  valid_project "$current_project" || fail "active receipt Caddy project is invalid" || return 1
  [ "$receipt_edge_project" = "$current_project" ] || fail "active receipt edge project does not match active Caddy project" || return 1
  valid_image "$current_image" || fail "active receipt Caddy image is not immutable" || return 1
  valid_name "$current_data_volume" && valid_name "$current_config_volume" || fail "active receipt Caddy volume is invalid" || return 1
  valid_name "$current_network" || fail "active receipt Caddy network is invalid" || return 1
  [ "$current_network" = "$receipt_network" ] || fail "active receipt Caddy/network binding mismatch" || return 1
  [ "$current_network" != "$message_network" ] || fail "fresh message stack must use a new public network" || return 1
  [ "$current_project" != "$edge_project" ] || fail "new edge Compose project must differ from the active project" || return 1
  [ "$caddy_data_volume" = "$current_data_volume" ] || fail "Caddy data volume must be reused exactly" || return 1
  [ "$caddy_config_volume" = "$current_config_volume" ] || fail "Caddy config volume must be reused exactly" || return 1
  [ "$current_http_port" = 80 ] && [ "$current_https_port" = 443 ] || fail "active receipt Caddy must own ports 80 and 443" || return 1

  bound_host_engine=$(host_engine_binding) || return 1
  receipt_identity="$receipt_host_name|$receipt_machine_id|$receipt_engine_id"
  [ "$receipt_identity" = "$bound_host_engine" ] || fail "active receipt host/Engine identity mismatch" || return 1
  bound_stack_env=$(file_binding "$stack_env") || return 1
  bound_edge_env=$(file_binding "$edge_env") || return 1
  bound_active_receipt=$(file_binding "$active_receipt") || return 1
  bound_stack_compose=$(file_binding "$stack_compose") || return 1
  bound_edge_compose=$(file_binding "$edge_compose") || return 1
  bound_caddyfile=$(file_binding "$caddyfile") || return 1
  bound_message_tls_cert=$(file_binding "$message_tls_cert") || return 1
}

render_and_verify_compose() {
  stack_render=$tmp_dir/message-compose.json
  edge_render=$tmp_dir/edge-compose.json
  compose_stack config --quiet >/dev/null 2>&1 || fail "new message stack compose render failed" || return 1
  compose_stack config --format json >"$stack_render" 2>/dev/null || fail "new message stack JSON render failed" || return 1
  compose_edge config --quiet >/dev/null 2>&1 || fail "edge compose render failed" || return 1
  compose_edge config --format json >"$edge_render" 2>/dev/null || fail "edge JSON render failed" || return 1
  "$jq_bin" -e --arg stack "$stack_name" --arg message "$message_image" --arg agent "$agent_image" '
    .name == $stack and .services["message-server"].image == $message and
    .services.agent.image == $agent and
    ([.services["message-server"].ports[] | .host_ip] | unique) == ["127.0.0.1"]
  ' "$stack_render" >/dev/null 2>&1 || fail "rendered new stack identity/image mismatch" || return 1
  "$jq_bin" -e --arg project "$edge_project" --arg image "$caddy_image" --arg network "$message_network" \
    --arg data "$caddy_data_volume" --arg config "$caddy_config_volume" '
      .name == $project and .services.caddy.image == $image and
      (.services.caddy.networks == ["message_public"] or (.services.caddy.networks | keys) == ["message_public"]) and
      ([.services.caddy.ports[] | .published] | sort) == ["443", "80"] and
      .networks.message_public.name == $network and .networks.message_public.external == true and
      .volumes.caddy_data.name == $data and .volumes.caddy_data.external == true and
      .volumes.caddy_config.name == $config and .volumes.caddy_config.external == true and
      .services.caddy.read_only == true and
      (.services.caddy.cap_drop | index("ALL")) != null and
      (.services.caddy.security_opt | index("no-new-privileges:true")) != null and
      ((.services.caddy.healthcheck.test // []) | length) > 1 and
      ((.services.caddy.healthcheck.test | join(" ")) | test("wget|curl")) and
      ([.services.caddy.volumes[] | select(.target == "/data" and .type == "volume" and .source == "caddy_data")] | length) == 1 and
      ([.services.caddy.volumes[] | select(.target == "/config" and .type == "volume" and .source == "caddy_config")] | length) == 1
    ' "$edge_render" >/dev/null 2>&1 || fail "rendered edge does not satisfy the hardened topology" || return 1
}

verify_message_stack() {
  message_id=$(compose_stack ps -q message-server 2>/dev/null) || fail "new message-server container is not discoverable" || return 1
  agent_id=$(compose_stack ps -q agent 2>/dev/null) || fail "new Agent container is not discoverable" || return 1
  [ -n "$message_id" ] && [ -n "$agent_id" ] || fail "new stack returned an empty service container ID" || return 1
  message_inspect=$tmp_dir/message.inspect.json
  agent_inspect=$tmp_dir/agent.inspect.json
  inspect_container "$message_id" "$message_inspect" || fail "new message-server container inspect failed" || return 1
  inspect_container "$agent_id" "$agent_inspect" || fail "new Agent container inspect failed" || return 1
  message_full_id=$(container_id_from_inspect "$message_inspect")
  agent_full_id=$(container_id_from_inspect "$agent_inspect")
  case "$message_full_id" in "$message_id"*) ;; *) fail "message-server container ID changed during inspect" || return 1 ;; esac
  case "$agent_full_id" in "$agent_id"*) ;; *) fail "Agent container ID changed during inspect" || return 1 ;; esac
  verify_container_labels "$message_inspect" "$stack_name" message-server || fail "message-server Compose identity mismatch" || return 1
  verify_container_labels "$agent_inspect" "$stack_name" agent || fail "Agent Compose identity mismatch" || return 1
  [ "$(container_image "$message_inspect")" = "$message_image" ] || fail "message-server image identity mismatch" || return 1
  [ "$(container_image "$agent_inspect")" = "$agent_image" ] || fail "Agent image identity mismatch" || return 1
  verify_loopback_message_ports "$message_inspect" "$message_http_port" "$message_https_port" || fail "message-server host ports must be loopback-only" || return 1
  verify_healthy_running_container "$message_inspect" || fail "new message-server is not healthy" || return 1
  verify_healthy_running_container "$agent_inspect" || fail "new Agent is not healthy" || return 1
  verify_network_contains "$message_network" "$message_full_id" "$stack_name" || fail "new public network identity mismatch" || return 1
  verify_network_contains "$agent_private_network" "$agent_full_id" "$stack_name" || fail "new Agent private network identity mismatch" || return 1
  message_network_json=$tmp_dir/message-public-network.json
  network_inspect_json "$message_network" "$message_network_json" || return 1
  message_network_id=$(network_id_from_json "$message_network_json")
  message_network_labels_hash=$(network_labels_hash "$message_network_json")
  [ -n "$message_network_id" ] || return 1
}

verify_message_host() {
  "$curl_bin" --fail --silent --show-error --max-time 15 \
    "http://127.0.0.1:${message_http_port}/_p2p/health" >/dev/null 2>&1 || fail "new message-server HTTP health check failed" || return 1
  [ -s "$message_tls_cert" ] || fail "new message-server TLS certificate is empty" || return 1
  "$curl_bin" --fail --silent --show-error --max-time 15 --resolve "${message_server_name}:${message_https_port}:127.0.0.1" \
    --cacert "$message_tls_cert" "https://${message_server_name}:${message_https_port}/_p2p/health" >/dev/null 2>&1 || fail "new message-server TLS health check failed" || return 1
}

verify_active_caddy() {
  old_inspect=$tmp_dir/active-caddy.inspect.json
  inspect_container "$current_id" "$old_inspect" || fail "active Caddy container cannot be inspected" || return 1
  inspected_current_id=$(container_id_from_inspect "$old_inspect")
  [ "$inspected_current_id" = "$current_id" ] || fail "active Caddy container ID no longer matches receipt" || return 1
  verify_old_identity "$old_inspect" "$current_id" "$current_project" "$current_image" "$current_network" "$current_data_volume" "$current_config_volume" || fail "active Caddy identity/health/ports/mounts check failed" || return 1
  [ "$("$jq_bin" -r '.[0].Image // ""' "$old_inspect")" = "$current_image_id" ] || fail "active Caddy image ID no longer matches receipt" || return 1
  verify_network_contains "$current_network" "$current_id" || fail "active Caddy is not attached to the recorded network" || return 1
  old_id=$current_id
  old_project=$current_project
  old_image=$current_image
  old_data_volume=$current_data_volume
  old_config_volume=$current_config_volume
}

verify_receipt_object_chain() {
  local image_json_file network_json_file data_json_file config_json_file actual_image_id actual_repo
  image_json_file=$tmp_dir/active-image.json
  network_json_file=$tmp_dir/active-network.json
  data_json_file=$tmp_dir/active-data-volume.json
  config_json_file=$tmp_dir/active-config-volume.json
  image_inspect_json "$current_image" "$image_json_file" || fail "active receipt image inspect failed" || return 1
  actual_image_id=$(image_id_from_json "$image_json_file")
  actual_repo=$(repo_digest_from_json "$current_image" "$image_json_file")
  [ "$actual_image_id" = "$current_image_id" ] && [ "$actual_repo" = "$current_repo_digest" ] || fail "active receipt image ID/RepoDigest mismatch" || return 1
  network_inspect_json "$current_network" "$network_json_file" || return 1
  [ "$(network_id_from_json "$network_json_file")" = "$current_network_id" ] || fail "active receipt network object mismatch" || return 1
  [ "$(network_labels_hash "$network_json_file")" = "$current_network_labels_hash" ] || fail "active receipt network labels mismatch" || return 1
  volume_inspect_json "$current_data_volume" "$data_json_file" || return 1
  volume_inspect_json "$current_config_volume" "$config_json_file" || return 1
  [ "$(volume_fingerprint_from_json "$data_json_file")" = "$current_data_volume_fingerprint" ] || fail "active receipt data volume mismatch" || return 1
  [ "$(volume_fingerprint_from_json "$config_json_file")" = "$current_config_volume_fingerprint" ] || fail "active receipt config volume mismatch" || return 1
}

verify_no_new_edge_container() {
  local ids
  ids=$(compose_edge ps -aq caddy 2>/dev/null) || fail "cannot inspect the new edge Compose project" || return 1
  [ -z "$ids" ] || fail "new edge Compose project already contains a Caddy container" || return 1
}

verify_public() {
  local curl_common=(--fail --silent --show-error --max-time 20 --resolve "${edge_domain}:443:127.0.0.1")
  if [ -n "${public_ca:-}" ]; then
    curl_common+=(--cacert "$public_ca")
  fi
  "$curl_bin" "${curl_common[@]}" --path-as-is "https://${edge_domain}${public_health_path}" >/dev/null 2>&1 || return 1
  "$curl_bin" "${curl_common[@]}" --path-as-is "https://${edge_domain}${well_known_path}" >/dev/null 2>&1 || return 1
}

verify_new_caddy() {
  new_id=$(compose_edge ps -q caddy 2>/dev/null) || fail "new Caddy container is not discoverable" || return 1
  [ -n "$new_id" ] || fail "new Caddy Compose start returned an empty container ID" || return 1
  new_inspect=$tmp_dir/new-caddy.inspect.json
  inspect_container "$new_id" "$new_inspect" || fail "new Caddy inspect failed" || return 1
  new_full_id=$(container_id_from_inspect "$new_inspect")
  case "$new_full_id" in "$new_id"*) ;; *) fail "new Caddy container ID changed during inspect" || return 1 ;; esac
  verify_new_identity "$new_inspect" "$new_full_id" "$edge_project" "$caddy_image" "$message_network" "$caddy_data_volume" "$caddy_config_volume" || fail "new Caddy identity/health/ports/mounts check failed" || return 1
  verify_network_contains "$message_network" "$new_full_id" || fail "new Caddy is not attached to the fresh public message network" || return 1
  new_id=$new_full_id
}

create_new_caddy() {
  local short_id inspect_file image_file network_file data_file config_file actual_id actual_repo
  compose_edge create caddy >/dev/null 2>&1 || fail "new Caddy pre-create failed" || return 1
  short_id=$(compose_edge ps -aq caddy 2>/dev/null) || fail "new Caddy candidate ID is unavailable" || return 1
  valid_container_id "$short_id" || fail "new Caddy candidate ID is invalid" || return 1
  inspect_file=$tmp_dir/new-caddy-pre.inspect.json
  inspect_container "$short_id" "$inspect_file" || fail "new Caddy candidate inspect failed" || return 1
  new_id=$(container_id_from_inspect "$inspect_file")
  valid_full_container_id "$new_id" || fail "new Caddy candidate ID is not full" || return 1
  case "$new_id" in "$short_id"*) ;; *) fail "new Caddy candidate ID changed during inspect" || return 1 ;; esac
  verify_new_identity_without_health "$inspect_file" "$new_id" "$edge_project" "$caddy_image" "$message_network" "$caddy_data_volume" "$caddy_config_volume" || fail "new Caddy candidate identity mismatch" || return 1
  verify_hardened_candidate "$inspect_file" || fail "new Caddy candidate hardening/healthcheck mismatch" || return 1
  candidate_image_id=$("$jq_bin" -r '.[0].Image // ""' "$inspect_file")
  image_file=$tmp_dir/new-caddy-image.json
  image_inspect_json "$caddy_image" "$image_file" || return 1
  actual_id=$(image_id_from_json "$image_file")
  actual_repo=$(repo_digest_from_json "$caddy_image" "$image_file")
  [ "$actual_id" = "$candidate_image_id" ] && [ -n "$actual_repo" ] || fail "new Caddy candidate image identity mismatch" || return 1
  candidate_repo_digest=$actual_repo
  network_file=$tmp_dir/new-caddy-network.json
  network_inspect_json "$message_network" "$network_file" || return 1
  candidate_network_id=$(network_id_from_json "$network_file")
  [ -n "$candidate_network_id" ] || return 1
  candidate_network_labels_hash=$(network_labels_hash "$network_file")
  [ "$candidate_network_id" = "$message_network_id" ] || return 1
  [ "$candidate_network_labels_hash" = "$message_network_labels_hash" ] || return 1
  data_file=$tmp_dir/new-caddy-data-volume.json
  config_file=$tmp_dir/new-caddy-config-volume.json
  volume_inspect_json "$caddy_data_volume" "$data_file" || return 1
  volume_inspect_json "$caddy_config_volume" "$config_file" || return 1
  candidate_data_volume_fingerprint=$(volume_fingerprint_from_json "$data_file") || return 1
  candidate_config_volume_fingerprint=$(volume_fingerprint_from_json "$config_file") || return 1
}

revalidate_candidate_exact() {
  local file=$tmp_dir/new-caddy-revalidate.inspect.json actual_id actual_repo image_file network_file data_file config_file
  inspect_container "$new_id" "$file" || return 1
  [ "$(container_id_from_inspect "$file")" = "$new_id" ] || return 1
  verify_new_identity_without_health "$file" "$new_id" "$edge_project" "$caddy_image" "$message_network" "$caddy_data_volume" "$caddy_config_volume" || return 1
  verify_hardened_candidate "$file" || return 1
  [ "$("$jq_bin" -r '.[0].Image // ""' "$file")" = "$candidate_image_id" ] || return 1
  image_file=$tmp_dir/new-caddy-revalidate-image.json
  image_inspect_json "$caddy_image" "$image_file" || return 1
  actual_id=$(image_id_from_json "$image_file")
  actual_repo=$(repo_digest_from_json "$caddy_image" "$image_file")
  [ "$actual_id" = "$candidate_image_id" ] && [ "$actual_repo" = "$candidate_repo_digest" ] || return 1
  network_file=$tmp_dir/new-caddy-revalidate-network.json
  network_inspect_json "$message_network" "$network_file" || return 1
  [ "$(network_id_from_json "$network_file")" = "$candidate_network_id" ] || return 1
  [ "$(network_labels_hash "$network_file")" = "$candidate_network_labels_hash" ] || return 1
  data_file=$tmp_dir/new-caddy-revalidate-data.json
  config_file=$tmp_dir/new-caddy-revalidate-config.json
  volume_inspect_json "$caddy_data_volume" "$data_file" || return 1
  volume_inspect_json "$caddy_config_volume" "$config_file" || return 1
  [ "$(volume_fingerprint_from_json "$data_file")" = "$candidate_data_volume_fingerprint" ] || return 1
  [ "$(volume_fingerprint_from_json "$config_file")" = "$candidate_config_volume_fingerprint" ] || return 1
}

wait_new_healthy() {
  local status
  for _ in $(seq 1 60); do
    inspect_container "$new_id" "$tmp_dir/new-caddy-health.inspect.json" || return 1
    status=$(container_health "$tmp_dir/new-caddy-health.inspect.json")
    if [ "$status" = healthy ]; then
      verify_healthy_running_container "$tmp_dir/new-caddy-health.inspect.json" || return 1
      revalidate_candidate_exact || return 1
      return 0
    fi
    [ "$status" != unhealthy ] || return 1
    sleep 1
  done
  return 1
}

remove_candidate_with_revalidation() {
  local after=$tmp_dir/candidate-after-remove.inspect.json
  revalidate_before_candidate_remove || return 1
  if "$docker_bin" rm -f "$new_id" >/dev/null 2>&1; then
    return 0
  fi
  # A failed rm is not permission to retry against a mutable name.  Rebind
  # the host, controls, objects, and full candidate ID immediately before the
  # exact-container stop fallback.
  revalidate_before_candidate_remove || return 1
  "$docker_bin" stop "$new_id" >/dev/null 2>&1 || return 1
  inspect_container "$new_id" "$after" || return 1
  [ "$(container_id_from_inspect "$after")" = "$new_id" ] || return 1
  [ "$("$jq_bin" -r '.[0].State.Status // ""' "$after")" != running ]
}

stop_old_checked() {
  local after=$tmp_dir/old-after-stop.inspect.json stop_status
  revalidate_before_old_stop || return 1
  if "$docker_bin" stop "$old_id" >/dev/null 2>&1; then
    stop_status=0
  else
    stop_status=$?
  fi
  if [ "${DIREXTALK_CUTOVER_TEST_CRASH_AFTER_OLD_STOP:-false}" = true ]; then
    log "test crash after old stop"
    exit 92
  fi
  inspect_container "$old_id" "$after" || {
    log "old Caddy stop result is uncertain; exact ID cannot be inspected"
    return 1
  }
  [ "$(container_id_from_inspect "$after")" = "$old_id" ] || return 1
  case "$("$jq_bin" -r '.[0].State.Status // ""' "$after"):$stop_status" in
    exited:0|created:0|exited:*)
      verify_old_identity_before_start "$after" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || return 1
      return 0
      ;;
    running:*)
      remove_candidate_with_revalidation || return 1
      log "old Caddy remained running after stop; candidate removed and public edge left unchanged"
      return 1
      ;;
    *)
      log "old Caddy stop left an uncertain exact-container state"
      return 1
      ;;
  esac
}

revalidate_control_bindings() {
  local current_host_engine
  current_host_engine=$(host_engine_binding) || return 1
  [ "$current_host_engine" = "$bound_host_engine" ] || fail "host or Docker Engine identity changed since preflight" || return 1
  [ "$(file_binding "$stack_env")" = "$bound_stack_env" ] || fail "message stack env identity changed since preflight" || return 1
  [ "$(file_binding "$edge_env")" = "$bound_edge_env" ] || fail "edge env identity changed since preflight" || return 1
  [ "$(file_binding "$active_receipt")" = "$bound_active_receipt" ] || fail "active receipt identity changed since preflight" || return 1
  [ "$(file_binding "$stack_compose")" = "$bound_stack_compose" ] || fail "message Compose identity changed since preflight" || return 1
  [ "$(file_binding "$edge_compose")" = "$bound_edge_compose" ] || fail "edge Compose identity changed since preflight" || return 1
  [ "$(file_binding "$caddyfile")" = "$bound_caddyfile" ] || fail "Caddyfile identity changed since preflight" || return 1
  [ "$(file_binding "$message_tls_cert")" = "$bound_message_tls_cert" ] || fail "message TLS certificate identity changed since preflight" || return 1
  [ "$(dir_binding "$output_parent")" = "$bound_output_parent" ] || fail "output receipt parent identity changed since preflight" || return 1
}

revalidate_active_objects() {
  local network_file data_file config_file
  network_file=$tmp_dir/revalidate-active-network.json
  data_file=$tmp_dir/revalidate-active-data.json
  config_file=$tmp_dir/revalidate-active-config.json
  network_inspect_json "$current_network" "$network_file" || return 1
  [ "$(network_id_from_json "$network_file")" = "$current_network_id" ] || return 1
  [ "$(network_labels_hash "$network_file")" = "$current_network_labels_hash" ] || return 1
  volume_inspect_json "$current_data_volume" "$data_file" || return 1
  volume_inspect_json "$current_config_volume" "$config_file" || return 1
  [ "$(volume_fingerprint_from_json "$data_file")" = "$current_data_volume_fingerprint" ] || return 1
  [ "$(volume_fingerprint_from_json "$config_file")" = "$current_config_volume_fingerprint" ] || return 1
  verify_receipt_object_chain
}

revalidate_before_old_stop() {
  local file=$tmp_dir/revalidate-old-stop.inspect.json
  revalidate_control_bindings || return 1
  revalidate_active_objects || return 1
  inspect_container "$old_id" "$file" || return 1
  verify_old_identity "$file" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume"
}

revalidate_before_candidate_start() {
  revalidate_control_bindings || return 1
  revalidate_active_objects || return 1
  revalidate_candidate_exact
}

revalidate_before_candidate_remove() {
  revalidate_control_bindings || return 1
  revalidate_active_objects || return 1
  revalidate_candidate_exact
}

verify_old_identity_before_start() {
  local file=$1 expected_id=$2 project=$3 image=$4 network=$5 data_volume=$6 config_volume=$7
  valid_container_id "$expected_id" || return 1
  "$jq_bin" -e --arg id "$expected_id" --arg project "$project" --arg image "$image" \
    --arg network "$network" --arg data "$data_volume" --arg config "$config_volume" '
      .[0].Id == $id and
      .[0].Config.Image == $image and
      .[0].Config.Labels["com.docker.compose.project"] == $project and
      .[0].Config.Labels["com.docker.compose.service"] == "caddy" and
      (.[0].NetworkSettings.Networks | keys | index($network)) != null and
      ([.[0].Mounts[] | select(.Destination == "/data" and .Type == "volume" and .Name == $data)] | length) == 1 and
      ([.[0].Mounts[] | select(.Destination == "/config" and .Type == "volume" and .Name == $config)] | length) == 1 and
      ([.[0].HostConfig.PortBindings["80/tcp"][]?.HostPort] | sort | unique) == ["80"] and
      ([.[0].HostConfig.PortBindings["443/tcp"][]?.HostPort] | sort | unique) == ["443"] and
      .[0].State.Status != "running"
    ' "$file" >/dev/null 2>&1
}

rollback() {
  local candidate_inspect candidate_full candidate_cleanup_ok=true
  log "cutover failed; removing only the new Caddy and restoring the exact recorded container"
  if [ -n "${new_id:-}" ] && valid_full_container_id "$new_id"; then
    candidate_inspect=$tmp_dir/rollback-candidate.inspect.json
    if inspect_container "$new_id" "$candidate_inspect"; then
      candidate_full=$(container_id_from_inspect "$candidate_inspect")
      if [ "$candidate_full" = "$new_id" ] && remove_candidate_with_revalidation; then
        :
      else
        log "refusing to remove an unverified replacement Caddy container"
        candidate_cleanup_ok=false
      fi
    else
      candidate_cleanup_ok=false
    fi
  fi
  [ "$candidate_cleanup_ok" = true ] || return 1
  revalidate_control_bindings || return 1
  revalidate_active_objects || return 1
  old_before_start=$tmp_dir/rollback-old-before-start.inspect.json
  inspect_container "$old_id" "$old_before_start" || return 1
  verify_old_identity_before_start "$old_before_start" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || {
    log "rollback old Caddy identity changed before start"
    return 1
  }
  revalidate_control_bindings || return 1
  revalidate_active_objects || return 1
  inspect_container "$old_id" "$tmp_dir/rollback-old-immediate.inspect.json" || return 1
  verify_old_identity_before_start "$tmp_dir/rollback-old-immediate.inspect.json" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || return 1
  "$docker_bin" start "$old_id" >/dev/null 2>&1 || return 1
  old_after_start=$tmp_dir/rollback-old.inspect.json
  inspect_container "$old_id" "$old_after_start" || return 1
  verify_old_identity "$old_after_start" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || return 1
  verify_public || return 1
}

cleanup_transaction() {
  [ -d "$txn_dir" ] && [ ! -L "$txn_dir" ] || return 0
  require_regular_owned "$txn_journal" 400 || return 1
  rm -f -- "$txn_journal" || return 1
  rmdir -- "$txn_dir"
}

write_transaction_journal() {
  local body journal_tmp
  if transaction_exists; then
    fail "cutover transaction journal already exists"
    return 1
  fi
  umask 077
  mkdir -m 700 -- "$txn_dir" || return 1
  body=$tmp_dir/cutover-transaction-body
  {
    printf '%s\n' '# dirextalk-cutover-journal-v1'
    printf 'owner_uid=%s\nhost_name=%s\nmachine_id=%s\ndocker_engine_id=%s\n' \
      "$(id -u)" "$receipt_host_name" "$receipt_machine_id" "$receipt_engine_id"
    printf 'stack_env=%s\nedge_env=%s\nactive_receipt=%s\noutput_receipt=%s\noutput_parent=%s\noutput_parent_binding=%s\n' \
      "$stack_env" "$edge_env" "$active_receipt" "$output_receipt" "$output_parent" "$bound_output_parent"
    printf 'stack_env_binding=%s\nedge_env_binding=%s\nactive_receipt_binding=%s\nstack_compose_binding=%s\nedge_compose_binding=%s\ncaddyfile_binding=%s\nmessage_tls_cert_binding=%s\n' \
      "$bound_stack_env" "$bound_edge_env" "$bound_active_receipt" "$bound_stack_compose" "$bound_edge_compose" "$bound_caddyfile" "$bound_message_tls_cert"
    printf 'old_caddy_container_id=%s\nold_caddy_project=%s\nold_caddy_image=%s\nold_caddy_image_id=%s\nold_caddy_repo_digest=%s\nold_caddy_network=%s\nold_caddy_network_id=%s\nold_caddy_network_labels_sha256=%s\nold_caddy_data_volume=%s\nold_caddy_data_volume_fingerprint_sha256=%s\nold_caddy_config_volume=%s\nold_caddy_config_volume_fingerprint_sha256=%s\n' \
      "$old_id" "$old_project" "$old_image" "$current_image_id" "$current_repo_digest" "$current_network" "$current_network_id" "$current_network_labels_hash" "$old_data_volume" "$current_data_volume_fingerprint" "$old_config_volume" "$current_config_volume_fingerprint"
    printf 'new_caddy_container_id=%s\nnew_caddy_project=%s\nnew_caddy_image=%s\nnew_caddy_image_id=%s\nnew_caddy_repo_digest=%s\nnew_caddy_network=%s\nnew_caddy_network_id=%s\nnew_caddy_network_labels_sha256=%s\nnew_caddy_data_volume=%s\nnew_caddy_data_volume_fingerprint_sha256=%s\nnew_caddy_config_volume=%s\nnew_caddy_config_volume_fingerprint_sha256=%s\n' \
      "$new_id" "$edge_project" "$caddy_image" "$candidate_image_id" "$candidate_repo_digest" "$message_network" "$candidate_network_id" "$candidate_network_labels_hash" "$caddy_data_volume" "$candidate_data_volume_fingerprint" "$caddy_config_volume" "$candidate_config_volume_fingerprint"
  } >"$body" || return 1
  chmod 400 "$body" || return 1
  journal_tmp=$(mktemp "$txn_dir/.journal.XXXXXX") || return 1
  cat -- "$body" >"$journal_tmp" || return 1
  chmod 400 "$journal_tmp" || return 1
  ln -- "$journal_tmp" "$txn_journal" || return 1
  rm -f -- "$journal_tmp"
  require_regular_owned "$txn_journal" 400
}

load_transaction_journal() {
  require_dir_owned "$txn_dir" 700 || return 1
  require_regular_owned "$txn_journal" 400 || return 1
  [ "$(sed -n '1p' "$txn_journal")" = '# dirextalk-cutover-journal-v1' ] || fail "cutover transaction journal header is invalid" || return 1
  [ "$(read_kv "$txn_journal" owner_uid)" = "$(id -u)" ] || return 1
  [ "$(read_kv "$txn_journal" host_name)" = "$receipt_host_name" ] || return 1
  [ "$(read_kv "$txn_journal" machine_id)" = "$receipt_machine_id" ] || return 1
  [ "$(read_kv "$txn_journal" docker_engine_id)" = "$receipt_engine_id" ] || return 1
  [ "$(read_kv "$txn_journal" stack_env)" = "$stack_env" ] || return 1
  [ "$(read_kv "$txn_journal" edge_env)" = "$edge_env" ] || return 1
  [ "$(read_kv "$txn_journal" active_receipt)" = "$active_receipt" ] || return 1
  [ "$(read_kv "$txn_journal" output_receipt)" = "$output_receipt" ] || return 1
  [ "$(read_kv "$txn_journal" output_parent)" = "$output_parent" ] || return 1
  [ "$(read_kv "$txn_journal" output_parent_binding)" = "$bound_output_parent" ] || return 1
  [ "$(read_kv "$txn_journal" stack_env_binding)" = "$bound_stack_env" ] || return 1
  [ "$(read_kv "$txn_journal" edge_env_binding)" = "$bound_edge_env" ] || return 1
  [ "$(read_kv "$txn_journal" active_receipt_binding)" = "$bound_active_receipt" ] || return 1
  [ "$(read_kv "$txn_journal" stack_compose_binding)" = "$bound_stack_compose" ] || return 1
  [ "$(read_kv "$txn_journal" edge_compose_binding)" = "$bound_edge_compose" ] || return 1
  [ "$(read_kv "$txn_journal" caddyfile_binding)" = "$bound_caddyfile" ] || return 1
  [ "$(read_kv "$txn_journal" message_tls_cert_binding)" = "$bound_message_tls_cert" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_container_id)" = "$old_id" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_project)" = "$old_project" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_image)" = "$old_image" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_image_id)" = "$current_image_id" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_repo_digest)" = "$current_repo_digest" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_network)" = "$current_network" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_network_id)" = "$current_network_id" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_network_labels_sha256)" = "$current_network_labels_hash" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_data_volume)" = "$old_data_volume" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_data_volume_fingerprint_sha256)" = "$current_data_volume_fingerprint" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_config_volume)" = "$old_config_volume" ] || return 1
  [ "$(read_kv "$txn_journal" old_caddy_config_volume_fingerprint_sha256)" = "$current_config_volume_fingerprint" ] || return 1
  new_id=$(read_kv "$txn_journal" new_caddy_container_id) || return 1
  [ "$(read_kv "$txn_journal" new_caddy_project)" = "$edge_project" ] || return 1
  [ "$(read_kv "$txn_journal" new_caddy_image)" = "$caddy_image" ] || return 1
  candidate_image_id=$(read_kv "$txn_journal" new_caddy_image_id) || return 1
  candidate_repo_digest=$(read_kv "$txn_journal" new_caddy_repo_digest) || return 1
  [ "$(read_kv "$txn_journal" new_caddy_network)" = "$message_network" ] || return 1
  candidate_network_id=$(read_kv "$txn_journal" new_caddy_network_id) || return 1
  candidate_network_labels_hash=$(read_kv "$txn_journal" new_caddy_network_labels_sha256) || return 1
  [ "$(read_kv "$txn_journal" new_caddy_data_volume)" = "$caddy_data_volume" ] || return 1
  candidate_data_volume_fingerprint=$(read_kv "$txn_journal" new_caddy_data_volume_fingerprint_sha256) || return 1
  [ "$(read_kv "$txn_journal" new_caddy_config_volume)" = "$caddy_config_volume" ] || return 1
  candidate_config_volume_fingerprint=$(read_kv "$txn_journal" new_caddy_config_volume_fingerprint_sha256) || return 1
  valid_full_container_id "$new_id" || return 1
}

recover_transaction() {
  local old_file=$tmp_dir/recovery-old.inspect.json candidate_file=$tmp_dir/recovery-new.inspect.json old_state candidate_state
  load_transaction_journal || return 1
  revalidate_control_bindings || return 1
  revalidate_active_objects || return 1
  inspect_container "$old_id" "$old_file" || return 1
  inspect_container "$new_id" "$candidate_file" || return 1
  [ "$(container_id_from_inspect "$old_file")" = "$old_id" ] || return 1
  [ "$(container_id_from_inspect "$candidate_file")" = "$new_id" ] || return 1
  old_state=$("$jq_bin" -r '.[0].State.Status // ""' "$old_file")
  candidate_state=$("$jq_bin" -r '.[0].State.Status // ""' "$candidate_file")
  case "$old_state:$candidate_state" in
    exited:running)
      verify_old_identity_before_start "$old_file" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || return 1
      revalidate_before_candidate_start || return 1
      wait_new_healthy || return 1
      verify_public || return 1
      write_receipt || return 1
      cleanup_transaction || return 1
      ;;
    exited:created|exited:exited)
      verify_old_identity_before_start "$old_file" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || return 1
      revalidate_before_candidate_start || return 1
      "$docker_bin" start "$new_id" >/dev/null 2>&1 || return 1
      wait_new_healthy || return 1
      verify_public || return 1
      write_receipt || return 1
      cleanup_transaction || return 1
      ;;
    running:created|running:exited)
      verify_old_identity "$old_file" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || return 1
      remove_candidate_with_revalidation || return 1
      cleanup_transaction || return 1
      fail "cutover recovery found legacy still public; candidate removed" || return 1
      ;;
    *)
      fail "cutover recovery found an uncertain exact-container state" || return 1
      ;;
  esac
}

write_receipt() {
  local tmp_receipt
  umask 077
  tmp_receipt=$(mktemp "$output_parent/.edge-receipt.XXXXXX") || return 1
  {
    printf '%s\n' '# dirextalk-edge-receipt-v1'
    printf 'receipt_kind=adopted-edge-v1\nowner_uid=%s\noperation_id=%s\nrevision=%s\n' "$(id -u)" "$receipt_operation" "$receipt_revision"
    printf 'host_name=%s\nmachine_id=%s\ndocker_engine_id=%s\n' "$receipt_host_name" "$receipt_machine_id" "$receipt_engine_id"
    printf 'edge_stack_name=%s\n' "$edge_project"
    printf 'message_stack_name=%s\n' "$stack_name"
    printf 'domain=%s\n' "$edge_domain"
    printf 'message_public_network=%s\n' "$message_network"
    printf 'caddy_image=%s\n' "$caddy_image"
    printf 'caddy_data_volume=%s\n' "$caddy_data_volume"
    printf 'caddy_data_volume_fingerprint_sha256=%s\n' "$candidate_data_volume_fingerprint"
    printf 'caddy_config_volume=%s\n' "$caddy_config_volume"
    printf 'caddy_config_volume_fingerprint_sha256=%s\n' "$candidate_config_volume_fingerprint"
    printf 'public_network_id=%s\npublic_network_labels_sha256=%s\n' "$candidate_network_id" "$candidate_network_labels_hash"
    printf 'old_caddy_container_id=%s\n' "$old_id"
    printf 'old_caddy_project=%s\n' "$old_project"
    printf 'old_caddy_image=%s\n' "$old_image"
    printf 'old_caddy_image_id=%s\nold_caddy_repo_digest=%s\n' "$current_image_id" "$current_repo_digest"
    printf 'old_caddy_data_volume=%s\n' "$old_data_volume"
    printf 'old_caddy_config_volume=%s\n' "$old_config_volume"
    printf 'old_caddy_network=%s\n' "$current_network"
    printf 'old_caddy_network_id=%s\nold_caddy_network_labels_sha256=%s\n' "$current_network_id" "$current_network_labels_hash"
    printf 'old_caddy_data_volume_fingerprint_sha256=%s\nold_caddy_config_volume_fingerprint_sha256=%s\n' "$current_data_volume_fingerprint" "$current_config_volume_fingerprint"
    printf 'old_caddy_http_port=80\nold_caddy_https_port=443\n'
    printf 'new_caddy_container_id=%s\n' "$new_id"
    printf 'new_caddy_project=%s\n' "$edge_project"
    printf 'new_caddy_image=%s\n' "$caddy_image"
    printf 'new_caddy_image_id=%s\nnew_caddy_repo_digest=%s\n' "$candidate_image_id" "$candidate_repo_digest"
    printf 'new_caddy_data_volume=%s\n' "$caddy_data_volume"
    printf 'new_caddy_config_volume=%s\n' "$caddy_config_volume"
    printf 'new_caddy_network=%s\n' "$message_network"
    printf 'new_caddy_network_id=%s\nnew_caddy_network_labels_sha256=%s\nnew_caddy_data_volume_fingerprint_sha256=%s\nnew_caddy_config_volume_fingerprint_sha256=%s\n' "$candidate_network_id" "$candidate_network_labels_hash" "$candidate_data_volume_fingerprint" "$candidate_config_volume_fingerprint"
    printf 'new_caddy_http_port=80\nnew_caddy_https_port=443\n'
    printf 'active_caddy_container_id=%s\n' "$new_id"
    printf 'active_caddy_project=%s\n' "$edge_project"
    printf 'active_caddy_image=%s\n' "$caddy_image"
    printf 'active_caddy_image_id=%s\nactive_caddy_repo_digest=%s\n' "$candidate_image_id" "$candidate_repo_digest"
    printf 'active_caddy_data_volume=%s\n' "$caddy_data_volume"
    printf 'active_caddy_config_volume=%s\n' "$caddy_config_volume"
    printf 'active_caddy_network=%s\n' "$message_network"
    printf 'active_caddy_network_id=%s\nactive_caddy_network_labels_sha256=%s\nactive_caddy_data_volume_fingerprint_sha256=%s\nactive_caddy_config_volume_fingerprint_sha256=%s\n' "$candidate_network_id" "$candidate_network_labels_hash" "$candidate_data_volume_fingerprint" "$candidate_config_volume_fingerprint"
    printf 'active_caddy_http_port=80\nactive_caddy_https_port=443\n'
    printf 'message_server_image=%s\n' "$message_image"
    printf 'agent_image=%s\n' "$agent_image"
  } >"$tmp_receipt" || { rm -f -- "$tmp_receipt"; return 1; }
  chmod 400 "$tmp_receipt" || { rm -f -- "$tmp_receipt"; return 1; }
  # A hard-link install is atomic and refuses a destination that appeared
  # after the initial absence check; unlike mv -f it cannot overwrite an
  # earlier audit receipt in a race.
  if [ -e "$output_receipt" ] || [ -L "$output_receipt" ]; then
    require_regular_owned "$output_receipt" 400 || { rm -f -- "$tmp_receipt"; return 1; }
    cmp -s -- "$tmp_receipt" "$output_receipt" || { rm -f -- "$tmp_receipt"; return 1; }
    rm -f -- "$tmp_receipt"
    return 0
  fi
  if ! ln -- "$tmp_receipt" "$output_receipt"; then
    rm -f -- "$tmp_receipt"
    return 1
  fi
  if [ "${DIREXTALK_CUTOVER_TEST_CRASH_AFTER_RECEIPT_INSTALL:-false}" = true ]; then
    log "test crash after receipt install"
    exit 97
  fi
  rm -f -- "$tmp_receipt"
  [ "$(stat -c '%a' "$output_receipt")" = 400 ] && [ ! -L "$output_receipt" ]
}

read_and_validate_inputs || exit 1
acquire_scope_lock || exit 1
render_and_verify_compose || exit 1
verify_message_stack || exit 1
verify_message_host || exit 1

if transaction_exists; then
  old_id=$current_id
  old_project=$current_project
  old_image=$current_image
  old_data_volume=$current_data_volume
  old_config_volume=$current_config_volume
  verify_receipt_object_chain || exit 1
  recover_transaction || exit 1
  printf 'edge cutover transaction recovered; receipt present with mode 0400\n'
  exit 0
fi

verify_active_caddy || exit 1
verify_receipt_object_chain || exit 1
verify_no_new_edge_container || exit 1

# Render and pre-create the candidate while the recorded legacy edge is still
# public.  All later mutations use only this full immutable ID.
create_new_caddy || exit 1
if ! write_transaction_journal; then
  remove_candidate_with_revalidation || true
  exit 1
fi

# Re-read the exact old container immediately before stopping it.  A same-name
# replacement is not an acceptable identity; only the recorded immutable ID
# can cross this boundary.
old_recheck=$tmp_dir/old-recheck.inspect.json
inspect_container "$old_id" "$old_recheck" || { fail "recorded old Caddy disappeared before cutover"; exit 1; }
verify_old_identity "$old_recheck" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || { fail "recorded old Caddy identity changed before cutover"; exit 1; }

if ! stop_old_checked; then
  # stop_old_checked removes a candidate only after exact identity checks when
  # the old ID remained running; otherwise it leaves the transaction journal
  # for a later exact-state recovery.
  exit 1
fi

old_after_stop=$tmp_dir/old-after-stop.inspect.json
inspect_container "$old_id" "$old_after_stop" || exit 1
verify_old_identity_before_start "$old_after_stop" "$old_id" "$old_project" "$old_image" "$current_network" "$old_data_volume" "$old_config_volume" || exit 1

if ! revalidate_before_candidate_start || ! "$docker_bin" start "$new_id" >/dev/null 2>&1; then
  if ! rollback; then
    fail "edge start failed and rollback failed"
  else
    fail "edge start failed; exact old Caddy restored" || true
    cleanup_transaction || true
  fi
  exit 1
fi

if [ "${DIREXTALK_CUTOVER_TEST_CRASH_AFTER_CANDIDATE_START:-false}" = true ]; then
  log "test crash after candidate start"
  exit 93
fi

if ! wait_new_healthy || ! verify_public; then
  if ! rollback; then
    fail "edge start failed and rollback failed"
  else
    fail "edge start failed; exact old Caddy restored"
    cleanup_transaction || true
  fi
  exit 1
fi

if ! write_receipt; then
  if ! rollback; then
    fail "receipt write failed and rollback failed"
  else
    fail "receipt write failed; exact old Caddy restored" || true
    cleanup_transaction || true
  fi
  exit 1
fi

cleanup_transaction || exit 1

printf 'edge cutover succeeded; receipt written with mode 0400\n'
