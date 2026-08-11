#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/accept-local.sh
readme=$script_dir/../README.md
provision_script=$script_dir/provision-local.sh
provision_test=$script_dir/provision-local.test.sh
[ -x "$script" ] || { echo "accept-local.sh must be executable" >&2; exit 1; }
[ -x "$provision_script" ] || { echo "provision-local.sh must be executable" >&2; exit 1; }
[ -x "$provision_test" ] || { echo "provision-local.test.sh must be executable" >&2; exit 1; }
bash -n "$script"
bash -n "$provision_script"
bash -n "$provision_test"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x "$script"
  shellcheck -x "$provision_script" "$provision_test"
fi

# Contract guard: the helper must use protected files and the message-server
# public envelope, never a direct Agent listener or shell tracing.
grep -Fq -- '--rawfile' "$script"
grep -Fq -- '--config' "$script"
grep -Fq -- '/_p2p/query' "$script"
grep -Fq -- '/_p2p/health' "$script"
grep -Fq -- 'portal.account.delete' "$script"
grep -Fq -- "account_delete_enabled=\$(env_or DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE true)" "$script"
grep -Fq -- "case \"\$account_delete_enabled\" in true|false)" "$script"
grep -Fq -- 'DIREXTALK_ACCEPTANCE_CLEANUP_AFTER=true is incompatible with account deletion disabled' "$script"
grep -Fq -- 'agent.knowledge.upload.start' "$script"
grep -Fq -- 'agent.knowledge.status' "$script"
grep -Fq -- 'agent.knowledge.search' "$script"
grep -Fq -- 'agent.knowledge.memory.create' "$script"
grep -Fq -- 'agent.knowledge.memories.update' "$script"
grep -Fq -- 'embedding_dimension' "$script"
grep -Fq -- ".source_id == \$id" "$script"
grep -Fq -- 'memory-search true' "$script"
grep -Fq -- 'agent.messages.send' "$script"
grep -Fq -- 'agent.core.model_profiles.sync' "$script"
grep -Fq -- 'agent.models.list' "$script"
grep -Fq -- 'agent.model_profiles.list' "$script"
grep -Fq -- 'agent.model_profiles.get' "$script"
grep -Fq -- '.api_key_configured == true and (has("api_key") | not)' "$script"
grep -Fq -- '.profile.api_key_configured == true and (.profile | has("api_key") | not)' "$script"
grep -Fq -- 'agent.chat.conversations.create' "$script"
grep -Fq -- 'agent.chat.conversations.list' "$script"
grep -Fq -- 'agent.chat.conversations.get' "$script"
grep -Fq -- 'any(.messages[]?; (.content // "") | contains($marker))' "$script"
grep -Fq -- 'history-context-after-restart' "$script"
grep -Fq -- 'Native Agent did not preserve multi-turn conversation context after restart' "$script"
grep -Fq -- 'DIREXTALK_TAVILY_API_KEY_FILE' "$script"
grep -Fq -- 'agent.web_search.config.get' "$script"
grep -Fq -- 'agent.web_search.config.update' "$script"
grep -Fq -- 'agent.web_search.test' "$script"
grep -Fq -- 'core_web_search_configs' "$script"
grep -Fq -- 'core_web_search_replays' "$script"
grep -Fq -- 'ACCEPT_WEB_SEARCH_OK' "$script"
grep -Fq -- 'ACCEPT_KNOWLEDGE_' "$script"
grep -Fq -- 'ACCEPT_MEMORY_NEW_' "$script"
grep -Fq -- 'fresh Native Agent conversation did not recall the updated long-term-memory marker' "$script"
grep -Fq -- 'core_message_tool_results' "$script"
grep -Fq -- 'db_query_expect_denied agent-postgres dirextalk_agent dirextalk_message_server' "$script"
grep -Fq -- 'db_query_expect_denied message-postgres dirextalk_message_server dirextalk_agent' "$script"
grep -Fq -- 'SET ROLE dirextalk_message_server' "$script"
grep -Fq -- 'SET ROLE dirextalk_agent' "$script"
grep -Fq -- 'services.postgres.networks' "$script"
grep -Fq -- 'postgres_admin_password' "$script"
grep -Fq -- '.services["extension-runner"].cpus == 2' "$script"
grep -Fq -- '.services["extension-runner"].mem_limit == "1073741824"' "$script"
grep -Fq -- '.services["extension-runner"].pids_limit == 256' "$script"
grep -Fq -- "SELECT count(*) FROM pg_extension WHERE extname = 'vector'" "$script"
grep -Fq -- "SELECT vector_dims('[1,2,3]'::vector)" "$script"
grep -Fq -- 'DIREXTALK_MESSAGE_SERVER_NAME' "$script"
grep -Fq -- "--resolve \"\${resolve_host}:\${https_bind}:127.0.0.1\"" "$script"
# The literal marker is intentionally matched in the target script.
# shellcheck disable=SC2016
grep -Fq -- '"$tmp/request-web-search-config-update.json") continue' "$script"
# The raw provider catalog request is also a protected credential input.
# shellcheck disable=SC2016
grep -Fq -- '"$tmp/request-model-sync.json"|"$tmp/request-model-catalog.json") continue' "$script"

