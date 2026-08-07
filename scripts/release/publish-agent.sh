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
docker push "$AGENT_RELEASE_IMAGE"
docker pull "$AGENT_RELEASE_IMAGE" >/dev/null
agent_release_verify_image "$AGENT_RELEASE_IMAGE"
[ "$(docker image inspect "$AGENT_RELEASE_IMAGE" --format '{{.Id}}')" = "${AGENT_RELEASE_EVIDENCE[4]}" ] || agent_release_die 'published Agent image changed after verification'
version_immutable=$(docker image inspect "$AGENT_RELEASE_IMAGE" --format '{{range .RepoDigests}}{{println .}}{{end}}' | awk '$0 ~ /^dirextalk\/agent@sha256:[0-9a-f]{64}$/ {print; exit}')
[ -n "$version_immutable" ] || agent_release_die 'published Agent version image has no immutable Docker Hub digest'

latest_image=dirextalk/agent:latest
docker tag "$AGENT_RELEASE_IMAGE" "$latest_image"
docker push "$latest_image"
docker pull "$latest_image" >/dev/null
agent_release_verify_image "$latest_image"
latest_immutable=$(docker image inspect "$latest_image" --format '{{range .RepoDigests}}{{println .}}{{end}}' | awk '$0 ~ /^dirextalk\/agent@sha256:[0-9a-f]{64}$/ {print; exit}')
[ -n "$latest_immutable" ] || agent_release_die 'published Agent latest image has no immutable Docker Hub digest'
[ "$latest_immutable" = "$version_immutable" ] || agent_release_die 'Agent version and latest tags resolve to different Docker Hub digests'

printf 'Agent release publish passed: version=%s immutable=%s latest=%s\n' \
  "$AGENT_RELEASE_VERSION" "$version_immutable" "$latest_immutable"
