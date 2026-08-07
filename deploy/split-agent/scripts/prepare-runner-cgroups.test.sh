#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/prepare-runner-cgroups.sh
systemd_dir=$script_dir/../systemd
sysusers_file=$script_dir/../sysusers.d/dirextalk-split-agent.conf
[ -x "$script" ] || { echo "prepare-runner-cgroups.sh must be executable" >&2; exit 1; }
[ -f "$sysusers_file" ] || { echo "sysusers fixture is missing" >&2; exit 1; }
bash -n "$script"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x "$script"
fi

stack=d-abcdefghijklmnopqrstuvwxyz
output=$(bash "$script" --dry-run "$stack")

# Dry-run is the host-free fixture path. Its stdout is consumed as a literal
# env file, so comments, status text, and command diagnostics are forbidden.
printf '%s\n' "$output" | awk 'NF && $0 !~ /^[A-Z0-9_]+=[^[:space:]]+$/ {exit 1}'

extension_hash=$(sha256sum -- "$systemd_dir/dirextalk-extension-runner@.service" | awk '{print $1}')
core_hash=$(sha256sum -- "$systemd_dir/dirextalk-core-runner@.service" | awk '{print $1}')
apparmor_hash=$(sha256sum -- "$script_dir/../apparmor.d/dirextalk-runner-userns" | awk '{print $1}')
apparmor_manager_hash=$(sha256sum -- "$script_dir/manage-runner-apparmor.sh" | awk '{print $1}')
grep -Fqx 'DIREXTALK_EXTENSION_CGROUP_ROOT=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CGROUP_ROOT=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_EXTENSION_CONTROL_GROUP=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CONTROL_GROUP=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER=65531:65531' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER=65530:65530' <<<"$output"
grep -Fqx 'DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE=644' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE=644' <<<"$output"
grep -Fqx "DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256=$extension_hash" <<<"$output"
grep -Fqx "DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256=$core_hash" <<<"$output"
grep -Fqx 'DIREXTALK_RUNNER_APPARMOR_PROFILE=dirextalk-runner-userns' <<<"$output"
grep -Fqx 'DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH=/etc/apparmor.d/dirextalk-runner-userns' <<<"$output"
grep -Fqx "DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256=$apparmor_hash" <<<"$output"
grep -Fqx "DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256=$apparmor_manager_hash" <<<"$output"
grep -Fqx 'DIREXTALK_RUNNER_PREP_MACHINE_ID=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID=unknown' <<<"$output"
grep -Fqx "DIREXTALK_CORE_EXTENSION_RUNNER_UID=65531" <<<"$output"
grep -Fqx "DIREXTALK_CORE_WORKLOAD_RUNNER_UID=65530" <<<"$output"

for invalid_stack in \
  d-abcdefghijklmnopqrstuvwxyz0 \
  d-aaaaaaaaaaaaaaaaaaaaaaaaa \
  d-aaaaaaaaaaaaaaaaaaaaaaaaaaa \
  D-abcdefghijklmnopqrstuvwxyz \
  d-abcdefghijklmnopqrstuvwxyz-; do
  if bash "$script" --dry-run "$invalid_stack" >/dev/null 2>&1; then
    echo "invalid stack identity was accepted: $invalid_stack" >&2
    exit 1
  fi
done

# These are repository fixtures, not host-installed units. systemd-analyze
# must accept the exact production unit syntax before a release can proceed.
if command -v systemd-analyze >/dev/null 2>&1; then
  systemd-analyze verify \
    "$systemd_dir/dirextalk-extension-runner@.service" \
    "$systemd_dir/dirextalk-core-runner@.service"
fi

grep -Fqx 'g dirextalk-extension-runner 65531' "$sysusers_file"
grep -Fqx 'u dirextalk-extension-runner 65531:65531 "Dirextalk Extension Runner" /nonexistent' "$sysusers_file"
grep -Fqx 'g dirextalk-core-runner 65530' "$sysusers_file"
grep -Fqx 'u dirextalk-core-runner 65530:65530 "Dirextalk Core Runner" /nonexistent' "$sysusers_file"

