#!/usr/bin/env bash
set -euo pipefail

# Read-only deployment gate. It provisions disposable files in /tmp, renders
# both Compose variants, and checks the split boundary. It never starts or
# mutates a Docker resource.

script_dir=$(cd "$(dirname "$0")" && pwd -P)
stack_dir=$(cd "$script_dir/.." && pwd -P)
message_root=$(cd "$script_dir/../../.." && pwd -P)
agent_root=$(cd "$message_root/../dirextalk-agent" && pwd -P)
tmp_root=$(printenv TMPDIR 2>/dev/null || true)
[ -n "$tmp_root" ] || tmp_root=/tmp
run_dir=$(mktemp -d "$tmp_root/dirextalk-split-verify.XXXXXX")
cleanup() {
  rm -rf "$run_dir"
}
trap cleanup EXIT

verify_http_bind=${DIREXTALK_MESSAGE_HTTP_BIND:-18008}
verify_https_bind=${DIREXTALK_MESSAGE_HTTPS_BIND:-18448}
DIREXTALK_MESSAGE_HTTP_BIND="$verify_http_bind" \
DIREXTALK_MESSAGE_HTTPS_BIND="$verify_https_bind" \
  "$script_dir/provision-local.sh" "$run_dir/provision" >/dev/null 2>"$run_dir/provision.stderr"
env_file=$run_dir/provision/.env
rendered=$run_dir/compose.json
production_rendered=$run_dir/production-compose.json
profile_run_dir=$run_dir/profile
profile_rendered=$run_dir/profile-compose.json

cd "$stack_dir"
docker compose --env-file "$env_file" -f compose.yaml -f compose.local.yaml config --quiet
docker compose --env-file "$env_file" -f compose.yaml -f compose.local.yaml config --format json >"$rendered"
docker compose --env-file "$env_file" --profile extensions --profile core-runner \
  -f compose.yaml config --quiet
docker compose --env-file "$env_file" --profile extensions --profile core-runner \
  -f compose.yaml config --format json >"$production_rendered"

# Render the opt-in profiles separately. Compose omits profile-gated services
# from the default model, so both the disabled baseline and the full
# acceptance shape need an explicit read-only render check. Enabling a runner
# is a host gate: provision-local refuses the global cgroup root and requires a
# real per-stack delegated cgroup-v2 subtree before it writes an enabled config.
mkdir -p "$profile_run_dir"
profile_env=$env_file
if DIREXTALK_CORE_EXTENSION_ENABLED=true \
  DIREXTALK_CORE_WORKLOAD_ENABLED=true \
  DIREXTALK_EXTENSION_CGROUP_ROOT=/sys/fs/cgroup \
  DIREXTALK_CORE_RUNNER_CGROUP_ROOT=/sys/fs/cgroup \
  "$script_dir/provision-local.sh" "$profile_run_dir/rejected" >/dev/null 2>"$profile_run_dir/provision.stderr"; then
  echo "global cgroup root unexpectedly accepted for runner profiles" >&2
  exit 1
else
  status=$?
  [ "$status" -eq 1 ] || {
    echo "unexpected cgroup rejection status: $status" >&2
    exit 1
  }
fi
docker compose --env-file "$profile_env" --profile extensions --profile core-runner \
  -f compose.yaml -f compose.local.yaml config --quiet
docker compose --env-file "$profile_env" --profile extensions --profile core-runner \
  -f compose.yaml -f compose.local.yaml config --format json >"$profile_rendered"

