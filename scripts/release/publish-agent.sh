#!/usr/bin/env bash
set -euo pipefail
# Resolved from this installed script directory.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/agent-lib.sh"
agent_release_init "$@"
agent_release_require_tools docker python3
agent_release_source_identity
agent_release_require_json "$AGENT_RELEASE_CONTEXT" prepared no
agent_release_require_json "$AGENT_RELEASE_VERIFIED" verified yes
[ "$(docker image inspect "$AGENT_RELEASE_IMAGE" --format '{{.Id}}')" = "${AGENT_RELEASE_EVIDENCE[4]}" ] || agent_release_die 'local Agent image changed after verification'
agent_release_verify_image "$AGENT_RELEASE_IMAGE"
agent_release_export_context
trap 'rm -rf "$AGENT_RELEASE_BUILD_CONTEXT"' EXIT
release_oci_probe_index agent_release_die "$AGENT_RELEASE_IMAGE"
if [ "$RELEASE_OCI_INDEX_EXISTS" = 1 ]; then
  version_digest=$RELEASE_OCI_INDEX_DIGEST
  version_config_digest=$RELEASE_OCI_PLATFORM_CONFIG_DIGEST
else
  buildx_metadata=$AGENT_RELEASE_OUTPUT/buildx-metadata.json
  rm -f "$buildx_metadata"
  docker buildx build \
    --pull \
    --platform linux/amd64 \
    --provenance=mode=max \
    --sbom=true \
    --push \
    --build-arg "VERSION=$AGENT_RELEASE_VERSION" \
    --build-arg "REVISION=$AGENT_RELEASE_COMMIT" \
    --build-arg "BUILD_TIME=$AGENT_RELEASE_BUILD_TIME" \
    --tag "$AGENT_RELEASE_IMAGE" \
    --metadata-file "$buildx_metadata" \
    --file "$AGENT_RELEASE_BUILD_CONTEXT/deploy/container/agent.Containerfile" \
    "$AGENT_RELEASE_BUILD_CONTEXT"
  built_digest=$(release_oci_buildx_metadata_digest agent_release_die "$buildx_metadata" Agent)
  release_oci_probe_index agent_release_die "$AGENT_RELEASE_IMAGE"
  [ "$RELEASE_OCI_INDEX_EXISTS" = 1 ] || agent_release_die 'published Agent version OCI index is unavailable'
  version_digest=$RELEASE_OCI_INDEX_DIGEST
  version_config_digest=$RELEASE_OCI_PLATFORM_CONFIG_DIGEST
  [ "$built_digest" = "$version_digest" ] || agent_release_die 'buildx and registry Agent OCI index digests differ'
fi
version_immutable_ref="dirextalk/agent@$version_digest"
docker pull --platform linux/amd64 "$version_immutable_ref" >/dev/null
agent_release_verify_image "$version_immutable_ref"
pulled_image_id=$(docker image inspect "$version_immutable_ref" --format '{{.Id}}')
[ "$pulled_image_id" = "$version_config_digest" ] || agent_release_die 'pulled Agent image ID does not match remote config digest'
[ "$pulled_image_id" = "${AGENT_RELEASE_EVIDENCE[4]}" ] || agent_release_die 'remote Agent config does not match locally verified image'

latest_image=dirextalk/agent:latest
release_oci_probe_index agent_release_die "$latest_image"
if [ "$RELEASE_OCI_INDEX_EXISTS" = 1 ]; then latest_digest=$RELEASE_OCI_INDEX_DIGEST; else latest_digest=; fi
if [ "$latest_digest" != "$version_digest" ]; then
  if [ -n "$latest_digest" ]; then
    docker pull --platform linux/amd64 "dirextalk/agent@$latest_digest" >/dev/null
    latest_version=$(docker image inspect "dirextalk/agent@$latest_digest" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')
    comparison=$(release_oci_compare_versions agent_release_die "$latest_version" "$AGENT_RELEASE_VERSION" latest)
    [ "$comparison" -lt 0 ] || agent_release_die 'Agent latest points to the same or a newer version with a different digest'
  fi
  [ "$(agent_release_remote_index_digest "$AGENT_RELEASE_IMAGE")" = "$version_digest" ] || agent_release_die 'Agent version index changed before latest publication'
  docker buildx imagetools create --tag "$latest_image" "$version_immutable_ref"
fi
release_oci_probe_index agent_release_die "$latest_image"
[ "$RELEASE_OCI_INDEX_EXISTS" = 1 ] && [ "$RELEASE_OCI_INDEX_DIGEST" = "$version_digest" ] || agent_release_die 'Agent version and latest tags resolve to different OCI indexes'
[ "$RELEASE_OCI_PLATFORM_CONFIG_DIGEST" = "$version_config_digest" ] || agent_release_die 'Agent latest config digest differs from version'
docker pull --platform linux/amd64 "$version_immutable_ref" >/dev/null
agent_release_verify_image "$version_immutable_ref"
[ "$(docker image inspect "$version_immutable_ref" --format '{{.Id}}')" = "${AGENT_RELEASE_EVIDENCE[4]}" ] || agent_release_die 'Agent latest content differs from locally verified image'
[ "$(agent_release_remote_index_digest "$AGENT_RELEASE_IMAGE")" = "$version_digest" ] || agent_release_die 'Agent version index changed during publication'
[ "$(agent_release_remote_index_digest "$latest_image")" = "$version_digest" ] || agent_release_die 'Agent latest index changed during publication'

printf 'Agent release publish passed: version=%s digest=%s\n' \
  "$AGENT_RELEASE_VERSION" "$version_digest"
