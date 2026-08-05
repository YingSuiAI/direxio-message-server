#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck disable=SC1091
source "$script_dir/agent-runtime-local-common.sh"

if [ "$#" -ne 1 ]; then
  agent_runtime_usage "$0"
fi

set +e
agent_runtime_main stop "$1"
status=$?
set -e
exit "$status"
