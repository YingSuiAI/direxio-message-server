#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-agent-release.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/agent/deploy/container" "$tmp/docker-state" "$tmp/out"
: >"$tmp/agent/deploy/container/agent.Containerfile"
: >"$tmp/agent/.codex-final-overlay.Containerfile"
log=$tmp/commands.log
commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
image_id=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
digest=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
cat >"$tmp/bin/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\n' "$*" >>"$FAKE_RELEASE_LOG"
if [ "$1" = -C ]; then shift 2; fi
case "$1 ${2:-}" in
  'status --porcelain=v1') printf '%s' "${FAKE_AGENT_STATUS:-?? .codex-final-overlay.Containerfile}" ;;
  'branch --show-current') printf 'adam/agent-core-v1-integration\n' ;;
  'rev-parse HEAD') printf '%s\n' "$FAKE_AGENT_COMMIT" ;;
  'ls-remote --exit-code') printf '%s\trefs/heads/main\n' "$FAKE_AGENT_COMMIT" ;;
  'show -s') printf '2026-08-06T00:00:00+08:00\n' ;;
  'archive --format=tar') tar -C "$FAKE_AGENT_SOURCE" --exclude=.codex-final-overlay.Containerfile -cf - . ;;
  *) exit 1 ;;
esac
EOF
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker %s\n' "$*" >>"$FAKE_RELEASE_LOG"
version_digest="sha256:$FAKE_AGENT_DIGEST"
descriptor_digest='sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'
attestation_digest='sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'
config_digest="$FAKE_AGENT_IMAGE_ID"
case "${1:-} ${2:-} ${3:-}" in
  'build '*|'buildx build '*)
    if [ "${1:-}" = buildx ]; then
      [ "${FAKE_AGENT_FAIL_VERSION_PUSH:-0}" != 1 ] || exit 1
      : >"$FAKE_AGENT_DOCKER_STATE/version-built"
      metadata_file=''
      previous=''
      for argument in "$@"; do
        if [ "$previous" = --metadata-file ]; then metadata_file=$argument; fi
        previous=$argument
      done
      [ -z "$metadata_file" ] || printf '{"containerimage.digest":"%s"}\n' "${FAKE_AGENT_BUILDX_DIGEST:-$version_digest}" >"$metadata_file"
    fi
    ;;
  'buildx imagetools inspect')
    ref=${4:-}
    if [ "${5:-}" = --raw ]; then
      case "$ref" in
        *@"$descriptor_digest") printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":1},"layers":[]}\n' "$config_digest" ;;
        *@"$attestation_digest") printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:9999999999999999999999999999999999999999999999999999999999999999","size":1},"layers":[{"mediaType":"application/vnd.in-toto+json","digest":"sha256:5555555555555555555555555555555555555555555555555555555555555555","size":1,"annotations":{"in-toto.io/predicate-type":"https://spdx.dev/Document"}},{"mediaType":"application/vnd.in-toto+json","digest":"sha256:6666666666666666666666666666666666666666666666666666666666666666","size":1,"annotations":{"in-toto.io/predicate-type":"https://slsa.dev/provenance/v0.2"}}]}\n' ;;
        *) exit 1 ;;
      esac
      exit
    fi
    if [ "$ref" = dirextalk/agent:latest ]; then
      if [ ! -f "$FAKE_AGENT_DOCKER_STATE/latest-created" ]; then printf 'ERROR: docker.io/%s: not found\n' "$ref" >&2; exit 1; fi
      index_digest=${FAKE_AGENT_LATEST_DIGEST:-$version_digest}
    else
      if [ ! -f "$FAKE_AGENT_DOCKER_STATE/version-built" ]; then printf 'ERROR: docker.io/%s: not found\n' "$ref" >&2; exit 1; fi
      index_digest=${FAKE_AGENT_INDEX_DIGEST:-$version_digest}
    fi
    FAKE_INDEX_DIGEST=$index_digest FAKE_DESCRIPTOR_DIGEST=$descriptor_digest \
      FAKE_ATTESTATION_DIGEST=$attestation_digest python3 - <<'PY'
import json, os
manifests = []
if os.environ.get("FAKE_AGENT_INCLUDE_AMD64", "1") == "1":
    manifests.append({
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "digest": os.environ["FAKE_DESCRIPTOR_DIGEST"],
        "platform": {"os": "linux", "architecture": "amd64"},
    })
if os.environ.get("FAKE_AGENT_EXTRA_PLATFORM", "0") == "1":
    manifests.append({
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "digest": "sha256:" + "f" * 64,
        "platform": {"os": "linux", "architecture": "arm64"},
    })
