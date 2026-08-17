#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
die() { printf 'split-agent update: %s\n' "$*" >&2; exit 1; }
negative() { printf 'split-agent update: %s\n' "$*" >&2; exit 3; }
usage() { printf 'usage: %s OUTPUT_DIR target_version minimum_server_version\n' "${0##*/}" >&2; exit 2; }
canonical_version() { printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; }
semver_ge() { local a=${1#v} b=${2#v} a1 a2 a3 b1 b2 b3; IFS=. read -r a1 a2 a3 <<<"$a"; IFS=. read -r b1 b2 b3 <<<"$b"; [ "$a1" -gt "$b1" ] || { [ "$a1" -eq "$b1" ] && { [ "$a2" -gt "$b2" ] || { [ "$a2" -eq "$b2" ] && [ "$a3" -ge "$b3" ]; }; }; }; }
read_pair() { local file=$1 key=$2 count value; count=$(awk -F= -v k="$key" '$0 !~ /^[[:space:]]*#/ && index($0,k "=")==1 {n++} END {print n+0}' "$file"); [ "$count" -eq 1 ] || die "$file must contain exactly one $key"; value=$(awk -F= -v k="$key" 'index($0,k "=")==1 {print substr($0,length(k)+2); exit}' "$file"); [ -n "$value" ] || die "$key is empty"; printf '%s' "$value"; }
recovery_runtime_valid() { case "$1:$2:$3" in agent:running:healthy|agent:running:unhealthy|agent:restarting:healthy|agent:restarting:unhealthy|extension-runner:running:healthy|extension-runner:running:unhealthy|core-runner:running:healthy) return 0;; *) return 1;; esac; }

[ "$#" -eq 3 ] || usage
out=$(readlink -m -- "$1"); target_version=$2; minimum_server_version=$3
if ! canonical_version "$target_version" || ! canonical_version "$minimum_server_version"; then
  usage
fi
required_owner=0; [ "${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}" != true ] || required_owner=$(id -u)
[ -d "$out" ] && [ ! -L "$out" ] && [ "$(stat -c '%a:%u' "$out")" = "700:$required_owner" ] || die 'OUTPUT_DIR must be a protected deployment directory'
env_file=$out/.env; manifest=$out/.manifest; receipt=$out/.cleanup-receipt
for file in "$env_file" "$manifest" "$receipt"; do [ -f "$file" ] && [ ! -L "$file" ] && [ "$(stat -c '%a:%u' "$file")" = "400:$required_owner" ] || die "invalid protected control file: $file"; done
command -v docker >/dev/null 2>&1 || die 'docker is required'; command -v jq >/dev/null 2>&1 || die 'jq is required'; command -v flock >/dev/null 2>&1 || die 'flock is required'
lock=$out/.agent-update.lock; umask 077; : >>"$lock"; chmod 600 "$lock"; exec 9<>"$lock"; flock -n 9 || die 'another Agent update is running'
[ "$(read_pair "$manifest" compose_mode)" = production ] || negative 'Agent updates apply only to production stacks'
[ "$(read_pair "$receipt" state)" = complete ] || die 'cleanup receipt is incomplete'
stack=$(read_pair "$manifest" stack_name); image=$(read_pair "$env_file" DIREXTALK_AGENT_IMAGE); message_image=$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_IMAGE)
recorded_version=$(read_pair "$env_file" DIREXTALK_AGENT_VERSION)
canonical_version "$recorded_version" || die 'recorded Agent version is invalid'
[ "$image" = "docker.io/dirextalk/agent:$recorded_version" ] || die 'Agent image does not match its recorded version tag'
target_image="docker.io/dirextalk/agent:$target_version"
local_image_ref=${DIREXTALK_AGENT_LOCAL_IMAGE_REF:-}
[ "$(id -u)" -eq 0 ] || [ -z "$local_image_ref" ] || [ "${DIREXTALK_AGENT_UPDATE_TEST_FIXTURE:-false}" = true ] || die 'local image apply requires root'
[[ "$local_image_ref" != *$'\n'* ]] || die 'local image ref contains a newline'
compose=(docker compose --env-file "$env_file" -f "$stack_dir/compose.yaml" -f "$stack_dir/compose.production.yaml" --project-name "$stack")

