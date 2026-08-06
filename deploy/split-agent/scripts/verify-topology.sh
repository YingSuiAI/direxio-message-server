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
DIREXTALK_SPLIT_FIXTURE_MODE=true \
DIREXTALK_SPLIT_TEST_MODE=true \
  "$script_dir/provision-local.sh" "$run_dir/provision" >/dev/null 2>"$run_dir/provision.stderr"
env_file=$run_dir/provision/.env
rendered=$run_dir/compose.json
production_rendered=$run_dir/production-compose.json

cd "$stack_dir"
docker compose --env-file "$env_file" -f compose.yaml -f compose.direct-tls.yaml -f compose.local.yaml config --quiet
docker compose --env-file "$env_file" -f compose.yaml -f compose.direct-tls.yaml -f compose.local.yaml config --format json >"$rendered"
DIREXTALK_SPLIT_COMPOSE_MODE=production \
DIREXTALK_MESSAGE_TLS_MODE=edge-terminated \
DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
DIREXTALK_MESSAGE_CLIENT_BASE_URL=https://message.example.com \
  docker compose --env-file "$env_file" -f compose.yaml config --quiet
DIREXTALK_SPLIT_COMPOSE_MODE=production \
DIREXTALK_MESSAGE_TLS_MODE=edge-terminated \
DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
DIREXTALK_MESSAGE_CLIENT_BASE_URL=https://message.example.com \
  docker compose --env-file "$env_file" -f compose.yaml config --format json >"$production_rendered"

agent_instance=$(sed -n 's/^DIREXTALK_AGENT_INSTANCE_ID=//p' "$env_file")
message_instance=$(sed -n 's/^DIREXTALK_MESSAGE_SERVER_INSTANCE_ID=//p' "$env_file")
account_generation=$(sed -n 's/^DIREXTALK_ACCOUNT_GENERATION=//p' "$env_file")
stack_name=$(sed -n 's/^DIREXTALK_SPLIT_STACK_NAME=//p' "$env_file")
http_bind=$(sed -n 's/^DIREXTALK_MESSAGE_HTTP_BIND=//p' "$env_file")
https_bind=$(sed -n 's/^DIREXTALK_MESSAGE_HTTPS_BIND=//p' "$env_file")
client_base_url=$(sed -n 's/^DIREXTALK_MESSAGE_CLIENT_BASE_URL=//p' "$env_file")
tls_mode=$(sed -n 's/^DIREXTALK_MESSAGE_TLS_MODE=//p' "$env_file")
server_name=$(sed -n 's/^DIREXTALK_MESSAGE_SERVER_NAME=//p' "$env_file")
tls_cert_file=$(sed -n 's/^DIREXTALK_MESSAGE_TLS_CERT_FILE=//p' "$env_file")
tls_key_file=$(sed -n 's/^DIREXTALK_MESSAGE_TLS_KEY_FILE=//p' "$env_file")
master_key_env=$(sed -n 's/^DIREXTALK_CORE_SECRET_MASTER_KEY_FILE=//p' "$env_file")
[ -n "$agent_instance" ] && [ -n "$message_instance" ] && [ "$agent_instance" != "$message_instance" ]
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$'
printf '%s\n' "$http_bind" | grep -Eq '^[0-9]+$'
printf '%s\n' "$https_bind" | grep -Eq '^[0-9]+$'
[ "$http_bind" != "$https_bind" ]
[ "$tls_mode" = local ]
[ "$server_name" = localhost ]
[ "$client_base_url" = "http://localhost:$http_bind" ]
[ "$tls_cert_file" = "$run_dir/provision/message-tls-external-cert.pem" ]
[ "$tls_key_file" = "$run_dir/provision/message-tls-external-key.pem" ]
[ "$master_key_env" = "$run_dir/provision/core-secret-master-key" ]
[ "$(stat -c '%a' "$run_dir/provision/.manifest")" = 400 ]
printf 'stack_name=%s\n' "$stack_name" | cmp -s - <(grep '^stack_name=' "$run_dir/provision/.manifest")
printf 'message_http_bind=%s\n' "$http_bind" | cmp -s - <(grep '^message_http_bind=' "$run_dir/provision/.manifest")
printf 'message_https_bind=%s\n' "$https_bind" | cmp -s - <(grep '^message_https_bind=' "$run_dir/provision/.manifest")
printf 'message_client_base_url=%s\n' "$client_base_url" | cmp -s - <(grep '^message_client_base_url=' "$run_dir/provision/.manifest")
printf 'message_tls_mode=local\n' | cmp -s - <(grep '^message_tls_mode=' "$run_dir/provision/.manifest")
printf 'message_server_name=localhost\n' | cmp -s - <(grep '^message_server_name=' "$run_dir/provision/.manifest")
printf 'message_tls_cert_path=%s\n' "$tls_cert_file" | cmp -s - <(grep '^message_tls_cert_path=' "$run_dir/provision/.manifest")
printf 'message_tls_key_path=%s\n' "$tls_key_file" | cmp -s - <(grep '^message_tls_key_path=' "$run_dir/provision/.manifest")
for tls_file in "$tls_cert_file" "$tls_key_file"; do
  [ -f "$tls_file" ] && [ ! -L "$tls_file" ]
  [ "$(stat -c '%a' "$tls_file")" = 400 ]
  [ "$(stat -c '%u' "$tls_file")" = "$(id -u)" ]
