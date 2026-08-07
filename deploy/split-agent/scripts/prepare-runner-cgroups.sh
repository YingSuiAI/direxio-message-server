#!/usr/bin/env bash
# Prepare the two root-owned, systemd-delegated cgroup subtrees used by the
# isolated runner containers.  This command is intentionally host-specific:
# it accepts only a freshly generated stack identity and never consumes a
# caller-supplied unit, user, path, command, or image.
set -Eeuo pipefail

usage() {
  echo "usage: $0 [--dry-run] STACK_NAME" >&2
  exit 2
}

die() {
  echo "runner cgroup preparation: $*" >&2
  exit 1
}

export PATH=/usr/sbin:/usr/bin:/sbin:/bin

dry_run=false
if [ "${1:-}" = --dry-run ]; then
  dry_run=true
  shift
fi
[ "$#" -eq 1 ] || usage
stack_name=$1
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || \
  die "STACK_NAME must be a fresh d-<26-char-lower-base32> identity"

extension_uid=65531
extension_gid=65531
extension_user=dirextalk-extension-runner
extension_template=dirextalk-extension-runner@.service
extension_unit=dirextalk-extension-runner@${stack_name}.service
extension_parent=${stack_name}-extension.slice

core_uid=65530
core_gid=65530
core_user=dirextalk-core-runner
core_template=dirextalk-core-runner@.service
core_unit=dirextalk-core-runner@${stack_name}.service
core_parent=${stack_name}-core-runner.slice

sysusers_file=dirextalk-split-agent.conf
script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
systemd_source_dir=$(cd -- "$script_dir/../systemd" && pwd -P)
sysusers_source_dir=$(cd -- "$script_dir/../sysusers.d" && pwd -P)
apparmor_source_dir=$(cd -- "$script_dir/../apparmor.d" && pwd -P)
extension_source=$systemd_source_dir/$extension_template
core_source=$systemd_source_dir/$core_template
sysusers_source=$sysusers_source_dir/$sysusers_file
apparmor_profile_name=dirextalk-runner-userns
apparmor_source=$apparmor_source_dir/$apparmor_profile_name
apparmor_manager=$script_dir/manage-runner-apparmor.sh
apparmor_target=/etc/apparmor.d/$apparmor_profile_name
helper_path=$(readlink -f -- "$0" 2>/dev/null || true)
helper_hash=$(sha256sum -- "$helper_path" 2>/dev/null | awk '{print $1}' || true)
case "$helper_path" in
  *[[:space:]]*) die "helper path must not contain whitespace" ;;
esac

unit_dir=/etc/systemd/system
sysusers_dir=/etc/sysusers.d
cgroup_fs=/sys/fs/cgroup
machine_id=unknown
docker_engine_id=unknown

print_env() {
  local extension_root=$1 core_root=$2 extension_cgroup=$3 core_cgroup=$4
  local extension_hash=$5 core_hash=$6
  printf 'DIREXTALK_EXTENSION_CGROUP_ROOT=%s\n' "$extension_root"
  printf 'DIREXTALK_CORE_RUNNER_CGROUP_ROOT=%s\n' "$core_root"
  printf 'DIREXTALK_EXTENSION_CGROUP_PARENT=%s\n' "$extension_parent"
  printf 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT=%s\n' "$core_parent"
  printf 'DIREXTALK_CORE_EXTENSION_RUNNER_UID=%s\n' "$extension_uid"
  printf 'DIREXTALK_CORE_WORKLOAD_RUNNER_UID=%s\n' "$core_uid"
  printf 'DIREXTALK_EXTENSION_RUNNER_UNIT=%s\n' "$extension_unit"
  printf 'DIREXTALK_CORE_RUNNER_UNIT=%s\n' "$core_unit"
  printf 'DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH=%s\n' "$unit_dir/$extension_template"
  printf 'DIREXTALK_CORE_RUNNER_FRAGMENT_PATH=%s\n' "$unit_dir/$core_template"
  printf 'DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256=%s\n' "$extension_hash"
  printf 'DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256=%s\n' "$core_hash"
  printf 'DIREXTALK_RUNNER_APPARMOR_PROFILE=%s\n' "$apparmor_profile_name"
  printf 'DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH=%s\n' "$apparmor_target"
  printf 'DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256=%s\n' "$apparmor_hash"
  printf 'DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH=%s\n' "$apparmor_manager"
  printf 'DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256=%s\n' "$apparmor_manager_hash"
  printf 'DIREXTALK_RUNNER_PREP_HELPER_PATH=%s\n' "$helper_path"
  printf 'DIREXTALK_RUNNER_PREP_HELPER_SHA256=%s\n' "$helper_hash"
  printf 'DIREXTALK_RUNNER_PREP_MACHINE_ID=%s\n' "$machine_id"
  printf 'DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID=%s\n' "$docker_engine_id"
  printf 'DIREXTALK_EXTENSION_CONTROL_GROUP=%s\n' "$extension_cgroup"
  printf 'DIREXTALK_CORE_RUNNER_CONTROL_GROUP=%s\n' "$core_cgroup"
  printf 'DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT=%s\n' "${extension_parent_root:-unknown}"
  printf 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT=%s\n' "${core_parent_root:-unknown}"
  printf 'DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS=%s\n' "${extension_parent_procs:-unknown}"
  printf 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS=%s\n' "${core_parent_procs:-unknown}"
  printf 'DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER=%s:%s\n' "$extension_uid" "$extension_gid"
  printf 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER=%s:%s\n' "$core_uid" "$core_gid"
  printf 'DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE=644\n'
  printf 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE=644\n'
}