agent_instance=$(sed -n 's/^DIREXTALK_AGENT_INSTANCE_ID=//p' "$env_file")
message_instance=$(sed -n 's/^DIREXTALK_MESSAGE_SERVER_INSTANCE_ID=//p' "$env_file")
account_generation=$(sed -n 's/^DIREXTALK_ACCOUNT_GENERATION=//p' "$env_file")
stack_name=$(sed -n 's/^DIREXTALK_SPLIT_STACK_NAME=//p' "$env_file")
http_bind=$(sed -n 's/^DIREXTALK_MESSAGE_HTTP_BIND=//p' "$env_file")
https_bind=$(sed -n 's/^DIREXTALK_MESSAGE_HTTPS_BIND=//p' "$env_file")
client_base_url=$(sed -n 's/^DIREXTALK_MESSAGE_CLIENT_BASE_URL=//p' "$env_file")
master_key_env=$(sed -n 's/^DIREXTALK_CORE_SECRET_MASTER_KEY_FILE=//p' "$env_file")
[ -n "$agent_instance" ] && [ -n "$message_instance" ] && [ "$agent_instance" != "$message_instance" ]
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$'
printf '%s\n' "$http_bind" | grep -Eq '^[0-9]+$'
printf '%s\n' "$https_bind" | grep -Eq '^[0-9]+$'
[ "$http_bind" != "$https_bind" ]
[ "$client_base_url" = "http://localhost:$http_bind" ]
[ "$master_key_env" = "$run_dir/provision/core-secret-master-key" ]
[ "$(stat -c '%a' "$run_dir/provision/.manifest")" = 400 ]
printf 'stack_name=%s\n' "$stack_name" | cmp -s - <(grep '^stack_name=' "$run_dir/provision/.manifest")
printf 'message_http_bind=%s\n' "$http_bind" | cmp -s - <(grep '^message_http_bind=' "$run_dir/provision/.manifest")
printf 'message_https_bind=%s\n' "$https_bind" | cmp -s - <(grep '^message_https_bind=' "$run_dir/provision/.manifest")
printf 'message_client_base_url=%s\n' "$client_base_url" | cmp -s - <(grep '^message_client_base_url=' "$run_dir/provision/.manifest")
master_key="$run_dir/provision/core-secret-master-key"
[ "$(stat -c '%a' "$master_key")" = 400 ]
[ "$(stat -c '%s' "$master_key")" = 32 ]
[ "$(stat -c '%u' "$master_key")" = "$(id -u)" ]
printf 'core_secret_master_key_path=%s\n' "$master_key" | cmp -s - <(grep '^core_secret_master_key_path=' "$run_dir/provision/.manifest")
printf 'product_capability_instance_id: %s\n' "$agent_instance" | cmp -s - <(grep '^product_capability_instance_id:' "$run_dir/provision/agent-config.yaml")
printf 'capability_peer_instance_id: %s\n' "$message_instance" | cmp -s - <(grep '^capability_peer_instance_id:' "$run_dir/provision/agent-config.yaml")
printf 'capability_account_generation: %s\n' "$account_generation" | cmp -s - <(grep '^capability_account_generation:' "$run_dir/provision/agent-config.yaml")
printf 'product_capability_account_generation: %s\n' "$account_generation" | cmp -s - <(grep '^product_capability_account_generation:' "$run_dir/provision/agent-config.yaml")
grep -Fqx 'core_secret_master_key_file: /run/secrets/core_secret_master_key' "$run_dir/provision/agent-config.yaml"
grep -Fqx 'core_secret_master_key_version: 1' "$run_dir/provision/agent-config.yaml"
printf '%s\n' "$account_generation" | grep -Eq '^[1-9][0-9]*$'
jq -e --arg generation "$account_generation" '.services["message-server"].environment.P2P_ACCOUNT_GENERATION == $generation' "$profile_rendered" >/dev/null

shellcheck \
  "$script_dir/provision-local.sh" \
  "$script_dir/verify-topology.sh" \
  "$script_dir/cleanup-local.sh" \
  "$script_dir/accept-local.sh" \
  "$script_dir/accept-local.test.sh" \
  "$script_dir/bootstrap-local-account.sh" \
  "$script_dir/export-portal-bootstrap.sh" \
  "$script_dir/build-local.sh" \
  "$script_dir/start-local.sh" \
  "$script_dir/start-local.test.sh" \
  "$script_dir/verify-production-images.sh" \
  "$script_dir/verify-production-images.test.sh" \
  "$script_dir/verify-production-tls.sh" \
  "$script_dir/verify-production-tls.test.sh" \
  "$script_dir/verify-build-contexts.sh" \
  "$script_dir/initialize-capability-ca.sh" \
  "$script_dir/initialize-capability-ca.test.sh" \
  "$script_dir/initialize-message-server.sh" \
  "$script_dir/materialize-agent-secrets.sh" \
  "$script_dir/message-server-entrypoint.sh" \
  "$script_dir/message-server-entrypoint.test.sh" \
  "$stack_dir/aws/validate-policy.sh" \
  "$stack_dir/aws/validate-policy.test.sh"
