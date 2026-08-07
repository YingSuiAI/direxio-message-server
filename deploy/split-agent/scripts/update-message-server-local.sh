#!/usr/bin/env bash
# Receipt-bound production message-server update adapter. The host updater
# supplies only the protected run directory and its authenticated canonical
# target version. Compose paths, project, service, and image repository remain
# repository-owned constants.
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
compose_file=$stack_dir/compose.yaml
production_compose_file=$stack_dir/compose.production.yaml

die() { printf 'split message-server update: %s\n' "$*" >&2; exit 1; }
negative() { printf 'split message-server update: %s\n' "$*" >&2; exit 3; }
usage() { printf 'usage: %s OUTPUT_DIR target_version\n' "${0##*/}" >&2; exit 1; }
canonical_version() {
  printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
}
semver_ge() {
  local left=${1#v} right=${2#v} l1 l2 l3 r1 r2 r3
  IFS=. read -r l1 l2 l3 <<<"$left"
  IFS=. read -r r1 r2 r3 <<<"$right"
  [ "$l1" -gt "$r1" ] || {
    [ "$l1" -eq "$r1" ] && {
      [ "$l2" -gt "$r2" ] || {
        [ "$l2" -eq "$r2" ] && [ "$l3" -ge "$r3" ]
      }
    }
  }
}
read_pair() {
  local file=$1 key=$2 count value
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {n++} END {print n+0}' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {print substr($0,length(wanted)+2); exit}' "$file")
  [ -n "$value" ] || die "$file contains an empty $key"
  printf '%s' "$value"
}
pair_count() {
  awk -F= -v wanted="$2" '$0 !~ /^[[:space:]]*#/ && index($0,wanted "=")==1 {n++} END {print n+0}' "$1"
}
file_identity() { stat -c '%d:%i:%u:%a' -- "$1"; }

[ "$#" -eq 2 ] || usage
out=$(readlink -m -- "$1")
target_version=$2
test_fixture=${DIREXTALK_MESSAGE_SERVER_UPDATE_TEST_FIXTURE:-false}
case "$test_fixture" in true|false) ;; *) usage ;; esac
required_owner=0
required_group=0
if [ "$test_fixture" = true ]; then required_owner=$(id -u); required_group=$(id -g); fi
canonical_version "$target_version" || usage
[ "$out" != / ] || die 'OUTPUT_DIR must not be the filesystem root'
[ -d "$out" ] && [ ! -L "$out" ] || die 'OUTPUT_DIR must be a non-symlink directory'
[ "$(stat -c '%a' -- "$out")" = 700 ] || die 'OUTPUT_DIR must be mode 0700'
[ "$(stat -c '%u' -- "$out")" = "$required_owner" ] || die 'OUTPUT_DIR must be owned by the release operator'
dir_identity=$(stat -c '%d:%i:%u:%g:%a' -- "$out")
verify_output_dir_identity() {
  [ -d "$out" ] && [ ! -L "$out" ] && [ "$(stat -c '%d:%i:%u:%g:%a' -- "$out")" = "$dir_identity" ] || die 'OUTPUT_DIR identity changed during message-server update'
}
durable_replace() {
  local source=$1 destination=$2
  verify_output_dir_identity
  sync -f "$source" || return 1
  verify_output_dir_identity
  mv -f -- "$source" "$destination" || return 1
  sync -f "$destination" || return 1
  sync -f "$out" || return 1
}
durable_remove() {
  local path=$1
  verify_output_dir_identity
  rm -f -- "$path" || return 1
  sync -f "$out" || return 1
}
[ -f "$compose_file" ] && [ ! -L "$compose_file" ] || die 'repository Compose file is unavailable'
[ -f "$production_compose_file" ] && [ ! -L "$production_compose_file" ] || die 'repository production Compose override is unavailable'
command -v docker >/dev/null 2>&1 || die 'docker is required'
command -v jq >/dev/null 2>&1 || die 'jq is required'
command -v flock >/dev/null 2>&1 || die 'flock is required'
command -v sync >/dev/null 2>&1 || die 'sync is required'

env_file=$out/.env
manifest=$out/.manifest
receipt=$out/.cleanup-receipt
for file in "$env_file" "$manifest" "$receipt"; do
  [ -f "$file" ] && [ ! -L "$file" ] || die "invalid protected control file: $file"
  [ "$(stat -c '%a' -- "$file")" = 400 ] || die "protected control file must be mode 0400: $file"
  [ "$(stat -c '%u' -- "$file")" = "$required_owner" ] || die "control file owner mismatch: $file"
done
lock_file=$out/.message-server-update.lock
if [ -e "$lock_file" ] || [ -L "$lock_file" ]; then
  [ -f "$lock_file" ] && [ ! -L "$lock_file" ] || die 'message-server update lock must be a regular non-symlink file'
