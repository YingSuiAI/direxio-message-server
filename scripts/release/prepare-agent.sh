#!/usr/bin/env bash
set -euo pipefail
# Resolved from this installed script directory.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/agent-lib.sh"
agent_release_init "$@"
agent_release_require_tools python3
agent_release_source_identity
rm -rf "$AGENT_RELEASE_OUTPUT"
agent_release_write_json "$AGENT_RELEASE_CONTEXT" prepared
printf 'Agent release prepare passed for %s at %s\n' "$AGENT_RELEASE_VERSION" "$AGENT_RELEASE_COMMIT"