if grep -Fq 'useradd' "$script" || grep -Fq 'systemd-run --user' "$script"; then
  echo "dynamic user creation/user-systemd delegation is forbidden" >&2
  exit 1
fi
if grep -Eq 'systemctl[[:space:]]+stop|systemctl[[:space:]]+disable|rm[[:space:]].*(/etc/systemd|/etc/sysusers)' "$script"; then
  echo "same-name unit stop/disable or host deletion is forbidden" >&2
  exit 1
fi
if grep -Fq -- "[ ! -s \"\$root/cgroup.procs\" ]" "$script"; then
  echo "prepare-runner-cgroups.sh must read cgroupfs process contents instead of using stat size" >&2
  exit 1
fi
grep -Fq -- "require_empty_cgroup_procs \"\$role delegated root\"" "$script"
if grep -Fq -- 'cgroup.procs" 2>/dev/null || true' "$script"; then
  echo "prepare-runner-cgroups.sh must fail closed when cgroup.procs cannot be read" >&2
  exit 1
fi
if [ "$(stat -fc '%T' /sys/fs/cgroup 2>/dev/null || true)" = cgroup2fs ] &&
   [ -f /sys/fs/cgroup/cgroup.procs ] &&
   [ -r /sys/fs/cgroup/cgroup.procs ]; then
  [ ! -s /sys/fs/cgroup/cgroup.procs ]
  live_processes=$(tr -d '[:space:]' </sys/fs/cgroup/cgroup.procs)
  [ -n "$live_processes" ]
fi
grep -Fq -- "setpriv --reuid=\"\$uid\"" "$script"
grep -Fq -- "runuser -u \"#\$uid\"" "$script"

# The Docker preflight must distinguish a failed info query from a valid
# negative value. Exercise the extracted function with a fake local context:
# SecurityOptions and Engine ID failures are both hard failures, while a
# successful SecurityOptions response without a rootless marker is accepted.
if grep -Fq -- "docker info --format '{{.Rootless}}'" "$script"; then
  echo "prepare-runner-cgroups.sh must not depend on the optional Rootless info field" >&2
  exit 1
fi
runner_test_tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-runner-prep-test.XXXXXX")
runner_test_cleanup() { rm -rf -- "$runner_test_tmp"; }
trap runner_test_cleanup EXIT

cat >"$runner_test_tmp/cgroup-procs-function.sh" <<'EOF'
die() {
  printf '%s\n' "$*" >&2
  exit 1
}
EOF
sed -n '/^require_empty_cgroup_procs() {/,/^}$/p' "$script" >>"$runner_test_tmp/cgroup-procs-function.sh"
: >"$runner_test_tmp/empty-cgroup.procs"
printf '456\n' >"$runner_test_tmp/nonempty-cgroup.procs"
bash -c 'source "$1"; require_empty_cgroup_procs fixture "$2"' \
  _ "$runner_test_tmp/cgroup-procs-function.sh" "$runner_test_tmp/empty-cgroup.procs"
if bash -c 'source "$1"; require_empty_cgroup_procs fixture "$2"' \
  _ "$runner_test_tmp/cgroup-procs-function.sh" "$runner_test_tmp/nonempty-cgroup.procs" \
  >"$runner_test_tmp/nonempty-procs.stdout" 2>"$runner_test_tmp/nonempty-procs.stderr"; then
  echo "nonempty delegated cgroup.procs was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'unexpected direct process' "$runner_test_tmp/nonempty-procs.stderr"
if bash -c 'source "$1"; require_empty_cgroup_procs fixture "$2"' \
  _ "$runner_test_tmp/cgroup-procs-function.sh" "$runner_test_tmp/missing-cgroup.procs" \
  >"$runner_test_tmp/missing-procs.stdout" 2>"$runner_test_tmp/missing-procs.stderr"; then
  echo "missing delegated cgroup.procs was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'process control read failed' "$runner_test_tmp/missing-procs.stderr"
