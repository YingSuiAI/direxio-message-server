#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/provision-local.sh
[ -x "$script" ] || { echo "provision-local.sh must be executable" >&2; exit 1; }
bash -n "$script"
grep -Fq "runner_apparmor_manager_path=\${DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH:-/usr/local/libexec/dirextalk/split-agent/scripts/manage-runner-apparmor.sh}" "$script"
grep -Fq "[ \"\$runner_apparmor_manager_path\" = /usr/local/libexec/dirextalk/split-agent/scripts/manage-runner-apparmor.sh ]" "$script"
if grep -Fq "[ \"\$runner_apparmor_manager_path\" = \"\$script_dir/manage-runner-apparmor.sh\" ]" "$script"; then
  echo "production provision must not bind the root receipt to a user-owned repository manager" >&2
  exit 1
fi
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x "$script"
fi
if grep -Fq -- "[ -w \"\$value/cgroup.subtree_control\" ]" "$script" || \
   grep -Fq -- "[ -w \"\$value/cgroup.procs\" ]" "$script"; then
  echo "provision-local.sh must not test cgroup writability as the current user" >&2
  exit 1
fi
if grep -Fq -- "[ -s \"\$value/cgroup.controllers\" ]" "$script"; then
  echo "provision-local.sh must read cgroupfs controller contents instead of using stat size" >&2
  exit 1
fi
grep -Fq -- "[ -n \"\$controllers\" ]" "$script"
grep -Fq -- 'validate_target_write_access' "$script"
grep -Fq -- 'getfacl -cp' "$script"
message_server_contract=$(sed -n '/^  message-server:/,/^networks:/p' "$script_dir/../compose.yaml")
grep -A2 -F '      coturn:' <<<"$message_server_contract" | grep -Fq 'condition: service_healthy'
coturn_contract=$(sed -n '/^  coturn:/,/^  postgres:/p' "$script_dir/../compose.yaml")
grep -Fq 'cap_add: ["DAC_READ_SEARCH", "NET_BIND_SERVICE"]' <<<"$coturn_contract"
grep -Fq "grep -Fqx 'listening-port=3478' /run/secrets/turnserver.conf && kill -0 1" <<<"$coturn_contract"
if grep -Fq 'test: ["CMD-SHELL", "kill -0 1"]' <<<"$coturn_contract"; then
  echo 'coturn healthcheck can pass without reading its protected configuration' >&2
  exit 1
fi

# cgroup-v2 controller pseudo-files report stat size zero even when readable
# controller content is present. Exercise the real filesystem semantic that
# the production consumer must handle rather than relying on regular fixtures.
if [ "$(stat -fc '%T' /sys/fs/cgroup 2>/dev/null || true)" = cgroup2fs ] &&
   [ -f /sys/fs/cgroup/cgroup.controllers ] &&
   [ -r /sys/fs/cgroup/cgroup.controllers ]; then
  [ ! -s /sys/fs/cgroup/cgroup.controllers ]
  live_controllers=$(tr '\n' ' ' </sys/fs/cgroup/cgroup.controllers)
  [ -n "$live_controllers" ]
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-provision-local.XXXXXX")
cleanup() {
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT
mkdir -p "$tmp_dir/bin"

# Provisioning only needs collision inspection from Docker. Keep this test
# host-only and prove the generated YAML/env gates without touching Docker.
cat >"$tmp_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  network|volume) exit 1 ;;
  ps) exit 0 ;;
  *) exit 1 ;;
esac
EOF
chmod 755 "$tmp_dir/bin/docker"
export PATH=$tmp_dir/bin:$PATH
export DIREXTALK_SPLIT_FIXTURE_MODE=true
export DIREXTALK_SPLIT_TEST_MODE=true

