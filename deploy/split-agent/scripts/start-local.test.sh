#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/start-local.sh
[ -x "$script" ] || { echo "start-local.sh must be executable" >&2; exit 1; }
bash -n "$script"
if grep -Fq -- "[ -w \"\$value/cgroup.subtree_control\" ]" "$script" || \
   grep -Fq -- "[ -w \"\$value/cgroup.procs\" ]" "$script"; then
  echo "start-local.sh must not test cgroup writability as the current user" >&2
  exit 1
fi
if grep -Fq -- "[ -s \"\$value/cgroup.controllers\" ]" "$script"; then
  echo "start-local.sh must read cgroupfs controller contents instead of using stat size" >&2
  exit 1
fi
if grep -Fq -- "[ ! -s \"\$root/cgroup.procs\" ]" "$script"; then
  echo "start-local.sh must read cgroupfs process contents instead of using stat size" >&2
  exit 1
fi
if grep -Fq -- "[ \"\$(runner_unit_property \"\$unit\" Delegate)\" = 'cpu memory pids' ]" "$script"; then
  echo "start-local.sh must validate Delegate plus DelegateControllers separately" >&2
  exit 1
fi
grep -Fq -- 'DelegateControllers' "$script"
grep -Fq -- "validate_runner_delegate \"\$role\" \"\$unit\"" "$script"
grep -Fq -- "require_empty_cgroup_procs \"\$role runner delegated root\"" "$script"
if grep -Fq -- 'cgroup.procs" 2>/dev/null || true' "$script"; then
  echo "start-local.sh must fail closed when cgroup.procs cannot be read" >&2
  exit 1
fi
grep -Fq -- "[ -n \"\$controllers\" ]" "$script"
grep -Fq -- 'validate_target_write_access' "$script"
grep -Fq -- 'getfacl -cp' "$script"

# Keep startup bound to cgroupfs contents, whose virtual stat size is zero.
if [ "$(stat -fc '%T' /sys/fs/cgroup 2>/dev/null || true)" = cgroup2fs ] &&
   [ -f /sys/fs/cgroup/cgroup.controllers ] &&
   [ -r /sys/fs/cgroup/cgroup.controllers ]; then
  [ ! -s /sys/fs/cgroup/cgroup.controllers ]
  live_controllers=$(tr '\n' ' ' </sys/fs/cgroup/cgroup.controllers)
  [ -n "$live_controllers" ]
fi
if [ "$(stat -fc '%T' /sys/fs/cgroup 2>/dev/null || true)" = cgroup2fs ] &&
   [ -f /sys/fs/cgroup/cgroup.procs ] &&
   [ -r /sys/fs/cgroup/cgroup.procs ]; then
  [ ! -s /sys/fs/cgroup/cgroup.procs ]
  live_processes=$(tr -d '[:space:]' </sys/fs/cgroup/cgroup.procs)
  [ -n "$live_processes" ]
fi
grep -Fq -- 'DIREXTALK_SPLIT_COMPOSE_MODE' "$script"
grep -Fq -- 'verify-production-tls.sh' "$script"
grep -Fq -- 'verify-production-images.sh' "$script"
grep -Fq -- 'pull --policy always' "$script"
grep -Fq -- 'up -d --no-build --pull never --wait message-server' "$script"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-start-local.XXXXXX")
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT
mkdir -p "$tmp_dir/bin"
docker_log=$tmp_dir/docker.log
docker_state=$tmp_dir/up.state

cat >"$tmp_dir/runner-binding-functions.sh" <<'EOF'
die() {
  printf '%s\n' "$*" >&2
  exit 1
}
runner_unit_property() {
  case "$2" in
    Delegate) printf '%s\n' "${DIREXTALK_FAKE_DELEGATE:-yes}" ;;
    DelegateControllers) printf '%s\n' "${DIREXTALK_FAKE_DELEGATE_CONTROLLERS:-pids cpu memory}" ;;
    *) exit 2 ;;
  esac
}
EOF
sed -n '/^validate_runner_delegate() {/,/^}$/p' "$script" >>"$tmp_dir/runner-binding-functions.sh"
sed -n '/^require_empty_cgroup_procs() {/,/^}$/p' "$script" >>"$tmp_dir/runner-binding-functions.sh"

bash -c 'source "$1"; validate_runner_delegate extension fixture.service' \
  _ "$tmp_dir/runner-binding-functions.sh"
