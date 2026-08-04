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

cat >"$env_file" <<EOF
DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=docker.io/library/postgres:18@sha256:$digest
DIREXTALK_UTILITY_IMAGE_IMMUTABLE=docker.io/library/postgres:18@sha256:$digest
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=registry.example/dirextalk-message-server@sha256:$digest
DIREXTALK_AGENT_IMAGE_IMMUTABLE=registry.example/dirextalk-agent@sha256:$digest
DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE=registry.example/dirextalk-extension-runner@sha256:$digest
DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE=registry.example/dirextalk-core-runner@sha256:$digest
DIREXTALK_QDRANT_IMAGE_IMMUTABLE=qdrant/qdrant:v1.18.3@sha256:$digest
EOF
chmod 400 "$env_file"
{
  printf '%s\n' '# dirextalk-image-attestation-v1' 'capability_api_version=v1.0.1' 'capability_api_source=published' 'source_revision=test-revision'
  while IFS='=' read -r key value; do
    printf 'image.%s=%s\n' "$key" "$value"
  done <"$env_file"
} >"$attestation"
chmod 400 "$attestation"

"$script_dir/verify-production-images.sh" "$env_file" "$attestation" >/dev/null

sed -i 's#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=.*#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=registry.example/dirextalk-agent@sha256:2222222222222222222222222222222222222222222222222222222222222222#' "$attestation"
if "$script_dir/verify-production-images.sh" "$env_file" "$attestation" >/dev/null 2>&1; then
  echo "changed attestation unexpectedly accepted" >&2
  exit 1
fi

sed -i 's#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=.*#image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=registry.example/dirextalk-agent@sha256:'"$digest"'#; s/^capability_api_source=.*/capability_api_source=local-relative-replace/' "$attestation"
if output=$("$script_dir/verify-production-images.sh" "$env_file" "$attestation" 2>&1); then
  echo "local relative replace unexpectedly accepted" >&2
  exit 1
fi
printf '%s\n' "$output" | grep -Fq 'remote publication is pending'

printf 'production image attestation checks verified\n'
