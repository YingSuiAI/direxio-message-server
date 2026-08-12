#!/usr/bin/env bash
set -euo pipefail
# Resolved from this installed script directory.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

release_init "$@"
release_preflight
release_require_context "$RELEASE_VERSION"
release_require_tools go docker
cd "$RELEASE_REPO_ROOT"

go test ./internal/releasecontrol ./internal/httputil ./setup ./p2p ./internal/productpolicy -count=1
go test -tags=dendrite_upgrade_tests ./cmd/dendrite-upgrade-tests -count=1
go build ./cmd/dirextalk-message-server

docker build \
  --build-arg "VERSION=$RELEASE_VERSION" \
  --build-arg "COMMIT=$RELEASE_COMMIT" \
  --build-arg "BUILD_TIME=$RELEASE_BUILD_TIME" \
  --label "org.opencontainers.image.version=$RELEASE_VERSION" \
  --label "org.opencontainers.image.revision=$RELEASE_COMMIT" \
  --label "org.opencontainers.image.created=$RELEASE_BUILD_TIME" \
  --tag "$RELEASE_IMAGE" .

identity="$(docker image inspect "$RELEASE_IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{index .Config.Labels "org.opencontainers.image.created"}}')"
[[ "$identity" == "$RELEASE_VERSION|$RELEASE_COMMIT|$RELEASE_BUILD_TIME" ]] || release_die 'local image metadata does not match the verified release'
probe="$(docker run --rm --entrypoint /usr/bin/dirextalk-message-server "$RELEASE_IMAGE" --version)"
[[ "$probe" == "$RELEASE_VERSION" ]] || release_die "image version probe returned $probe"
docker compose -f docker-compose.p2p.yml config >/dev/null
release_write_verified
printf 'release verify passed for %s\n' "$RELEASE_VERSION"
