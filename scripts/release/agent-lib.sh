#!/usr/bin/env bash
set -euo pipefail

agent_release_die() {
  printf 'Agent release gate failed: %s\n' "$*" >&2
  exit 1
}

agent_release_init() {
  [ "$#" -eq 1 ] || agent_release_die 'usage: <script> vX.Y.Z'
  AGENT_RELEASE_VERSION=$1
  printf '%s\n' "$AGENT_RELEASE_VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || \
    agent_release_die 'version must be canonical vX.Y.Z'
  AGENT_RELEASE_MESSAGE_ROOT=$(cd "$(dirname "${BASH_SOURCE[1]}")/../.." && pwd -P)
  AGENT_RELEASE_SOURCE_ROOT=${AGENT_RELEASE_SOURCE_ROOT:-$(cd "$AGENT_RELEASE_MESSAGE_ROOT/../dirextalk-agent" && pwd -P)}
  AGENT_RELEASE_OUTPUT=${AGENT_RELEASE_OUTPUT:-$AGENT_RELEASE_MESSAGE_ROOT/.release/agent/$AGENT_RELEASE_VERSION}
  AGENT_RELEASE_IMAGE="dirextalk/agent:$AGENT_RELEASE_VERSION"
  AGENT_RELEASE_CONTEXT=$AGENT_RELEASE_OUTPUT/context.json
  AGENT_RELEASE_VERIFIED=$AGENT_RELEASE_OUTPUT/verified.json
  export AGENT_RELEASE_VERSION AGENT_RELEASE_MESSAGE_ROOT AGENT_RELEASE_SOURCE_ROOT AGENT_RELEASE_OUTPUT
  export AGENT_RELEASE_IMAGE AGENT_RELEASE_CONTEXT AGENT_RELEASE_VERIFIED
}

agent_release_require_tools() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || agent_release_die "required tool is unavailable: $tool"
  done
}

agent_release_source_identity() {
  agent_release_require_tools git
  [ -f "$AGENT_RELEASE_SOURCE_ROOT/deploy/container/agent.Containerfile" ] || agent_release_die 'Agent Containerfile is missing'
  local status branch head expected_branch
  status=$(git -C "$AGENT_RELEASE_SOURCE_ROOT" status --porcelain=v1 --untracked-files=all)
  if [ -n "$status" ] && [ "$status" != '?? .codex-final-overlay.Containerfile' ]; then
    agent_release_die 'Agent source may contain only the protected untracked .codex-final-overlay.Containerfile'
  fi
  expected_branch=${AGENT_RELEASE_EXPECTED_BRANCH:-adam/agent-core-v1-integration}
  branch=$(git -C "$AGENT_RELEASE_SOURCE_ROOT" branch --show-current)
  [ "$branch" = "$expected_branch" ] || agent_release_die "Agent release source must be on $expected_branch"
  head=$(git -C "$AGENT_RELEASE_SOURCE_ROOT" rev-parse HEAD)
  printf '%s\n' "$head" | grep -Eq '^[0-9a-f]{40}$' || agent_release_die 'Agent source commit is invalid'
  AGENT_RELEASE_COMMIT=$head
  AGENT_RELEASE_BUILD_TIME=$(git -C "$AGENT_RELEASE_SOURCE_ROOT" show -s --format=%cI HEAD)
  export AGENT_RELEASE_COMMIT AGENT_RELEASE_BUILD_TIME
}

agent_release_export_context() {
  agent_release_require_tools git tar
  AGENT_RELEASE_BUILD_CONTEXT=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-agent-release-context.XXXXXX")
  export AGENT_RELEASE_BUILD_CONTEXT
  if ! git -C "$AGENT_RELEASE_SOURCE_ROOT" archive --format=tar "$AGENT_RELEASE_COMMIT" | tar -x -C "$AGENT_RELEASE_BUILD_CONTEXT"; then
    rm -rf "$AGENT_RELEASE_BUILD_CONTEXT"
    agent_release_die 'could not export committed Agent build context'
  fi
  [ ! -e "$AGENT_RELEASE_BUILD_CONTEXT/.codex-final-overlay.Containerfile" ] || {
    rm -rf "$AGENT_RELEASE_BUILD_CONTEXT"
    agent_release_die 'protected local overlay entered the committed build context'
  }
  [ -f "$AGENT_RELEASE_BUILD_CONTEXT/deploy/container/agent.Containerfile" ] || {
    rm -rf "$AGENT_RELEASE_BUILD_CONTEXT"
    agent_release_die 'committed Agent Containerfile is missing from exported context'
  }
}