# With no supplied portal source, the generated initial password is exactly
# eight decimal digits in a protected file and never appears in output.
generated_dir=$tmp_dir/generated
generated_log=$tmp_dir/generated.log
"$script" "$generated_dir" >"$generated_log" 2>&1
generated_password=$(tr -d '\n' <"$generated_dir/message-portal-password")
[ "$(wc -c <"$generated_dir/message-portal-password")" -eq 9 ]
printf '%s\n' "$generated_password" | grep -Eq '^[0-9]{8}$'
[ "$(stat -c '%a' "$generated_dir/message-portal-password")" = 400 ]
if grep -Fq "$generated_password" "$generated_log" || grep -Fq "$generated_password" "$generated_dir/.env"; then
  echo "generated portal password leaked into output or .env" >&2
  exit 1
fi
if grep -Eq '^DIREXTALK_(EXTENSION_RUNNER|CORE_RUNNER)_IMAGE_' "$generated_dir/.env"; then
  echo "runner-specific image variables must not be provisioned" >&2
  exit 1
fi
postgres_admin_password=$(tr -d '\n' <"$generated_dir/postgres-admin-password")
message_postgres_password=$(tr -d '\n' <"$generated_dir/message-postgres-password")
agent_postgres_password=$(tr -d '\n' <"$generated_dir/agent-postgres-password")
for password in "$postgres_admin_password" "$message_postgres_password" "$agent_postgres_password"; do
  printf '%s\n' "$password" | grep -Eq '^[0-9a-f]{48}$'
done
[ "$postgres_admin_password" != "$message_postgres_password" ]
[ "$postgres_admin_password" != "$agent_postgres_password" ]
[ "$message_postgres_password" != "$agent_postgres_password" ]
[ "$(stat -c '%a' "$generated_dir/postgres-admin-password")" = 400 ]
grep -Fqx "DIREXTALK_POSTGRES_ADMIN_PASSWORD_FILE=$generated_dir/postgres-admin-password" "$generated_dir/.env"
grep -Fqx "DIREXTALK_POSTGRES_ENTRYPOINT_FILE=$script_dir/postgres-entrypoint.sh" "$generated_dir/.env"
grep -Fqx 'DIREXTALK_POSTGRES_VOLUME='"$(sed -n 's/^DIREXTALK_SPLIT_STACK_NAME=//p' "$generated_dir/.env")"'-postgres' "$generated_dir/.env"
grep -Fqx 'resource.volume.postgres='"$(sed -n 's/^DIREXTALK_SPLIT_STACK_NAME=//p' "$generated_dir/.env")"'-postgres' "$generated_dir/.manifest"
if grep -Eq '^DIREXTALK_(MESSAGE|AGENT)_POSTGRES_VOLUME=' "$generated_dir/.env" ||
   grep -Eq '^resource\.volume\.(message_postgres|agent_postgres)=' "$generated_dir/.manifest"; then
  echo "split PostgreSQL volumes survived fresh-state provisioning" >&2
  exit 1
fi
grep -Eq '^postgresql://dirextalk_message_server:[0-9a-f]{48}@message-postgres:5432/dirextalk_message_server\?sslmode=disable$' "$generated_dir/message-database-url"
grep -Eq '^postgresql://dirextalk_agent:[0-9a-f]{48}@agent-postgres:5432/dirextalk_agent\?sslmode=disable$' "$generated_dir/agent-database-url"
grep -Fqx 'core_knowledge_vector_dimension: 1536' "$generated_dir/agent-config.yaml"
if grep -Eiq 'qdrant|core_knowledge_content_quota_bytes' "$generated_dir/agent-config.yaml"; then
  echo "retired Knowledge storage configuration survived fresh-state provisioning" >&2
  exit 1
fi
for public_file in "$generated_log" "$generated_dir/.env" "$generated_dir/.manifest"; do
  for password in "$postgres_admin_password" "$message_postgres_password" "$agent_postgres_password"; do
    if grep -Fq "$password" "$public_file"; then
      echo "PostgreSQL credential leaked into public provisioning output" >&2
      exit 1
    fi
  done
