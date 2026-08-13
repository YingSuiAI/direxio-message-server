#!/usr/bin/env bash
# shellcheck disable=SC2016
set -Eeuo pipefail
umask 077

usage() {
  echo "usage: $0 OUTPUT_DIR [CHAT_MODEL] [EMBEDDING_MODEL]" >&2
  exit 2
}

die() {
  echo "split acceptance: $*" >&2
  exit 1
}

[ "$#" -ge 1 ] || usage
out_input=$1
chat_model=
embedding_model=
[ "$#" -ge 2 ] && chat_model=$2
[ "$#" -ge 3 ] && embedding_model=$3

env_or() {
  local value
  value=$(printenv "$1" 2>/dev/null || true)
  [ -n "$value" ] || value=$2
  printf '%s' "$value"
}

[ -n "$chat_model" ] || chat_model=$(env_or DIREXTALK_ACCEPTANCE_CHAT_MODEL openai/gpt-oss-20b:free)
[ -n "$embedding_model" ] || embedding_model=$(env_or DIREXTALK_ACCEPTANCE_EMBEDDING_MODEL openai/text-embedding-3-small)
provider=$(env_or DIREXTALK_ACCEPTANCE_PROVIDER openrouter)
base_url=$(env_or DIREXTALK_ACCEPTANCE_BASE_URL https://openrouter.ai/api/v1)
compose_mode=$(env_or DIREXTALK_ACCEPTANCE_COMPOSE_MODE local)
timeout_seconds=$(env_or DIREXTALK_ACCEPTANCE_TIMEOUT 180)
cleanup_after=$(env_or DIREXTALK_ACCEPTANCE_CLEANUP_AFTER false)
account_delete_enabled=$(env_or DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE true)

case "$compose_mode" in local|production) ;; *) die "DIREXTALK_ACCEPTANCE_COMPOSE_MODE must be local or production" ;; esac
case "$cleanup_after" in true|false) ;; *) die "DIREXTALK_ACCEPTANCE_CLEANUP_AFTER must be true or false" ;; esac
case "$account_delete_enabled" in true|false) ;; *) die "DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE must be true or false" ;; esac
[ "$account_delete_enabled" = true ] || [ "$cleanup_after" = false ] || die "DIREXTALK_ACCEPTANCE_CLEANUP_AFTER=true is incompatible with account deletion disabled"
case "$timeout_seconds" in ''|*[!0-9]*) die "DIREXTALK_ACCEPTANCE_TIMEOUT must be decimal seconds" ;; esac
[ "$timeout_seconds" -ge 10 ] 2>/dev/null && [ "$timeout_seconds" -le 600 ] 2>/dev/null || die "DIREXTALK_ACCEPTANCE_TIMEOUT must be between 10 and 600"

