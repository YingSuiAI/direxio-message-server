#!/bin/sh
# Dash regression: an ownership guard failure must prevent Compose down.
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/run"
printf '%s\n' 'placeholder' > "$tmp/run/server.env"
if TEST_OWNERSHIP_GUARD_FAIL=1 "$root/scripts/agent-core-local.sh" down "$tmp/run" >/dev/null 2>&1; then
  echo 'ownership guard unexpectedly allowed cleanup' >&2
  exit 1
fi
echo 'ownership guard blocks destructive down passed'