done
turn_secret=$(tr -d '\n' <"$generated_dir/turn-shared-secret")
[ "${#turn_secret}" -eq 64 ]
printf '%s\n' "$turn_secret" | grep -Eq '^[0-9a-f]{64}$'
[ "$(stat -c '%a' "$generated_dir/turn-shared-secret")" = 400 ]
[ "$(stat -c '%a' "$generated_dir/turnserver.conf")" = 400 ]
if grep -Fqx 'no-sqlite' "$generated_dir/turnserver.conf"; then
  echo 'coturn 4.6.3 does not accept the no-sqlite configuration option' >&2
  exit 1
fi
config_turn_secret=
while IFS= read -r line; do
  case "$line" in static-auth-secret=*) config_turn_secret=${line#static-auth-secret=} ;; esac
done <"$generated_dir/turnserver.conf"
[ "$config_turn_secret" = "$turn_secret" ]
grep -Fqx 'DIREXTALK_TURN_EXTERNAL_IP=127.0.0.1' "$generated_dir/.env"
grep -Fqx "DIREXTALK_TURN_SHARED_SECRET_FILE=$generated_dir/turn-shared-secret" "$generated_dir/.env"
grep -Fqx "DIREXTALK_COTURN_CONFIG_FILE=$generated_dir/turnserver.conf" "$generated_dir/.env"
grep -Fqx 'DIREXTALK_RELEASE_CATALOG_ORIGIN=https://imadmin.dirextalk.ai' "$generated_dir/.env"
for public_file in "$generated_log" "$generated_dir/.env"; do
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      *"$turn_secret"*) echo "TURN shared secret leaked into output or .env" >&2; exit 1 ;;
    esac
  done <"$public_file"
done

# External TLS is provisioned only from two protected, current-UID source
# files. Source paths and material stay out of logs, .env, and the manifest;
# only output targets and their immutable identities are recorded.
tls_cert_source=$tmp_dir/tls-cert-source.pem
tls_key_source=$tmp_dir/tls-key-source.pem
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj '/C=CN/ST=Beijing/L=Beijing/O=Dirextalk/CN=message.example.com' \
  -addext 'subjectAltName=DNS:message.example.com' \
  -keyout "$tls_key_source" -out "$tls_cert_source" >/dev/null 2>&1
chmod 400 "$tls_cert_source" "$tls_key_source"
external_dir=$tmp_dir/external
external_log=$tmp_dir/external.log
DIREXTALK_MESSAGE_TLS_MODE=external \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
  DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_key_source \
  "$script" "$external_dir" '' '' '' '' >"$external_log" 2>&1