cat >"$runner_test_tmp/prepare-function.sh" <<'EOF'
die() {
  printf '%s\n' "$*" >&2
  exit 1
}
EOF
sed -n '/^require_rootful_docker() {/,/^}$/p' "$script" >>"$runner_test_tmp/prepare-function.sh"
cat >"$runner_test_tmp/bin-docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = context ]; then
  printf 'unix:///run/docker.sock\n'
  exit 0
fi
if [ "$1" = info ]; then
  format=${3:-}
  case "${DIREXTALK_FAKE_DOCKER_INFO_FAILURE:-}" in
    security) [[ "$format" == *SecurityOptions* ]] && exit 42 ;;
    engine) [[ "$format" == *'{{.ID}}'* ]] && exit 43 ;;
  esac
  case "$format" in
    *SecurityOptions*) printf '["name=seccomp"]\n' ;;
    *CgroupDriver*) printf 'systemd\n' ;;
    *DockerRootDir*) printf '/var/lib/docker\n' ;;
    *'{{.ID}}'*) printf 'engine-test\n' ;;
    *) exit 44 ;;
  esac
  exit 0
fi
exit 45
EOF
chmod 755 "$runner_test_tmp/bin-docker"
runner_test_path="$runner_test_tmp/bin:$PATH"
mkdir -p "$runner_test_tmp/bin"
mv -- "$runner_test_tmp/bin-docker" "$runner_test_tmp/bin/docker"
for failure_case in security engine; do
  if PATH="$runner_test_path" DIREXTALK_FAKE_DOCKER_INFO_FAILURE="$failure_case" \
    bash -c 'source "$1"; require_rootful_docker' _ "$runner_test_tmp/prepare-function.sh" \
    >/dev/null 2>"$runner_test_tmp/$failure_case.stderr"; then
    echo "Docker info $failure_case failure was unexpectedly accepted" >&2
    exit 1
  fi
  grep -Fq "query failed" "$runner_test_tmp/$failure_case.stderr"
done
PATH="$runner_test_path" bash -c 'source "$1"; require_rootful_docker' _ "$runner_test_tmp/prepare-function.sh"

# Exercise the real passwd/group parsers with canonical NSS records. The
# password placeholder is the second field, so accepting it as UID/GID would
# reject a correctly realized systemd-sysusers identity on the first host run.
cat >"$runner_test_tmp/identity-functions.sh" <<'EOF'
die() {
  printf '%s\n' "$*" >&2
  exit 1
}
EOF
{
  sed -n '/^existing_passwd_identity() {/,/^}$/p' "$script"
  sed -n '/^existing_group_identity() {/,/^}$/p' "$script"
  sed -n '/^verify_identity() {/,/^}$/p' "$script"
  sed -n '/^runner_identities_exist() {/,/^}$/p' "$script"
} >>"$runner_test_tmp/identity-functions.sh"
cat >"$runner_test_tmp/bin/getent" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
kind=$1
key=$2
if [ "${DIREXTALK_FAKE_IDENTITY_MISSING:-false}" = true ] && [ "$kind:$key" = passwd:65530 ]; then
  exit 2
fi
if [ "${DIREXTALK_FAKE_IDENTITY_COLLISION:-false}" = true ] && [ "$kind:$key" = passwd:65531 ]; then
  printf 'another-runner:x:65531:65531:Unexpected:/nonexistent:/usr/sbin/nologin\n'
  exit 0
