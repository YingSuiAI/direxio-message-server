#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
entrypoint=$script_dir/postgres-entrypoint.sh
initializer=$script_dir/initialize-postgres.sh
image=${DIREXTALK_POSTGRES_TEST_IMAGE:-docker.io/pgvector/pgvector:pg18@sha256:691673308c99d2161ba298736f3147f1f22d79de2fb7ec93ae9b4afcab870b62}

[ -x "$entrypoint" ] || { echo "postgres-entrypoint.sh must be executable" >&2; exit 1; }
[ -x "$initializer" ] || { echo "initialize-postgres.sh must be executable" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "docker daemon is unavailable" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-postgres-entrypoint.XXXXXX")
test_id=$(basename -- "$tmp")
container=dirextalk-postgres-secret-test-${test_id##*.}
volume=${container}-data
cleanup() {
  local owner
  if docker container inspect "$container" >/dev/null 2>&1; then
    owner=$(docker container inspect --format '{{ index .Config.Labels "io.dirextalk.test-id" }}' "$container" 2>/dev/null || true)
    if [ "$owner" = "$test_id" ]; then
      docker container rm -f "$container" >/dev/null 2>&1 || true
    else
      echo "refusing to remove replacement test container: $container" >&2
    fi
  fi
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    owner=$(docker volume inspect --format '{{ index .Labels "io.dirextalk.test-id" }}' "$volume" 2>/dev/null || true)
    if [ "$owner" = "$test_id" ]; then
      docker volume rm "$volume" >/dev/null 2>&1 || true
    else
      echo "refusing to remove replacement test volume: $volume" >&2
    fi
  fi
  rm -rf -- "$tmp"
}
trap cleanup EXIT

mkdir -m 700 "$tmp/secrets"
printf '%048x\n' 1 >"$tmp/secrets/postgres_admin_password"
printf '%048x\n' 2 >"$tmp/secrets/message_postgres_password"
printf '%048x\n' 3 >"$tmp/secrets/agent_postgres_password"
chmod 0400 "$tmp/secrets/"*
docker volume create --label "io.dirextalk.test-id=$test_id" "$volume" >/dev/null

docker run -d \
  --name "$container" \
  --label "io.dirextalk.test-id=$test_id" \
  --read-only \
  --cap-drop ALL \
  --cap-add CHOWN \
  --cap-add FOWNER \
  --cap-add DAC_OVERRIDE \
  --cap-add SETUID \
  --cap-add SETGID \
  --security-opt no-new-privileges:true \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=1777 \
  --tmpfs /run/postgresql:rw,noexec,nosuid,nodev,mode=2775 \
  --tmpfs /run/dirextalk-postgres-secrets:rw,noexec,nosuid,nodev,mode=0700,uid=0,gid=0 \
  --mount "type=volume,src=$volume,dst=/var/lib/postgresql" \
  --mount "type=bind,src=$entrypoint,dst=/usr/local/bin/dirextalk-postgres-entrypoint,readonly" \
  --mount "type=bind,src=$initializer,dst=/docker-entrypoint-initdb.d/10-dirextalk-databases.sh,readonly" \
  --mount "type=bind,src=$tmp/secrets/postgres_admin_password,dst=/run/secrets/postgres_admin_password,readonly" \
  --mount "type=bind,src=$tmp/secrets/message_postgres_password,dst=/run/secrets/message_postgres_password,readonly" \
  --mount "type=bind,src=$tmp/secrets/agent_postgres_password,dst=/run/secrets/agent_postgres_password,readonly" \
  --env POSTGRES_DB=postgres \
  --env POSTGRES_USER=dirextalk_cluster_admin \
  --env POSTGRES_PASSWORD_FILE=/run/dirextalk-postgres-secrets/postgres_admin_password \
  --env POSTGRES_INIT_SECRET_DIR=/run/dirextalk-postgres-secrets \
  --entrypoint /usr/local/bin/dirextalk-postgres-entrypoint \
  "$image" postgres >/dev/null

wait_for_postgres() {
  local expected_ready_count=$1 attempt=0 ready_count
  while [ "$attempt" -lt 60 ]; do
    ready_count=$(docker logs "$container" 2>&1 | grep -Fc 'database system is ready to accept connections' || true)
    if [ "$ready_count" -ge "$expected_ready_count" ] && \
       docker exec "$container" pg_isready -U dirextalk_cluster_admin -d postgres >/dev/null 2>&1; then
      return 0
    fi
    if [ "$(docker container inspect --format '{{.State.Running}}' "$container" 2>/dev/null || true)" != true ]; then
      docker logs "$container" >&2 || true
      return 1
    fi
    sleep 1
    attempt=$((attempt + 1))
  done
  docker logs "$container" >&2 || true
  return 1
}

wait_for_postgres 2
docker exec -u postgres "$container" sh -ec '
  postgres_uid=$(id -u postgres)
  postgres_gid=$(id -g postgres)
  test "$(stat -c "%u:%g:%a" /run/dirextalk-postgres-secrets)" = "0:$postgres_gid:750"
  for name in postgres_admin_password message_postgres_password agent_postgres_password; do
    test "$(stat -c "%u:%g:%a" "/run/dirextalk-postgres-secrets/$name")" = "$postgres_uid:$postgres_gid:400"
    test -r "/run/dirextalk-postgres-secrets/$name"
    test ! -r "/run/secrets/$name"
  done
'

[ "$(docker exec -u postgres "$container" psql -U dirextalk_cluster_admin -d postgres -Atc \
  "SELECT count(*) FROM pg_roles WHERE rolname IN ('dirextalk_message_server', 'dirextalk_agent');")" = 2 ]
[ "$(docker exec -u postgres "$container" psql -U dirextalk_cluster_admin -d postgres -Atc \
  "SELECT count(*) FROM pg_database WHERE datname IN ('dirextalk_message_server', 'dirextalk_agent');")" = 2 ]
[ "$(docker exec -u postgres "$container" psql -U dirextalk_cluster_admin -d dirextalk_agent -Atc \
  "SELECT count(*) FROM pg_extension WHERE extname = 'vector';")" = 1 ]

docker restart "$container" >/dev/null
wait_for_postgres 3
[ "$(docker exec -u postgres "$container" psql -U dirextalk_cluster_admin -d postgres -Atc 'SELECT 1;')" = 1 ]

echo "PostgreSQL root secret materializer real-consumer test passed"