grep -Fqx 'DIREXTALK_MESSAGE_TLS_MODE=external' "$external_dir/.env"
grep -Fqx 'DIREXTALK_MESSAGE_SERVER_NAME=message.example.com' "$external_dir/.env"
grep -Fqx 'DIREXTALK_MESSAGE_CLIENT_BASE_URL=https://message.example.com' "$external_dir/.env"
grep -Fqx 'core_static_sites_public_origin: https://message.example.com' "$external_dir/agent-config.yaml"
grep -Fqx "DIREXTALK_MESSAGE_TLS_CERT_FILE=$external_dir/message-tls-external-cert.pem" "$external_dir/.env"
grep -Fqx "DIREXTALK_MESSAGE_TLS_KEY_FILE=$external_dir/message-tls-external-key.pem" "$external_dir/.env"
grep -Fqx 'message_tls_mode=external' "$external_dir/.manifest"
grep -Fqx 'message_server_name=message.example.com' "$external_dir/.manifest"
grep -Fqx 'message_client_base_url=https://message.example.com' "$external_dir/.manifest"
grep -Fqx "message_tls_cert_path=$external_dir/message-tls-external-cert.pem" "$external_dir/.manifest"
grep -Fqx "message_tls_key_path=$external_dir/message-tls-external-key.pem" "$external_dir/.manifest"
cmp -- "$tls_cert_source" "$external_dir/message-tls-external-cert.pem"
cmp -- "$tls_key_source" "$external_dir/message-tls-external-key.pem"
[ "$(stat -c '%a' "$external_dir/message-tls-external-cert.pem")" = 400 ]
[ "$(stat -c '%a' "$external_dir/message-tls-external-key.pem")" = 400 ]
[ "$(stat -c '%u' "$external_dir/message-tls-external-cert.pem")" = "$(id -u)" ]
[ "$(stat -c '%u' "$external_dir/message-tls-external-key.pem")" = "$(id -u)" ]
grep -Fqx "message_tls_cert_device=$(stat -c '%d' "$external_dir/message-tls-external-cert.pem")" "$external_dir/.manifest"
grep -Fqx "message_tls_cert_inode=$(stat -c '%i' "$external_dir/message-tls-external-cert.pem")" "$external_dir/.manifest"
grep -Fqx "message_tls_cert_uid=$(stat -c '%u' "$external_dir/message-tls-external-cert.pem")" "$external_dir/.manifest"
grep -Fqx "message_tls_cert_sha256=$(sha256sum "$external_dir/message-tls-external-cert.pem" | awk '{print $1}')" "$external_dir/.manifest"
grep -Fqx "message_tls_key_device=$(stat -c '%d' "$external_dir/message-tls-external-key.pem")" "$external_dir/.manifest"
grep -Fqx "message_tls_key_inode=$(stat -c '%i' "$external_dir/message-tls-external-key.pem")" "$external_dir/.manifest"
grep -Fqx "message_tls_key_uid=$(stat -c '%u' "$external_dir/message-tls-external-key.pem")" "$external_dir/.manifest"
grep -Fqx "message_tls_key_sha256=$(sha256sum "$external_dir/message-tls-external-key.pem" | awk '{print $1}')" "$external_dir/.manifest"
if grep -Fq "$tls_cert_source" "$external_log" || grep -Fq "$tls_key_source" "$external_log" || \
  grep -Fq "$tls_cert_source" "$external_dir/.env" || grep -Fq "$tls_key_source" "$external_dir/.env" || \
  grep -Fq "$tls_cert_source" "$external_dir/.manifest" || grep -Fq "$tls_key_source" "$external_dir/.manifest" || \
  grep -Eq 'BEGIN (CERTIFICATE|.*PRIVATE KEY)' "$external_log" "$external_dir/.env" "$external_dir/.manifest"; then
  echo "external TLS source path or material leaked into provision output" >&2
  exit 1
fi
"$script_dir/verify-production-tls.sh" "$external_dir/.env" >/dev/null


if DIREXTALK_SPLIT_COMPOSE_MODE=production \
  "$script" "$tmp_dir/production-without-runners" >/dev/null 2>&1; then
  echo "production without both isolated runners unexpectedly passed" >&2
  exit 1
fi
production_dir=$tmp_dir/production
DIREXTALK_SPLIT_FIXTURE_MODE=true \
DIREXTALK_SPLIT_TEST_MODE=true \
DIREXTALK_SPLIT_COMPOSE_MODE=production \
DIREXTALK_CORE_EXTENSION_ENABLED=true \
DIREXTALK_CORE_WORKLOAD_ENABLED=true \
DIREXTALK_TURN_EXTERNAL_IP=192.0.2.10 \
DIREXTALK_MESSAGE_SERVER_IMAGE=docker.io/dirextalk/message-server:latest \
DIREXTALK_MESSAGE_SERVER_VERSION=v1.1.33 \
DIREXTALK_MESSAGE_SOURCE_REVISION=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:latest \
DIREXTALK_AGENT_VERSION=v1.0.69 \
DIREXTALK_AGENT_SOURCE_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
DIREXTALK_MESSAGE_TLS_MODE=external \
DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_key_source \
  "$script" "$production_dir" >/dev/null
grep -Fqx 'DIREXTALK_SPLIT_COMPOSE_MODE=production' "$production_dir/.env"
grep -Fqx 'DIREXTALK_RELEASE_CATALOG_ORIGIN=https://imadmin.dirextalk.ai' "$production_dir/.env"
grep -Fqx 'compose_mode=production' "$production_dir/.manifest"
grep -Fqx 'DIREXTALK_MESSAGE_SERVER_IMAGE=docker.io/dirextalk/message-server:latest' "$production_dir/.env"
grep -Fqx 'DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:latest' "$production_dir/.env"

