#!/usr/bin/env bash
set -euo pipefail
# Resolved from this installed script directory.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/agent-lib.sh"
agent_release_init "$@"
agent_release_require_tools docker python3
agent_release_source_identity
agent_release_require_json "$AGENT_RELEASE_CONTEXT" prepared no
agent_release_export_context
trap 'rm -rf "$AGENT_RELEASE_BUILD_CONTEXT"' EXIT
docker build --pull \
  --build-arg "VERSION=$AGENT_RELEASE_VERSION" \
  --build-arg "REVISION=$AGENT_RELEASE_COMMIT" \
  --label "org.opencontainers.image.created=$AGENT_RELEASE_BUILD_TIME" \
  --tag "$AGENT_RELEASE_IMAGE" \
  --file "$AGENT_RELEASE_BUILD_CONTEXT/deploy/container/agent.Containerfile" \
  "$AGENT_RELEASE_BUILD_CONTEXT"
agent_release_verify_image "$AGENT_RELEASE_IMAGE"
image_id=$(docker image inspect "$AGENT_RELEASE_IMAGE" --format '{{.Id}}')
printf '%s\n' "$image_id" | grep -Eq '^sha256:[0-9a-f]{64}$' || agent_release_die 'Agent image ID is invalid'
agent_release_write_json "$AGENT_RELEASE_VERIFIED" verified "$image_id"
printf 'Agent release verify passed for %s (%s)\n' "$AGENT_RELEASE_VERSION" "$image_id"