else
  old_umask=$(umask)
  umask 077
  if ! (set -o noclobber; : >"$lock_file") 2>/dev/null; then
    umask "$old_umask"
    die 'could not atomically create the message-server update lock'
  fi
  umask "$old_umask"
  chmod 600 -- "$lock_file" || die 'could not protect the message-server update lock'
  chown "$required_owner:$required_group" -- "$lock_file" || die 'could not bind the message-server update lock owner'
fi
[ "$(stat -c '%u:%g:%a' -- "$lock_file")" = "$required_owner:$required_group:600" ] || die 'message-server update lock owner or mode is invalid'
lock_identity=$(file_identity "$lock_file")
exec 9<>"$lock_file"
[ "$(file_identity "$lock_file")" = "$lock_identity" ] || die 'message-server update lock changed before acquisition'
flock -n 9 || die 'another message-server update owns this run directory'
[ "$(file_identity "$lock_file")" = "$lock_identity" ] || die 'message-server update lock changed after acquisition'

transaction_journal=$out/.message-server-update.transaction
if [ -e "$transaction_journal" ] || [ -L "$transaction_journal" ]; then
  die 'an unfinished one-way message-server update requires explicit operator recovery'
fi
for unfinished_path in \
  "$out/.message-server-update.new.attestation" \
  "$out/.message-server-update.new.manifest" \
  "$out/.message-server-update.new.env" \
  "$out/.message-server-update.new.receipt"; do
  [ ! -e "$unfinished_path" ] && [ ! -L "$unfinished_path" ] || die 'unfinished message-server update evidence requires explicit operator recovery'
done

env_identity=$(file_identity "$env_file")
manifest_identity=$(file_identity "$manifest")
receipt_identity=$(file_identity "$receipt")
env_sha=$(sha256sum -- "$env_file" | awk '{print $1}')
manifest_sha=$(sha256sum -- "$manifest" | awk '{print $1}')
receipt_sha=$(sha256sum -- "$receipt" | awk '{print $1}')

grep -Fqx '# dirextalk-split-cleanup-receipt-v1' "$receipt" || die 'cleanup receipt version is unsupported'
[ "$(read_pair "$receipt" state)" = complete ] || die 'cleanup receipt is incomplete'
[ "$(read_pair "$receipt" control.env_identity)" = "${env_identity%:*}" ] || die '.env identity differs from cleanup receipt'
[ "$(read_pair "$receipt" control.manifest_identity)" = "${manifest_identity%:*}" ] || die 'manifest identity differs from cleanup receipt'
[ "$(read_pair "$receipt" control.env_sha256)" = "$env_sha" ] || die '.env digest differs from cleanup receipt'
[ "$(read_pair "$receipt" control.manifest_sha256)" = "$manifest_sha" ] || die 'manifest digest differs from cleanup receipt'
[ "$(read_pair "$env_file" DIREXTALK_RELEASE_CATALOG_ORIGIN)" = https://imadmin.dirextalk.ai ] || die 'release catalog origin differs from the canonical deployment origin'
[ "$(read_pair "$manifest" compose_mode)" = production ] || negative 'message-server release updates apply only to production stacks'
stack=$(read_pair "$manifest" stack_name)
printf '%s\n' "$stack" | grep -Eq '^d-[a-z2-7]{26}$' || die 'manifest stack identity is invalid'
[ "$stack" = "$(read_pair "$receipt" stack_name)" ] || die 'cleanup receipt stack identity mismatch'
[ "$stack" = "$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)" ] || die '.env stack identity mismatch'

manifest_machine_id=$(read_pair "$manifest" runner.machine_id)
manifest_engine_id=$(read_pair "$manifest" runner.docker_engine_id)
receipt_machine_id=$(read_pair "$receipt" host.machine_id)
receipt_engine_id=$(read_pair "$receipt" docker.engine_id)
receipt_endpoint=$(read_pair "$receipt" docker.context_endpoint)
receipt_socket=$(read_pair "$receipt" docker.context_socket)
[ "$manifest_machine_id" = "$receipt_machine_id" ] || die 'manifest/receipt machine identity mismatch'
[ "$manifest_engine_id" = "$receipt_engine_id" ] || die 'manifest/receipt Docker Engine identity mismatch'
printf '%s\n' "$receipt_machine_id" | grep -Eq '^[0-9a-f]{32}$' || die 'cleanup receipt machine identity is invalid'
printf '%s\n' "$receipt_engine_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || die 'cleanup receipt Docker Engine identity is invalid'
case "$receipt_endpoint" in unix:///*) ;; *) die 'cleanup receipt is not bound to a local Docker endpoint' ;; esac
[ "$receipt_socket" = /run/docker.sock ] || die 'cleanup receipt Docker socket is not canonical'

attestation=$(read_pair "$env_file" DIREXTALK_IMAGE_ATTESTATION_FILE)
[ "$attestation" = "$out/image-attestation" ] || die 'image attestation path is outside the receipt-bound run directory'
[ "$attestation" = "$(read_pair "$manifest" image_attestation_path)" ] || die 'image attestation path differs from manifest'
[ -f "$attestation" ] && [ ! -L "$attestation" ] || die 'image attestation must be a regular non-symlink file'
[ "$(stat -c '%a' -- "$attestation")" = 400 ] || die 'image attestation must be mode 0400'
[ "$(stat -c '%u' -- "$attestation")" = "$required_owner" ] || die 'image attestation owner mismatch'
[ "$(stat -c '%d' -- "$attestation")" = "$(read_pair "$manifest" image_attestation_device)" ] || die 'image attestation device differs from manifest'
[ "$(stat -c '%i' -- "$attestation")" = "$(read_pair "$manifest" image_attestation_inode)" ] || die 'image attestation inode differs from manifest'
[ "$(stat -c '%u' -- "$attestation")" = "$(read_pair "$manifest" image_attestation_uid)" ] || die 'image attestation UID differs from manifest'
attestation_identity=$(file_identity "$attestation")
attestation_sha=$(sha256sum -- "$attestation" | awk '{print $1}')
[ "$attestation_sha" = "$(read_pair "$manifest" image_attestation_sha256)" ] || die 'image attestation digest differs from manifest'
[ "$(sed -n '1p' "$attestation")" = '# dirextalk-image-attestation-v2' ] || die 'image attestation version is unsupported'

current_ref=$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE)
printf '%s\n' "$current_ref" | grep -Eq '^(docker\.io/)?dirextalk/message-server@sha256:[0-9a-f]{64}$' || \
  die 'current message-server image is not the fixed immutable repository'
