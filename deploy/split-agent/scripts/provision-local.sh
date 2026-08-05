#!/usr/bin/env bash
set -euo pipefail

# Provision one disposable split-stack namespace. Reuse of non-empty output
# directories is forbidden because Core v1 accepts fresh data only.
#
# Usage:
#   provision-local.sh OUTPUT_DIR [OPENROUTER_KEY_FILE] [EMBEDDING_KEY_FILE]
#
# Secret source files are copied with mode 0400. Values never enter .env,
# Compose YAML, or command output. Missing sources become protected empty
# placeholders so topology/image checks can still run; model acceptance must
# replace them first.

usage() {
  echo "usage: $0 OUTPUT_DIR [OPENROUTER_KEY_FILE] [EMBEDDING_KEY_FILE]" >&2
  exit 2
}

die() {
  echo "split-stack provision: $*" >&2
  exit 1
}

parse_bool() {
  local name=$1 value=$2
  case "$value" in
    true|false) printf '%s' "$value" ;;
    *) die "$name must be exactly true or false" ;;
  esac
}

validate_uuid() {
  local name=$1 value=$2
  printf '%s\n' "$value" | grep -Eiq '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' || die "$name must be a UUIDv4 credential reference"
}

validate_safe_value() {
  local name=$1 value=$2 pattern=$3
  printf '%s\n' "$value" | grep -Eq "$pattern" || die "$name contains unsafe characters"
}

validate_host_port() {
  local name=$1 value=$2
  case "$value" in
    ''|0*|*[!0-9]*) die "$name must be a non-zero decimal host port without leading zeros" ;;
  esac
  [ "$value" -ge 1024 ] 2>/dev/null && [ "$value" -le 65535 ] 2>/dev/null || \
    die "$name must be between 1024 and 65535"
}

validate_vector_dimension() {
  local name=$1 value=$2
  case "$value" in
    ''|0*|*[!0-9]*) die "$name must be a positive decimal dimension without leading zeros" ;;
  esac
  [ "$value" -le 65536 ] 2>/dev/null || die "$name must be at most 65536"
}

validate_uid() {
  local name=$1 value=$2
  case "$value" in
    ''|*[!0-9]*|0*) die "$name must be a positive decimal UID" ;;
  esac
  [ "$value" != "65532" ] || die "$name must differ from the Agent UID 65532"
}

validate_absolute_path() {
  local name=$1 value=$2
  case "$value" in
    /*) ;;
    *) die "$name must be an absolute path" ;;
  esac
  case "$value" in
    *'//'*) die "$name must be a clean path" ;;
    *'..'*) die "$name must be a clean path" ;;
    *[[:space:]]*) die "$name must not contain whitespace" ;;
    *[[:cntrl:]]*) die "$name must not contain control bytes" ;;
    */) die "$name must be a clean path" ;;
  esac
}

validate_socket() {
  local name=$1 value=$2
  validate_absolute_path "$name" "$value"
}

validate_cgroup_parent() {
  local name=$1 value=$2
  case "$value" in
    *[[:cntrl:]]*) die "$name must not contain control bytes" ;;
  esac
  printf '%s\n' "$value" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?\.slice$' || die "$name must be a safe systemd slice name"
}

validate_delegated_cgroup_root() {
  local name=$1 value=$2 marker=$3 parent=$4 fs_type owner current_uid canonical
  case "$value" in
    /sys/fs/cgroup|/sys/fs/cgroup/|/sys/fs/cgroup/system.slice|/sys/fs/cgroup/user.slice|/sys/fs/cgroup/global.slice)
      die "$name must be a per-stack delegated subtree, not the cgroup root or a system/user/global slice" ;;
  esac
  case "$value" in
    *"$marker"*) ;;
    *) die "$name must contain the fresh stack identity $marker" ;;
  esac
  case "$value" in
    *"${parent%.slice}"*) ;;
    *) die "$name must be beneath the delegated parent identity ${parent}: $value" ;;
  esac
  [ -d "$value" ] || die "$name must already exist as a delegated cgroup-v2 directory: $value"
  canonical=$(readlink -f -- "$value" 2>/dev/null || true)
  [ "$canonical" = "$value" ] || die "$name must be a canonical path without symlink indirection: $value"
  fs_type=$(stat -fc '%T' "$value" 2>/dev/null || true)
  [ "$fs_type" = cgroup2fs ] || die "$name is not on a cgroup-v2 filesystem: $value"
  [ -s "$value/cgroup.controllers" ] || die "$name has no delegated controllers: $value"
  [ -f "$value/cgroup.subtree_control" ] && [ -w "$value/cgroup.subtree_control" ] || die "$name subtree control is not writable: $value"
  [ -f "$value/cgroup.procs" ] && [ -w "$value/cgroup.procs" ] || die "$name process control is not writable: $value"
  owner=$(stat -c '%u' "$value" 2>/dev/null || true)
  current_uid=$(id -u)
  [ "$owner" = "$current_uid" ] || die "$name must be owned by the provisioning user ($current_uid), got $owner: $value"
}