done
[ ! -s "$tls_cert_file" ] && [ ! -s "$tls_key_file" ]
printf 'message_tls_cert_device=%s\n' "$(stat -c '%d' "$tls_cert_file")" | cmp -s - <(grep '^message_tls_cert_device=' "$run_dir/provision/.manifest")
printf 'message_tls_cert_inode=%s\n' "$(stat -c '%i' "$tls_cert_file")" | cmp -s - <(grep '^message_tls_cert_inode=' "$run_dir/provision/.manifest")
printf 'message_tls_cert_uid=%s\n' "$(stat -c '%u' "$tls_cert_file")" | cmp -s - <(grep '^message_tls_cert_uid=' "$run_dir/provision/.manifest")
printf 'message_tls_cert_sha256=%s\n' "$(sha256sum "$tls_cert_file" | awk '{print $1}')" | cmp -s - <(grep '^message_tls_cert_sha256=' "$run_dir/provision/.manifest")
printf 'message_tls_key_device=%s\n' "$(stat -c '%d' "$tls_key_file")" | cmp -s - <(grep '^message_tls_key_device=' "$run_dir/provision/.manifest")
printf 'message_tls_key_inode=%s\n' "$(stat -c '%i' "$tls_key_file")" | cmp -s - <(grep '^message_tls_key_inode=' "$run_dir/provision/.manifest")
printf 'message_tls_key_uid=%s\n' "$(stat -c '%u' "$tls_key_file")" | cmp -s - <(grep '^message_tls_key_uid=' "$run_dir/provision/.manifest")
printf 'message_tls_key_sha256=%s\n' "$(sha256sum "$tls_key_file" | awk '{print $1}')" | cmp -s - <(grep '^message_tls_key_sha256=' "$run_dir/provision/.manifest")
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
jq -e --arg generation "$account_generation" '.services["message-server"].environment.P2P_ACCOUNT_GENERATION == $generation' "$production_rendered" >/dev/null