message_alias_count=$(pair_count "$env_file" MESSAGE_SERVER_IMAGE)
[ "$message_alias_count" -le 1 ] || die '.env contains duplicate MESSAGE_SERVER_IMAGE entries'
if [ "$message_alias_count" -eq 1 ]; then
  [ "$(read_pair "$env_file" MESSAGE_SERVER_IMAGE)" = "$current_ref" ] || die 'MESSAGE_SERVER_IMAGE differs from the immutable message-server image'
fi
current_attested_ref=$(read_pair "$attestation" image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE)
[ "$current_attested_ref" = "$current_ref" ] || die 'message-server image differs from image attestation'
current_attested_revision=$(read_pair "$attestation" message_source_revision)
printf '%s\n' "$current_attested_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'attested message-server revision is invalid'

container_count=$(read_pair "$receipt" container.count)
printf '%s\n' "$container_count" | grep -Eq '^[1-9][0-9]{0,3}$' || die 'cleanup receipt container count is invalid'
message_index=
message_id=
declare -A seen_services=()
for ((index=0; index<container_count; index++)); do
  id=$(read_pair "$receipt" "container.$index.id")
  name=$(read_pair "$receipt" "container.$index.name")
  service=$(read_pair "$receipt" "container.$index.service")
  project=$(read_pair "$receipt" "container.$index.project")
  printf '%s\n' "$id" | grep -Eq '^[0-9a-f]{64}$' || die 'cleanup receipt container ID is invalid'
  printf '%s\n' "$name" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || die 'cleanup receipt container name is invalid'
  printf '%s\n' "$service" | grep -Eq '^[a-z0-9][a-z0-9_-]*$' || die 'cleanup receipt container service is invalid'
  [ "$project" = "$stack" ] || die 'cleanup receipt container is outside the bound stack'
  [ -z "${seen_services[$service]:-}" ] || die "cleanup receipt contains duplicate service $service"
  seen_services[$service]=true
  if [ "$service" = message-server ]; then message_index=$index; message_id=$id; fi
done
[ -n "$message_id" ] || die 'cleanup receipt lacks message-server'

