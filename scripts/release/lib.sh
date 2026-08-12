#!/usr/bin/env bash
set -euo pipefail
release_die() {
  printf 'release gate failed: %s\n' "$*" >&2
  exit 1
}

release_init() {
  [[ $# -eq 1 ]] || release_die 'usage: <script> vX.Y.Z'
  RELEASE_VERSION="$1"
  [[ "$RELEASE_VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || release_die 'version must be canonical vX.Y.Z'

  local discovered_root configured_root configured_output expected_output
  discovered_root="$(cd "$(dirname "${BASH_SOURCE[1]}")/../.." && pwd)"
  configured_root="${RELEASE_REPO_ROOT:-$discovered_root}"
  if [[ "$configured_root" != "$discovered_root" && "${RELEASE_CONTRACT_TEST:-0}" != 1 ]]; then
    release_die 'RELEASE_REPO_ROOT override is allowed only in contract tests'
  fi
  RELEASE_REPO_ROOT="$(cd "$configured_root" && pwd -P)"
  expected_output="$RELEASE_REPO_ROOT/.release/$RELEASE_VERSION"
  configured_output="${RELEASE_OUTPUT_DIR:-$expected_output}"
  if [[ "$configured_output" != "$expected_output" && "${RELEASE_CONTRACT_TEST:-0}" != 1 ]]; then
    release_die 'release output must be the repository .release/version directory'
  fi

  RELEASE_OUTPUT_DIR="$configured_output"
  RELEASE_EXPECTED_BRANCH="${RELEASE_EXPECTED_BRANCH:-main}"
  RELEASE_CONFIG="$RELEASE_REPO_ROOT/release/$RELEASE_VERSION.json"
  RELEASE_CONTEXT="$RELEASE_OUTPUT_DIR/release-context.json"
  RELEASE_VERIFIED="$RELEASE_OUTPUT_DIR/verified.json"
  RELEASE_IMAGE="dirextalk/message-server:$RELEASE_VERSION"
  export RELEASE_VERSION RELEASE_REPO_ROOT RELEASE_OUTPUT_DIR RELEASE_EXPECTED_BRANCH RELEASE_CONFIG RELEASE_CONTEXT RELEASE_VERIFIED RELEASE_IMAGE
}

release_require_tools() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || release_die "required tool is unavailable: $tool"
  done
}

release_validate_config() {
  [[ $# -eq 2 ]] || release_die 'internal error: release config validation requires source schema versions'
  [[ -f "$RELEASE_CONFIG" ]] || release_die "missing release config release/$RELEASE_VERSION.json"
  python3 - "$RELEASE_CONFIG" "$RELEASE_VERSION" "$1" "$2" <<'PY'
import json, re, sys

path, expected, source_schema, source_compat = sys.argv[1:]
try:
    config = json.loads(open(path, "rb").read())
except Exception as exc:
    raise SystemExit(f"invalid release config: {exc}")

required = {
    "version",
    "minimum_client_version",
    "maximum_client_version_exclusive",
    "schema_version",
    "schema_compat_version",
}
if not isinstance(config, dict) or set(config) != required:
    raise SystemExit("release config fields do not match the fixed contract")

version_re = re.compile(r"^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$")
if config["version"] != expected or not version_re.fullmatch(expected):
    raise SystemExit("release config version mismatch")
for field in ("minimum_client_version", "maximum_client_version_exclusive"):
    if not isinstance(config[field], str) or not version_re.fullmatch(config[field]):
        raise SystemExit(f"{field} must be canonical")

def semver(value):
    return tuple(map(int, value[1:].split(".")))

if semver(config["minimum_client_version"]) >= semver(config["maximum_client_version_exclusive"]):
    raise SystemExit("minimum_client_version must be lower than maximum_client_version_exclusive")

schema = config["schema_version"]
compat = config["schema_compat_version"]
if isinstance(schema, bool) or isinstance(compat, bool) or not isinstance(schema, int) or not isinstance(compat, int) or compat < 1 or schema < compat:
    raise SystemExit("schema compatibility is invalid")
if schema != int(source_schema) or compat != int(source_compat):
    raise SystemExit("release config schema versions do not match internal/version.go")
PY
}

release_preflight() {
  release_require_tools git python3
  cd "$RELEASE_REPO_ROOT"
  [[ -z "$(git status --porcelain)" ]] || release_die 'working tree must be clean'

  local branch head remote_line remote_head source_values
  branch="$(git branch --show-current)"
  [[ "$branch" == "$RELEASE_EXPECTED_BRANCH" ]] || release_die "release branch must be $RELEASE_EXPECTED_BRANCH"
  head="$(git rev-parse HEAD)"
  remote_line="$(git ls-remote --exit-code origin "refs/heads/$RELEASE_EXPECTED_BRANCH")"
  [[ "$remote_line" =~ ^([0-9a-f]{40})[[:space:]]refs/heads/$RELEASE_EXPECTED_BRANCH$ ]] || release_die 'remote release branch response is invalid'
  remote_head="${BASH_REMATCH[1]}"
  [[ "$head" =~ ^[0-9a-f]{40}$ && "$head" == "$remote_head" ]] || release_die 'HEAD must exactly match the pushed release branch'
  grep -Eq "^##[[:space:]]+$RELEASE_VERSION([[:space:]]|$)" release/RELEASE_NOTES.md || release_die 'matching release notes section is required'
  source_values="$(python3 - internal/version.go <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
patterns = (
    r'(?m)^\s*version\s*=\s*"([^"]+)"',
    r'(?m)^\s*SchemaVersion\s*=\s*([0-9]+)\s*$',
    r'(?m)^\s*SchemaCompatVersion\s*=\s*([0-9]+)\s*$',
)
values = []
for pattern in patterns:
    match = re.search(pattern, text)
    if not match:
        raise SystemExit("internal/version.go does not declare the fixed release identity")
    values.append(match.group(1))
print("\n".join(values))
PY
)" || release_die 'source release identity is invalid'
  mapfile -t source_fields <<<"$source_values"
  [[ "${#source_fields[@]}" == 3 ]] || release_die 'source release identity is incomplete'
  [[ "${source_fields[0]}" == "$RELEASE_VERSION" ]] || release_die 'source default version does not match release version'
  release_validate_config "${source_fields[1]}" "${source_fields[2]}"

  RELEASE_COMMIT="$head"
  RELEASE_BUILD_TIME="$(git show -s --format=%cI HEAD)"
  [[ "$RELEASE_BUILD_TIME" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T ]] || release_die 'commit build time is invalid'
  export RELEASE_COMMIT RELEASE_BUILD_TIME
}

release_write_context() {
  mkdir -p "$RELEASE_OUTPUT_DIR"
  python3 - "$RELEASE_CONTEXT" "$RELEASE_VERSION" "$RELEASE_COMMIT" "$RELEASE_BUILD_TIME" "$RELEASE_IMAGE" <<'PY'
import json, os, pathlib, sys, tempfile
path = pathlib.Path(sys.argv[1])
value = dict(zip(("version", "commit", "build_time", "image"), sys.argv[2:]))
data = json.dumps(value, separators=(",", ":"), sort_keys=True).encode()
fd, temporary = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
try:
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
PY
}

release_require_context() {
  [[ -f "$RELEASE_CONTEXT" ]] || release_die 'prepare must complete first'
  local current_head values
  current_head="$(cd "$RELEASE_REPO_ROOT" && git rev-parse HEAD)"
  values="$(python3 - "$RELEASE_CONTEXT" <<'PY'
import json, re, sys
try:
    raw = open(sys.argv[1], "rb").read()
    value = json.loads(raw)
except Exception as exc:
    raise SystemExit(f"invalid release context: {exc}")
if set(value) != {"version", "commit", "build_time", "image"}:
    raise SystemExit("invalid release context fields")
patterns = {
    "version": r"^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$",
    "commit": r"^[0-9a-f]{40}$",
    "build_time": r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[^\r\n]+$",
    "image": r"^dirextalk/message-server:v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$",
}
if any(not isinstance(value[key], str) or not re.fullmatch(pattern, value[key]) for key, pattern in patterns.items()):
    raise SystemExit("invalid release context value")
canonical = json.dumps(value, separators=(",", ":"), sort_keys=True).encode()
if raw != canonical:
    raise SystemExit("release context is not canonical")
print(value["version"])
print(value["commit"])
print(value["build_time"])
print(value["image"])
PY
)" || release_die 'prepared context is invalid'
  mapfile -t context_values <<<"$values"
  [[ "${#context_values[@]}" == 4 ]] || release_die 'prepared context is incomplete'
  [[ "${context_values[0]}" == "$1" && "${context_values[0]}" == "$RELEASE_VERSION" && "${context_values[1]}" == "$current_head" && "${context_values[1]}" == "$RELEASE_COMMIT" && "${context_values[2]}" == "$RELEASE_BUILD_TIME" && "${context_values[3]}" == "$RELEASE_IMAGE" ]] || release_die 'prepared context does not match current HEAD/version/build/image'
}

release_write_verified() {
  python3 - "$RELEASE_VERIFIED" "$RELEASE_VERSION" "$RELEASE_COMMIT" "$RELEASE_IMAGE" <<'PY'
import json, os, pathlib, sys, tempfile
path = pathlib.Path(sys.argv[1])
value = {"commit": sys.argv[3], "image": sys.argv[4], "image_probe": "passed", "tests": "passed", "version": sys.argv[2]}
data = json.dumps(value, separators=(",", ":"), sort_keys=True).encode()
fd, temporary = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
try:
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
PY
}

release_require_verified() {
  [[ -f "$RELEASE_VERIFIED" ]] || release_die 'verify must complete first'
  local values
  values="$(python3 - "$RELEASE_VERIFIED" <<'PY'
import json, re, sys
try:
    raw = open(sys.argv[1], "rb").read()
    value = json.loads(raw)
except Exception as exc:
    raise SystemExit(f"invalid verification evidence: {exc}")
required = {"version", "commit", "image", "tests", "image_probe"}
if set(value) != required or raw != json.dumps(value, separators=(",", ":"), sort_keys=True).encode():
    raise SystemExit("verification evidence is not canonical")
if value["tests"] != "passed" or value["image_probe"] != "passed":
    raise SystemExit("verification gates did not pass")
patterns = {
    "version": r"^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$",
    "commit": r"^[0-9a-f]{40}$",
    "image": r"^dirextalk/message-server:v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$",
}
if any(not isinstance(value[key], str) or not re.fullmatch(pattern, value[key]) for key, pattern in patterns.items()):
    raise SystemExit("verification evidence value is invalid")
print(value["version"])
print(value["commit"])
print(value["image"])
PY
)" || release_die 'verification evidence is invalid'
  mapfile -t verified_values <<<"$values"
  [[ "${#verified_values[@]}" == 3 && "${verified_values[0]}" == "$RELEASE_VERSION" && "${verified_values[1]}" == "$RELEASE_COMMIT" && "${verified_values[2]}" == "$RELEASE_IMAGE" ]] || release_die 'verification evidence does not match release context'
}

release_write_notes() {
  local destination=$1
  python3 - "$RELEASE_REPO_ROOT/release/RELEASE_NOTES.md" "$RELEASE_VERSION" "$destination" <<'PY'
import pathlib, re, sys
source, version, destination = sys.argv[1:]
text = pathlib.Path(source).read_text(encoding="utf-8")
match = re.search(rf"(?ms)^##[ \t]+{re.escape(version)}[ \t]*\n.*?(?=^##[ \t]+v|\Z)", text)
if not match:
    raise SystemExit("release notes section is missing")
pathlib.Path(destination).write_text(match.group(0).rstrip() + "\n", encoding="utf-8", newline="\n")
PY
}