shellcheck \
  "$script_dir/provision-local.sh" \
  "$script_dir/verify-topology.sh" \
  "$script_dir/cleanup-local.sh" \
  "$script_dir/cleanup-provision-failure.sh" \
  "$script_dir/cleanup-provision-failure.test.sh" \
  "$script_dir/accept-local.sh" \
  "$script_dir/accept-local.test.sh" \
  "$script_dir/bootstrap-local-account.sh" \
  "$script_dir/export-portal-bootstrap.sh" \
  "$script_dir/build-local.sh" \
  "$script_dir/start-local.sh" \
  "$script_dir/start-local.test.sh" \
  "$script_dir/agent-runtime-local-common.sh" \
  "$script_dir/stop-agent-local.sh" \
  "$script_dir/restart-agent-local.sh" \
  "$script_dir/agent-runtime-local.test.sh" \
  "$script_dir/verify-first-fresh.sh" \
  "$script_dir/verify-first-fresh.test.sh" \
  "$script_dir/prepare-runner-cgroups.sh" \
  "$script_dir/prepare-runner-cgroups.test.sh" \
  "$script_dir/manage-runner-apparmor.sh" \
  "$script_dir/manage-runner-apparmor.test.sh" \
  "$script_dir/update-agent-local.sh" \
  "$script_dir/update-agent-local.test.sh" \
  "$script_dir/provision-local.test.sh" \
  "$script_dir/verify-production-images.sh" \
  "$script_dir/verify-production-images.test.sh" \
  "$script_dir/verify-production-tls.sh" \
  "$script_dir/verify-production-tls.test.sh" \
  "$script_dir/cutover-edge.sh" \
  "$script_dir/cutover-edge.test.sh" \
  "$script_dir/verify-build-contexts.sh" \
  "$script_dir/initialize-capability-ca.sh" \
  "$script_dir/initialize-capability-ca.test.sh" \
  "$script_dir/initialize-message-server.sh" \
  "$script_dir/initialize-message-server.test.sh" \
  "$script_dir/materialize-agent-secrets.sh" \
  "$script_dir/message-server-entrypoint.sh" \
  "$script_dir/message-server-entrypoint.test.sh" \
  "$script_dir/message-server-healthcheck.test.sh" \
  "$stack_dir/aws/validate-policy.sh" \
  "$stack_dir/aws/validate-policy.test.sh"
"$script_dir/message-server-entrypoint.test.sh" >/dev/null
"$script_dir/message-server-healthcheck.test.sh" >/dev/null
"$script_dir/initialize-capability-ca.test.sh" >/dev/null
"$script_dir/initialize-message-server.test.sh" >/dev/null
"$script_dir/accept-local.test.sh" >/dev/null
"$script_dir/start-local.test.sh" >/dev/null
"$script_dir/cleanup-provision-failure.test.sh" >/dev/null
"$script_dir/agent-runtime-local.test.sh" >/dev/null
"$script_dir/verify-first-fresh.test.sh" >/dev/null
"$script_dir/prepare-runner-cgroups.test.sh" >/dev/null
"$script_dir/manage-runner-apparmor.test.sh" >/dev/null
"$script_dir/update-agent-local.test.sh" >/dev/null
apparmor_parser --preprocess "$stack_dir/apparmor.d/dirextalk-runner-userns" >/dev/null
grep -Fqx 'profile dirextalk-runner-userns flags=(unconfined) {' "$stack_dir/apparmor.d/dirextalk-runner-userns"
grep -Fqx '  userns,' "$stack_dir/apparmor.d/dirextalk-runner-userns"
"$script_dir/provision-local.test.sh" >/dev/null
"$script_dir/verify-production-images.test.sh" >/dev/null
"$script_dir/verify-production-tls.test.sh" >/dev/null
"$script_dir/cutover-edge.test.sh" >/dev/null
"$stack_dir/aws/validate-policy.test.sh" >/dev/null
"$script_dir/verify-build-contexts.sh" "$agent_root" "$message_root" >/dev/null

