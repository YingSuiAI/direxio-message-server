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
static_sites_root=$(awk -F= '$1 == "DIREXTALK_STATIC_SITES_ROOT" {print substr($0,index($0,"=")+1)}' "$run_dir/provision/.env")
[ -n "$static_sites_root" ]
[ "$(awk -F= '$1 == "DIREXTALK_AGENT_BUILD_VERSION" {print $2}' "$run_dir/provision/.env")" = dev ]

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
  jq -e --arg static_root "$static_sites_root" --arg mode "$mode" '
    def exact_runner_security:
      (.security_opt | sort) == ([
        "apparmor=dirextalk-runner-userns",
        "no-new-privileges:true",
        "seccomp=unconfined"
      ] | sort);
    def preserves_outer_runner_boundary:
      .read_only == true and
      .user != "0:0" and
      (.cap_drop == ["ALL"]) and
      .network_mode == "none" and
      exact_runner_security;
    .services["extension-runner"].cpus == 2 and
    (.services["extension-runner"].mem_limit | tostring) == "1073741824" and
    .services["extension-runner"].pids_limit == 256 and
    (.services["extension-runner"] | preserves_outer_runner_boundary) and
    (.services["core-runner"] | preserves_outer_runner_boundary) and
    .services["extension-runner"].cgroup == "host" and
    .services["core-runner"].cgroup == "host" and
    ([.services | to_entries[] |
      select(.key != "extension-runner" and .key != "core-runner") |
      .value.security_opt[]? |
      select(. == "apparmor=dirextalk-runner-userns" or . == "seccomp=unconfined")]
      | length) == 0 and
    (.services["extension-runner"].tmpfs | any(. == "/tmp:rw,noexec,nosuid,nodev,mode=1777")) and
    (.services["core-runner"].tmpfs | any(. == "/tmp:rw,noexec,nosuid,nodev,mode=1777")) and
    (.services["extension-runner"].command as $command |
      ($command | index("--prepared-root")) as $prepared |
      ($command | index("--node-runtime-root")) as $runtime |
      $prepared != null and $command[$prepared + 1] == "/var/lib/dirextalk-agent/extension-install/.prepared" and
      $runtime != null and $command[$runtime + 1] == "/usr/local/libexec/dirextalk-node-runtime") and
    ((.services["extension-runner-storage-init"].command | join("\n")) as $init |
      ($init | contains("mkdir -p /install/.prepared;")) and
      ($init | contains("chown 65531:65531 /install/.prepared;")) and
      ($init | contains("chmod 0700 /install/.prepared;")) and
      ($init | contains("stat -c")) and
      ($init | contains("/install/.prepared"))) and
    ($mode != "local" or
      (.services.agent.build.args.VERSION == "dev" and
       .services["agent-migrate"].build.args.VERSION == "dev")) and
    ([.services.agent.volumes[] | select(.type == "bind" and .source == $static_root and .target == "/var/lib/dirextalk-agent/static-sites" and .bind.create_host_path == false)] | length) == 1 and
    ([.services["extension-runner"].volumes[] | select(.source == $static_root or .target == "/var/lib/dirextalk-agent/static-sites")] | length) == 0 and
    ([.services["core-runner"].volumes[] | select(.source == $static_root or .target == "/var/lib/dirextalk-agent/static-sites")] | length) == 0
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
