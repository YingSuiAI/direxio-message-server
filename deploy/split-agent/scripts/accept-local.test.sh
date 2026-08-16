#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/accept-local.sh
[ -x "$script" ] || { echo "accept-local.sh must be executable" >&2; exit 1; }
bash -n "$script"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck "$script"
fi

for required in \
  'agent.session.create' \
  '/agent/v1/health' \
  '/agent/v1/catalog' \
  '/agent/v1/capabilities/agent.models.v1/operations/sync_models' \
  '/agent/v1/conversations/$conversation_id/turns' \
  '/agent/v1/operations/$operation_id' \
  '/agent/v1/operations/$operation_id/events?after_seq=$resume_after' \
  'Last-Event-ID: $resume_after' \
  '/agent/v1/conversations/$conversation_id?limit=100'; do
  grep -Fq "$required" "$script"
done

for retired in \
  'call agent.chat' \
  'agent.model_profiles.' \
  '/_p2p/agent/chat/' \
  'P2P_AGENT_CAPABILITY_' \
  'agent:50052'; do
  if grep -Fq "$retired" "$script"; then
    echo "retired Message Server Agent proxy survived acceptance: $retired" >&2
    exit 1
  fi
done

grep -Fq 'http_json POST "$turn_url" "$turn_request" "$turn_receipt" 202' "$script"
grep -Fq 'http_json POST "$turn_url" "$turn_request" "$turn_replay" 202' "$script"
grep -Fq '.replayed == true' "$script"
grep -Fq 'wait_operation "$operation_id"' "$script"
grep -Fq 'authoritative history' "$script"

echo "accept-local direct Agent contract test passed"