source_hash() {
  sha256sum -- "$1" | awk '{print $1}'
}

[ -f "$extension_source" ] && [ ! -L "$extension_source" ] || \
  die "missing repository-owned extension template: $extension_source"
[ -f "$core_source" ] && [ ! -L "$core_source" ] || \
  die "missing repository-owned Core template: $core_source"
[ -f "$sysusers_source" ] && [ ! -L "$sysusers_source" ] || \
  die "missing repository-owned sysusers file: $sysusers_source"
[ -f "$apparmor_source" ] && [ ! -L "$apparmor_source" ] || \
  die "missing repository-owned AppArmor profile: $apparmor_source"
[ -f "$apparmor_manager" ] && [ ! -L "$apparmor_manager" ] || \
  die "missing repository-owned AppArmor manager: $apparmor_manager"

require_root_owned_immutable() {
  local path=$1 current parent mode permissions
  [ -e "$path" ] && [ ! -L "$path" ] || die "immutable production asset is missing or symlinked: $path"
  [ "$(stat -c '%u:%g' -- "$path")" = 0:0 ] || die "production asset must be root-owned: $path"
  mode=$(stat -c '%a' -- "$path")
  permissions=$((8#$mode))
  if (( (permissions & 18) != 0 )); then
    die "production asset is group/world writable: $path"
  fi
  current=$path
  [ -f "$current" ] && current=${current%/*}
  while :; do
    [ -d "$current" ] && [ ! -L "$current" ] || die "production asset parent is not a directory: $current"
    [ "$(stat -c '%u:%g' -- "$current")" = 0:0 ] || die "production asset parent must be root-owned: $current"
    mode=$(stat -c '%a' -- "$current")
    permissions=$((8#$mode))
    if (( (permissions & 18) != 0 )); then
      die "production asset parent is group/world writable: $current"
    fi
    [ "$current" = / ] && break
    parent=${current%/*}
    [ -n "$parent" ] || parent=/
    current=$parent
  done
}

extension_hash=$(source_hash "$extension_source")
core_hash=$(source_hash "$core_source")
apparmor_hash=$(source_hash "$apparmor_source")
apparmor_manager_hash=$(source_hash "$apparmor_manager")

if [ "$dry_run" = true ]; then
  # A dry run is deliberately deterministic and has no host side effects.
  # Cgroup roots and host identities remain unknown until systemd and Docker
  # are queried by a real rootful run; never synthesize a hierarchy here.
  print_env \
    unknown \
    unknown \
    unknown \
    unknown \
    "$extension_hash" "$core_hash"
  exit 0
fi

[ -n "$helper_path" ] && [ "$helper_path" = "$script_dir/prepare-runner-cgroups.sh" ] || \
  die "helper path must resolve to the repository-owned production entrypoint"
require_root_owned_immutable "$helper_path"
require_root_owned_immutable "$extension_source"
require_root_owned_immutable "$core_source"
require_root_owned_immutable "$sysusers_source"
require_root_owned_immutable "$apparmor_source"
require_root_owned_immutable "$apparmor_manager"
if [ -z "$helper_hash" ] || ! printf '%s\n' "$helper_hash" | grep -Eq '^[0-9a-f]{64}$'; then
  die "helper package SHA-256 is unavailable"
fi

# Install/reload the fixed userns exception before creating runner units. The
# manager refuses same-name policy drift and verifies the loaded profile.
"$apparmor_manager" install >/dev/null || die "runner AppArmor profile installation failed"

[ "$(id -u)" = 0 ] || die "root is required to install static users and system units"

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

for command_name in \
  awk cat chmod chown cmp cp docker getent grep id install ln mktemp readlink rm sha256sum sleep stat tee tr \
  systemctl systemd-analyze systemd-sysusers; do
  require_command "$command_name"
done

if ! command -v setpriv >/dev/null 2>&1 && ! command -v runuser >/dev/null 2>&1; then
  die "setpriv or runuser is required for runner-identity cgroup write probes"
fi

require_regular_nonsymlink() {
  local path=$1
  [ -f "$path" ] && [ ! -L "$path" ] || die "path must be a regular non-symlink file: $path"
}

require_directory() {
  local path=$1
  [ -d "$path" ] && [ ! -L "$path" ] || die "path must be a regular non-symlink directory: $path"
}

require_ubuntu_2404() {
  local os_file=/etc/os-release os_canonical os_id os_version major minor
  [ -e "$os_file" ] || die "Ubuntu release metadata is unavailable"
  os_canonical=$(readlink -f -- "$os_file" 2>/dev/null || true)
  [ -n "$os_canonical" ] || die "Ubuntu release metadata cannot be canonicalized"
  require_regular_nonsymlink "$os_canonical"
  [ "$(stat -c '%u:%g' -- "$os_canonical")" = 0:0 ] || die "Ubuntu release metadata is not root-owned"
  [ "$(stat -c '%a' -- "$os_canonical")" = 644 ] || die "Ubuntu release metadata has unexpected mode"
  os_file=$os_canonical
  os_id=$(awk -F= '$1 == "ID" {gsub(/^"|"$/, "", $2); print $2; exit}' "$os_file")
  os_version=$(awk -F= '$1 == "VERSION_ID" {gsub(/^"|"$/, "", $2); print $2; exit}' "$os_file")
  [ "$os_id" = ubuntu ] || die "only Ubuntu 24.04+ is supported (ID=$os_id)"
  printf '%s\n' "$os_version" | grep -Eq '^[0-9]+\.[0-9]+$' || \
    die "Ubuntu VERSION_ID is invalid: $os_version"
  major=${os_version%%.*}
  minor=${os_version#*.}
  if [ "$major" -lt 24 ] || { [ "$major" = 24 ] && [ "$minor" -lt 4 ]; }; then
    die "Ubuntu 24.04+ is required (VERSION_ID=$os_version)"
  fi
}

require_systemd_254() {
  local version
  [ -d /run/systemd/system ] || die "systemd PID 1 is required"
  [ -f /proc/1/comm ] && [ "$(tr -d '[:space:]' </proc/1/comm)" = systemd ] || \
    die "PID 1 is not systemd"
  version=$(systemd-analyze --version | awk 'NR == 1 {print $2; exit}')
  printf '%s\n' "$version" | grep -Eq '^[0-9]+$' || die "cannot determine systemd version"
  [ "$version" -ge 254 ] || die "systemd >= 254 is required (found $version)"
}

require_unified_cgroup2() {
  local canonical fs_type
  require_directory "$cgroup_fs"
  canonical=$(readlink -f -- "$cgroup_fs" 2>/dev/null || true)
  [ "$canonical" = "$cgroup_fs" ] || die "cgroup2 mount must be canonical: $cgroup_fs"
  fs_type=$(stat -fc '%T' "$cgroup_fs" 2>/dev/null || true)
  [ "$fs_type" = cgroup2fs ] || die "unified cgroup2 is required (found ${fs_type:-unknown})"
  [ -f "$cgroup_fs/cgroup.controllers" ] || die "cgroup2 controllers file is missing"
}

require_rootful_docker() {
  local security_options cgroup_driver root_dir context_endpoint context_socket context_canonical docker_status
  [ -z "${DOCKER_HOST:-}" ] || die "DOCKER_HOST must be unset for the local rootful daemon"
  case "${DOCKER_CONTEXT:-default}" in
    ''|default) ;;
    *) die "DOCKER_CONTEXT must be unset or default for the local rootful daemon" ;;
  esac
  if context_endpoint=$(docker context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker default context inspection failed (status $docker_status)"
  fi
  case "$context_endpoint" in
    unix:///*) ;;
    *) die "Docker default context must use a local Unix socket" ;;
  esac
  context_socket=${context_endpoint#unix://}
  [ -S "$context_socket" ] || die "Docker default context socket is unavailable"
  context_canonical=$(readlink -f -- "$context_socket" 2>/dev/null || true)
  [ "$context_canonical" = /run/docker.sock ] || die "Docker default context socket is not the local rootful socket"
  if security_options=$(docker info --format '{{json .SecurityOptions}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker SecurityOptions query failed (status $docker_status)"
  fi
  [ -n "$security_options" ] || die "Docker SecurityOptions query returned no value"
  case "$security_options" in
    *rootless*) die "rootful Docker is required (rootless security option detected)" ;;
  esac
  if cgroup_driver=$(docker info --format '{{.CgroupDriver}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker CgroupDriver query failed (status $docker_status)"
  fi
  [ "$cgroup_driver" = systemd ] || \
    die "Docker Engine must use the systemd cgroup driver (got ${cgroup_driver:-unknown})"
  if root_dir=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker root directory query failed (status $docker_status)"
  fi
  printf '%s\n' "$root_dir" | grep -Eq '^/[^[:space:]]*$' || \
    die "Docker root directory is unavailable; rootful Docker is required"
  if docker_engine_id=$(docker info --format '{{.ID}}' 2>/dev/null); then
    :
  else
    docker_status=$?
    die "Docker Engine ID query failed (status $docker_status)"
  fi
  printf '%s\n' "$docker_engine_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || \
    die "Docker Engine ID is unavailable"
}

read_machine_id() {
  local machine_file=/etc/machine-id
  [ -f "$machine_file" ] && [ ! -L "$machine_file" ] || die "host machine-id is unavailable"
  [ "$(stat -c '%u:%g' -- "$machine_file")" = 0:0 ] || die "host machine-id must be root-owned"
  machine_id=$(tr -d '[:space:]' <"$machine_file")
  printf '%s\n' "$machine_id" | grep -Eq '^[0-9a-f]{32}$' || die "host machine-id is invalid"
}

require_ubuntu_2404
require_systemd_254
require_unified_cgroup2
require_rootful_docker
read_machine_id

ensure_root_directory() {
  local path=$1
  if [ -e "$path" ] || [ -L "$path" ]; then
    require_directory "$path"
    [ "$(stat -c '%u:%g' -- "$path")" = 0:0 ] || die "directory must be root-owned: $path"
    [ "$(stat -c '%a' -- "$path")" = 755 ] || die "directory must be mode 0755: $path"
  else
    install -d -m 0755 -- "$path"
  fi
}

ensure_root_directory "$unit_dir"
ensure_root_directory "$sysusers_dir"

verify_file_metadata() {
  local path=$1
  require_regular_nonsymlink "$path"
  [ "$(stat -c '%u:%g' -- "$path")" = 0:0 ] || die "file must be root-owned: $path"
  [ "$(stat -c '%a' -- "$path")" = 644 ] || die "file must be mode 0644: $path"
}

verify_repository_file() {
  local path=$1
  require_regular_nonsymlink "$path"
  [ "$(stat -c '%a' -- "$path")" = 644 ] || die "repository file must be mode 0644: $path"
}

check_existing_exact_file() {
  local source=$1 target=$2
  verify_repository_file "$source"
  if [ -e "$target" ] || [ -L "$target" ]; then
    verify_file_metadata "$target"
    cmp -s -- "$source" "$target" || die "existing file differs and will not be overwritten: $target"
  fi
}

install_exact_file() {
  local source=$1 target=$2 target_dir tmp
  target_dir=${target%/*}
  verify_repository_file "$source"
  if [ -e "$target" ] || [ -L "$target" ]; then
    verify_file_metadata "$target"
    cmp -s -- "$source" "$target" || die "existing file differs and will not be overwritten: $target"
    return 0
  fi
  tmp=$(mktemp "$target_dir/.runner-prep.XXXXXX")
  cp -- "$source" "$tmp"
  chmod 0644 -- "$tmp"
  chown 0:0 -- "$tmp"
  if [ -e "$target" ] || [ -L "$target" ]; then
    rm -f -- "$tmp"
    verify_file_metadata "$target"
    cmp -s -- "$source" "$target" || die "existing file differs and will not be overwritten: $target"
    return 0
  fi
  if ! ln -- "$tmp" "$target"; then
    rm -f -- "$tmp"
    verify_file_metadata "$target"
    cmp -s -- "$source" "$target" || die "existing file differs and will not be overwritten: $target"
    return 0
  fi
  rm -f -- "$tmp"
  verify_file_metadata "$target"
  cmp -s -- "$source" "$target" || die "installed file differs from repository source: $target"
}

existing_passwd_identity() {
  local user=$1 uid=$2 gid=$3 line account_name actual_uid_name actual_uid actual_gid home shell
  line=$(getent passwd "$user" || true)
  if [ -n "$line" ]; then
    IFS=: read -r account_name _ actual_uid actual_gid _ home shell <<<"$line"
    [ "$account_name" = "$user" ] || die "user lookup returned an unexpected account: $account_name"
    [ "$actual_uid" = "$uid" ] || die "user $user exists with UID $actual_uid, expected $uid"
    [ "$actual_gid" = "$gid" ] || die "user $user exists with GID $actual_gid, expected $gid"
    [ "$home" = /nonexistent ] || die "user $user has unexpected home directory $home"
    [ "$shell" = /usr/sbin/nologin ] || die "user $user has unexpected shell $shell"
  fi
  line=$(getent passwd "$uid" || true)
  if [ -n "$line" ]; then
    IFS=: read -r account_name _ actual_uid actual_gid _ home shell <<<"$line"
    actual_uid_name=$account_name
    [ "$actual_uid_name" = "$user" ] && [ "$actual_uid" = "$uid" ] || \
      die "UID $uid is already assigned to another host user"
  fi
}

existing_group_identity() {
  local group=$1 gid=$2 line group_name actual_group_name actual_gid
  line=$(getent group "$group" || true)
  if [ -n "$line" ]; then
    IFS=: read -r group_name _ actual_gid _ <<<"$line"
    [ "$group_name" = "$group" ] || die "group lookup returned an unexpected group: $group_name"
    [ "$actual_gid" = "$gid" ] || die "group $group exists with GID $actual_gid, expected $gid"
  fi
  line=$(getent group "$gid" || true)
  if [ -n "$line" ]; then
    IFS=: read -r group_name _ actual_gid _ <<<"$line"
    actual_group_name=$group_name
    [ "$actual_group_name" = "$group" ] && [ "$actual_gid" = "$gid" ] || \
      die "GID $gid is already assigned to another host group"
  fi
}

verify_identity() {
  local user=$1 uid=$2 gid=$3 group=$4 line account_name group_name actual_uid actual_gid home shell
  line=$(getent passwd "$user" || true)
  [ -n "$line" ] || die "static user was not created: $user"
  IFS=: read -r account_name _ actual_uid actual_gid _ home shell <<<"$line"
  [ "$account_name" = "$user" ] || die "static user lookup returned an unexpected account: $account_name"
  [ "$actual_uid" = "$uid" ] && [ "$actual_gid" = "$gid" ] || \
    die "static user identity mismatch: $user"
  [ "$home" = /nonexistent ] && [ "$shell" = /usr/sbin/nologin ] || \
    die "static user profile mismatch: $user"
  [ "$(id -u "$user")" = "$uid" ] && [ "$(id -g "$user")" = "$gid" ] || \
    die "static user lookup mismatch: $user"
  line=$(getent group "$group" || true)
  [ -n "$line" ] || die "static group was not created: $group"
  IFS=: read -r group_name _ actual_gid _ <<<"$line"
  [ "$group_name" = "$group" ] || die "static group lookup returned an unexpected group: $group_name"
  [ "$actual_gid" = "$gid" ] || die "static group identity mismatch: $group"
}

runner_identities_exist() {
  getent passwd "$extension_user" >/dev/null 2>&1 &&
    getent passwd "$extension_uid" >/dev/null 2>&1 &&
    getent group "$extension_user" >/dev/null 2>&1 &&
    getent group "$extension_gid" >/dev/null 2>&1 &&
    getent passwd "$core_user" >/dev/null 2>&1 &&
    getent passwd "$core_uid" >/dev/null 2>&1 &&
    getent group "$core_user" >/dev/null 2>&1 &&
    getent group "$core_gid" >/dev/null 2>&1
}

runner_identities_present=false
if runner_identities_exist; then
  runner_identities_present=true
fi

existing_passwd_identity "$extension_user" "$extension_uid" "$extension_gid"
existing_group_identity "$extension_user" "$extension_gid"
existing_passwd_identity "$core_user" "$core_uid" "$core_gid"
existing_group_identity "$core_user" "$core_gid"

# Check every repository-owned target before realizing users or reloading
# systemd. A pre-existing target is accepted only when its metadata and bytes
# are an exact match; no caller-owned file can be replaced.
check_existing_exact_file "$sysusers_source" "$sysusers_dir/$sysusers_file"
check_existing_exact_file "$extension_source" "$unit_dir/$extension_template"
check_existing_exact_file "$core_source" "$unit_dir/$core_template"

# Syntax-check before installing. The actual invocation below is the only
# account-creation mechanism; imperative account-management commands are
# intentionally never used.
systemd-sysusers --dry-run "$sysusers_source" >/dev/null 2>&1 || \
  die "repository sysusers configuration failed systemd-sysusers validation"
install_exact_file "$sysusers_source" "$sysusers_dir/$sysusers_file"
if [ "$runner_identities_present" != true ]; then
  systemd-sysusers "$sysusers_dir/$sysusers_file" >/dev/null 2>&1 || \
    die "systemd-sysusers failed to realize static runner identities"
fi
verify_identity "$extension_user" "$extension_uid" "$extension_gid" "$extension_user"
verify_identity "$core_user" "$core_uid" "$core_gid" "$core_user"

install_exact_file "$extension_source" "$unit_dir/$extension_template"
install_exact_file "$core_source" "$unit_dir/$core_template"
systemctl daemon-reload >/dev/null 2>&1 || die "systemd daemon-reload failed"

unit_property() {
  local unit=$1 property=$2 value
  value=$(systemctl show "$unit" --property="$property" --value 2>/dev/null || true)
  [ -n "$value" ] || die "$unit has no $property property"
  printf '%s' "$value"
}

verify_unit_definition() {
  local unit=$1 template=$2 user=$3 parent=$4 delegate delegate_controllers controller
  local subgroup fragment_path dropins
  local -a controller_items
  fragment_path=$(unit_property "$unit" FragmentPath)
  [ "$fragment_path" = "$unit_dir/$template" ] || \
    die "$unit FragmentPath is not the repository-owned template: $unit"
  [ "$(grep -c '^ExecStart=' -- "$fragment_path")" -eq 1 ] || die "$unit must have exactly one ExecStart"
  grep -Fxq 'ExecStart=/usr/bin/sleep infinity' -- "$fragment_path" || die "$unit ExecStart is not fixed sleep infinity"
  dropins=$(systemctl show "$unit" --property=DropInPaths --value 2>/dev/null || true)
  [ -z "$dropins" ] || die "$unit has unsupported drop-in paths"
  [ "$(unit_property "$unit" User)" = "$user" ] || die "$unit User property is not fixed"
  [ "$(unit_property "$unit" Group)" = "$user" ] || die "$unit Group property is not fixed"
  [ "$(unit_property "$unit" Slice)" = "$parent" ] || die "$unit Slice property is not stack-bound"
  delegate=$(unit_property "$unit" Delegate)
  [ "$delegate" = yes ] || die "$unit Delegate property is not enabled"
  delegate_controllers=$(unit_property "$unit" DelegateControllers)
  read -r -a controller_items <<<"$delegate_controllers"
  [ "${#controller_items[@]}" -eq 3 ] || \
    die "$unit DelegateControllers must contain exactly cpu memory pids"
  for controller in "${controller_items[@]}"; do
    case "$controller" in
      cpu|memory|pids) ;;
      *) die "$unit DelegateControllers contains unsupported controller $controller" ;;
    esac
  done
  for controller in cpu memory pids; do
    printf ' %s ' "$delegate_controllers" | grep -Fq " $controller " || \
      die "$unit DelegateControllers is missing $controller"
  done
  subgroup=$(unit_property "$unit" DelegateSubgroup)
  [ "$subgroup" = keeper ] || die "$unit DelegateSubgroup property is not keeper"
  [ "$(unit_property "$unit" LoadState)" = loaded ] || die "$unit is not loaded"
}

verify_enabled() {
  local unit=$1 state
  state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  [ "$state" = enabled ] || die "$unit is not persistently enabled (state=${state:-unknown})"
}

wait_unit_active() {
  local unit=$1 attempt=0 state substate
  while [ "$attempt" -lt 30 ]; do
    attempt=$((attempt + 1))
    state=$(systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)
    substate=$(systemctl show "$unit" --property=SubState --value 2>/dev/null || true)
    [ "$state" = active ] && [ "$substate" = running ] && return 0
    [ "$state" = failed ] && die "$unit entered failed state"
    sleep 1
  done
  die "$unit did not become active"
}

unit_control_group() {
  local unit=$1 value
  value=$(unit_property "$unit" ControlGroup)
  printf '%s\n' "$value" | grep -Eq '^/[^/[:space:]][^[:space:]]*$' || \
    die "$unit ControlGroup is not an absolute cgroup path"
  case "$value" in
    *'//'|*'/../'*|*'/./'*) die "$unit ControlGroup contains non-canonical path components" ;;
  esac
  printf '%s' "$value"
}

write_subtree_controllers() {
  local uid=$1 gid=$2 target=$3
  if command -v setpriv >/dev/null 2>&1; then
    printf '%s\n' '+cpu +memory +pids' | \
      setpriv --reuid="$uid" --regid="$gid" --clear-groups \
        --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
        -- /usr/bin/tee "$target" >/dev/null
  else
    printf '%s\n' '+cpu +memory +pids' | \
      runuser -u "#$uid" -g "#$gid" -- /usr/bin/tee "$target" >/dev/null
  fi
}

require_control_group_identity() {
  local role=$1 unit=$2 parent=$3 control_group=$4
  case "$control_group" in
    *"/$parent/$unit"|*"/$parent/$unit/"*) ;;
    *) die "$role ControlGroup is not bound to exact stack parent/unit: $control_group" ;;
  esac
  [ "$control_group" != / ] || die "$role ControlGroup cannot be the cgroup root"
}

require_empty_cgroup_procs() {
  local label=$1 path=$2 contents read_status
  if contents=$(tr -d '[:space:]' <"$path" 2>/dev/null); then
    :
  else
    read_status=$?
    die "$label process control read failed (status $read_status)"
  fi
  [ -z "$contents" ] || die "$label has an unexpected direct process"
}

prepare_root() {
  local role=$1 uid=$2 gid=$3 unit=$4 parent=$5 control_group=$6 main_pid=$7 root canonical fs_type owner controllers subtree required keeper_owner
  root=$cgroup_fs$control_group
  [ -d "$root" ] && [ ! -L "$root" ] || die "$role delegated root is not a directory: $root"
  canonical=$(readlink -f -- "$root" 2>/dev/null || true)
  [ "$canonical" = "$root" ] || die "$role delegated root is not canonical: $root"
  fs_type=$(stat -fc '%T' "$root" 2>/dev/null || true)
  [ "$fs_type" = cgroup2fs ] || die "$role delegated root is not cgroup-v2: $root"
  owner=$(stat -c '%u:%g' -- "$root" 2>/dev/null || true)
  [ "$owner" = "$uid:$gid" ] || die "$role delegated root owner is $owner, expected $uid:$gid"
  [ -f "$root/cgroup.subtree_control" ] && [ -r "$root/cgroup.subtree_control" ] || \
    die "$role subtree control is missing or unreadable: $root"
  [ -f "$root/cgroup.procs" ] && [ -r "$root/cgroup.procs" ] || \
    die "$role process control is missing or unreadable: $root"
  controllers=$(tr '\n' ' ' <"$root/cgroup.controllers" 2>/dev/null || true)
  [ -n "$controllers" ] || die "$role delegated root has no delegated controllers"
  for required in cpu memory pids; do
    printf ' %s ' "$controllers" | grep -Fq " $required " || \
      die "$role delegated root does not expose controller $required"
  done
  [ -d "$root/keeper" ] && [ ! -L "$root/keeper" ] || \
    die "$role DelegateSubgroup=keeper is missing: $root/keeper"
  keeper_owner=$(stat -c '%u:%g' -- "$root/keeper" 2>/dev/null || true)
  [ "$keeper_owner" = "$uid:$gid" ] || die "$role keeper owner is $keeper_owner, expected $uid:$gid"
  require_empty_cgroup_procs "$role delegated root" "$root/cgroup.procs"
  printf '%s\n' "$main_pid" | grep -Eq '^[1-9][0-9]*$' || die "$role MainPID is invalid"
  grep -Fxq "$main_pid" "$root/keeper/cgroup.procs" || die "$role MainPID is not held in keeper subgroup"
  probe_runner_write() {
    local probe_path=$1
    if command -v setpriv >/dev/null 2>&1; then
      setpriv --reuid="$uid" --regid="$gid" --clear-groups \
        --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
        -- /usr/bin/test -w "$probe_path" >/dev/null 2>&1
    else
      runuser -u "#$uid" -g "#$gid" -- /usr/bin/test -w "$probe_path" >/dev/null 2>&1
    fi
  }
  probe_runner_write "$root" || die "$role delegated root is not writable by runner UID/GID $uid:$gid"
  probe_runner_write "$root/cgroup.subtree_control" || die "$role subtree control is not writable by runner UID/GID $uid:$gid"
  probe_runner_write "$root/cgroup.procs" || die "$role process control is not writable by runner UID/GID $uid:$gid"
  write_subtree_controllers "$uid" "$gid" "$root/cgroup.subtree_control" || \
    die "$role runner identity could not enable delegated controllers"
  subtree=$(tr '\n' ' ' <"$root/cgroup.subtree_control" 2>/dev/null || true)
  for required in cpu memory pids; do
    printf ' %s ' "$subtree" | grep -Fq " $required " || \
      die "$role delegated root did not enable controller $required in subtree_control"
  done
  require_control_group_identity "$role" "$unit" "$parent" "$control_group"
  printf '%s' "$root"
}

prepare_parent_process_control() {
  local role=$1 uid=$2 gid=$3 parent=$4 control_group=$5
  local parent_group parent_root parent_procs canonical fs_type owner mode
  parent_group=${control_group%/*}
  [ "$parent_group" != "$control_group" ] && [ "$parent_group" != / ] || \
    die "$role parent ControlGroup cannot be derived safely"
  case "$parent_group" in
    *"/$parent") ;;
    *) die "$role parent ControlGroup is not the exact stack slice: $parent_group" ;;
  esac
  parent_root=$cgroup_fs$parent_group
  [ -d "$parent_root" ] && [ ! -L "$parent_root" ] || \
    die "$role parent slice is not a directory: $parent_root"
  canonical=$(readlink -f -- "$parent_root" 2>/dev/null || true)
  [ "$canonical" = "$parent_root" ] || die "$role parent slice is not canonical: $parent_root"
  fs_type=$(stat -fc '%T' "$parent_root" 2>/dev/null || true)
  [ "$fs_type" = cgroup2fs ] || die "$role parent slice is not cgroup-v2: $parent_root"
  [ "$(stat -c '%u:%g' -- "$parent_root")" = 0:0 ] || \
    die "$role parent slice directory must remain root-owned"
  parent_procs=$parent_root/cgroup.procs
  [ -f "$parent_procs" ] && [ ! -L "$parent_procs" ] || \
    die "$role parent process control is missing: $parent_procs"
  owner=$(stat -c '%u:%g' -- "$parent_procs" 2>/dev/null || true)
  case "$owner" in
    0:0|"$uid:$gid") ;;
    *) die "$role parent process control has unexpected owner $owner" ;;
  esac
  mode=$(stat -c '%a' -- "$parent_procs" 2>/dev/null || true)
  [ "$mode" = 644 ] || die "$role parent process control has unexpected mode $mode"
  chown "$uid:$gid" -- "$parent_procs" || die "$role parent process control ownership update failed"
  chmod 0644 -- "$parent_procs" || die "$role parent process control mode update failed"
  [ "$(stat -c '%u:%g' -- "$parent_procs")" = "$uid:$gid" ] || \
    die "$role parent process control owner postcondition failed"
  [ "$(stat -c '%a' -- "$parent_procs")" = 644 ] || \
    die "$role parent process control mode postcondition failed"
  printf '%s\n%s' "$parent_root" "$parent_procs"
}

# Refuse to touch a same-name service until its exact, repository-owned
# definition has been loaded and verified.  In particular, this helper never
# stops, resets, disables, or removes a pre-existing instance.
systemctl enable "$extension_unit" "$core_unit" >/dev/null 2>&1 || \
  die "failed to persistently enable runner template instances"
verify_enabled "$extension_unit"
verify_enabled "$core_unit"
verify_unit_definition "$extension_unit" "$extension_template" "$extension_user" "$extension_parent"
verify_unit_definition "$core_unit" "$core_template" "$core_user" "$core_parent"

systemctl start "$extension_unit" >/dev/null 2>&1 || die "failed to start $extension_unit"
wait_unit_active "$extension_unit"
verify_unit_definition "$extension_unit" "$extension_template" "$extension_user" "$extension_parent"
extension_control_group=$(unit_control_group "$extension_unit")
require_control_group_identity extension "$extension_unit" "$extension_parent" "$extension_control_group"
extension_main_pid=$(unit_property "$extension_unit" MainPID)
extension_parent_output=$(prepare_parent_process_control extension "$extension_uid" "$extension_gid" "$extension_parent" "$extension_control_group")
mapfile -t extension_parent_metadata <<<"$extension_parent_output"
extension_parent_root=${extension_parent_metadata[0]}
extension_parent_procs=${extension_parent_metadata[1]}
extension_root=$(prepare_root extension "$extension_uid" "$extension_gid" "$extension_unit" "$extension_parent" "$extension_control_group" "$extension_main_pid")

systemctl start "$core_unit" >/dev/null 2>&1 || die "failed to start $core_unit"
wait_unit_active "$core_unit"
verify_unit_definition "$core_unit" "$core_template" "$core_user" "$core_parent"
core_control_group=$(unit_control_group "$core_unit")
require_control_group_identity core "$core_unit" "$core_parent" "$core_control_group"
core_main_pid=$(unit_property "$core_unit" MainPID)
core_parent_output=$(prepare_parent_process_control core "$core_uid" "$core_gid" "$core_parent" "$core_control_group")
mapfile -t core_parent_metadata <<<"$core_parent_output"
core_parent_root=${core_parent_metadata[0]}
core_parent_procs=${core_parent_metadata[1]}
core_root=$(prepare_root core "$core_uid" "$core_gid" "$core_unit" "$core_parent" "$core_control_group" "$core_main_pid")

# The success channel is machine-readable by design: no status text, unit
# output, or command diagnostics are emitted here.
print_env "$extension_root" "$core_root" "$extension_control_group" \
  "$core_control_group" "$extension_hash" "$core_hash"
