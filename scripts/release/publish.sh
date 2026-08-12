#!/usr/bin/env bash
set -euo pipefail
# Resolved from this installed script directory.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

release_init "$@"
release_preflight
release_require_context "$RELEASE_VERSION"
release_require_tools docker gh python3
release_require_verified
cd "$RELEASE_REPO_ROOT"

verify_image() {
  local ref="$1" identity probe
  identity="$(docker image inspect "$ref" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{index .Config.Labels "org.opencontainers.image.created"}}')"
  [[ "$identity" == "$RELEASE_VERSION|$RELEASE_COMMIT|$RELEASE_BUILD_TIME" ]] || release_die 'release image metadata does not match the verified release'
  probe="$(docker run --rm --entrypoint /usr/bin/dirextalk-message-server "$ref" --version)"
  [[ "$probe" == "$RELEASE_VERSION" ]] || release_die 'release image reports a different version'
}

verify_remote_platform_image() {
  local immutable_ref="$1" expected_config_digest="$2" image_id
  docker pull --platform linux/amd64 "$immutable_ref" >/dev/null || \
    release_die "could not pull remote linux/amd64 image: $immutable_ref"
  verify_image "$immutable_ref"
  image_id="$(docker image inspect "$immutable_ref" --format '{{.Id}}')" || \
    release_die 'could not inspect pulled remote image ID'
  [[ "$image_id" == "$expected_config_digest" ]] || \
    release_die 'pulled image ID does not match the remote linux/amd64 config digest'
  [[ "$image_id" == "$RELEASE_VERIFIED_IMAGE_ID" ]] || \
    release_die 'remote linux/amd64 config does not match the locally verified image'
}