"$script_dir/message-server-entrypoint.test.sh" >/dev/null
"$script_dir/initialize-capability-ca.test.sh" >/dev/null
"$script_dir/accept-local.test.sh" >/dev/null
"$script_dir/start-local.test.sh" >/dev/null
"$script_dir/verify-production-images.test.sh" >/dev/null
"$script_dir/verify-production-tls.test.sh" >/dev/null
"$stack_dir/aws/validate-policy.test.sh" >/dev/null
"$script_dir/verify-build-contexts.sh" "$agent_root" "$message_root" >/dev/null

jq -e --arg http "$http_bind" --arg https "$https_bind" '
  ([.services["message-server"].ports[] | .published] | sort) == [$http, $https] and
  ([.services | to_entries[] | select(.key != "message-server") | .value.ports // [] | length] | add // 0) == 0
' "$rendered" >/dev/null || {
  published=$(jq -c '[.services | to_entries[] | .value.ports // [] | .[] | .published] | sort' "$rendered")
  echo "unexpected host-published ports (message-server only, $http_bind/$https_bind expected): $published" >&2
  exit 1
}

jq -e '
  .services.agent.ports == null and
  .services.agent["expose"] == ["9443","50052","8444"] and
  .services["message-server"].expose == ["50053"] and
  .services["agent-postgres"].ports == null and
  .services["message-postgres"].ports == null and
  .services.qdrant.ports == null
' "$rendered" >/dev/null

jq -e '
  ([.services | to_entries[] | .value.image] | all(test("@sha256:[0-9a-f]{64}$"))) and
  ([.services | to_entries[] | .value.build // null] | all(. == null))
' "$production_rendered" >/dev/null

jq -e '
  (.services["message-server-init"].entrypoint == ["/usr/local/bin/initialize-message-server"]) and
  ([.services["message-server-init"].configs[] | select(.target == "/usr/local/bin/initialize-message-server" and .mode == "0555")] | length) == 1 and
  ([.services["message-server-init"].configs[] | select(.target == "/usr/local/bin/initialize-capability-ca" and .mode == "0555")] | length) == 1 and
  (.services["agent-secret-init"].entrypoint == ["/usr/local/bin/materialize-agent-secrets"]) and
  ([.services["agent-secret-init"].configs[] | select(.target == "/usr/local/bin/materialize-agent-secrets" and .mode == "0555")] | length) == 1 and
  ([.services["message-server-init"].secrets[] | select(.target == "/run/secrets/message_registration_shared_secret")] | length) == 1 and
  ([.services["message-server-init"].secrets[] | select(.target == "/run/secrets/message_database_url")] | length) == 0 and
  (.services["message-server-init"].environment.MESSAGE_SERVER_TLS_MODE == "local") and
  (.services["message-server-init"].environment.MESSAGE_LOCAL_BOOTSTRAP_ENABLED == "false") and
  ([.services["message-server-init"].volumes[] | select(.target == "/bootstrap/external/server.crt")] | length) == 1 and
  ([.services["message-server-init"].volumes[] | select(.target == "/bootstrap/external/server.key")] | length) == 1 and
  ([.services["message-server"].secrets[] | select(.target == "/run/secrets/message_portal_password")] | length) == 1 and
  (.services["message-server"].environment.P2P_PORTAL_PASSWORD_FILE == "/run/secrets/message_portal_password") and
  (.services["message-server"].cap_add | index("DAC_READ_SEARCH")) != null and
  (.services["message-server"].entrypoint == ["/usr/local/bin/message-server-entrypoint"]) and
  (.services["message-server"].command == [
    "--http-bind-address", ":8008",
    "--https-bind-address", ":8448",
    "--tls-cert", "/etc/dirextalk-message-server/server.crt",
    "--tls-key", "/etc/dirextalk-message-server/server.key"
  ]) and
  (.services["message-server"].healthcheck.test == [
    "CMD-SHELL",
    "wget -q -O - http://127.0.0.1:8008/_p2p/health >/dev/null && wget --no-check-certificate -q -O - https://127.0.0.1:8448/_p2p/health >/dev/null"
  ]) and
  ([.services["message-server"].configs[] | select(.target == "/usr/local/bin/message-server-entrypoint")] | length) == 1 and
  ([.services["message-server"].secrets[] | select(.target == "/run/secrets/message_database_url")] | length) == 1 and
  (.services["message-server"].tmpfs | any(test("^/tmp:rw,noexec")))
  and (.services.qdrant.tmpfs | any(test("^/qdrant/snapshots:rw"))) and
  ([.services["agent-secret-init"].secrets[] | select(.source == "core_secret_master_key" and .target == "core_secret_master_key")] | length) == 1 and
  ([.services | to_entries[] | select(.key != "agent-secret-init") | .value.secrets // [] | .[] | select(.source == "core_secret_master_key" or (.target // "" | test("core_secret_master_key")))] | length) == 0
' "$production_rendered" >/dev/null

jq -e --arg agent_root "$agent_root" --arg message_root "$message_root" '
  .services["message-server"].image == "dirextalk-message-server:split-local" and
  .services.agent.image == "dirextalk-agent:split-local" and
  .services.agent.build.context == $agent_root and
  .services.agent.build.dockerfile == "deploy/container/agent.Containerfile" and
  .services["agent-migrate"].build.context == $agent_root and
  .services["agent-migrate"].build.dockerfile == "deploy/container/agent.Containerfile" and
  .services["message-server"].build.context == $message_root and
  .services["message-server"].build.dockerfile == "deploy/split-agent/container/message.local.Containerfile" and
  .services["message-server-init"].build.context == $message_root and
  .services["message-server-init"].build.dockerfile == "deploy/split-agent/container/message.local.Containerfile" and
  ([.services | to_entries[] | .value.build.additional_contexts // null] | all(. == null)) and
  .services["message-server"].depends_on.agent.condition == "service_healthy" and
  .services["message-server"].depends_on.agent.required == true and
  .services["extension-runner"] == null and
  .services["core-runner"] == null
' "$rendered" >/dev/null

jq -e '
  ([.services | keys[] | select(. == "extension-runner" or . == "core-runner" or . == "extension-socket-init" or . == "core-runner-socket-init")] | length) == 0
' "$rendered" >/dev/null

jq -e --arg agent_root "$agent_root" '
  .services["extension-runner"].profiles == ["extensions"] and
  .services["core-runner"].profiles == ["core-runner"] and
  .services["extension-socket-init"].profiles == ["extensions"] and
  .services["core-runner-socket-init"].profiles == ["core-runner"] and
  .services["extension-runner"].network_mode == "none" and
  .services["core-runner"].network_mode == "none" and
  .services["extension-runner"].networks == null and
  .services["core-runner"].networks == null and
  .services["extension-runner"].secrets == null and
  .services["core-runner"].secrets == null and
  .services["extension-runner"].user == "65531:65531" and
  .services["core-runner"].user == "65530:65530" and
  (.services["extension-runner"].group_add | index("65532")) != null and
  (.services["core-runner"].group_add | index("65532")) != null and
  .services["extension-runner"].build.context == $agent_root and
  .services["extension-runner"].build.dockerfile == "deploy/container/extension-runner.Containerfile" and
  .services["core-runner"].build.context == $agent_root and
  .services["core-runner"].build.dockerfile == "deploy/container/core-runner.Containerfile" and
  .services["extension-runner"].build.additional_contexts == null and
  .services["core-runner"].build.additional_contexts == null and
  ([.services["extension-runner"].volumes[], .services["core-runner"].volumes[]] | all(.type == "volume" or (.type == "bind" and .target == "/cgroup"))) and
  ([.services["extension-runner"].volumes[], .services["core-runner"].volumes[]] | map(.source // "") | any(test("docker.sock|/var/run/docker"))) == false and
  (.services["extension-runner"].healthcheck.test[0] == "CMD") and
  (.services["core-runner"].healthcheck.test[0] == "CMD") and
  (.services["extension-runner"].depends_on["extension-socket-init"].condition == "service_completed_successfully") and
  (.services["core-runner"].depends_on["core-runner-socket-init"].condition == "service_completed_successfully")
' "$profile_rendered" >/dev/null

jq -e --arg http "$http_bind" --arg https "$https_bind" '
  ([.services["message-server"].ports[] | .published] | sort) == [$http, $https] and
  ([.services | to_entries[] | select(.key != "message-server") | .value.ports // [] | length] | add // 0) == 0
' "$profile_rendered" >/dev/null

jq -e '
  (.services.agent.volumes | map(.source // "") | any(test("agent_extension_socket|core_runner_socket"))) and
  (.services.agent.volumes | map(.source // "") | any(test("agent_extension_staging|agent_extension_workspaces"))) and
  (.services.agent.depends_on["extension-runner"].condition == "service_healthy") and
  (.services.agent.depends_on["extension-runner"].required == false) and
  (.services.agent.depends_on["core-runner"].condition == "service_healthy") and
  (.services.agent.depends_on["core-runner"].required == false)
' "$profile_rendered" >/dev/null

jq -e '
  .networks.agent_private.internal == true and
  .networks.agent_database.internal == true and
  .networks.agent_caller.internal == true and
  .networks.message_private.internal == true and
  .networks.message_public.internal != true and
  .networks.message_database.internal == true and
  .networks.agent_egress.internal != true
' "$rendered" >/dev/null

jq -e '
  (.services["agent-postgres"].networks | keys) == ["agent_database"] and
  (.services["message-postgres"].networks | keys) == ["message_database"] and
  ((.services.agent.networks | keys) | sort) == ["agent_caller","agent_database","agent_egress","agent_private"] and
  ((.services["message-server"].networks | keys) | sort) == ["agent_caller","message_database","message_private","message_public"] and
  ([.services | to_entries[] | select(.key != "message-server") | .value.networks // {} | keys[] | select(. == "message_public")] | length) == 0
' "$rendered" >/dev/null

jq -e '
  ([.services["message-server"].volumes[] | select(.target == "/run/capability-private" and .read_only == true)] | length) == 1 and
  ([.services["message-server"].volumes[] | select(.target == "/run/capability" and .read_only == true)] | length) == 1 and
  ([.services["agent-secret-init"].volumes[] | select(.target == "/bootstrap/capability" and .read_only == true)] | length) == 1 and
  (.services["agent-secret-init"].depends_on["message-server-init"].condition == "service_completed_successfully") and
  (.services["message-server"].environment.P2P_CAPABILITY_GRANT_PRIVATE_KEY_FILE == "/run/capability-private/grant-private.key") and
  ([.services["agent-secret-init"].secrets[] | select(.target == "openrouter_api_key" or .target == "embedding_api_key")] | length) == 0 and
  ([.services.agent.secrets // [] | .[] | select(.target | test("openrouter|embedding"))] | length) == 0
' "$rendered" >/dev/null

for secret in \
  "$run_dir/provision/agent-postgres-password" \
  "$run_dir/provision/message-postgres-password" \
  "$run_dir/provision/agent-database-url" \
  "$run_dir/provision/message-database-url" \
  "$run_dir/provision/message-registration-shared-secret" \
  "$run_dir/provision/message-portal-password" \
  "$run_dir/provision/core-secret-master-key" \
  "$run_dir/provision/message-tls-external-cert.pem" \
  "$run_dir/provision/message-tls-external-key.pem" \
  "$run_dir/provision/openrouter-api-key" \
  "$run_dir/provision/embedding-api-key"; do
  [ "$(stat -c '%a' "$secret")" = 400 ] || {
    echo "secret mode is not 0400: $secret" >&2
    exit 1
  }
done
[ "$(stat -c '%a' "$env_file")" = 400 ]
[ "$(stat -c '%a' "$run_dir/provision/agent-config.yaml")" = 400 ]

for secret in \
  "$run_dir/provision/agent-postgres-password" \
  "$run_dir/provision/message-postgres-password" \
  "$run_dir/provision/agent-database-url" \
  "$run_dir/provision/message-database-url" \
  "$run_dir/provision/message-registration-shared-secret" \
  "$run_dir/provision/message-portal-password"; do
  value=$(cat "$secret")
  if grep -Fq -- "$value" "$env_file"; then
    echo "secret value rendered into .env: $secret" >&2
    exit 1
  fi
  if grep -Fq -- "$value" "$run_dir/provision/agent-config.yaml"; then
    echo "secret value rendered into Agent config: $secret" >&2
    exit 1
  fi
  if grep -Fq -- "$value" "$rendered"; then
    echo "secret value rendered into Compose config: $secret" >&2
    exit 1
  fi
done

printf 'split-stack topology, fresh secret permissions, and Compose rendering verified\n'
