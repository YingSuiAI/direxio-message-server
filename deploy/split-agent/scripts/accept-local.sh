#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

die() {
  echo "split acceptance: $*" >&2
  exit 1
}

[ "$#" -ge 1 ] && [ "$#" -le 3 ] || die "usage: $0 OUTPUT_DIR [CHAT_MODEL] [EMBEDDING_MODEL]"

out=$(readlink -m -- "$1")
chat_model=${2:-${DIREXTALK_ACCEPTANCE_CHAT_MODEL:-openai/gpt-oss-20b:free}}
embedding_model=${3:-${DIREXTALK_ACCEPTANCE_EMBEDDING_MODEL:-openai/text-embedding-3-small}}
provider=${DIREXTALK_ACCEPTANCE_PROVIDER:-openrouter}
provider_base_url=${DIREXTALK_ACCEPTANCE_BASE_URL:-https://openrouter.ai/api/v1}
timeout_seconds=${DIREXTALK_ACCEPTANCE_TIMEOUT:-180}

[ -d "$out" ] && [ ! -L "$out" ] || die "OUTPUT_DIR must be a provisioned directory"
env_file=$out/.env
manifest=$out/.manifest
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "missing protected .env"
[ -f "$manifest" ] && [ ! -L "$manifest" ] || die "missing protected .manifest"
case "$timeout_seconds" in ''|*[!0-9]*) die "DIREXTALK_ACCEPTANCE_TIMEOUT must be decimal seconds" ;; esac
[ "$timeout_seconds" -ge 10 ] && [ "$timeout_seconds" -le 600 ] || die "DIREXTALK_ACCEPTANCE_TIMEOUT must be between 10 and 600"

read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

uuid4() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
  else
    tr '[:upper:]' '[:lower:]' </proc/sys/kernel/random/uuid
  fi
}

stack_name=$(read_pair "$manifest" stack_name)
[ "$stack_name" = "$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)" ] || die "stack identity differs between .manifest and .env"
compose_mode=$(read_pair "$manifest" compose_mode)
[ "$compose_mode" = "$(read_pair "$env_file" DIREXTALK_SPLIT_COMPOSE_MODE)" ] || die "compose mode differs between .manifest and .env"
http_bind=$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTP_BIND)
portal_password=$(read_pair "$env_file" DIREXTALK_MESSAGE_PORTAL_PASSWORD_FILE)
openrouter_key=$(read_pair "$env_file" DIREXTALK_OPENROUTER_API_KEY_FILE)
embedding_key=$(read_pair "$env_file" DIREXTALK_EMBEDDING_API_KEY_FILE)
for secret_file in "$portal_password" "$openrouter_key" "$embedding_key"; do
  [ -f "$secret_file" ] && [ ! -L "$secret_file" ] && [ -s "$secret_file" ] || die "acceptance secret file is missing"
done

message_base=${DIREXTALK_ACCEPTANCE_MESSAGE_BASE_URL:-http://127.0.0.1:$http_bind}
# Both planes must cross one edge origin. Local first-fresh environments may
# point this variable at their disposable Caddy edge; a private Agent address
# is intentionally unsupported.
agent_base=$message_base
case "$message_base" in
  http://127.0.0.1:*|https://*|http://localhost:*) ;;
  *) die "DIREXTALK_ACCEPTANCE_MESSAGE_BASE_URL must be a public or local edge origin" ;;
esac

for command_name in curl jq; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done

tmp=$(mktemp -d "$out/.agent-direct-acceptance.XXXXXX")
chmod 700 "$tmp"
trap 'rm -rf -- "$tmp"' EXIT

write_file() {
  local file=$1
  : >"$file"
  chmod 600 "$file"
}