remote_image_version() {
  local immutable_ref="$1" version
  docker pull --platform linux/amd64 "$immutable_ref" >/dev/null || \
    release_die "could not pull existing remote image: $immutable_ref"
  version="$(docker image inspect "$immutable_ref" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')" || \
    release_die 'could not inspect existing remote image version'
  [[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
    release_die 'existing latest image has no canonical version label'
  printf '%s\n' "$version"
}

formal_release_exists() {
  gh release view "$1" --repo YingSuiAI/dirextalk-message-server >/dev/null 2>&1
}

assert_formal_release() {
  local tag="$1" notes_file="$2" metadata_file
  metadata_file="$RELEASE_OUTPUT_DIR/github-release.json"
  gh release view "$tag" \
    --repo YingSuiAI/dirextalk-message-server \
    --json tagName,name,body,isDraft,isPrerelease,assets >"$metadata_file"
  python3 - "$metadata_file" "$tag" "Dirextalk Message Server $tag" "$notes_file" <<'PY'
import json, pathlib, sys

metadata_path, expected_tag, expected_title, notes_path = sys.argv[1:]
try:
    metadata = json.loads(pathlib.Path(metadata_path).read_text(encoding="utf-8"))
except Exception as exc:
    raise SystemExit(f"invalid GitHub Release metadata: {exc}")

expected_notes = pathlib.Path(notes_path).read_text(encoding="utf-8")
if metadata.get("tagName") != expected_tag:
    raise SystemExit("GitHub Release is bound to another tag")
if metadata.get("isDraft") is not False or metadata.get("isPrerelease") is not False:
    raise SystemExit("GitHub Release must be formal")
if metadata.get("name") != expected_title:
    raise SystemExit("GitHub Release title does not match the checked-in contract")
if metadata.get("body") != expected_notes:
    raise SystemExit("GitHub Release notes do not match the checked-in contract")
assets = metadata.get("assets")
if not isinstance(assets, list) or assets:
    raise SystemExit("GitHub Release must not contain assets")
PY
}

write_expected_release_notes() {
  local notes_file="$1"
  python3 - release/RELEASE_NOTES.md "$RELEASE_VERSION" "$notes_file" <<'PY'
import pathlib, re, sys
source, version, destination = sys.argv[1:]
text = pathlib.Path(source).read_text(encoding="utf-8")
match = re.search(rf"(?ms)^##[ \t]+{re.escape(version)}[ \t]*\n.*?(?=^##[ \t]+v|\Z)", text)
if not match:
    raise SystemExit("release notes section is missing")
pathlib.Path(destination).write_text(match.group(0).rstrip() + "\n", encoding="utf-8", newline="\n")
PY
}

validate_remote_tag() {
  local output line hash ref direct="" peeled=""
  output="$(git ls-remote --tags origin "refs/tags/$RELEASE_VERSION" "refs/tags/$RELEASE_VERSION^{}")"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    [[ "$line" =~ ^([0-9a-f]{40})[[:space:]](.+)$ ]] || release_die 'remote release tag response is invalid'
    hash="${BASH_REMATCH[1]}"
    ref="${BASH_REMATCH[2]}"
    if [[ "$ref" == "refs/tags/$RELEASE_VERSION" ]]; then
      [[ -z "$direct" ]] || release_die 'remote release tag response contains duplicates'
      direct="$hash"
    elif [[ "$ref" == "refs/tags/$RELEASE_VERSION^{}" ]]; then
      [[ -z "$peeled" ]] || release_die 'remote release tag response contains duplicates'
      peeled="$hash"
    else
      release_die 'remote release tag response is invalid'
    fi
  done <<<"$output"

  if [[ -z "$direct" && -z "$peeled" ]]; then
    REMOTE_TAG_EXISTS=0
    return
  fi
  [[ -n "$direct" && -n "$peeled" ]] || release_die 'remote release tag must be annotated'
  [[ "$peeled" == "$RELEASE_COMMIT" ]] || release_die 'remote release tag already points to another commit'
  REMOTE_TAG_EXISTS=1
}

notes_file="$RELEASE_OUTPUT_DIR/release-notes.md"
write_expected_release_notes "$notes_file"

existing_tag="$(git tag --list "$RELEASE_VERSION")"
if [[ -n "$existing_tag" ]]; then
  [[ "$(git rev-list -n 1 "$RELEASE_VERSION")" == "$RELEASE_COMMIT" ]] || release_die 'release tag already points to another commit'
  [[ "$(git cat-file -t "$RELEASE_VERSION")" == tag ]] || release_die 'release tag must be annotated'
fi
validate_remote_tag

if [[ "$REMOTE_TAG_EXISTS" == 0 && -z "$existing_tag" ]]; then
  git var GIT_COMMITTER_IDENT >/dev/null 2>&1 || release_die 'annotated tag creation requires a valid Git committer identity'
fi

if formal_release_exists "$RELEASE_VERSION"; then
  [[ "$REMOTE_TAG_EXISTS" == 1 ]] || release_die 'existing GitHub Release requires its remote annotated tag'
  assert_formal_release "$RELEASE_VERSION" "$notes_file"
fi

verify_image "$RELEASE_IMAGE"
[[ "$(docker image inspect "$RELEASE_IMAGE" --format '{{.Id}}')" == "$RELEASE_VERIFIED_IMAGE_ID" ]] || \
  release_die 'local release image changed after verification'
release_probe_remote_index "$RELEASE_IMAGE"
if [[ "$RELEASE_OCI_INDEX_EXISTS" == 1 ]]; then
  version_digest="$RELEASE_OCI_INDEX_DIGEST"
  version_config_digest="$RELEASE_OCI_PLATFORM_CONFIG_DIGEST"
else
  buildx_metadata="$RELEASE_OUTPUT_DIR/buildx-metadata.json"
  rm -f "$buildx_metadata"
  docker buildx build \
    --platform linux/amd64 \
    --provenance=mode=max \
    --sbom=true \
    --push \
    --build-arg "VERSION=$RELEASE_VERSION" \
    --build-arg "COMMIT=$RELEASE_COMMIT" \
    --build-arg "BUILD_TIME=$RELEASE_BUILD_TIME" \
    --label "org.opencontainers.image.version=$RELEASE_VERSION" \
    --label "org.opencontainers.image.revision=$RELEASE_COMMIT" \
    --label "org.opencontainers.image.created=$RELEASE_BUILD_TIME" \
    --tag "$RELEASE_IMAGE" \
    --metadata-file "$buildx_metadata" .
  built_digest="$(release_oci_buildx_metadata_digest release_die "$buildx_metadata" version)" || \
    release_die 'version buildx digest evidence is invalid'
  release_probe_remote_index "$RELEASE_IMAGE"
  [[ "$RELEASE_OCI_INDEX_EXISTS" == 1 ]] || release_die 'published version OCI index is unavailable'
  version_digest="$RELEASE_OCI_INDEX_DIGEST"
  version_config_digest="$RELEASE_OCI_PLATFORM_CONFIG_DIGEST"
  [[ "$built_digest" == "$version_digest" ]] || \
    release_die 'buildx and registry version OCI index digests differ'
fi
version_immutable_ref="dirextalk/message-server@$version_digest"
verify_remote_platform_image "$version_immutable_ref" "$version_config_digest"

if [[ "$REMOTE_TAG_EXISTS" == 0 ]]; then
  if [[ -z "$existing_tag" ]]; then
    git tag -a "$RELEASE_VERSION" -m "Dirextalk Message Server $RELEASE_VERSION"
  fi
  git push origin "refs/tags/$RELEASE_VERSION"
fi
validate_remote_tag
[[ "$REMOTE_TAG_EXISTS" == 1 ]] || release_die 'remote release tag is missing after publication'

if formal_release_exists "$RELEASE_VERSION"; then
  assert_formal_release "$RELEASE_VERSION" "$notes_file"
else
  gh release create "$RELEASE_VERSION" \
    --repo YingSuiAI/dirextalk-message-server \
    --title "Dirextalk Message Server $RELEASE_VERSION" \
    --notes-file "$notes_file" \
    --verify-tag
fi
assert_formal_release "$RELEASE_VERSION" "$notes_file"

latest_image=dirextalk/message-server:latest
release_probe_remote_index "$latest_image"
if [[ "$RELEASE_OCI_INDEX_EXISTS" == 1 ]]; then
  latest_digest="$RELEASE_OCI_INDEX_DIGEST"
  if [[ "$latest_digest" != "$version_digest" ]]; then
    latest_version="$(remote_image_version "dirextalk/message-server@$latest_digest")"
    comparison="$(release_oci_compare_versions release_die "$latest_version" "$RELEASE_VERSION" latest)"
    [[ "$comparison" -lt 0 ]] || \
      release_die 'latest points to the same or a newer version with a different digest'
  fi
else
  latest_digest=
fi

if [[ "$latest_digest" != "$version_digest" ]]; then
  pre_promotion_version_digest="$(release_remote_index_digest "$RELEASE_IMAGE")"
  [[ "$pre_promotion_version_digest" == "$version_digest" ]] || \
    release_die 'version OCI index changed before latest publication'
  docker buildx imagetools create \
    --tag "$latest_image" \
    "$version_immutable_ref"
fi
release_probe_remote_index "$latest_image"
[[ "$RELEASE_OCI_INDEX_EXISTS" == 1 && "$RELEASE_OCI_INDEX_DIGEST" == "$version_digest" ]] || \
  release_die 'version and latest tags resolve to different OCI indexes'
latest_config_digest="$RELEASE_OCI_PLATFORM_CONFIG_DIGEST"
verify_remote_platform_image "dirextalk/message-server@$version_digest" "$latest_config_digest"
final_version_digest="$(release_remote_index_digest "$RELEASE_IMAGE")"
final_latest_digest="$(release_remote_index_digest "$latest_image")"
[[ "$final_version_digest" == "$version_digest" && "$final_latest_digest" == "$version_digest" ]] || \
  release_die 'version or latest OCI index changed during publication'

printf 'release publish passed for %s (%s)\n' "$RELEASE_VERSION" "$version_digest"
