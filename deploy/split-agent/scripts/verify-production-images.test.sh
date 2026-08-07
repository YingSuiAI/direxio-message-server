#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-image-gate.XXXXXX")
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

env_file=$tmp_dir/.env
attestation=$tmp_dir/attestation
digest=1111111111111111111111111111111111111111111111111111111111111111
message_revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
agent_revision=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

cat >"$env_file" <<EOF
DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=docker.io/pgvector/pgvector:pg18@sha256:$digest
DIREXTALK_UTILITY_IMAGE_IMMUTABLE=docker.io/library/postgres:18@sha256:$digest
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=docker.io/dirextalk/message-server@sha256:$digest
DIREXTALK_AGENT_IMAGE_IMMUTABLE=docker.io/dirextalk/agent@sha256:$digest
DIREXTALK_COTURN_IMAGE_IMMUTABLE=coturn/coturn:4.6.3-alpine@sha256:$digest
EOF
chmod 400 "$env_file"
{
  printf '%s\n' '# dirextalk-image-attestation-v2' 'capability_api_version=v1.0.3' 'capability_api_source=published' \
    "message_source_revision=$message_revision" "agent_source_revision=$agent_revision"
  while IFS='=' read -r key value; do
    printf 'image.%s=%s\n' "$key" "$value"
  done <"$env_file"
} >"$attestation"
chmod 400 "$attestation"

docker_bin=$tmp_dir/bin
mkdir -p "$docker_bin"
docker_log=$tmp_dir/docker.log
cat >"$docker_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_DOCKER_LOG"
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
  case "${3:-}" in
    *dirextalk/agent@*) printf '%s\n' "${FAKE_DOCKER_AGENT_REVISION:?}" ;;
    *dirextalk/message-server@*)
      case "${5:-}" in
        '{{.Config.User}}') printf '%s' "${FAKE_DOCKER_MESSAGE_USER:-}" ;;
        *) printf '%s\n' "${FAKE_DOCKER_MESSAGE_REVISION:?}" ;;
      esac
      ;;
    *) exit 98 ;;
  esac
  exit 0
fi
if [ "${1:-}" = run ]; then
  entrypoint=''
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --rm) shift ;;
      --entrypoint)
        entrypoint=$2
        shift 2
        ;;
      *)
        shift
        break
        ;;
    esac
  done
  if [ "${FAKE_DOCKER_MISSING_BINARY:-}" = "$entrypoint" ]; then
    exit 127
  fi
  if [ -n "${FAKE_DOCKER_SMOKE_STATUS:-}" ]; then
    exit "$FAKE_DOCKER_SMOKE_STATUS"
  fi
  case "$entrypoint" in
    /usr/local/bin/dirextalk-agent)
      printf '%s\n' 'usage: dirextalk-agent [--config PATH] <migrate|serve|healthcheck>' >&2
      exit 1
      ;;
    /usr/local/bin/dirextalk-extension-runner)
      printf '%s\n' 'usage: dirextalk-extension-runner serve --socket ... --agent-uid ... --install-root ... --workspace-root ... --cgroup-root ...' >&2
      exit 2
      ;;
    /usr/local/bin/dirextalk-core-runner)
      printf '%s\n' 'usage: dirextalk-core-runner serve --socket --agent-uid' >&2
      exit 2
      ;;
    *) exit 127 ;;
  esac
fi
exit 99
EOF
chmod +x "$docker_bin/docker"

run_gate() {
  local agent_revision_value=${1:-$agent_revision} message_revision_value=${2:-$message_revision}
  local missing_binary=${3:-} smoke_status=${4:-} message_user=${5:-}
  PATH="$docker_bin:$PATH" \
    FAKE_DOCKER_LOG="$docker_log" \
    FAKE_DOCKER_AGENT_REVISION="$agent_revision_value" \
    FAKE_DOCKER_MESSAGE_REVISION="$message_revision_value" \
    FAKE_DOCKER_MESSAGE_USER="$message_user" \
    FAKE_DOCKER_MISSING_BINARY="$missing_binary" \
    FAKE_DOCKER_SMOKE_STATUS="$smoke_status" \
    "$script_dir/verify-production-images.sh" "$env_file" "$attestation"
}

