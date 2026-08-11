#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
stack_dir=$(cd -- "$script_dir/.." && pwd -P)
tmp_root=$(printenv TMPDIR 2>/dev/null || true)
[ -n "$tmp_root" ] || tmp_root=/tmp
run_dir=$(mktemp -d "$tmp_root/dirextalk-runner-limits.XXXXXX")
cleanup() {
  rm -rf -- "$run_dir"
}
trap cleanup EXIT

DIREXTALK_MESSAGE_HTTP_BIND=18008 \
DIREXTALK_MESSAGE_HTTPS_BIND=18448 \
DIREXTALK_SPLIT_FIXTURE_MODE=true \
DIREXTALK_SPLIT_TEST_MODE=true \
  "$script_dir/provision-local.sh" "$run_dir/provision" >/dev/null 2>"$run_dir/provision.stderr"

render_and_assert() {
  local mode=$1 output=$2
  shift 2
  (
    local compose=(docker compose --env-file "$run_dir/provision/.env" -f compose.yaml)
    cd -- "$stack_dir"
    if [ "$mode" = local ]; then
      compose+=(-f compose.direct-tls.yaml -f compose.local.yaml)
    else
      compose+=(-f compose.production.yaml)
    fi
    "$@" "${compose[@]}" config --format json
  ) >"$output"
  jq -e '
    .services["extension-runner"].cpus == 2 and
    (.services["extension-runner"].mem_limit | tostring) == "1073741824" and
    .services["extension-runner"].pids_limit == 256 and
    .services["extension-runner"].network_mode == "none" and
    .services["extension-runner"].cgroup == "host"
  ' "$output" >/dev/null
}

render_and_assert local "$run_dir/local.json" env
render_and_assert production "$run_dir/production.json" env \
  DIREXTALK_SPLIT_COMPOSE_MODE=production \
  DIREXTALK_MESSAGE_TLS_MODE=edge-terminated \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_CLIENT_BASE_URL=https://message.example.com \
  DIREXTALK_RELEASE_CATALOG_ORIGIN=https://imadmin.dirextalk.ai

printf '%s\n' 'extension-runner Compose limits test passed'
