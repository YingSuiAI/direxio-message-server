#!/bin/sh
# Ownership guard regression using a fake Docker CLI. No daemon or service is
# contacted; the fake exercises owned, unowned, and inspect-error resources.
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
script=$root/scripts/agent-core-e2e.sh
fail() { echo "agent-core-e2e guard assertion failed: $*" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fakebin=$tmp/bin
mkdir -p "$fakebin"
cat > "$fakebin/docker" <<'EOF'
#!/bin/sh
set -eu
marker=${FAKE_DOCKER_MARKER:?}
if [ "${1:-}" = compose ]; then
  project=
  previous=
  for arg in "$@"; do
    if [ "$previous" = -p ]; then project=$arg; fi
    previous=$arg
  done
  case " $* " in
    *' config --format json '*|*' config --format json')
      printf '{"networks":{"private":{"name":"%s-network"}},"volumes":{"data":{"name":"%s-volume"}}}\n' "$project" "$project"
      ;;
    *' ps -aq '*|*' ps -aq') ;;
    *' down --volumes --remove-orphans '*|*' down --volumes --remove-orphans')
      printf '%s\n' "$project" >> "$marker"
      ;;
  esac
  exit 0
fi
kind=${1:-}
op=${2:-}
name=${3:-}
if [ "$op" = inspect ] && [ "$name" = --format ]; then name=${5:-}; fi
case "$kind:$op" in
  network:ls|volume:ls)
    printf '%s\n' "$FAKE_DOCKER_RESOURCES" | tr ' ' '\n'
    ;;
  network:inspect|volume:inspect)
    case " ${FAKE_DOCKER_INSPECT_ERROR:-} " in *" $name "*) exit 7 ;; esac
    case " ${FAKE_DOCKER_UNOWNED:-} " in *" $name "*) printf '%s\n' other-project ;; *) printf '%s\n' "${name%-network}" | sed 's/-volume$//' ;; esac
    ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$fakebin/docker"

export DIREXTALK_E2E_RUN_ROOT=$tmp/runs
export DIREXTALK_AGENT_IMAGE_IMMUTABLE=example/agent@sha256:1111111111111111111111111111111111111111111111111111111111111111
export DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE=example/runner@sha256:2222222222222222222222222222222222222222222222222222222222222222
export DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE=example/core-runner@sha256:3333333333333333333333333333333333333333333333333333333333333333
export DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=postgres@sha256:4444444444444444444444444444444444444444444444444444444444444444
export DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=example/server@sha256:5555555555555555555555555555555555555555555555555555555555555555
"$script" prepare guard >/dev/null 2>&1
export FAKE_DOCKER_MARKER=$tmp/marker
server_project=dirextalk-message-server-e2e-guard
agent_project=dirextalk-agent-e2e-guard
export FAKE_DOCKER_RESOURCES="$server_project-network $server_project-volume $agent_project-network $agent_project-volume"
export PATH=$fakebin:$PATH

: > "$FAKE_DOCKER_MARKER"
"$script" down guard >/dev/null 2>&1 || fail 'owned resources were rejected'
[ "$(wc -l < "$FAKE_DOCKER_MARKER")" -eq 2 ] || fail 'owned cleanup did not reach both Compose projects'

: > "$FAKE_DOCKER_MARKER"
export FAKE_DOCKER_UNOWNED="$server_project-network"
if "$script" down guard >/dev/null 2>&1; then fail 'unowned resource was accepted'; fi
[ ! -s "$FAKE_DOCKER_MARKER" ] || fail 'unowned cleanup ran Compose down'
unset FAKE_DOCKER_UNOWNED

: > "$FAKE_DOCKER_MARKER"
export FAKE_DOCKER_INSPECT_ERROR="$agent_project-volume"
if "$script" down guard >/dev/null 2>&1; then fail 'inspect error was accepted'; fi
[ ! -s "$FAKE_DOCKER_MARKER" ] || fail 'inspect-error cleanup ran Compose down'

echo 'agent-core-e2e ownership guards passed'