validate_immutable_image() {
  local name=$1 value=$2
  printf '%s\n' "$value" | grep -Eq '@sha256:[0-9a-f]{64}$' || die "$name must end with @sha256:<64 lowercase hex>; use the *_IMAGE_LOCAL override for local builds"
}

require_fresh_docker_namespace() {
  command -v docker >/dev/null 2>&1 || die "docker is required for the immutable fresh-namespace inspection gate"
  local kind name status existing
  for kind in network volume; do
    for name in "$@"; do
      if docker "$kind" inspect "$name" >/dev/null 2>&1; then
        die "fresh namespace collision: Docker $kind already exists: $name"
      else
        status=$?
        [ "$status" -eq 1 ] || die "Docker $kind inspect failed for $name (status $status)"
      fi
    done
  done
  if existing=$(docker ps -aq --filter "label=com.docker.compose.project=$stack_name"); then
    [ -z "$existing" ] || die "fresh namespace collision: existing Compose containers use project $stack_name"
  else
    status=$?
    die "Docker container collision inspection failed (status $status)"
  fi
}

[ "$#" -ge 1 ] || usage
out_input=$1
openrouter_source=
[ "$#" -ge 2 ] && openrouter_source=$2
embedding_source=$openrouter_source
[ "$#" -ge 3 ] && embedding_source=$3

core_extension_enabled=$(parse_bool DIREXTALK_CORE_EXTENSION_ENABLED "${DIREXTALK_CORE_EXTENSION_ENABLED:-false}")
core_workload_enabled=$(parse_bool DIREXTALK_CORE_WORKLOAD_ENABLED "${DIREXTALK_CORE_WORKLOAD_ENABLED:-false}")
core_aws_enabled=$(parse_bool DIREXTALK_CORE_AWS_ENABLED "${DIREXTALK_CORE_AWS_ENABLED:-false}")
core_knowledge_vector_dimension=${DIREXTALK_CORE_KNOWLEDGE_VECTOR_DIMENSION:-1536}
validate_vector_dimension DIREXTALK_CORE_KNOWLEDGE_VECTOR_DIMENSION "$core_knowledge_vector_dimension"
core_aws_ssm_credential_reference=${DIREXTALK_CORE_AWS_SSM_CREDENTIAL_REFERENCE:-00000000-0000-4000-8000-000000000001}
core_aws_ssm_region=${DIREXTALK_CORE_AWS_SSM_REGION:-us-east-1}
core_aws_ssm_account_id=${DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID:-000000000000}
core_aws_ssm_instance_id=${DIREXTALK_CORE_AWS_SSM_INSTANCE_ID:-i-disabled}
core_aws_ssm_document_version=${DIREXTALK_CORE_AWS_SSM_DOCUMENT_VERSION:-1}
core_aws_ssm_systemd_service=${DIREXTALK_CORE_AWS_SSM_SYSTEMD_SERVICE:-dirextalk-agent.service}
core_aws_ssm_required_tag_key=${DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_KEY:-managed}
core_aws_ssm_required_tag_value=${DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_VALUE:-true}
message_http_bind=${DIREXTALK_MESSAGE_HTTP_BIND:-8008}
message_https_bind=${DIREXTALK_MESSAGE_HTTPS_BIND:-8448}
validate_host_port DIREXTALK_MESSAGE_HTTP_BIND "$message_http_bind"
validate_host_port DIREXTALK_MESSAGE_HTTPS_BIND "$message_https_bind"
[ "$message_http_bind" != "$message_https_bind" ] || die "DIREXTALK_MESSAGE_HTTP_BIND and DIREXTALK_MESSAGE_HTTPS_BIND must differ"
message_client_base_url=http://localhost:$message_http_bind
if [ -n "${DIREXTALK_MESSAGE_CLIENT_BASE_URL:-}" ]; then
  validate_safe_value DIREXTALK_MESSAGE_CLIENT_BASE_URL "$DIREXTALK_MESSAGE_CLIENT_BASE_URL" '^https?://[A-Za-z0-9._:-]+$'
  [ "$DIREXTALK_MESSAGE_CLIENT_BASE_URL" = "$message_client_base_url" ] || \
    die "DIREXTALK_MESSAGE_CLIENT_BASE_URL must be derived from DIREXTALK_MESSAGE_HTTP_BIND ($message_client_base_url)"
