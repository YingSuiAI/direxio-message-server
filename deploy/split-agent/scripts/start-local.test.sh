#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/start-local.sh
[ -x "$script" ] || { echo "start-local.sh must be executable" >&2; exit 1; }
bash -n "$script"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-start-local.XXXXXX")
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT
mkdir -p "$tmp_dir/bin"
docker_log=$tmp_dir/docker.log
docker_state=$tmp_dir/up.state

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
        *com.docker.compose.service=message-server*) printf 'message-container\n' ;;
      esac
    fi
    ;;
  inspect)
    case "${*: -1}" in
      agent-container) printf '%s|agent|healthy\n' "$DIREXTALK_FAKE_STACK" ;;
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

"$script_dir/provision-local.sh" "$tmp_dir/stack" >/dev/null
env_file=$tmp_dir/stack/.env
export DIREXTALK_FAKE_STACK
DIREXTALK_FAKE_STACK=$(sed -n 's/^DIREXTALK_SPLIT_STACK_NAME=//p' "$env_file")

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
"$script" "$env_file" >/dev/null
agent_build=$(grep -nE '^compose .* build --pull=false agent$' "$docker_log" | cut -d: -f1)
message_build=$(grep -nE '^compose .* build --pull=false message-server$' "$docker_log" | cut -d: -f1)
startup=$(grep -nE '^compose .* up -d --no-build --wait message-server$' "$docker_log" | cut -d: -f1)
[ -n "$agent_build" ] && [ -n "$message_build" ] && [ -n "$startup" ]
[ "$agent_build" -lt "$message_build" ] && [ "$message_build" -lt "$startup" ] || {
  echo "local startup order is not Agent build, message build, Compose up" >&2
  exit 1
}

if grep -Eq 'docker (compose )?(down|rm)|volume (rm|prune)|system prune' "$script"; then
  echo "start-local.sh must never delete existing stacks or volumes" >&2
  exit 1
fi

printf 'fresh-stack startup ownership and sequence verified\n'
