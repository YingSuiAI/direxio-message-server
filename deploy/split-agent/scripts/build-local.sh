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
capability_root=$(cd "$message_root/../dirextalk-capability-api" && pwd -P)
"$script_dir/verify-build-contexts.sh" "$agent_root" "$message_root" "$capability_root" >/dev/null

services=(message-server agent)
[ "$#" -eq 0 ] || services=("$@")
cd "$stack_dir"
docker compose --env-file "$env_file" -f compose.yaml -f compose.local.yaml \
  build --pull=false "${services[@]}"
printf 'local consumer build completed with capability-api additional context and build-stage replace\n'