agent_config=$(read_pair "$env_file" DIREXTALK_AGENT_CONFIG_FILE)
[ "$agent_config" = "$out/agent-config.yaml" ] || die 'Agent config path is outside the protected deployment directory'
[ -f "$agent_config" ] && [ ! -L "$agent_config" ] && [ "$(stat -c '%a:%u' "$agent_config")" = "400:$required_owner" ] || die 'invalid protected Agent config file'
retired_config_pattern='^(core_aws_enabled|capability_enabled|capability_grpc_listen|capability_ca_cert_file|capability_tls_cert_file|capability_tls_key_file|capability_token_file|capability_peer_common_name|capability_peer_instance_id|capability_max_concurrent_query|capability_max_concurrent_watch):'
! grep -Eq "$retired_config_pattern" "$agent_config" || die 'Agent config contains retired Message Server gateway settings'
[ "$(grep -Fxc 'agent_http_enabled: true' "$agent_config" || true)" -eq 1 ] || die 'Agent config must contain exactly one agent_http_enabled: true'
[ "$(grep -Fxc 'agent_http_listen: 0.0.0.0:8082' "$agent_config" || true)" -eq 1 ] || die 'Agent config must contain exactly one agent_http_listen: 0.0.0.0:8082'
[ "$(grep -Ec '^agent_http_enabled:' "$agent_config" || true)" -eq 1 ] || die 'Agent config contains an invalid agent_http_enabled setting'
[ "$(grep -Ec '^agent_http_listen:' "$agent_config" || true)" -eq 1 ] || die 'Agent config contains an invalid agent_http_listen setting'

container_count=$(read_pair "$receipt" container.count); message_id=
declare -A old_ids=() indexes=() receipt_projects=()
for ((index=0; index<container_count; index++)); do service=$(read_pair "$receipt" "container.$index.service"); case "$service" in message-server) message_id=$(read_pair "$receipt" "container.$index.id"); indexes[$service]=$index; receipt_projects[$service]=$(read_pair "$receipt" "container.$index.project");; agent|extension-runner|core-runner) old_ids[$service]=$(read_pair "$receipt" "container.$index.id"); indexes[$service]=$index; receipt_projects[$service]=$(read_pair "$receipt" "container.$index.project");; esac; done
[ -n "$message_id" ] && [ "${#old_ids[@]}" -eq 3 ] || die 'cleanup receipt lacks application services'
for service in message-server agent extension-runner core-runner; do [ "${receipt_projects[$service]}" = "$stack" ] || die "$service receipt project does not match the deployment stack"; done

