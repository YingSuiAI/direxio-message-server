#!/usr/bin/env bash
set -Eeuo pipefail

# Install or remove the one host AppArmor profile shared by the two isolated
# runner containers. Exit 0 means the requested mutation/postcondition
# succeeded, 3 is an expected negative state (already absent or still in use),
# and 1 is an unsafe identity/infrastructure failure.

usage() {
  echo "usage: $0 install|verify|remove" >&2
  exit 2
}

die() {
  echo "runner AppArmor management: $*" >&2
  exit 1
}

expected_negative() {
  echo "runner AppArmor management: expected negative: $*" >&2
  exit 3
}

[ "$#" -eq 1 ] || usage
action=$1
case "$action" in
  install|verify|remove) ;;
  *) usage ;;
esac

if [ "${DIREXTALK_SPLIT_TEST_MODE:-false}" != true ]; then
  export PATH=/usr/sbin:/usr/bin:/sbin:/bin
fi
profile_name=dirextalk-runner-userns
script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
source_path=$(cd -- "$script_dir/../apparmor.d" && pwd -P)/$profile_name
target_dir=/etc/apparmor.d
loaded_profiles=/sys/kernel/security/apparmor/profiles

# Test-only path substitution is deliberately unavailable in production.
if [ "${DIREXTALK_SPLIT_TEST_MODE:-false}" = true ]; then
  target_dir=${DIREXTALK_APPARMOR_TARGET_DIR:-$target_dir}
  loaded_profiles=${DIREXTALK_APPARMOR_LOADED_PROFILES:-$loaded_profiles}
fi
target_path=$target_dir/$profile_name

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

for command_name in apparmor_parser awk chmod chown cmp docker grep id install readlink rm sha256sum stat tr; do
  require_command "$command_name"
done

[ "$(id -u)" = 0 ] || {
  [ "${DIREXTALK_SPLIT_TEST_MODE:-false}" = true ] || die "root is required"
}

require_root_owned_asset() {
  local label=$1 path=$2 current parent mode permissions
  [ -f "$path" ] && [ ! -L "$path" ] || die "$label is missing, not regular, or symlinked: $path"
  if [ "${DIREXTALK_SPLIT_TEST_MODE:-false}" != true ]; then
    [ "$(stat -c '%u:%g' -- "$path")" = 0:0 ] || die "$label must be root-owned: $path"
  fi
  mode=$(stat -c '%a' -- "$path")
  permissions=$((8#$mode))
  (( (permissions & 18) == 0 )) || die "$label must not be group/world writable: $path"
  [ "${DIREXTALK_SPLIT_TEST_MODE:-false}" = true ] && return 0
  current=${path%/*}
  while :; do
    [ -d "$current" ] && [ ! -L "$current" ] || die "$label parent is unsafe: $current"
    [ "$(stat -c '%u:%g' -- "$current")" = 0:0 ] || die "$label parent must be root-owned: $current"
    mode=$(stat -c '%a' -- "$current")
    permissions=$((8#$mode))
    (( (permissions & 18) == 0 )) || die "$label parent must not be group/world writable: $current"
    [ "$current" = / ] && break
    parent=${current%/*}
    [ -n "$parent" ] || parent=/
    current=$parent
  done
}

profile_hash() {
  sha256sum -- "$1" | awk '{print $1}'
}

profile_loaded() {
  [ -f "$loaded_profiles" ] && [ ! -L "$loaded_profiles" ] || die "loaded-profile registry is unavailable or symlinked"
  grep -Eq "^${profile_name} \\(unconfined\\)$" "$loaded_profiles"
}

require_root_owned_asset "repository profile" "$source_path"
source_hash=$(profile_hash "$source_path")
printf '%s\n' "$source_hash" | grep -Eq '^[0-9a-f]{64}$' || die "repository profile SHA-256 is invalid"

if [ "$action" = install ]; then
  [ -d "$target_dir" ] && [ ! -L "$target_dir" ] || die "AppArmor policy directory is missing or symlinked"
  if [ -e "$target_path" ]; then
    require_root_owned_asset "installed profile" "$target_path"
    [ "$(stat -c '%a' -- "$target_path")" = 644 ] || die "installed profile mode must be 0644"
    [ "$(profile_hash "$target_path")" = "$source_hash" ] || die "installed same-name profile differs from repository profile"
  else
    if [ "${DIREXTALK_SPLIT_TEST_MODE:-false}" = true ]; then
      install -m 0644 -- "$source_path" "$target_path" || die "profile installation failed"
    else
      install -o root -g root -m 0644 -- "$source_path" "$target_path" || die "profile installation failed"
    fi
  fi
  require_root_owned_asset "installed profile" "$target_path"
  [ "$(stat -c '%a' -- "$target_path")" = 644 ] || die "installed profile mode postcondition failed"
  [ "$(profile_hash "$target_path")" = "$source_hash" ] || die "installed profile hash postcondition failed"
  apparmor_parser --replace -- "$target_path" >/dev/null || die "profile parser/load failed"
  profile_loaded || die "profile load postcondition failed"
  printf 'profile=%s\nprofile_path=%s\nprofile_sha256=%s\n' "$profile_name" "$target_path" "$source_hash"
  exit 0
fi

if [ ! -e "$target_path" ]; then
  [ "$action" != verify ] || die "installed profile is absent"
  profile_loaded && die "profile is loaded without its repository-owned policy file"
  expected_negative "profile is already absent"
fi
require_root_owned_asset "installed profile" "$target_path"
[ "$(stat -c '%a' -- "$target_path")" = 644 ] || die "installed profile mode must be 0644"
[ "$(profile_hash "$target_path")" = "$source_hash" ] || die "installed profile hash differs; refusing removal"
profile_loaded || die "installed profile is unexpectedly not loaded"

if [ "$action" = verify ]; then
  printf 'verified_profile=%s\nverified_path=%s\nverified_sha256=%s\n' "$profile_name" "$target_path" "$source_hash"
  exit 0
fi

container_ids=$(docker ps --all --quiet) || die "Docker container inventory failed"
for container_id in $container_ids; do
  attached_profile=$(docker inspect --format '{{.AppArmorProfile}}' "$container_id") || die "Docker profile inspection failed: $container_id"
  [ "$attached_profile" != "$profile_name" ] || expected_negative "profile is still referenced by Docker container $container_id"
done

# Also fence non-Docker processes and races where a container disappeared from
# the inventory but its task has not left the profile yet.
for current_label in /proc/[0-9]*/attr/current; do
  [ -r "$current_label" ] || continue
  label=$(tr -d '\000' <"$current_label" 2>/dev/null | awk '{print $1}' || true)
  [ "$label" != "$profile_name" ] || expected_negative "profile is still attached to a live process"
done

apparmor_parser --remove -- "$target_path" >/dev/null || die "profile unload failed"
if profile_loaded; then
  die "profile remained loaded after unload"
fi
rm -- "$target_path" || die "installed profile removal failed"
[ ! -e "$target_path" ] || die "installed profile removal postcondition failed"
printf 'removed_profile=%s\nremoved_sha256=%s\n' "$profile_name" "$source_hash"