edge_production_dir=$tmp_dir/production-edge-terminated
DIREXTALK_SPLIT_FIXTURE_MODE=true \
DIREXTALK_SPLIT_TEST_MODE=true \
DIREXTALK_SPLIT_COMPOSE_MODE=production \
DIREXTALK_CORE_EXTENSION_ENABLED=true \
DIREXTALK_CORE_WORKLOAD_ENABLED=true \
DIREXTALK_TURN_EXTERNAL_IP=192.0.2.10 \
DIREXTALK_MESSAGE_SERVER_IMAGE=docker.io/dirextalk/message-server:latest \
DIREXTALK_MESSAGE_SERVER_VERSION=v1.1.33 \
DIREXTALK_MESSAGE_SOURCE_REVISION=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:latest \
DIREXTALK_AGENT_VERSION=v1.0.69 \
DIREXTALK_AGENT_SOURCE_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
DIREXTALK_MESSAGE_TLS_MODE=edge-terminated \
DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  "$script" "$edge_production_dir" >/dev/null
grep -Fqx 'DIREXTALK_MESSAGE_TLS_MODE=edge-terminated' "$edge_production_dir/.env"
grep -Fqx 'DIREXTALK_MESSAGE_CLIENT_BASE_URL=https://message.example.com' "$edge_production_dir/.env"
grep -Fqx 'core_static_sites_public_origin: https://message.example.com' "$edge_production_dir/agent-config.yaml"
grep -Fqx 'core_extension_staging_root: /var/lib/dirextalk-agent/extension-staging' "$edge_production_dir/agent-config.yaml"
if grep -Eq 'core_aws|DIREXTALK_CORE_AWS|core_cloud_worker:|worker_control|model_relay|artifact_bucket|kms_key|ami_id|resource_graph' \
    "$edge_production_dir/agent-config.yaml" "$edge_production_dir/.env"; then
  echo 'production config retained a superseded deploy-time AWS or Cloud Worker binding' >&2
  exit 1
fi
grep -Fqx 'message_tls_mode=edge-terminated' "$edge_production_dir/.manifest"
[ ! -s "$edge_production_dir/message-tls-external-cert.pem" ]
[ ! -s "$edge_production_dir/message-tls-external-key.pem" ]
"$script_dir/verify-production-tls.sh" "$edge_production_dir/.env" >/dev/null

if DIREXTALK_MESSAGE_TLS_MODE=edge-terminated \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  "$script" "$tmp_dir/local-edge-terminated" >/dev/null 2>&1; then
  echo "edge-terminated TLS outside production unexpectedly passed" >&2
  exit 1
fi
if DIREXTALK_SPLIT_COMPOSE_MODE=production \
  DIREXTALK_CORE_EXTENSION_ENABLED=true \
  DIREXTALK_CORE_WORKLOAD_ENABLED=true \
  DIREXTALK_MESSAGE_TLS_MODE=edge-terminated \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
  "$script" "$tmp_dir/edge-terminated-with-cert" >/dev/null 2>&1; then
  echo "edge-terminated TLS with a direct certificate source unexpectedly passed" >&2
  exit 1
fi

if DIREXTALK_MESSAGE_TLS_MODE=external \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_key_source \
  "$script" "$tmp_dir/external-missing-cert" '' '' '' '' >/dev/null 2>&1; then
  echo "external TLS without certificate source was unexpectedly accepted" >&2
  exit 1
fi
if DIREXTALK_MESSAGE_TLS_MODE=external \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
  "$script" "$tmp_dir/external-missing-key" '' '' '' '' >/dev/null 2>&1; then
  echo "external TLS without private-key source was unexpectedly accepted" >&2
  exit 1
fi
if DIREXTALK_MESSAGE_TLS_MODE=external \
  DIREXTALK_MESSAGE_SERVER_NAME=other.example.com \
  DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
  DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_key_source \
  "$script" "$tmp_dir/external-wrong-host" '' '' '' '' >/dev/null 2>&1; then
  echo "external TLS with a mismatched certificate identity was unexpectedly accepted" >&2
  exit 1