manifests.append({
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "digest": os.environ["FAKE_ATTESTATION_DIGEST"],
    "platform": {"os": "unknown", "architecture": "unknown"},
    "annotations": {"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": os.environ["FAKE_DESCRIPTOR_DIGEST"]},
})
print(json.dumps({"manifest": {
    "mediaType": os.environ.get("FAKE_AGENT_MEDIA_TYPE", "application/vnd.oci.image.index.v1+json"),
    "digest": os.environ["FAKE_INDEX_DIGEST"],
    "manifests": manifests,
}}, separators=(",", ":")))
PY
    ;;
  'buildx imagetools create')
    : >"$FAKE_AGENT_DOCKER_STATE/latest-created"
    ;;
  'pull --platform linux/amd64')
    [ "${FAKE_AGENT_FAIL_VERSION_PULL:-0}" != 1 ] || exit 1
    printf '%s\n' "${4:-}" >"$FAKE_AGENT_DOCKER_STATE/last-pulled"
    ;;
  'image inspect '*)
    if [[ "$*" == *'{{.Id}}'* ]]; then
      printf '%s\n' "$FAKE_AGENT_IMAGE_ID"
    else
      ref=${3:-}
      revision=$FAKE_AGENT_COMMIT
      if [ -f "$FAKE_AGENT_DOCKER_STATE/last-pulled" ] &&
         [ "$(<"$FAKE_AGENT_DOCKER_STATE/last-pulled")" = "$ref" ]; then
        if [ "$ref" = dirextalk/agent:latest ]; then
          revision=${FAKE_AGENT_LATEST_PULL_REVISION:-${FAKE_AGENT_PULL_REVISION:-$revision}}
        else
          revision=${FAKE_AGENT_PULL_REVISION:-$revision}
        fi
      fi
      printf '%s|%s|2026-08-06T00:00:00+08:00\n' "$FAKE_AGENT_VERSION" "$revision"
    fi
    ;;
  'run --rm --entrypoint')
    binary=${4:-}
    ref=${5:-}
    if [ "${6:-}" = --version ]; then
      version=$FAKE_AGENT_VERSION
      if [ -f "$FAKE_AGENT_DOCKER_STATE/last-pulled" ] &&
         [ "$(<"$FAKE_AGENT_DOCKER_STATE/last-pulled")" = "$ref" ]; then
        if [ "$ref" = dirextalk/agent:latest ]; then
          version=${FAKE_AGENT_LATEST_PULL_VERSION:-${FAKE_AGENT_PULL_VERSION:-$version}}
        else
          version=${FAKE_AGENT_PULL_VERSION:-$version}
        fi
      fi
      printf '%s\n' "$version"
      exit 0
    fi
    case "$binary" in
      *dirextalk-extension-runner) printf 'usage: dirextalk-extension-runner\n' >&2; exit 2 ;;
      *dirextalk-core-runner) printf 'usage: dirextalk-core-runner\n' >&2; exit 2 ;;
      *) printf 'usage: dirextalk-agent\n' >&2; exit 1 ;;
    esac
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/git" "$tmp/bin/docker"
run_release() {
  PATH="$tmp/bin:$PATH" FAKE_RELEASE_LOG=$log FAKE_AGENT_COMMIT=$commit \
    FAKE_AGENT_SOURCE=$tmp/agent \
    FAKE_AGENT_IMAGE_ID=$image_id FAKE_AGENT_DIGEST=$digest \
    FAKE_AGENT_DOCKER_STATE=$tmp/docker-state FAKE_AGENT_VERSION=v1.0.0 \
    AGENT_RELEASE_SOURCE_ROOT=$tmp/agent AGENT_RELEASE_OUTPUT=$tmp/out "$@"
}
if FAKE_AGENT_STATUS=' M tracked.go' run_release "$script_dir/prepare-agent.sh" v1.0.0 >/dev/null 2>&1; then
  echo 'Agent prepare accepted tracked worktree pollution' >&2
  exit 1
fi
if FAKE_AGENT_STATUS='?? unexpected.txt' run_release "$script_dir/prepare-agent.sh" v1.0.0 >/dev/null 2>&1; then
  echo 'Agent prepare accepted an unrelated untracked file' >&2
  exit 1
fi
run_release "$script_dir/prepare-agent.sh" v1.0.0 >/dev/null
run_release "$script_dir/verify-agent.sh" v1.0.0 >/dev/null
grep -Fq 'docker build --pull --build-arg VERSION=v1.0.0 --build-arg REVISION=' "$log"
if grep -Fq '.codex-final-overlay.Containerfile' "$log"; then
  echo 'protected local overlay appeared in the Docker build command' >&2
  exit 1