http_json() {
  local method=$1 url=$2 body=$3 output=$4 expected=$5 auth=${6:-} idem=${7:-}
  local error=$output.err status message
  write_file "$output"
  write_file "$error"
  local args=(--silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" --request "$method" --output "$output" --write-out '%{http_code}' --stderr "$error")
  [ -z "$auth" ] || args+=(--config "$auth")
  [ -z "$idem" ] || args+=(--header "Idempotency-Key: $idem")
  if [ -n "$body" ]; then
    args+=(--header 'Content-Type: application/json' --data-binary "@$body")
  fi
  if ! status=$(curl "${args[@]}" "$url"); then
    die "$method $url failed at the HTTP transport"
  fi
  [ "$status" = "$expected" ] || {
    message=$(jq -r '.message // .error // .errcode // "request failed"' "$output" 2>/dev/null || true)
    die "$method $url returned HTTP $status: ${message:0:240}"
  }
}

control_action() {
  local endpoint=$1 action=$2 params=$3 output=$4 auth=${5:-}
  local request
  request=$tmp/control-$(uuid4).json
  jq -n --arg action "$action" --slurpfile params "$params" '{action:$action,params:$params[0]}' >"$request"
  chmod 400 "$request"
  http_json POST "$message_base/_p2p/$endpoint" "$request" "$output" 200 "$auth"
}

wait_operation() {
  local operation_id=$1 output=$2 elapsed=0 state message
  while :; do
    http_json GET "$agent_base/agent/v1/operations/$operation_id" '' "$output" 200 "$ticket_auth"
    state=$(jq -r '.state // empty' "$output")
    case "$state" in
      completed) return 0 ;;
      failed|cancelled|uncertain)
        message=$(jq -r '.error.message // .result.turn.terminal_summary // "operation did not complete"' "$output")
        die "Agent operation $operation_id ended as $state: ${message:0:240}"
        ;;
      pending|running|accepted) ;;
      *) die "Agent operation $operation_id returned unknown state $state" ;;
    esac
    [ "$elapsed" -lt "$timeout_seconds" ] || die "Agent operation $operation_id timed out"
    sleep 1
    elapsed=$((elapsed + 1))
  done
}

health=$tmp/message-health.json
http_json GET "$message_base/_p2p/health" '' "$health" 200
jq -e '.status == "ok"' "$health" >/dev/null || die "Message Server health is not ok"

portal_params=$tmp/portal-params.json
jq -n --rawfile password "$portal_password" '{password:($password|rtrimstr("\n")|rtrimstr("\r"))}' >"$portal_params"
chmod 400 "$portal_params"
session=$tmp/portal-session.json
control_action query portal.bootstrap "$portal_params" "$session"
jq -e '(.access_token|type=="string" and length>0) and (.user_id|type=="string" and length>0)' "$session" >/dev/null || die "portal.bootstrap response is incomplete"

owner_auth=$tmp/owner-auth.conf
jq -r '"header = " + (("Authorization: Bearer " + .access_token) | @json)' "$session" >"$owner_auth"
chmod 400 "$owner_auth"

empty=$tmp/empty.json
printf '%s\n' '{}' >"$empty"
chmod 400 "$empty"
ticket_response=$tmp/agent-session.json
control_action command agent.session.create "$empty" "$ticket_response" "$owner_auth"
jq -e '.base_path == "/agent/v1" and (.ticket|type=="string" and length>0) and (.session_id|type=="string" and length>0)' "$ticket_response" >/dev/null || die "agent.session.create response is incomplete"

ticket_auth=$tmp/ticket-auth.conf
jq -r '"header = " + (("Authorization: Bearer " + .ticket) | @json)' "$ticket_response" >"$ticket_auth"
chmod 400 "$ticket_auth"

agent_health=$tmp/agent-health.json
http_json GET "$agent_base/agent/v1/health" '' "$agent_health" 200
jq -e '.status == "ok"' "$agent_health" >/dev/null || die "Agent data-plane health is not ok"

catalog=$tmp/catalog.json
http_json GET "$agent_base/agent/v1/catalog" '' "$catalog" 200 "$ticket_auth"
jq -e '
  any(.capabilities[]?; .capability_id == "agent.chat.v1" and .readiness == true and
    any(.operations[]?; .operation_id == "start_turn" and .type == "mutation")) and
  any(.capabilities[]?; .capability_id == "agent.models.v1" and .readiness == true and
    any(.operations[]?; .operation_id == "sync_models" and .type == "mutation"))
