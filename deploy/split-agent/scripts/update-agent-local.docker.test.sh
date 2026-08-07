#!/usr/bin/env bash
# Real-Docker regression for the update wrapper's runner-volume consumer path.
# It starts with fresh empty extension/Core socket volumes, executes the
# repository Compose initializers, mounts each through the real runner service,
# then exercises the production extension health command.
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
compose_file=$stack_dir/compose.yaml
production_compose_file=$stack_dir/compose.production.yaml
command -v docker >/dev/null 2>&1 || { echo 'docker is required' >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo 'Docker Engine is unavailable' >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-update-volume.XXXXXX")
suffix=$(printf '%s' "$tmp-$$" | sha256sum | cut -c1-20)
stack=d-${suffix}aaaaaa
extension_socket_volume=dirextalk-update-volume-$suffix
core_socket_volume=dirextalk-update-core-volume-$suffix
server_container=dirextalk-update-probe-$suffix
cleanup() {
  docker container rm -f "$server_container" >/dev/null 2>&1 || true
  docker volume rm "$extension_socket_volume" "$core_socket_volume" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

agent_image=${DIREXTALK_UPDATE_TEST_AGENT_IMAGE:-docker.io/dirextalk/agent:v1.0.3}
utility_image=${DIREXTALK_UPDATE_TEST_UTILITY_IMAGE:-postgres:18}
docker image inspect "$agent_image" >/dev/null 2>&1 || { echo "missing local test image: $agent_image" >&2; exit 1; }
docker image inspect "$utility_image" >/dev/null 2>&1 || { echo "missing local test image: $utility_image" >&2; exit 1; }

env_file=$tmp/stack.env
cat >"$env_file" <<EOF
DIREXTALK_SPLIT_STACK_NAME=$stack
DIREXTALK_SPLIT_COMPOSE_MODE=production
DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=$utility_image
DIREXTALK_UTILITY_IMAGE_IMMUTABLE=$utility_image
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=docker.io/dirextalk/message-server:v1.1.5
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image
DIREXTALK_QDRANT_IMAGE_IMMUTABLE=qdrant/qdrant:v1.18.3
DIREXTALK_COTURN_IMAGE_IMMUTABLE=coturn/coturn:4.6.3-alpine
DIREXTALK_ACCOUNT_GENERATION=1
DIREXTALK_AGENT_INSTANCE_ID=11111111-1111-4111-8111-111111111111
DIREXTALK_MESSAGE_SERVER_INSTANCE_ID=22222222-2222-4222-8222-222222222222
DIREXTALK_MESSAGE_CLIENT_BASE_URL=https://localhost
DIREXTALK_RELEASE_CATALOG_ORIGIN=https://imadmin.dirextalk.ai
DIREXTALK_MESSAGE_TLS_MODE=edge
DIREXTALK_MESSAGE_HTTP_BIND=18008
DIREXTALK_CORE_EXTENSION_RUNNER_DIR=/run/dirextalk-agent
DIREXTALK_CORE_EXTENSION_RUNNER_SOCKET=/run/dirextalk-agent/extension-runner.sock
DIREXTALK_CORE_WORKLOAD_RUNNER_DIR=/run/dirextalk-core-runner
DIREXTALK_CORE_WORKLOAD_RUNNER_SOCKET=/run/dirextalk-core-runner/runner.sock
DIREXTALK_EXTENSION_CGROUP_ROOT=/tmp
DIREXTALK_CORE_RUNNER_CGROUP_ROOT=/tmp
DIREXTALK_AGENT_CONFIG_FILE=/dev/null
DIREXTALK_MESSAGE_SERVER_INITIALIZER_FILE=/dev/null
DIREXTALK_AGENT_SECRET_MATERIALIZER_FILE=/dev/null
DIREXTALK_MESSAGE_SERVER_ENTRYPOINT_FILE=/dev/null
DIREXTALK_CAPABILITY_CA_INITIALIZER_FILE=/dev/null
DIREXTALK_COTURN_CONFIG_FILE=/dev/null
DIREXTALK_TURN_SHARED_SECRET_FILE=/dev/null
DIREXTALK_MESSAGE_POSTGRES_PASSWORD_FILE=/dev/null
DIREXTALK_AGENT_POSTGRES_PASSWORD_FILE=/dev/null
DIREXTALK_MESSAGE_DATABASE_URL_FILE=/dev/null
DIREXTALK_MESSAGE_REGISTRATION_SHARED_SECRET_FILE=/dev/null
DIREXTALK_MESSAGE_PORTAL_PASSWORD_FILE=/dev/null
DIREXTALK_MESSAGE_TLS_CERT_FILE=/dev/null
DIREXTALK_MESSAGE_TLS_KEY_FILE=/dev/null
DIREXTALK_AGENT_DATABASE_URL_FILE=/dev/null
DIREXTALK_CORE_SECRET_MASTER_KEY_FILE=/dev/null
EOF
for name in \
  MESSAGE_POSTGRES_VOLUME MESSAGE_CONFIG_VOLUME MESSAGE_DATA_VOLUME MESSAGE_PLUGINS_VOLUME \
  AGENT_POSTGRES_VOLUME AGENT_SECRET_VOLUME AGENT_CONFIG_VOLUME AGENT_CORE_DATA_VOLUME \
  AGENT_INSTALL_VOLUME AGENT_STAGING_VOLUME AGENT_RUNNER_WORKSPACE_VOLUME AGENT_RUNNER_STATE_VOLUME \
  AGENT_KNOWLEDGE_CONTENT_VOLUME AGENT_KNOWLEDGE_MOUNT_VOLUME AGENT_QDRANT_VOLUME \
  CAPABILITY_AUTHORITY_VOLUME CAPABILITY_SHARED_VOLUME CAPABILITY_PRIVATE_VOLUME \
  CORE_RUNNER_INSTALL_VOLUME CORE_RUNNER_WORKSPACE_VOLUME CORE_RUNNER_STATE_VOLUME; do
  printf 'DIREXTALK_%s=%s-%s\n' "$name" "$stack" "${name,,}" >>"$env_file"
done
cat >>"$env_file" <<EOF
DIREXTALK_AGENT_SOCKET_VOLUME=$extension_socket_volume
DIREXTALK_CORE_RUNNER_SOCKET_VOLUME=$core_socket_volume
DIREXTALK_MESSAGE_PUBLIC_NETWORK=$stack-message-public
DIREXTALK_MESSAGE_PRIVATE_NETWORK=$stack-message-private
DIREXTALK_MESSAGE_DATABASE_NETWORK=$stack-message-database
DIREXTALK_AGENT_PRIVATE_NETWORK=$stack-agent-private
DIREXTALK_AGENT_DATABASE_NETWORK=$stack-agent-database
DIREXTALK_AGENT_CALLER_NETWORK=$stack-agent-caller
DIREXTALK_AGENT_EGRESS_NETWORK=$stack-agent-egress
EOF

docker volume create "$extension_socket_volume" >/dev/null
docker volume create "$core_socket_volume" >/dev/null

# This is the exact Compose consumer command used by normalize_runner_volumes.
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image docker compose --env-file "$env_file" \
  -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
  run --rm --no-deps --pull never -T --interactive=false extension-socket-init >/dev/null
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image docker compose --env-file "$env_file" \
  -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
  run --rm --no-deps --pull never -T --interactive=false core-runner-socket-init >/dev/null
extension_actual=$(docker run --rm --network none -v "$extension_socket_volume:/socket" "$utility_image" stat -c '%u:%g:%a' /socket)
core_actual=$(docker run --rm --network none -v "$core_socket_volume:/socket" "$utility_image" stat -c '%u:%g:%a' /socket)
[ "$extension_actual" = 65531:65532:2750 ] || { echo "extension socket initializer produced $extension_actual" >&2; exit 1; }
[ "$core_actual" = 65530:65532:2750 ] || { echo "Core socket initializer produced $core_actual" >&2; exit 1; }

# Missing sockets are the expected negative result. The real runner mounts
# must not copy image-directory metadata over the initialized volume roots.
if DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image docker compose --env-file "$env_file" \
    -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
    run --rm --no-deps --pull never -T --interactive=false extension-runner \
    probe --socket /run/dirextalk-agent/missing.sock --runner-uid 65531 >/dev/null 2>&1; then
  echo 'extension runner missing-socket probe unexpectedly succeeded' >&2
  exit 1
else
  status=$?
  [ "$status" -ne 125 ] || { echo 'extension runner mount probe had a Docker infrastructure failure' >&2; exit 1; }
fi
if DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image docker compose --env-file "$env_file" \
    -f "$compose_file" -f "$production_compose_file" --project-name "$stack" \
    run --rm --no-deps --pull never -T --interactive=false core-runner \
    probe --socket /run/dirextalk-core-runner/missing.sock >/dev/null 2>&1; then
  echo 'Core runner missing-socket probe unexpectedly succeeded' >&2
  exit 1
else
  status=$?
  [ "$status" -ne 125 ] || { echo 'Core runner mount probe had a Docker infrastructure failure' >&2; exit 1; }
fi
extension_actual=$(docker run --rm --network none -v "$extension_socket_volume:/socket" "$utility_image" stat -c '%u:%g:%a' /socket)
core_actual=$(docker run --rm --network none -v "$core_socket_volume:/socket" "$utility_image" stat -c '%u:%g:%a' /socket)
[ "$extension_actual" = 65531:65532:2750 ] || { echo "extension runner mount changed socket root to $extension_actual" >&2; exit 1; }
[ "$core_actual" = 65530:65532:2750 ] || { echo "Core runner mount changed socket root to $core_actual" >&2; exit 1; }

cat >"$tmp/probe-server.c" <<'EOF'
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stddef.h>
int main(int argc, char **argv) {
  int fd, client, i; struct sockaddr_un addr; char request[4096], response[8192];
  if (argc != 2 || (fd=socket(AF_UNIX,SOCK_SEQPACKET,0)) < 0) return 2;
  memset(&addr,0,sizeof(addr)); addr.sun_family=AF_UNIX;
  if (strlen(argv[1]) >= sizeof(addr.sun_path)) return 2;
  strcpy(addr.sun_path,argv[1]); unlink(argv[1]);
  if (bind(fd,(struct sockaddr *)&addr,offsetof(struct sockaddr_un,sun_path)+strlen(addr.sun_path)+1) || chmod(argv[1],0660) || listen(fd,4)) return 2;
  for (i=0;i<2;i++) {
    ssize_t n; char *start,*end; size_t nonce_len;
    if ((client=accept(fd,NULL,NULL)) < 0 || (n=read(client,request,sizeof(request)-1)) <= 0) return 2;
    request[n]=0; start=strstr(request,"\"nonce\":\""); if (!start) return 2; start+=9;
    end=strchr(start,'\"'); if (!end || (nonce_len=(size_t)(end-start)) > 128) return 2;
    n=snprintf(response,sizeof(response),"{\"ready\":true,\"version\":\"dirextalk.extension.runner.probe.v1\",\"nonce\":\"%.*s\"}",(int)nonce_len,start);
    if (write(client,response,(size_t)n) != n) return 2; close(client);
  }
  close(fd); unlink(argv[1]); return 0;
}
EOF
gcc -static -O2 -o "$tmp/probe-server" "$tmp/probe-server.c"
docker run -d --name "$server_container" --user 65531:65531 --network none \
  -v "$extension_socket_volume:/run/dirextalk-agent" -v "$tmp/probe-server:/probe-server:ro" \
  --entrypoint /probe-server "$utility_image" /run/dirextalk-agent/extension-runner.sock >/dev/null
for _ in $(seq 1 50); do
  socket_meta=$(docker run --rm --network none -v "$extension_socket_volume:/socket" "$utility_image" \
    sh -c 'stat -c "%u:%g:%a" /socket/extension-runner.sock 2>/dev/null' || true)
  [ "$socket_meta" = 65531:65532:660 ] && break
  sleep 0.1
done
[ "${socket_meta:-}" = 65531:65532:660 ] || { echo "runner socket produced ${socket_meta:-missing}" >&2; exit 1; }
for uid in 65531 65532; do
  docker run --rm --user "$uid:$uid" --network none -v "$extension_socket_volume:/run/dirextalk-agent" \
    --entrypoint /usr/local/bin/dirextalk-extension-runner "$agent_image" \
    probe --socket /run/dirextalk-agent/extension-runner.sock --runner-uid 65531
done
docker wait "$server_container" >/dev/null
[ "$(docker inspect -f '{{.State.ExitCode}}' "$server_container")" -eq 0 ]
printf 'real Docker fresh socket nocopy and runner health consumer passed\n'
