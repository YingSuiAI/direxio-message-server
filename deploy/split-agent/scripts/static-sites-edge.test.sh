#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
stack_dir=$(cd -- "$script_dir/.." && pwd -P)
tmp_root=$(printenv TMPDIR 2>/dev/null || true)
[ -n "$tmp_root" ] || tmp_root=/tmp
run_dir=$(mktemp -d "$tmp_root/dirextalk-static-sites-edge.XXXXXX")
cleanup() {
  rm -rf -- "$run_dir"
}
trap cleanup EXIT

mkdir -p "$run_dir/static-sites/public"
cat >"$run_dir/edge.env" <<EOF
DIREXTALK_EDGE_STACK_NAME=dirextalk-static-sites-test
DIREXTALK_PUBLIC_DOMAIN=static.example.test
DIREXTALK_MESSAGE_PUBLIC_NETWORK=dirextalk-static-sites-public
DIREXTALK_CADDY_IMAGE_IMMUTABLE=docker.io/library/caddy@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648
DIREXTALK_CADDY_DATA_VOLUME=dirextalk-static-sites-caddy-data
DIREXTALK_CADDY_CONFIG_VOLUME=dirextalk-static-sites-caddy-config
DIREXTALK_CADDYFILE=$stack_dir/Caddyfile.static-sites.example
DIREXTALK_STATIC_SITES_ROOT=$run_dir/static-sites
EOF

docker compose --env-file "$run_dir/edge.env" -f "$stack_dir/edge-compose.yaml" config --format json >"$run_dir/edge.json"
jq -e --arg root "$run_dir/static-sites/public" '
  .services.caddy.environment.DIREXTALK_PUBLIC_DOMAIN == "static.example.test" and
  ([.services.caddy.volumes[] | select(.type == "bind" and .source == $root and .target == "/srv/dirextalk-sites" and .read_only == true)] | length) == 1 and
  .services.caddy.read_only == true and
  (.services.caddy.cap_drop | index("ALL")) != null and
  (.services.caddy.cap_add | index("NET_BIND_SERVICE")) != null and
  (.services.caddy.healthcheck.test | join(" ") | contains("wget -Y off"))
' "$run_dir/edge.json" >/dev/null

grep -Eq 'handle_path[[:space:]]+/\.sites/\*' "$stack_dir/Caddyfile.static-sites.example"
grep -Eq 'root[[:space:]]+\*[[:space:]]+/srv/dirextalk-sites' "$stack_dir/Caddyfile.static-sites.example"
grep -Eq "Content-Security-Policy.*sandbox;.*script-src 'none';.*connect-src 'none';.*form-action 'none'" "$stack_dir/Caddyfile.static-sites.example"
grep -Eq 'handle[[:space:]]+/agent/v1/\*' "$stack_dir/Caddyfile.static-sites.example"
grep -Eq 'reverse_proxy[[:space:]]+agent:8082' "$stack_dir/Caddyfile.static-sites.example"
grep -Eq 'flush_interval[[:space:]]+-1' "$stack_dir/Caddyfile.static-sites.example"
if grep -Eq 'handle_path[[:space:]]+/agent/v1/\*' "$stack_dir/Caddyfile.static-sites.example"; then
  echo 'Agent data-plane proxy must preserve the /agent/v1 path' >&2
  exit 1
fi

printf '%s\n' 'static-site edge Compose and Caddy contract passed'