run_gate >/dev/null
grep -Fq -- 'image inspect docker.io/dirextalk/agent@sha256:' "$docker_log"
grep -Fq -- 'image inspect docker.io/dirextalk/message-server@sha256:' "$docker_log"
grep -Fq -- 'run --rm --entrypoint /usr/local/bin/dirextalk-agent docker.io/dirextalk/agent@sha256:' "$docker_log"
grep -Fq -- 'run --rm --entrypoint /usr/local/bin/dirextalk-extension-runner docker.io/dirextalk/agent@sha256:' "$docker_log"
grep -Fq -- 'run --rm --entrypoint /usr/local/bin/dirextalk-core-runner docker.io/dirextalk/agent@sha256:' "$docker_log"

sed -i 's#docker.io/pgvector/pgvector:pg18#docker.io/library/postgres:18#g' "$env_file" "$attestation"
if output=$(run_gate 2>&1); then
  echo "plain PostgreSQL image unexpectedly accepted for Agent Knowledge" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'must use the maintained pgvector/pgvector:pg18 image'
sed -i 's#docker.io/library/postgres:18#docker.io/pgvector/pgvector:pg18#g' "$env_file" "$attestation"

if output=$(run_gate "$agent_revision" "$message_revision" '' '' 65532 2>&1); then
  echo "non-root message-server image unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'must use UID 0'

if output=$(run_gate "$agent_revision" "$message_revision" /usr/local/bin/dirextalk-extension-runner 2>&1); then
  echo "missing Agent binary unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'smoke failed'

if output=$(run_gate "$agent_revision" "$message_revision" '' 0 2>&1); then
  echo "successful no-op Agent binary unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'smoke failed'

if output=$(run_gate "$agent_revision" "$message_revision" '' 125 2>&1); then
  echo "Docker smoke infrastructure failure unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'infrastructure failure'

if output=$(run_gate cccccccccccccccccccccccccccccccccccccccc "$message_revision" 2>&1); then
  echo "wrong Agent revision label unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'revision label does not match'

if output=$(run_gate "$agent_revision" dddddddddddddddddddddddddddddddddddddddd 2>&1); then
  echo "wrong message-server revision label unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'message-server image revision label does not match'

sed -i 's/^capability_api_version=.*/capability_api_version=v1.0.1/' "$attestation"
if output=$("$script_dir/verify-production-images.sh" "$env_file" "$attestation" 2>&1); then
  echo "old capability API version unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'unsupported capability API attestation version: v1.0.1'
sed -i 's/^capability_api_version=.*/capability_api_version=v1.0.3/' "$attestation"

sed -i 's#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=.*#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=docker.io/dirextalk/agent@sha256:2222222222222222222222222222222222222222222222222222222222222222#' "$attestation"
if "$script_dir/verify-production-images.sh" "$env_file" "$attestation" >/dev/null 2>&1; then
  echo "changed attestation unexpectedly accepted" >&2
  exit 1
fi

sed -i 's#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=.*#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=docker.io/dirextalk/agent@sha256:'"$digest"'#; s/^capability_api_source=.*/capability_api_source=local-relative-replace/' "$attestation"
if output=$("$script_dir/verify-production-images.sh" "$env_file" "$attestation" 2>&1); then
  echo "local relative replace unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'published capability-api v1.0.3 is required'

sed -i 's/^capability_api_source=.*/capability_api_source=published/; s#DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=.*#DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=registry.example/dirextalk-message-server@sha256:'"$digest"'#; s#image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=.*#image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=registry.example/dirextalk-message-server@sha256:'"$digest"'#' "$env_file" "$attestation"
if output=$(run_gate 2>&1); then
  echo "non-Docker-Hub image unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'Docker Hub'

printf 'production image attestation checks verified\n'
