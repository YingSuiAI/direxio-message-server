#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
deploy_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT HUP INT TERM

while IFS= read -r var; do
  export "$var=x"
done < <(rg -o '\$\{[A-Z0-9_]+' "$deploy_dir/compose.yaml" "$deploy_dir/compose.cloud-worker.yaml" | sed 's/.*${//' | sort -u)

export DIREXTALK_SPLIT_STACK_NAME=test-cloud-worker
export DIREXTALK_MESSAGE_HTTP_BIND=18008
export DIREXTALK_CLOUD_WORKER_AGENT_BIND_IP=10.0.1.20
export DIREXTALK_EXTENSION_CGROUP_PARENT=extension.slice
export DIREXTALK_CORE_RUNNER_CGROUP_PARENT=core.slice

docker compose -f "$deploy_dir/compose.yaml" -f "$deploy_dir/compose.cloud-worker.yaml" config --format json >"$tmp"
jq -e '
  .services.agent.networks == {
    agent_caller:null, agent_database:null, agent_egress:null,
    agent_private:null
  } and
  ([.services.agent.ports[] | select(.target == 10443 and .published == "10443" and .host_ip == "10.0.1.20")] | length) == 1 and
  ([.services.agent.ports[] | select(.target == 11443 and .published == "11443" and .host_ip == "10.0.1.20")] | length) == 1 and
  ([.services.agent.ports[] | select(.target == 10443 or .target == 11443) | select(.host_ip == "0.0.0.0" or .host_ip == "::")] | length) == 0 and
  ([.services.agent.volumes[] | select(.target | startswith("/run/cloud-worker/"))] | length) == 7 and
  ([.services.agent.volumes[] | select((.target | startswith("/run/cloud-worker/")) and .read_only == true)] | length) == 7 and
  (.services | has("cloud-worker-controlled-proxy") | not) and
  (.services | has("cloud-worker-edge") | not)
' "$tmp" >/dev/null

printf '%s\n' 'Cloud Worker Compose rendering tests passed'