jq -e --arg http "$http_bind" --arg https "$https_bind" '
  ([.services["message-server"].ports[] | .published] | sort) == [$http, $https] and
  ([.services["message-server"].ports[] | .host_ip] | unique) == ["127.0.0.1"] and
  ([.services | to_entries[] | select(.key != "message-server") | .value.ports // [] | length] | add // 0) == 0
' "$rendered" >/dev/null || {
  published=$(jq -c '[.services | to_entries[] | .value.ports // [] | .[] | .published] | sort' "$rendered")
  echo "unexpected host-published ports (message-server only, $http_bind/$https_bind expected): $published" >&2
  exit 1
}

jq -e --arg http "$http_bind" '
  ([.services["message-server"].ports[] | {published, host_ip, target}]) ==
    [{"published":$http,"host_ip":"127.0.0.1","target":8008}] and
  ([.services | to_entries[] | select(.key != "message-server") | .value.ports // [] | length] | add // 0) == 0
' "$production_rendered" >/dev/null || {
  echo "edge-terminated production must publish only loopback HTTP :8008" >&2
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
  ([.services | to_entries[] | .value.build // null] | all(. == null)) and
  (.services.agent.image == .services["extension-runner"].image) and
  (.services.agent.image == .services["core-runner"].image)
' "$production_rendered" >/dev/null

jq -e '
  (.services["message-server-init"].entrypoint == ["/usr/local/bin/initialize-message-server"]) and
  ([.services["message-server-init"].configs[] | select(.target == "/usr/local/bin/initialize-message-server" and .mode == "0555")] | length) == 1 and
  ([.services["message-server-init"].configs[] | select(.target == "/usr/local/bin/initialize-capability-ca" and .mode == "0555")] | length) == 1 and
  (.services["agent-secret-init"].entrypoint == ["/usr/local/bin/materialize-agent-secrets"]) and
  ([.services["agent-secret-init"].configs[] | select(.target == "/usr/local/bin/materialize-agent-secrets" and .mode == "0555")] | length) == 1 and
  ([.services["message-server-init"].secrets[] | select(.target == "/run/secrets/message_registration_shared_secret")] | length) == 1 and
  ([.services["message-server-init"].secrets[] | select(.target == "/run/secrets/message_database_url")] | length) == 0 and
  (.services["message-server-init"].environment.MESSAGE_SERVER_TLS_MODE == "edge-terminated") and
  (.services["message-server-init"].environment.MESSAGE_DEPLOYMENT_MODE == "production") and
  (.services["message-server-init"].environment.MESSAGE_LOCAL_BOOTSTRAP_ENABLED == "false") and
  ([.services["message-server-init"].volumes[] | select(.target == "/bootstrap/external/server.crt")] | length) == 1 and
  ([.services["message-server-init"].volumes[] | select(.target == "/bootstrap/external/server.key")] | length) == 1 and
  ([.services["message-server"].secrets[] | select(.target == "/run/secrets/message_portal_password")] | length) == 1 and
  (.services["message-server"].environment.P2P_PORTAL_PASSWORD_FILE == "/run/secrets/message_portal_password") and
  (.services["message-server"].cap_add | index("DAC_READ_SEARCH")) != null and
  (.services["message-server"].entrypoint == ["/usr/local/bin/message-server-entrypoint"]) and
  (.services["message-server"].init == true) and
  (.services["message-server"].command == [
    "--http-bind-address", ":8008"
  ]) and
  (.services["message-server"].healthcheck.test == [
    "CMD", "wget", "-q", "-O", "-", "http://127.0.0.1:8008/_p2p/health"
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
  .services["extension-runner"].image == "dirextalk-agent:split-local" and
  .services["core-runner"].image == "dirextalk-agent:split-local" and
  .services["extension-runner"].build == null and
  .services["core-runner"].build == null
' "$rendered" >/dev/null

jq -e '
  .services.agent.image == .services["extension-runner"].image and
  .services.agent.image == .services["core-runner"].image and
  .services["extension-runner"].entrypoint == ["/usr/local/bin/dirextalk-extension-runner"] and
  .services["core-runner"].entrypoint == ["/usr/local/bin/dirextalk-core-runner"] and
  .services["extension-runner"].network_mode == "none" and
  .services["core-runner"].network_mode == "none" and
  .services["extension-runner"].networks == null and
  .services["core-runner"].networks == null and
  .services["extension-runner"].secrets == null and
  .services["core-runner"].secrets == null and
  .services["extension-runner"].user == "65531:65531" and
  .services["core-runner"].user == "65530:65530" and
  .services["extension-runner"].read_only == true and
  .services["core-runner"].read_only == true and
  .services["extension-runner"].cap_drop == ["ALL"] and
  .services["core-runner"].cap_drop == ["ALL"] and
  ([.services["extension-runner"].security_opt[], .services["core-runner"].security_opt[]] | map(select(. == "apparmor=dirextalk-runner-userns")) | length) == 2 and
  ([.services["extension-runner"].security_opt[], .services["core-runner"].security_opt[]] | map(select(. == "seccomp=unconfined")) | length) == 2 and
  ([.services["extension-runner"].security_opt[], .services["core-runner"].security_opt[]] | map(select(. == "no-new-privileges:true")) | length) == 2 and
  (.services["extension-runner"].group_add | index("65532")) != null and
  (.services["core-runner"].group_add | index("65532")) != null and
  .services["extension-runner"].build == null and
  .services["core-runner"].build == null and
  .services["extension-runner"].build.additional_contexts == null and
  .services["core-runner"].build.additional_contexts == null and
  ([.services["extension-runner"].volumes[], .services["core-runner"].volumes[]] | map(select(.target == "/cgroup") | .bind.create_host_path) | all(. == false)) and
  ([.services["extension-runner"].volumes[], .services["core-runner"].volumes[]] | all(.type == "volume" or (.type == "bind" and .target == "/cgroup"))) and
  ([.services["extension-runner"].volumes[], .services["core-runner"].volumes[]] | map(.source // "") | any(test("docker.sock|/var/run/docker"))) == false and
  (.services["extension-runner"].healthcheck.test[0] == "CMD") and
  (.services["core-runner"].healthcheck.test[0] == "CMD") and
  (.services["extension-runner"].depends_on["extension-socket-init"].condition == "service_completed_successfully") and
  (.services["core-runner"].depends_on["core-runner-socket-init"].condition == "service_completed_successfully") and
  (.services["extension-runner"].depends_on["extension-runner-storage-init"].condition == "service_completed_successfully") and
  (.services["core-runner"].depends_on["core-runner-storage-init"].condition == "service_completed_successfully") and
  (.services["extension-socket-init"].command | join(" ") | contains("chmod 2750 /socket")) and
  (.services["core-runner-socket-init"].command | join(" ") | contains("chmod 2750 /socket")) and
  (.services["extension-runner-storage-init"].network_mode == "none") and
  (.services["core-runner-storage-init"].network_mode == "none") and
  (.services["extension-runner-storage-init"].command | join(" ") | contains("chown 65531:65531 /install /state")) and
  (.services["extension-runner-storage-init"].command | join(" ") | contains("chmod 0700 /install /state")) and
  (.services["extension-runner-storage-init"].command | join(" ") | contains("chown 65531:65532 /workspace")) and
  (.services["extension-runner-storage-init"].command | join(" ") | contains("chmod 0770 /workspace")) and
  (.services["core-runner-storage-init"].command | join(" ") | contains("chown 65530:65530 /install /workspace /state")) and
  (.services["core-runner-storage-init"].command | join(" ") | contains("chmod 0700 /install /workspace /state"))
' "$rendered" >/dev/null

jq -e --arg http "$http_bind" --arg https "$https_bind" '
  ([.services["message-server"].ports[] | .published] | sort) == [$http, $https] and
  ([.services["message-server"].ports[] | .host_ip] | unique) == ["127.0.0.1"] and
  ([.services | to_entries[] | select(.key != "message-server") | .value.ports // [] | length] | add // 0) == 0
' "$rendered" >/dev/null

jq -e '
  (.services.agent.volumes | map(.source // "") | any(test("agent_extension_socket|core_runner_socket"))) and
  (.services.agent.volumes | map(.source // "") | any(test("agent_extension_staging|agent_extension_workspaces"))) and
  (.services.agent.depends_on["extension-runner"].condition == "service_healthy") and
  (.services.agent.depends_on["extension-runner"].required == true) and
  (.services.agent.depends_on["core-runner"].condition == "service_healthy") and
  (.services.agent.depends_on["core-runner"].required == true)
' "$rendered" >/dev/null

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