# Every HTTP agent.chat request is built through one closed helper. It always
# supplies the complete persisted server profile triple and accepts only the
# documented chat fields; request-scoped history, profiles, and credentials
# must not reappear.
chat_builder=$(sed -n '/^write_chat_params() {/,/^}/p' "$script")
for required_chat_field in model_profile_id model_profile_revision credential_version turn_id message; do
  grep -Fq -- "$required_chat_field" <<<"$chat_builder"
done
grep -Fq -- 'conversation_id' <<<"$chat_builder"
if grep -Eq -- 'messages|conversation_context|tool_credentials|inline_profile|api_key|base_url|provider' <<<"$chat_builder"; then
  echo "chat request builder contains a forbidden inline context/profile/credential field" >&2
  exit 1
fi
chat_call_count=$(grep -Ec -- '^call agent\.chat ' "$script")
chat_request_count=$(grep -E -- '^call agent\.chat ' "$script" | awk '{print $3}' | sort -u | wc -l | tr -d '[:space:]')
chat_builder_call_count=$(grep -Ec -- '^write_chat_params ' "$script")
[ "$chat_call_count" -ge "$chat_request_count" ] && [ "$chat_request_count" -eq "$chat_builder_call_count" ] || {
  echo "an agent.chat call bypasses the closed chat request builder" >&2
  exit 1
}

# Knowledge evidence is only the public source search marker; no chat prompt
# may claim to ground on or reproduce the uploaded marker. Memory recall is the
# inverse: the fresh-chat prompt must not contain the generated memory marker.
if grep -E -- '^write_chat_params ' "$script" | grep -Fq -- '$knowledge_phrase'; then
  echo "a chat prompt leaks the Knowledge marker" >&2
  exit 1
fi
grep -Fq -- 'write_chat_params "$memory_recall_chat"' "$script"
memory_recall_builder=$(sed -n '/^write_chat_params "$memory_recall_chat"/,/^call agent\.chat "$memory_recall_chat"/p' "$script")
if grep -Fq -- '$new_memory_phrase' <<<"$memory_recall_builder"; then
  echo "memory recall chat prompt leaks the expected marker" >&2
  exit 1
fi

# The post-restart second turn must use the same durable conversation with a
# fresh turn id, while omitting the first-turn marker from its request.
history_context_builder=$(sed -n '/^write_chat_params "$history_context_chat"/,/^call agent\.chat "$history_context_chat"/p' "$script")
grep -Fq -- '"$history_conversation_id"' <<<"$history_context_builder"
grep -Fq -- '"$history_context_turn_id"' <<<"$history_context_builder"
grep -Fq -- 'Recall the unique marker from the first turn of this conversation.' <<<"$history_context_builder"
if grep -Fq -- '$history_marker' <<<"$history_context_builder"; then
  echo "multi-turn continuation request leaks the first-turn marker" >&2
  exit 1