' "$catalog" >/dev/null || die "Agent catalog does not expose ready direct chat and model operations"

chat_client_id=$(uuid4)
embedding_client_id=$(uuid4)
model_key=$(uuid4)
model_request=$tmp/model-sync.json
jq -n --rawfile chat_key "$openrouter_key" --rawfile embedding_key "$embedding_key" \
  --arg idem "$model_key" --arg chat_client "$chat_client_id" --arg embedding_client "$embedding_client_id" \
  --arg provider "$provider" --arg base_url "$provider_base_url" --arg chat_model "$chat_model" --arg embedding_model "$embedding_model" '
  def secret: rtrimstr("\n")|rtrimstr("\r");
  {
    idempotency_key:$idem,
    default_conversation_client_profile_id:$chat_client,
    default_embedding_client_profile_id:$embedding_client,
    entries:[
      {client_profile_id:$chat_client,display_name:"Direct acceptance chat",provider:$provider,base_url:$base_url,model:$chat_model,model_kind:"conversation",api_key:($chat_key|secret)},
      {client_profile_id:$embedding_client,display_name:"Direct acceptance embedding",provider:$provider,base_url:$base_url,model:$embedding_model,model_kind:"embedding",api_key:($embedding_key|secret)}
    ]
  }' >"$model_request"
chmod 400 "$model_request"
model_receipt=$tmp/model-receipt.json
model_url=$agent_base/agent/v1/capabilities/agent.models.v1/operations/sync_models
http_json POST "$model_url" "$model_request" "$model_receipt" 202 "$ticket_auth" "$model_key"
model_operation=$(jq -r '.operation_id // empty' "$model_receipt")
[ -n "$model_operation" ] || die "model sync returned no operation_id"

model_replay=$tmp/model-replay.json
http_json POST "$model_url" "$model_request" "$model_replay" 202 "$ticket_auth" "$model_key"
jq -e --arg operation "$model_operation" '.operation_id == $operation and .replayed == true' "$model_replay" >/dev/null || die "model sync replay changed the frozen operation"

model_status=$tmp/model-status.json
wait_operation "$model_operation" "$model_status"
jq -e --arg chat "$chat_client_id" --arg embedding "$embedding_client_id" '
  any(.result.profiles[]?; .client_profile_id == $chat and .api_key_configured == true) and
  any(.result.profiles[]?; .client_profile_id == $embedding and .api_key_configured == true)
' "$model_status" >/dev/null || die "model sync did not persist both redacted profiles"
chat_profile_id=$(jq -r --arg client "$chat_client_id" '.result.profiles[] | select(.client_profile_id==$client) | .id' "$model_status" | head -n1)
chat_revision=$(jq -r --arg client "$chat_client_id" '.result.profiles[] | select(.client_profile_id==$client) | .revision' "$model_status" | head -n1)
chat_credential_version=$(jq -r --arg client "$chat_client_id" '.result.profiles[] | select(.client_profile_id==$client) | .credential_version' "$model_status" | head -n1)
[ -n "$chat_profile_id" ] && [ "$chat_revision" -gt 0 ] && [ "$chat_credential_version" -gt 0 ] || die "model sync returned invalid execution pins"

conversation_id=$(uuid4)
turn_key=$(uuid4)
marker=DIRECT_AGENT_ACCEPT_$(date +%s)_$(od -An -N3 -tx1 /dev/urandom | tr -d '[:space:]')
turn_request=$tmp/turn-request.json
jq -n --arg idem "$turn_key" --arg marker "$marker" --arg profile "$chat_profile_id" \
  --argjson revision "$chat_revision" --argjson credential "$chat_credential_version" \
  '{idempotency_key:$idem,message:("Reply with exactly " + $marker),model_profile_id:$profile,model_profile_revision:$revision,credential_version:$credential}' >"$turn_request"
