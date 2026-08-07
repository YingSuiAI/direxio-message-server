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
usage() { printf 'usage: %s OUTPUT_DIR target_version\n' "${0##*/}" >&2; exit 2; }
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
[ -f "$compose_file" ] && [ ! -L "$compose_file" ] || die 'repository Compose file is unavailable'
[ -f "$production_compose_file" ] && [ ! -L "$production_compose_file" ] || die 'repository production Compose override is unavailable'
command -v docker >/dev/null 2>&1 || die 'docker is required'
command -v jq >/dev/null 2>&1 || die 'jq is required'
command -v flock >/dev/null 2>&1 || die 'flock is required'

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
resume_unfinished_transaction() {
  local journal=$transaction_journal state stack_name old_message_id old_ref old_image_id old_revision
  local target_ref target_image_id target_revision target_image_identity journal_machine journal_engine journal_endpoint journal_socket
  local backup pending current_path old_identity old_digest target_identity target_digest current_identity current_digest target_key_count
  local candidate_ids candidate_id candidate_data candidate_ref candidate_image candidate_status candidate_health rollback_container_id
  local compose_env rollback_id attempts data tmp_attestation tmp_manifest tmp_env tmp_receipt control_name
  local -a control_names=(attestation manifest env receipt)
  local -a backup_paths=(
    "$out/.message-server-update.old.attestation"
    "$out/.message-server-update.old.manifest"
    "$out/.message-server-update.old.env"
    "$out/.message-server-update.old.receipt"
  )
  local -a live_paths=("$out/image-attestation" "$out/.manifest" "$out/.env" "$out/.cleanup-receipt")
  local -a pending_paths=(
    "$out/.message-server-update.new.attestation"
    "$out/.message-server-update.new.manifest"
    "$out/.message-server-update.new.env"
    "$out/.message-server-update.new.receipt"
  )

  [ -f "$journal" ] && [ ! -L "$journal" ] || die 'unfinished update journal is not a regular protected file'
  [ "$(stat -c '%u:%g:%a' -- "$journal")" = "$required_owner:$required_group:400" ] || die 'unfinished update journal owner or mode is invalid'
  [ "$(sed -n '1p' "$journal")" = '# dirextalk-message-server-update-transaction-v1' ] || die 'unfinished update journal version is unsupported'
  state=$(read_pair "$journal" state)
  case "$state" in prepared|target-ready|rolling-back|rollback-ready) ;; *) die 'unfinished update journal state is invalid' ;; esac
  [ "$(read_pair "$journal" target_version)" = "$target_version" ] || die 'unfinished update target differs from requested target'
  stack_name=$(read_pair "$journal" stack_name)
  printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die 'unfinished update stack identity is invalid'
  old_message_id=$(read_pair "$journal" old_message_id)
  old_ref=$(read_pair "$journal" old_image_ref)
  old_image_id=$(read_pair "$journal" old_image_id)
  old_revision=$(read_pair "$journal" old_image_revision)
  target_ref=$(read_pair "$journal" target_image_ref)
  target_image_id=$(read_pair "$journal" target_image_id)
  target_revision=$(read_pair "$journal" target_image_revision)
  printf '%s\n' "$old_message_id" | grep -Eq '^[0-9a-f]{64}$' || die 'unfinished update old container ID is invalid'
  for image_id in "$old_image_id" "$target_image_id"; do printf '%s\n' "$image_id" | grep -Eq '^sha256:[0-9a-f]{64}$' || die 'unfinished update image ID is invalid'; done
  for revision in "$old_revision" "$target_revision"; do printf '%s\n' "$revision" | grep -Eq '^[0-9a-f]{40}$' || die 'unfinished update image revision is invalid'; done
  printf '%s\n' "$old_ref" | grep -Eq '^(docker\.io/)?dirextalk/message-server@sha256:[0-9a-f]{64}$' || die 'unfinished update old image reference is invalid'
  printf '%s\n' "$target_ref" | grep -Eq '^(docker\.io/)?dirextalk/message-server@sha256:[0-9a-f]{64}$' || die 'unfinished update target image reference is invalid'

  journal_machine=$(read_pair "$journal" host_machine_id)
  journal_engine=$(read_pair "$journal" docker_engine_id)
  journal_endpoint=$(read_pair "$journal" docker_context_endpoint)
  journal_socket=$(read_pair "$journal" docker_context_socket)
  [ "$(tr -d '[:space:]' </etc/machine-id)" = "$journal_machine" ] || die 'unfinished update host identity changed'
  [ "$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null)" = "$journal_endpoint" ] || die 'unfinished update Docker endpoint changed'
  [ "$(readlink -f -- "${journal_endpoint#unix://}")" = "$journal_socket" ] || die 'unfinished update Docker socket changed'
  [ "$(docker info --format '{{.ID}}' 2>/dev/null)" = "$journal_engine" ] || die 'unfinished update Docker Engine changed'

  for ((resume_index=0; resume_index<4; resume_index++)); do
    control_name=${control_names[$resume_index]}
    backup=${backup_paths[$resume_index]}
    pending=${pending_paths[$resume_index]}
    current_path=${live_paths[$resume_index]}
    [ -f "$backup" ] && [ ! -L "$backup" ] && [ "$(stat -c '%u:%a' -- "$backup")" = "$required_owner:400" ] || die "unfinished update old $control_name recovery file is invalid"
    old_identity=$(read_pair "$journal" "old.$control_name.identity")
    old_digest=$(read_pair "$journal" "old.$control_name.sha256")
    printf '%s\n' "$old_identity" | grep -Eq '^[0-9]+:[0-9]+:[0-9]+:400$' || die "unfinished update old $control_name identity is invalid"
    printf '%s\n' "$old_digest" | grep -Eq '^[0-9a-f]{64}$' || die "unfinished update old $control_name digest is invalid"
    [ "$(file_identity "$backup")" = "$old_identity" ] || die "unfinished update old $control_name identity changed"
    [ "$(sha256sum -- "$backup" | awk '{print $1}')" = "$old_digest" ] || die "unfinished update old $control_name digest changed"
    [ -f "$current_path" ] && [ ! -L "$current_path" ] || die "unfinished update live $control_name file is invalid"
    current_identity=$(file_identity "$current_path")
    current_digest=$(sha256sum -- "$current_path" | awk '{print $1}')
    target_key_count=$(pair_count "$journal" "target.$control_name.identity")
    [ "$target_key_count" -le 1 ] || die "unfinished update target $control_name identity is duplicated"
    [ "$(pair_count "$journal" "target.$control_name.sha256")" -eq "$target_key_count" ] || die "unfinished update target $control_name identity/hash pair is incomplete"
    if [ "$state" = target-ready ] || { [ "$state" = rollback-ready ] && [ "$control_name" = receipt ]; }; then
      [ "$target_key_count" -eq 1 ] || die "unfinished update target $control_name identity is missing"
    fi
    if [ "$target_key_count" -eq 1 ]; then
      target_identity=$(read_pair "$journal" "target.$control_name.identity")
      target_digest=$(read_pair "$journal" "target.$control_name.sha256")
      printf '%s\n' "$target_identity" | grep -Eq '^[0-9]+:[0-9]+:[0-9]+:400$' || die "unfinished update target $control_name identity is invalid"
      printf '%s\n' "$target_digest" | grep -Eq '^[0-9a-f]{64}$' || die "unfinished update target $control_name digest is invalid"
      if [ -e "$pending" ] || [ -L "$pending" ]; then
        [ -f "$pending" ] && [ ! -L "$pending" ] || die "unfinished update pending $control_name file is invalid"
        [ "$(file_identity "$pending")" = "$target_identity" ] && [ "$(sha256sum -- "$pending" | awk '{print $1}')" = "$target_digest" ] || \
          die "unfinished update pending $control_name identity changed"
      fi
    fi
    if [ "$current_identity" = "$old_identity" ] && [ "$current_digest" = "$old_digest" ]; then
      continue
    fi
    if [ "$state" = rollback-ready ] && [ "$control_name" != receipt ]; then
      die "unfinished rollback-ready transaction has an unexpected live $control_name identity"
    fi
    [ "$state" = target-ready ] || [ "$state" = rolling-back ] || [ "$control_name" = receipt ] || die "unfinished prepared transaction has an unexpected live $control_name identity"
    [ "$current_identity" = "$target_identity" ] && [ "$current_digest" = "$target_digest" ] || \
      die "unfinished target-ready transaction has an unexpected live $control_name identity"
  done

  candidate_id=
  if candidate_data=$(docker inspect "$old_message_id" 2>/dev/null); then
    [ "$(jq -r '.[0].Id // empty' <<<"$candidate_data")" = "$old_message_id" ] || die 'unfinished update old container identity changed'
    candidate_id=$old_message_id
  else
    candidate_ids=$(docker ps --no-trunc -aq \
      --filter "label=com.docker.compose.project=$stack_name" \
      --filter 'label=com.docker.compose.service=message-server') || die 'unfinished update target container lookup failed'
    [ "$(printf '%s\n' "$candidate_ids" | awk 'NF {n++} END {print n+0}')" -eq 1 ] || die 'unfinished update does not have exactly one journal-bound message-server container'
    candidate_id=$(printf '%s\n' "$candidate_ids" | awk 'NF {print; exit}')
    printf '%s\n' "$candidate_id" | grep -Eq '^[0-9a-f]{64}$' || die 'unfinished update candidate container ID is invalid'
    candidate_data=$(docker inspect "$candidate_id" 2>/dev/null) || die 'unfinished update candidate container inspection failed'
  fi
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$candidate_data")" = "$stack_name" ] || die 'unfinished update candidate project differs'
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$candidate_data")" = message-server ] || die 'unfinished update candidate service differs'
  candidate_ref=$(jq -r '.[0].Config.Image // empty' <<<"$candidate_data")
  candidate_image=$(jq -r '.[0].Image // empty' <<<"$candidate_data")
  candidate_status=$(jq -r '.[0].State.Status // empty' <<<"$candidate_data")
  candidate_health=$(jq -r '.[0].State.Health.Status // empty' <<<"$candidate_data")
  rollback_container_id=
  [ "$state" != rollback-ready ] || rollback_container_id=$(read_pair "$journal" rollback_container_id)
  if [ "$candidate_id" = "$old_message_id" ]; then
    [ "$candidate_ref" = "$old_ref" ] && [ "$candidate_image" = "$old_image_id" ] || die 'unfinished update old container image differs'
  elif [ "$state" = rollback-ready ] && [ "$candidate_id" = "$rollback_container_id" ]; then
    [ "$candidate_ref" = "$old_ref" ] && [ "$candidate_image" = "$old_image_id" ] || die 'unfinished update rollback container image differs'
  elif [ "$state" = rolling-back ] && [ "$candidate_ref" = "$old_ref" ]; then
    [ "$candidate_ref" = "$old_ref" ] && [ "$candidate_image" = "$old_image_id" ] || die 'unfinished update rollback container image differs'
  else
    [ "$candidate_ref" = "$target_ref" ] && [ "$candidate_image" = "$target_image_id" ] || die 'unfinished update candidate is not the exact target image'
    target_image_identity=$(docker image inspect "$target_ref" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || die 'unfinished update target image disappeared'
    [ "$target_image_identity" = "$target_version|$target_image_id|$target_revision" ] || die 'unfinished update target image version or immutable identity changed'
  fi

  resume_restore() {
    local source=$1 target=$2 temp
    if [ -e "$target" ] && [ "$target" -ef "$source" ]; then return 0; fi
    temp=$(mktemp "$out/.message-server-update.restore.XXXXXX") || return 1
    rm -f -- "$temp"
    ln -- "$source" "$temp" || return 1
    mv -f -- "$temp" "$target"
  }
  resume_write_attestation() {
    awk -F= -v ref="$target_ref" -v revision="$target_revision" '
      $1=="message_source_revision" {$0="message_source_revision=" revision}
      $1=="image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE" {$0="image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=" ref}
      {print}
    ' "${backup_paths[0]}" >"$1" && chmod 400 -- "$1"
  }
  resume_write_manifest() {
    local attestation_file=$1 destination=$2 device inode uid digest
    device=$(stat -c '%d' -- "$attestation_file"); inode=$(stat -c '%i' -- "$attestation_file")
    uid=$(stat -c '%u' -- "$attestation_file"); digest=$(sha256sum -- "$attestation_file" | awk '{print $1}')
    awk -F= -v device="$device" -v inode="$inode" -v uid="$uid" -v digest="$digest" '
      $1=="image_attestation_device" {$0="image_attestation_device=" device}
      $1=="image_attestation_inode" {$0="image_attestation_inode=" inode}
      $1=="image_attestation_uid" {$0="image_attestation_uid=" uid}
      $1=="image_attestation_sha256" {$0="image_attestation_sha256=" digest}
      {print}
    ' "${backup_paths[1]}" >"$destination" && chmod 400 -- "$destination"
  }
  resume_write_env() {
    awk -F= -v ref="$target_ref" '
      $1=="MESSAGE_SERVER_IMAGE" {$0="MESSAGE_SERVER_IMAGE=" ref; alias_seen=1}
      $1=="DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE" {$0="DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=" ref}
      {print}
      END {if (!alias_seen) print "MESSAGE_SERVER_IMAGE=" ref}
    ' "${backup_paths[2]}" >"$1" && chmod 400 -- "$1"
  }
  resume_write_receipt() {
    local env_path=$1 manifest_path=$2 container_id=$3 destination=$4 env_identity env_hash manifest_identity manifest_hash message_index
    env_identity=$(stat -c '%d:%i:%u' -- "$env_path"); env_hash=$(sha256sum -- "$env_path" | awk '{print $1}')
    manifest_identity=$(stat -c '%d:%i:%u' -- "$manifest_path"); manifest_hash=$(sha256sum -- "$manifest_path" | awk '{print $1}')
    message_index=$(read_pair "$journal" message_index)
    awk -F= -v ei="$env_identity" -v eh="$env_hash" -v mi="$manifest_identity" -v mh="$manifest_hash" -v target_index="$message_index" -v id="$container_id" '
      $1=="control.env_identity" {$0="control.env_identity=" ei}
      $1=="control.env_sha256" {$0="control.env_sha256=" eh}
      $1=="control.manifest_identity" {$0="control.manifest_identity=" mi}
      $1=="control.manifest_sha256" {$0="control.manifest_sha256=" mh}
      $1==("container." target_index ".id") {$0=$1 "=" id}
      {print}
    ' "${backup_paths[3]}" >"$destination" && chmod 400 -- "$destination"
  }
  resume_mark_rollback_ready() {
    local receipt_path=$1 container_id=$2 temp
    temp=$(mktemp "$out/.message-server-update.transaction.XXXXXX") || return 1
    awk -F= '
      $1=="state" {$0="state=rollback-ready"}
      $1=="rollback_container_id" {next}
      $1=="target.receipt.identity" {next}
      $1=="target.receipt.sha256" {next}
      {print}
    ' "$journal" >"$temp" || { rm -f -- "$temp"; return 1; }
    printf 'rollback_container_id=%s\ntarget.receipt.identity=%s\ntarget.receipt.sha256=%s\n' \
      "$container_id" "$(file_identity "$receipt_path")" "$(sha256sum -- "$receipt_path" | awk '{print $1}')" >>"$temp"
    chmod 400 -- "$temp" && chown "$required_owner:$required_group" -- "$temp" && mv -f -- "$temp" "$journal"
  }
  resume_mark_state() {
    local next_state=$1 temp
    temp=$(mktemp "$out/.message-server-update.transaction.XXXXXX") || return 1
    awk -F= -v state="$next_state" '$1=="state" {$0="state=" state} {print}' "$journal" >"$temp" || { rm -f -- "$temp"; return 1; }
    chmod 400 -- "$temp" && chown "$required_owner:$required_group" -- "$temp" && mv -f -- "$temp" "$journal"
  }
  resume_mark_target_ready() {
    local attestation_path=$1 manifest_path=$2 env_path=$3 receipt_path=$4 temp control path
    temp=$(mktemp "$out/.message-server-update.transaction.XXXXXX") || return 1
    awk -F= '$1=="state" {$0="state=target-ready"} $1 ~ /^target\.(attestation|manifest|env|receipt)\.(identity|sha256)$/ {next} {print}' "$journal" >"$temp" || { rm -f -- "$temp"; return 1; }
    for control_path in "attestation:$attestation_path" "manifest:$manifest_path" "env:$env_path" "receipt:$receipt_path"; do
      control=${control_path%%:*}; path=${control_path#*:}
      printf 'target.%s.identity=%s\ntarget.%s.sha256=%s\n' "$control" "$(file_identity "$path")" "$control" "$(sha256sum -- "$path" | awk '{print $1}')" >>"$temp"
    done
    chmod 400 -- "$temp" && chown "$required_owner:$required_group" -- "$temp" && mv -f -- "$temp" "$journal"
  }
  resume_clear() {
    rm -f -- "${pending_paths[@]}" "${backup_paths[@]}" || return 1
    rm -f -- "$journal"
  }

  if { [ "$state" = prepared ] || [ "$state" = target-ready ]; } && \
     [ "$candidate_ref" = "$target_ref" ] && [ "$candidate_status" = running ] && [ "$candidate_health" = healthy ]; then
    rm -f -- "${pending_paths[@]}" || die 'unfinished update stale pending-control cleanup failed'
    tmp_attestation=$(mktemp "$out/.image-attestation.XXXXXX"); resume_write_attestation "$tmp_attestation" || die 'unfinished update attestation completion failed'
    tmp_manifest=$(mktemp "$out/.manifest.XXXXXX"); resume_write_manifest "$tmp_attestation" "$tmp_manifest" || die 'unfinished update manifest completion failed'
    tmp_env=$(mktemp "$out/.env.XXXXXX"); resume_write_env "$tmp_env" || die 'unfinished update environment completion failed'
    tmp_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX"); resume_write_receipt "$tmp_env" "$tmp_manifest" "$candidate_id" "$tmp_receipt" || die 'unfinished update receipt completion failed'
    resume_mark_target_ready "$tmp_attestation" "$tmp_manifest" "$tmp_env" "$tmp_receipt" || die 'unfinished update target-ready journal refresh failed'
    mv -f -- "$tmp_attestation" "${live_paths[0]}"
    mv -f -- "$tmp_manifest" "${live_paths[1]}"
    mv -f -- "$tmp_env" "${live_paths[2]}"
    mv -f -- "$tmp_receipt" "${live_paths[3]}"
    resume_clear || die 'unfinished update completion cleanup failed'
    printf 'split message-server update resumed: version=%s image=%s\n' "$target_version" "$target_ref"
    return 0
  fi

  compose_env=${backup_paths[2]}
  if [ "$candidate_ref" = "$old_ref" ] && [ "$candidate_status" = running ] && [ "$candidate_health" = healthy ]; then
    rollback_id=$candidate_id
  else
    [ "$state" = rolling-back ] || resume_mark_state rolling-back || die 'unfinished update rollback intent commit failed'
    state=rolling-back
    DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE="$old_ref" docker compose --env-file "$compose_env" \
      -f "$compose_file" -f "$production_compose_file" --project-name "$stack_name" \
      up -d --no-deps --force-recreate --no-build --pull never message-server >/dev/null || die 'unfinished update old-image rollback recreate failed'
    attempts=${DIREXTALK_MESSAGE_SERVER_UPDATE_HEALTH_ATTEMPTS:-60}
    rollback_id=
    while [ "$attempts" -gt 0 ]; do
      candidate_ids=$(docker ps --no-trunc -aq --filter "label=com.docker.compose.project=$stack_name" --filter 'label=com.docker.compose.service=message-server') || die 'unfinished rollback container lookup failed'
      if [ "$(printf '%s\n' "$candidate_ids" | awk 'NF {n++} END {print n+0}')" -eq 1 ]; then
        candidate_id=$(printf '%s\n' "$candidate_ids" | awk 'NF {print; exit}')
        data=$(docker inspect "$candidate_id" 2>/dev/null) || die 'unfinished rollback container inspection failed'
        if [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$old_ref" ] && [ "$(jq -r '.[0].Image // empty' <<<"$data")" = "$old_image_id" ] && \
           [ "$(jq -r '.[0].State.Status // empty' <<<"$data")" = running ] && [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ]; then rollback_id=$candidate_id; break; fi
      fi
      attempts=$((attempts-1)); [ "$attempts" -gt 0 ] && sleep 1
    done
    [ -n "$rollback_id" ] || die 'unfinished update old-image rollback health failed'
  fi
  resume_restore "${backup_paths[0]}" "${live_paths[0]}" || die 'unfinished update attestation rollback failed'
  resume_restore "${backup_paths[1]}" "${live_paths[1]}" || die 'unfinished update manifest rollback failed'
  resume_restore "${backup_paths[2]}" "${live_paths[2]}" || die 'unfinished update environment rollback failed'
  tmp_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX"); resume_write_receipt "${live_paths[2]}" "${live_paths[1]}" "$rollback_id" "$tmp_receipt" || die 'unfinished update receipt rollback failed'
  resume_mark_rollback_ready "$tmp_receipt" "$rollback_id" || die 'unfinished update rollback journal commit failed'
  mv -f -- "$tmp_receipt" "${live_paths[3]}"
  resume_clear || die 'unfinished update rollback cleanup failed'
  if [ "$rollback_id" = "$old_message_id" ] && [ "$state" = prepared ]; then return 10; fi
  die 'unfinished target was not healthy; exact old-image rollback completed'
}

if [ -e "$transaction_journal" ] || [ -L "$transaction_journal" ]; then
  if resume_unfinished_transaction; then exit 0; else resume_status=$?; fi
  [ "$resume_status" -eq 10 ] || exit "$resume_status"
else
  orphan_backups=("$out/.message-server-update.old.attestation" "$out/.message-server-update.old.manifest" "$out/.message-server-update.old.env" "$out/.message-server-update.old.receipt")
  orphan_live=("$out/image-attestation" "$out/.manifest" "$out/.env" "$out/.cleanup-receipt")
  orphan_count=0
  for orphan_path in "${orphan_backups[@]}"; do
    [ ! -e "$orphan_path" ] && [ ! -L "$orphan_path" ] || orphan_count=$((orphan_count+1))
  done
  if [ "$orphan_count" -gt 0 ]; then
    [ "$orphan_count" -eq 4 ] || die 'incomplete pre-journal recovery set is unsafe'
    for ((orphan_index=0; orphan_index<4; orphan_index++)); do
      orphan_path=${orphan_backups[$orphan_index]}; live_path=${orphan_live[$orphan_index]}
      [ -f "$orphan_path" ] && [ ! -L "$orphan_path" ] && [ "$orphan_path" -ef "$live_path" ] || die 'pre-journal recovery file does not match the live control identity'
    done
    rm -f -- "${orphan_backups[@]}"
  fi
fi

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

old_env=$out/.message-server-update.old.env
old_manifest=$out/.message-server-update.old.manifest
old_attestation=$out/.message-server-update.old.attestation
old_receipt=$out/.message-server-update.old.receipt
transaction_journal=$out/.message-server-update.transaction
new_attestation=$out/.message-server-update.new.attestation
new_manifest=$out/.message-server-update.new.manifest
new_env=$out/.message-server-update.new.env
new_receipt=$out/.message-server-update.new.receipt
for transaction_path in "$old_env" "$old_manifest" "$old_attestation" "$old_receipt" "$transaction_journal" \
  "$new_attestation" "$new_manifest" "$new_env" "$new_receipt"; do
  [ ! -e "$transaction_path" ] && [ ! -L "$transaction_path" ] || die 'an unfinished message-server update transaction already exists'
done
ln -- "$env_file" "$old_env" || die 'could not preserve the pre-update environment identity'
ln -- "$manifest" "$old_manifest" || { rm -f -- "$old_env"; die 'could not preserve the pre-update manifest identity'; }
ln -- "$attestation" "$old_attestation" || { rm -f -- "$old_env" "$old_manifest"; die 'could not preserve the pre-update attestation identity'; }
ln -- "$receipt" "$old_receipt" || { rm -f -- "$old_env" "$old_manifest" "$old_attestation"; die 'could not preserve the pre-update receipt identity'; }
write_transaction_journal() {
  local state=$1 pending_attestation=${2:-} pending_manifest=${3:-} pending_env=${4:-} pending_receipt=${5:-} tmp control_name control_path journal_index
  local -a journal_names=(attestation manifest env receipt)
  local -a old_paths=("$old_attestation" "$old_manifest" "$old_env" "$old_receipt")
  local -a target_paths=("$pending_attestation" "$pending_manifest" "$pending_env" "$pending_receipt")
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
    if [ "$state" = target-ready ]; then
      for ((journal_index=0; journal_index<4; journal_index++)); do
        control_name=${journal_names[$journal_index]}; control_path=${target_paths[$journal_index]}
        printf 'target.%s.identity=%s\ntarget.%s.sha256=%s\n' "$control_name" "$(file_identity "$control_path")" "$control_name" "$(sha256sum -- "$control_path" | awk '{print $1}')"
      done
    elif [ "$state" = rollback-ready ]; then
      printf 'rollback_container_id=%s\n' "$pending_manifest"
      printf 'target.receipt.identity=%s\ntarget.receipt.sha256=%s\n' \
        "$(file_identity "$pending_attestation")" "$(sha256sum -- "$pending_attestation" | awk '{print $1}')"
    fi
  } >"$tmp" || { rm -f -- "$tmp"; return 1; }
  chmod 400 -- "$tmp" || { rm -f -- "$tmp"; return 1; }
  chown "$required_owner:$required_group" -- "$tmp" || { rm -f -- "$tmp"; return 1; }
  mv -f -- "$tmp" "$transaction_journal"
}
write_transaction_journal prepared || { rm -f -- "$old_env" "$old_manifest" "$old_attestation" "$old_receipt"; die 'could not durably prepare the update transaction journal'; }
mark_transaction_state() {
  local next_state=$1 temp
  temp=$(mktemp "$out/.message-server-update.transaction.XXXXXX") || return 1
  awk -F= -v state="$next_state" '$1=="state" {$0="state=" state} {print}' "$transaction_journal" >"$temp" || { rm -f -- "$temp"; return 1; }
  chmod 400 -- "$temp" && chown "$required_owner:$required_group" -- "$temp" && mv -f -- "$temp" "$transaction_journal"
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
  local source=$1 destination=$2 bound_env=$3 bound_manifest=$4 new_id=$5
  local new_env_identity new_env_sha new_manifest_identity new_manifest_sha
  new_env_identity=$(stat -c '%d:%i:%u' -- "$bound_env")
  new_env_sha=$(sha256sum -- "$bound_env" | awk '{print $1}')
  new_manifest_identity=$(stat -c '%d:%i:%u' -- "$bound_manifest")
  new_manifest_sha=$(sha256sum -- "$bound_manifest" | awk '{print $1}')
  awk -F= -v env_identity="$new_env_identity" -v env_digest="$new_env_sha" \
    -v manifest_identity="$new_manifest_identity" -v manifest_digest="$new_manifest_sha" \
    -v target_index="$message_index" -v id="$new_id" '
    $1=="control.env_identity" {$0="control.env_identity=" env_identity}
    $1=="control.env_sha256" {$0="control.env_sha256=" env_digest}
    $1=="control.manifest_identity" {$0="control.manifest_identity=" manifest_identity}
    $1=="control.manifest_sha256" {$0="control.manifest_sha256=" manifest_digest}
    $1==("container." target_index ".id") {$0=$1 "=" id}
    {print}
  ' "$source" >"$destination"
  chmod 400 -- "$destination"
}
restore_control_file() {
  local source=$1 target=$2
  if [ -e "$target" ] && [ "$target" -ef "$source" ]; then
    rm -f -- "$source"
  else
    mv -f -- "$source" "$target"
  fi
}
rollback() {
  local rollback_id rollback_receipt current_id current_data current_ref_observed current_image_observed status=0
  (verify_host_docker_identity) || return 1
  mark_transaction_state rolling-back || return 1
  current_id=$(DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE="$current_ref" docker compose --env-file "$old_env" \
    -f "$compose_file" -f "$production_compose_file" --project-name "$stack" ps -q message-server 2>/dev/null) || return 1
  printf '%s\n' "$current_id" | grep -Eq '^[0-9a-f]{64}$' || return 1
  current_data=$(docker inspect "$current_id" 2>/dev/null) || return 1
  [ "$(jq -r '.[0].Id // empty' <<<"$current_data")" = "$current_id" ] || return 1
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.project"] // empty' <<<"$current_data")" = "$stack" ] || return 1
  [ "$(jq -r '.[0].Config.Labels["com.docker.compose.service"] // empty' <<<"$current_data")" = message-server ] || return 1
  current_ref_observed=$(jq -r '.[0].Config.Image // empty' <<<"$current_data")
  current_image_observed=$(jq -r '.[0].Image // empty' <<<"$current_data")
  if [ "$current_ref_observed" = "$current_ref" ]; then
    [ "$current_image_observed" = "$current_image_id" ] || return 1
  elif [ "$current_ref_observed" = "$target_ref" ]; then
    [ "$current_image_observed" = "$target_image_id" ] || return 1
  else
    return 1
  fi
  DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE="$current_ref" docker compose --env-file "$old_env" \
    -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
    up -d --no-deps --force-recreate --no-build --pull never message-server >/dev/null || status=1
  if [ "$status" -eq 0 ]; then
    rollback_id=$(wait_message "$old_env" "$current_image_id" "$current_ref") || status=1
  fi
  [ "$status" -eq 0 ] || return 1
  restore_control_file "$old_attestation" "$attestation" || return 1
  restore_control_file "$old_manifest" "$manifest" || return 1
  restore_control_file "$old_env" "$env_file" || return 1
  rollback_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX") || return 1
  write_receipt "$old_receipt" "$rollback_receipt" "$env_file" "$manifest" "$rollback_id" || { rm -f -- "$rollback_receipt"; return 1; }
  write_transaction_journal rollback-ready "$rollback_receipt" "$rollback_id" || { rm -f -- "$rollback_receipt"; return 1; }
  mv -f -- "$rollback_receipt" "$receipt" || { rm -f -- "$rollback_receipt"; return 1; }
}
transaction_exit() {
  local status=$? recovery_status=0 preserve_transaction=false
  trap - EXIT
  set +e
  if [ "$mutated" = true ] && [ "$commit_complete" = false ]; then
    rollback || recovery_status=1
    if [ "$recovery_status" -ne 0 ]; then
      printf 'split message-server update: update failed and exact old-image/receipt rollback did not complete\n' >&2
      status=1
      preserve_transaction=true
    fi
  fi
  [ -z "$new_env" ] || rm -f -- "$new_env"
  [ -z "$new_manifest" ] || rm -f -- "$new_manifest"
  [ -z "$new_attestation" ] || rm -f -- "$new_attestation"
  [ -z "$new_receipt" ] || rm -f -- "$new_receipt"
  if [ "$preserve_transaction" = false ]; then
    rm -f -- "$old_env" "$old_manifest" "$old_attestation" "$old_receipt" || true
    rm -f -- "$transaction_journal"
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

write_attestation "$old_attestation" "$new_attestation" "$target_ref" "$target_revision" || die 'could not render updated image attestation'
write_manifest "$old_manifest" "$new_manifest" "$new_attestation" || die 'could not render updated manifest'
write_env "$old_env" "$new_env" "$target_ref" || die 'could not render updated environment'
write_receipt "$old_receipt" "$new_receipt" "$new_env" "$new_manifest" "$new_message_id" || die 'could not render updated cleanup receipt'
write_transaction_journal target-ready "$new_attestation" "$new_manifest" "$new_env" "$new_receipt" || die 'could not durably record target-ready control identities'
mv -f -- "$new_attestation" "$attestation"
new_attestation=
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = attestation ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = attestation ]; then
  die 'injected image attestation commit failure'
fi
mv -f -- "$new_manifest" "$manifest"
new_manifest=
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = manifest ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = manifest ]; then
  die 'injected manifest commit failure'
fi
mv -f -- "$new_env" "$env_file"
new_env=
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = env ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = env ]; then
  die 'injected environment commit failure'
fi
mv -f -- "$new_receipt" "$receipt"
new_receipt=
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT:-}" = receipt ]; then kill -KILL "$$"; fi
if [ "$test_fixture" = true ] && [ "${DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT:-}" = receipt ]; then
  die 'injected cleanup receipt commit failure'
fi
commit_complete=true
mutated=false
trap - EXIT
rm -f -- "$old_env" "$old_manifest" "$old_attestation" "$old_receipt"
rm -f -- "$transaction_journal"
printf 'split message-server update passed: version=%s image=%s\n' "$target_version" "$target_ref"