fi

for failure in FAKE_AGENT_FAIL_VERSION_PUSH FAKE_AGENT_FAIL_VERSION_PULL; do
  : >"$log"
  rm -f "$tmp/docker-state/"*
  if run_release env "$failure=1" "$script_dir/publish-agent.sh" v1.0.0 >/dev/null 2>&1; then
    echo "Agent publish accepted $failure" >&2
    exit 1
  fi
  if grep -Fq 'docker buildx imagetools create --tag dirextalk/agent:latest' "$log"; then
    echo "Agent latest moved after $failure" >&2
    exit 1
  fi
done

for remote_case in media-type missing-amd64 extra-platform invalid-digest labels version; do
  : >"$log"
  rm -f "$tmp/docker-state/"*
  case "$remote_case" in
    media-type) remote_env=(FAKE_AGENT_MEDIA_TYPE='application/vnd.docker.distribution.manifest.v2+json') ;;
    missing-amd64) remote_env=(FAKE_AGENT_INCLUDE_AMD64=0) ;;
    extra-platform) remote_env=(FAKE_AGENT_EXTRA_PLATFORM=1) ;;
    invalid-digest) remote_env=(FAKE_AGENT_INDEX_DIGEST='sha256:invalid') ;;
    labels) remote_env=(FAKE_AGENT_PULL_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb) ;;
    version) remote_env=(FAKE_AGENT_PULL_VERSION=v9.9.9) ;;
  esac
  if run_release env "${remote_env[@]}" "$script_dir/publish-agent.sh" v1.0.0 >/dev/null 2>&1; then
    echo "Agent publish accepted invalid remote proof: $remote_case" >&2
    exit 1
  fi
  if grep -Fq 'docker buildx imagetools create --tag dirextalk/agent:latest' "$log"; then
    echo "Agent latest moved after invalid remote proof: $remote_case" >&2
    exit 1
  fi
done

: >"$log"
rm -f "$tmp/docker-state/"*
if run_release env \
    FAKE_AGENT_LATEST_DIGEST=sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff \
    "$script_dir/publish-agent.sh" v1.0.0 >/dev/null 2>&1; then
  echo 'Agent publish accepted different version and latest OCI index digests' >&2
  exit 1
fi
grep -Fq 'docker buildx imagetools create --tag dirextalk/agent:latest' "$log"
if grep -Fq 'docker pull --platform linux/amd64 dirextalk/agent:latest' "$log"; then
  echo 'Agent latest was pulled after its index digest mismatched' >&2
  exit 1
fi

: >"$log"
rm -f "$tmp/docker-state/"*
run_release "$script_dir/publish-agent.sh" v1.0.0 >/dev/null
grep -Fq 'docker buildx build --pull --platform linux/amd64 --provenance=mode=max --sbom=true --push' "$log"
grep -Fq 'docker buildx imagetools inspect dirextalk/agent:v1.0.0' "$log"
grep -Fq 'docker pull --platform linux/amd64 dirextalk/agent@sha256:' "$log"
grep -Fq 'docker buildx imagetools create --tag dirextalk/agent:latest' "$log"
grep -Fq 'docker buildx imagetools inspect dirextalk/agent:latest' "$log"
grep -Fq 'docker pull --platform linux/amd64 dirextalk/agent@sha256:' "$log"
version_push_line=$(grep -nF 'docker buildx build --pull --platform linux/amd64' "$log" | cut -d: -f1)
version_inspect_line=$(grep -nF 'docker buildx imagetools inspect dirextalk/agent:v1.0.0' "$log" | head -1 | cut -d: -f1)
latest_create_line=$(grep -nF 'docker buildx imagetools create --tag dirextalk/agent:latest' "$log" | cut -d: -f1)
latest_inspect_line=$(grep -nF 'docker buildx imagetools inspect dirextalk/agent:latest' "$log" | head -1 | cut -d: -f1)
(( version_inspect_line < version_push_line && version_push_line < latest_inspect_line && latest_inspect_line < latest_create_line )) || {
  echo 'Agent publish order is not version index/proof -> latest index/proof' >&2
  exit 1
}
for binary in dirextalk-agent dirextalk-extension-runner dirextalk-core-runner; do
  grep -Fq -- "--entrypoint /usr/local/bin/$binary dirextalk/agent@sha256:" "$log"
done
printf 'formal Agent OCI build, three-binary version verification, and version/latest digest contract verified\n'
