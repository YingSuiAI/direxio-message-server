#!/usr/bin/env bash
set -euo pipefail

# Regression test for the split message-server healthcheck's process boundary.
# BusyBox wget's TLS helper can outlive an interrupted healthcheck. Compose's
# init:true must therefore be present so Docker's init reaps an orphaned
# helper instead of letting it consume the container PID limit.
script_dir=$(cd "$(dirname "$0")" && pwd -P)
compose_file=$script_dir/../compose.yaml
direct_tls_file=$script_dir/../compose.direct-tls.yaml
bootstrap_script=$script_dir/bootstrap-local-account.sh
grep -Fqx '    init: true' <(sed -n '/^  message-server:/,/^  [a-zA-Z].*:/p' "$compose_file")
grep -Fq 'test: ["CMD", "wget", "-Y", "off", "-q", "-O", "-", "http://127.0.0.1:8008/_p2p/health"]' "$compose_file"
grep -Fq 'wget -Y off -q -O - http://127.0.0.1:8008/_p2p/health' "$direct_tls_file"
grep -Fq 'wget -Y off --no-check-certificate -q -O - https://127.0.0.1:8448/_p2p/health' "$direct_tls_file"
grep -Fq 'wget -Y off -q -O - http://127.0.0.1:8008/_p2p/health' "$bootstrap_script"

# This is intentionally a real Docker process test, not a shell-only fixture.
# Do not pull or build anything here; the standard Alpine utility image is the
# same public base used by the deployment images.
if ! docker image inspect alpine:latest >/dev/null 2>&1; then
  echo 'message-server healthcheck init regression test skipped: alpine:latest is not present' >&2
  exit 0
fi

container_id=
cleanup() {
  if [[ $container_id =~ ^[0-9a-f]{64}$ ]] && docker inspect "$container_id" >/dev/null 2>&1; then
    docker stop "$container_id" >/dev/null 2>&1 || true
    docker rm "$container_id" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

container_id=$(docker run -d --init alpine:latest sh -c 'sleep 20')
[[ $container_id =~ ^[0-9a-f]{64}$ ]]
# Simulate Docker terminating a timed-out healthcheck shell while its TLS
# helper is still alive. The helper becomes an orphan under the container init.
docker exec "$container_id" sh -c 'sleep 0.2 & kill -9 $$' >/dev/null 2>&1 || true
sleep 1
zombies=$(docker exec "$container_id" sh -c "ps -eo stat | awk '\$1 ~ /^Z/ { count++ } END { print count + 0 }'")
[ "$zombies" = 0 ] || {
  echo "container init left $zombies orphaned healthcheck processes" >&2
  exit 1
}

printf 'message-server healthcheck init/reaping regression passed\n'