verify_control_identity() {
  verify_output_dir_identity
  [ "$(file_identity "$env_file")" = "$env_identity" ] || die '.env identity changed during update preflight'
  [ "$(file_identity "$manifest")" = "$manifest_identity" ] || die 'manifest identity changed during update preflight'
  [ "$(file_identity "$receipt")" = "$receipt_identity" ] || die 'cleanup receipt identity changed during update preflight'
  [ "$(sha256sum -- "$env_file" | awk '{print $1}')" = "$env_sha" ] || die '.env contents changed during update preflight'
  [ "$(sha256sum -- "$manifest" | awk '{print $1}')" = "$manifest_sha" ] || die 'manifest contents changed during update preflight'
  [ "$(sha256sum -- "$receipt" | awk '{print $1}')" = "$receipt_sha" ] || die 'cleanup receipt contents changed during update preflight'
  [ "$(file_identity "$attestation")" = "$attestation_identity" ] || die 'image attestation identity changed during update preflight'
  [ "$(sha256sum -- "$attestation" | awk '{print $1}')" = "$attestation_sha" ] || die 'image attestation contents changed during update preflight'
  [ "$(file_identity "$lock_file")" = "$lock_identity" ] || die 'message-server update lock changed while held'
}
verify_host_docker_identity() {
  local current_machine current_endpoint current_engine endpoint_socket
  [ -z "${DOCKER_HOST:-}" ] || die 'DOCKER_HOST must be unset for the local rootful Docker daemon'
  case "${DOCKER_CONTEXT:-default}" in ''|default) ;; *) die 'DOCKER_CONTEXT must be unset or default' ;; esac
  [ -f /etc/machine-id ] && [ ! -L /etc/machine-id ] || die 'host machine-id is unavailable'
  [ "$(stat -c '%u:%g' -- /etc/machine-id)" = 0:0 ] || die 'host machine-id is not root-owned'
  current_machine=$(tr -d '[:space:]' </etc/machine-id)
  [ "$current_machine" = "$receipt_machine_id" ] || die 'host machine identity changed since startup'
  current_endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null) || die 'Docker context inspection failed'
  [ "$current_endpoint" = "$receipt_endpoint" ] || die 'Docker context endpoint changed since startup'
  endpoint_socket=${current_endpoint#unix://}
  [ -S "$endpoint_socket" ] || die 'bound local Docker socket is unavailable'
  [ "$(readlink -f -- "$endpoint_socket")" = "$receipt_socket" ] || die 'bound Docker socket is no longer canonical'
  current_engine=$(docker info --format '{{.ID}}' 2>/dev/null) || die 'Docker Engine identity query failed'
  [ "$current_engine" = "$receipt_engine_id" ] || die 'Docker Engine identity changed since startup'
}
inspect_bound_message() {
  local expected_id=$1 expected_image_id=$2 expected_ref=$3 data
  data=$(docker inspect "$expected_id" 2>/dev/null) || die 'receipt-bound message-server container is unavailable'
  [ "$(jq -r '.[0].Id // empty' <<<"$data")" = "$expected_id" ] || die 'message-server container identity changed'
  [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$expected_ref" ] || die 'message-server container image reference differs from .env'
  [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$expected_image_id" ] || die 'message-server image identity changed'
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")" = "$stack" ] || die 'message-server container project changed'
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")" = message-server ] || die 'message-server service identity changed'
  [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] || die 'message-server is not running'
  [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ] || die 'message-server is not healthy'
}
read_image_identity() {
  docker image inspect "$1" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{join .RepoDigests ","}}' 2>/dev/null
}
parse_image_identity() {
  local identity=$1 expected_repo=$2 normalized_expected_repo
  IFS='|' read -r parsed_version parsed_id parsed_revision parsed_digests <<<"$identity"
  canonical_version "$parsed_version" || die 'message-server image version label is not canonical'
  printf '%s\n' "$parsed_id" | grep -Eq '^sha256:[0-9a-f]{64}$' || die 'message-server image ID is invalid'
  printf '%s\n' "$parsed_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'message-server image revision label is invalid'
  normalized_expected_repo=${expected_repo#docker.io/}
  printf '%s\n' "$parsed_digests" | tr ',' '\n' | sed 's#^docker\.io/##' | grep -Fqx "$normalized_expected_repo" || \
    die 'message-server image RepoDigest is missing'
}

verify_control_identity
verify_host_docker_identity
old_identity=$(read_image_identity "$current_ref") || die 'current message-server image inspection failed'
parse_image_identity "$old_identity" "$current_ref"
IFS='|' read -r current_version current_image_id _ <<<"$old_identity"
IFS='|' read -r _ _ current_revision _ <<<"$old_identity"
[ "$current_revision" = "$current_attested_revision" ] || die 'running message-server revision differs from image attestation'
inspect_bound_message "$message_id" "$current_image_id" "$current_ref"
semver_ge "$current_version" "$target_version" && negative "message-server $target_version is not newer than running $current_version"

compose=(docker compose --env-file "$env_file" -f "$compose_file" -f "$production_compose_file" --project-name "$stack")
verify_control_identity
verify_host_docker_identity
"${compose[@]}" config --quiet >/dev/null || die 'production Compose validation failed'
target_tag=docker.io/dirextalk/message-server:$target_version
verify_control_identity
verify_host_docker_identity
docker pull "$target_tag" >/dev/null || die 'target message-server image pull failed'
target_identity=$(read_image_identity "$target_tag") || die 'target message-server image inspection failed'
IFS='|' read -r target_label target_image_id target_revision target_digests <<<"$target_identity"
[ "$target_label" = "$target_version" ] || die 'target message-server image version label mismatch'
printf '%s\n' "$target_image_id" | grep -Eq '^sha256:[0-9a-f]{64}$' || die 'target message-server image ID is invalid'
printf '%s\n' "$target_revision" | grep -Eq '^[0-9a-f]{40}$' || die 'target message-server image revision label is invalid'
target_ref=$(printf '%s\n' "$target_digests" | tr ',' '\n' | awk '$0 ~ /^(docker\.io\/)?dirextalk\/message-server@sha256:[0-9a-f]{64}$/ {print; exit}')
[ -n "$target_ref" ] || die 'target message-server image has no fixed immutable Docker Hub digest'

transaction_journal=$out/.message-server-update.transaction
new_attestation=$out/.message-server-update.new.attestation
new_manifest=$out/.message-server-update.new.manifest
new_env=$out/.message-server-update.new.env
new_receipt=$out/.message-server-update.new.receipt
final_receipt=$out/.message-server-update.final.receipt
for transaction_path in "$transaction_journal" "$new_attestation" "$new_manifest" "$new_env" "$new_receipt" "$final_receipt"; do
  [ ! -e "$transaction_path" ] && [ ! -L "$transaction_path" ] || die 'an unfinished message-server update transaction already exists'
done
write_transaction_journal() {
  local state=$1 tmp control_name control_path journal_index journal_service journal_id
  local -a journal_names=(attestation manifest env receipt)
  local -a old_paths=("$attestation" "$manifest" "$env_file" "$receipt")
  [ "$state" = prepared ] || return 1
  verify_output_dir_identity
  tmp=$(mktemp "$out/.message-server-update.transaction.XXXXXX") || return 1
  {
    printf '%s\n' '# dirextalk-message-server-update-transaction-v1'
    printf 'state=%s\ntarget_version=%s\nstack_name=%s\nmessage_index=%s\n' "$state" "$target_version" "$stack" "$message_index"
    printf 'old_message_id=%s\nold_image_ref=%s\nold_image_id=%s\nold_image_revision=%s\n' "$message_id" "$current_ref" "$current_image_id" "$current_revision"
    printf 'target_image_ref=%s\ntarget_image_id=%s\ntarget_image_revision=%s\n' "$target_ref" "$target_image_id" "$target_revision"
    printf 'host_machine_id=%s\ndocker_engine_id=%s\ndocker_context_endpoint=%s\ndocker_context_socket=%s\n' \
      "$receipt_machine_id" "$receipt_engine_id" "$receipt_endpoint" "$receipt_socket"
    for ((journal_index=0; journal_index<4; journal_index++)); do
      control_name=${journal_names[$journal_index]}; control_path=${old_paths[$journal_index]}
      printf 'old.%s.identity=%s\nold.%s.sha256=%s\n' "$control_name" "$(file_identity "$control_path")" "$control_name" "$(sha256sum -- "$control_path" | awk '{print $1}')"
    done
    printf 'old.container.count=%s\n' "$container_count"
    for ((journal_index=0; journal_index<container_count; journal_index++)); do
      journal_service=$(read_pair "$receipt" "container.$journal_index.service")
      journal_id=$(read_pair "$receipt" "container.$journal_index.id")
      printf 'old.container.%s.service=%s\nold.container.%s.id=%s\n' "$journal_index" "$journal_service" "$journal_index" "$journal_id"
    done
  } >"$tmp" || { rm -f -- "$tmp"; return 1; }
  chmod 400 -- "$tmp" || { rm -f -- "$tmp"; return 1; }
  chown "$required_owner:$required_group" -- "$tmp" || { rm -f -- "$tmp"; return 1; }
  verify_output_dir_identity
  durable_replace "$tmp" "$transaction_journal" || { rm -f -- "$tmp"; return 1; }
}
write_transaction_journal prepared || die 'could not durably prepare the update transaction journal'

mark_message_journal_cleanup_pending() {
  local temp
  verify_output_dir_identity
  temp=$(mktemp "$out/.message-server-update.transaction.XXXXXX") || return 1
  awk -F= '$1=="state" {$0="state=cleanup-pending"} {print}' "$transaction_journal" >"$temp" || { rm -f "$temp"; return 1; }
  if ! chmod 400 "$temp" || ! chown "$required_owner:$required_group" "$temp"; then rm -f "$temp"; return 1; fi
  verify_output_dir_identity
  durable_replace "$temp" "$transaction_journal" || { rm -f -- "$temp"; return 1; }
}

message_journal_bound_id() {
  local wanted=$1 count index journal_service
  count=$(read_pair "$transaction_journal" old.container.count) || return 1
  for ((index=0; index<count; index++)); do
    journal_service=$(read_pair "$transaction_journal" "old.container.$index.service") || return 1
    if [ "$journal_service" = "$wanted" ]; then read_pair "$transaction_journal" "old.container.$index.id"; return; fi
  done
  return 1
}
mutated=false
commit_complete=false

wait_message() {
  local env_path=$1 expected_image_id=$2 expected_ref=$3 attempts id data
  attempts=${DIREXTALK_MESSAGE_SERVER_UPDATE_HEALTH_ATTEMPTS:-60}
  printf '%s\n' "$attempts" | grep -Eq '^[1-9][0-9]{0,3}$' || return 1
  while [ "$attempts" -gt 0 ]; do
    id=$(DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE="$expected_ref" docker compose --env-file "$env_path" \
      -f "$compose_file" -f "$production_compose_file" --project-name "$stack" ps -q message-server 2>/dev/null) || return 1
    if printf '%s\n' "$id" | grep -Eq '^[0-9a-f]{64}$'; then
      data=$(docker inspect "$id" 2>/dev/null) || return 1
      if [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$expected_image_id" ] && \
         [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$expected_ref" ] && \
         [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")" = "$stack" ] && \
         [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")" = message-server ] && \
         [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] && \
         [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ]; then
        printf '%s' "$id"
        return 0
      fi
      [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" != unhealthy ] || return 1
    fi
    attempts=$((attempts-1))
    [ "$attempts" -gt 0 ] && sleep 1
  done
  return 1
}
cleanup_previous_image() {
  local active_id active_data all_ids id data project service observed_ref status bound_id refs ref resolved_id retain_image=false removable=false
  local -a fixed_refs=()
  verify_control_identity
  verify_host_docker_identity
  [ -f "$transaction_journal" ] && [ ! -L "$transaction_journal" ] && [ "$(stat -c '%u:%g:%a' "$transaction_journal")" = "$required_owner:$required_group:400" ] || return 1
  [ "$(read_pair "$transaction_journal" state)" = cleanup-pending ] || return 1
  [ "$(read_pair "$transaction_journal" old_image_ref)" = "$current_ref" ] || return 1
  [ "$(read_pair "$transaction_journal" old_image_id)" = "$current_image_id" ] || return 1
  active_id=$(DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE="$target_ref" docker compose --env-file "$env_file" \
    -f "$compose_file" -f "$production_compose_file" --project-name "$stack" ps -q message-server 2>/dev/null) || return 1
  [ "$active_id" = "$new_message_id" ] || return 1
  active_data=$(docker inspect "$active_id" 2>/dev/null) || return 1
  [ "$(jq -r '.[0].Image // empty' <<<"$active_data")" = "$target_image_id" ] || return 1
  [ "$(jq -r '.[0].Config.Image // empty' <<<"$active_data")" = "$target_ref" ] || return 1
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$active_data")" = "$stack" ] || return 1
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$active_data")" = message-server ] || return 1
  [ "$(jq -r '.[0].State.Status // empty' <<<"$active_data")" = running ] || return 1
  [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$active_data")" = healthy ] || return 1
  all_ids=$(docker ps -aq --no-trunc) || return 1
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    data=$(docker inspect "$id" 2>/dev/null) || return 1
    [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$current_image_id" ] || continue
    observed_ref=$(jq -r '.[0].Config.Image // empty' <<<"$data")
    project=$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$data")
    service=$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$data")
    bound_id=''
    [ -z "$service" ] || bound_id=$(message_journal_bound_id "$service" 2>/dev/null || true)
    status=$(jq -r '.[0].State.Status // empty' <<<"$data")
    removable=false
    if [ "$project" = "$stack" ] && [ "$observed_ref" = "$current_ref" ] && [ "$status" != running ] && [ "$bound_id" = "$id" ]; then
      if [ "$service" = message-server ] && [ "$id" = "$message_id" ]; then removable=true; fi
      if [ "$service" = message-server-init ]; then removable=true; fi
    fi
    if [ "$removable" = true ]; then
      verify_control_identity
      verify_host_docker_identity
      docker container rm "$id" >/dev/null || return 1
    else
      retain_image=true
    fi
  done <<<"$all_ids"
  verify_control_identity
  verify_host_docker_identity
  [ "$(docker image inspect "$current_ref" --format '{{.Id}}' 2>/dev/null)" = "$current_image_id" ] || return 1
  refs=$(docker image inspect "$current_image_id" --format '{{range .RepoTags}}{{println .}}{{end}}{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null) || return 1
  while IFS= read -r ref; do
    [ -n "$ref" ] || continue
    if printf '%s\n' "$ref" | grep -Eq '^(docker\.io/)?dirextalk/message-server([:@])'; then
      fixed_refs+=("$ref")
    else
      retain_image=true
    fi
  done <<<"$refs"
  for ref in "${fixed_refs[@]}"; do
    verify_output_dir_identity
    verify_host_docker_identity
    resolved_id=$(docker image inspect "$ref" --format '{{.Id}}' 2>/dev/null) || return 1
    [ "$resolved_id" = "$current_image_id" ] || return 1
    docker image rm "$ref" >/dev/null || return 1
  done
  if [ "$retain_image" = true ]; then
    printf 'split message-server update: previous image ID retained because another container or foreign repository alias still uses it\n' >&2
    return 0
  fi
  verify_output_dir_identity
  verify_host_docker_identity
  if docker image inspect "$current_image_id" >/dev/null 2>&1; then
    docker image rm "$current_image_id" >/dev/null || return 1
  fi
}
write_env() {
  local source=$1 destination=$2 replacement=$3
  awk -F= -v replacement="$replacement" '
    $1=="MESSAGE_SERVER_IMAGE" {$0="MESSAGE_SERVER_IMAGE=" replacement; alias_seen=1}
    $1=="DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE" {$0="DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=" replacement; immutable_seen=1}
    {print}
    END {if (!alias_seen) print "MESSAGE_SERVER_IMAGE=" replacement; if (!immutable_seen) exit 1}
  ' "$source" >"$destination"
  chmod 400 -- "$destination"
}
write_attestation() {
  local source=$1 destination=$2 replacement_ref=$3 replacement_revision=$4
  awk -F= -v ref="$replacement_ref" -v revision="$replacement_revision" '
    $1=="message_source_revision" {$0="message_source_revision=" revision; revision_seen=1}
    $1=="image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE" {$0="image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=" ref; image_seen=1}
    {print}
    END {if (!revision_seen || !image_seen) exit 1}
  ' "$source" >"$destination"
  chmod 400 -- "$destination"
}
write_manifest() {
  local source=$1 destination=$2 bound_attestation=$3 device inode uid digest
  device=$(stat -c '%d' -- "$bound_attestation")
  inode=$(stat -c '%i' -- "$bound_attestation")
  uid=$(stat -c '%u' -- "$bound_attestation")
  digest=$(sha256sum -- "$bound_attestation" | awk '{print $1}')
  awk -F= -v device="$device" -v inode="$inode" -v uid="$uid" -v digest="$digest" '
    $1=="image_attestation_device" {$0="image_attestation_device=" device; device_seen=1}
    $1=="image_attestation_inode" {$0="image_attestation_inode=" inode; inode_seen=1}
    $1=="image_attestation_uid" {$0="image_attestation_uid=" uid; uid_seen=1}
    $1=="image_attestation_sha256" {$0="image_attestation_sha256=" digest; digest_seen=1}
    {print}
    END {if (!device_seen || !inode_seen || !uid_seen || !digest_seen) exit 1}
  ' "$source" >"$destination"
  chmod 400 -- "$destination"
}
write_receipt() {
  local source=$1 destination=$2 bound_env=$3 bound_manifest=$4 new_id=$5 state=$6
  local new_env_identity new_env_sha new_manifest_identity new_manifest_sha
  case "$state" in cleanup-pending|complete) ;; *) return 1 ;; esac
  new_env_identity=$(stat -c '%d:%i:%u' -- "$bound_env")
  new_env_sha=$(sha256sum -- "$bound_env" | awk '{print $1}')
  new_manifest_identity=$(stat -c '%d:%i:%u' -- "$bound_manifest")
  new_manifest_sha=$(sha256sum -- "$bound_manifest" | awk '{print $1}')
  awk -F= -v state="$state" -v env_identity="$new_env_identity" -v env_digest="$new_env_sha" \
    -v manifest_identity="$new_manifest_identity" -v manifest_digest="$new_manifest_sha" \
    -v target_index="$message_index" -v id="$new_id" '
    $1=="state" {$0="state=" state}
    $1=="control.env_identity" {$0="control.env_identity=" env_identity}
    $1=="control.env_sha256" {$0="control.env_sha256=" env_digest}
    $1=="control.manifest_identity" {$0="control.manifest_identity=" manifest_identity}
    $1=="control.manifest_sha256" {$0="control.manifest_sha256=" manifest_digest}
    $1==("container." target_index ".id") {$0=$1 "=" id}
    {print}
  ' "$source" >"$destination"
  chmod 400 -- "$destination"
}
transaction_exit() {
  local status=$?
  trap - EXIT
  set +e
  if [ "$mutated" = false ]; then
    [ -z "$new_env" ] || rm -f -- "$new_env"
    [ -z "$new_manifest" ] || rm -f -- "$new_manifest"
    [ -z "$new_attestation" ] || rm -f -- "$new_attestation"
    [ -z "$new_receipt" ] || rm -f -- "$new_receipt"
    [ -z "$final_receipt" ] || rm -f -- "$final_receipt"
    durable_remove "$transaction_journal" || status=1
  elif [ "$commit_complete" = false ]; then
    printf 'split message-server update: one-way update failed; protected transaction evidence retained\n' >&2
    status=1
  fi
  exit "$status"
}
trap transaction_exit EXIT