for delegate_case in disabled old-shape missing extra duplicate; do
  delegate=yes
  controllers='pids cpu memory'
  case "$delegate_case" in
    disabled) delegate=no ;;
    old-shape) delegate='cpu memory pids' ;;
    missing) controllers='cpu memory' ;;
    extra) controllers='cpu memory pids io' ;;
    duplicate) controllers='cpu memory memory' ;;
  esac
  if DIREXTALK_FAKE_DELEGATE="$delegate" DIREXTALK_FAKE_DELEGATE_CONTROLLERS="$controllers" \
    bash -c 'source "$1"; validate_runner_delegate extension fixture.service' \
      _ "$tmp_dir/runner-binding-functions.sh" \
      >"$tmp_dir/delegate-$delegate_case.stdout" 2>"$tmp_dir/delegate-$delegate_case.stderr"; then
    echo "invalid start-local Delegate case was unexpectedly accepted: $delegate_case" >&2
    exit 1
  fi
  grep -Fq 'Delegate' "$tmp_dir/delegate-$delegate_case.stderr"
done

: >"$tmp_dir/empty-cgroup.procs"
printf '123\n' >"$tmp_dir/nonempty-cgroup.procs"
bash -c 'source "$1"; require_empty_cgroup_procs fixture "$2"' \
  _ "$tmp_dir/runner-binding-functions.sh" "$tmp_dir/empty-cgroup.procs"
if bash -c 'source "$1"; require_empty_cgroup_procs fixture "$2"' \
  _ "$tmp_dir/runner-binding-functions.sh" "$tmp_dir/nonempty-cgroup.procs" \
  >"$tmp_dir/nonempty-procs.stdout" 2>"$tmp_dir/nonempty-procs.stderr"; then
  echo "nonempty cgroup.procs was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'unexpected direct process' "$tmp_dir/nonempty-procs.stderr"
if bash -c 'source "$1"; require_empty_cgroup_procs fixture "$2"' \
  _ "$tmp_dir/runner-binding-functions.sh" "$tmp_dir/missing-cgroup.procs" \
  >"$tmp_dir/missing-procs.stdout" 2>"$tmp_dir/missing-procs.stderr"; then
  echo "missing cgroup.procs was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'process control read failed' "$tmp_dir/missing-procs.stderr"

cat >"$tmp_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DIREXTALK_DOCKER_LOG"
case "$1" in
  network|volume)
    [ "${DIREXTALK_DOCKER_COLLISION:-false}" = true ] && exit 0
    exit 1
    ;;
  ps)
    if [ -f "$DIREXTALK_DOCKER_STATE" ]; then
      case "$*" in
        *com.docker.compose.service=agent*) printf 'agent-container\n' ;;
        *com.docker.compose.service=extension-runner*) printf 'extension-runner-container\n' ;;
        *com.docker.compose.service=core-runner*) printf 'core-runner-container\n' ;;
        *com.docker.compose.service=message-server*) printf 'message-container\n' ;;
      esac
    fi
    ;;
  inspect)
    case "${*: -1}" in
      agent-container) printf '%s|agent|healthy\n' "$DIREXTALK_FAKE_STACK" ;;
      extension-runner-container) printf '%s|extension-runner|healthy\n' "$DIREXTALK_FAKE_STACK" ;;
      core-runner-container) printf '%s|core-runner|healthy\n' "$DIREXTALK_FAKE_STACK" ;;
      message-container) printf '%s|message-server|healthy\n' "$DIREXTALK_FAKE_STACK" ;;
      *) exit 1 ;;
    esac
    ;;
  compose)
    case " $* " in
      *' up '*) touch "$DIREXTALK_DOCKER_STATE" ;;
    esac
    ;;
  *) exit 1 ;;
esac
EOF
chmod 755 "$tmp_dir/bin/docker"
cat >"$tmp_dir/bin/ss" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${DIREXTALK_PORT_COLLISION:-false}" = true ]; then
  printf 'LISTEN 0 4096 *:8008 *:*\n'
fi
EOF
chmod 755 "$tmp_dir/bin/ss"
export PATH=$tmp_dir/bin:$PATH
export DIREXTALK_DOCKER_LOG=$docker_log
export DIREXTALK_DOCKER_STATE=$docker_state
export DIREXTALK_SPLIT_FIXTURE_MODE=true
export DIREXTALK_SPLIT_TEST_MODE=true

"$script_dir/provision-local.sh" "$tmp_dir/stack" >/dev/null
unset DIREXTALK_SPLIT_FIXTURE_MODE DIREXTALK_SPLIT_TEST_MODE
env_file=$tmp_dir/stack/.env
export DIREXTALK_FAKE_STACK
DIREXTALK_FAKE_STACK=$(sed -n 's/^DIREXTALK_SPLIT_STACK_NAME=//p' "$env_file")