fi
if [ "$core_aws_enabled" = true ]; then
  validate_uuid DIREXTALK_CORE_AWS_SSM_CREDENTIAL_REFERENCE "$core_aws_ssm_credential_reference"
  validate_safe_value DIREXTALK_CORE_AWS_SSM_REGION "$core_aws_ssm_region" '^[a-z0-9-]{1,32}$'
  validate_safe_value DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID "$core_aws_ssm_account_id" '^[0-9]{12}$'
  validate_safe_value DIREXTALK_CORE_AWS_SSM_INSTANCE_ID "$core_aws_ssm_instance_id" '^i-[a-z0-9]+$'
  validate_safe_value DIREXTALK_CORE_AWS_SSM_DOCUMENT_VERSION "$core_aws_ssm_document_version" '^[0-9]+$'
  validate_safe_value DIREXTALK_CORE_AWS_SSM_SYSTEMD_SERVICE "$core_aws_ssm_systemd_service" '^[A-Za-z0-9_.-]+\.service$'
  validate_safe_value DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_KEY "$core_aws_ssm_required_tag_key" '^[A-Za-z0-9_.:/-]{1,128}$'
  validate_safe_value DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_VALUE "$core_aws_ssm_required_tag_value" '^[A-Za-z0-9_.:/=-]{1,256}$'