fi
restart_line=$(grep -nF -- 'run_compose restart agent' "$script" | head -n 1 | cut -d: -f1)
history_context_line=$(grep -nF -- 'call agent.chat "$history_context_chat" history-context-after-restart' "$script" | head -n 1 | cut -d: -f1)
[ -n "$restart_line" ] && [ -n "$history_context_line" ] && [ "$history_context_line" -gt "$restart_line" ] || {
  echo "multi-turn continuation does not execute after the Agent restart" >&2
  exit 1
}

# Account deletion remains the default disposable-account gate, but the final
# persistent-account lane must be able to skip only the deprovision assertions.
account_guard_line=$(grep -nF -- 'if [ "$account_delete_enabled" = true ]; then' "$script" | head -n 1 | cut -d: -f1)
account_delete_line=$(grep -nF -- "call portal.account.delete \"\$account_delete\" account-delete" "$script" | head -n 1 | cut -d: -f1)
[ -n "$account_guard_line" ] && [ -n "$account_delete_line" ]
[ "$account_delete_line" -gt "$account_guard_line" ] || {
  echo "portal.account.delete is not guarded by account deletion switch" >&2
  exit 1
}

# Exercise the option parser before any provisioned-directory or Docker gate so
# invalid values cannot silently select a destructive/persistent mode.
if account_invalid_output=$(DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE=maybe "$script" "$script_dir/.acceptance-missing" 2>&1); then
  echo "invalid account deletion switch was accepted" >&2
  exit 1
fi
case "$account_invalid_output" in
  *"DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE must be true or false"*) ;;
  *) echo "invalid account deletion switch returned the wrong error" >&2; exit 1 ;;
esac
if cleanup_mismatch_output=$(DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE=false DIREXTALK_ACCEPTANCE_CLEANUP_AFTER=true "$script" "$script_dir/.acceptance-missing" 2>&1); then
  echo "cleanup-after was accepted with account deletion disabled" >&2
  exit 1
fi
case "$cleanup_mismatch_output" in
  *"DIREXTALK_ACCEPTANCE_CLEANUP_AFTER=true is incompatible with account deletion disabled"*) ;;
  *) echo "cleanup-after mismatch returned the wrong error" >&2; exit 1 ;;
esac
if grep -Fq -- 'agent.knowledge.index' "$script"; then
  echo "accept-local.sh must not invoke the retired knowledge index action" >&2
  exit 1
fi
if grep -Eq -- 'model profile create/list/get|model connection test' "$readme"; then
  echo "README claims a model-profile operation that acceptance does not execute" >&2
  exit 1
fi
grep -Fq -- 'does not automatically inject ordinary uploaded Knowledge' "$readme"
grep -Fq -- 'does not claim Native Agent WebSocket streaming' "$readme"
grep -Fq -- 'an HTTP `agent.chat` response cannot' "$readme"
if grep -Eiq -- 'https?://agent(:|/)' "$script"; then
  echo "accept-local.sh must not bypass message-server with direct Agent HTTP" >&2
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

# Production TLS must use the verified server name for both certificate
# validation and SNI while pinning the connection to the message-server
# loopback port. Keep the validator exercised with one accepted DNS name and
# several malformed/injection-shaped values.
eval "$(sed -n '/^validate_server_name() {/,/^}/p' "$script")"
validate_server_name s1.dirextalk.ai
for invalid_server_name in \
  'https://s1.dirextalk.ai' \
  's1.dirextalk.ai:8448' \
  's1.dirextalk.ai/health' \
  '*.dirextalk.ai' \
  's1.dirextalk.ai\n--resolve attacker:8448:127.0.0.1'; do
  if validate_server_name "$invalid_server_name"; then
    echo "invalid production server name was accepted" >&2
    exit 1
  fi
done

