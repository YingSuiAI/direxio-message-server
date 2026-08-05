#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ENV_FILE [SERVICE ...]" >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
env_file=$1
shift
[ -f "$env_file" ] && [ ! -L "$env_file" ] || {
  echo "local build: ENV_FILE must be a regular non-symlink file" >&2
  exit 1
}
[ "$(stat -c '%a' "$env_file")" = 400 ] || {
  echo "local build: ENV_FILE must be mode 0400" >&2
  exit 1
}
script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
message_root=$(cd "$stack_dir/../.." && pwd -P)
agent_root=$(cd "$message_root/../dirextalk-agent" && pwd -P)
"$script_dir/verify-build-contexts.sh" "$agent_root" "$message_root" >/dev/null

services=(agent message-server)
[ "$#" -eq 0 ] || services=("$@")
cd "$stack_dir"
for service in "${services[@]}"; do
  case "$service" in
    -*|'') echo "local build: invalid service name: $service" >&2; exit 1 ;;
  esac
  docker compose --env-file "$env_file" -f compose.yaml -f compose.local.yaml \
    build --pull=false "$service"
done
printf 'local repository-owned image build completed\n'
