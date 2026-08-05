#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2317
set -euo pipefail

# First-edge adoption is deliberately a two-step operation.  `probe` only
# inspects the already running legacy edge and the reviewed control files.  A
# later `commit` consumes the exact probe binding, creates one candidate in a
# distinct Compose project, and switches only the exact container IDs that
# were inspected.  This is not a compatibility path for cutover-edge.sh.

usage() {
  cat >&2 <<'EOF'
usage:
  adopt-edge.sh probe  EDGE_ENV LEGACY_CADDY_ID PROBE_RECEIPT OPERATION REVISION
  adopt-edge.sh commit EDGE_ENV PROBE_RECEIPT OPERATION REVISION ACTIVE_RECEIPT LEGACY_SNAPSHOT

The legacy Caddy ID must be the full 64-character Docker ID.  OPERATION and
REVISION on commit must exactly match the values recorded by probe.
EOF
  exit 2
}

log() {
  printf 'edge adoption: %s\n' "$*" >&2
}

fail() {
  log "$*"
  return 1
}

[ "$#" -ge 1 ] || usage
mode=$1
shift

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
default_edge_compose=$script_dir/../edge-compose.yaml
docker_bin=${DIREXTALK_DOCKER_BIN:-docker}
curl_bin=${DIREXTALK_CURL_BIN:-curl}
jq_bin=${DIREXTALK_JQ_BIN:-jq}
flock_bin=${DIREXTALK_FLOCK_BIN:-flock}

tmp_dir=''
lock_file=''
lock_fd=''
output_parent=''
txn_dir=''
txn_journal=''
edge_env=''
legacy_id=''
probe_receipt=''
active_receipt=''
legacy_snapshot=''
stop_failure_cleanup_safe=false

cleanup() {
  if [ -n "${lock_fd:-}" ]; then
    "$flock_bin" -u "$lock_fd" 2>/dev/null || true
    eval "exec ${lock_fd}>&-" 2>/dev/null || true
  fi
  if [ -n "${tmp_dir:-}" ] && [ -d "$tmp_dir" ]; then
    rm -rf -- "$tmp_dir"
  fi
}
trap cleanup EXIT

require_regular_owned() {
  local file=$1 mode=${2:-}
  [ -f "$file" ] && [ ! -L "$file" ] || fail "file must be a regular non-symlink: $file" || return 1
  [ "$(stat -c '%u' "$file")" = "$(id -u)" ] || fail "file owner mismatch: $file" || return 1
  if [ -n "$mode" ]; then
    [ "$(stat -c '%a' "$file")" = "$mode" ] || fail "file mode must be $mode: $file" || return 1
  fi
}

require_private_dir() {
  local dir=$1 create=${2:-false}
  if [ ! -e "$dir" ]; then
    [ "$create" = true ] || fail "directory does not exist: $dir" || return 1
    umask 077
    mkdir -m 700 -- "$dir" || return 1
  fi
  [ -d "$dir" ] && [ ! -L "$dir" ] || fail "directory must be a regular non-symlink: $dir" || return 1
  [ "$(stat -c '%u' "$dir")" = "$(id -u)" ] || fail "directory owner mismatch: $dir" || return 1
  [ "$(stat -c '%a' "$dir")" = 700 ] || fail "directory mode must be 700: $dir" || return 1
}

require_new_path() {
  local path=$1
  [ ! -e "$path" ] && [ ! -L "$path" ] || fail "output path already exists: $path" || return 1
}

# Read one literal KEY=VALUE entry without sourcing an untrusted env file.
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

valid_safe_id() {
  printf '%s' "$1" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+@-]{0,255}$'
}

valid_token() {
  printf '%s' "$1" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$'
}

valid_project() {
  printf '%s' "$1" | grep -Eq '^[a-z0-9][a-z0-9_-]{0,62}$'
}

valid_domain() {
  printf '%s' "$1" | grep -Eq '^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?))+$'
}

valid_name() {
  printf '%s' "$1" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$'
}

valid_full_container_id() {
  printf '%s' "$1" | grep -Eq '^[0-9a-f]{64}$' && [ "${1//0/}" ]
}

valid_container_id() {
  printf '%s' "$1" | grep -Eq '^[0-9a-f]{12,64}$' && [ "${1//0/}" ]
}

valid_image() {
  local image=$1 digest repository canonical
  printf '%s' "$image" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' || return 1
  case "$image" in
    *registry.invalid*|*@sha256:0000000000000000000000000000000000000000000000000000000000000000) return 1 ;;
  esac
  repository=${image%%@sha256:*}
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
  digest=${image##*@sha256:}
  [ "${digest//0/}" ]
}

valid_port() {
  printf '%s' "$1" | grep -Eq '^[0-9]+$' && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

sha256_file() {
  sha256sum -- "$1" | awk '{print $1}'
}

file_binding() {
  local file=$1 mode sha
  require_regular_owned "$file" || return 1
  mode=$(stat -c '%a' "$file")
  sha=$(sha256_file "$file") || return 1
  printf '%s|%s|%s|%s|%s' "$(stat -c '%d' "$file")" "$(stat -c '%i' "$file")" "$(stat -c '%u' "$file")" "$mode" "$sha"
}

machine_identity() {
  local machine_file=${DIREXTALK_MACHINE_ID_FILE:-/etc/machine-id} machine_id host_name
  [ -f "$machine_file" ] && [ ! -L "$machine_file" ] || fail "machine identity file is unavailable" || return 1
  machine_id=$(tr -d '[:space:]' <"$machine_file") || return 1
  [ -n "$machine_id" ] || fail "machine identity is empty" || return 1
  host_name=$(hostname 2>/dev/null || true)
  [ -n "$host_name" ] || fail "host name is empty" || return 1
  valid_safe_id "$host_name" || fail "host name contains unsupported characters" || return 1
  valid_safe_id "$machine_id" || fail "machine identity contains unsupported characters" || return 1
  printf '%s|%s' "$host_name" "$machine_id"
}

docker_engine_identity() {
  local value
  value=$("$docker_bin" info --format '{{.ID}}' 2>/dev/null) || return 1
  value=$(printf '%s' "$value" | tr -d '[:space:]')
  [ -n "$value" ] || fail "Docker Engine ID is empty" || return 1
  valid_safe_id "$value" || fail "Docker Engine ID contains unsupported characters" || return 1
  printf '%s' "$value"
}

inspect_json() {
  local object=$1 out=$2
  "$docker_bin" inspect "$object" >"$out" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1' "$out" >/dev/null 2>&1 || return 1
}

network_json() {
  local name=$1 out=$2
  "$docker_bin" network inspect "$name" >"$out" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1' "$out" >/dev/null 2>&1 || return 1
}

volume_json() {
  local name=$1 out=$2
  "$docker_bin" volume inspect "$name" >"$out" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1' "$out" >/dev/null 2>&1 || return 1
}

image_json() {
  local image=$1 out=$2
  "$docker_bin" image inspect "$image" >"$out" 2>/dev/null || return 1
  "$jq_bin" -e 'type == "array" and length == 1' "$out" >/dev/null 2>&1 || return 1
}

container_id_from_json() { "$jq_bin" -r '.[0].Id // ""' "$1"; }
container_config_image() { "$jq_bin" -r '.[0].Config.Image // ""' "$1"; }
container_image_id() { "$jq_bin" -r '.[0].Image // ""' "$1"; }
container_health() { "$jq_bin" -r '.[0].State.Health.Status // ""' "$1"; }

legacy_health_is_unconfigured() {
  local file=$1
  "$jq_bin" -e '
    .[0].Config.Healthcheck == null and
    .[0].State.Health == null
  ' "$file" >/dev/null 2>&1
}

classify_legacy_health() {
  local file=$1 health
  health=$(container_health "$file") || return 1
  if [ "$health" = healthy ]; then
    printf 'healthy'
    return 0
  fi
  if [ -z "$health" ] && legacy_health_is_unconfigured "$file"; then
    printf 'unconfigured-public-probe'
    return 0
  fi
  return 1
}

verify_legacy_ready() {
  local file=$1
  [ "$(container_state "$file")" = running ] || return 1
  case "${legacy_health:-}" in
    healthy) [ "$(container_health "$file")" = healthy ] ;;
    unconfigured-public-probe) legacy_health_is_unconfigured "$file" ;;
    *) return 1 ;;
  esac
}

labels_hash() {
  local file=$1
  "$jq_bin" -c '.[0].Labels // {} | to_entries | sort_by(.key)' "$file" | sha256sum | awk '{print $1}'
}

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

network_id_from_json() { "$jq_bin" -r '.[0].Id // .[0].ID // ""' "$1"; }
network_name_from_json() { "$jq_bin" -r '.[0].Name // ""' "$1"; }

