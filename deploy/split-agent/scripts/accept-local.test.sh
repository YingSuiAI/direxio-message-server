#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/accept-local.sh
[ -x "$script" ] || { echo "accept-local.sh must be executable" >&2; exit 1; }
bash -n "$script"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x "$script"
fi

# Contract guard: the helper must use protected files and the message-server
# public envelope, never a direct Agent/Qdrant HTTP listener or shell tracing.
grep -Fq -- '--rawfile' "$script"
grep -Fq -- '--config' "$script"
grep -Fq -- '/_p2p/query' "$script"
grep -Fq -- '/_p2p/health' "$script"
grep -Fq -- 'portal.account.delete' "$script"
grep -Fq -- 'agent.knowledge.upload.start' "$script"
grep -Fq -- 'agent.knowledge.memory.create' "$script"
grep -Fq -- 'agent.knowledge.memories.update' "$script"
grep -Fq -- 'embedding_dimension' "$script"
grep -Fq -- ".source_id == \$id" "$script"
grep -Fq -- 'memory-search true' "$script"
grep -Fq -- 'agent.messages.send' "$script"
grep -Fq -- 'agent.core.model_profiles.sync' "$script"
# The literal marker is intentionally matched in the target script.
# shellcheck disable=SC2016
grep -Fq -- '"$tmp/request-model-sync.json") continue' "$script"
if grep -Fq -- 'agent.knowledge.index' "$script"; then
  echo "accept-local.sh must not invoke the retired knowledge index action" >&2
  exit 1
fi
if grep -Eiq -- 'https?://(agent|qdrant)(:|/)' "$script"; then
  echo "accept-local.sh must not bypass message-server with direct Agent/Qdrant HTTP" >&2
  exit 1
fi
if grep -Fq -- 'set -x' "$script"; then
  echo "accept-local.sh must not enable shell tracing around protected values" >&2
  exit 1
fi
if grep -Fq -- 'curl --data' "$script"; then
  echo "accept-local.sh must pass request bodies through protected files" >&2
  exit 1
fi
if grep -Fq -- 'echo "$' "$script"; then
  echo "accept-local.sh must not echo variable-backed protected values" >&2
  exit 1
fi

echo "accept-local static contract test passed"
