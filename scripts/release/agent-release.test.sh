#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-agent-release.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/agent/deploy/container" "$tmp/out"
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
case "$1" in
  build|push|pull) exit 0 ;;
  image)
    case "$*" in
      *'{{.Id}}'*) printf '%s\n' "$FAKE_AGENT_IMAGE_ID" ;;
      *'.RepoDigests'*) printf 'dirextalk/agent@sha256:%s\n' "$FAKE_AGENT_DIGEST" ;;
      *) printf '%s|%s\n' "$FAKE_AGENT_VERSION" "$FAKE_AGENT_COMMIT" ;;
    esac ;;
  run)
    case "$*" in
      *dirextalk-extension-runner*) printf 'usage: dirextalk-extension-runner\n' >&2; exit 2 ;;
      *dirextalk-core-runner*) printf 'usage: dirextalk-core-runner\n' >&2; exit 2 ;;
      *) printf 'usage: dirextalk-agent\n' >&2; exit 1 ;;
    esac ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/git" "$tmp/bin/docker"
run_release() {
  PATH="$tmp/bin:$PATH" FAKE_RELEASE_LOG=$log FAKE_AGENT_COMMIT=$commit \
    FAKE_AGENT_SOURCE=$tmp/agent \
    FAKE_AGENT_IMAGE_ID=$image_id FAKE_AGENT_DIGEST=$digest FAKE_AGENT_VERSION=v1.0.0 \
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
run_release "$script_dir/publish-agent.sh" v1.0.0 >/dev/null
grep -Fq 'docker build --pull --build-arg VERSION=v1.0.0 --build-arg REVISION=' "$log"
if grep -Fq '.codex-final-overlay.Containerfile' "$log"; then
  echo 'protected local overlay appeared in the Docker build command' >&2
  exit 1
fi
grep -Fq 'docker push dirextalk/agent:v1.0.0' "$log"
for binary in dirextalk-agent dirextalk-extension-runner dirextalk-core-runner; do
  grep -Fq -- "--entrypoint /usr/local/bin/$binary dirextalk/agent:v1.0.0" "$log"
done
printf 'formal Agent build, three-binary verification, and digest publication contract verified\n'
