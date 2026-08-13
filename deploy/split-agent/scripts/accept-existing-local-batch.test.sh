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
[ "$(grep -Fc 'run_group static_site_lifecycle static_site_group' "$script")" -eq 1 ]
grep -Fq 'client.native_agent_stream' "$helper"
grep -Fq 'after_seq' "$helper"
static_group=$(sed -n '/^static_site_group() {/,/^}/p' "$script")
for required in \
  'static-publish-config.json' \
  'static-publish.frames.jsonl' \
  'expect:"done",reconnect:false' \
  'select(.event=="done")' \
  '.data.text' \
  'agent.static_sites.list' \
  'agent.static_sites.delete'; do
  grep -Fq -- "$required" <<<"$static_group"
done
if grep -Fq 'call_http agent.chat ' <<<"$static_group"; then
  echo 'static-site acceptance bypasses the durable Native WS helper' >&2
  exit 1
fi
terminal_text_filter='[.[] | select(.event=="done") | (.data.text // empty) | select(type=="string" and length>0)] | last // empty'
missing_terminal_text=$(printf '%s\n' '{"event":"done","data":{}}' | jq -rs "$terminal_text_filter")
[ -z "$missing_terminal_text" ]
published_terminal_text=$(printf '%s\n' '{"event":"done","data":{"text":"https://example.test/site"}}' | jq -rs "$terminal_text_filter")
[ "$published_terminal_text" = 'https://example.test/site' ]
if grep -Eq 'docker compose .*\b(restart|up|down|create|pull)\b|aws[[:space:]]' "$script"; then
  echo 'existing-stack batch acceptance contains infrastructure mutation' >&2
  exit 1
fi
echo 'accept-existing-local-batch contract test passed'