fi
extension_runner_socket=${DIREXTALK_CORE_EXTENSION_RUNNER_SOCKET:-/run/dirextalk-agent/extension-runner.sock}
workload_runner_socket=${DIREXTALK_CORE_WORKLOAD_RUNNER_SOCKET:-/run/dirextalk-core-runner/runner.sock}
extension_runner_dir=${extension_runner_socket%/*}
workload_runner_dir=${workload_runner_socket%/*}
extension_runner_uid=${DIREXTALK_CORE_EXTENSION_RUNNER_UID:-65531}
workload_runner_uid=${DIREXTALK_CORE_WORKLOAD_RUNNER_UID:-65530}
validate_socket DIREXTALK_CORE_EXTENSION_RUNNER_SOCKET "$extension_runner_socket"
validate_socket DIREXTALK_CORE_WORKLOAD_RUNNER_SOCKET "$workload_runner_socket"
validate_uid DIREXTALK_CORE_EXTENSION_RUNNER_UID "$extension_runner_uid"
validate_uid DIREXTALK_CORE_WORKLOAD_RUNNER_UID "$workload_runner_uid"

script_dir=$(cd "$(dirname "$0")" && pwd -P)
split_deploy_dir=$(cd "$script_dir/.." && pwd -P)
message_root=$(cd "$script_dir/../../.." && pwd -P)
agent_root=$(cd "$message_root/../dirextalk-agent" && pwd -P)

case "$out_input" in
  /*) out=$(readlink -m -- "$out_input") ;;
  *) out=$(readlink -m -- "$(pwd -P)/$out_input") ;;
esac
[ "$out" != "/" ] || die "refusing to use / as output directory"

if [ -e "$out" ]; then
  [ -d "$out" ] || die "output path exists and is not a directory: $out"
  [ -z "$(find "$out" -mindepth 1 -maxdepth 1 -print -quit)" ] || die "output directory is not empty; use a new fresh directory"
else
  mkdir -p "$out"
fi
chmod 700 "$out"

[ -x "$split_deploy_dir/scripts/message-server-entrypoint.sh" ] || die "message-server entrypoint helper is missing or not executable"
[ -x "$split_deploy_dir/scripts/initialize-capability-ca.sh" ] || die "Capability CA initializer is missing or not executable"

uuid4() {
  local value
  if command -v uuidgen >/dev/null 2>&1; then
    value=$(uuidgen | tr '[:upper:]' '[:lower:]')
  else
    value=$(cat /proc/sys/kernel/random/uuid)
  fi
  printf '%s\n' "$value"
}

write_secret() {
  local target=$1 value=$2
  umask 077
  printf '%s\n' "$value" >"$target"
  chmod 400 "$target"
}

write_raw_secret() {
  local target=$1 bytes=$2
  umask 077
  head -c "$bytes" /dev/urandom >"$target"
  chmod 400 "$target"
  [ "$(wc -c <"$target")" -eq "$bytes" ] || die "failed to generate exact-size protected key: $target"
}

copy_secret_or_empty() {
  local source=$1 target=$2 label=$3
  if [ -n "$source" ]; then
    [ -f "$source" ] && [ ! -L "$source" ] || die "$label source must be a regular non-symlink file"
    install -m 0400 "$source" "$target"
  else
    : >"$target"
    chmod 400 "$target"
    echo "warning: $label is an empty protected placeholder; replace it before model acceptance" >&2
  fi
}

agent_instance_id=$(uuid4)
message_instance_id=$(uuid4)
embedding_profile_id=$(uuid4)
generation_hex=$(od -An -N6 -tx1 /dev/urandom | tr -d '[:space:]')
account_generation=$((16#$generation_hex + 1))
agent_password=$(openssl rand -hex 24)
message_password=$(openssl rand -hex 24)
message_registration_shared_secret=$(openssl rand -hex 32)
message_portal_password=$(openssl rand -hex 24)

command -v base32 >/dev/null 2>&1 || die "base32 is required to create a high-entropy fresh stack namespace"
stack_nonce=$(head -c 16 /dev/urandom | base32 | tr '[:upper:]' '[:lower:]' | tr -d '=[:space:]')
[ "${#stack_nonce}" -eq 26 ] || die "failed to create the 128-bit fresh stack nonce"
stack_name=$(printenv DIREXTALK_SPLIT_STACK_NAME 2>/dev/null || true)
[ -n "$stack_name" ] || stack_name=d-$stack_nonce
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "DIREXTALK_SPLIT_STACK_NAME must be the generated d-<26-char-base32 nonce>"
[ "${#stack_name}" -le 29 ] || die "DIREXTALK_SPLIT_STACK_NAME is too long for the derived Docker resource names"
stack_nonce=${stack_name#d-}

extension_cgroup_root=${DIREXTALK_EXTENSION_CGROUP_ROOT:-/sys/fs/cgroup/$stack_name-extension}
core_runner_cgroup_root=${DIREXTALK_CORE_RUNNER_CGROUP_ROOT:-/sys/fs/cgroup/$stack_name-core-runner}
extension_cgroup_parent=${DIREXTALK_EXTENSION_CGROUP_PARENT:-$stack_name-extension.slice}
workload_cgroup_parent=${DIREXTALK_CORE_RUNNER_CGROUP_PARENT:-$stack_name-core-runner.slice}
validate_absolute_path DIREXTALK_EXTENSION_CGROUP_ROOT "$extension_cgroup_root"
validate_absolute_path DIREXTALK_CORE_RUNNER_CGROUP_ROOT "$core_runner_cgroup_root"
validate_cgroup_parent DIREXTALK_EXTENSION_CGROUP_PARENT "$extension_cgroup_parent"
validate_cgroup_parent DIREXTALK_CORE_RUNNER_CGROUP_PARENT "$workload_cgroup_parent"
if [ "$core_extension_enabled" = true ] && [ -z "${DIREXTALK_EXTENSION_CGROUP_ROOT:-}" ]; then
  die "DIREXTALK_EXTENSION_CGROUP_ROOT must point to a delegated cgroup-v2 subtree when extensions are enabled"
fi
if [ "$core_workload_enabled" = true ] && [ -z "${DIREXTALK_CORE_RUNNER_CGROUP_ROOT:-}" ]; then
  die "DIREXTALK_CORE_RUNNER_CGROUP_ROOT must point to a delegated cgroup-v2 subtree when Core Runner is enabled"
fi
if [ "$core_extension_enabled" = true ]; then
  validate_delegated_cgroup_root DIREXTALK_EXTENSION_CGROUP_ROOT "$extension_cgroup_root" "$stack_name" "$extension_cgroup_parent"
fi
if [ "$core_workload_enabled" = true ]; then
  validate_delegated_cgroup_root DIREXTALK_CORE_RUNNER_CGROUP_ROOT "$core_runner_cgroup_root" "$stack_name" "$workload_cgroup_parent"
fi

require_fresh_docker_namespace \
  "$stack_name-message-private" "$stack_name-message-public" "$stack_name-message-db" \
  "$stack_name-agent-private" "$stack_name-agent-db" \
  "$stack_name-agent-caller" "$stack_name-agent-egress" \
  "$stack_name-message-postgres" "$stack_name-message-config" \
  "$stack_name-message-data" "$stack_name-message-plugins" \
  "$stack_name-agent-postgres" "$stack_name-agent-secrets" \
  "$stack_name-agent-config" "$stack_name-agent-core-data" \
  "$stack_name-agent-extension-socket" "$stack_name-agent-extension-install" \
  "$stack_name-agent-extension-staging" "$stack_name-agent-extension-workspaces" \
  "$stack_name-agent-extension-runner-workspaces" "$stack_name-agent-extension-runner-state" \
  "$stack_name-agent-knowledge-content" "$stack_name-agent-knowledge-mount" \
  "$stack_name-agent-qdrant" "$stack_name-capability-authority" \
  "$stack_name-capability-shared" "$stack_name-capability-private" \
  "$stack_name-core-runner-socket" \
  "$stack_name-core-runner-installs" "$stack_name-core-runner-workspaces" \
  "$stack_name-core-runner-state"

postgres_image=$(printenv DIREXTALK_POSTGRES_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$postgres_image" ] || postgres_image=docker.io/library/postgres:18@sha256:3a82e1f56c8f0f5616a11103ac3d47e632c3938698946a7ad26da0df1334744a
utility_image=$(printenv DIREXTALK_UTILITY_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$utility_image" ] || utility_image=$postgres_image
message_image=$(printenv DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$message_image" ] || message_image=registry.invalid/dirextalk-message-server@sha256:0000000000000000000000000000000000000000000000000000000000000000
agent_image=$(printenv DIREXTALK_AGENT_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$agent_image" ] || agent_image=registry.invalid/dirextalk-agent@sha256:0000000000000000000000000000000000000000000000000000000000000000
extension_runner_image=$(printenv DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$extension_runner_image" ] || extension_runner_image=registry.invalid/dirextalk-extension-runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
core_runner_image=$(printenv DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$core_runner_image" ] || core_runner_image=registry.invalid/dirextalk-core-runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
qdrant_image=$(printenv DIREXTALK_QDRANT_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$qdrant_image" ] || qdrant_image=qdrant/qdrant:v1.18.3@sha256:0bd98fa7977f1e75694779359ca4e212822e5a71334e28421182f72f209d5286
for image_pair in \
  DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:$postgres_image \
  DIREXTALK_UTILITY_IMAGE_IMMUTABLE:$utility_image \
  DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE:$message_image \
  DIREXTALK_AGENT_IMAGE_IMMUTABLE:$agent_image \
  DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE:$extension_runner_image \
  DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE:$core_runner_image \
  DIREXTALK_QDRANT_IMAGE_IMMUTABLE:$qdrant_image; do
  image_name=${image_pair%%:*}
  image_value=${image_pair#*:}
  validate_immutable_image "$image_name" "$image_value"
done

write_secret "$out/agent-postgres-password" "$agent_password"
write_secret "$out/message-postgres-password" "$message_password"
write_secret "$out/agent-database-url" "postgresql://dirextalk_agent:$agent_password@agent-postgres:5432/dirextalk_agent?sslmode=disable"
write_secret "$out/message-database-url" "postgresql://dirextalk_message_server:$message_password@message-postgres:5432/dirextalk_message_server?sslmode=disable"
write_secret "$out/message-registration-shared-secret" "$message_registration_shared_secret"
write_secret "$out/message-portal-password" "$message_portal_password"
write_raw_secret "$out/core-secret-master-key" 32
core_secret_master_key_device=$(stat -c '%d' "$out/core-secret-master-key")
core_secret_master_key_inode=$(stat -c '%i' "$out/core-secret-master-key")
core_secret_master_key_uid=$(stat -c '%u' "$out/core-secret-master-key")

# Keep TLS source paths explicit and path-only. Local mode intentionally starts
# with empty protected placeholders; message-server-init generates the
# disposable certificate/key in its fresh config volume. Production replaces
# these files with a provisioned pair and runs verify-production-tls.sh before
# enabling external mode. Neither certificate nor key material is interpolated
# into Compose or written to the manifest.
message_tls_cert_file=$out/message-tls-external-cert.pem
message_tls_key_file=$out/message-tls-external-key.pem

copy_secret_or_empty "$openrouter_source" "$out/openrouter-api-key" "OpenRouter API key"
copy_secret_or_empty "$embedding_source" "$out/embedding-api-key" "embedding API key"
copy_secret_or_empty "" "$message_tls_cert_file" "external message-server TLS certificate"
copy_secret_or_empty "" "$message_tls_key_file" "external message-server TLS private key"

knowledge_collection=$(printf '%s' "$agent_instance_id" | tr -d '-')
cat >"$out/agent-config.yaml" <<EOF
instance_id: $agent_instance_id
database_url_file: /run/secrets/database_url
grpc_listen: ":9443"
tls_cert_file: /run/secrets/tls_cert
tls_key_file: /run/secrets/tls_key
service_token_file: /run/secrets/service_token
core_voice_callback_relay_token_file: /run/secrets/voice_relay_token
enable_health_service: true
enable_reflection: false
capability_enabled: true
capability_grpc_listen: ":50052"
capability_ca_cert_file: /run/secrets/capability_ca
capability_tls_cert_file: /run/secrets/tls_cert
capability_tls_key_file: /run/secrets/tls_key
capability_token_file: /run/secrets/ms_to_agent_token
capability_grant_public_key_file: /run/secrets/grant_public_key
capability_peer_common_name: message-server-client
capability_peer_instance_id: $message_instance_id
capability_account_generation: $account_generation
capability_max_concurrent_query: 32
capability_max_concurrent_watch: 64
product_capability_enabled: true
product_capability_address: message-server:50053
product_capability_ca_cert_file: /run/secrets/product_ca
product_capability_tls_cert_file: /run/secrets/product_tls_cert
product_capability_tls_key_file: /run/secrets/product_tls_key
product_capability_token_file: /run/secrets/agent_to_ms_token
product_capability_server_name: dirextalk-message-server
product_capability_instance_id: $agent_instance_id
product_capability_account_generation: $account_generation
core_task_max_concurrency: 4
core_task_lease_ttl: 30s
core_schedule_sweep_interval: 1s
core_shutdown_grace: 30s
core_extension_enabled: $core_extension_enabled
core_extension_staging_root: /var/lib/dirextalk-agent/extension-staging
core_extension_workspace_root: /var/lib/dirextalk-agent/extension-workspaces
core_extension_runner_socket: $extension_runner_socket
core_extension_runner_uid: $extension_runner_uid
core_workload_enabled: $core_workload_enabled
core_workload_runner_socket: $workload_runner_socket
core_workload_runner_uid: $workload_runner_uid
core_aws_enabled: $core_aws_enabled
core_secret_master_key_file: /run/secrets/core_secret_master_key
core_secret_master_key_version: 1
core_aws_ssm_readiness:
  credential_reference: $core_aws_ssm_credential_reference
  target:
    region: $core_aws_ssm_region
    account_id: "$core_aws_ssm_account_id"
    instance_id: $core_aws_ssm_instance_id
    identity:
      kind: AWS_EC2_SSM
      region: $core_aws_ssm_region
      account_id: "$core_aws_ssm_account_id"
      instance_id: $core_aws_ssm_instance_id
    ec2_document_version: "$core_aws_ssm_document_version"
    ec2_systemd_service: $core_aws_ssm_systemd_service
    required_instance_tags:
      $core_aws_ssm_required_tag_key: "$core_aws_ssm_required_tag_value"
core_knowledge_enabled: true
core_knowledge_content_root: /var/lib/dirextalk-agent/knowledge-content
core_knowledge_mount_root: /var/lib/dirextalk-agent/knowledge-mount
core_knowledge_content_quota_bytes: 1073741824
core_knowledge_embedding_profile_id: $embedding_profile_id
core_knowledge_qdrant_endpoint: http://qdrant:6333
core_knowledge_qdrant_collection: dirextalk_knowledge_$knowledge_collection
core_knowledge_qdrant_dimension: $core_knowledge_vector_dimension
core_knowledge_sweep_interval: 1s
EOF
chmod 400 "$out/agent-config.yaml"

cat >"$out/.env" <<EOF
DIREXTALK_SPLIT_STACK_NAME=$stack_name
DIREXTALK_MESSAGE_SERVER_ENTRYPOINT_FILE=$split_deploy_dir/scripts/message-server-entrypoint.sh
DIREXTALK_CAPABILITY_CA_INITIALIZER_FILE=$split_deploy_dir/scripts/initialize-capability-ca.sh
DIREXTALK_MESSAGE_SERVER_INITIALIZER_FILE=$split_deploy_dir/scripts/initialize-message-server.sh
DIREXTALK_AGENT_SECRET_MATERIALIZER_FILE=$split_deploy_dir/scripts/materialize-agent-secrets.sh
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$message_image
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image
DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=$postgres_image
DIREXTALK_UTILITY_IMAGE_IMMUTABLE=$utility_image
DIREXTALK_QDRANT_IMAGE_IMMUTABLE=$qdrant_image
DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE=$extension_runner_image
DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE=$core_runner_image
DIREXTALK_AGENT_BUILD_CONTEXT=$agent_root
DIREXTALK_MESSAGE_BUILD_CONTEXT=$message_root
DIREXTALK_AGENT_BUILD_VERSION=local
DIREXTALK_AGENT_BUILD_REVISION=working-tree
DIREXTALK_MESSAGE_BUILD_VERSION=local
DIREXTALK_MESSAGE_BUILD_REVISION=working-tree
DIREXTALK_MESSAGE_SERVER_IMAGE_LOCAL=dirextalk-message-server:split-local
DIREXTALK_AGENT_IMAGE_LOCAL=dirextalk-agent:split-local
DIREXTALK_EXTENSION_RUNNER_IMAGE_LOCAL=dirextalk-extension-runner:split-local
DIREXTALK_CORE_RUNNER_IMAGE_LOCAL=dirextalk-core-runner:split-local
DIREXTALK_MESSAGE_SERVER_INSTANCE_ID=$message_instance_id
DIREXTALK_AGENT_INSTANCE_ID=$agent_instance_id
DIREXTALK_ACCOUNT_GENERATION=$account_generation
DIREXTALK_AGENT_TLS_SERVER_NAME=dirextalk-agent
DIREXTALK_MESSAGE_SERVER_NAME=localhost
DIREXTALK_MESSAGE_CLIENT_BASE_URL=$message_client_base_url
DIREXTALK_MESSAGE_TLS_MODE=local
DIREXTALK_MESSAGE_TLS_CERT_FILE=$message_tls_cert_file
DIREXTALK_MESSAGE_TLS_KEY_FILE=$message_tls_key_file
DIREXTALK_MESSAGE_HTTP_BIND=$message_http_bind
DIREXTALK_MESSAGE_HTTPS_BIND=$message_https_bind
DIREXTALK_AGENT_CONFIG_FILE=$out/agent-config.yaml
DIREXTALK_MESSAGE_POSTGRES_PASSWORD_FILE=$out/message-postgres-password
DIREXTALK_AGENT_POSTGRES_PASSWORD_FILE=$out/agent-postgres-password
DIREXTALK_MESSAGE_DATABASE_URL_FILE=$out/message-database-url
DIREXTALK_MESSAGE_REGISTRATION_SHARED_SECRET_FILE=$out/message-registration-shared-secret
DIREXTALK_MESSAGE_PORTAL_PASSWORD_FILE=$out/message-portal-password
DIREXTALK_AGENT_DATABASE_URL_FILE=$out/agent-database-url
DIREXTALK_OPENROUTER_API_KEY_FILE=$out/openrouter-api-key
DIREXTALK_EMBEDDING_API_KEY_FILE=$out/embedding-api-key
DIREXTALK_CORE_SECRET_MASTER_KEY_FILE=$out/core-secret-master-key
DIREXTALK_MESSAGE_PRIVATE_NETWORK=$stack_name-message-private
DIREXTALK_MESSAGE_PUBLIC_NETWORK=$stack_name-message-public
DIREXTALK_MESSAGE_DATABASE_NETWORK=$stack_name-message-db
DIREXTALK_AGENT_PRIVATE_NETWORK=$stack_name-agent-private
DIREXTALK_AGENT_DATABASE_NETWORK=$stack_name-agent-db
DIREXTALK_AGENT_CALLER_NETWORK=$stack_name-agent-caller
DIREXTALK_AGENT_EGRESS_NETWORK=$stack_name-agent-egress
DIREXTALK_MESSAGE_POSTGRES_VOLUME=$stack_name-message-postgres
DIREXTALK_MESSAGE_CONFIG_VOLUME=$stack_name-message-config
DIREXTALK_MESSAGE_DATA_VOLUME=$stack_name-message-data
DIREXTALK_MESSAGE_PLUGINS_VOLUME=$stack_name-message-plugins
DIREXTALK_AGENT_POSTGRES_VOLUME=$stack_name-agent-postgres
DIREXTALK_AGENT_SECRET_VOLUME=$stack_name-agent-secrets
DIREXTALK_AGENT_CONFIG_VOLUME=$stack_name-agent-config
DIREXTALK_AGENT_CORE_DATA_VOLUME=$stack_name-agent-core-data
DIREXTALK_AGENT_SOCKET_VOLUME=$stack_name-agent-extension-socket
DIREXTALK_AGENT_INSTALL_VOLUME=$stack_name-agent-extension-install
DIREXTALK_AGENT_STAGING_VOLUME=$stack_name-agent-extension-staging
DIREXTALK_AGENT_WORKSPACE_VOLUME=$stack_name-agent-extension-workspaces
DIREXTALK_AGENT_RUNNER_WORKSPACE_VOLUME=$stack_name-agent-extension-runner-workspaces
DIREXTALK_AGENT_RUNNER_STATE_VOLUME=$stack_name-agent-extension-runner-state
DIREXTALK_AGENT_KNOWLEDGE_CONTENT_VOLUME=$stack_name-agent-knowledge-content
DIREXTALK_AGENT_KNOWLEDGE_MOUNT_VOLUME=$stack_name-agent-knowledge-mount
DIREXTALK_AGENT_QDRANT_VOLUME=$stack_name-agent-qdrant
DIREXTALK_CAPABILITY_AUTHORITY_VOLUME=$stack_name-capability-authority
DIREXTALK_CAPABILITY_SHARED_VOLUME=$stack_name-capability-shared
DIREXTALK_CAPABILITY_PRIVATE_VOLUME=$stack_name-capability-private
DIREXTALK_CORE_RUNNER_SOCKET_VOLUME=$stack_name-core-runner-socket
DIREXTALK_CORE_RUNNER_INSTALL_VOLUME=$stack_name-core-runner-installs
DIREXTALK_CORE_RUNNER_WORKSPACE_VOLUME=$stack_name-core-runner-workspaces
DIREXTALK_CORE_RUNNER_STATE_VOLUME=$stack_name-core-runner-state
DIREXTALK_EXTENSION_CGROUP_ROOT=$extension_cgroup_root
DIREXTALK_CORE_RUNNER_CGROUP_ROOT=$core_runner_cgroup_root
DIREXTALK_EXTENSION_CGROUP_PARENT=$extension_cgroup_parent
DIREXTALK_CORE_RUNNER_CGROUP_PARENT=$workload_cgroup_parent
DIREXTALK_CORE_EXTENSION_ENABLED=$core_extension_enabled
DIREXTALK_CORE_WORKLOAD_ENABLED=$core_workload_enabled
DIREXTALK_CORE_AWS_ENABLED=$core_aws_enabled
DIREXTALK_CORE_AWS_SSM_CREDENTIAL_REFERENCE=$core_aws_ssm_credential_reference
DIREXTALK_CORE_AWS_SSM_REGION=$core_aws_ssm_region
DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID=$core_aws_ssm_account_id
DIREXTALK_CORE_AWS_SSM_INSTANCE_ID=$core_aws_ssm_instance_id
DIREXTALK_CORE_AWS_SSM_DOCUMENT_VERSION=$core_aws_ssm_document_version
DIREXTALK_CORE_AWS_SSM_SYSTEMD_SERVICE=$core_aws_ssm_systemd_service
DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_KEY=$core_aws_ssm_required_tag_key
DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_VALUE=$core_aws_ssm_required_tag_value
DIREXTALK_CORE_EXTENSION_RUNNER_SOCKET=$extension_runner_socket
DIREXTALK_CORE_WORKLOAD_RUNNER_SOCKET=$workload_runner_socket
DIREXTALK_CORE_EXTENSION_RUNNER_DIR=$extension_runner_dir
DIREXTALK_CORE_WORKLOAD_RUNNER_DIR=$workload_runner_dir
DIREXTALK_CORE_EXTENSION_RUNNER_UID=$extension_runner_uid
DIREXTALK_CORE_WORKLOAD_RUNNER_UID=$workload_runner_uid
DIREXTALK_LOCAL_BOOTSTRAP_ENABLED=false
EOF
chmod 400 "$out/.env"

cat >"$out/.manifest" <<EOF
# dirextalk-split-manifest-v1
stack_name=$stack_name
stack_nonce=$stack_nonce
agent_instance_id=$agent_instance_id
message_instance_id=$message_instance_id
account_generation=$account_generation
core_secret_master_key_path=$out/core-secret-master-key
core_secret_master_key_device=$core_secret_master_key_device
core_secret_master_key_inode=$core_secret_master_key_inode
core_secret_master_key_uid=$core_secret_master_key_uid
message_http_bind=$message_http_bind
message_https_bind=$message_https_bind
message_client_base_url=$message_client_base_url
resource.network.message_private=$stack_name-message-private
resource.network.message_public=$stack_name-message-public
resource.network.message_database=$stack_name-message-db
resource.network.agent_private=$stack_name-agent-private
resource.network.agent_database=$stack_name-agent-db
resource.network.agent_caller=$stack_name-agent-caller
resource.network.agent_egress=$stack_name-agent-egress
resource.volume.message_postgres=$stack_name-message-postgres
resource.volume.message_config=$stack_name-message-config
resource.volume.message_data=$stack_name-message-data
resource.volume.message_plugins=$stack_name-message-plugins
resource.volume.agent_postgres=$stack_name-agent-postgres
resource.volume.agent_secrets=$stack_name-agent-secrets
resource.volume.agent_config=$stack_name-agent-config
resource.volume.agent_core_data=$stack_name-agent-core-data
resource.volume.agent_extension_socket=$stack_name-agent-extension-socket
resource.volume.agent_extension_install=$stack_name-agent-extension-install
resource.volume.agent_extension_staging=$stack_name-agent-extension-staging
resource.volume.agent_extension_workspaces=$stack_name-agent-extension-workspaces
resource.volume.agent_runner_workspaces=$stack_name-agent-extension-runner-workspaces
resource.volume.agent_runner_state=$stack_name-agent-extension-runner-state
resource.volume.agent_knowledge_content=$stack_name-agent-knowledge-content
resource.volume.agent_knowledge_mount=$stack_name-agent-knowledge-mount
resource.volume.agent_qdrant=$stack_name-agent-qdrant
resource.volume.capability_authority=$stack_name-capability-authority
resource.volume.capability_shared=$stack_name-capability-shared
resource.volume.capability_private=$stack_name-capability-private
resource.volume.core_runner_socket=$stack_name-core-runner-socket
resource.volume.core_runner_installs=$stack_name-core-runner-installs
resource.volume.core_runner_workspaces=$stack_name-core-runner-workspaces
resource.volume.core_runner_state=$stack_name-core-runner-state
EOF
chmod 400 "$out/.manifest"

printf 'Provisioned fresh split-stack namespace: %s\n' "$stack_name"
printf 'Non-secret Compose environment: %s/.env\n' "$out"
printf 'Before model acceptance, replace the protected OpenRouter/embedding key files if placeholders were created.\n'