recorded_available=true
agent_recovered_to_target=false
docker inspect "$message_id" >/dev/null 2>&1 || recorded_available=false
for service in agent extension-runner core-runner; do docker inspect "${old_ids[$service]}" >/dev/null 2>&1 || recorded_available=false; done
if [ "$recorded_available" = false ]; then
  declare -A recovered_ids=() recovered_statuses=()
  recovered_image_id= recovered_config_image=
  recovered_message_id=$("${compose[@]}" ps -q message-server 2>/dev/null) || die 'could not resolve current message-server container'
  printf '%s\n' "$recovered_message_id" | grep -Eq '^[0-9a-f]{64}$' || die 'current message-server container identity is invalid'
  data=$(docker inspect "$recovered_message_id" 2>/dev/null) || die 'current message-server container is unavailable'
  jq -e --arg id "$recovered_message_id" --arg project "$stack" --arg image "$message_image" '
    length == 1 and .[0].Id == $id and
    .[0].Config.Labels["com.docker.compose.project"] == $project and
    .[0].Config.Labels["com.docker.compose.service"] == "message-server" and
    .[0].Config.Image == $image and .[0].State.Status == "running" and
    .[0].State.Health.Status == "healthy"
  ' <<<"$data" >/dev/null || die 'current message-server identity or health does not match the receipt-bound compose service'
  message_id=$recovered_message_id
  for service in agent extension-runner core-runner; do
    recovered_ids[$service]=$("${compose[@]}" ps -q "$service" 2>/dev/null) || die "could not resolve current $service container"
    printf '%s\n' "${recovered_ids[$service]}" | grep -Eq '^[0-9a-f]{64}$' || die "current $service container identity is invalid"
    data=$(docker inspect "${recovered_ids[$service]}" 2>/dev/null) || die "current $service container is unavailable"
    jq -e --arg id "${recovered_ids[$service]}" --arg project "$stack" --arg service "$service" '
      length == 1 and .[0].Id == $id and
      .[0].Config.Labels["com.docker.compose.project"] == $project and
      .[0].Config.Labels["com.docker.compose.service"] == $service
    ' <<<"$data" >/dev/null || die "current $service container identity does not match the receipt-bound compose service"
    config_image=$(jq -r '.[0].Config.Image // empty' <<<"$data")
    [ -n "$config_image" ] || die "current $service image reference is missing"
    [ -z "$recovered_config_image" ] && recovered_config_image=$config_image
    [ "$config_image" = "$recovered_config_image" ] || die 'current Agent containers do not use one image reference'
    status=$(jq -r '.[0].State.Status // empty' <<<"$data"); health=$(jq -r '.[0].State.Health.Status // empty' <<<"$data")
    recovery_runtime_valid "$service" "$status" "$health" || die "current $service runtime state is invalid for interrupted update recovery"
    recovered_statuses[$service]=$status
    observed=$(jq -r '.[0].Image // empty' <<<"$data")
    [ -n "$observed" ] || die "current $service image identity is missing"
    [ -z "$recovered_image_id" ] && recovered_image_id=$observed
    [ "$observed" = "$recovered_image_id" ] || die 'current Agent containers do not use one image ID'
  done
  recovered_identity=$(docker image inspect "$recovered_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || die 'current Agent image identity inspection failed'
  IFS='|' read -r recovered_version recovered_revision <<<"$recovered_identity"
  canonical_version "$recovered_version" || die 'current Agent version is invalid'
  printf '%s\n' "$recovered_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'current Agent revision is invalid'
  recovered_image="docker.io/dirextalk/agent:$recovered_version"
  [ "$recovered_config_image" = "$recovered_image" ] || die 'current Agent image reference does not match its version label'
  [ "$recovered_version" != "$target_version" ] || agent_recovered_to_target=true
  for pair in agent:/usr/local/bin/dirextalk-agent extension-runner:/usr/local/bin/dirextalk-extension-runner core-runner:/usr/local/bin/dirextalk-core-runner; do
    service=${pair%%:*}; binary=${pair#*:}
    if [ "$service" = agent ] && [ "${recovered_statuses[$service]}" = restarting ]; then
      binary_version=$(docker run --rm --entrypoint "$binary" "$recovered_image_id" --version 2>/dev/null)
    else
      binary_version=$(docker exec "${recovered_ids[$service]}" "$binary" --version 2>/dev/null)
    fi
    [ "$binary_version" = "$recovered_version" ] || die "current $service binary version does not match its image"
  done

  repair_env='' repair_receipt=''
  cleanup_repair() { local status=$?; [ -z "$repair_env" ] || rm -f -- "$repair_env"; [ -z "$repair_receipt" ] || rm -f -- "$repair_receipt"; return "$status"; }
  trap cleanup_repair EXIT
  repair_env=$(mktemp "$out/.env.XXXXXX")
  awk -F= -v image="$recovered_image" -v version="$recovered_version" -v revision="$recovered_revision" '
    $1=="DIREXTALK_AGENT_IMAGE" {$0=$1 "=" image; is=1}
    $1=="DIREXTALK_AGENT_VERSION" {$0=$1 "=" version; vs=1}
    $1=="DIREXTALK_AGENT_SOURCE_REVISION" {$0=$1 "=" revision; rs=1}
    {print}
    END {if (!is || !vs || !rs) exit 1}
  ' "$env_file" >"$repair_env" || die 'could not repair expected Agent identity'
  chmod 400 "$repair_env"
  repair_env_identity=$(stat -c '%d:%i:%u' "$repair_env"); repair_env_sha=$(sha256sum "$repair_env" | awk '{print $1}')
  repair_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
  awk -F= -v identity="$repair_env_identity" -v digest="$repair_env_sha" -v mi="${indexes[message-server]}" -v mid="$message_id" -v ai="${indexes[agent]}" -v aid="${recovered_ids[agent]}" -v ei="${indexes[extension-runner]}" -v eid="${recovered_ids[extension-runner]}" -v ci="${indexes[core-runner]}" -v cid="${recovered_ids[core-runner]}" '
    $1=="control.env_identity" {$0=$1 "=" identity}
    $1=="control.env_sha256" {$0=$1 "=" digest}
    $1==("container." mi ".id") {$0=$1 "=" mid}
    $1==("container." ai ".id") {$0=$1 "=" aid}
    $1==("container." ei ".id") {$0=$1 "=" eid}
    $1==("container." ci ".id") {$0=$1 "=" cid}
    {print}
  ' "$receipt" >"$repair_receipt"
  chmod 400 "$repair_receipt"
  for service in agent extension-runner core-runner; do
    data=$(docker inspect "${recovered_ids[$service]}" 2>/dev/null) || die "current $service container changed before receipt repair"
    jq -e --arg id "${recovered_ids[$service]}" --arg project "$stack" --arg service "$service" --arg image "$recovered_image" --arg image_id "$recovered_image_id" '
      length == 1 and .[0].Id == $id and .[0].Image == $image_id and
      .[0].Config.Labels["com.docker.compose.project"] == $project and
      .[0].Config.Labels["com.docker.compose.service"] == $service and
      .[0].Config.Image == $image
    ' <<<"$data" >/dev/null || die "current $service container changed before receipt repair"
    status=$(jq -r '.[0].State.Status // empty' <<<"$data"); health=$(jq -r '.[0].State.Health.Status // empty' <<<"$data")
    recovery_runtime_valid "$service" "$status" "$health" || die "current $service runtime state changed before receipt repair"
  done
  mv -f "$repair_env" "$env_file"; repair_env=
  mv -f "$repair_receipt" "$receipt"; repair_receipt=
  trap - EXIT
  image=$recovered_image
  recorded_version=$recovered_version
  for service in agent extension-runner core-runner; do old_ids[$service]=${recovered_ids[$service]}; done
fi

message_data=$(docker inspect "$message_id" 2>/dev/null) || die 'message-server container is unavailable'
jq -e --arg id "$message_id" --arg project "$stack" --arg image "$message_image" '
  length == 1 and .[0].Id == $id and
  .[0].Config.Labels["com.docker.compose.project"] == $project and
  .[0].Config.Labels["com.docker.compose.service"] == "message-server" and
  .[0].Config.Image == $image
' <<<"$message_data" >/dev/null || die 'message-server identity does not match the receipt-bound compose service'
message_image_id=$(jq -r '.[0].Image // empty' <<<"$message_data"); [ -n "$message_image_id" ] || die 'message-server image identity is missing'
server_version=$(docker image inspect "$message_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null) || die 'message-server version inspection failed'
canonical_version "$server_version" || die 'message-server version is invalid'
semver_ge "$server_version" "$minimum_server_version" || negative "target requires message-server $minimum_server_version (running $server_version)"

old_image_id=
for service in agent extension-runner core-runner; do
  data=$(docker inspect "${old_ids[$service]}" 2>/dev/null) || die "recorded $service container is unavailable"
  [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$image" ] || die "$service does not use its recorded version tag"
  observed=$(jq -r '.[0].Image // empty' <<<"$data"); [ -z "$old_image_id" ] && old_image_id=$observed; [ "$observed" = "$old_image_id" ] || die 'Agent containers do not use one image ID'
done
current_version=$(docker image inspect "$old_image_id" --format '{{index .Config.Labels "org.opencontainers.image.version"}}' 2>/dev/null) || die 'running Agent version inspection failed'
[ "$current_version" = "$recorded_version" ] || die 'running Agent version differs from its receipt'
declare -A new_ids=()
if [ "$agent_recovered_to_target" = true ]; then
  target_image_id=$old_image_id
  target_revision=$recovered_revision
  for service in agent extension-runner core-runner; do new_ids[$service]=${old_ids[$service]}; done
  rollback_needed=false
else
  semver_ge "$current_version" "$target_version" && negative "Agent $target_version is not newer than running $current_version"

  if [ -n "$local_image_ref" ]; then
    identity=$(docker image inspect "$local_image_ref" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || die 'local Agent image is unavailable'
  else
    docker pull "$target_image" >/dev/null || die 'Agent target version pull failed'
    identity=$(docker image inspect "$target_image" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || die 'Agent target version inspection failed'
  fi
  IFS='|' read -r pulled_version target_image_id target_revision <<<"$identity"
  [ "$pulled_version" = "$target_version" ] || die "target image is $pulled_version, expected $target_version"
  printf '%s\n' "$target_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'target revision label is invalid'
  for binary in /usr/local/bin/dirextalk-agent /usr/local/bin/dirextalk-extension-runner /usr/local/bin/dirextalk-core-runner; do [ "$(docker run --rm --entrypoint "$binary" "$target_image_id" --version)" = "$target_version" ] || die "$binary version mismatch"; done

  rollback_needed=false
  rollback_agent() {
  local status=$? rollback_receipt rollback_message_id rollback_agent_id rollback_extension_id rollback_core_id id data attempts rollback_ready
  [ "$rollback_needed" = true ] || return "$status"
  trap - EXIT
  printf 'split-agent update: restoring previous local image after failed apply\n' >&2
  if DIREXTALK_AGENT_IMAGE="$image" \
      "${compose[@]}" up -d --no-deps --force-recreate --no-build --pull never extension-runner core-runner agent message-server >/dev/null 2>&1; then
    attempts=${DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS:-60}
    while [ "$attempts" -gt 0 ]; do
      rollback_message_id=$("${compose[@]}" ps -q message-server 2>/dev/null || true)
      rollback_agent_id=$("${compose[@]}" ps -q agent 2>/dev/null || true)
      rollback_extension_id=$("${compose[@]}" ps -q extension-runner 2>/dev/null || true)
      rollback_core_id=$("${compose[@]}" ps -q core-runner 2>/dev/null || true)
      rollback_ready=true
      data=$(docker inspect "$rollback_message_id" 2>/dev/null || true)
      [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$message_image_id" ] && [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ] || rollback_ready=false
      for id in "$rollback_agent_id" "$rollback_extension_id" "$rollback_core_id"; do
        data=$(docker inspect "$id" 2>/dev/null || true)
        [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$old_image_id" ] && [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ] || rollback_ready=false
      done
      if [ "$rollback_ready" = true ] && [ "$(docker exec "$rollback_message_id" /usr/bin/dirextalk-message-server --version 2>/dev/null || true)" = "$server_version" ]; then
        rollback_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
        awk -F= -v mi="${indexes[message-server]}" -v mid="$rollback_message_id" -v ai="${indexes[agent]}" -v aid="$rollback_agent_id" -v ei="${indexes[extension-runner]}" -v eid="$rollback_extension_id" -v ci="${indexes[core-runner]}" -v cid="$rollback_core_id" \
          '$1==("container." mi ".id") {$0=$1 "=" mid} $1==("container." ai ".id") {$0=$1 "=" aid} $1==("container." ei ".id") {$0=$1 "=" eid} $1==("container." ci ".id") {$0=$1 "=" cid} {print}' "$receipt" >"$rollback_receipt" &&
          chmod 400 "$rollback_receipt" && mv -f "$rollback_receipt" "$receipt"
        return "$status"
      fi
      attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1
    done
  fi
  printf 'split-agent update: previous local image restoration failed\n' >&2
  return 1
  }
  trap rollback_agent EXIT
  if [ -n "$local_image_ref" ]; then
    docker image tag "$target_image_id" "$target_image" >/dev/null || die 'could not bind local Agent image to the target version tag'
  fi
  rollback_needed=true
  DIREXTALK_AGENT_IMAGE="$target_image" \
    "${compose[@]}" run --rm --no-deps --pull never -T --interactive=false agent-migrate >/dev/null || die 'Agent storage migration failed'
  DIREXTALK_AGENT_IMAGE="$target_image" \
    "${compose[@]}" up -d --no-deps --force-recreate --no-build --pull never extension-runner core-runner agent >/dev/null || die 'Agent recreate failed'
  for service in agent extension-runner core-runner; do
    attempts=${DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS:-60}
    while [ "$attempts" -gt 0 ]; do new_ids[$service]=$("${compose[@]}" ps -q "$service" 2>/dev/null || true); data=$(docker inspect "${new_ids[$service]}" 2>/dev/null || true); if [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$target_image_id" ] && [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ]; then break; fi; attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1; done
    [ "$attempts" -gt 0 ] || die "$service did not become healthy"
  done
  for pair in agent:/usr/local/bin/dirextalk-agent extension-runner:/usr/local/bin/dirextalk-extension-runner core-runner:/usr/local/bin/dirextalk-core-runner; do service=${pair%%:*}; binary=${pair#*:}; [ "$(docker exec "${new_ids[$service]}" "$binary" --version)" = "$target_version" ] || die "$service running binary version mismatch"; done
fi

DIREXTALK_AGENT_IMAGE="$target_image" \
  "${compose[@]}" up -d --no-deps --force-recreate --no-build --pull never message-server >/dev/null || die 'message-server recreate after Agent update failed'
attempts=${DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS:-60}
while [ "$attempts" -gt 0 ]; do
  new_message_id=$("${compose[@]}" ps -q message-server 2>/dev/null || true)
  data=$(docker inspect "$new_message_id" 2>/dev/null || true)
  if jq -e --arg id "$new_message_id" --arg project "$stack" --arg image "$message_image" --arg image_id "$message_image_id" '
    length == 1 and .[0].Id == $id and .[0].Image == $image_id and
    .[0].Config.Labels["com.docker.compose.project"] == $project and
    .[0].Config.Labels["com.docker.compose.service"] == "message-server" and
    .[0].Config.Image == $image and .[0].State.Status == "running" and
    .[0].State.Health.Status == "healthy"
  ' <<<"$data" >/dev/null 2>&1; then break; fi
  attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1
done
[ "$attempts" -gt 0 ] || die 'message-server did not become healthy after Agent update'
[ "$(docker exec "$new_message_id" /usr/bin/dirextalk-message-server --version)" = "$server_version" ] || die 'message-server version changed during Agent catalog refresh'

new_env=$(mktemp "$out/.env.XXXXXX")
awk -F= -v image="$target_image" -v version="$target_version" -v revision="$target_revision" '$1=="DIREXTALK_AGENT_IMAGE" {$0=$1 "=" image; is=1} $1=="DIREXTALK_AGENT_VERSION" {$0=$1 "=" version; vs=1} $1=="DIREXTALK_AGENT_SOURCE_REVISION" {$0=$1 "=" revision; rs=1} {print} END {if (!is || !vs || !rs) exit 1}' "$env_file" >"$new_env" || die 'could not update expected Agent release'
chmod 400 "$new_env"; new_env_identity=$(stat -c '%d:%i:%u' "$new_env"); new_env_sha=$(sha256sum "$new_env" | awk '{print $1}')
new_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX")
awk -F= -v identity="$new_env_identity" -v digest="$new_env_sha" -v mi="${indexes[message-server]}" -v mid="$new_message_id" -v ai="${indexes[agent]}" -v aid="${new_ids[agent]}" -v ei="${indexes[extension-runner]}" -v eid="${new_ids[extension-runner]}" -v ci="${indexes[core-runner]}" -v cid="${new_ids[core-runner]}" '$1=="control.env_identity" {$0=$1 "=" identity} $1=="control.env_sha256" {$0=$1 "=" digest} $1==("container." mi ".id") {$0=$1 "=" mid} $1==("container." ai ".id") {$0=$1 "=" aid} $1==("container." ei ".id") {$0=$1 "=" eid} $1==("container." ci ".id") {$0=$1 "=" cid} {print}' "$receipt" >"$new_receipt"
chmod 400 "$new_receipt"; mv -f "$new_env" "$env_file"; mv -f "$new_receipt" "$receipt"
rollback_needed=false
trap - EXIT
if [ "$old_image_id" != "$target_image_id" ] && ! docker ps -aq --filter "ancestor=$old_image_id" | grep -q .; then docker image rm "$old_image_id" >/dev/null 2>&1 || true; fi
printf 'split-agent update passed: version=%s image=%s revision=%s\n' "$target_version" "$target_image" "$target_revision"