# A canonical production bundle excludes compose.local.yaml and requires the
# host-updater override. Exercise the real
# input gate, Compose argument builder, and rendering consumer for success,
# expected-negative local input, and Docker infrastructure failure outcomes.
compose_tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-accept-compose.XXXXXX")
trap 'rm -rf -- "$compose_tmp"' EXIT
mkdir -p "$compose_tmp/bin" "$compose_tmp/bundle" "$compose_tmp/output"
: >"$compose_tmp/bundle/compose.yaml"
: >"$compose_tmp/bundle/compose.production.yaml"
: >"$compose_tmp/bundle/compose.direct-tls.yaml"
cat >"$compose_tmp/compose-wrapper.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
die() { printf '%s\n' "$*" >&2; exit 1; }
new_file() { : >"$1"; chmod 600 "$1"; }
EOF
{
  sed -n '/^require_compose_inputs() {/,/^}/p' "$script"
  sed -n '/^run_compose() {/,/^}/p' "$script"
  sed -n '/^render_compose_topology() {/,/^}/p' "$script"
} >>"$compose_tmp/compose-wrapper.sh"
cat >>"$compose_tmp/compose-wrapper.sh" <<'EOF'
compose_mode=$1
tls_mode=$2
compose_yaml=$3/compose.yaml
compose_production_yaml=$3/compose.production.yaml
compose_direct_tls_yaml=$3/compose.direct-tls.yaml
compose_local_yaml=$3/compose.local.yaml
stack_name=d-aaaaaaaaaaaaaaaaaaaaaaaaaa
env_file=$3/.env
require_compose_inputs
render_compose_topology "$4/compose.json" "$4/compose.err"
EOF
chmod 755 "$compose_tmp/compose-wrapper.sh"
cat >"$compose_tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DIREXTALK_ACCEPT_COMPOSE_LOG"
case "${DIREXTALK_ACCEPT_COMPOSE_MODE:-success}" in
  success) printf '{"services":{}}\n' ;;
  infrastructure) exit 42 ;;
esac
EOF
chmod 755 "$compose_tmp/bin/docker"
compose_log=$compose_tmp/docker.log
export DIREXTALK_ACCEPT_COMPOSE_LOG=$compose_log

: >"$compose_log"
PATH="$compose_tmp/bin:$PATH" "$compose_tmp/compose-wrapper.sh" \
  production edge-terminated "$compose_tmp/bundle" "$compose_tmp/output"
grep -Fq 'compose --project-name d-aaaaaaaaaaaaaaaaaaaaaaaaaa' "$compose_log"
grep -Fq 'compose.production.yaml' "$compose_log"
if grep -Fq 'compose.local.yaml' "$compose_log"; then
  echo "production acceptance consumed the local Compose override" >&2
  exit 1
fi

mv "$compose_tmp/bundle/compose.production.yaml" "$compose_tmp/bundle/compose.production.yaml.missing"
: >"$compose_log"
if PATH="$compose_tmp/bin:$PATH" "$compose_tmp/compose-wrapper.sh" \
    production edge-terminated "$compose_tmp/bundle" "$compose_tmp/output" >/dev/null 2>"$compose_tmp/production.error"; then
  echo "production acceptance unexpectedly accepted a missing compose.production.yaml" >&2
  exit 1
fi
grep -Fq 'compose.production.yaml is missing' "$compose_tmp/production.error"
[ ! -s "$compose_log" ] || { echo "missing production override reached Docker Compose" >&2; exit 1; }
mv "$compose_tmp/bundle/compose.production.yaml.missing" "$compose_tmp/bundle/compose.production.yaml"

: >"$compose_log"
if PATH="$compose_tmp/bin:$PATH" "$compose_tmp/compose-wrapper.sh" \
    local local "$compose_tmp/bundle" "$compose_tmp/output" >/dev/null 2>"$compose_tmp/local.error"; then
  echo "local acceptance unexpectedly accepted a missing compose.local.yaml" >&2
  exit 1
fi
grep -Fq 'compose.local.yaml is missing' "$compose_tmp/local.error"
[ ! -s "$compose_log" ] || { echo "missing local override reached Docker Compose" >&2; exit 1; }

: >"$compose_log"
if PATH="$compose_tmp/bin:$PATH" DIREXTALK_ACCEPT_COMPOSE_MODE=infrastructure \
    "$compose_tmp/compose-wrapper.sh" production edge-terminated \
    "$compose_tmp/bundle" "$compose_tmp/output" >/dev/null 2>"$compose_tmp/infrastructure.error"; then
  echo "production Compose infrastructure failure was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'Compose topology rendering failed (status 42)' "$compose_tmp/infrastructure.error"

"$provision_test" >/dev/null

echo "accept-local static contract test passed"