repo_digest_from_image_json() {
  "$jq_bin" -r --arg image "$1" '
    (.[0].RepoDigests // []) | map(select(type == "string" and contains("@sha256:"))) |
    map(select((split("@sha256:")[1]) == ($image | split("@sha256:")[1]))) | .[0] // ""
  ' "$2"
}

image_id_from_image_json() { "$jq_bin" -r '.[0].Id // .[0].ID // ""' "$1"; }

valid_legacy_caddy_config_image() {
  printf '%s' "$1" | grep -Eq '^(caddy|library/caddy|docker\.io/library/caddy|index\.docker\.io/library/caddy)(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$' || valid_image "$1"
}

legacy_caddy_repo_digest_from_image_json() {
  "$jq_bin" -r '
    [
      (.[0].RepoDigests // [])[]? |
      select(type == "string") |
      select(test("^(docker\\.io/)?(library/)?caddy@sha256:[0-9a-f]{64}$")) |
      sub("^docker\\.io/"; "") |
      sub("^library/"; "") |
      "docker.io/library/" + .
    ] | unique | if length == 1 then .[0] else "" end
  ' "$1"
}

verify_image_binding() {
  local image=$1 image_file=$2 expected_id=$3 expected_repo=$4 actual_id actual_repo
  valid_image "$image" || return 1
  actual_id=$(image_id_from_image_json "$image_file")
  actual_repo=$(repo_digest_from_image_json "$image" "$image_file")
  [ -n "$actual_id" ] && [ "$actual_id" = "$expected_id" ] || return 1
  [ -n "$actual_repo" ] && [ "$actual_repo" = "$expected_repo" ] || return 1
}

verify_legacy_image_binding() {
  local image_file=$1 expected_id=$2 expected_repo=$3 actual_id actual_repo
  actual_id=$(image_id_from_image_json "$image_file")
  actual_repo=$(legacy_caddy_repo_digest_from_image_json "$image_file")
  [ -n "$actual_id" ] && [ "$actual_id" = "$expected_id" ] || return 1
  [ -n "$actual_repo" ] && [ "$actual_repo" = "$expected_repo" ]
}

verify_ports_80_443() {
  local file=$1
  "$jq_bin" -e '
    ([.[0].HostConfig.PortBindings["80/tcp"][]?.HostPort] | sort | unique) == ["80"] and
    ([.[0].HostConfig.PortBindings["443/tcp"][]?.HostPort] | sort | unique) == ["443"]
  ' "$file" >/dev/null 2>&1
}

verify_volume_mounts() {
  local file=$1 data=$2 config=$3
  "$jq_bin" -e --arg data "$data" --arg config "$config" '
    ([.[0].Mounts[] | select(.Type == "volume" and .Destination == "/data" and .Name == $data)] | length) == 1 and
    ([.[0].Mounts[] | select(.Type == "volume" and .Destination == "/config" and .Name == $config)] | length) == 1
  ' "$file" >/dev/null 2>&1
}

verify_container_identity() {
  local file=$1 expected_id=$2 project=$3 image=$4 network=$5 data=$6 config=$7 health=$8
  local required_health='false'
  [ "$health" = true ] && required_health='true'
  valid_full_container_id "$expected_id" || return 1
  "$jq_bin" -e --arg id "$expected_id" --arg project "$project" --arg image "$image" \
    --arg network "$network" --arg data "$data" --arg config "$config" --argjson require_health "$required_health" '
      .[0].Id == $id and
      .[0].Config.Image == $image and
      .[0].Config.Labels["com.docker.compose.project"] == $project and
      .[0].Config.Labels["com.docker.compose.service"] == "caddy" and
      (.[0].NetworkSettings.Networks | keys | index($network)) != null and
      ([.[0].Mounts[] | select(.Type == "volume" and .Destination == "/data" and .Name == $data)] | length) == 1 and
      ([.[0].Mounts[] | select(.Type == "volume" and .Destination == "/config" and .Name == $config)] | length) == 1 and
      ([.[0].HostConfig.PortBindings["80/tcp"][]?.HostPort] | sort | unique) == ["80"] and
      ([.[0].HostConfig.PortBindings["443/tcp"][]?.HostPort] | sort | unique) == ["443"] and
      .[0].State.Status == "running" and
      ($require_health == false or .[0].State.Health.Status == "healthy")
    ' "$file" >/dev/null 2>&1
}

verify_hardened_candidate() {
  local file=$1
  "$jq_bin" -e '
    .[0].HostConfig.ReadonlyRootfs == true and
    ((.[0].HostConfig.CapDrop // []) | map(ascii_upcase) | index("ALL")) != null and
    ((.[0].HostConfig.SecurityOpt // []) | map(ascii_downcase) | any(. == "no-new-privileges:true" or . == "no-new-privileges")) and
    ((.[0].Config.Healthcheck.Test // []) | length) > 1 and
    ((.[0].Config.Healthcheck.Test // [""])[0] == "CMD-SHELL") and
    ((.[0].Config.Healthcheck.Test // [] | join(" ")) | test("wget|curl"))
  ' "$file" >/dev/null 2>&1
}

verify_network_binding() {
  local file=$1 expected_id=$2 expected_name=$3 expected_labels_hash=$4
  local actual_id actual_name actual_hash
  actual_id=$(network_id_from_json "$file")
  actual_name=$(network_name_from_json "$file")
  actual_hash=$(labels_hash "$file")
  [ -n "$actual_id" ] && [ "$actual_id" = "$expected_id" ] || return 1
  [ "$actual_name" = "$expected_name" ] || return 1
  [ "$actual_hash" = "$expected_labels_hash" ]
}

verify_file_binding() {
  local file=$1 expected=$2 actual
  actual=$(file_binding "$file") || return 1
  [ "$actual" = "$expected" ]
}

public_ca_file_binding() {
  if [ -z "${public_ca:-}" ]; then
    printf 'none'
  else
    file_binding "$public_ca"
  fi
}

verify_public_ca_binding() {
  case "${public_ca_binding:-}" in
    none) [ -z "${public_ca:-}" ] ;;
    '') return 1 ;;
    *) [ -n "${public_ca:-}" ] && verify_file_binding "$public_ca" "$public_ca_binding" ;;
  esac
}

require_protected_file() {
  local file=$1
  require_regular_owned "$file" 400 || return 1
}

container_state() {
  "$jq_bin" -r '.[0].State.Status // ""' "$1"
}

container_matches_candidate() {
  local file=$1 expected_state=${2:-}
  "$jq_bin" -e --arg id "$candidate_id" --arg project "$edge_project" --arg image "$caddy_image" --arg network "$edge_network" \
    '.[0].Id == $id and .[0].Config.Image == $image and
     .[0].Config.Labels["com.docker.compose.project"] == $project and
     .[0].Config.Labels["com.docker.compose.service"] == "caddy" and
     .[0].NetworkSettings.Networks[$network] != null' "$file" >/dev/null 2>&1 || return 1
  verify_ports_80_443 "$file" || return 1
  verify_volume_mounts "$file" "$caddy_data_volume" "$caddy_config_volume" || return 1
  verify_hardened_candidate "$file" || return 1
  if [ -n "$expected_state" ]; then
    [ "$(container_state "$file")" = "$expected_state" ] || return 1
  fi
}

revalidate_candidate() {
  local expected_state=${1:-}
  inspect_json "$candidate_id" "$tmp_dir/candidate-revalidate.json" || fail "candidate exact ID is unavailable" || return 1
  container_matches_candidate "$tmp_dir/candidate-revalidate.json" "$expected_state" || fail "candidate exact identity changed" || return 1
  candidate_image_id_actual=$(container_image_id "$tmp_dir/candidate-revalidate.json")
  [ "$candidate_image_id_actual" = "$candidate_image_id" ] || fail "candidate image ID changed" || return 1
  image_json "$caddy_image" "$tmp_dir/candidate-image-revalidate.json" || return 1
  verify_image_binding "$caddy_image" "$tmp_dir/candidate-image-revalidate.json" "$candidate_image_id" "$candidate_repo_digest" || fail "candidate RepoDigest/image ID changed" || return 1
  network_json "$edge_network" "$tmp_dir/candidate-network-revalidate.json" || return 1
  verify_network_binding "$tmp_dir/candidate-network-revalidate.json" "$network_id" "$edge_network" "$network_labels_hash" || fail "candidate network object changed" || return 1
  volume_json "$caddy_data_volume" "$tmp_dir/candidate-data-revalidate.json" || return 1
  volume_json "$caddy_config_volume" "$tmp_dir/candidate-config-revalidate.json" || return 1
  [ "$(volume_fingerprint_from_json "$tmp_dir/candidate-data-revalidate.json")" = "$data_volume_fingerprint" ] || fail "candidate data volume object changed" || return 1
  [ "$(volume_fingerprint_from_json "$tmp_dir/candidate-config-revalidate.json")" = "$config_volume_fingerprint" ] || fail "candidate config volume object changed" || return 1
}

compose_edge() {
  "$docker_bin" compose --env-file "$edge_env" -f "$edge_compose" "$@"
}

read_edge_env() {
  require_regular_owned "$edge_env" 400 || return 1
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
  legacy_message_stack=$(optional_kv "$edge_env" DIREXTALK_LEGACY_MESSAGE_STACK_NAME) || return 1

  valid_project "$edge_project" || fail "invalid edge Compose project name" || return 1
  valid_name "$edge_network" || fail "invalid public network name" || return 1
  valid_image "$caddy_image" || fail "Caddy image is not an immutable digest" || return 1
  valid_name "$caddy_data_volume" && valid_name "$caddy_config_volume" || fail "invalid Caddy volume name" || return 1
  valid_project "${legacy_message_stack:-legacy-edge}" || fail "invalid legacy message stack name" || return 1
  valid_domain "$edge_domain" || fail "invalid public domain" || return 1
  case "$well_known_path:$public_health_path" in /*:/*) ;; *) fail "public endpoint paths must be absolute" || return 1 ;; esac
  require_regular_owned "$edge_compose" || return 1
  require_regular_owned "$caddyfile" || return 1
  [ -z "${public_ca:-}" ] || require_regular_owned "$public_ca" || return 1
  grep -Eq '(^|[[:space:]])reverse_proxy[[:space:]]+message-server:(8008|8448)([[:space:]]|$)' "$caddyfile" || fail "Caddyfile must proxy to message-server" || return 1
}

ensure_output_parent() {
  output_parent=$(dirname -- "$1")
  require_private_dir "$output_parent" true || return 1
}

atomic_write_0400() {
  local destination=$1 header=$2 body_file=$3 tmp_file
  require_new_path "$destination" || return 1
  ensure_output_parent "$destination" || return 1
  umask 077
  tmp_file=$(mktemp "$output_parent/.edge-receipt.XXXXXX") || return 1
  {
    printf '%s\n' "$header"
    cat -- "$body_file"
  } >"$tmp_file" || return 1
  chmod 400 "$tmp_file" || return 1
  ln -- "$tmp_file" "$destination" || return 1
  rm -f -- "$tmp_file"
  [ "$(stat -c '%a' "$destination")" = 400 ] && [ ! -L "$destination" ]
}

install_pending_file() {
  local pending=$1 destination=$2
  require_protected_file "$pending" || return 1
  if [ -e "$destination" ] || [ -L "$destination" ]; then
    require_protected_file "$destination" || return 1
    cmp -s -- "$pending" "$destination" || fail "existing receipt bytes do not match transaction" || return 1
    return 0
  fi
  ln -- "$pending" "$destination" || return 1
  require_protected_file "$destination"
}

cleanup_transaction() {
  [ -n "${txn_dir:-}" ] || return 0
  [ -d "$txn_dir" ] || return 0
  rm -f -- "$txn_dir/journal" "$txn_dir/active.receipt" "$txn_dir/legacy.snapshot" "$txn_dir/marker" 2>/dev/null || return 1
  rmdir -- "$txn_dir"
}

write_transaction() {
  local body journal_tmp
  txn_dir=$(dirname -- "$probe_receipt")/.adopt-edge-txn.$operation
  txn_journal=$txn_dir/journal
  [ ! -e "$txn_dir" ] && [ ! -L "$txn_dir" ] || fail "transaction journal already exists" || return 1
  umask 077
  mkdir -m 700 -- "$txn_dir" || return 1
  body=$tmp_dir/transaction-body
  {
    printf 'owner_uid=%s\noperation_id=%s\nrevision=%s\n' "$(id -u)" "$operation" "$revision"
    printf 'host_name=%s\nmachine_id=%s\ndocker_engine_id=%s\n' "$probe_host_name" "$probe_machine_id" "$probe_engine_id"
    printf 'probe_receipt=%s\nactive_receipt=%s\nlegacy_snapshot=%s\n' "$probe_receipt" "$active_receipt" "$legacy_snapshot"
    printf 'legacy_caddy_container_id=%s\nlegacy_caddy_project=%s\nlegacy_caddy_config_image=%s\nlegacy_caddy_image_id=%s\nlegacy_caddy_repo_digest=%s\n' "$legacy_id" "$legacy_project" "$legacy_config_image" "$legacy_image_id" "$legacy_repo_digest"
    printf 'candidate_caddy_container_id=%s\ncandidate_caddy_project=%s\ncandidate_caddy_image=%s\ncandidate_caddy_image_id=%s\ncandidate_caddy_repo_digest=%s\n' "$candidate_id" "$edge_project" "$caddy_image" "$candidate_image_id" "$candidate_repo_digest"
    printf 'public_network_name=%s\npublic_network_id=%s\npublic_network_labels_sha256=%s\n' "$edge_network" "$network_id" "$network_labels_hash"
    printf 'caddy_data_volume=%s\ncaddy_data_volume_fingerprint_sha256=%s\ncaddy_config_volume=%s\ncaddy_config_volume_fingerprint_sha256=%s\n' "$caddy_data_volume" "$data_volume_fingerprint" "$caddy_config_volume" "$config_volume_fingerprint"
    printf 'edge_env_binding=%s\ncaddyfile_binding=%s\nedge_compose_binding=%s\npublic_ca_binding=%s\n' "$edge_env_binding" "$caddyfile_binding" "$edge_compose_binding" "$public_ca_binding"
  } >"$body"
  chmod 400 "$body"
  {
    printf '%s\n' '# dirextalk-edge-adoption-journal-v1'
    cat -- "$body"
  } >"$tmp_dir/journal"
  chmod 400 "$tmp_dir/journal"
  journal_tmp=$(mktemp "$txn_dir/.journal.XXXXXX") || return 1
  cat -- "$tmp_dir/journal" >"$journal_tmp"
  chmod 400 "$journal_tmp"
  ln -- "$journal_tmp" "$txn_journal" || return 1
  rm -f -- "$journal_tmp"
  prepare_receipt_bodies
}

prepare_receipt_bodies() {
  local active_body legacy_body
  active_body=$txn_dir/active.receipt
  legacy_body=$txn_dir/legacy.snapshot
  {
    printf '%s\n' '# dirextalk-edge-receipt-v1'
    printf 'receipt_kind=adopted-edge-v1\nowner_uid=%s\noperation_id=%s\nrevision=%s\n' "$(id -u)" "$operation" "$revision"
    printf 'host_name=%s\nmachine_id=%s\ndocker_engine_id=%s\n' "$probe_host_name" "$probe_machine_id" "$probe_engine_id"
    printf 'edge_stack_name=%s\nmessage_stack_name=%s\ndomain=%s\nmessage_public_network=%s\n' "$edge_project" "$legacy_message_stack" "$edge_domain" "$edge_network"
    printf 'public_network_id=%s\npublic_network_labels_sha256=%s\n' "$network_id" "$network_labels_hash"
    printf 'caddy_image=%s\ncaddy_data_volume=%s\ncaddy_data_volume_fingerprint_sha256=%s\ncaddy_config_volume=%s\ncaddy_config_volume_fingerprint_sha256=%s\n' "$caddy_image" "$caddy_data_volume" "$data_volume_fingerprint" "$caddy_config_volume" "$config_volume_fingerprint"
    printf 'old_caddy_container_id=%s\nold_caddy_project=%s\nold_caddy_config_image=%s\nold_caddy_image_id=%s\nold_caddy_repo_digest=%s\nold_caddy_data_volume=%s\nold_caddy_data_volume_fingerprint_sha256=%s\nold_caddy_config_volume=%s\nold_caddy_config_volume_fingerprint_sha256=%s\nold_caddy_network=%s\nold_caddy_network_id=%s\nold_caddy_http_port=80\nold_caddy_https_port=443\nold_caddy_state=stopped\n' "$legacy_id" "$legacy_project" "$legacy_config_image" "$legacy_image_id" "$legacy_repo_digest" "$caddy_data_volume" "$data_volume_fingerprint" "$caddy_config_volume" "$config_volume_fingerprint" "$edge_network" "$network_id"
    printf 'new_caddy_container_id=%s\nnew_caddy_project=%s\nnew_caddy_image=%s\nnew_caddy_image_id=%s\nnew_caddy_repo_digest=%s\nnew_caddy_data_volume=%s\nnew_caddy_data_volume_fingerprint_sha256=%s\nnew_caddy_config_volume=%s\nnew_caddy_config_volume_fingerprint_sha256=%s\nnew_caddy_network=%s\nnew_caddy_network_id=%s\nnew_caddy_http_port=80\nnew_caddy_https_port=443\n' "$candidate_id" "$edge_project" "$caddy_image" "$candidate_image_id" "$candidate_repo_digest" "$caddy_data_volume" "$data_volume_fingerprint" "$caddy_config_volume" "$config_volume_fingerprint" "$edge_network" "$network_id"
    printf 'active_caddy_container_id=%s\nactive_caddy_project=%s\nactive_caddy_image=%s\nactive_caddy_image_id=%s\nactive_caddy_repo_digest=%s\nactive_caddy_data_volume=%s\nactive_caddy_data_volume_fingerprint_sha256=%s\nactive_caddy_config_volume=%s\nactive_caddy_config_volume_fingerprint_sha256=%s\nactive_caddy_network=%s\nactive_caddy_network_id=%s\nactive_caddy_network_labels_sha256=%s\nactive_caddy_http_port=80\nactive_caddy_https_port=443\n' "$candidate_id" "$edge_project" "$caddy_image" "$candidate_image_id" "$candidate_repo_digest" "$caddy_data_volume" "$data_volume_fingerprint" "$caddy_config_volume" "$config_volume_fingerprint" "$edge_network" "$network_id" "$network_labels_hash"
    printf 'edge_env_binding=%s\ncaddyfile_binding=%s\nedge_compose_binding=%s\npublic_ca_binding=%s\n' "$edge_env_binding" "$caddyfile_binding" "$edge_compose_binding" "$public_ca_binding"
  } >"$active_body"
  {
    printf '%s\n' '# dirextalk-edge-legacy-snapshot-v1'
    printf 'receipt_kind=adopted-edge-v1\nowner_uid=%s\noperation_id=%s\nrevision=%s\n' "$(id -u)" "$operation" "$revision"
    printf 'host_name=%s\nmachine_id=%s\ndocker_engine_id=%s\n' "$probe_host_name" "$probe_machine_id" "$probe_engine_id"
    printf 'legacy_caddy_container_id=%s\nlegacy_caddy_project=%s\nlegacy_caddy_config_image=%s\nlegacy_caddy_image_id=%s\nlegacy_caddy_repo_digest=%s\nlegacy_caddy_data_volume=%s\nlegacy_caddy_data_volume_fingerprint_sha256=%s\nlegacy_caddy_config_volume=%s\nlegacy_caddy_config_volume_fingerprint_sha256=%s\nlegacy_caddy_network=%s\nlegacy_caddy_network_id=%s\nlegacy_caddy_http_port=80\nlegacy_caddy_https_port=443\nlegacy_caddy_state=stopped\n' "$legacy_id" "$legacy_project" "$legacy_config_image" "$legacy_image_id" "$legacy_repo_digest" "$caddy_data_volume" "$data_volume_fingerprint" "$caddy_config_volume" "$config_volume_fingerprint" "$edge_network" "$network_id"
    printf 'edge_env_binding=%s\ncaddyfile_binding=%s\nedge_compose_binding=%s\npublic_ca_binding=%s\n' "$edge_env_binding" "$caddyfile_binding" "$edge_compose_binding" "$public_ca_binding"
  } >"$legacy_body"
  chmod 400 "$active_body" "$legacy_body"
}

load_transaction() {
  local tx_header tx_operation tx_revision tx_active tx_legacy
  require_protected_file "$txn_journal" || return 1
  tx_header=$(sed -n '1p' "$txn_journal")
  [ "$tx_header" = '# dirextalk-edge-adoption-journal-v1' ] || fail "transaction journal header is invalid" || return 1
  tx_operation=$(read_kv "$txn_journal" operation_id) || return 1
  tx_revision=$(read_kv "$txn_journal" revision) || return 1
  [ "$tx_operation" = "$operation" ] && [ "$tx_revision" = "$revision" ] || fail "transaction operation/revision mismatch" || return 1
  tx_active=$(read_kv "$txn_journal" active_receipt) || return 1
  tx_legacy=$(read_kv "$txn_journal" legacy_snapshot) || return 1
  [ "$tx_active" = "$active_receipt" ] && [ "$tx_legacy" = "$legacy_snapshot" ] || fail "transaction receipt path mismatch" || return 1
  [ "$(read_kv "$txn_journal" owner_uid)" = "$(id -u)" ] || fail "transaction owner mismatch" || return 1
  [ "$(read_kv "$txn_journal" host_name)" = "$probe_host_name" ] || fail "transaction host binding mismatch" || return 1
  [ "$(read_kv "$txn_journal" machine_id)" = "$probe_machine_id" ] || fail "transaction machine binding mismatch" || return 1
  [ "$(read_kv "$txn_journal" docker_engine_id)" = "$probe_engine_id" ] || fail "transaction Engine binding mismatch" || return 1
  [ "$(read_kv "$txn_journal" legacy_caddy_container_id)" = "$legacy_id" ] || return 1
  [ "$(read_kv "$txn_journal" candidate_caddy_project)" = "$edge_project" ] || return 1
  candidate_id=$(read_kv "$txn_journal" candidate_caddy_container_id) || return 1
  candidate_image_id=$(read_kv "$txn_journal" candidate_caddy_image_id) || return 1
  candidate_repo_digest=$(read_kv "$txn_journal" candidate_caddy_repo_digest) || return 1
  [ "$(read_kv "$txn_journal" candidate_caddy_image)" = "$caddy_image" ] || return 1
  [ "$(read_kv "$txn_journal" public_network_name)" = "$edge_network" ] || return 1
  [ "$(read_kv "$txn_journal" public_network_id)" = "$network_id" ] || return 1
  [ "$(read_kv "$txn_journal" public_network_labels_sha256)" = "$network_labels_hash" ] || return 1
  [ "$(read_kv "$txn_journal" caddy_data_volume)" = "$caddy_data_volume" ] || return 1
  [ "$(read_kv "$txn_journal" caddy_data_volume_fingerprint_sha256)" = "$data_volume_fingerprint" ] || return 1
  [ "$(read_kv "$txn_journal" caddy_config_volume)" = "$caddy_config_volume" ] || return 1
  [ "$(read_kv "$txn_journal" caddy_config_volume_fingerprint_sha256)" = "$config_volume_fingerprint" ] || return 1
  [ "$(read_kv "$txn_journal" edge_env_binding)" = "$edge_env_binding" ] || return 1
  [ "$(read_kv "$txn_journal" caddyfile_binding)" = "$caddyfile_binding" ] || return 1
  [ "$(read_kv "$txn_journal" edge_compose_binding)" = "$edge_compose_binding" ] || return 1
  [ "$(read_kv "$txn_journal" public_ca_binding)" = "$public_ca_binding" ] || return 1
  valid_full_container_id "$candidate_id" || fail "transaction candidate ID is not full" || return 1
  require_protected_file "$txn_dir/active.receipt" || return 1
  require_protected_file "$txn_dir/legacy.snapshot" || return 1
}

install_transaction_receipts() {
  install_pending_file "$txn_dir/legacy.snapshot" "$legacy_snapshot" || return 1
  if [ "${DIREXTALK_ADOPT_TEST_CRASH_AFTER_LEGACY_RECEIPT:-false}" = true ]; then
    log "test crash after legacy snapshot install"
    exit 97
  fi
  install_pending_file "$txn_dir/active.receipt" "$active_receipt" || return 1
  if [ "${DIREXTALK_ADOPT_TEST_CRASH_AFTER_ACTIVE_RECEIPT:-false}" = true ]; then
    log "test crash after active receipt install"
    exit 98
  fi
  marker=$(dirname -- "$probe_receipt")/.adopt-edge-committed.$operation
  if [ -e "$marker" ] || [ -L "$marker" ]; then
    require_protected_file "$marker" || return 1
    [ "$(cat -- "$marker")" = "$revision" ] || fail "consumed marker mismatch" || return 1
  else
    marker_tmp=$(mktemp "$txn_dir/.marker.XXXXXX") || return 1
    printf '%s\n' "$revision" >"$marker_tmp"
    chmod 400 "$marker_tmp"
    ln -- "$marker_tmp" "$marker" || return 1
    rm -f -- "$marker_tmp"
  fi
  cleanup_transaction || fail "transaction cleanup failed" || return 1
}

recover_transaction() {
  local old_state candidate_state old_file candidate_file
  txn_dir=$(dirname -- "$probe_receipt")/.adopt-edge-txn.$operation
  txn_journal=$txn_dir/journal
  load_transaction || return 1
  revalidate_bound_files_and_host || return 1
  revalidate_objects || return 1
  old_file=$tmp_dir/recovery-legacy.json
  candidate_file=$tmp_dir/recovery-candidate.json
  inspect_json "$legacy_id" "$old_file" || fail "recovery legacy exact ID unavailable" || return 1
  inspect_json "$candidate_id" "$candidate_file" || fail "recovery candidate exact ID unavailable" || return 1
  old_state=$(container_state "$old_file")
  candidate_state=$(container_state "$candidate_file")
  case "$old_state:$candidate_state" in
    exited:running)
      revalidate_bound_files_and_host || return 1
      revalidate_objects || return 1
      revalidate_candidate running || return 1
      [ "$(container_health "$tmp_dir/candidate-revalidate.json")" = healthy ] || return 1
      verify_public_after_switch || return 1
      install_transaction_receipts
      ;;
    exited:created|exited:exited)
      revalidate_bound_files_and_host || return 1
      revalidate_objects || return 1
      revalidate_candidate "$candidate_state" || return 1
      "$docker_bin" start "$candidate_id" >/dev/null 2>&1 || return 1
      wait_candidate_healthy || return 1
      verify_public_after_switch || return 1
      install_transaction_receipts
      ;;
    running:created|running:exited)
      revalidate_bound_files_and_host || return 1
      revalidate_objects || return 1
      revalidate_candidate "$candidate_state" || return 1
      remove_candidate_exact || return 1
      cleanup_transaction || return 1
      fail "recovery found an uncommitted candidate; legacy edge left public" || return 1
      ;;
    *)
      fail "recovery found an uncertain legacy/candidate state" || return 1
      ;;
  esac
}

probe_public() {
  local curl_common=(--fail --silent --show-error --max-time 20 --resolve "${edge_domain}:443:127.0.0.1")
  verify_public_ca_binding || fail "public CA identity changed" || return 1
  [ -z "${public_ca:-}" ] || curl_common+=(--cacert "$public_ca")
  "$curl_bin" "${curl_common[@]}" --path-as-is "https://${edge_domain}${public_health_path}" >/dev/null 2>&1 || fail "public health check failed" || return 1
  "$curl_bin" "${curl_common[@]}" --path-as-is "https://${edge_domain}${well_known_path}" >/dev/null 2>&1 || fail "public Matrix well-known check failed" || return 1
}

probe_read_only_candidate() {
  local ids
  compose_edge config --quiet >/dev/null 2>&1 || fail "edge Compose render failed" || return 1
  compose_edge config --format json >"$tmp_dir/edge-compose.json" 2>/dev/null || fail "edge Compose JSON render failed" || return 1
  "$jq_bin" -e --arg project "$edge_project" --arg image "$caddy_image" --arg network "$edge_network" \
    --arg data "$caddy_data_volume" --arg config "$caddy_config_volume" '
      .name == $project and .services.caddy.image == $image and
      (.services.caddy.networks == ["message_public"] or (.services.caddy.networks | keys) == ["message_public"]) and
      ([.services.caddy.ports[] | (.published|tostring)] | sort) == ["443", "80"] and
      .networks.message_public.name == $network and .networks.message_public.external == true and
      .volumes.caddy_data.name == $data and .volumes.caddy_data.external == true and
      .volumes.caddy_config.name == $config and .volumes.caddy_config.external == true and
      .services.caddy.read_only == true and (.services.caddy.cap_drop | index("ALL")) != null and
      (.services.caddy.security_opt | index("no-new-privileges:true")) != null and
      ((.services.caddy.healthcheck.test // []) | length) > 1 and
      ((.services.caddy.healthcheck.test | join(" ")) | test("wget|curl"))
    ' "$tmp_dir/edge-compose.json" >/dev/null 2>&1 || fail "edge Compose is not a hardened digest-pinned candidate" || return 1
  ids=$(compose_edge ps -aq caddy 2>/dev/null) || fail "cannot inspect candidate Compose project" || return 1
  [ -z "$ids" ] || fail "candidate Compose project already contains a Caddy container" || return 1
}

probe() {
  [ "$#" -eq 5 ] || usage
  local first=$1 second=$2
  if [ -f "$first" ] && valid_full_container_id "$second"; then
    edge_env=$first; legacy_id=$second; probe_receipt=$3; operation=$4; revision=$5
  elif valid_full_container_id "$first" && [ -f "$second" ]; then
    legacy_id=$first; edge_env=$second; probe_receipt=$3; operation=$4; revision=$5
  else
    fail "probe requires edge env and a full legacy Caddy ID" || return 1
  fi
  valid_token "$operation" && valid_token "$revision" || fail "operation and revision must be safe identifiers" || return 1
  require_new_path "$probe_receipt" || return 1
  ensure_output_parent "$probe_receipt" || return 1
  tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-edge-adopt-probe.XXXXXX")
  chmod 700 "$tmp_dir"
  edge_env_binding=$(file_binding "$edge_env") || return 1
  read_edge_env || return 1
  verify_file_binding "$edge_env" "$edge_env_binding" || fail "edge env identity changed during probe" || return 1
  public_ca_binding=$(public_ca_file_binding) || return 1
  verify_public_ca_binding || fail "public CA identity changed during probe" || return 1
  valid_full_container_id "$legacy_id" || fail "legacy Caddy ID must be the full 64-character ID" || return 1

  identity=$(machine_identity) || return 1
  host_name=${identity%%|*}; machine_id=${identity#*|}
  engine_id=$(docker_engine_identity) || fail "Docker Engine identity unavailable" || return 1
  inspect_json "$legacy_id" "$tmp_dir/legacy.json" || fail "legacy Caddy inspect failed" || return 1
  [ "$(container_id_from_json "$tmp_dir/legacy.json")" = "$legacy_id" ] || fail "legacy Caddy inspect returned a different ID" || return 1
  legacy_config_image=$(container_config_image "$tmp_dir/legacy.json")
  legacy_image_id=$(container_image_id "$tmp_dir/legacy.json")
  legacy_project=$("$jq_bin" -r '.[0].Config.Labels["com.docker.compose.project"] // ""' "$tmp_dir/legacy.json")
  legacy_service=$("$jq_bin" -r '.[0].Config.Labels["com.docker.compose.service"] // ""' "$tmp_dir/legacy.json")
  [ -n "$legacy_config_image" ] && [ -n "$legacy_image_id" ] || fail "legacy image identity is incomplete" || return 1
  valid_safe_id "$legacy_config_image" || fail "legacy Config.Image is invalid" || return 1
  valid_legacy_caddy_config_image "$legacy_config_image" || fail "legacy Config.Image is not an approved Docker Hub Caddy reference" || return 1
  valid_safe_id "$legacy_image_id" || fail "legacy image ID is invalid" || return 1
  valid_project "$legacy_project" || fail "legacy Compose project label is invalid" || return 1
  [ "$legacy_service" = caddy ] || fail "legacy Compose service label is not caddy" || return 1
  [ "$edge_project" != "$legacy_project" ] || fail "candidate edge project must differ from legacy project" || return 1
  verify_ports_80_443 "$tmp_dir/legacy.json" || fail "legacy Caddy must own ports 80 and 443" || return 1
  [ "$(container_state "$tmp_dir/legacy.json")" = running ] || fail "legacy Caddy is not running" || return 1
  legacy_health=$(classify_legacy_health "$tmp_dir/legacy.json") || fail "legacy Caddy health state is unsupported" || return 1
  verify_legacy_ready "$tmp_dir/legacy.json" || fail "legacy Caddy readiness changed during probe" || return 1
  verify_volume_mounts "$tmp_dir/legacy.json" "$caddy_data_volume" "$caddy_config_volume" || fail "legacy Caddy volume bindings differ from edge env" || return 1
  "$jq_bin" -e --arg network "$edge_network" '.[0].NetworkSettings.Networks[$network] != null' "$tmp_dir/legacy.json" >/dev/null 2>&1 || fail "legacy Caddy is not attached to the public network" || return 1

  image_json "$legacy_image_id" "$tmp_dir/legacy-image.json" || fail "legacy image inspect failed" || return 1
  legacy_repo_digest=$(legacy_caddy_repo_digest_from_image_json "$tmp_dir/legacy-image.json")
  [ -n "$legacy_repo_digest" ] || fail "legacy image has no bound RepoDigest" || return 1
  [ "$(image_id_from_image_json "$tmp_dir/legacy-image.json")" = "$legacy_image_id" ] || fail "legacy image ID differs from container identity" || return 1

  network_json "$edge_network" "$tmp_dir/network.json" || fail "public network inspect failed" || return 1
  network_id=$(network_id_from_json "$tmp_dir/network.json")
  network_name=$(network_name_from_json "$tmp_dir/network.json")
  network_labels_hash=$(labels_hash "$tmp_dir/network.json")
  [ -n "$network_id" ] && [ "$network_name" = "$edge_network" ] || fail "public network identity is incomplete" || return 1
  "$jq_bin" -e --arg id "$legacy_id" '.[0].Containers[$id] != null' "$tmp_dir/network.json" >/dev/null 2>&1 || fail "legacy Caddy is not present in the public network object" || return 1

  volume_json "$caddy_data_volume" "$tmp_dir/data-volume.json" || fail "Caddy data volume inspect failed" || return 1
  volume_json "$caddy_config_volume" "$tmp_dir/config-volume.json" || fail "Caddy config volume inspect failed" || return 1
  data_volume_fingerprint=$(volume_fingerprint_from_json "$tmp_dir/data-volume.json") || fail "Caddy data volume identity metadata is incomplete" || return 1
  config_volume_fingerprint=$(volume_fingerprint_from_json "$tmp_dir/config-volume.json") || fail "Caddy config volume identity metadata is incomplete" || return 1

  caddyfile_binding=$(file_binding "$caddyfile") || return 1
  edge_compose_binding=$(file_binding "$edge_compose") || return 1
  probe_read_only_candidate || return 1
  probe_public || return 1

  legacy_message_stack=${legacy_message_stack:-legacy-$legacy_project}
  valid_project "$legacy_message_stack" || fail "derived legacy message stack name is invalid" || return 1
  body=$tmp_dir/body
  {
    printf 'owner_uid=%s\n' "$(id -u)"
    printf 'host_name=%s\n' "$host_name"
    printf 'machine_id=%s\n' "$machine_id"
    printf 'docker_engine_id=%s\n' "$engine_id"
    printf 'operation_id=%s\n' "$operation"
    printf 'revision=%s\n' "$revision"
    printf 'edge_stack_name=%s\n' "$edge_project"
    printf 'legacy_caddy_container_id=%s\n' "$legacy_id"
    printf 'legacy_caddy_config_image=%s\n' "$legacy_config_image"
    printf 'legacy_caddy_image_id=%s\n' "$legacy_image_id"
    printf 'legacy_caddy_repo_digest=%s\n' "$legacy_repo_digest"
    printf 'legacy_caddy_project=%s\n' "$legacy_project"
    printf 'legacy_caddy_service=%s\n' "$legacy_service"
    printf 'legacy_message_stack_name=%s\n' "$legacy_message_stack"
    printf 'public_network_name=%s\n' "$network_name"
    printf 'public_network_id=%s\n' "$network_id"
    printf 'public_network_labels_sha256=%s\n' "$network_labels_hash"
    printf 'caddy_data_volume=%s\n' "$caddy_data_volume"
    printf 'caddy_data_volume_fingerprint_sha256=%s\n' "$data_volume_fingerprint"
    printf 'caddy_config_volume=%s\n' "$caddy_config_volume"
    printf 'caddy_config_volume_fingerprint_sha256=%s\n' "$config_volume_fingerprint"
    printf 'caddy_image_candidate=%s\n' "$caddy_image"
    printf 'edge_env_dev_inode_uid_mode_sha256=%s\n' "$edge_env_binding"
    printf 'caddyfile_dev_inode_uid_mode_sha256=%s\n' "$caddyfile_binding"
    printf 'edge_compose_dev_inode_uid_mode_sha256=%s\n' "$edge_compose_binding"
    printf 'public_ca_dev_inode_uid_mode_sha256=%s\n' "$public_ca_binding"
    printf 'public_domain=%s\n' "$edge_domain"
    printf 'public_health_path=%s\n' "$public_health_path"
    printf 'well_known_path=%s\n' "$well_known_path"
    printf 'legacy_http_port=80\nlegacy_https_port=443\nlegacy_state=running\nlegacy_health=%s\n' "$legacy_health"
  } >"$body"
  chmod 400 "$body"
  atomic_write_0400 "$probe_receipt" '# dirextalk-edge-adoption-probe-v1' "$body" || return 1
  printf 'edge adoption probe succeeded; protected probe receipt written\n'
}

read_probe() {
  require_regular_owned "$probe_receipt" 400 || return 1
  [ "$(sed -n '1p' "$probe_receipt")" = '# dirextalk-edge-adoption-probe-v1' ] || fail "probe receipt header is invalid" || return 1
  owner_uid=$(read_kv "$probe_receipt" owner_uid) || return 1
  [ "$owner_uid" = "$(id -u)" ] || fail "probe receipt owner mismatch" || return 1
  probe_host_name=$(read_kv "$probe_receipt" host_name) || return 1
  probe_machine_id=$(read_kv "$probe_receipt" machine_id) || return 1
  probe_engine_id=$(read_kv "$probe_receipt" docker_engine_id) || return 1
  operation=$(read_kv "$probe_receipt" operation_id) || return 1
  revision=$(read_kv "$probe_receipt" revision) || return 1
  probe_edge_project=$(read_kv "$probe_receipt" edge_stack_name) || return 1
  legacy_id=$(read_kv "$probe_receipt" legacy_caddy_container_id) || return 1
  legacy_config_image=$(read_kv "$probe_receipt" legacy_caddy_config_image) || return 1
  legacy_image_id=$(read_kv "$probe_receipt" legacy_caddy_image_id) || return 1
  legacy_repo_digest=$(read_kv "$probe_receipt" legacy_caddy_repo_digest) || return 1
  legacy_project=$(read_kv "$probe_receipt" legacy_caddy_project) || return 1
  legacy_service=$(read_kv "$probe_receipt" legacy_caddy_service) || return 1
  legacy_message_stack=$(read_kv "$probe_receipt" legacy_message_stack_name) || return 1
  edge_network=$(read_kv "$probe_receipt" public_network_name) || return 1
  bound_network=$edge_network
  network_id=$(read_kv "$probe_receipt" public_network_id) || return 1
  network_labels_hash=$(read_kv "$probe_receipt" public_network_labels_sha256) || return 1
  caddy_data_volume=$(read_kv "$probe_receipt" caddy_data_volume) || return 1
  data_volume_fingerprint=$(read_kv "$probe_receipt" caddy_data_volume_fingerprint_sha256) || return 1
  caddy_config_volume=$(read_kv "$probe_receipt" caddy_config_volume) || return 1
  config_volume_fingerprint=$(read_kv "$probe_receipt" caddy_config_volume_fingerprint_sha256) || return 1
  caddy_image=$(read_kv "$probe_receipt" caddy_image_candidate) || return 1
  edge_env_binding=$(read_kv "$probe_receipt" edge_env_dev_inode_uid_mode_sha256) || return 1
  caddyfile_binding=$(read_kv "$probe_receipt" caddyfile_dev_inode_uid_mode_sha256) || return 1
  edge_compose_binding=$(read_kv "$probe_receipt" edge_compose_dev_inode_uid_mode_sha256) || return 1
  public_ca_binding=$(read_kv "$probe_receipt" public_ca_dev_inode_uid_mode_sha256) || return 1
  edge_domain=$(read_kv "$probe_receipt" public_domain) || return 1
  public_health_path=$(read_kv "$probe_receipt" public_health_path) || return 1
  well_known_path=$(read_kv "$probe_receipt" well_known_path) || return 1
  bound_legacy_message_stack=$legacy_message_stack
  [ "$(read_kv "$probe_receipt" legacy_state)" = running ] || fail "probe legacy state is not running" || return 1
  legacy_health=$(read_kv "$probe_receipt" legacy_health) || return 1
  case "$legacy_health" in
    healthy|unconfigured-public-probe) ;;
    *) fail "probe legacy health mode is invalid" || return 1 ;;
  esac
  valid_full_container_id "$legacy_id" || fail "probe legacy ID is not full" || return 1
  valid_project "$probe_edge_project" && valid_project "$legacy_project" && valid_project "$legacy_message_stack" || return 1
  valid_token "$operation" && valid_token "$revision" || fail "probe operation/revision is invalid" || return 1
  valid_image "$caddy_image" || return 1
}

revalidate_bound_files_and_host() {
  local identity host_name machine_id engine_id
  identity=$(machine_identity) || return 1
  host_name=${identity%%|*}; machine_id=${identity#*|}
  [ "$host_name" = "$probe_host_name" ] || fail "host identity changed since probe" || return 1
  [ "$machine_id" = "$probe_machine_id" ] || fail "machine identity changed since probe" || return 1
  engine_id=$(docker_engine_identity) || fail "Docker Engine identity unavailable" || return 1
  [ "$engine_id" = "$probe_engine_id" ] || fail "Docker Engine identity changed since probe" || return 1
  verify_file_binding "$edge_env" "$edge_env_binding" || fail "edge env identity changed since probe" || return 1
  read_edge_env || return 1
  verify_file_binding "$edge_env" "$edge_env_binding" || fail "edge env identity changed during revalidation" || return 1
  verify_public_ca_binding || fail "public CA identity changed since probe" || return 1
  [ "$edge_project" = "$probe_edge_project" ] || fail "edge project changed since probe" || return 1
  [ "$edge_network" = "$bound_network" ] || fail "public network changed since probe" || return 1
  [ "${legacy_message_stack:-legacy-$legacy_project}" = "$bound_legacy_message_stack" ] || fail "legacy message stack binding changed since probe" || return 1
  verify_file_binding "$caddyfile" "$caddyfile_binding" || fail "Caddyfile identity changed since probe" || return 1
  verify_file_binding "$edge_compose" "$edge_compose_binding" || fail "edge Compose identity changed since probe" || return 1
}

revalidate_objects() {
  inspect_json "$legacy_id" "$tmp_dir/legacy-pre.json" || fail "legacy Caddy is unavailable" || return 1
  [ "$(container_id_from_json "$tmp_dir/legacy-pre.json")" = "$legacy_id" ] || fail "legacy Caddy ID changed" || return 1
  [ "$(container_config_image "$tmp_dir/legacy-pre.json")" = "$legacy_config_image" ] || fail "legacy Config.Image changed" || return 1
  [ "$(container_image_id "$tmp_dir/legacy-pre.json")" = "$legacy_image_id" ] || fail "legacy image ID changed" || return 1
  "$jq_bin" -e --arg project "$legacy_project" --arg service "$legacy_service" --arg network "$edge_network" \
    '.[0].Config.Labels["com.docker.compose.project"] == $project and .[0].Config.Labels["com.docker.compose.service"] == $service and .[0].NetworkSettings.Networks[$network] != null' \
    "$tmp_dir/legacy-pre.json" >/dev/null 2>&1 || fail "legacy Compose/network identity changed" || return 1
  verify_ports_80_443 "$tmp_dir/legacy-pre.json" || fail "legacy Caddy port identity changed" || return 1
  verify_volume_mounts "$tmp_dir/legacy-pre.json" "$caddy_data_volume" "$caddy_config_volume" || fail "legacy Caddy volume bindings changed" || return 1
  network_json "$edge_network" "$tmp_dir/network-pre.json" || return 1
  verify_network_binding "$tmp_dir/network-pre.json" "$network_id" "$edge_network" "$network_labels_hash" || fail "public network object identity changed" || return 1
  "$jq_bin" -e --arg id "$legacy_id" '.[0].Containers[$id] != null' "$tmp_dir/network-pre.json" >/dev/null 2>&1 || fail "legacy Caddy left public network" || return 1
  volume_json "$caddy_data_volume" "$tmp_dir/data-volume-pre.json" || return 1
  volume_json "$caddy_config_volume" "$tmp_dir/config-volume-pre.json" || return 1
  [ "$(volume_fingerprint_from_json "$tmp_dir/data-volume-pre.json")" = "$data_volume_fingerprint" ] || fail "Caddy data volume identity changed" || return 1
  [ "$(volume_fingerprint_from_json "$tmp_dir/config-volume-pre.json")" = "$config_volume_fingerprint" ] || fail "Caddy config volume identity changed" || return 1
  image_json "$legacy_image_id" "$tmp_dir/legacy-image-pre.json" || return 1
  verify_legacy_image_binding "$tmp_dir/legacy-image-pre.json" "$legacy_image_id" "$legacy_repo_digest" || fail "legacy image RepoDigest/image ID changed" || return 1
}

candidate_create_and_verify() {
  local candidate_short candidate_full
  compose_edge create caddy >/dev/null 2>&1 || fail "candidate pre-create failed" || return 1
  candidate_short=$(compose_edge ps -q caddy 2>/dev/null) || fail "candidate ID is unavailable" || return 1
  valid_container_id "$candidate_short" || fail "candidate ID is not a Docker ID" || return 1
  inspect_json "$candidate_short" "$tmp_dir/candidate-pre.json" || fail "candidate inspect failed" || return 1
  candidate_full=$(container_id_from_json "$tmp_dir/candidate-pre.json")
  valid_full_container_id "$candidate_full" || fail "candidate inspect did not return a full ID" || return 1
  case "$candidate_full" in "$candidate_short"*) ;; *) fail "candidate ID changed during inspect" || return 1 ;; esac
  candidate_id=$candidate_full
  "$jq_bin" -e --arg project "$edge_project" --arg image "$caddy_image" --arg network "$edge_network" \
    '.[0].Config.Labels["com.docker.compose.project"] == $project and .[0].Config.Labels["com.docker.compose.service"] == "caddy" and .[0].Config.Image == $image and .[0].NetworkSettings.Networks[$network] != null' \
    "$tmp_dir/candidate-pre.json" >/dev/null 2>&1 || fail "candidate image/labels/network identity mismatch" || return 1
  verify_ports_80_443 "$tmp_dir/candidate-pre.json" || fail "candidate must publish 80 and 443" || return 1
  verify_volume_mounts "$tmp_dir/candidate-pre.json" "$caddy_data_volume" "$caddy_config_volume" || fail "candidate volume identity mismatch" || return 1
  verify_hardened_candidate "$tmp_dir/candidate-pre.json" || fail "candidate hardening/healthcheck assumptions are not met" || return 1
  image_json "$caddy_image" "$tmp_dir/candidate-image.json" || fail "candidate image inspect failed" || return 1
  candidate_image=$(container_image_id "$tmp_dir/candidate-pre.json")
  candidate_image_id=$(image_id_from_image_json "$tmp_dir/candidate-image.json")
  candidate_repo=$(repo_digest_from_image_json "$caddy_image" "$tmp_dir/candidate-image.json")
  [ -n "$candidate_image" ] && [ "$candidate_image" = "$candidate_image_id" ] || fail "candidate image ID differs from image object" || return 1
  [ -n "$candidate_repo" ] || fail "candidate image has no RepoDigest" || return 1
  candidate_repo_digest=$candidate_repo
  candidate_image_id=$candidate_image
  network_json "$edge_network" "$tmp_dir/candidate-network.json" || return 1
  verify_network_binding "$tmp_dir/candidate-network.json" "$network_id" "$edge_network" "$network_labels_hash" || fail "candidate public network object identity mismatch" || return 1
  volume_json "$caddy_data_volume" "$tmp_dir/candidate-data-volume.json" || return 1
  volume_json "$caddy_config_volume" "$tmp_dir/candidate-config-volume.json" || return 1
  [ "$(volume_fingerprint_from_json "$tmp_dir/candidate-data-volume.json")" = "$data_volume_fingerprint" ] || fail "candidate data volume object identity mismatch" || return 1
  [ "$(volume_fingerprint_from_json "$tmp_dir/candidate-config-volume.json")" = "$config_volume_fingerprint" ] || fail "candidate config volume object identity mismatch" || return 1
}

wait_candidate_healthy() {
  local status
  for _ in $(seq 1 60); do
    inspect_json "$candidate_id" "$tmp_dir/candidate-health.json" || return 1
    status=$(container_health "$tmp_dir/candidate-health.json")
    if [ "$status" = healthy ]; then
      "$jq_bin" -e '.[0].State.Status == "running"' "$tmp_dir/candidate-health.json" >/dev/null 2>&1 || return 1
      verify_hardened_candidate "$tmp_dir/candidate-health.json" || return 1
      return 0
    fi
    [ "$status" != unhealthy ] || return 1
    sleep 1
  done
  return 1
}

verify_public_after_switch() {
  probe_public
}

remove_candidate_exact() {
  local candidate_inspect candidate_full after
  [ -n "${candidate_id:-}" ] && valid_full_container_id "$candidate_id" || return 0
  candidate_inspect=$tmp_dir/candidate-pre-stop-rollback.json
  inspect_json "$candidate_id" "$candidate_inspect" || return 1
  candidate_full=$(container_id_from_json "$candidate_inspect")
  [ "$candidate_full" = "$candidate_id" ] || return 1
  "$jq_bin" -e --arg project "$edge_project" --arg image "$caddy_image" --arg network "$edge_network" \
    '.[0].Config.Labels["com.docker.compose.project"] == $project and .[0].Config.Labels["com.docker.compose.service"] == "caddy" and .[0].Config.Image == $image and .[0].NetworkSettings.Networks[$network] != null' \
    "$candidate_inspect" >/dev/null 2>&1 || return 1
  verify_ports_80_443 "$candidate_inspect" || return 1
  verify_volume_mounts "$candidate_inspect" "$caddy_data_volume" "$caddy_config_volume" || return 1
  revalidate_bound_files_and_host || return 1
  revalidate_objects || return 1
  revalidate_candidate || return 1
  if "$docker_bin" rm -f "$candidate_id" >/dev/null 2>&1; then
    return 0
  fi
  # Re-assert the complete immutable binding before retrying with stop after
  # an uncertain rm result.
  revalidate_bound_files_and_host || return 1
  revalidate_objects || return 1
  revalidate_candidate || return 1
  "$docker_bin" stop "$candidate_id" >/dev/null 2>&1 || return 1
  after=$tmp_dir/candidate-after-stop.json
  inspect_json "$candidate_id" "$after" || return 1
  [ "$(container_id_from_json "$after")" = "$candidate_id" ] || return 1
  [ "$(container_state "$after")" != running ]
}

rollback_exact() {
  local candidate_inspect candidate_full old_inspect candidate_cleanup_ok=true
  log "adoption failed; verifying candidate and restoring exact legacy ID"
  if [ -n "${candidate_id:-}" ] && valid_full_container_id "$candidate_id"; then
    candidate_inspect=$tmp_dir/candidate-rollback.json
    if inspect_json "$candidate_id" "$candidate_inspect"; then
      candidate_full=$(container_id_from_json "$candidate_inspect")
      if [ "$candidate_full" = "$candidate_id" ] && "$jq_bin" -e --arg project "$edge_project" --arg image "$caddy_image" --arg network "$edge_network" \
        '.[0].Config.Labels["com.docker.compose.project"] == $project and .[0].Config.Labels["com.docker.compose.service"] == "caddy" and .[0].Config.Image == $image and .[0].NetworkSettings.Networks[$network] != null' \
        "$candidate_inspect" >/dev/null 2>&1 && verify_ports_80_443 "$candidate_inspect" && verify_volume_mounts "$candidate_inspect" "$caddy_data_volume" "$caddy_config_volume" && verify_hardened_candidate "$candidate_inspect"; then
        if ! remove_candidate_exact; then
          candidate_cleanup_ok=false
        fi
      else
        log "refusing to remove an unverified candidate identity"
        candidate_cleanup_ok=false
      fi
    else
      candidate_cleanup_ok=false
    fi
  fi
  [ "$candidate_cleanup_ok" = true ] || return 1
  revalidate_bound_files_and_host || return 1
  revalidate_objects || return 1
  old_inspect=$tmp_dir/legacy-rollback.json
  if ! inspect_json "$legacy_id" "$old_inspect" || ! "$jq_bin" -e --arg id "$legacy_id" --arg project "$legacy_project" --arg image "$legacy_config_image" --arg network "$edge_network" \
    '.[0].Id == $id and .[0].Config.Labels["com.docker.compose.project"] == $project and .[0].Config.Labels["com.docker.compose.service"] == "caddy" and .[0].Config.Image == $image and .[0].NetworkSettings.Networks[$network] != null' \
    "$old_inspect" >/dev/null 2>&1; then
    fail "legacy ID changed before rollback start" || true
    return 1
  fi
  revalidate_bound_files_and_host || return 1
  revalidate_objects || return 1
  "$docker_bin" start "$legacy_id" >/dev/null 2>&1 || return 1
  inspect_json "$legacy_id" "$tmp_dir/legacy-restored.json" || return 1
  verify_container_identity "$tmp_dir/legacy-restored.json" "$legacy_id" "$legacy_project" "$legacy_config_image" "$edge_network" "$caddy_data_volume" "$caddy_config_volume" false || return 1
  verify_legacy_ready "$tmp_dir/legacy-restored.json" || return 1
  verify_public_after_switch || return 1
}

stop_legacy_exact() {
  local before=$tmp_dir/legacy-before-stop.json after=$tmp_dir/legacy-after-stop.json
  stop_failure_cleanup_safe=false
  revalidate_bound_files_and_host || return 1
  revalidate_objects || return 1
  inspect_json "$legacy_id" "$before" || return 1
  [ "$(container_id_from_json "$before")" = "$legacy_id" ] || return 1
  verify_legacy_ready "$before" || return 1
  probe_public || return 1
  if "$docker_bin" stop "$legacy_id" >/dev/null 2>&1; then
    stop_status=0
  else
    stop_status=$?
  fi
  inspect_json "$legacy_id" "$after" || return 1
  [ "$(container_id_from_json "$after")" = "$legacy_id" ] || return 1
  case "$(container_state "$after"):$stop_status" in
    exited:0|created:0|exited:*) return 0 ;;
    running:*)
      remove_candidate_exact || return 1
      stop_failure_cleanup_safe=true
      fail "legacy stop returned uncertain while exact legacy remained running" || return 1
      ;;
    *)
      fail "legacy stop left an uncertain exact-container state" || return 1
      ;;
  esac
}

start_candidate_exact() {
  revalidate_bound_files_and_host || return 1
  revalidate_objects || return 1
  revalidate_candidate created || return 1
  "$docker_bin" start "$candidate_id" >/dev/null 2>&1 || return 1
  wait_candidate_healthy || return 1
  verify_public_after_switch || return 1
}

commit() {
  [ "$#" -eq 6 ] || usage
  edge_env=$1; probe_receipt=$2; confirm_operation=$3; confirm_revision=$4; active_receipt=$5; legacy_snapshot=$6
  [ -f "$edge_env" ] || fail "edge env is unavailable" || return 1
  read_probe || return 1
  [ "$confirm_operation" = "$operation" ] || fail "operation confirmation does not match probe" || return 1
  [ "$confirm_revision" = "$revision" ] || fail "revision confirmation does not match probe" || return 1
  [ "$active_receipt" != "$legacy_snapshot" ] || fail "active receipt and legacy snapshot must differ" || return 1
  txn_dir=$(dirname -- "$probe_receipt")/.adopt-edge-txn.$operation
  txn_journal=$txn_dir/journal
  marker=$(dirname -- "$probe_receipt")/.adopt-edge-committed.$operation
  txn_exists=false
  [ -e "$txn_dir" ] || [ -L "$txn_dir" ] && txn_exists=true
  if [ "$txn_exists" = false ]; then
    require_new_path "$active_receipt" || return 1
    require_new_path "$legacy_snapshot" || return 1
  fi
  ensure_output_parent "$active_receipt" || return 1
  ensure_output_parent "$legacy_snapshot" || return 1
  # Share the public-edge scope lock with cutover-edge.sh so adoption and
  # cutover cannot mutate the same public edge concurrently.
  lock_file=$(dirname -- "$active_receipt")/.dirextalk-edge-public.lock
  require_private_dir "$(dirname -- "$active_receipt")" false || return 1
  if [ -e "$lock_file" ] || [ -L "$lock_file" ]; then
    require_regular_owned "$lock_file" 600 || return 1
  else
    umask 077
    : >"$lock_file" || return 1
    chmod 600 "$lock_file" || return 1
  fi
  require_regular_owned "$lock_file" 600 || return 1
  exec {lock_fd}>>"$lock_file" || return 1
  if ! "$flock_bin" -n "$lock_fd"; then
    eval "exec ${lock_fd}>&-" 2>/dev/null || true
    lock_fd=''
    fail "another edge adoption operation is in progress" || return 1
  fi
  tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-edge-adopt-commit.XXXXXX")
  chmod 700 "$tmp_dir"
  if [ "$txn_exists" = true ]; then
    [ -d "$txn_dir" ] && [ ! -L "$txn_dir" ] || fail "transaction path is not a private directory" || return 1
    read_edge_env || return 1
    recover_transaction
    exit $?
  fi
  [ ! -e "$marker" ] && [ ! -L "$marker" ] || fail "probe operation has already been committed" || return 1
  revalidate_bound_files_and_host || return 1
  [ "$edge_network" = "$(read_kv "$probe_receipt" public_network_name)" ] || fail "public network changed since probe" || return 1
  [ "$caddy_data_volume" = "$(read_kv "$probe_receipt" caddy_data_volume)" ] || fail "Caddy data volume changed since probe" || return 1
  [ "$caddy_config_volume" = "$(read_kv "$probe_receipt" caddy_config_volume)" ] || fail "Caddy config volume changed since probe" || return 1
  [ "$caddy_image" = "$(read_kv "$probe_receipt" caddy_image_candidate)" ] || fail "candidate image changed since probe" || return 1
  revalidate_objects || return 1
  probe_read_only_candidate || return 1
  if ! candidate_create_and_verify; then
    if ! remove_candidate_exact; then
      fail "candidate verification failed and its exact identity could not be removed"
    fi
    return 1
  fi
  write_transaction || return 1
  if ! stop_legacy_exact; then
    if [ "$stop_failure_cleanup_safe" = true ]; then
      cleanup_transaction 2>/dev/null || true
    fi
    return 1
  fi
  if ! start_candidate_exact; then
    if ! rollback_exact; then
      fail "candidate start/public verification failed and exact rollback failed"
    else
      cleanup_transaction 2>/dev/null || true
      fail "candidate start/public verification failed; exact legacy Caddy restored"
    fi
    return 1
  fi
  install_transaction_receipts
  printf 'edge adoption commit succeeded; legacy snapshot and active receipt written\n'
}

case "$mode" in
  probe) probe "$@" ;;
  commit) commit "$@" ;;
  *) usage ;;
esac
