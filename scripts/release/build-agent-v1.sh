#!/usr/bin/env bash
# Compatibility entrypoint for the first formal Agent release. It builds and
# verifies locally but never publishes.
set -euo pipefail
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
version=${1:-v1.0.0}
"$script_dir/prepare-agent.sh" "$version"
"$script_dir/verify-agent.sh" "$version"
