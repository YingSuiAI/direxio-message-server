#!/usr/bin/env bash
set -euo pipefail
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

release_init "$@"
release_preflight
release_require_context "$RELEASE_VERSION"
release_require_tools docker gh
release_require_verified
cd "$RELEASE_REPO_ROOT"

repository=YingSuiAI/dirextalk-message-server
latest_image=dirextalk/message-server:latest
notes_file=$RELEASE_OUTPUT_DIR/release-notes.md
release_write_notes "$notes_file"

verify_image() {
  local ref=$1 identity probe
  identity=$(docker image inspect "$ref" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}')
  [[ "$identity" == "$RELEASE_VERSION|$RELEASE_COMMIT" ]] || release_die 'published image identity is incorrect'
  probe=$(docker run --rm --entrypoint /usr/bin/dirextalk-message-server "$ref" --version)
  [[ "$probe" == "$RELEASE_VERSION" ]] || release_die 'published binary reports a different version'
}

docker buildx build \
  --pull \
  --platform linux/amd64 \
  --push \
  --build-arg "VERSION=$RELEASE_VERSION" \
  --build-arg "COMMIT=$RELEASE_COMMIT" \
  --build-arg "BUILD_TIME=$RELEASE_BUILD_TIME" \
  --tag "$RELEASE_IMAGE" .

docker pull --platform linux/amd64 "$RELEASE_IMAGE" >/dev/null
verify_image "$RELEASE_IMAGE"

if [[ -z "$(git tag --list "$RELEASE_VERSION")" ]]; then
  git tag -a "$RELEASE_VERSION" -m "Dirextalk Message Server $RELEASE_VERSION"
fi
[[ "$(git rev-list -n 1 "$RELEASE_VERSION")" == "$RELEASE_COMMIT" ]] || \
  release_die 'release tag points to another commit'
if ! git ls-remote --exit-code --tags origin "refs/tags/$RELEASE_VERSION" >/dev/null 2>&1; then
  git push origin "refs/tags/$RELEASE_VERSION"
fi
remote_tag=$(git ls-remote --exit-code --tags origin "refs/tags/$RELEASE_VERSION^{}")
[[ "$remote_tag" == "$RELEASE_COMMIT"$'\t'"refs/tags/$RELEASE_VERSION^{}" ]] || \
  release_die 'remote release tag points to another commit'

if ! gh release view "$RELEASE_VERSION" --repo "$repository" >/dev/null 2>&1; then
  gh release create "$RELEASE_VERSION" \
    --repo "$repository" \
    --title "Dirextalk Message Server $RELEASE_VERSION" \
    --notes-file "$notes_file" \
    --verify-tag
fi

docker buildx imagetools create --tag "$latest_image" "$RELEASE_IMAGE"
docker pull --platform linux/amd64 "$latest_image" >/dev/null
verify_image "$latest_image"

printf 'release publish passed for %s\n' "$RELEASE_VERSION"