case "$out_input" in
  /*) out=$(readlink -m -- "$out_input") ;;
  *) out=$(readlink -m -- "$(pwd -P)/$out_input") ;;
esac
[ -d "$out" ] && [ ! -L "$out" ] || die "OUTPUT_DIR must be a provisioned regular directory"
[ "$(stat -c '%a' "$out" 2>/dev/null || true)" = 700 ] || die "OUTPUT_DIR must be mode 0700"
env_file=$out/.env
manifest=$out/.manifest
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "missing protected .env"
[ -f "$manifest" ] && [ ! -L "$manifest" ] || die "missing protected .manifest"
[ "$(stat -c '%a' "$env_file" 2>/dev/null || true)" = 400 ] || die ".env must be mode 0400"
[ "$(stat -c '%a' "$manifest" 2>/dev/null || true)" = 400 ] || die ".manifest must be mode 0400"

read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

stack_name=$(read_pair "$manifest" stack_name)
env_stack=$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)
[ "$stack_name" = "$env_stack" ] || die "stack identity differs between .manifest and .env"
manifest_compose_mode=$(read_pair "$manifest" compose_mode)
env_compose_mode=$(read_pair "$env_file" DIREXTALK_SPLIT_COMPOSE_MODE)
case "$manifest_compose_mode" in local|production) ;; *) die "manifest compose mode is invalid" ;; esac
[ "$env_compose_mode" = "$manifest_compose_mode" ] || die "compose mode differs between .manifest and .env"
[ "$compose_mode" = "$manifest_compose_mode" ] || die "DIREXTALK_ACCEPTANCE_COMPOSE_MODE differs from the provisioned stack"
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "stack identity is not a fresh namespace"
http_bind=$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTP_BIND)
https_bind=$(read_pair "$env_file" DIREXTALK_MESSAGE_HTTPS_BIND)
tls_mode=$(read_pair "$env_file" DIREXTALK_MESSAGE_TLS_MODE)
manifest_tls_mode=$(read_pair "$manifest" message_tls_mode)
[ "$tls_mode" = "$manifest_tls_mode" ] || die "message TLS mode differs between .manifest and .env"
case "$tls_mode" in local|external|edge-terminated) ;; *) die "message TLS mode is invalid" ;; esac
[ "$compose_mode" = production ] || [ "$tls_mode" != edge-terminated ] || die "edge-terminated TLS is production-only"
case "$http_bind" in ''|*[!0-9]*) die "invalid HTTP host bind" ;; esac
case "$https_bind" in ''|*[!0-9]*) die "invalid HTTPS host bind" ;; esac
[ "$http_bind" != "$https_bind" ] || die "HTTP and HTTPS host binds must differ"
http_base=http://127.0.0.1:$http_bind

validate_server_name() {
  local value=$1
  printf '%s\n' "$value" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$'
}

message_server_name=
https_resolve_host=
if [ "$compose_mode" = production ]; then
  message_server_name=$(read_pair "$env_file" DIREXTALK_MESSAGE_SERVER_NAME)
  validate_server_name "$message_server_name" || die "DIREXTALK_MESSAGE_SERVER_NAME must be a DNS host name without a scheme, port, or wildcard"
  https_base=https://$message_server_name:$https_bind
  https_resolve_host=$message_server_name
else
  https_base=https://127.0.0.1:$https_bind
fi

openrouter_key=$(read_pair "$env_file" DIREXTALK_OPENROUTER_API_KEY_FILE)
embedding_key=$(read_pair "$env_file" DIREXTALK_EMBEDDING_API_KEY_FILE)
tavily_key=$(read_pair "$env_file" DIREXTALK_TAVILY_API_KEY_FILE)
portal_password=$(read_pair "$env_file" DIREXTALK_MESSAGE_PORTAL_PASSWORD_FILE)
agent_config=$(read_pair "$env_file" DIREXTALK_AGENT_CONFIG_FILE)
[ "$openrouter_key" = "$out/openrouter-api-key" ] || die "OpenRouter key path is outside the provisioned stack"
[ "$embedding_key" = "$out/embedding-api-key" ] || die "embedding key path is outside the provisioned stack"
[ "$tavily_key" = "$out/tavily-api-key" ] || die "Tavily key path is outside the provisioned stack"
[ "$portal_password" = "$out/message-portal-password" ] || die "portal password path is outside the provisioned stack"
[ "$agent_config" = "$out/agent-config.yaml" ] || die "Agent config path is outside the provisioned stack"

check_resource_name() {
  local key=$1 expected=$2 value
  value=$(read_pair "$env_file" "$key")
  [ "$value" = "$expected" ] || die "$key is not owned by this fresh stack"
}
check_resource_name DIREXTALK_MESSAGE_PRIVATE_NETWORK "$stack_name-message-private"
check_resource_name DIREXTALK_MESSAGE_PUBLIC_NETWORK "$stack_name-message-public"
check_resource_name DIREXTALK_MESSAGE_DATABASE_NETWORK "$stack_name-message-db"
check_resource_name DIREXTALK_AGENT_PRIVATE_NETWORK "$stack_name-agent-private"
check_resource_name DIREXTALK_AGENT_DATABASE_NETWORK "$stack_name-agent-db"
check_resource_name DIREXTALK_AGENT_CALLER_NETWORK "$stack_name-agent-caller"
check_resource_name DIREXTALK_AGENT_EGRESS_NETWORK "$stack_name-agent-egress"

check_secret_file() {
  local label=$1 file=$2
  [ -f "$file" ] && [ ! -L "$file" ] || die "$label is not a regular non-symlink file"
  [ "$(stat -c '%u' "$file" 2>/dev/null || true)" = "$(id -u)" ] || die "$label has the wrong owner"
  [ "$(stat -c '%a' "$file" 2>/dev/null || true)" = 400 ] || die "$label must be mode 0400"
  [ -s "$file" ] || die "$label is empty"
}
check_secret_file "OpenRouter key file" "$openrouter_key"
check_secret_file "embedding key file" "$embedding_key"
check_secret_file "Tavily key file" "$tavily_key"
check_secret_file "portal password file" "$portal_password"
[ -f "$agent_config" ] && [ ! -L "$agent_config" ] || die "Agent config is not a regular file"
[ "$(stat -c '%a' "$agent_config" 2>/dev/null || true)" = 400 ] || die "Agent config must be mode 0400"
embedding_dimension=$(awk -F: '$1 == "core_knowledge_vector_dimension" { value=$2; sub(/^[[:space:]]+/, "", value); print value; exit }' "$agent_config")
case "$embedding_dimension" in ''|0*|*[!0-9]*) die "Agent config has an invalid embedding dimension" ;; esac

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
deploy_dir=$(cd -- "$script_dir/.." && pwd -P)
compose_yaml=$deploy_dir/compose.yaml
compose_production_yaml=$deploy_dir/compose.production.yaml
compose_direct_tls_yaml=$deploy_dir/compose.direct-tls.yaml
compose_local_yaml=$deploy_dir/compose.local.yaml
require_compose_inputs() {
  [ -f "$compose_yaml" ] || die "compose.yaml is missing"
  [ -f "$compose_direct_tls_yaml" ] || die "compose.direct-tls.yaml is missing"
  if [ "$compose_mode" = production ]; then
    [ -f "$compose_production_yaml" ] || die "compose.production.yaml is missing"
  else
    [ -f "$compose_local_yaml" ] || die "compose.local.yaml is missing"
  fi
}
require_compose_inputs
command -v docker >/dev/null 2>&1 || die "docker is required"
command -v jq >/dev/null 2>&1 || die "jq is required"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"

run_compose() {
  local compose=(docker compose --project-name "$stack_name" --env-file "$env_file" -f "$compose_yaml")
  if [ "$compose_mode" = production ]; then
    compose+=(-f "$compose_production_yaml")
  fi
  [ "$tls_mode" = edge-terminated ] || compose+=(-f "$compose_direct_tls_yaml")
  if [ "$compose_mode" = local ]; then
    compose+=(-f "$compose_local_yaml")
  fi
  "${compose[@]}" "$@"
}

tmp=$(mktemp -d "$out/.acceptance.XXXXXX")
chmod 700 "$tmp"
cleanup_tmp() {
  rm -rf -- "$tmp"
}
trap cleanup_tmp EXIT

new_file() {
  local file=$1
  if [ -e "$file" ] || [ -L "$file" ]; then
    [ -f "$file" ] && [ ! -L "$file" ] || die "acceptance workspace file is not a regular non-symlink file"
    chmod 600 "$file"
  fi
  : >"$file"
  # Callers write through curl, jq, psql, or shell redirection before sealing
  # request/session artifacts. The private 0700 workspace plus mode 0600 keeps
  # in-flight output protected without making the file unwritable.
  chmod 600 "$file"
}
render_compose_topology() {
  local output=$1 error=$2 status
  new_file "$output"
  new_file "$error"
  if run_compose config --format json >"$output" 2>"$error"; then
    :
  else
    status=$?
    die "Compose topology rendering failed (status $status)"
  fi
}
uuid4() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
  else
    cat /proc/sys/kernel/random/uuid
  fi
}
write_chat_params() {
  local file=$1 conversation_id=$2 idempotency_key=$3 message=$4
  new_file "$file"
  if [ -n "$conversation_id" ]; then
    jq -n --arg conversation "$conversation_id" --arg idem "$idempotency_key" --arg message "$message" \
      --arg profile "$chat_model_profile_id" --argjson revision "$chat_model_profile_revision" \
      --argjson credential "$chat_credential_version" \
      '{conversation_id:$conversation,idempotency_key:$idem,model_profile_id:$profile,model_profile_revision:$revision,credential_version:$credential,message:$message}' \
      >"$file"
  else
    jq -n --arg idem "$idempotency_key" --arg message "$message" \
      --arg profile "$chat_model_profile_id" --argjson revision "$chat_model_profile_revision" \
      --argjson credential "$chat_credential_version" \
      '{idempotency_key:$idem,model_profile_id:$profile,model_profile_revision:$revision,credential_version:$credential,message:$message}' \
      >"$file"
  fi
  chmod 400 "$file"
}
assert_secret_absent() {
  local file=$1
  if [ -s "$file" ] && grep -F -q -f "$openrouter_key" "$file"; then
    die "OpenRouter key appeared in an output file"
  fi
  if [ -s "$file" ] && grep -F -q -f "$embedding_key" "$file"; then
    die "embedding key appeared in an output file"
  fi
  if [ -s "$file" ] && grep -F -q -f "$tavily_key" "$file"; then
    die "Tavily key appeared in an output file"
  fi
}
assert_clean_response() {
  local file=$1
  assert_secret_absent "$file"
  if grep -Eiq 'sk-or-v1-[A-Za-z0-9_-]{8,}|Bearer[[:space:]]+[A-Za-z0-9._-]{16,}' "$file"; then
    die "credential-like material appeared in a response"
  fi
}

auth_file=$tmp/curl-auth.conf
session_file=$tmp/session.json
new_file "$auth_file"
new_file "$session_file"
last_response=
last_status=

call() {
  local action=$1 params_file=$2 name=$3 expected=false
  [ "$#" -ge 4 ] && expected=$4
  local request=$tmp/request-$name.json
  local response=$tmp/response-$name.json
  local curl_error=$tmp/curl-$name.err
  new_file "$request"
  new_file "$response"
  new_file "$curl_error"
  jq -n --arg action "$action" --slurpfile params "$params_file" '{action:$action,params:$params[0]}' >"$request"
  chmod 400 "$request"
  if last_status=$(curl --config "$auth_file" --silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" \
    --request POST --header 'Content-Type: application/json' --data-binary @"$request" \
    --output "$response" --write-out '%{http_code}' --stderr "$curl_error" "$http_base/_p2p/query"); then
    :
  else
    die "HTTP request failed for $action (details are in the protected acceptance workspace)"
  fi
  case "$last_status" in
    2??) ;;
    400|401|403|409|412|428)
      if [ "$expected" != true ]; then
        assert_clean_response "$response"
        response_error=$(jq -r 'if type == "object" then (.error // .message // .errcode // "request failed") else "request failed" end | tostring' "$response" 2>/dev/null || printf '%s' 'request failed')
        response_error=$(printf '%.300s' "$response_error" | tr '\r\n' '  ')
        die "$action returned HTTP $last_status: $response_error"
      fi
      assert_clean_response "$response"
      last_response=$response
      return 0
      ;;
    *)
      assert_clean_response "$response"
      response_error=$(jq -r 'if type == "object" then (.error // .message // .errcode // "request failed") else "request failed" end | tostring' "$response" 2>/dev/null || printf '%s' 'request failed')
      response_error=$(printf '%.300s' "$response_error" | tr '\r\n' '  ')
      die "$action returned unexpected HTTP $last_status: $response_error"
      ;;
  esac
  assert_clean_response "$response"
  last_response=$response
}

health_check() {
  local base=$1 name=$2 insecure=$3 resolve_host=${4:-} status='' request_ok=false elapsed=0
  local response=$tmp/health-$name.json error=$tmp/health-$name.err
  new_file "$response"
  new_file "$error"
  while :; do
    status=
    request_ok=false
    if [ "$insecure" = true ]; then
      if status=$(curl --insecure --silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" \
        --output "$response" --write-out '%{http_code}' --stderr "$error" "$base/_p2p/health"); then
        request_ok=true
      fi
    elif [ -n "$resolve_host" ]; then
      if status=$(curl --silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" \
        --resolve "${resolve_host}:${https_bind}:127.0.0.1" \
        --output "$response" --write-out '%{http_code}' --stderr "$error" "$base/_p2p/health"); then
        request_ok=true
      fi
    elif status=$(curl --silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" \
      --output "$response" --write-out '%{http_code}' --stderr "$error" "$base/_p2p/health"); then
      request_ok=true
    fi
    if [ "$request_ok" = true ] && [ "$status" = 200 ] && jq -e '.status == "ok"' "$response" >/dev/null 2>&1; then
      assert_clean_response "$response"
      return 0
    fi
    [ ! -s "$response" ] || assert_clean_response "$response"
    [ "$elapsed" -lt "$timeout_seconds" ] || die "$name health did not become ready (last HTTP ${status:-transport_error})"
    sleep 1
    elapsed=$((elapsed + 1))
  done
}
health_check "$http_base" http false
if [ "$tls_mode" != edge-terminated ]; then
  https_insecure=true
  [ "$compose_mode" = production ] && https_insecure=false
  health_check "$https_base" https "$https_insecure" "$https_resolve_host"
fi

topology=$tmp/compose.json
topology_error=$tmp/compose.err
render_compose_topology "$topology" "$topology_error"
jq -e --arg http "$http_bind" --arg https "$https_bind" --arg tls_mode "$tls_mode" '
  ([.services | to_entries[] | select((.value.ports // []) | length > 0) | .key] | length == 1 and .[0] == "message-server") and
  ([.services["message-server"].ports[]?.published | tostring] | sort == (if $tls_mode == "edge-terminated" then [$http] else ([$http,$https] | sort) end)) and
  (all(.services | to_entries[]; .key == "message-server" or ((.value.ports // []) | length == 0))) and
  ((.networks.message_private.internal // false) == true) and
  ((.networks.message_database.internal // false) == true) and
  ((.networks.agent_private.internal // false) == true) and
  ((.networks.agent_database.internal // false) == true) and
  ((.networks.agent_caller.internal // false) == true) and
  ((.networks.agent_egress.internal // false) == false) and
  ((.networks.message_public.internal // false) == false) and
  ([.services | to_entries[] | select(.value.networks | has("message_public")) | .key] == ["message-server"]) and
  ([.services | to_entries[] | select(.value.networks | has("agent_egress")) | .key] == ["agent"]) and
  (.services["extension-runner"].cgroup == "host") and
  (.services["extension-runner"].cpus == 2) and
  (.services["extension-runner"].mem_limit == "1073741824") and
  (.services["extension-runner"].pids_limit == 256) and
  (.services["core-runner"].cgroup == "host") and
  ([.services.agent.volumes[] | select(.target == "/var/lib/dirextalk-agent/extension-workspaces" and .source == "agent_runner_workspaces")] | length == 1) and
  ([.services["extension-runner"].volumes[] | select(.target == "/var/lib/dirextalk-agent/extension-workspaces" and .source == "agent_runner_workspaces")] | length == 1) and
  ((.services.postgres.networks | keys) == ["agent_database","message_database"]) and
  ((.services.agent.networks | has("message_database")) | not) and
  ((.services["message-server"].networks | has("agent_database")) | not) and
  ([.services | to_entries[] | select(any(.value.secrets[]?; .source == "postgres_admin_password")) | .key] == ["postgres"])
' "$topology" >/dev/null || die "Compose topology violates public-port or private-network isolation"

portal_request=$tmp/portal-request.json
portal_error=$tmp/portal.err
new_file "$portal_request"
new_file "$portal_error"
jq -n --rawfile password "$portal_password" \
  '{action:"portal.bootstrap",params:{password:($password|rtrimstr("\n")|rtrimstr("\r"))}}' >"$portal_request"
chmod 400 "$portal_request"
if portal_status=$(curl --silent --show-error --connect-timeout 10 --max-time "$timeout_seconds" \
  --request POST --header 'Content-Type: application/json' --data-binary @"$portal_request" \
  --output "$session_file" --write-out '%{http_code}' --stderr "$portal_error" "$http_base/_p2p/query"); then
  :
else
  die "portal.bootstrap HTTP request failed"
fi
[ "$portal_status" = 200 ] || die "portal.bootstrap returned HTTP $portal_status"
chmod 400 "$session_file" "$portal_request"
jq -e 'type == "object" and (.access_token|type=="string" and length>0) and (.user_id|type=="string" and length>0) and (.agent_room_id|type=="string" and length>0)' "$session_file" >/dev/null || die "portal.bootstrap response is incomplete"
assert_clean_response "$session_file"
jq -r '"header = " + (("Authorization: Bearer " + .access_token) | @json)' "$session_file" >"$auth_file"
chmod 400 "$auth_file"

agent_container=
agent_health() {
  agent_container=$(run_compose ps -q agent 2>"$tmp/agent-ps.err" || true)
  [ -n "$agent_container" ] || return 1
  health=$(docker inspect -f '{{.State.Health.Status}}' "$agent_container" 2>"$tmp/agent-inspect.err" || true)
  [ "$health" = healthy ]
}
i=0
while ! agent_health; do
  [ "$i" -lt "$timeout_seconds" ] || die "Agent did not become healthy"
  sleep 2
  i=$((i + 2))
done

db_query() {
  local service=$1 user=$2 database=$3 sql=$4 target=$5 secret
  local error=$tmp/db-$service.err
  case "$service" in
    agent-postgres) secret=agent_postgres_password ;;
    message-postgres) secret=message_postgres_password ;;
    *) die "unsupported database service $service" ;;
  esac
  new_file "$target"
  new_file "$error"
  if run_compose exec -T postgres sh -ec 'password=$(cat "$1"); shift; PGPASSWORD="$password" psql -h 127.0.0.1 -At -U "$1" -d "$2" -c "$3"' sh "/run/dirextalk-postgres-secrets/$secret" "$user" "$database" "$sql" >"$target" 2>"$error"; then
    :
  else
    die "database query failed for $service"
  fi
}

db_query_expect_denied() {
  local service=$1 user=$2 database=$3 sql=$4 secret status
  case "$service" in
    agent-postgres) secret=agent_postgres_password ;;
    message-postgres) secret=message_postgres_password ;;
    *) die "unsupported database service $service" ;;
  esac
  if run_compose exec -T postgres sh -ec 'password=$(cat "$1"); shift; PGPASSWORD="$password" psql -h 127.0.0.1 -At -U "$1" -d "$2" -c "$3"' sh "/run/dirextalk-postgres-secrets/$secret" "$user" "$database" "$sql" >"$tmp/db-denied.out" 2>"$tmp/db-denied.err"; then
    die "$service unexpectedly accessed $database"
  else
    status=$?
    [ "$status" -ne 125 ] || die "database isolation check infrastructure failed"
  fi
}

db_query agent-postgres dirextalk_agent dirextalk_agent \
  "SELECT rolsuper::text || '|' || rolcreatedb::text || '|' || rolcreaterole::text || '|' || rolinherit::text FROM pg_roles WHERE rolname=current_user;" \
  "$tmp/agent-role-flags.out"
[ "$(tr -d '[:space:]' <"$tmp/agent-role-flags.out")" = 'false|false|false|false' ] || die "Agent database role is overprivileged"
db_query message-postgres dirextalk_message_server dirextalk_message_server \
  "SELECT rolsuper::text || '|' || rolcreatedb::text || '|' || rolcreaterole::text || '|' || rolinherit::text FROM pg_roles WHERE rolname=current_user;" \
  "$tmp/message-role-flags.out"
[ "$(tr -d '[:space:]' <"$tmp/message-role-flags.out")" = 'false|false|false|false' ] || die "message-server database role is overprivileged"
db_query_expect_denied agent-postgres dirextalk_agent dirextalk_message_server 'SELECT 1;'
db_query_expect_denied message-postgres dirextalk_message_server dirextalk_agent 'SELECT 1;'
db_query_expect_denied agent-postgres dirextalk_agent dirextalk_agent 'SET ROLE dirextalk_message_server;'
db_query_expect_denied message-postgres dirextalk_message_server dirextalk_message_server 'SET ROLE dirextalk_agent;'
db_query agent-postgres dirextalk_agent dirextalk_agent \
  "SELECT count(*) FROM pg_extension WHERE extname = 'vector';" "$tmp/agent-vector-extension.count"
[ "$(tr -d '[:space:]' <"$tmp/agent-vector-extension.count")" = 1 ] || die "Agent database does not have exactly one pgvector extension"
db_query agent-postgres dirextalk_agent dirextalk_agent \
  "SELECT vector_dims('[1,2,3]'::vector);" "$tmp/agent-vector-dimensions.out"
[ "$(tr -d '[:space:]' <"$tmp/agent-vector-dimensions.out")" = 3 ] || die "Agent database pgvector operations failed"
db_query message-postgres dirextalk_message_server dirextalk_message_server \
  "SELECT count(*) FROM pg_extension WHERE extname = 'vector';" "$tmp/message-vector-extension.count"
[ "$(tr -d '[:space:]' <"$tmp/message-vector-extension.count")" = 0 ] || die "pgvector was installed in the message-server database"

params=$tmp/empty.params.json
new_file "$params"
printf '%s\n' '{}' >"$params"
call agent.backends.get "$params" backends
jq -e 'type=="object"' "$last_response" >/dev/null || die "Agent status response is not an object"

call agent.web_search.config.get "$params" web-search-config-get
jq -e '.provider == "tavily" and (.api_key_configured | type == "boolean") and
  ((.revision // -1) >= 0) and ((has("api_key") | not))' "$last_response" >/dev/null || \
  die "web search config.get did not return the current safe Tavily state"
web_search_revision=$(jq -r '.revision' "$last_response")
web_search_update=$tmp/web-search-config-update.params.json
web_search_update_key=$(uuid4)
jq -n --rawfile api_key "$tavily_key" --arg idem "$web_search_update_key" --argjson revision "$web_search_revision" \
  '{idempotency_key:$idem,expected_revision:$revision,enabled:true,provider:"tavily",api_key:($api_key|rtrimstr("\n")|rtrimstr("\r"))}' >"$web_search_update"
chmod 400 "$web_search_update"
call agent.web_search.config.update "$web_search_update" web-search-config-update
jq -e '.enabled == true and .provider == "tavily" and .api_key_configured == true and ((.revision // 0) > 0)' "$last_response" >/dev/null || \
  die "web search config.update did not configure Tavily"
web_search_updated_revision=$(jq -r '.revision' "$last_response")
[ "$web_search_updated_revision" -gt "$web_search_revision" ] 2>/dev/null || die "web search config.update did not advance revision"
call agent.web_search.config.get "$params" web-search-config-safe-get
jq -e --argjson revision "$web_search_updated_revision" \
  '.enabled == true and .provider == "tavily" and .api_key_configured == true and .revision == $revision and ((has("api_key") | not))' \
  "$last_response" >/dev/null || die "web search config.get did not preserve the configured safe projection"
call agent.web_search.test "$params" web-search-test
jq -e --argjson revision "$web_search_updated_revision" \
  '.ok == true and .provider == "tavily" and (.result_count // -1) >= 0 and .enabled == true and .api_key_configured == true and .revision == $revision' \
  "$last_response" >/dev/null || die "web search test did not succeed with the stored Tavily credential"

model_catalog_params=$tmp/model-catalog.params.json
new_file "$model_catalog_params"
jq -n --rawfile api_key "$openrouter_key" --arg provider "$provider" --arg base "$base_url" \
  '{provider:$provider,base_url:$base,model_kind:"conversation",api_key:($api_key|rtrimstr("\n")|rtrimstr("\r"))}' \
  >"$model_catalog_params"
chmod 400 "$model_catalog_params"
call agent.models.list "$model_catalog_params" model-catalog
jq -e '
  (.models | type == "array" and length > 0) and
  (.providers | type == "array" and length > 0) and
  all(.models[]; (.id | type == "string" and length > 0) and (.provider | type == "string" and length > 0))
' "$last_response" >/dev/null || die "OpenRouter model catalog returned no canonical conversation models"

chat_profile=$(uuid4)
embedding_profile=$(uuid4)
sync_key=$(uuid4)
chat_key=$openrouter_key
embed_key=$embedding_key
model_params=$tmp/model-sync.params.json
new_file "$model_params"
jq -n --rawfile ck "$chat_key" --rawfile ek "$embed_key" \
  --arg chat_id "$chat_profile" --arg embedding_id "$embedding_profile" \
  --arg provider "$provider" --arg base "$base_url" --arg chat_model "$chat_model" --arg embedding_model "$embedding_model" \
  --arg idem "$sync_key" '
  def secret: rtrimstr("\n")|rtrimstr("\r");
  {
    idempotency_key:$idem,
    default_client_profile_id:$chat_id,
    default_conversation_client_profile_id:$chat_id,
    default_embedding_client_profile_id:$embedding_id,
    entries:[
      {client_profile_id:$chat_id,display_name:"OpenRouter acceptance chat",provider:$provider,base_url:$base,model:$chat_model,model_kind:"conversation",api_key:($ck|secret)},
      {client_profile_id:$embedding_id,display_name:"OpenRouter acceptance embedding",provider:$provider,base_url:$base,model:$embedding_model,model_kind:"embedding",api_key:($ek|secret)}
    ]
  }' >"$model_params"
chmod 400 "$model_params"
call agent.model_profiles.sync "$model_params" model-sync
jq -e --arg chat "$chat_profile" --arg embed "$embedding_profile" '
  (.profiles | type == "array") and
  (any(.profiles[]; .client_profile_id == $chat and .api_key_configured == true)) and
  (any(.profiles[]; .client_profile_id == $embed and .api_key_configured == true))
' "$last_response" >/dev/null || die "model sync did not return both configured profiles"
chat_internal=$(jq -r --arg id "$chat_profile" '.profiles[] | select(.client_profile_id==$id) | .profile_id' "$last_response" | head -n 1)
embed_internal=$(jq -r --arg id "$embedding_profile" '.profiles[] | select(.client_profile_id==$id) | .profile_id' "$last_response" | head -n 1)
printf '%s\n' "$chat_internal" | grep -Eiq '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' || die "chat profile response has no internal profile id"
printf '%s\n' "$embed_internal" | grep -Eiq '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' || die "embedding profile response has no internal profile id"
stored_catalog_params=$tmp/stored-model-catalog.params.json
new_file "$stored_catalog_params"
jq -n --arg client_profile "$chat_profile" \
  '{client_model_profile_id:$client_profile,model_kind:"conversation"}' >"$stored_catalog_params"
chmod 400 "$stored_catalog_params"
call agent.models.list "$stored_catalog_params" stored-model-catalog
jq -e '
  (.models | type == "array" and length > 0) and
  all(.models[]; (.id | type == "string" and length > 0) and (.provider | type == "string" and length > 0))
' "$last_response" >/dev/null || die "stored OpenRouter profile model catalog returned no canonical conversation models"

# Read back the persisted profile through the same public profile surface used
# by Flutter. Chat requests must carry only this server-owned triple; never
# infer revisions or credential versions from the sync request.
profile_list_params=$tmp/model-profile-list.params.json
jq -n '{page_size:100}' >"$profile_list_params"
chmod 400 "$profile_list_params"
call agent.model_profiles.list "$profile_list_params" model-profile-list
jq -e --arg client "$chat_profile" '
  ([.profiles[]? | select(.client_profile_id == $client)] | length == 1) and
  (any(.profiles[]?; .client_profile_id == $client and
    (.profile_id | type == "string" and length > 0) and
    (.revision | type == "number" and . > 0) and
    (.credential_version | type == "number" and . > 0) and
    .api_key_configured == true and (has("api_key") | not)))
' "$last_response" >/dev/null || die "model profile list did not return one redacted configured chat profile"
chat_model_profile_id=$(jq -r --arg client "$chat_profile" '.profiles[] | select(.client_profile_id == $client) | .profile_id' "$last_response" | head -n 1)
chat_model_profile_revision=$(jq -r --arg client "$chat_profile" '.profiles[] | select(.client_profile_id == $client) | .revision' "$last_response" | head -n 1)
chat_credential_version=$(jq -r --arg client "$chat_profile" '.profiles[] | select(.client_profile_id == $client) | .credential_version' "$last_response" | head -n 1)
printf '%s\n' "$chat_model_profile_id" | grep -Eiq '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' || die "model profile list returned no canonical chat profile id"
case "$chat_model_profile_revision" in ''|*[!0-9]*) die "model profile list returned an invalid chat profile revision" ;; esac
[ "$chat_model_profile_revision" -gt 0 ] 2>/dev/null || die "chat profile revision is not positive"
case "$chat_credential_version" in ''|*[!0-9]*) die "model profile list returned an invalid chat credential version" ;; esac
[ "$chat_credential_version" -gt 0 ] 2>/dev/null || die "chat credential version is not positive"

profile_get_params=$tmp/model-profile-get.params.json
jq -n --arg profile "$chat_model_profile_id" '{profile_id:$profile}' >"$profile_get_params"
chmod 400 "$profile_get_params"
call agent.model_profiles.get "$profile_get_params" model-profile-get
jq -e --arg profile "$chat_model_profile_id" --arg client "$chat_profile" \
  --argjson revision "$chat_model_profile_revision" --argjson credential "$chat_credential_version" \
  '.profile.profile_id == $profile and .profile.client_profile_id == $client and
   .profile.api_key_configured == true and (.profile | has("api_key") | not) and
   .profile.revision == $revision and .profile.credential_version == $credential' \
  "$last_response" >/dev/null || die "model profile get did not return a redacted configured chat profile"
config_params=$tmp/config.params.json
printf '%s\n' '{}' >"$config_params"
chmod 400 "$config_params"
call agent.knowledge.config.get "$config_params" knowledge-config
jq -e --arg embed "$embed_internal" --argjson dimension "$embedding_dimension" \
  '.embedding_profile_id == $embed and .dimension == $dimension' "$last_response" >/dev/null || \
  die "knowledge embedding profile/dimension does not match the fresh stack configuration"

chat_idempotency_key=$(uuid4)
chat_params=$tmp/chat.params.json
write_chat_params "$chat_params" '' "$chat_idempotency_key" 'Reply with exactly ACCEPT_CHAT_OK'
call agent.chat "$chat_params" chat-first
jq -e '.text|type=="string" and length>0' "$last_response" >/dev/null || die "Native Agent chat returned no text"
jq -r '.text' "$last_response" >"$tmp/chat-text-1"
chmod 400 "$tmp/chat-text-1"
grep -F -q 'ACCEPT_CHAT_OK' "$tmp/chat-text-1" || die "Native Agent chat did not produce the acceptance marker"
call agent.chat "$chat_params" chat-replay
jq -r '.text' "$last_response" >"$tmp/chat-text-2"
chmod 400 "$tmp/chat-text-2"
cmp -s "$tmp/chat-text-1" "$tmp/chat-text-2" || die "identical turn replay returned different text"

history_conversation_id=$(uuid4)
history_create=$tmp/history-conversation-create.params.json
history_create_key=$(uuid4)
jq -n --arg id "$history_conversation_id" --arg idem "$history_create_key" \
  '{conversation_id:$id,title:"Split acceptance history",idempotency_key:$idem}' >"$history_create"
chmod 400 "$history_create"
call agent.chat.conversations.create "$history_create" history-conversation-create
jq -e --arg id "$history_conversation_id" \
  '.conversation.conversation_id == $id and .conversation.status == "active"' \
  "$last_response" >/dev/null || die "conversation.create did not return the active acceptance conversation"

history_marker=ACCEPT_HISTORY_$(date +%s)_$(od -An -N3 -tx1 /dev/urandom | tr -d '[:space:]')
history_idempotency_key=$(uuid4)
history_chat=$tmp/history-chat.params.json
write_chat_params "$history_chat" "$history_conversation_id" "$history_idempotency_key" \
  "Reply with exactly ACCEPT_HISTORY_OK and include this marker: $history_marker"
call agent.chat "$history_chat" history-chat
jq -e --arg marker "$history_marker" '.text|type=="string" and contains("ACCEPT_HISTORY_OK") and contains($marker)' "$last_response" >/dev/null || die "history conversation chat did not return its marker"

history_list=$tmp/history-conversation-list.params.json
jq -n '{page_size:100}' >"$history_list"
chmod 400 "$history_list"
call agent.chat.conversations.list "$history_list" history-conversation-list
jq -e --arg id "$history_conversation_id" 'any(.conversations[]?; .conversation_id == $id)' "$last_response" >/dev/null || die "conversation.list did not return the acceptance conversation"

history_get=$tmp/history-conversation-get.params.json
jq -n --arg id "$history_conversation_id" '{conversation_id:$id,message_limit:200}' >"$history_get"
chmod 400 "$history_get"
call agent.chat.conversations.get "$history_get" history-conversation-get
jq -e --arg id "$history_conversation_id" --arg marker "$history_marker" \
  '.conversation.conversation_id == $id and (.messages | type == "array") and
   any(.messages[]?; (.content // "") | contains($marker))' \
  "$last_response" >/dev/null || die "conversation.get did not return persisted history"

db_query agent-postgres dirextalk_agent dirextalk_agent \
  "SELECT count(*) FROM core_message_tool_results WHERE result_json->>'tool_name'='web_search' AND COALESCE((result_json->>'is_error')::boolean,false)=false;" \
  "$tmp/web-search-tool-before.count"
web_search_tool_before=$(tr -d '[:space:]' <"$tmp/web-search-tool-before.count")
case "$web_search_tool_before" in ''|*[!0-9]*) die "could not read the pre-chat Web Search tool count" ;; esac
web_search_idempotency_key=$(uuid4)
web_search_chat_params=$tmp/web-search-chat.params.json
write_chat_params "$web_search_chat_params" '' "$web_search_idempotency_key" \
  'You must use the web_search tool to search the live web for the official OpenAI homepage. Do not answer from memory. After the tool succeeds, reply with ACCEPT_WEB_SEARCH_OK followed by one https:// source URL from the tool result.'
call agent.chat "$web_search_chat_params" web-search-chat
jq -e '.text|type=="string" and contains("ACCEPT_WEB_SEARCH_OK")' "$last_response" >/dev/null || \
  die "Native Agent Web Search chat did not return the acceptance marker"
jq -r '.text' "$last_response" | grep -Eq 'https://[^[:space:]]+' || \
  die "Native Agent Web Search chat returned no source URL"
db_query agent-postgres dirextalk_agent dirextalk_agent \
  "SELECT count(*) FROM core_message_tool_results WHERE result_json->>'tool_name'='web_search' AND COALESCE((result_json->>'is_error')::boolean,false)=false;" \
  "$tmp/web-search-tool-after.count"
web_search_tool_after=$(tr -d '[:space:]' <"$tmp/web-search-tool-after.count")
case "$web_search_tool_after" in ''|*[!0-9]*) die "could not read the post-chat Web Search tool count" ;; esac
[ "$web_search_tool_after" -gt "$web_search_tool_before" ] 2>/dev/null || \
  die "Native Agent chat did not persist a successful Web Search tool result"

contacts_params=$tmp/contacts.params.json
printf '%s\n' '{"limit":100}' >"$contacts_params"
chmod 400 "$contacts_params"
call agent.contacts.list "$contacts_params" contacts-list
jq -e '.contacts|type=="array"' "$last_response" >/dev/null || die "Product contacts capability did not return contacts"
forged_params=$tmp/forged.params.json
printf '%s\n' '{"owner_id":"@forged:invalid","limit":1}' >"$forged_params"
chmod 400 "$forged_params"
call agent.contacts.list "$forged_params" forged-owner true
[ "$last_status" = 400 ] || [ "$last_status" = 401 ] || [ "$last_status" = 403 ] || [ "$last_status" = 409 ] || die "forged owner identity was not rejected"

agent_room_id=$(jq -r '.agent_room_id' "$session_file")
product_marker=SPLIT_PRODUCT_EXACT_ONCE_$(date +%s)_$(od -An -N3 -tx1 /dev/urandom | tr -d '[:space:]')
product_operation=$(uuid4)
message_params=$tmp/message.params.json
jq -n --arg room "$agent_room_id" --arg msg "$product_marker" --arg operation "$product_operation" '{room_id:$room,msg:$msg,operation_id:$operation}' >"$message_params"
chmod 400 "$message_params"
call agent.messages.send "$message_params" product-send
event_id=$(jq -r '.event_id // empty' "$last_response")
[ -n "$event_id" ] || die "Product message send returned no event_id"
first_event=$event_id
call agent.messages.send "$message_params" product-replay
second_event=$(jq -r '.event_id // empty' "$last_response")
[ "$first_event" = "$second_event" ] || die "Product message replay returned a different event_id"
list_params=$tmp/message-list.params.json
jq -n --arg room "$agent_room_id" '{room_id:$room,limit:100}' >"$list_params"
chmod 400 "$list_params"
i=0
while :; do
  call agent.messages.list "$list_params" product-list
  count=$( (grep -oF "$product_marker" "$last_response" || true) | wc -l | tr -d '[:space:]' )
  [ "$count" -eq 1 ] && break
  [ "$i" -lt "$timeout_seconds" ] || die "Product message was not visible exactly once"
  sleep 2
  i=$((i + 2))
done

knowledge_phrase=ACCEPT_KNOWLEDGE_$(date +%s)_$(od -An -N4 -tx1 /dev/urandom | tr -d '[:space:]')
knowledge_question='What is the unique answer recorded by the split acceptance knowledge document?'
knowledge_file=$tmp/knowledge.txt
knowledge_b64=$tmp/knowledge.b64
printf 'Question: %s\nAnswer: %s\nThis file verifies upload, automatic indexing, search and deletion.\n' \
  "$knowledge_question" "$knowledge_phrase" >"$knowledge_file"
chmod 400 "$knowledge_file"
size=$(wc -c <"$knowledge_file" | tr -d '[:space:]')
content_sha=$(sha256sum "$knowledge_file" | awk '{print $1}')
if base64 --wrap=0 "$knowledge_file" >"$knowledge_b64" 2>"$tmp/base64.err"; then
  :
else
  base64 "$knowledge_file" | tr -d '\n' >"$knowledge_b64"
fi
chmod 400 "$knowledge_b64"
upload_start=$tmp/upload-start.params.json
upload_start_key=$(uuid4)
jq -n --arg filename "split-acceptance.txt" --arg mime "text/plain" --arg sha "$content_sha" --argjson size "$size" --arg idem "$upload_start_key" '{filename:$filename,mime_type:$mime,size:$size,content_sha256:$sha,idempotency_key:$idem}' >"$upload_start"
chmod 400 "$upload_start"
call agent.knowledge.upload.start "$upload_start" knowledge-upload-start
upload_id=$(jq -r '.upload_id // empty' "$last_response")
source_id=$(jq -r '.source_id // empty' "$last_response")
[ -n "$upload_id" ] && [ -n "$source_id" ] || die "knowledge upload.start returned no upload/source id"
upload_chunk=$tmp/upload-chunk.params.json
upload_chunk_key=$(uuid4)
chunk_sha=$content_sha
jq -n --rawfile data "$knowledge_b64" --arg upload "$upload_id" --arg sha "$chunk_sha" --arg idem "$upload_chunk_key" \
  --argjson size "$size" '{upload_id:$upload,offset:0,data:($data|rtrimstr("\n")|rtrimstr("\r")),chunk_sha256:$sha,idempotency_key:$idem}' >"$upload_chunk"
chmod 400 "$upload_chunk"
call agent.knowledge.upload.chunk "$upload_chunk" knowledge-upload-chunk
db_query agent-postgres dirextalk_agent dirextalk_agent \
  "SELECT revision FROM core_knowledge_uploads WHERE upload_id=CAST('$upload_id' AS uuid);" \
  "$tmp/knowledge-upload-revision.out"
upload_revision=$(tr -d '[:space:]' <"$tmp/knowledge-upload-revision.out")
case "$upload_revision" in ''|0|*[!0-9]*) die "knowledge upload.chunk returned no positive revision" ;; esac
upload_finish=$tmp/upload-finish.params.json
upload_finish_key=$(uuid4)
jq -n --arg upload "$upload_id" --arg sha "$content_sha" --arg idem "$upload_finish_key" \
  --argjson revision "$upload_revision" \
  '{upload_id:$upload,expected_revision:$revision,content_sha256:$sha,idempotency_key:$idem}' >"$upload_finish"
chmod 400 "$upload_finish"
call agent.knowledge.upload.finish "$upload_finish" knowledge-upload-finish
finished_source=$(jq -r '.source_id // .source.source_id // .source.id // empty' "$last_response")
[ "$finished_source" = "$source_id" ] || die "knowledge upload.finish returned a different source id"

status_params=$tmp/status.params.json
printf '%s\n' '{}' >"$status_params"
chmod 400 "$status_params"
i=0
while :; do
  call agent.knowledge.status "$status_params" knowledge-status
  if jq -e '.supported == true and ((.embedding_indexed // 0) >= 1)' "$last_response" >/dev/null 2>&1; then break; fi
  [ "$i" -lt "$timeout_seconds" ] || die "knowledge embedding index did not become ready"
  sleep 2
  i=$((i + 2))
done
search_params=$tmp/knowledge-search.params.json
jq -n --arg query "$knowledge_question" '{query:$query,page_size:20}' >"$search_params"
chmod 400 "$search_params"
i=0
while :; do
  call agent.knowledge.search "$search_params" knowledge-search
  if jq -e --arg sid "$source_id" --arg phrase "$knowledge_phrase" 'any(.items[]?; (.source_id == $sid) and ((.snippet // "") | contains($phrase)))' "$last_response" >/dev/null 2>&1; then break; fi
  [ "$i" -lt "$timeout_seconds" ] || die "knowledge search did not return the uploaded source"
  sleep 2
  i=$((i + 2))
done

if run_compose restart agent >"$tmp/restart.out" 2>"$tmp/restart.err"; then
  :
else
  die "Agent restart failed"
fi
i=0
while ! agent_health; do
  [ "$i" -lt "$timeout_seconds" ] || die "Agent did not become healthy after restart"
  sleep 2
  i=$((i + 2))
done
call agent.backends.get "$params" backends-after-restart
# Continue the pre-restart conversation with a fresh turn. The second prompt
# deliberately omits history_marker, so the only production path that can
# return it is Agent-owned durable conversation context loaded after restart.
history_context_idempotency_key=$(uuid4)
history_context_chat=$tmp/history-context-chat.params.json
write_chat_params "$history_context_chat" "$history_conversation_id" "$history_context_idempotency_key" \
  'Recall the unique marker from the first turn of this conversation. Reply with ACCEPT_CONTEXT_OK followed by that exact marker, and nothing else.'
call agent.chat "$history_context_chat" history-context-after-restart
jq -e --arg marker "$history_marker" \
  '.text | type == "string" and contains("ACCEPT_CONTEXT_OK") and contains($marker)' \
  "$last_response" >/dev/null || die "Native Agent did not preserve multi-turn conversation context after restart"

call agent.knowledge.search "$search_params" knowledge-search-after-restart
jq -e --arg sid "$source_id" 'any(.items[]?; .source_id == $sid)' "$last_response" >/dev/null || die "knowledge source was not searchable after Agent restart"
call agent.web_search.config.get "$params" web-search-config-after-restart
jq -e --argjson revision "$web_search_updated_revision" \
  '.enabled == true and .provider == "tavily" and .api_key_configured == true and .revision == $revision and ((has("api_key") | not))' \
  "$last_response" >/dev/null || die "web search config was not preserved after Agent restart"
call agent.web_search.test "$params" web-search-test-after-restart
jq -e --argjson revision "$web_search_updated_revision" \
  '.ok == true and .provider == "tavily" and (.result_count // -1) >= 0 and .enabled == true and .api_key_configured == true and .revision == $revision' \
  "$last_response" >/dev/null || die "web search test did not succeed after Agent restart"

sources_params=$tmp/sources.params.json
jq -n '{page_size:100}' >"$sources_params"
chmod 400 "$sources_params"
call agent.knowledge.sources.list "$sources_params" sources-before-delete
source_revision=$(jq -r --arg id "$source_id" '.sources[]? | select((.source_id // .id)==$id) | (.revision // 1)' "$last_response" | head -n 1)
[ -n "$source_revision" ] || die "uploaded source did not appear in source list"
source_delete=$tmp/source-delete.params.json
source_delete_key=$(uuid4)
jq -n --arg id "$source_id" --arg idem "$source_delete_key" --argjson revision "$source_revision" \
  '{source_id:$id,expected_revision:$revision,idempotency_key:$idem}' >"$source_delete"
chmod 400 "$source_delete"
call agent.knowledge.sources.delete "$source_delete" source-delete
call agent.knowledge.sources.list "$sources_params" sources-after-delete
if jq -e --arg id "$source_id" 'any(.sources[]?; ((.source_id // .id)==$id) and ((.status // "") != "deleted"))' "$last_response" >/dev/null 2>&1; then
  die "deleted knowledge source remains active"
fi
call agent.knowledge.search "$search_params" knowledge-search-after-delete true
if [ "$last_status" = 200 ] && jq -e --arg id "$source_id" 'any(.items[]?; .source_id == $id)' "$last_response" >/dev/null 2>&1; then
  die "deleted knowledge source remains searchable"
fi

if [ "$account_delete_enabled" = true ]; then
  account_delete=$tmp/account-delete.params.json
  account_delete_key=$(uuid4)
  jq -n --arg idem "$account_delete_key" '{confirm:"delete_account",idempotency_key:$idem}' >"$account_delete"
  chmod 400 "$account_delete"
  pre_delete_agent_container=$agent_container
  pre_delete_restart_count=$(docker inspect -f '{{.RestartCount}}' "$pre_delete_agent_container" 2>"$tmp/agent-restart-before.err" || true)
  case "$pre_delete_restart_count" in ''|*[!0-9]*) die "could not read Agent restart count before account deletion" ;; esac
  call portal.account.delete "$account_delete" account-delete
  jq -e '(.status // "") as $s | (.account_deleted // false) == true or ($s == "deprovisioned" or $s == "deleted" or $s == "completed")' "$last_response" >/dev/null || die "account deletion did not return a completed/deprovisioned status"

  # Account deprovision seals the Agent but does not stop its process. Observe two
  # complete image healthcheck intervals so a crash-loop, a late background write,
  # or post-purge Knowledge recovery cannot pass on a transient sample.
  for health_interval in 1 2; do
    sleep 16
    post_delete_agent_container=$(run_compose ps -q agent 2>"$tmp/agent-ps-after-delete-$health_interval.err" || true)
    [ "$post_delete_agent_container" = "$pre_delete_agent_container" ] || die "Agent container identity changed after account deletion"
    post_delete_health=$(docker inspect -f '{{.State.Health.Status}}' "$post_delete_agent_container" 2>"$tmp/agent-health-after-delete-$health_interval.err" || true)
    [ "$post_delete_health" = healthy ] || die "Agent did not remain healthy after account deletion"
    post_delete_restart_count=$(docker inspect -f '{{.RestartCount}}' "$post_delete_agent_container" 2>"$tmp/agent-restart-after-delete-$health_interval.err" || true)
    [ "$post_delete_restart_count" = "$pre_delete_restart_count" ] || die "Agent restarted after account deletion"
  done

  # The deleted owner session must not retain an ordinary Agent capability path.
  # Rejection may occur at message-server authorization or at the sealed Agent
  # lifecycle boundary; either is valid, but a successful response is not.
  call agent.backends.get "$params" backends-after-account-delete true
  case "$last_status" in
    400|401|403|409|412|428) ;;
    *) die "ordinary Agent capability remained accessible after account deletion" ;;
  esac

  db_query agent-postgres dirextalk_agent dirextalk_agent 'SELECT count(*) FROM core_model_profiles;' "$tmp/core-model-profiles.count"
  db_query agent-postgres dirextalk_agent dirextalk_agent 'SELECT count(*) FROM core_knowledge_sources;' "$tmp/core-knowledge-sources.count"
  db_query agent-postgres dirextalk_agent dirextalk_agent 'SELECT count(*) FROM core_conversation_turns;' "$tmp/core-conversation-turns.count"
  db_query agent-postgres dirextalk_agent dirextalk_agent 'SELECT (SELECT count(*) FROM core_web_search_configs) || '\''|'\'' || (SELECT count(*) FROM core_web_search_replays);' "$tmp/core-web-search.count"
  db_query agent-postgres dirextalk_agent dirextalk_agent 'SELECT count(*) FROM agent_account_deprovisions WHERE state = '\''completed'\'';' "$tmp/account-deprovisions.count"
  db_query agent-postgres dirextalk_agent dirextalk_agent 'SELECT count(*) || '\''|'\'' || count(*) FILTER (WHERE state = '\''completed'\'') || '\''|'\'' || count(*) FILTER (WHERE state <> '\''completed'\'') FROM agent_account_deprovisions;' "$tmp/account-deprovisions-shape.count"
  db_query agent-postgres dirextalk_agent dirextalk_agent 'SELECT count(*) FROM agent_capability_operations WHERE capability_id <> '\''agent.account.v1'\'' OR operation_name <> '\''deprovision_account'\'';' "$tmp/non-deprovision-operations.count"
  [ "$(tr -d '[:space:]' <"$tmp/core-model-profiles.count")" = 0 ] || die "Agent model profile rows survived account deletion"
  [ "$(tr -d '[:space:]' <"$tmp/core-knowledge-sources.count")" = 0 ] || die "Agent knowledge source rows survived account deletion"
  [ "$(tr -d '[:space:]' <"$tmp/core-conversation-turns.count")" = 0 ] || die "Agent conversation rows survived account deletion"
  [ "$(tr -d '[:space:]' <"$tmp/core-web-search.count")" = '0|0' ] || die "Agent web search config or replay rows survived account deletion"
  [ "$(tr -d '[:space:]' <"$tmp/account-deprovisions.count")" -ge 1 ] 2>/dev/null || die "Agent deprovision ledger has no completed row"
  [ "$(tr -d '[:space:]' <"$tmp/account-deprovisions-shape.count")" = '1|1|0' ] || die "Agent deprovision ledger is not one exact completed receipt"
  [ "$(tr -d '[:space:]' <"$tmp/non-deprovision-operations.count")" = 0 ] || die "ordinary capability operation rows survived account deletion"

  # Audit every current/future Agent-owned business table. The protected
  # instance/schema metadata and minimal deprovision/capability ledgers are the
  # only exclusions, matching CoreDeprovisionStore's ownership boundary.
  agent_business_audit='DO $audit$ DECLARE item record; row_count bigint; BEGIN FOR item IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind='\''r'\'' AND (left(c.relname,5)='\''core_'\'' OR left(c.relname,6)='\''agent_'\'') AND c.relname NOT IN ('\''agent_instance_metadata'\'','\''agent_schema_migrations'\'','\''agent_capability_operations'\'','\''agent_capability_operation_events'\'','\''agent_account_deprovisions'\'') ORDER BY c.relname LOOP EXECUTE format('\''SELECT count(*) FROM %I'\'',item.relname) INTO row_count; IF row_count <> 0 THEN RAISE EXCEPTION '\''Agent business table % retained % rows after deprovision'\'',item.relname,row_count; END IF; END LOOP; END $audit$;'
  db_query agent-postgres dirextalk_agent dirextalk_agent "$agent_business_audit" "$tmp/agent-business-audit.out"

fi

message_dump=$tmp/message-db.dump
agent_dump=$tmp/agent-db.dump
new_file "$message_dump"
new_file "$agent_dump"
new_file "$tmp/message-dump.err"
new_file "$tmp/agent-dump.err"
if run_compose exec -T postgres sh -ec 'password=$(cat /run/dirextalk-postgres-secrets/message_postgres_password); PGPASSWORD="$password" pg_dump -h 127.0.0.1 -U dirextalk_message_server -d dirextalk_message_server' >"$message_dump" 2>"$tmp/message-dump.err"; then
  :
else
  die "message PostgreSQL dump failed"
fi
if run_compose exec -T postgres sh -ec 'password=$(cat /run/dirextalk-postgres-secrets/agent_postgres_password); PGPASSWORD="$password" pg_dump -h 127.0.0.1 -U dirextalk_agent -d dirextalk_agent' >"$agent_dump" 2>"$tmp/agent-dump.err"; then
  :
else
  die "Agent PostgreSQL dump failed"
fi
if grep -Eiq '^CREATE TABLE (public\.)?(core_|agent_)' "$message_dump"; then
  die "message-server database contains Agent/Core tables"
fi
grep -Eiq '^CREATE TABLE (public\.)?agent_' "$agent_dump" || die "Agent database dump has no Agent-owned tables"

run_compose logs --no-color --timestamps message-server >"$tmp/message-server.log" 2>"$tmp/message-server-log.err" || true
run_compose logs --no-color --timestamps agent >"$tmp/agent.log" 2>"$tmp/agent-log.err" || true
find "$out" "$tmp" -type f -print0 | while IFS= read -r -d '' file; do
  case "$file" in
    # model-sync is the protected transport envelope derived from model_params;
    # it is an input containing the same API keys, not an observable output.
    # These are protected request inputs containing credentials, not observable
    # responses or stack output. Every other file is scanned for every key.
    "$openrouter_key"|"$embedding_key"|"$tavily_key"|"$portal_password"|"$auth_file"|"$session_file"|"$model_params"|"$model_catalog_params"|"$web_search_update"|"$tmp/request-web-search-config-update.json") continue ;;
    "$tmp/request-model-sync.json"|"$tmp/request-model-catalog.json") continue ;;
  esac
  assert_secret_absent "$file"
  if grep -Eiq 'sk-or-v1-[A-Za-z0-9_-]{8,}|Bearer[[:space:]]+[A-Za-z0-9._-]{16,}' "$file"; then
    die "credential-like material appeared in stack output or log"
  fi
done

if [ "$cleanup_after" = true ]; then
  "$deploy_dir/scripts/cleanup-local.sh" --purge "$out" >"$tmp/cleanup.out" 2>"$tmp/cleanup.err" || die "post-acceptance cleanup failed"
fi

if [ "$account_delete_enabled" = true ]; then
  account_delete_summary='account deprovision'
else
  account_delete_summary='account deletion skipped; model/Tavily/conversation state retained'
fi
echo "split local acceptance passed: bootstrap, HTTP+HTTPS health, live model catalog plus stored-profile catalog, model sync/chat replay plus durable multi-turn restart context, Tavily credential/config/test plus real Web Search chat/restart, Product capability exact-once, Knowledge/memory indexing+restart+delete, $account_delete_summary, DB/network isolation and secret canary"
