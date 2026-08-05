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
grep -Fqx 'DIREXTALK_EXTENSION_CGROUP_ROOT=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CGROUP_ROOT=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_EXTENSION_CONTROL_GROUP=unknown' <<<"$output"
grep -Fqx 'DIREXTALK_CORE_RUNNER_CONTROL_GROUP=unknown' <<<"$output"
grep -Fqx "DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256=$extension_hash" <<<"$output"
grep -Fqx "DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256=$core_hash" <<<"$output"
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

echo "prepare-runner-cgroups fixture tests passed"
