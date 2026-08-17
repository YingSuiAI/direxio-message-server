#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ENV_FILE [--running]" >&2
  exit 2
}

die() {
  echo "production image gate: $*" >&2
  exit 1
}

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage
env_file=$1
mode=${2:-images}
case "$mode" in images|--running) ;; *) usage ;; esac
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "environment file must be a regular non-symlink file"
[ "$(stat -c '%a' "$env_file")" = 400 ] || die "environment file must be mode 0400"

read_env_value() {
  local key=$1 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$env_file")
  [ "$count" -eq 1 ] || die "environment file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$env_file")
  [ -n "$value" ] || die "$key is empty"
  printf '%s' "$value"
}

validate_digest_image() {
  local name=$1 value=$2 repository
  printf '%s\n' "$value" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' || die "$name must be a digest reference"
  repository=${value%%@sha256:*}
  case "$name:$repository" in
    DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:docker.io/pgvector/pgvector:pg18|\
    DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:pgvector/pgvector:pg18|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:docker.io/pgvector/pgvector:pg18|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:pgvector/pgvector:pg18|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:docker.io/library/postgres:*|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:docker.io/library/alpine:*|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:docker.io/library/busybox:*|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:docker.io/dirextalk/utility:*|\
    DIREXTALK_COTURN_IMAGE_IMMUTABLE:docker.io/coturn/coturn:*|\
    DIREXTALK_COTURN_IMAGE_IMMUTABLE:coturn/coturn:*) ;;
    *) die "$name is not an approved Docker Hub image" ;;
  esac
}

for key in DIREXTALK_POSTGRES_IMAGE_IMMUTABLE DIREXTALK_UTILITY_IMAGE_IMMUTABLE DIREXTALK_COTURN_IMAGE_IMMUTABLE; do
  validate_digest_image "$key" "$(read_env_value "$key")"
done

message_image=$(read_env_value DIREXTALK_MESSAGE_SERVER_IMAGE)
agent_image=$(read_env_value DIREXTALK_AGENT_IMAGE)
message_version=$(read_env_value DIREXTALK_MESSAGE_SERVER_VERSION)
message_revision=$(read_env_value DIREXTALK_MESSAGE_SOURCE_REVISION)
agent_version=$(read_env_value DIREXTALK_AGENT_VERSION)
agent_revision=$(read_env_value DIREXTALK_AGENT_SOURCE_REVISION)
for version in "$message_version" "$agent_version"; do
  printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || die "expected application version is invalid"
done
[ "$message_image" = "docker.io/dirextalk/message-server:$message_version" ] || die "message-server image must match its prepared version tag"
[ "$agent_image" = "docker.io/dirextalk/agent:$agent_version" ] || die "Agent image must match its prepared version tag"
for revision in "$message_revision" "$agent_revision"; do
  printf '%s\n' "$revision" | grep -Eq '^[0-9a-f]{40}$' || die "expected application revision is invalid"
done

verify_image() {
  local image=$1 expected_version=$2 expected_revision=$3 binaries=$4 label binary probe
  label=$(docker image inspect "$image" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || \
    die "application image inspection failed: $image"
  [ "$label" = "$expected_version|$expected_revision" ] || die "application image version/revision mismatch: $image"
  for binary in $binaries; do
    probe=$(docker run --rm --entrypoint "$binary" "$image" --version 2>/dev/null) || die "application binary version probe failed: $binary"
    [ "$probe" = "$expected_version" ] || die "application binary version mismatch: $binary"
  done
}

verify_running() {
  local service=$1 expected_image=$2 expected_version=$3 expected_revision=$4 binary=$5 stack id data label probe
  stack=$(read_env_value DIREXTALK_SPLIT_STACK_NAME)
  id=$(docker ps -q --filter "label=com.docker.compose.project=$stack" --filter "label=com.docker.compose.service=$service") || \
    die "$service container lookup failed"
  [ -n "$id" ] && [ "${id#*$'\n'}" = "$id" ] || die "$service does not have exactly one running container"
  data=$(docker inspect "$id" 2>/dev/null) || die "$service container inspection failed"
  [ "$(jq -r '.[0].Config.Image // empty' <<<"$data")" = "$expected_image" ] || die "$service container does not use the expected release channel"
  [ "$(jq -r '.[0].State.Health.Status // empty' <<<"$data")" = healthy ] || die "$service container is not healthy"
  label=$(docker image inspect "$(jq -r '.[0].Image // empty' <<<"$data")" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null) || \
    die "$service container image inspection failed"
  [ "$label" = "$expected_version|$expected_revision" ] || die "$service running image version/revision mismatch"
  probe=$(docker exec "$id" "$binary" --version 2>/dev/null) || die "$service running binary version probe failed"
  [ "$probe" = "$expected_version" ] || die "$service running binary version mismatch"
}

if [ "$mode" = images ]; then
  verify_image "$message_image" "$message_version" "$message_revision" /usr/bin/dirextalk-message-server
  verify_image "$agent_image" "$agent_version" "$agent_revision" "/usr/local/bin/dirextalk-agent /usr/local/bin/dirextalk-extension-runner /usr/local/bin/dirextalk-core-runner"
else
  command -v jq >/dev/null 2>&1 || die "jq is required for running-container verification"
  verify_running message-server "$message_image" "$message_version" "$message_revision" /usr/bin/dirextalk-message-server
  verify_running agent "$agent_image" "$agent_version" "$agent_revision" /usr/local/bin/dirextalk-agent
  verify_running extension-runner "$agent_image" "$agent_version" "$agent_revision" /usr/local/bin/dirextalk-extension-runner
  verify_running core-runner "$agent_image" "$agent_version" "$agent_revision" /usr/local/bin/dirextalk-core-runner
fi

printf 'production application release channels and runtime versions verified\n'
