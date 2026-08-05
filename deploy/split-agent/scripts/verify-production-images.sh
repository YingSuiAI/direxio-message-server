#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ENV_FILE ATTESTATION_FILE" >&2
  exit 2
}

die() {
  echo "production image gate: $*" >&2
  exit 1
}

[ "$#" -eq 2 ] || usage
env_file=$1
attestation_file=$2
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "environment file must be a regular non-symlink file"
[ -f "$attestation_file" ] && [ ! -L "$attestation_file" ] || die "attestation file must be a regular non-symlink file"
[ "$(stat -c '%a' "$env_file")" = 400 ] || die "environment file must be mode 0400"
[ "$(stat -c '%a' "$attestation_file")" = 400 ] || die "attestation file must be mode 0400"

read_env_value() {
  local key=$1 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$env_file")
  [ "$count" -eq 1 ] || die "environment file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$env_file")
  [ -n "$value" ] || die "$key is empty"
  printf '%s' "$value"
}

read_attestation_value() {
  local key=$1 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$attestation_file")
  [ "$count" -eq 1 ] || die "attestation must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$attestation_file")
  [ -n "$value" ] || die "$key is empty"
  printf '%s' "$value"
}

validate_image_ref() {
  local name=$1 value=$2 digest repository canonical
  printf '%s\n' "$value" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' || die "$name must be a lowercase immutable digest reference"
  case "$value" in
    *registry.invalid*|*0000000000000000000000000000000000000000000000000000000000000000) die "$name is a placeholder or untrusted registry reference" ;;
  esac
  repository=${value%%@sha256:*}
  case "$repository" in
    docker.io/*) canonical=${repository#docker.io/} ;;
    */*)
      case "${repository%%/*}" in
        *.*|*:*|localhost) die "$name must use Docker Hub (docker.io or an implicit Docker Hub reference)" ;;
        *) canonical=$repository ;;
      esac
      ;;
    *) canonical="library/$repository" ;;
  esac
  canonical=${canonical%:*}
  case "$name:$canonical" in
    DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE:dirextalk/message-server|\
    DIREXTALK_AGENT_IMAGE_IMMUTABLE:dirextalk/agent|\
    DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:library/postgres|\
    DIREXTALK_QDRANT_IMAGE_IMMUTABLE:qdrant/qdrant|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:library/postgres|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:library/alpine|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:library/busybox|\
    DIREXTALK_UTILITY_IMAGE_IMMUTABLE:dirextalk/utility|\
    DIREXTALK_CADDY_IMAGE_IMMUTABLE:library/caddy) ;;
    *) die "$name is not an approved public Docker Hub repository" ;;
  esac
  digest=${value##*@sha256:}
  [ "${digest//0/}" ] || die "$name uses an all-zero digest"
}

image_keys=(
  DIREXTALK_POSTGRES_IMAGE_IMMUTABLE
  DIREXTALK_UTILITY_IMAGE_IMMUTABLE
  DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE
  DIREXTALK_AGENT_IMAGE_IMMUTABLE
  DIREXTALK_QDRANT_IMAGE_IMMUTABLE
)

[ "$(sed -n '1p' "$attestation_file")" = '# dirextalk-image-attestation-v2' ] || die "attestation header is invalid"
attestation_version=$(read_attestation_value capability_api_version)
[ "$attestation_version" = v1.0.3 ] || die "unsupported capability API attestation version: $attestation_version"
message_source_revision=$(read_attestation_value message_source_revision)
agent_source_revision=$(read_attestation_value agent_source_revision)
printf '%s\n' "$message_source_revision" | grep -Eq '^[0-9a-f]{40}$' || die "message_source_revision must be a full lowercase Git commit"
printf '%s\n' "$agent_source_revision" | grep -Eq '^[0-9a-f]{40}$' || die "agent_source_revision must be a full lowercase Git commit"
capability_api_source=$(read_attestation_value capability_api_source)
case "$capability_api_source" in
  published) ;;
  local-relative-replace)
    die "local relative replace cannot pass the production image gate; published capability-api v1.0.3 is required" ;;
  *)
    die "capability_api_source must be published (local development is not a production release)" ;;
esac

for key in "${image_keys[@]}"; do
  env_value=$(read_env_value "$key")
  validate_image_ref "$key" "$env_value"
  attestation_value=$(read_attestation_value "image.$key")
  [ "$attestation_value" = "$env_value" ] || die "$key does not match its signed image attestation"
done

awk -F= '
  NR == 1 && $0 == "# dirextalk-image-attestation-v1" { next }
  /^[[:space:]]*#/ { next }
  /^[[:space:]]*$/ { next }
  $1 !~ /^(capability_api_version|message_source_revision|agent_source_revision|capability_api_source|image\.DIREXTALK_(POSTGRES|UTILITY|MESSAGE_SERVER|AGENT|QDRANT)_IMAGE_IMMUTABLE)$/ { exit 1 }
' "$attestation_file" || die "attestation contains an unknown or malformed field"

agent_image=$(read_env_value DIREXTALK_AGENT_IMAGE_IMMUTABLE)
message_image=$(read_env_value DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE)
if agent_revision=$(docker image inspect "$agent_image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null); then
  :
else
  die "Agent image metadata inspection failed"
fi
[ "$agent_revision" = "$agent_source_revision" ] || die "Agent image revision label does not match the attested Agent source revision"
if message_revision=$(docker image inspect "$message_image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null); then
  :
else
  die "message-server image metadata inspection failed"
fi
[ "$message_revision" = "$message_source_revision" ] || die "message-server image revision label does not match the attested message-server source revision"
if message_user=$(docker image inspect "$message_image" --format '{{.Config.User}}' 2>/dev/null); then
  :
else
  die "message-server image runtime-user inspection failed"
fi
case "$message_user" in
  ''|0|0:0|0:) ;;
  *) die "message-server image must use UID 0 to read the protected runtime secrets" ;;
esac

smoke_agent_binary() {
  local binary=$1 expected_status=$2 usage_marker=$3 output status
  if output=$(docker run --rm --entrypoint "$binary" "$agent_image" 2>&1); then
    status=0
  else
    status=$?
  fi
  if [ "$status" -eq "$expected_status" ] && printf '%s' "$output" | grep -Fq "$usage_marker"; then
    return 0
  fi
  if [ "$status" -eq 125 ]; then
    die "$binary smoke infrastructure failure (docker run status 125)"
  fi
  die "$binary smoke failed"
}

smoke_agent_binary /usr/local/bin/dirextalk-agent 1 'usage: dirextalk-agent'
smoke_agent_binary /usr/local/bin/dirextalk-extension-runner 2 'usage: dirextalk-extension-runner'
smoke_agent_binary /usr/local/bin/dirextalk-core-runner 2 'usage: dirextalk-core-runner'

printf 'production immutable image, attestation, and Agent runtime smoke checks passed\n'