if DIREXTALK_SPLIT_FIXTURE_MODE=true "$script" "$env_file" >/dev/null 2>"$tmp_dir/fixture-start.stderr"; then
  echo "fixture mode was unexpectedly accepted by production start-local" >&2
  exit 1
fi
grep -Fq 'fixture mode is forbidden for production start-local' "$tmp_dir/fixture-start.stderr"
unset DIREXTALK_SPLIT_FIXTURE_MODE DIREXTALK_SPLIT_TEST_MODE

: >"$docker_log"
if DIREXTALK_PORT_COLLISION=true "$script" "$env_file" >/dev/null 2>&1; then
  echo "occupied host port unexpectedly accepted" >&2
  exit 1
fi
if grep -Eq '^compose .* (build|up) ' "$docker_log"; then
  echo "port-collision path mutated Docker state" >&2
  exit 1
fi

: >"$docker_log"
if DIREXTALK_DOCKER_COLLISION=true "$script" "$env_file" >/dev/null 2>&1; then
  echo "existing Docker resource unexpectedly accepted" >&2
  exit 1
fi
if grep -Eq '^compose .* (build|up) ' "$docker_log"; then
  echo "collision path mutated Docker state" >&2
  exit 1
fi

: >"$docker_log"
if output=$("$script" "$env_file" 2>&1); then
  echo "missing delegated cgroup roots were unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'must already exist as a delegated cgroup-v2 directory'
if grep -Eq '^compose .* (build|up) ' "$docker_log"; then
  echo "cgroup preflight failure mutated Docker state" >&2
  exit 1
fi

# Runner identity bindings are checked before any Docker operation. A copied
# env file with one unit field changed must fail closed against the immutable
# manifest binding.
env_backup=$tmp_dir/env.backup
cp -- "$env_file" "$env_backup"
chmod 600 "$env_file"
sed -i 's/^DIREXTALK_EXTENSION_RUNNER_UNIT=.*/DIREXTALK_EXTENSION_RUNNER_UNIT=not-the-stack.service/' "$env_file"
chmod 400 "$env_file"
if "$script" "$env_file" >/dev/null 2>"$tmp_dir/tampered-env.stderr"; then
  echo "tampered runner env was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'DIREXTALK_EXTENSION_RUNNER_UNIT differs from its manifest runner binding' "$tmp_dir/tampered-env.stderr"
chmod 600 "$env_file"
cp -- "$env_backup" "$env_file"
chmod 400 "$env_file"

# A manifest edit is equally rejected before Docker is inspected. Restore the
# protected fixture byte-for-byte afterwards so the test remains self-contained.
manifest_backup=$tmp_dir/manifest.backup
cp -- "$tmp_dir/stack/.manifest" "$manifest_backup"
chmod 600 "$tmp_dir/stack/.manifest"
sed -i 's/^runner.extension.parent=.*/runner.extension.parent=wrong.slice/' "$tmp_dir/stack/.manifest"
chmod 400 "$tmp_dir/stack/.manifest"
if "$script" "$env_file" >/dev/null 2>"$tmp_dir/tampered-manifest.stderr"; then
  echo "tampered runner manifest was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'DIREXTALK_EXTENSION_CGROUP_PARENT differs from its manifest runner binding' "$tmp_dir/tampered-manifest.stderr"
chmod 600 "$tmp_dir/stack/.manifest"
cp -- "$manifest_backup" "$tmp_dir/stack/.manifest"
chmod 400 "$tmp_dir/stack/.manifest"

# The production gate records the template hash. A fixture change must never
# compare equal to the repository-owned unit hash.
fixture_unit=$tmp_dir/fixture-extension.service
cp -- "$script_dir/../systemd/dirextalk-extension-runner@.service" "$fixture_unit"
fixture_hash=$(sha256sum -- "$fixture_unit" | awk '{print $1}')
sed -i 's/^TasksMax=infinity$/TasksMax=1/' "$fixture_unit"
tampered_fixture_hash=$(sha256sum -- "$fixture_unit" | awk '{print $1}')
[ "$fixture_hash" != "$tampered_fixture_hash" ] || {
  echo "tampered unit fixture hash unexpectedly matched" >&2
  exit 1
}

if grep -Eq 'docker (compose )?(down|rm)|volume (rm|prune)|system prune' "$script"; then
  echo "start-local.sh must never delete existing stacks or volumes" >&2
  exit 1
fi

printf 'fresh-stack startup ownership and sequence verified\n'
