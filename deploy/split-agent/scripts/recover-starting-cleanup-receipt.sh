#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "split-stack cleanup recovery: $*" >&2
  exit 1
}

[ "$#" -eq 1 ] || die "usage: $0 OUTPUT_DIR"
case "$1" in /*) out=$(readlink -m -- "$1") ;; *) out=$(readlink -m -- "$(pwd -P)/$1") ;; esac
[ "$out" != / ] && [ -d "$out" ] && [ ! -L "$out" ] || die "output directory is unsafe"
[ "$(stat -c '%a' "$out")" = 700 ] || die "output directory must be mode 0700"
env_file=$out/.env
manifest=$out/.manifest
receipt=$out/.cleanup-receipt
for file in "$env_file" "$manifest" "$receipt"; do
  [ -f "$file" ] && [ ! -L "$file" ] || die "missing regular control file: $file"
  [ "$(stat -c '%a' "$file")" = 400 ] || die "control file must be mode 0400: $file"
  [ "$(stat -c '%u' "$file")" = "$(id -u)" ] || die "control file owner differs: $file"
done

read_pair() {
  local file=$1 key=$2 count value
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 {n++} END {print n+0}' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 {print substr($0,length(wanted)+2); exit}' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

grep -Fqx '# dirextalk-split-cleanup-receipt-v1' "$receipt" || die "cleanup receipt version is unsupported"
stack_name=$(read_pair "$receipt" stack_name)
[ "$(read_pair "$receipt" state)" = starting ] || die "cleanup receipt is not a starting journal"
[ "$stack_name" = "$(read_pair "$manifest" stack_name)" ] || die "receipt stack differs from manifest"
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "stack identity is invalid"

env_identity=$(stat -c '%d:%i:%u' "$env_file")
manifest_identity=$(stat -c '%d:%i:%u' "$manifest")
receipt_identity=$(stat -c '%d:%i:%u' "$receipt")
receipt_env_identity=$(read_pair "$receipt" control.env_identity)
receipt_manifest_identity=$(read_pair "$receipt" control.manifest_identity)
receipt_env_sha256=$(read_pair "$receipt" control.env_sha256)
receipt_manifest_sha256=$(read_pair "$receipt" control.manifest_sha256)
machine_id=$(read_pair "$receipt" host.machine_id)
engine_id=$(read_pair "$receipt" docker.engine_id)
context_endpoint=$(read_pair "$receipt" docker.context_endpoint)
context_socket=$(read_pair "$receipt" docker.context_socket)
[ "$receipt_env_identity" = "$env_identity" ] || die "receipt .env identity differs"
[ "$receipt_manifest_identity" = "$manifest_identity" ] || die "receipt manifest identity differs"
[ "$receipt_env_sha256" = "$(sha256sum -- "$env_file" | awk '{print $1}')" ] || die "receipt .env digest differs"
[ "$receipt_manifest_sha256" = "$(sha256sum -- "$manifest" | awk '{print $1}')" ] || die "receipt manifest digest differs"
[ "$machine_id" = "$(read_pair "$manifest" runner.machine_id)" ] || die "receipt machine-id differs from manifest"
[ "$engine_id" = "$(read_pair "$manifest" runner.docker_engine_id)" ] || die "receipt Engine ID differs from manifest"
[ "$context_socket" = /run/docker.sock ] || die "receipt Docker socket is not canonical rootful Docker"
case "$context_endpoint" in unix:///*) ;; *) die "receipt Docker endpoint is not local Unix" ;; esac

mapfile -t networks < <(awk -F= '$1 ~ /^resource\.network\.[a-z0-9_]+$/ {print $2}' "$manifest")
mapfile -t volumes < <(awk -F= '$1 ~ /^resource\.volume\.[a-z0-9_]+$/ {print $2}' "$manifest")
[ "${#networks[@]}" -gt 0 ] && [ "${#volumes[@]}" -gt 0 ] || die "manifest planned resource list is empty"
[ "$(printf '%s\n' "${networks[@]}" | sort -u | wc -l)" -eq "${#networks[@]}" ] || die "manifest has duplicate network names"
[ "$(printf '%s\n' "${volumes[@]}" | sort -u | wc -l)" -eq "${#volumes[@]}" ] || die "manifest has duplicate volume names"
[ "$(read_pair "$receipt" planned.network.count)" -eq "${#networks[@]}" ] || die "planned network count differs from manifest"
[ "$(read_pair "$receipt" planned.volume.count)" -eq "${#volumes[@]}" ] || die "planned volume count differs from manifest"
for ((i=0; i<${#networks[@]}; i++)); do
  [ "${networks[i]}" = "$(read_pair "$receipt" planned.network.$i.name)" ] || die "planned network differs from manifest"
  case "${networks[i]}" in "$stack_name"-*) ;; *) die "planned network is outside stack namespace" ;; esac
done
for ((i=0; i<${#volumes[@]}; i++)); do
  [ "${volumes[i]}" = "$(read_pair "$receipt" planned.volume.$i.name)" ] || die "planned volume differs from manifest"
  case "${volumes[i]}" in "$stack_name"-*) ;; *) die "planned volume is outside stack namespace" ;; esac
done

runner_roles=(extension core)
runner_units=("$(read_pair "$receipt" runner.extension.unit)" "$(read_pair "$receipt" runner.core.unit)")
runner_groups=("$(read_pair "$receipt" runner.extension.control_group)" "$(read_pair "$receipt" runner.core.control_group)")
runner_pids=("$(read_pair "$receipt" runner.extension.main_pid)" "$(read_pair "$receipt" runner.core.main_pid)")
runner_fragments=("$(read_pair "$receipt" runner.extension.fragment_path)" "$(read_pair "$receipt" runner.core.fragment_path)")
runner_hashes=("$(read_pair "$receipt" runner.extension.fragment_sha256)" "$(read_pair "$receipt" runner.core.fragment_sha256)")
for ((i=0; i<2; i++)); do
  role=${runner_roles[i]}
  printf '%s\n' "${runner_units[i]}" | grep -Eq "^dirextalk-${role}-runner@${stack_name}\.service$" || die "$role runner unit is not stack-bound"
  printf '%s\n' "${runner_pids[i]}" | grep -Eq '^[1-9][0-9]*$' || die "$role runner PID is invalid"
  [ -f "${runner_fragments[i]}" ] && [ ! -L "${runner_fragments[i]}" ] || die "$role runner fragment is missing"
  [ "$(stat -c '%u:%g' "${runner_fragments[i]}")" = 0:0 ] || die "$role runner fragment is not root-owned"
  [ "$(stat -c '%a' "${runner_fragments[i]}")" = 644 ] || die "$role runner fragment mode differs"
  [ "$(sha256sum -- "${runner_fragments[i]}" | awk '{print $1}')" = "${runner_hashes[i]}" ] || die "$role runner fragment digest differs"
done

verify_controls() {
  [ -f "$receipt" ] && [ ! -L "$receipt" ] || die "cleanup receipt was replaced"
  [ "$(stat -c '%d:%i:%u' "$receipt")" = "$receipt_identity" ] || die "cleanup receipt identity changed"
  [ "$(stat -c '%a' "$receipt")" = 400 ] || die "cleanup receipt mode changed"
  [ "$(stat -c '%d:%i:%u' "$env_file")" = "$env_identity" ] || die ".env identity changed"
  [ "$(stat -c '%d:%i:%u' "$manifest")" = "$manifest_identity" ] || die "manifest identity changed"
  [ "$(sha256sum -- "$env_file" | awk '{print $1}')" = "$receipt_env_sha256" ] || die ".env content changed"
  [ "$(sha256sum -- "$manifest" | awk '{print $1}')" = "$receipt_manifest_sha256" ] || die "manifest content changed"
}

verify_host() {
  local endpoint socket canonical current_machine current_engine
  [ -z "${DOCKER_HOST:-}" ] || die "DOCKER_HOST must be unset"
  case "${DOCKER_CONTEXT:-default}" in ''|default) ;; *) die "DOCKER_CONTEXT must be default" ;; esac
  endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}') || die "Docker context inspection failed"
  [ "$endpoint" = "$context_endpoint" ] || die "Docker context endpoint changed"
  socket=${endpoint#unix://}
  [ -S "$socket" ] || die "Docker socket is unavailable"
  canonical=$(readlink -f -- "$socket") || die "Docker socket canonicalization failed"
  [ "$canonical" = "$context_socket" ] || die "Docker socket identity changed"
  [ -f /etc/machine-id ] && [ ! -L /etc/machine-id ] && [ "$(stat -c '%u:%g' /etc/machine-id)" = 0:0 ] || die "host machine-id control is unsafe"
  current_machine=$(tr -d '[:space:]' </etc/machine-id)
  [ "$current_machine" = "$machine_id" ] || die "host machine-id changed"
  current_engine=$(docker info --format '{{.ID}}') || die "Docker Engine inspection failed"
  [ "$current_engine" = "$engine_id" ] || die "Docker Engine ID changed"
}

verify_runners() {
  local i role unit value
  for ((i=0; i<2; i++)); do
    role=${runner_roles[i]}; unit=${runner_units[i]}
    value=$(systemctl show "$unit" --property=LoadState --value) || die "$role runner load-state inspection failed"
    [ "$value" = loaded ] || die "$role runner is not loaded"
    value=$(systemctl show "$unit" --property=FragmentPath --value) || die "$role runner fragment inspection failed"
    [ "$value" = "${runner_fragments[i]}" ] || die "$role runner FragmentPath changed"
    value=$(systemctl show "$unit" --property=ControlGroup --value) || die "$role runner ControlGroup inspection failed"
    [ "$value" = "${runner_groups[i]}" ] || die "$role runner ControlGroup changed"
    value=$(systemctl show "$unit" --property=MainPID --value) || die "$role runner PID inspection failed"
    [ "$value" = "${runner_pids[i]}" ] || die "$role runner PID changed"
  done
}

inspect_error=$(mktemp "$out/.cleanup-recovery-inspect.XXXXXX") || die "cannot allocate inspection error file"
tmp_receipt=
cleanup_tmp() { rm -f -- "$inspect_error" ${tmp_receipt:+"$tmp_receipt"}; }
trap cleanup_tmp EXIT

inspect_object() {
  local kind=$1 target=$2 data status
  local -a command
  case "$kind" in container) command=(docker inspect "$target") ;; network|volume) command=(docker "$kind" inspect "$target") ;; *) return 2 ;; esac
  : >"$inspect_error"
  if data=$("${command[@]}" 2>"$inspect_error"); then
    jq -e 'type=="array" and length==1' <<<"$data" >/dev/null || return 2
    printf '%s' "$data"; return 0
  else status=$?; fi
  if [ "$status" -eq 1 ] && grep -Eiq 'no such (object|container|network|volume)|not found' "$inspect_error"; then return 1; fi
  return 2
}

volume_fingerprint() {
  local canonical
  canonical=$(jq -c -e '.[0] as $v | if (($v.Name|type)=="string" and ($v.Driver|type)=="string" and ($v.Scope|type)=="string" and ($v.CreatedAt|type)=="string" and ($v.Mountpoint|type)=="string" and (($v.Labels==null) or (($v.Labels|type)=="object")) and (($v.Options==null) or (($v.Options|type)=="object"))) then {Name:$v.Name,Driver:$v.Driver,Scope:$v.Scope,CreatedAt:$v.CreatedAt,Mountpoint:$v.Mountpoint,Labels:($v.Labels//{}|to_entries|sort_by(.key)|from_entries),Options:($v.Options//{}|to_entries|sort_by(.key)|from_entries)} else error("incomplete") end' <<<"$1") || return 1
  printf '%s' "$canonical" | sha256sum | awk '{print $1}'
}

capture_objects() {
  local ids id data status actual_id raw name service project replica planned network_id network_name network_project volume_name volume_project fingerprint record i
  local -a sorted=() containers=() found_networks=() found_volumes=()
  # Cleanup receipts bind the immutable full container ID. Docker's default
  # quiet listing is truncated, so request the full value before validation.
  ids=$(docker ps --no-trunc -aq --filter "label=com.docker.compose.project=$stack_name") || return 2
  [ -z "$ids" ] || mapfile -t sorted < <(printf '%s\n' "$ids" | sed '/^$/d' | sort -u)
  for id in "${sorted[@]}"; do
    printf '%s\n' "$id" | grep -Eq '^[0-9a-f]{64}$' || return 2
    if data=$(inspect_object container "$id"); then :; else status=$?; [ "$status" -eq 1 ] && continue; return 2; fi
    actual_id=$(jq -r '.[0].Id//empty' <<<"$data"); raw=$(jq -r '.[0].Name//empty' <<<"$data")
    service=$(jq -r '.[0].Config.Labels["com.docker.compose.service"]//empty' <<<"$data"); project=$(jq -r '.[0].Config.Labels["com.docker.compose.project"]//empty' <<<"$data")
    [ "$actual_id" = "$id" ] && [ "$project" = "$stack_name" ] || return 2
    case "$raw" in /*) name=${raw#/} ;; *) return 2 ;; esac
    printf '%s\n' "$service" | grep -Eq '^[a-z0-9][a-z0-9_-]*$' || return 2
    case "$name" in "$stack_name-$service"-*) replica=${name#"$stack_name-$service"-} ;; *) return 2 ;; esac
    printf '%s\n' "$replica" | grep -Eq '^[1-9][0-9]*$' || return 2
    containers+=("$id|$name|$service|$project")
  done
  for planned in "${networks[@]}"; do
    if data=$(inspect_object network "$planned"); then :; else status=$?; [ "$status" -eq 1 ] && continue; return 2; fi
    network_id=$(jq -r '.[0].Id//empty' <<<"$data"); network_name=$(jq -r '.[0].Name//empty' <<<"$data"); network_project=$(jq -r '.[0].Labels["com.docker.compose.project"]//empty' <<<"$data")
    printf '%s\n' "$network_id" | grep -Eq '^[0-9a-f]{64}$' || return 2
    [ "$network_name" = "$planned" ] && [ "$network_project" = "$stack_name" ] || return 2
    found_networks+=("$network_id|$network_name|$network_project")
  done
  for planned in "${volumes[@]}"; do
    if data=$(inspect_object volume "$planned"); then :; else status=$?; [ "$status" -eq 1 ] && continue; return 2; fi
    volume_name=$(jq -r '.[0].Name//empty' <<<"$data"); volume_project=$(jq -r '.[0].Labels["com.docker.compose.project"]//empty' <<<"$data"); fingerprint=$(volume_fingerprint "$data") || return 2
    [ "$volume_name" = "$planned" ] && [ "$volume_project" = "$stack_name" ] || return 2
    found_volumes+=("$volume_name|$volume_project|$fingerprint")
  done
  printf 'container.count=%s\n' "${#containers[@]}"; i=0
  for record in "${containers[@]}"; do IFS='|' read -r id name service project <<<"$record"; printf 'container.%s.id=%s\ncontainer.%s.name=%s\ncontainer.%s.service=%s\ncontainer.%s.project=%s\n' "$i" "$id" "$i" "$name" "$i" "$service" "$i" "$project"; i=$((i+1)); done
  printf 'network.count=%s\n' "${#found_networks[@]}"; i=0
  for record in "${found_networks[@]}"; do IFS='|' read -r network_id network_name network_project <<<"$record"; printf 'network.%s.id=%s\nnetwork.%s.name=%s\nnetwork.%s.project=%s\n' "$i" "$network_id" "$i" "$network_name" "$i" "$network_project"; i=$((i+1)); done
  printf 'volume.count=%s\n' "${#found_volumes[@]}"; i=0
  for record in "${found_volumes[@]}"; do IFS='|' read -r volume_name volume_project fingerprint <<<"$record"; printf 'volume.%s.name=%s\nvolume.%s.project=%s\nvolume.%s.fingerprint_sha256=%s\n' "$i" "$volume_name" "$i" "$volume_project" "$i" "$fingerprint"; i=$((i+1)); done
}

verify_controls; verify_host; verify_runners
first=$(capture_objects) || die "Docker infrastructure or object ownership inspection failed"
verify_controls; verify_host; verify_runners
second=$(capture_objects) || die "Docker pre-mutation identity revalidation failed"
[ "$first" = "$second" ] || die "Docker object identity drifted during recovery"

tmp_receipt=$(mktemp "$out/.cleanup-receipt.XXXXXX") || die "cannot allocate recovered receipt"
{
  printf '%s\n' '# dirextalk-split-cleanup-receipt-v1'
  printf 'stack_name=%s\nstate=incomplete\n' "$stack_name"
  printf 'control.env_identity=%s\ncontrol.manifest_identity=%s\n' "$receipt_env_identity" "$receipt_manifest_identity"
  printf 'control.env_sha256=%s\ncontrol.manifest_sha256=%s\n' "$receipt_env_sha256" "$receipt_manifest_sha256"
  printf 'host.machine_id=%s\ndocker.engine_id=%s\n' "$machine_id" "$engine_id"
  printf 'docker.context_endpoint=%s\ndocker.context_socket=%s\n' "$context_endpoint" "$context_socket"
  printf '%s\n' "$second"
  for ((i=0; i<2; i++)); do role=${runner_roles[i]}; printf 'runner.%s.unit=%s\nrunner.%s.control_group=%s\nrunner.%s.main_pid=%s\nrunner.%s.fragment_path=%s\nrunner.%s.fragment_sha256=%s\n' "$role" "${runner_units[i]}" "$role" "${runner_groups[i]}" "$role" "${runner_pids[i]}" "$role" "${runner_fragments[i]}" "$role" "${runner_hashes[i]}"; done
} >"$tmp_receipt" || die "cannot write recovered receipt"
chmod 400 -- "$tmp_receipt" || die "cannot protect recovered receipt"
verify_controls; verify_host; verify_runners
third=$(capture_objects) || die "final Docker identity revalidation failed"
[ "$third" = "$second" ] || die "Docker object identity drifted before recovery commit"
mv -- "$tmp_receipt" "$receipt" || die "cannot commit recovered receipt"
tmp_receipt=
printf 'split-stack cleanup recovery: captured exact partial-stack identities in %s\n' "$receipt"
