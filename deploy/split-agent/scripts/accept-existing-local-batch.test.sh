#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/accept-existing-local-batch.sh
helper=$script_dir/internal/accept-existing-ws/main.go
[ -x "$script" ]
bash -n "$script"
test -z "$(gofmt -d "$helper")"
go test ./deploy/split-agent/scripts/internal/accept-existing-ws
for required in \
  'set -uo pipefail' \
  'run_group skills_mcp' \
  'run_group http_chat' \
  'run_group ws_stream_reconnect' \
  'run_group durable_failed_history' \
  'run_group memory_ambiguous_readback' \
  'run_group static_site_lifecycle' \
  'qwen/qwen3-32b' \
  'agent.core.mcp.list' \
  'agent.core.skills.list' \
  'agent.chat.turns.list' \
  'agent.memory.facts.update' \
  'agent.static_sites.delete'; do
  grep -Fq -- "$required" "$script"
done
grep -Fq 'client.native_agent_stream' "$helper"
grep -Fq 'after_seq' "$helper"
if grep -Eq 'docker compose .*\b(restart|up|down|create|pull)\b|aws[[:space:]]' "$script"; then
  echo 'existing-stack batch acceptance contains infrastructure mutation' >&2
  exit 1
fi
echo 'accept-existing-local-batch contract test passed'