fi
tls_wrong_key=$tmp_dir/tls-wrong-key.pem
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$tls_wrong_key" >/dev/null 2>&1
chmod 400 "$tls_wrong_key"
if DIREXTALK_MESSAGE_TLS_MODE=external \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
  DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_wrong_key \
  "$script" "$tmp_dir/external-wrong-pair" '' '' '' '' >/dev/null 2>&1; then
  echo "external TLS with a mismatched certificate/key pair was unexpectedly accepted" >&2
  exit 1
fi
chmod 600 "$tls_cert_source"
if DIREXTALK_MESSAGE_TLS_MODE=external \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
  DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_key_source \
  "$script" "$tmp_dir/external-insecure-cert" '' '' '' '' >/dev/null 2>&1; then
  echo "mode-0600 external TLS certificate source was unexpectedly accepted" >&2
  exit 1
fi
chmod 400 "$tls_cert_source"
tls_symlink=$tmp_dir/tls-cert-symlink.pem
ln -s "$tls_cert_source" "$tls_symlink"
if DIREXTALK_MESSAGE_TLS_MODE=external \
  DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
  DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_symlink \
  DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_key_source \
  "$script" "$tmp_dir/external-symlink-source" '' '' '' '' >/dev/null 2>&1; then
  echo "symlinked external TLS source was unexpectedly accepted" >&2
  exit 1
fi

provider_bad_source=$tmp_dir/provider-bad-mode
printf 'provider-secret' >"$provider_bad_source"
chmod 600 "$provider_bad_source"
if "$script" "$tmp_dir/provider-insecure" "$provider_bad_source" >/dev/null 2>&1; then
  echo "mode-0600 provider source was unexpectedly accepted" >&2
  exit 1
fi

for fixed_uid_case in \
  'DIREXTALK_CORE_EXTENSION_RUNNER_UID:65530:extension' \
  'DIREXTALK_CORE_WORKLOAD_RUNNER_UID:65531:workload' \
  'DIREXTALK_CORE_EXTENSION_RUNNER_UID:not-a-uid:extension-nonnumeric' \
  'DIREXTALK_CORE_WORKLOAD_RUNNER_UID::workload-empty'; do
  IFS=: read -r fixed_uid_name fixed_uid_value fixed_uid_label <<<"$fixed_uid_case"
  if env "$fixed_uid_name=$fixed_uid_value" "$script" "$tmp_dir/fixed-uid-$fixed_uid_label" >/dev/null 2>"$tmp_dir/fixed-uid-$fixed_uid_label.stderr"; then
    echo "$fixed_uid_name override $fixed_uid_value was unexpectedly accepted" >&2
    exit 1
  fi
  grep -Fq "$fixed_uid_name is fixed at" "$tmp_dir/fixed-uid-$fixed_uid_label.stderr"
done

# Supplying the image ABI values explicitly remains accepted and is normalized
# back into the generated environment/configuration.
explicit_uid_dir=$tmp_dir/explicit-fixed-uids
DIREXTALK_CORE_EXTENSION_RUNNER_UID=65531 \
  DIREXTALK_CORE_WORKLOAD_RUNNER_UID=65530 \
  "$script" "$explicit_uid_dir" >/dev/null
grep -Fqx 'DIREXTALK_CORE_EXTENSION_RUNNER_UID=65531' "$explicit_uid_dir/.env"
grep -Fqx 'DIREXTALK_CORE_WORKLOAD_RUNNER_UID=65530' "$explicit_uid_dir/.env"

if DIREXTALK_CORE_EXTENSION_ENABLED=true \
  DIREXTALK_CORE_EXTENSION_RUNNER_UID=65532 \
  DIREXTALK_EXTENSION_CGROUP_ROOT=/sys/fs/cgroup \
  "$script" "$tmp_dir/extension-agent-uid" >/dev/null 2>&1; then
  echo "extension runner UID collision with Agent UID was unexpectedly accepted" >&2
  exit 1