fi
case "$kind:$key" in
  passwd:dirextalk-extension-runner|passwd:65531)
    printf 'dirextalk-extension-runner:x:65531:65531:Dirextalk Extension Runner:/nonexistent:/usr/sbin/nologin\n'
    ;;
  group:dirextalk-extension-runner|group:65531)
    printf 'dirextalk-extension-runner:x:65531:\n'
    ;;
  passwd:dirextalk-core-runner|passwd:65530)
    printf 'dirextalk-core-runner:x:65530:65530:Dirextalk Core Runner:/nonexistent:/usr/sbin/nologin\n'
    ;;
  group:dirextalk-core-runner|group:65530)
    printf 'dirextalk-core-runner:x:65530:\n'
    ;;
  *) exit 2 ;;
esac
EOF
cat >"$runner_test_tmp/bin/id" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1:$2" in
  -u:dirextalk-extension-runner|-g:dirextalk-extension-runner) printf '65531\n' ;;
  -u:dirextalk-core-runner|-g:dirextalk-core-runner) printf '65530\n' ;;
  *) exit 2 ;;
esac
EOF
chmod 755 "$runner_test_tmp/bin/getent" "$runner_test_tmp/bin/id"
PATH="$runner_test_path" bash -c '
  source "$1"
  extension_user=dirextalk-extension-runner; extension_uid=65531; extension_gid=65531
  core_user=dirextalk-core-runner; core_uid=65530; core_gid=65530
  runner_identities_exist
  existing_passwd_identity dirextalk-extension-runner 65531 65531
  existing_group_identity dirextalk-extension-runner 65531
  verify_identity dirextalk-extension-runner 65531 65531 dirextalk-extension-runner
' _ "$runner_test_tmp/identity-functions.sh"
if PATH="$runner_test_path" DIREXTALK_FAKE_IDENTITY_MISSING=true bash -c '
  source "$1"
  extension_user=dirextalk-extension-runner; extension_uid=65531; extension_gid=65531
  core_user=dirextalk-core-runner; core_uid=65530; core_gid=65530
  runner_identities_exist
' _ "$runner_test_tmp/identity-functions.sh"; then
  echo "missing runner identity was unexpectedly reported as complete" >&2
  exit 1
fi
if PATH="$runner_test_path" DIREXTALK_FAKE_IDENTITY_COLLISION=true bash -c '
  source "$1"
  existing_passwd_identity dirextalk-extension-runner 65531 65531
' _ "$runner_test_tmp/identity-functions.sh" >"$runner_test_tmp/identity-collision.stdout" 2>"$runner_test_tmp/identity-collision.stderr"; then
  echo "numeric UID collision was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'UID 65531 is already assigned to another host user' "$runner_test_tmp/identity-collision.stderr"

# systemd exposes Delegate= as a boolean plus a separate controller set in
# `systemctl show`; it does not echo the unit-file token list through the
# Delegate property. Accept any ordering of the exact required set and reject
# missing controllers or a disabled boolean.
cat >"$runner_test_tmp/unit-functions.sh" <<'EOF'
die() {
  printf '%s\n' "$*" >&2
  exit 1
}
EOF
sed -n '/^unit_property() {/,/^}$/p' "$script" >>"$runner_test_tmp/unit-functions.sh"
sed -n '/^verify_unit_definition() {/,/^}$/p' "$script" >>"$runner_test_tmp/unit-functions.sh"
cat >"$runner_test_tmp/unit-template.service" <<'EOF'
[Service]
ExecStart=/usr/bin/sleep infinity
EOF
cat >"$runner_test_tmp/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = show ]
property=''
for argument in "$@"; do
  case "$argument" in --property=*) property=${argument#--property=} ;; esac
done
case "$property" in
  FragmentPath) printf '%s/unit-template.service\n' "$DIREXTALK_FAKE_UNIT_DIR" ;;
  DropInPaths) ;;
  User|Group) printf 'dirextalk-extension-runner\n' ;;
  Slice) printf 'd-abcdefghijklmnopqrstuvwxyz-extension.slice\n' ;;
  Delegate) printf '%s\n' "${DIREXTALK_FAKE_DELEGATE:-yes}" ;;
  DelegateControllers) printf '%s\n' "${DIREXTALK_FAKE_DELEGATE_CONTROLLERS:-pids cpu memory}" ;;
  DelegateSubgroup) printf 'keeper\n' ;;
  LoadState) printf 'loaded\n' ;;
  *) exit 2 ;;
