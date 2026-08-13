#!/usr/bin/env bash
set -uo pipefail
umask 077

usage() { echo "usage: $0 ABSOLUTE_RUN_DIR" >&2; exit 2; }
[ "$#" -eq 1 ] || usage
run=$1
case "$run" in /*) ;; *) usage ;; esac
[ -d "$run" ] && [ ! -L "$run" ] || { echo "batch acceptance: invalid run directory" >&2; exit 2; }
for required in .env .manifest message-portal-password compose.e2e.json; do
  [ -f "$run/$required" ] && [ ! -L "$run/$required" ] || { echo "batch acceptance: missing $required" >&2; exit 2; }
done

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
stamp=$(date -u +%Y%m%dT%H%M%SZ)-$(od -An -N3 -tx1 /dev/urandom | tr -d '[:space:]')
out=$run/.accept-existing-batch.$stamp
mkdir -m 700 -- "$out" || exit 2
summary=$out/summary.tsv
: >"$summary"; chmod 600 "$summary"
timeout_seconds=${DIREXTALK_BATCH_TIMEOUT_SECONDS:-180}
chat_model=${DIREXTALK_BATCH_CHAT_MODEL:-}
failure_model=${DIREXTALK_BATCH_FAILURE_MODEL:-$chat_model}
case "$timeout_seconds" in ''|*[!0-9]*) echo "batch acceptance: invalid timeout" >&2; exit 2 ;; esac
[ "$timeout_seconds" -ge 30 ] && [ "$timeout_seconds" -le 600 ] || { echo "batch acceptance: timeout must be 30..600" >&2; exit 2; }

read_pair() {
  local file=$1 key=$2
  awk -F= -v key="$key" '$1==key {print substr($0,length(key)+2); exit}' "$file"
}
http_bind=$(read_pair "$run/.env" DIREXTALK_MESSAGE_HTTP_BIND)
stack=$(read_pair "$run/.manifest" stack_name)
http_base=http://127.0.0.1:$http_bind
compose=(docker compose --env-file "$run/.env" -f "$run/compose.e2e.json" --project-name "$stack")
session=$out/.session.json
auth=$out/.curl-auth.conf
last_response=
last_status=
failures=0

uuid4() { cat /proc/sys/kernel/random/uuid; }
seal() { chmod 400 "$@" 2>/dev/null || true; }
record() {
  local group=$1 state=$2 detail=$3
  detail=$(printf '%.400s' "$detail" | tr '\r\n\t' '   ')
  printf '%s\t%s\t%s\n' "$group" "$state" "$detail" >>"$summary"
  printf '%-28s %s %s\n' "$group" "$state" "$detail"
  [ "$state" = PASS ] || failures=$((failures + 1))
}
run_group() {
  local group=$1; shift
  local log=$out/$group.log
  if "$@" >"$log" 2>&1; then
    seal "$log"; record "$group" PASS "completed"
  else
    local status=$?
    seal "$log"; record "$group" FAIL "status=$status log=$log"
  fi
}
safe_response() {
  local file=$1
  [ -s "$file" ] || return 0
  ! grep -Eiq 'sk-or-v1-[A-Za-z0-9_-]{8,}|Bearer[[:space:]]+[A-Za-z0-9._-]{16,}' "$file"
}
call_http() {
  local action=$1 params=$2 name=$3
  local request=$out/request-$name.json response=$out/response-$name.json error=$out/curl-$name.err
  rm -f -- "$request" "$response" "$error"
  jq -n --arg action "$action" --argjson params "$params" '{action:$action,params:$params}' >"$request" || return 1
  seal "$request"
  last_status=$(curl --config "$auth" --silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" \
    --request POST --header 'Content-Type: application/json' --data-binary @"$request" \
    --output "$response" --write-out '%{http_code}' --stderr "$error" "$http_base/_p2p/query") || return 1
  seal "$response" "$error"
  safe_response "$response" || return 1
  last_response=$response
  case "$last_status" in 2??) return 0 ;; *) return 1 ;; esac
}
bootstrap() {
  local request=$out/bootstrap-request.json error=$out/bootstrap.err status
  jq -n --rawfile password "$run/message-portal-password" '{action:"portal.bootstrap",params:{password:($password|rtrimstr("\n")|rtrimstr("\r"))}}' >"$request" || return 1
  seal "$request"
  status=$(curl -sS --connect-timeout 10 --max-time "$timeout_seconds" -H 'Content-Type: application/json' \
    --data-binary @"$request" -o "$session" -w '%{http_code}' "$http_base/_p2p/query" 2>"$error") || return 1
  [ "$status" = 200 ] && jq -e '.access_token|type=="string" and length>0' "$session" >/dev/null || return 1
  jq -r '"header = " + (("Authorization: Bearer " + .access_token)|@json)' "$session" >"$auth" || return 1
  seal "$session" "$auth" "$error"
}
profile_json() {
  local model=$1
  call_http agent.model_profiles.list '{"page_size":100}' model-profiles || return 1
  jq -c --arg model "$model" '[.profiles[]? | select(.model_kind=="conversation" and .api_key_configured==true) | select($model=="" or .model==$model)] | first // empty' "$last_response"
}
chat_params() {
  local profile=$1 conversation=$2 idem=$3 message=$4
  jq -cn --argjson profile "$profile" --arg conversation "$conversation" --arg idem "$idem" --arg message "$message" \
    '{conversation_id:$conversation,idempotency_key:$idem,message:$message,model_profile_id:$profile.profile_id,model_profile_revision:$profile.revision,credential_version:$profile.credential_version}'
}
create_conversation() {
  local id=$1 title=$2
  call_http agent.chat.conversations.create "$(jq -cn --arg id "$id" --arg key "$(uuid4)" --arg title "$title" '{conversation_id:$id,idempotency_key:$key,title:$title}')" conversation-create-$id
}
delete_conversation() {
  local id=$1 revision
  call_http agent.chat.conversations.get "$(jq -cn --arg id "$id" '{conversation_id:$id,message_limit:1}')" conversation-get-cleanup-$id || return 0
  revision=$(jq -r '.conversation.revision // 0' "$last_response")
  [ "$revision" -gt 0 ] 2>/dev/null || return 0
  call_http agent.chat.conversations.delete "$(jq -cn --arg id "$id" --arg key "$(uuid4)" --argjson revision "$revision" '{conversation_id:$id,idempotency_key:$key,expected_revision:$revision}')" conversation-delete-$id >/dev/null 2>&1 || true
}

owned_conversations=()
owned_releases=()
memory_restore=
memory_fact=
memory_marker=
cleanup_owned() {
  local id release current
  for release in "${owned_releases[@]}"; do
    [ -n "$release" ] || continue
    call_http agent.static_sites.delete "$(jq -cn --arg id "$release" --arg key "$(uuid4)" '{release_id:$id,idempotency_key:$key}')" static-cleanup-$release >/dev/null 2>&1 || true
  done
  if [ -n "$memory_marker" ]; then
    call_http agent.memory.status '{}' memory-status-cleanup >/dev/null 2>&1 || true
    if [ "$last_status" = 200 ]; then
      memory_fact=$(jq -r --arg marker "$memory_marker" '.facts[]? | select((.value // "")|contains($marker)) | .id' "$last_response" | head -n1)
    fi
  fi
  if [ -n "$memory_fact" ]; then
    call_http agent.memory.facts.delete "$(jq -cn --arg id "$memory_fact" --arg key "$(uuid4)" '{fact_id:$id,idempotency_key:$key}')" memory-cleanup >/dev/null 2>&1 || true
  fi
  if [ -n "$memory_restore" ]; then
    call_http agent.memory.config.get '{}' memory-config-cleanup >/dev/null 2>&1 || true
    if [ "$last_status" = 200 ]; then
      current=$(jq -r '.revision // -1' "$last_response")
      [ "$current" -ge 0 ] 2>/dev/null && call_http agent.memory.config.update "$(jq -cn --arg key "$(uuid4)" --argjson revision "$current" --argjson enabled "$memory_restore" '{idempotency_key:$key,expected_revision:$revision,enabled:$enabled}')" memory-config-restore >/dev/null 2>&1 || true
    fi
  fi
  for id in "${owned_conversations[@]}"; do delete_conversation "$id"; done
}
trap cleanup_owned EXIT

identity_group() {
  curl -sS --max-time 20 "$http_base/_p2p/health" | jq -e '.status=="ok"' >/dev/null || return 1
  "${compose[@]}" ps --format json
  local state health
  state=$("${compose[@]}" ps agent --format json | jq -r '.State // ""')
  health=$("${compose[@]}" ps agent --format json | jq -r '.Health // ""')
  [ "$state" = running ] && [ "$health" = healthy ]
}

skills_mcp_group() {
  local extension_enabled mcp='' skills='' count=0
  extension_enabled=$(read_pair "$run/.env" DIREXTALK_CORE_EXTENSION_ENABLED)
  call_http agent.core.mcp.list '{"page_size":100}' mcp-list && mcp=$last_response || true
  call_http agent.core.skills.list '{"page_size":100}' skills-list && skills=$last_response || true
  [ -z "$mcp" ] || count=$((count + $(jq -r '(.installations // [])|length' "$mcp" 2>/dev/null || echo 0)))
  [ -z "$skills" ] || count=$((count + $(jq -r '(.installations // [])|length' "$skills" 2>/dev/null || echo 0)))
  printf 'core_extension_enabled=%s visible_installations=%s\n' "$extension_enabled" "$count"
  [ "$extension_enabled" = true ] && [ "$count" -gt 0 ]
}

http_chat_group() {
  local profile conversation params
  profile=$(profile_json "$chat_model"); [ -n "$profile" ] || return 1
  conversation=$(uuid4); owned_conversations+=("$conversation")
  create_conversation "$conversation" "Batch HTTP chat" || return 1
  params=$(chat_params "$profile" "$conversation" "$(uuid4)" 'Reply with exactly BATCH_HTTP_OK')
  call_http agent.chat "$params" http-chat || return 1
  jq -e '.text|contains("BATCH_HTTP_OK")' "$last_response" >/dev/null
}

ws_reconnect_group() {
  local profile conversation params config frames
  profile=$(profile_json "$chat_model"); [ -n "$profile" ] || return 1
  conversation=$(uuid4); owned_conversations+=("$conversation")
  create_conversation "$conversation" "Batch WS reconnect" || return 1
  params=$(chat_params "$profile" "$conversation" "$(uuid4)" 'Reply with exactly BATCH_WS_OK')
  config=$out/ws-reconnect-config.json; frames=$out/ws-reconnect.frames.jsonl
  jq -n --arg base "$http_base" --arg session "$session" --argjson params "$params" --arg output "$frames" \
    '{http_base:$base,session_file:$session,params:$params,expect:"done",reconnect:true,stop_after_reconnect:true,output_file:$output}' >"$config"
  seal "$config"
  go run "$script_dir/internal/accept-existing-ws" "$config" || return 1
  seal "$frames"
  jq -s -e 'any(.[]; .type=="server.native_agent_stream.accepted") and ([.[] | (.seq // 0)] | max) > ([.[] | select(.type=="server.native_agent_stream.accepted") | .seq] | first)' "$frames" >/dev/null
}

failed_history_group() {
  local profile conversation params config frames
  profile=$(profile_json "$failure_model"); [ -n "$profile" ] || return 1
  conversation=$(uuid4); owned_conversations+=("$conversation")
  create_conversation "$conversation" "Batch durable cancellation" || return 1
  params=$(chat_params "$profile" "$conversation" "$(uuid4)" 'Write a detailed response until this acceptance run cancels the durable turn.')
  config=$out/ws-failure-config.json; frames=$out/ws-failure.frames.jsonl
  jq -n --arg base "$http_base" --arg session "$session" --argjson params "$params" --arg output "$frames" \
    '{http_base:$base,session_file:$session,params:$params,expect:"error",reconnect:false,stop_after_accepted:true,output_file:$output}' >"$config"
  seal "$config"
  go run "$script_dir/internal/accept-existing-ws" "$config" || return 1
  seal "$frames"
  jq -s -e 'any(.[]; .event=="error" and .error_code=="canceled")' "$frames" >/dev/null || return 1
  call_http agent.chat.conversations.get "$(jq -cn --arg id "$conversation" '{conversation_id:$id,message_limit:100}')" failed-conversation-get || return 1
  jq -e --arg id "$conversation" '.conversation.conversation_id==$id' "$last_response" >/dev/null || return 1
  call_http agent.chat.turns.list "$(jq -cn --arg id "$conversation" '{conversation_id:$id,limit:100}')" failed-turns-list || return 1
  jq -e 'any(.turns[]?; .state=="canceled" and .last_sequence>0)' "$last_response" >/dev/null
}

memory_readback_group() {
  local profile conversation enabled revision marker params status fact='' updated attempt=0
  profile=$(profile_json "$chat_model"); [ -n "$profile" ] || return 1
  call_http agent.memory.config.get '{}' memory-config-before || return 1
  enabled=$(jq -r '.enabled' "$last_response"); revision=$(jq -r '.revision' "$last_response")
  if [ "$enabled" != true ]; then
    memory_restore=false
    call_http agent.memory.config.update "$(jq -cn --arg key "$(uuid4)" --argjson revision "$revision" '{idempotency_key:$key,expected_revision:$revision,enabled:true}')" memory-enable || return 1
  fi
  marker=BATCH_MEMORY_$stamp; memory_marker=$marker
  conversation=$(uuid4); owned_conversations+=("$conversation")
  create_conversation "$conversation" "Batch memory" || return 1
  params=$(chat_params "$profile" "$conversation" "$(uuid4)" "Remember this durable fact: my batch acceptance label is $marker. Reply with BATCH_MEMORY_CHAT_OK.")
  call_http agent.chat "$params" memory-create-chat || return 1
  while [ "$attempt" -lt "$timeout_seconds" ]; do
    call_http agent.memory.status '{}' memory-status-create || true; status=$last_response
    fact=$(jq -r --arg marker "$marker" '.facts[]? | select((.value // "")|contains($marker)) | .id' "$status" 2>/dev/null | head -n1)
    [ -n "$fact" ] && break
    sleep 2; attempt=$((attempt + 2))
  done
  [ -n "$fact" ] || return 1
  memory_fact=$fact
  updated=${marker}_UPDATED
  # Ignore the mutation response and prove the outcome through an independent
  # read, matching the client's lost-response/502 recovery path.
  call_http agent.memory.facts.update "$(jq -cn --arg id "$fact" --arg key "$(uuid4)" --arg value "$updated" '{fact_id:$id,idempotency_key:$key,value:$value}')" memory-update-ambiguous || true
  call_http agent.memory.status '{}' memory-status-readback || return 1
  memory_fact=$(jq -r --arg value "$updated" '.facts[]? | select(.value==$value) | .id' "$last_response" | head -n1)
  [ -n "$memory_fact" ]
}

static_site_group() {
  local profile conversation before marker params config frames published_text after release url body status
  profile=$(profile_json "$chat_model"); [ -n "$profile" ] || return 1
  call_http agent.static_sites.list '{"page_size":100}' static-list-before || return 1; before=$last_response
  conversation=$(uuid4); owned_conversations+=("$conversation")
  create_conversation "$conversation" "Batch static site" || return 1
  marker=BATCH_STATIC_SITE_$stamp
  params=$(chat_params "$profile" "$conversation" "$(uuid4)" "Use static_site_publish to publish one complete HTML page whose visible body contains $marker. After publishing, reply with the complete public URL.")
  config=$out/static-publish-config.json; frames=$out/static-publish.frames.jsonl
  jq -n --arg base "$http_base" --arg session "$session" --argjson params "$params" --arg output "$frames" \
    '{http_base:$base,session_file:$session,params:$params,expect:"done",reconnect:false,output_file:$output}' >"$config" || return 1
  seal "$config"
  go run "$script_dir/internal/accept-existing-ws" "$config" || return 1
  seal "$frames"
  published_text=$(jq -rs '[.[] | select(.event=="done") | (.data.text // empty) | select(type=="string" and length>0)] | last // empty' "$frames")
  [ -n "$published_text" ] || return 1
  call_http agent.static_sites.list '{"page_size":100}' static-list-after || return 1; after=$last_response
  release=$(jq -r --slurpfile before "$before" --arg published "$published_text" \
    '[.releases[]? | select(.release_id as $id | ([ $before[0].releases[]?.release_id ] | index($id) | not)) | select(.public_url as $url | ($published | contains($url)))] | first.release_id // empty' "$after")
  [ -n "$release" ] || return 1
  url=$(jq -r --arg id "$release" '.releases[] | select(.release_id==$id) | .public_url' "$after")
  owned_releases+=("$release")
  body=$out/static-site.html
  curl -sS --max-time 30 "$url" -o "$body" || return 1; seal "$body"
  grep -Fq "$marker" "$body" || return 1
  call_http agent.static_sites.delete "$(jq -cn --arg id "$release" --arg key "$(uuid4)" '{release_id:$id,idempotency_key:$key}')" static-delete || return 1
  jq -e --arg id "$release" '.deleted==true and .release_id==$id' "$last_response" >/dev/null || return 1
  owned_releases=()
  status=$(curl -sS --max-time 30 -o "$out/static-after-delete.body" -w '%{http_code}' "$url") || return 1
  seal "$out/static-after-delete.body"
  [ "$status" = 404 ]
}

if ! bootstrap; then
  record bootstrap FAIL "could not create protected owner session; logs=$out"
  seal "$summary"
  exit 1
fi
record bootstrap PASS "owner session created"
run_group identity identity_group
run_group skills_mcp skills_mcp_group
run_group http_chat http_chat_group
run_group ws_stream_reconnect ws_reconnect_group
run_group durable_failed_history failed_history_group
run_group memory_ambiguous_readback memory_readback_group
run_group static_site_lifecycle static_site_group
cleanup_owned
trap - EXIT
seal "$summary"
chmod 600 "$session" "$auth" 2>/dev/null || true
: >"$session"; : >"$auth"; seal "$session" "$auth"
printf 'batch acceptance summary: failures=%d output=%s\n' "$failures" "$out"
[ "$failures" -eq 0 ]