verify_control_identity
verify_host_docker_identity
inspect_bound_message "$message_id" "$current_image_id" "$current_ref"
check_target_identity=$(read_image_identity "$target_ref") || die 'target immutable image disappeared before recreate'
IFS='|' read -r check_target_version check_target_id check_target_revision _ <<<"$check_target_identity"
[ "$check_target_version" = "$target_version" ] && [ "$check_target_id" = "$target_image_id" ] && \
  [ "$check_target_revision" = "$target_revision" ] || die 'target immutable image identity changed before recreate'
mutated=true
if ! DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE="$target_ref" docker compose --env-file "$env_file" \
  -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
  up -d --no-deps --force-recreate --no-build --pull never message-server >/dev/null; then
  die 'target message-server recreate failed'
fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_RECREATE:-false}" = true ]; then
  kill -KILL "$$"
fi
new_message_id=$(wait_message "$env_file" "$target_image_id" "$target_ref") || die 'target message-server health verification failed'
(verify_host_docker_identity) || die 'host/Docker identity changed after target health verification'
inspect_bound_message "$new_message_id" "$target_image_id" "$target_ref"

write_attestation "$attestation" "$new_attestation" "$target_ref" "$target_revision" || die 'could not render updated image attestation'
write_manifest "$manifest" "$new_manifest" "$new_attestation" || die 'could not render updated manifest'
write_env "$env_file" "$new_env" "$target_ref" || die 'could not render updated environment'
write_receipt "$receipt" "$new_receipt" "$new_env" "$new_manifest" "$new_message_id" cleanup-pending || die 'could not render cleanup-pending receipt'
write_receipt "$receipt" "$final_receipt" "$new_env" "$new_manifest" "$new_message_id" complete || die 'could not render final cleanup receipt'
verify_output_dir_identity
durable_replace "$new_attestation" "$attestation" || die 'could not durably commit updated image attestation'
new_attestation=
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = attestation ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = attestation ]; then
  die 'injected image attestation commit failure'