esac
EOF
chmod 755 "$runner_test_tmp/bin/systemctl"
PATH="$runner_test_path" DIREXTALK_FAKE_UNIT_DIR="$runner_test_tmp" bash -c '
  source "$1"
  unit_dir=$2
  verify_unit_definition \
    dirextalk-extension-runner@d-abcdefghijklmnopqrstuvwxyz.service \
    unit-template.service dirextalk-extension-runner \
    d-abcdefghijklmnopqrstuvwxyz-extension.slice
' _ "$runner_test_tmp/unit-functions.sh" "$runner_test_tmp"
for invalid_delegate_case in disabled missing; do
  delegate=yes
  controllers='pids cpu memory'
  case "$invalid_delegate_case" in
    disabled) delegate=no ;;
    missing) controllers='cpu memory' ;;
  esac
  if PATH="$runner_test_path" DIREXTALK_FAKE_UNIT_DIR="$runner_test_tmp" \
    DIREXTALK_FAKE_DELEGATE="$delegate" DIREXTALK_FAKE_DELEGATE_CONTROLLERS="$controllers" \
    bash -c '
      source "$1"
      unit_dir=$2
      verify_unit_definition \
        dirextalk-extension-runner@d-abcdefghijklmnopqrstuvwxyz.service \
        unit-template.service dirextalk-extension-runner \
        d-abcdefghijklmnopqrstuvwxyz-extension.slice
    ' _ "$runner_test_tmp/unit-functions.sh" "$runner_test_tmp" \
    >"$runner_test_tmp/delegate-$invalid_delegate_case.stdout" \
    2>"$runner_test_tmp/delegate-$invalid_delegate_case.stderr"; then
    echo "invalid Delegate case was unexpectedly accepted: $invalid_delegate_case" >&2
    exit 1
  fi
done
grep -Fq 'Delegate property is not enabled' "$runner_test_tmp/delegate-disabled.stderr"
grep -Fq 'DelegateControllers must contain exactly cpu memory pids' "$runner_test_tmp/delegate-missing.stderr"

# A delegated unit exposes controllers but leaves subtree_control for the
# delegate owner to enable. Exercise the exact runner-identity write command
# and require its infrastructure failure to remain non-zero.
sed -n '/^write_subtree_controllers() {/,/^}$/p' "$script" >"$runner_test_tmp/controller-function.sh"
cat >"$runner_test_tmp/bin/setpriv" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${DIREXTALK_FAKE_SETPRIV_FAILURE:-false}" != true ] || exit 42
while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
[ "${1:-}" = -- ]
shift
exec "$@"
EOF
chmod 755 "$runner_test_tmp/bin/setpriv"
controller_target=$runner_test_tmp/cgroup.subtree_control
: >"$controller_target"
PATH="$runner_test_path" bash -c '
  set -o pipefail
  source "$1"
  write_subtree_controllers 65531 65531 "$2"
' _ "$runner_test_tmp/controller-function.sh" "$controller_target"
grep -Fxq '+cpu +memory +pids' "$controller_target"
if PATH="$runner_test_path" DIREXTALK_FAKE_SETPRIV_FAILURE=true bash -c '
  set -o pipefail
  source "$1"
  write_subtree_controllers 65531 65531 "$2"
' _ "$runner_test_tmp/controller-function.sh" "$controller_target" \
  >"$runner_test_tmp/controller-failure.stdout" 2>"$runner_test_tmp/controller-failure.stderr"; then
  echo "runner-identity controller write failure was unexpectedly accepted" >&2
  exit 1
fi

echo "prepare-runner-cgroups fixture tests passed"
