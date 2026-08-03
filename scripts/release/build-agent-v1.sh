#!/usr/bin/env bash
# Build and verify the local Agent-v1 image. This script never publishes.
set -euo pipefail

readonly IMAGE='dirextalk/message-server:agent-v1'
readonly VARIANT='agent-v1'
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$repo_root"

for tool in git docker; do
  command -v "$tool" >/dev/null 2>&1 || {
    printf 'agent-v1 build requires %s\n' "$tool" >&2
    exit 1
  }
done

if [[ -n "$(git status --porcelain)" ]]; then
  printf 'agent-v1 build requires a clean working tree so its labels identify the build context\n' >&2
  exit 1
fi

commit="$(git rev-parse HEAD)"
build_time="$(git show -s --format=%cI HEAD)"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || {
  printf 'agent-v1 build could not resolve a full commit id\n' >&2
  exit 1
}

docker build --no-cache --pull \
  --build-arg "VERSION=$VARIANT" \
  --build-arg "COMMIT=$commit" \
  --build-arg "BUILD_TIME=$build_time" \
  --label "org.opencontainers.image.version=$VARIANT" \
  --label "org.opencontainers.image.revision=$commit" \
  --label "org.opencontainers.image.created=$build_time" \
  --label "ai.dirextalk.variant=$VARIANT" \
  --tag "$IMAGE" .

identity="$(docker image inspect "$IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{index .Config.Labels "org.opencontainers.image.created"}}|{{index .Config.Labels "ai.dirextalk.variant"}}')"
[[ "$identity" == "$VARIANT|$commit|$build_time|$VARIANT" ]] || {
  printf 'agent-v1 image labels do not match the checked-out commit\n' >&2
  exit 1
}

version="$(docker run --rm --entrypoint /usr/bin/dirextalk-message-server "$IMAGE" --version)"
[[ "$version" == "$VARIANT" ]] || {
  printf 'agent-v1 server version probe returned %s\n' "$version" >&2
  exit 1
}
docker run --rm --entrypoint /bin/sh "$IMAGE" -ec \
  'test -x /usr/bin/dirextalk-message-server && test -x /usr/bin/agent-secretctl'

image_id="$(docker image inspect "$IMAGE" --format '{{.Id}}')"
printf 'agent-v1 build verified: image=%s id=%s revision=%s created=%s\n' \
  "$IMAGE" "$image_id" "$commit" "$build_time"
printf 'not published; for deployment set MESSAGE_SERVER_IMAGE=%s, preferably to a pushed immutable digest\n' "$IMAGE"