fi
verify_output_dir_identity
durable_replace "$new_manifest" "$manifest" || die 'could not durably commit updated manifest'
new_manifest=
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = manifest ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = manifest ]; then
  die 'injected manifest commit failure'
fi
verify_output_dir_identity
durable_replace "$new_env" "$env_file" || die 'could not durably commit updated environment'
new_env=
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = env ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = env ]; then
  die 'injected environment commit failure'
fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = receipt ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = receipt ]; then
  die 'injected cleanup receipt commit failure'
fi
verify_output_dir_identity
durable_replace "$new_receipt" "$receipt" || die 'could not durably commit cleanup-pending receipt'
new_receipt=
env_identity=$(file_identity "$env_file"); env_sha=$(sha256sum "$env_file" | awk '{print $1}')
manifest_identity=$(file_identity "$manifest"); manifest_sha=$(sha256sum "$manifest" | awk '{print $1}')
receipt_identity=$(file_identity "$receipt"); receipt_sha=$(sha256sum "$receipt" | awk '{print $1}')
attestation_identity=$(file_identity "$attestation"); attestation_sha=$(sha256sum "$attestation" | awk '{print $1}')
verify_control_identity
verify_host_docker_identity
mark_message_journal_cleanup_pending || die 'could not commit message-server cleanup-pending journal state'
cleanup_previous_image || die 'previous message-server image cleanup failed'
verify_output_dir_identity
durable_replace "$final_receipt" "$receipt" || die 'could not durably commit complete cleanup receipt'
final_receipt=
commit_complete=true
mutated=false
trap - EXIT
verify_output_dir_identity
durable_remove "$transaction_journal" || die 'could not durably remove completed message-server update journal'
printf 'split message-server update passed: version=%s image=%s\n' "$target_version" "$target_ref"