fi
if DIREXTALK_CORE_WORKLOAD_ENABLED=true \
  DIREXTALK_CORE_WORKLOAD_RUNNER_UID=65532 \
  DIREXTALK_CORE_RUNNER_CGROUP_ROOT=/sys/fs/cgroup \
  "$script" "$tmp_dir/workload-agent-uid" >/dev/null 2>&1; then
  echo "Core runner UID collision with Agent UID was unexpectedly accepted" >&2
  exit 1
fi

# A protected fifth positional source is imported exactly, while its value
# remains absent from process output and the generated non-secret environment.
portal_source=$tmp_dir/portal-password-source
printf '24681357\n' >"$portal_source"
chmod 400 "$portal_source"
source_dir=$tmp_dir/from-source
source_log=$tmp_dir/from-source.log
"$script" "$source_dir" '' '' '' "$portal_source" >"$source_log" 2>&1
cmp -- "$portal_source" "$source_dir/message-portal-password"
[ "$(stat -c '%a' "$source_dir/message-portal-password")" = 400 ]
if grep -Fq '24681357' "$source_log" || grep -Fq '24681357' "$source_dir/.env"; then
  echo "portal password source value leaked into output or .env" >&2
  exit 1
fi

# Invalid mode and malformed content are rejected before provisioning succeeds.
invalid_mode_source=$tmp_dir/portal-password-invalid-mode
printf '24681357\n' >"$invalid_mode_source"
chmod 600 "$invalid_mode_source"
if "$script" "$tmp_dir/invalid-mode" '' '' '' "$invalid_mode_source" >/dev/null 2>&1; then
  echo "mode-0600 portal password source was unexpectedly accepted" >&2
  exit 1
fi
for invalid_content in '2468135' '246813579' '24681357\n24681357' '2468abcd'; do
  invalid_content_source=$tmp_dir/portal-password-invalid-content-${#invalid_content}
  printf '%b\n' "$invalid_content" >"$invalid_content_source"
  chmod 400 "$invalid_content_source"
  if "$script" "$tmp_dir/invalid-content-${#invalid_content}" '' '' '' "$invalid_content_source" >/dev/null 2>&1; then
    echo "malformed portal password source was unexpectedly accepted" >&2
    exit 1
  fi
done
invalid_nul_source=$tmp_dir/portal-password-invalid-nul
printf '24681357\0\n' >"$invalid_nul_source"
chmod 400 "$invalid_nul_source"
if "$script" "$tmp_dir/invalid-nul" '' '' '' "$invalid_nul_source" >/dev/null 2>&1; then
  echo "NUL-containing portal password source was unexpectedly accepted" >&2
  exit 1
fi

# The fifth source is the final supported positional argument.
if "$script" "$tmp_dir/too-many-args" a b c d e >/dev/null 2>"$tmp_dir/usage.stderr"; then
  echo "extra provision-local arguments were unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'usage:' "$tmp_dir/usage.stderr"

# Production provisioning is never allowed to synthesize runner roots.  The
# explicit fixture switch used by this test is removed for this gate.
if env -u DIREXTALK_SPLIT_FIXTURE_MODE -u DIREXTALK_SPLIT_TEST_MODE \
  "$script" "$tmp_dir/missing-host-prep" >/dev/null 2>"$tmp_dir/missing-host-prep.stderr"; then
  echo "provisioning without root-owned host preparation was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256 must come from prepare-runner-cgroups.sh' "$tmp_dir/missing-host-prep.stderr"
if env DIREXTALK_SPLIT_FIXTURE_MODE=true DIREXTALK_SPLIT_TEST_MODE=false \
  "$script" "$tmp_dir/fixture-without-test-mode" >/dev/null 2>"$tmp_dir/fixture-without-test-mode.stderr"; then
  echo "fixture mode without explicit test mode was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'DIREXTALK_SPLIT_FIXTURE_MODE requires explicit DIREXTALK_SPLIT_TEST_MODE=true' "$tmp_dir/fixture-without-test-mode.stderr"

echo "provision-local password and split topology tests passed"