agent_release_write_json() {
  local path=$1 kind=$2 image_id=${3:-}
  mkdir -p "$AGENT_RELEASE_OUTPUT"
  python3 - "$path" "$kind" "$AGENT_RELEASE_VERSION" "$AGENT_RELEASE_COMMIT" "$AGENT_RELEASE_BUILD_TIME" "$AGENT_RELEASE_IMAGE" "$image_id" <<'PY'
import json, os, pathlib, sys, tempfile
path = pathlib.Path(sys.argv[1])
keys = ("kind", "version", "commit", "build_time", "image", "image_id")
value = dict(zip(keys, sys.argv[2:]))
if not value["image_id"]:
    value.pop("image_id")
data = json.dumps(value, separators=(",", ":"), sort_keys=True).encode()
fd, temporary = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
try:
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data); stream.flush(); os.fsync(stream.fileno())
    os.replace(temporary, path)
finally:
    if os.path.exists(temporary): os.unlink(temporary)
PY
}

agent_release_require_json() {
  local path=$1 expected_kind=$2 require_id=$3 values
  [ -f "$path" ] || agent_release_die "missing $(basename "$path") evidence"
  values=$(python3 - "$path" "$expected_kind" "$require_id" <<'PY'
import json, pathlib, re, sys
path, kind, require_id = sys.argv[1:]
raw = pathlib.Path(path).read_bytes()
value = json.loads(raw)
required = {"kind", "version", "commit", "build_time", "image"}
if require_id == "yes": required.add("image_id")
if set(value) != required or raw != json.dumps(value, separators=(",", ":"), sort_keys=True).encode():
    raise SystemExit("evidence is not canonical")
patterns = {
  "version": r"^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$",
  "commit": r"^[0-9a-f]{40}$", "build_time": r"^[^\r\n]+$",
  "image": r"^dirextalk/agent:v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$",
}
if require_id == "yes": patterns["image_id"] = r"^sha256:[0-9a-f]{64}$"
if value["kind"] != kind or any(not isinstance(value[k], str) or not re.fullmatch(p, value[k]) for k,p in patterns.items()):
    raise SystemExit("evidence value is invalid")
for key in ("version", "commit", "build_time", "image", "image_id"):
    print(value.get(key, ""))
PY
) || agent_release_die "invalid $(basename "$path") evidence"
  mapfile -t AGENT_RELEASE_EVIDENCE <<<"$values"
  [ "${AGENT_RELEASE_EVIDENCE[0]}" = "$AGENT_RELEASE_VERSION" ] && \
    [ "${AGENT_RELEASE_EVIDENCE[1]}" = "$AGENT_RELEASE_COMMIT" ] && \
    [ "${AGENT_RELEASE_EVIDENCE[2]}" = "$AGENT_RELEASE_BUILD_TIME" ] && \
    [ "${AGENT_RELEASE_EVIDENCE[3]}" = "$AGENT_RELEASE_IMAGE" ] || agent_release_die 'evidence does not match current Agent source'
}

agent_release_smoke() {
  local ref=$1 binary expected marker output status
  while IFS='|' read -r binary expected marker; do
    if output=$(docker run --rm --entrypoint "$binary" "$ref" 2>&1); then status=0; else status=$?; fi
    [ "$status" -ne 125 ] || agent_release_die "$binary smoke infrastructure failure"
    if [ "$status" -ne "$expected" ] || ! printf '%s' "$output" | grep -Fq "$marker"; then
      agent_release_die "$binary smoke failed"
    fi
  done <<'EOF'
/usr/local/bin/dirextalk-agent|1|usage: dirextalk-agent
/usr/local/bin/dirextalk-extension-runner|2|usage: dirextalk-extension-runner
/usr/local/bin/dirextalk-core-runner|2|usage: dirextalk-core-runner
EOF
}

agent_release_verify_image() {
  local ref=$1 identity
  identity=$(docker image inspect "$ref" --format '{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}') || agent_release_die 'Agent image inspection failed'
  [ "$identity" = "$AGENT_RELEASE_VERSION|$AGENT_RELEASE_COMMIT" ] || agent_release_die 'Agent image labels do not match the release source'
  agent_release_smoke "$ref"
}