chmod 400 "$turn_request"
turn_url=$agent_base/agent/v1/conversations/$conversation_id/turns
turn_receipt=$tmp/turn-receipt.json
http_json POST "$turn_url" "$turn_request" "$turn_receipt" 202 "$ticket_auth" "$turn_key"
turn_id=$(jq -r '.turn_id // empty' "$turn_receipt")
operation_id=$(jq -r '.operation_id // empty' "$turn_receipt")
[ -n "$turn_id" ] && [ "$turn_id" = "$operation_id" ] || die "turn admission did not return one authoritative operation/turn id"

turn_replay=$tmp/turn-replay.json
http_json POST "$turn_url" "$turn_request" "$turn_replay" 202 "$ticket_auth" "$turn_key"
jq -e --arg operation "$operation_id" --arg key "$turn_key" '.operation_id == $operation and .turn_id == $operation and .idempotency_key == $key and .replayed == true' "$turn_replay" >/dev/null || die "turn replay changed the frozen idempotency tuple"

# Open the independent stream immediately. A timeout represents an intentional
# client disconnect; completion before the timeout is also valid. The same
# POST is never regenerated or resubmitted during recovery.
first_stream=$tmp/turn-events-first.sse
write_file "$first_stream"
if curl --config "$ticket_auth" --silent --show-error --connect-timeout 10 --max-time 2 \
  "$agent_base/agent/v1/operations/$operation_id/events?after_seq=0" --output "$first_stream" 2>"$first_stream.err"; then
  :
else
  stream_status=$?
  [ "$stream_status" -eq 28 ] || die "initial Agent SSE stream failed"
fi

turn_status=$tmp/turn-status.json
wait_operation "$operation_id" "$turn_status"
last_sequence=$(jq -r '.sequence // 0' "$turn_status")
[ "$last_sequence" -gt 0 ] || die "completed turn has no durable event sequence"

resume_after=$((last_sequence - 1))
resumed_stream=$tmp/turn-events-resumed.sse
write_file "$resumed_stream"
if ! curl --config "$ticket_auth" --silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" \
  --header "Last-Event-ID: $resume_after" \
  "$agent_base/agent/v1/operations/$operation_id/events?after_seq=$resume_after" --output "$resumed_stream" 2>"$resumed_stream.err"; then
  die "resumed Agent SSE stream failed"
fi
grep -Fqx "id: $last_sequence" "$resumed_stream" || die "SSE recovery did not resume at the durable tail"
if awk -v after="$resume_after" '/^id: / { if (($2 + 0) <= after) exit 1; seen=1 } END { if (!seen) exit 1 }' "$resumed_stream"; then :; else
  die "SSE recovery replayed an event at or before the frozen cursor"
fi

history=$tmp/conversation-history.json
http_json GET "$agent_base/agent/v1/conversations/$conversation_id?limit=100" '' "$history" 200 "$ticket_auth"
jq -e --arg id "$conversation_id" --arg marker "$marker" '
  .conversation.conversation_id == $id and
  any(.messages[]?; .role == "user" and (.content | contains($marker))) and
  any(.messages[]?; .role == "assistant" and (.content | contains($marker)))
' "$history" >/dev/null || die "Agent authoritative history did not recover the completed conversation"

turns=$tmp/conversation-turns.json
http_json GET "$agent_base/agent/v1/conversations/$conversation_id/turns?limit=100" '' "$turns" 200 "$ticket_auth"
jq -e --arg turn "$turn_id" --arg key "$turn_key" 'any(.turns[]?; .turn_id == $turn and .idempotency_key == $key and .state == "completed")' "$turns" >/dev/null || die "Agent authoritative turn history is incomplete"

for output in "$model_receipt" "$model_replay" "$model_status" "$turn_receipt" "$turn_replay" "$turn_status" "$history" "$turns"; do
  if grep -Fq -f "$openrouter_key" "$output" || grep -Fq -f "$embedding_key" "$output"; then
    die "Agent response exposed a configured model credential"
  fi
done

printf 'direct Agent first-fresh acceptance passed: conversation=%s turn=%s\n' "$conversation_id" "$turn_id"
