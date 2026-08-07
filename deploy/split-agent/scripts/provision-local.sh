#!/usr/bin/env bash
set -euo pipefail

# Provision one disposable split-stack namespace. Reuse of non-empty output
# directories is forbidden because Core v1 accepts fresh data only.
#
# Usage:
#   provision-local.sh OUTPUT_DIR [OPENROUTER_KEY_FILE] [EMBEDDING_KEY_FILE] [TAVILY_KEY_FILE] [PORTAL_PASSWORD_FILE]
#
# Secret source files are copied with mode 0400. Values never enter .env,
# Compose YAML, or command output. Missing sources become protected empty
# placeholders so topology/image checks can still run; model acceptance must
# replace them first. A supplied portal-password source is stricter: it must be
# a current-UID-owned regular non-symlink mode-0400 file containing one
# 8-digit line.

usage() {
  echo "usage: $0 OUTPUT_DIR [OPENROUTER_KEY_FILE] [EMBEDDING_KEY_FILE] [TAVILY_KEY_FILE] [PORTAL_PASSWORD_FILE]" >&2
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

validate_server_name() {
  local name=$1 value=$2
  printf '%s\n' "$value" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$' || \
    die "$name must be a DNS host name without a scheme, port, or wildcard"
}

validate_ipv4() {
  local name=$1 value=$2 octet
  local IFS=.
  read -r -a octets <<<"$value"
  [ "${#octets[@]}" -eq 4 ] || die "$name must be an IPv4 address"
  for octet in "${octets[@]}"; do
    case "$octet" in
      ''|*[!0-9]*|0[0-9]*) die "$name must be a canonical IPv4 address" ;;
    esac
    [ "$octet" -le 255 ] 2>/dev/null || die "$name contains an IPv4 octet above 255"
  done
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

validate_fixed_runner_uid() {
  local name=$1 expected=$2 value
  if [ -n "${!name+x}" ]; then
    value=${!name}
    [ "$value" = "$expected" ] || die "$name is fixed at $expected for the bundled runner image; custom runner UIDs are unsupported"
  fi
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

validate_target_write_access() {
  local name=$1 path=$2 expected_uid=$3 expected_gid=$4 metadata status owner_uid owner_gid mode permissions acl
  if metadata=$(stat -c '%u %g %a' -- "$path" 2>/dev/null); then
    :
  else
    status=$?
    die "$name metadata check failed (status $status): $path"
  fi
  read -r owner_uid owner_gid mode <<<"$metadata"
  [ -n "$owner_uid" ] && [ -n "$owner_gid" ] && [ -n "$mode" ] || die "$name metadata is incomplete: $path"
  permissions=$((8#$mode))
  if [ "$owner_uid" = "$expected_uid" ] && (( permissions & 0200 )); then
    (( permissions & 0002 )) && die "$name must not be world-writable: $path"
    return 0
  fi
  if [ "$owner_gid" = "$expected_gid" ] && (( permissions & 0020 )); then
    (( permissions & 0002 )) && die "$name must not be world-writable: $path"
    return 0
  fi
  (( permissions & 0002 )) && die "$name must not be world-writable: $path"
  if command -v getfacl >/dev/null 2>&1; then
    if acl=$(getfacl -cp -- "$path" 2>/dev/null); then
      if printf '%s\n' "$acl" | awk -F: -v uid="$expected_uid" -v gid="$expected_gid" '
        $1 == "mask" && $2 == "" { mask = $3 }
        $1 == "user" && $2 == uid && $3 ~ /w/ { user_write = 1 }
        $1 == "group" && $2 == gid && $3 ~ /w/ { group_write = 1 }
        END { if ((user_write || group_write) && mask ~ /w/) exit 0; exit 1 }
      '; then
        return 0
      fi
    fi
  fi
  die "$name is not writable by runner UID/GID $expected_uid:$expected_gid: $path"
}

validate_delegated_cgroup_root() {
  local name=$1 value=$2 marker=$3 parent=$4 expected_owner=$5 fs_type owner canonical controllers subtree required
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
  [ -f "$value/cgroup.controllers" ] && [ -r "$value/cgroup.controllers" ] || \
    die "$name controllers file is missing or unreadable: $value"
  controllers=$(tr '\n' ' ' <"$value/cgroup.controllers" 2>/dev/null || true)
  [ -n "$controllers" ] || die "$name has no delegated controllers: $value"
  for required in cpu memory pids; do
    printf ' %s ' "$controllers" | grep -Fq " $required " || \
      die "$name does not expose controller $required: $value"
  done
  [ -f "$value/cgroup.subtree_control" ] || die "$name subtree control is missing: $value"
  [ -f "$value/cgroup.procs" ] || die "$name process control is missing: $value"
  subtree=$(tr '\n' ' ' <"$value/cgroup.subtree_control" 2>/dev/null || true)
  for required in cpu memory pids; do
    printf ' %s ' "$subtree" | grep -Fq " $required " || \
      die "$name has not enabled controller $required in subtree_control: $value"
  done
  owner=$(stat -c '%u:%g' "$value" 2>/dev/null || true)
  [ "$owner" = "$expected_owner:$expected_owner" ] || die "$name must be owned by runner UID/GID $expected_owner, got $owner: $value"
  validate_target_write_access "$name delegated root" "$value" "$expected_owner" "$expected_owner"
  validate_target_write_access "$name subtree control" "$value/cgroup.subtree_control" "$expected_owner" "$expected_owner"
  validate_target_write_access "$name process control" "$value/cgroup.procs" "$expected_owner" "$expected_owner"
}

validate_runner_control_group() {
  local name=$1 value=$2 stack=$3 parent=$4 unit=$5
  printf '%s\n' "$value" | grep -Eq '^/[^/[:space:]][^[:space:]]*$' || die "$name must be an absolute systemd ControlGroup path"
  case "$value" in
    *'//'|*'/../'*|*'/./'*) die "$name must be canonical without path traversal" ;;
    *"/$parent/$unit"|*"/$parent/$unit/"*) ;;
    *) die "$name must contain exact stack parent/unit ($stack/$parent/$unit)" ;;
  esac
}

validate_runner_fragment() {
  local name=$1 value=$2 expected=$3 hash_name=$4 expected_hash=$5 actual_hash
  [ "$value" = "$expected" ] || die "$name must be the repository-owned template path: $value"
  printf '%s\n' "$expected_hash" | grep -Eq '^[0-9a-f]{64}$' || die "$hash_name must be a lowercase SHA-256"
  actual_hash=$(sha256sum -- "$expected" | awk '{print $1}')
  [ "$actual_hash" = "$expected_hash" ] || die "$hash_name does not match installed repository template"
}

validate_root_owned_asset() {
  local name=$1 path=$2 current parent mode permissions
  [ -f "$path" ] && [ ! -L "$path" ] || die "$name must be a root-owned immutable regular file"
  [ "$(stat -c '%u:%g' -- "$path")" = 0:0 ] || die "$name must be root-owned"
  mode=$(stat -c '%a' -- "$path")
  permissions=$((8#$mode))
  (( (permissions & 18) == 0 )) || die "$name must not be group/world writable"
  current=${path%/*}
  while :; do
    [ -d "$current" ] && [ ! -L "$current" ] || die "$name parent must be a regular directory: $current"
    [ "$(stat -c '%u:%g' -- "$current")" = 0:0 ] || die "$name parent must be root-owned: $current"
    mode=$(stat -c '%a' -- "$current")
    permissions=$((8#$mode))
    (( (permissions & 18) == 0 )) || die "$name parent must not be group/world writable: $current"
    [ "$current" = / ] && break
    parent=${current%/*}
    [ -n "$parent" ] || parent=/
    current=$parent
  done
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
[ "$#" -le 5 ] || usage
out_input=$1
openrouter_source=
[ "$#" -ge 2 ] && openrouter_source=$2
embedding_source=$openrouter_source
[ "$#" -ge 3 ] && embedding_source=$3
tavily_source=
[ "$#" -ge 4 ] && tavily_source=$4
portal_password_source=
[ "$#" -ge 5 ] && portal_password_source=$5

core_extension_enabled=$(parse_bool DIREXTALK_CORE_EXTENSION_ENABLED "${DIREXTALK_CORE_EXTENSION_ENABLED:-false}")
core_workload_enabled=$(parse_bool DIREXTALK_CORE_WORKLOAD_ENABLED "${DIREXTALK_CORE_WORKLOAD_ENABLED:-false}")
runner_fixture_mode=$(parse_bool DIREXTALK_SPLIT_FIXTURE_MODE "${DIREXTALK_SPLIT_FIXTURE_MODE:-false}")
compose_mode=${DIREXTALK_SPLIT_COMPOSE_MODE:-local}
case "$compose_mode" in
  local|production) ;;
  *) die "DIREXTALK_SPLIT_COMPOSE_MODE must be local or production" ;;
esac
if [ "$compose_mode" = production ]; then
  [ "$core_extension_enabled" = true ] || die "production requires DIREXTALK_CORE_EXTENSION_ENABLED=true"
  [ "$core_workload_enabled" = true ] || die "production requires DIREXTALK_CORE_WORKLOAD_ENABLED=true"
fi
release_catalog_origin=https://imadmin.dirextalk.ai
if [ "$runner_fixture_mode" = true ] && [ "${DIREXTALK_SPLIT_TEST_MODE:-false}" != true ]; then
  die "DIREXTALK_SPLIT_FIXTURE_MODE requires explicit DIREXTALK_SPLIT_TEST_MODE=true"
fi
core_aws_enabled=$(parse_bool DIREXTALK_CORE_AWS_ENABLED "${DIREXTALK_CORE_AWS_ENABLED:-false}")
core_aws_ssm_enabled=$(parse_bool DIREXTALK_CORE_AWS_SSM_ENABLED "${DIREXTALK_CORE_AWS_SSM_ENABLED:-false}")
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
message_tls_mode=${DIREXTALK_MESSAGE_TLS_MODE:-local}
case "$message_tls_mode" in
  local|external|edge-terminated) ;;
  *) die "DIREXTALK_MESSAGE_TLS_MODE must be local, external, or edge-terminated" ;;
esac
if [ "$compose_mode" = production ] && [ "$message_tls_mode" = local ]; then
  die "production requires DIREXTALK_MESSAGE_TLS_MODE=external or edge-terminated"
fi
if [ "$compose_mode" != production ] && [ "$message_tls_mode" = edge-terminated ]; then
  die "DIREXTALK_MESSAGE_TLS_MODE=edge-terminated is valid only for production"
fi
if [ "$message_tls_mode" = external ] || [ "$message_tls_mode" = edge-terminated ]; then
  message_server_name=${DIREXTALK_MESSAGE_SERVER_NAME:-}
  [ -n "$message_server_name" ] || die "DIREXTALK_MESSAGE_SERVER_NAME is required for public TLS"
  validate_server_name DIREXTALK_MESSAGE_SERVER_NAME "$message_server_name"
  message_client_base_url=https://$message_server_name
fi
if [ "$message_tls_mode" = external ]; then
  message_tls_cert_source=${DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE:-}
  message_tls_key_source=${DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE:-}
  [ -n "$message_tls_cert_source" ] || die "DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE is required for external TLS"
  [ -n "$message_tls_key_source" ] || die "DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE is required for external TLS"
  validate_absolute_path DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE "$message_tls_cert_source"
  validate_absolute_path DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE "$message_tls_key_source"
elif [ "$message_tls_mode" = edge-terminated ]; then
  message_tls_cert_source=
  message_tls_key_source=
  [ -z "${DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE:-}" ] || \
    die "DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE is forbidden with edge-terminated TLS"
  [ -z "${DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE:-}" ] || \
    die "DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE is forbidden with edge-terminated TLS"
else
  message_server_name=localhost
  message_tls_cert_source=
  message_tls_key_source=
  [ -z "${DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE:-}" ] || \
    die "DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE is only valid with external TLS"
  [ -z "${DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE:-}" ] || \
    die "DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE is only valid with external TLS"
  if [ -n "${DIREXTALK_MESSAGE_SERVER_NAME:-}" ] && [ "$DIREXTALK_MESSAGE_SERVER_NAME" != localhost ]; then
    die "DIREXTALK_MESSAGE_SERVER_NAME must be localhost for local TLS"
  fi
  message_client_base_url=http://localhost:$message_http_bind
fi
if [ -n "${DIREXTALK_MESSAGE_CLIENT_BASE_URL:-}" ]; then
  validate_safe_value DIREXTALK_MESSAGE_CLIENT_BASE_URL "$DIREXTALK_MESSAGE_CLIENT_BASE_URL" '^https?://[A-Za-z0-9._:-]+$'
  [ "$DIREXTALK_MESSAGE_CLIENT_BASE_URL" = "$message_client_base_url" ] || \
    die "DIREXTALK_MESSAGE_CLIENT_BASE_URL must be derived from DIREXTALK_MESSAGE_HTTP_BIND ($message_client_base_url)"
fi
turn_external_ip=${DIREXTALK_TURN_EXTERNAL_IP:-}
if [ "$compose_mode" = production ]; then
  [ -n "$turn_external_ip" ] || die "DIREXTALK_TURN_EXTERNAL_IP is required for production TURN relay"
else
  [ -n "$turn_external_ip" ] || turn_external_ip=127.0.0.1
fi
validate_ipv4 DIREXTALK_TURN_EXTERNAL_IP "$turn_external_ip"

if [ "$core_aws_ssm_enabled" = true ]; then
  [ "$core_aws_enabled" = true ] || die "DIREXTALK_CORE_AWS_SSM_ENABLED requires DIREXTALK_CORE_AWS_ENABLED=true"
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
extension_runner_uid=65531
workload_runner_uid=65530
validate_fixed_runner_uid DIREXTALK_CORE_EXTENSION_RUNNER_UID "$extension_runner_uid"
validate_fixed_runner_uid DIREXTALK_CORE_WORKLOAD_RUNNER_UID "$workload_runner_uid"
validate_socket DIREXTALK_CORE_EXTENSION_RUNNER_SOCKET "$extension_runner_socket"
validate_socket DIREXTALK_CORE_WORKLOAD_RUNNER_SOCKET "$workload_runner_socket"

script_dir=$(cd "$(dirname "$0")" && pwd -P)
split_deploy_dir=$(cd "$script_dir/.." && pwd -P)
if [ "$compose_mode" = local ]; then
  message_root=$(cd "$script_dir/../../.." && pwd -P)
  agent_root=$(cd "$message_root/../dirextalk-agent" && pwd -P)
  agent_build_version=local
  agent_build_revision=working-tree
  message_build_version=local
  message_build_revision=working-tree
else
  # Production consumes only this root-owned deployment bundle plus immutable
  # public images.  It must not require either Git checkout on the target host.
  message_root=$split_deploy_dir
  agent_root=$split_deploy_dir
  agent_build_version=production-attested
  agent_build_revision=production-attested
  message_build_version=production-attested
  message_build_revision=production-attested
fi

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

generate_portal_password() {
  local random32
  # Reject the short tail of the 32-bit range so decimal conversion does not
  # introduce modulo bias while retaining the backend's 8-digit contract.
  while :; do
    if random32=$(od -An -N4 -tu4 /dev/urandom | tr -d '[:space:]'); then
      :
    else
      die "failed to read cryptographically secure randomness for portal password"
    fi
    case "$random32" in
      ''|*[!0-9]*) die "cryptographically secure portal password source was malformed" ;;
    esac
    if [ "$random32" -lt 4200000000 ] 2>/dev/null; then
      printf -v portal_password_value '%08d' "$((random32 % 100000000))"
      return
    fi
  done
}

read_portal_password_source() {
  local source=$1 portal_fd status
  local before_type before_device before_inode before_uid before_mode before_metadata
  local fd_type fd_device fd_inode fd_uid fd_mode fd_metadata
  local after_type after_device after_inode after_uid after_mode after_metadata
  local portal_password_hex portal_password_value_read byte offset
  local current_uid

  [ -f "$source" ] || die "portal password source must be a regular non-symlink file"
  [ ! -L "$source" ] || die "portal password source must be a regular non-symlink file"
  current_uid=$(id -u) || die "cannot determine current UID for portal password source"
  if before_metadata=$(stat -c '%d %i %u %a' -- "$source" 2>/dev/null); then
    :
  else
    status=$?
    die "portal password source metadata check failed (status $status)"
  fi
  read -r before_device before_inode before_uid before_mode <<<"$before_metadata"
  [ -n "$before_device" ] && [ -n "$before_inode" ] && [ -n "$before_uid" ] && [ -n "$before_mode" ] || \
    die "portal password source metadata is incomplete"
  if before_type=$(stat -c '%F' -- "$source" 2>/dev/null); then
    :
  else
    status=$?
    die "portal password source type check failed (status $status)"
  fi
  [ "$before_type" = "regular file" ] || die "portal password source must be a regular non-symlink file"
  [ "$before_uid" = "$current_uid" ] || die "portal password source must be owned by the provisioning user"
  [ "$before_mode" = 400 ] || die "portal password source must have mode 0400"

  if exec {portal_fd}<"$source"; then
    :
  else
    status=$?
    die "portal password source could not be opened (status $status)"
  fi
  if fd_metadata=$(stat -Lc '%d %i %u %a' -- "/proc/self/fd/$portal_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {portal_fd}<&-
    die "portal password source opened-FD metadata check failed (status $status)"
  fi
  read -r fd_device fd_inode fd_uid fd_mode <<<"$fd_metadata"
  if fd_type=$(stat -Lc '%F' -- "/proc/self/fd/$portal_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {portal_fd}<&-
    die "portal password source opened-FD type check failed (status $status)"
  fi
  if [ "$fd_type" != "$before_type" ] || [ "$fd_device" != "$before_device" ] || [ "$fd_inode" != "$before_inode" ] || \
    [ "$fd_uid" != "$before_uid" ] || [ "$fd_mode" != "$before_mode" ]; then
    exec {portal_fd}<&-
    die "portal password source changed before the protected read"
  fi

  if portal_password_hex=$(od -An -v -tx1 <&"$portal_fd" | tr -d '[:space:]'); then
    :
  else
    status=$?
    exec {portal_fd}<&-
    die "portal password source could not be read (status $status)"
  fi
  case "${#portal_password_hex}" in
    16) ;;
    18) [ "${portal_password_hex:16:2}" = 0a ] || {
      exec {portal_fd}<&-
      die "portal password source must contain exactly one 8-digit line"
    } ;;
    *)
      exec {portal_fd}<&-
      die "portal password source must contain exactly one 8-digit line"
      ;;
  esac
  portal_password_value_read=
  for offset in 0 2 4 6 8 10 12 14; do
    byte=${portal_password_hex:offset:2}
    case "$byte" in
      3[0-9]) portal_password_value_read="${portal_password_value_read}${byte:1:1}" ;;
      *)
        exec {portal_fd}<&-
        die "portal password source must contain exactly one 8-digit line"
        ;;
    esac
  done

  [ -f "$source" ] || {
    exec {portal_fd}<&-
    die "portal password source was replaced after the protected read"
  }
  [ ! -L "$source" ] || {
    exec {portal_fd}<&-
    die "portal password source was replaced after the protected read"
  }
  if after_metadata=$(stat -c '%d %i %u %a' -- "$source" 2>/dev/null); then
    :
  else
    status=$?
    exec {portal_fd}<&-
    die "portal password source post-read metadata check failed (status $status)"
  fi
  read -r after_device after_inode after_uid after_mode <<<"$after_metadata"
  if after_type=$(stat -c '%F' -- "$source" 2>/dev/null); then
    :
  else
    status=$?
    exec {portal_fd}<&-
    die "portal password source post-read type check failed (status $status)"
  fi
  if [ "$after_type" != "$before_type" ] || [ "$after_device" != "$before_device" ] || [ "$after_inode" != "$before_inode" ] || \
    [ "$after_uid" != "$before_uid" ] || [ "$after_mode" != "$before_mode" ]; then
    exec {portal_fd}<&-
    die "portal password source was replaced after the protected read"
  fi
  exec {portal_fd}<&-
  portal_password_value=$portal_password_value_read
}

copy_secret_or_empty() {
  local source=$1 target=$2 label=$3 warn_empty=${4:-true}
  local source_fd path_fd status current_uid
  local fd_metadata fd_device fd_inode fd_uid fd_mode fd_type
  local path_metadata path_device path_inode path_uid path_mode path_type

  if [ -z "$source" ]; then
    umask 077
    : >"$target"
    chmod 400 "$target"
    [ "$warn_empty" != true ] || echo "warning: $label is an empty protected placeholder; replace it before model acceptance" >&2
    return
  fi

  # The shell performs the path opens so source values never become argv for
  # helper processes. Validate the opened descriptor and then compare a fresh
  # descriptor for the named path before and after the protected copy.
  [ -f "$source" ] || die "$label source must be a regular non-symlink file"
  [ ! -L "$source" ] || die "$label source must be a regular non-symlink file"
  current_uid=$(id -u) || die "cannot determine current UID for $label source"
  if exec {source_fd}<"$source"; then
    :
  else
    status=$?
    die "$label source could not be opened (status $status)"
  fi
  if fd_metadata=$(stat -Lc '%d %i %u %a' -- "/proc/self/fd/$source_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {source_fd}<&-
    die "$label source opened-FD metadata check failed (status $status)"
  fi
  read -r fd_device fd_inode fd_uid fd_mode <<<"$fd_metadata"
  if fd_type=$(stat -Lc '%F' -- "/proc/self/fd/$source_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {source_fd}<&-
    die "$label source opened-FD type check failed (status $status)"
  fi
  [ "$fd_type" = "regular file" ] || {
    exec {source_fd}<&-
    die "$label source must be a regular non-symlink file"
  }
  [ "$fd_uid" = "$current_uid" ] || {
    exec {source_fd}<&-
    die "$label source must be owned by the provisioning user"
  }
  [ "$fd_mode" = 400 ] || {
    exec {source_fd}<&-
    die "$label source must have mode 0400"
  }

  [ -f "$source" ] && [ ! -L "$source" ] || {
    exec {source_fd}<&-
    die "$label source changed before the protected read"
  }
  if exec {path_fd}<"$source"; then
    :
  else
    status=$?
    exec {source_fd}<&-
    die "$label source could not be reopened (status $status)"
  fi
  if path_metadata=$(stat -Lc '%d %i %u %a' -- "/proc/self/fd/$path_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {path_fd}<&-
    exec {source_fd}<&-
    die "$label source reopened-FD metadata check failed (status $status)"
  fi
  read -r path_device path_inode path_uid path_mode <<<"$path_metadata"
  if path_type=$(stat -Lc '%F' -- "/proc/self/fd/$path_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {path_fd}<&-
    exec {source_fd}<&-
    die "$label source reopened-FD type check failed (status $status)"
  fi
  exec {path_fd}<&-
  if [ "$path_type" != "$fd_type" ] || [ "$path_device" != "$fd_device" ] || [ "$path_inode" != "$fd_inode" ] || \
    [ "$path_uid" != "$fd_uid" ] || [ "$path_mode" != "$fd_mode" ]; then
    exec {source_fd}<&-
    die "$label source changed before the protected read"
  fi

  umask 077
  if cat <&"$source_fd" >"$target"; then
    :
  else
    status=$?
    exec {source_fd}<&-
    die "$label source could not be copied (status $status)"
  fi
  chmod 400 "$target"

  [ -f "$source" ] && [ ! -L "$source" ] || {
    exec {source_fd}<&-
    die "$label source was replaced after the protected read"
  }
  if exec {path_fd}<"$source"; then
    :
  else
    status=$?
    exec {source_fd}<&-
    die "$label source could not be reopened after the protected read (status $status)"
  fi
  if path_metadata=$(stat -Lc '%d %i %u %a' -- "/proc/self/fd/$path_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {path_fd}<&-
    exec {source_fd}<&-
    die "$label source post-read metadata check failed (status $status)"
  fi
  read -r path_device path_inode path_uid path_mode <<<"$path_metadata"
  if path_type=$(stat -Lc '%F' -- "/proc/self/fd/$path_fd" 2>/dev/null); then
    :
  else
    status=$?
    exec {path_fd}<&-
    exec {source_fd}<&-
    die "$label source post-read type check failed (status $status)"
  fi
  exec {path_fd}<&-
  exec {source_fd}<&-
  if [ "$path_type" != "$fd_type" ] || [ "$path_device" != "$fd_device" ] || [ "$path_inode" != "$fd_inode" ] || \
    [ "$path_uid" != "$fd_uid" ] || [ "$path_mode" != "$fd_mode" ]; then
    die "$label source was replaced after the protected read"
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
turn_shared_secret=$(openssl rand -hex 32)
if [ -n "$portal_password_source" ]; then
  read_portal_password_source "$portal_password_source"
else
  generate_portal_password
fi
message_portal_password=$portal_password_value

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
extension_runner_unit=${DIREXTALK_EXTENSION_RUNNER_UNIT:-dirextalk-extension-runner@${stack_name}.service}
core_runner_unit=${DIREXTALK_CORE_RUNNER_UNIT:-dirextalk-core-runner@${stack_name}.service}
extension_fragment_path=${DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH:-/etc/systemd/system/dirextalk-extension-runner@.service}
core_fragment_path=${DIREXTALK_CORE_RUNNER_FRAGMENT_PATH:-/etc/systemd/system/dirextalk-core-runner@.service}
extension_fragment_sha256=${DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256:-$(sha256sum -- "$split_deploy_dir/systemd/dirextalk-extension-runner@.service" | awk '{print $1}')}
core_fragment_sha256=${DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256:-$(sha256sum -- "$split_deploy_dir/systemd/dirextalk-core-runner@.service" | awk '{print $1}')}
runner_apparmor_profile=${DIREXTALK_RUNNER_APPARMOR_PROFILE:-dirextalk-runner-userns}
runner_apparmor_profile_path=${DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH:-/etc/apparmor.d/dirextalk-runner-userns}
runner_apparmor_profile_sha256=${DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256:-$(sha256sum -- "$split_deploy_dir/apparmor.d/dirextalk-runner-userns" | awk '{print $1}')}
runner_apparmor_manager_path=${DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH:-$script_dir/manage-runner-apparmor.sh}
runner_apparmor_manager_sha256=${DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256:-$(sha256sum -- "$runner_apparmor_manager_path" | awk '{print $1}')}
runner_prep_helper_path=${DIREXTALK_RUNNER_PREP_HELPER_PATH:-$script_dir/prepare-runner-cgroups.sh}
runner_prep_helper_sha256=${DIREXTALK_RUNNER_PREP_HELPER_SHA256:-$(sha256sum -- "$runner_prep_helper_path" | awk '{print $1}')}
runner_prep_machine_id=${DIREXTALK_RUNNER_PREP_MACHINE_ID:-unknown}
runner_prep_docker_engine_id=${DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID:-unknown}
extension_control_group=${DIREXTALK_EXTENSION_CONTROL_GROUP:-/system.slice/${extension_cgroup_parent}/${extension_runner_unit}}
core_control_group=${DIREXTALK_CORE_RUNNER_CONTROL_GROUP:-/system.slice/${workload_cgroup_parent}/${core_runner_unit}}
extension_parent_root=${DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT:-/sys/fs/cgroup${extension_control_group%/*}}
core_parent_root=${DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT:-/sys/fs/cgroup${core_control_group%/*}}
extension_parent_procs=${DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS:-$extension_parent_root/cgroup.procs}
core_parent_procs=${DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS:-$core_parent_root/cgroup.procs}
extension_parent_procs_owner=${DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER:-$extension_runner_uid:$extension_runner_uid}
core_parent_procs_owner=${DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER:-$workload_runner_uid:$workload_runner_uid}
extension_parent_procs_mode=${DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE:-644}
core_parent_procs_mode=${DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE:-644}
extension_runner_user=dirextalk-extension-runner
core_runner_user=dirextalk-core-runner
runner_host_prepared=true
if [ "$runner_fixture_mode" = false ]; then
  [ -n "${DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256:-}" ] || die "DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256 must come from prepare-runner-cgroups.sh"
  [ -n "${DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256:-}" ] || die "DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256 must come from prepare-runner-cgroups.sh"
  [ "$runner_apparmor_profile" = dirextalk-runner-userns ] || die "runner AppArmor profile name is not repository-fixed"
  [ "$runner_apparmor_profile_path" = /etc/apparmor.d/dirextalk-runner-userns ] || die "runner AppArmor profile path is not repository-fixed"
  [ "$runner_apparmor_manager_path" = "$script_dir/manage-runner-apparmor.sh" ] || die "runner AppArmor manager path is not the repository entrypoint"
  printf '%s\n' "$runner_apparmor_profile_sha256" | grep -Eq '^[0-9a-f]{64}$' || die "runner AppArmor profile SHA-256 is invalid"
  printf '%s\n' "$runner_apparmor_manager_sha256" | grep -Eq '^[0-9a-f]{64}$' || die "runner AppArmor manager SHA-256 is invalid"
  validate_root_owned_asset DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH "$runner_apparmor_profile_path"
  validate_root_owned_asset DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH "$runner_apparmor_manager_path"
  [ "$(sha256sum -- "$runner_apparmor_profile_path" | awk '{print $1}')" = "$runner_apparmor_profile_sha256" ] || die "runner AppArmor installed profile hash differs from preparation receipt"
  [ "$(sha256sum -- "$runner_apparmor_manager_path" | awk '{print $1}')" = "$runner_apparmor_manager_sha256" ] || die "runner AppArmor manager hash differs from preparation receipt"
  [ "$extension_cgroup_parent" = "$stack_name-extension.slice" ] || die "extension runner parent slice must be stack-bound"
  [ "$workload_cgroup_parent" = "$stack_name-core-runner.slice" ] || die "Core runner parent slice must be stack-bound"
  [ "$extension_runner_unit" = "dirextalk-extension-runner@${stack_name}.service" ] || die "extension runner unit is not stack-bound"
  [ "$core_runner_unit" = "dirextalk-core-runner@${stack_name}.service" ] || die "Core runner unit is not stack-bound"
  validate_runner_control_group DIREXTALK_EXTENSION_CONTROL_GROUP "$extension_control_group" "$stack_name" "$extension_cgroup_parent" "$extension_runner_unit"
  validate_runner_control_group DIREXTALK_CORE_RUNNER_CONTROL_GROUP "$core_control_group" "$stack_name" "$workload_cgroup_parent" "$core_runner_unit"
  [ "$extension_cgroup_root" = "/sys/fs/cgroup${extension_control_group}" ] || die "extension cgroup root must bind its exact ControlGroup"
  [ "$core_runner_cgroup_root" = "/sys/fs/cgroup${core_control_group}" ] || die "Core runner cgroup root must bind its exact ControlGroup"
  [ "$extension_parent_root" = "/sys/fs/cgroup${extension_control_group%/*}" ] || die "extension parent slice root must bind its exact ControlGroup parent"
  [ "$core_parent_root" = "/sys/fs/cgroup${core_control_group%/*}" ] || die "Core parent slice root must bind its exact ControlGroup parent"
  [ "$extension_parent_procs" = "$extension_parent_root/cgroup.procs" ] || die "extension parent process control path is not exact"
  [ "$core_parent_procs" = "$core_parent_root/cgroup.procs" ] || die "Core parent process control path is not exact"
  [ "$extension_parent_procs_owner" = "$extension_runner_uid:$extension_runner_uid" ] || die "extension parent process control owner binding is invalid"
  [ "$core_parent_procs_owner" = "$workload_runner_uid:$workload_runner_uid" ] || die "Core parent process control owner binding is invalid"
  [ "$extension_parent_procs_mode" = 644 ] || die "extension parent process control mode binding is invalid"
  [ "$core_parent_procs_mode" = 644 ] || die "Core parent process control mode binding is invalid"
  [ -d "$extension_parent_root" ] && [ ! -L "$extension_parent_root" ] || die "extension parent slice root is missing or symlinked"
  [ -d "$core_parent_root" ] && [ ! -L "$core_parent_root" ] || die "Core parent slice root is missing or symlinked"
  [ "$(stat -c '%u:%g' -- "$extension_parent_root")" = 0:0 ] || die "extension parent slice directory is not root-owned"
  [ "$(stat -c '%u:%g' -- "$core_parent_root")" = 0:0 ] || die "Core parent slice directory is not root-owned"
  [ "$(stat -c '%u:%g' -- "$extension_parent_procs")" = "$extension_parent_procs_owner" ] || die "extension parent process control owner differs from preparation receipt"
  [ "$(stat -c '%u:%g' -- "$core_parent_procs")" = "$core_parent_procs_owner" ] || die "Core parent process control owner differs from preparation receipt"
  [ "$(stat -c '%a' -- "$extension_parent_procs")" = "$extension_parent_procs_mode" ] || die "extension parent process control mode differs from preparation receipt"
  [ "$(stat -c '%a' -- "$core_parent_procs")" = "$core_parent_procs_mode" ] || die "Core parent process control mode differs from preparation receipt"
  [ -n "${DIREXTALK_RUNNER_PREP_HELPER_SHA256:-}" ] || die "DIREXTALK_RUNNER_PREP_HELPER_SHA256 must come from prepare-runner-cgroups.sh"
  [ -n "${DIREXTALK_RUNNER_PREP_MACHINE_ID:-}" ] || die "DIREXTALK_RUNNER_PREP_MACHINE_ID must come from prepare-runner-cgroups.sh"
  [ -n "${DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID:-}" ] || die "DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID must come from prepare-runner-cgroups.sh"
  printf '%s\n' "$runner_prep_machine_id" | grep -Eq '^[0-9a-f]{32}$' || die "DIREXTALK_RUNNER_PREP_MACHINE_ID must be a 32-char machine-id"
  printf '%s\n' "$runner_prep_docker_engine_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,255}$' || die "DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID is invalid"
  printf '%s\n' "$runner_prep_helper_sha256" | grep -Eq '^[0-9a-f]{64}$' || die "DIREXTALK_RUNNER_PREP_HELPER_SHA256 must be a lowercase SHA-256"
  validate_root_owned_asset DIREXTALK_RUNNER_PREP_HELPER_PATH "$runner_prep_helper_path"
  actual_runner_prep_hash=$(sha256sum -- "$runner_prep_helper_path" | awk '{print $1}')
  [ "$actual_runner_prep_hash" = "$runner_prep_helper_sha256" ] || die "runner preparation helper hash differs from installed asset"
  validate_runner_fragment DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH "$extension_fragment_path" \
    /etc/systemd/system/dirextalk-extension-runner@.service DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256 "$extension_fragment_sha256"
  validate_runner_fragment DIREXTALK_CORE_RUNNER_FRAGMENT_PATH "$core_fragment_path" \
    /etc/systemd/system/dirextalk-core-runner@.service DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256 "$core_fragment_sha256"
else
  echo "warning: explicit runner fixture mode skips host cgroup identity checks; never use it in production" >&2
fi
validate_absolute_path DIREXTALK_EXTENSION_CGROUP_ROOT "$extension_cgroup_root"
validate_absolute_path DIREXTALK_CORE_RUNNER_CGROUP_ROOT "$core_runner_cgroup_root"
validate_cgroup_parent DIREXTALK_EXTENSION_CGROUP_PARENT "$extension_cgroup_parent"
validate_cgroup_parent DIREXTALK_CORE_RUNNER_CGROUP_PARENT "$workload_cgroup_parent"
if [ "$runner_fixture_mode" = false ] && [ -z "${DIREXTALK_EXTENSION_CGROUP_ROOT:-}" ]; then
  die "DIREXTALK_EXTENSION_CGROUP_ROOT must point to a delegated cgroup-v2 subtree when extensions are enabled"
fi
if [ "$runner_fixture_mode" = false ] && [ -z "${DIREXTALK_CORE_RUNNER_CGROUP_ROOT:-}" ]; then
  die "DIREXTALK_CORE_RUNNER_CGROUP_ROOT must point to a delegated cgroup-v2 subtree when Core Runner is enabled"
fi
if [ "$runner_fixture_mode" = false ]; then
  validate_delegated_cgroup_root DIREXTALK_EXTENSION_CGROUP_ROOT "$extension_cgroup_root" "$stack_name" "$extension_cgroup_parent" "$extension_runner_uid"
  validate_delegated_cgroup_root DIREXTALK_CORE_RUNNER_CGROUP_ROOT "$core_runner_cgroup_root" "$stack_name" "$workload_cgroup_parent" "$workload_runner_uid"
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
  "$stack_name-agent-extension-staging" \
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
qdrant_image=$(printenv DIREXTALK_QDRANT_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$qdrant_image" ] || qdrant_image=qdrant/qdrant:v1.18.3@sha256:0bd98fa7977f1e75694779359ca4e212822e5a71334e28421182f72f209d5286
coturn_image=$(printenv DIREXTALK_COTURN_IMAGE_IMMUTABLE 2>/dev/null || true)
[ -n "$coturn_image" ] || coturn_image=docker.io/coturn/coturn:4.6.3-alpine@sha256:e2bca2f79a4269d7240de5872ab60a9305013ad37296d2acf14f9510874346be
for image_pair in \
  DIREXTALK_POSTGRES_IMAGE_IMMUTABLE:$postgres_image \
  DIREXTALK_UTILITY_IMAGE_IMMUTABLE:$utility_image \
  DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE:$message_image \
  DIREXTALK_AGENT_IMAGE_IMMUTABLE:$agent_image \
  DIREXTALK_QDRANT_IMAGE_IMMUTABLE:$qdrant_image \
  DIREXTALK_COTURN_IMAGE_IMMUTABLE:$coturn_image; do
  image_name=${image_pair%%:*}
  image_value=${image_pair#*:}
  validate_immutable_image "$image_name" "$image_value"
done
if [ "$compose_mode" = production ]; then
  case "$message_image:$agent_image" in
    *registry.invalid*|*sha256:0000000000000000000000000000000000000000000000000000000000000000*)
      die "production application images must be non-placeholder Docker Hub digests"
      ;;
  esac
fi

write_secret "$out/agent-postgres-password" "$agent_password"
write_secret "$out/message-postgres-password" "$message_password"
write_secret "$out/agent-database-url" "postgresql://dirextalk_agent:$agent_password@agent-postgres:5432/dirextalk_agent?sslmode=disable"
write_secret "$out/message-database-url" "postgresql://dirextalk_message_server:$message_password@message-postgres:5432/dirextalk_message_server?sslmode=disable"
write_secret "$out/message-registration-shared-secret" "$message_registration_shared_secret"
write_secret "$out/turn-shared-secret" "$turn_shared_secret"
umask 077
{
  printf '%s\n' \
    'listening-port=3478' \
    'min-port=49160' \
    'max-port=49200' \
    "realm=$message_server_name" \
    "external-ip=$turn_external_ip" \
    'fingerprint' \
    'use-auth-secret'
  printf 'static-auth-secret=%s\n' "$turn_shared_secret"
  printf '%s\n' \
    'stale-nonce=600' \
    'no-cli' \
    'no-multicast-peers' \
    'no-tls' \
    'no-dtls' \
    'no-sqlite' \
    'pidfile=/tmp/turnserver.pid'
} >"$out/turnserver.conf"
chmod 400 "$out/turnserver.conf"
unset turn_shared_secret
write_secret "$out/message-portal-password" "$message_portal_password"
write_raw_secret "$out/core-secret-master-key" 32
core_secret_master_key_device=$(stat -c '%d' "$out/core-secret-master-key")
core_secret_master_key_inode=$(stat -c '%i' "$out/core-secret-master-key")
core_secret_master_key_uid=$(stat -c '%u' "$out/core-secret-master-key")

# Keep TLS source paths explicit and path-only. Local mode intentionally starts
# with empty protected placeholders; message-server-init generates the
# disposable certificate/key in its fresh config volume. External mode copies
# a caller-provided, protected pair through descriptor-bound reads before
# validating it. Neither source path nor certificate/key material is written to
# Compose or the manifest.
message_tls_cert_file=$out/message-tls-external-cert.pem
message_tls_key_file=$out/message-tls-external-key.pem
image_attestation_source=${DIREXTALK_IMAGE_ATTESTATION_SOURCE_FILE:-}
image_attestation_file=$out/image-attestation
if [ "$compose_mode" = production ]; then
  [ -n "$image_attestation_source" ] || die "DIREXTALK_IMAGE_ATTESTATION_SOURCE_FILE is required for production"
  validate_absolute_path DIREXTALK_IMAGE_ATTESTATION_SOURCE_FILE "$image_attestation_source"
elif [ -n "$image_attestation_source" ]; then
  die "DIREXTALK_IMAGE_ATTESTATION_SOURCE_FILE is only valid for production"
fi

copy_secret_or_empty "$openrouter_source" "$out/openrouter-api-key" "OpenRouter API key"
copy_secret_or_empty "$embedding_source" "$out/embedding-api-key" "embedding API key"
copy_secret_or_empty "$tavily_source" "$out/tavily-api-key" "Tavily API key"
tls_placeholder_warning=true
[ "$message_tls_mode" != edge-terminated ] || tls_placeholder_warning=false
copy_secret_or_empty "$message_tls_cert_source" "$message_tls_cert_file" "external message-server TLS certificate" "$tls_placeholder_warning"
copy_secret_or_empty "$message_tls_key_source" "$message_tls_key_file" "external message-server TLS private key" "$tls_placeholder_warning"
copy_secret_or_empty "$image_attestation_source" "$image_attestation_file" "production image attestation"

if [ "$compose_mode" = production ]; then
  [ -s "$image_attestation_file" ] || die "production image attestation must be non-empty"
fi
[ -f "$image_attestation_file" ] && [ ! -L "$image_attestation_file" ] || die "provisioned image attestation is not a regular non-symlink file"
[ "$(stat -c '%u' -- "$image_attestation_file")" = "$(id -u)" ] || die "provisioned image attestation is not owned by the provisioning user"
[ "$(stat -c '%a' -- "$image_attestation_file")" = 400 ] || die "provisioned image attestation must have mode 0400"
image_attestation_device=$(stat -c '%d' -- "$image_attestation_file")
image_attestation_inode=$(stat -c '%i' -- "$image_attestation_file")
image_attestation_uid=$(stat -c '%u' -- "$image_attestation_file")
image_attestation_sha256=$(sha256sum -- "$image_attestation_file" | awk '{print $1}')

if [ "$message_tls_mode" = external ]; then
  [ -s "$message_tls_cert_file" ] || die "external message-server TLS certificate must be non-empty"
  [ -s "$message_tls_key_file" ] || die "external message-server TLS private key must be non-empty"
  openssl x509 -in "$message_tls_cert_file" -noout >/dev/null 2>&1 || die "external message-server TLS certificate cannot be parsed"
  openssl pkey -in "$message_tls_key_file" -noout >/dev/null 2>&1 || die "external message-server TLS private key cannot be parsed"
  openssl x509 -in "$message_tls_cert_file" -checkend 604800 -noout >/dev/null 2>&1 || \
    die "external message-server TLS certificate expires within seven days"
  if cert_host_check=$(openssl x509 -in "$message_tls_cert_file" -checkhost "$message_server_name" -noout 2>/dev/null); then
    case "$cert_host_check" in
      *" does match certificate") ;;
      *) die "external message-server TLS certificate SAN/CN does not match the configured server identity" ;;
    esac
  else
    die "external message-server TLS certificate SAN/CN could not be checked"
  fi
  if cert_pub=$(openssl x509 -in "$message_tls_cert_file" -pubkey -noout 2>/dev/null | \
    openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}'); then
    :
  else
    die "external message-server TLS certificate public key could not be derived"
  fi
  if key_pub=$(openssl pkey -in "$message_tls_key_file" -pubout 2>/dev/null | \
    openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}'); then
    :
  else
    die "external message-server TLS private key public key could not be derived"
  fi
  [ -n "$cert_pub" ] && [ "$cert_pub" = "$key_pub" ] || die "external message-server TLS certificate and private key do not match"
fi

for message_tls_file in "$message_tls_cert_file" "$message_tls_key_file"; do
  [ -f "$message_tls_file" ] && [ ! -L "$message_tls_file" ] || die "provisioned message-server TLS file is not a regular non-symlink file"
  [ "$(stat -c '%u' -- "$message_tls_file")" = "$(id -u)" ] || die "provisioned message-server TLS file is not owned by the provisioning user"
  [ "$(stat -c '%a' -- "$message_tls_file")" = 400 ] || die "provisioned message-server TLS file must have mode 0400"
done
message_tls_cert_device=$(stat -c '%d' -- "$message_tls_cert_file")
message_tls_cert_inode=$(stat -c '%i' -- "$message_tls_cert_file")
message_tls_cert_uid=$(stat -c '%u' -- "$message_tls_cert_file")
message_tls_cert_sha256=$(sha256sum -- "$message_tls_cert_file" | awk '{print $1}')
message_tls_key_device=$(stat -c '%d' -- "$message_tls_key_file")
message_tls_key_inode=$(stat -c '%i' -- "$message_tls_key_file")
message_tls_key_uid=$(stat -c '%u' -- "$message_tls_key_file")
message_tls_key_sha256=$(sha256sum -- "$message_tls_key_file" | awk '{print $1}')

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
EOF
if [ "$core_aws_ssm_enabled" = true ]; then
  cat >>"$out/agent-config.yaml" <<EOF
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
EOF
fi
cat >>"$out/agent-config.yaml" <<EOF
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
DIREXTALK_SPLIT_COMPOSE_MODE=$compose_mode
DIREXTALK_MESSAGE_SERVER_ENTRYPOINT_FILE=$split_deploy_dir/scripts/message-server-entrypoint.sh
DIREXTALK_CAPABILITY_CA_INITIALIZER_FILE=$split_deploy_dir/scripts/initialize-capability-ca.sh
DIREXTALK_MESSAGE_SERVER_INITIALIZER_FILE=$split_deploy_dir/scripts/initialize-message-server.sh
DIREXTALK_AGENT_SECRET_MATERIALIZER_FILE=$split_deploy_dir/scripts/materialize-agent-secrets.sh
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$message_image
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image
DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=$postgres_image
DIREXTALK_UTILITY_IMAGE_IMMUTABLE=$utility_image
DIREXTALK_QDRANT_IMAGE_IMMUTABLE=$qdrant_image
DIREXTALK_COTURN_IMAGE_IMMUTABLE=$coturn_image
DIREXTALK_AGENT_BUILD_CONTEXT=$agent_root
DIREXTALK_MESSAGE_BUILD_CONTEXT=$message_root
DIREXTALK_AGENT_BUILD_VERSION=$agent_build_version
DIREXTALK_AGENT_BUILD_REVISION=$agent_build_revision
DIREXTALK_MESSAGE_BUILD_VERSION=$message_build_version
DIREXTALK_MESSAGE_BUILD_REVISION=$message_build_revision
DIREXTALK_MESSAGE_SERVER_IMAGE_LOCAL=dirextalk-message-server:split-local
DIREXTALK_AGENT_IMAGE_LOCAL=dirextalk-agent:split-local
DIREXTALK_IMAGE_ATTESTATION_FILE=$image_attestation_file
DIREXTALK_MESSAGE_SERVER_INSTANCE_ID=$message_instance_id
DIREXTALK_AGENT_INSTANCE_ID=$agent_instance_id
DIREXTALK_ACCOUNT_GENERATION=$account_generation
DIREXTALK_RELEASE_CATALOG_ORIGIN=$release_catalog_origin
DIREXTALK_AGENT_TLS_SERVER_NAME=dirextalk-agent
DIREXTALK_MESSAGE_SERVER_NAME=$message_server_name
DIREXTALK_MESSAGE_CLIENT_BASE_URL=$message_client_base_url
DIREXTALK_MESSAGE_TLS_MODE=$message_tls_mode
DIREXTALK_MESSAGE_TLS_CERT_FILE=$message_tls_cert_file
DIREXTALK_MESSAGE_TLS_KEY_FILE=$message_tls_key_file
DIREXTALK_MESSAGE_HTTP_BIND=$message_http_bind
DIREXTALK_MESSAGE_HTTPS_BIND=$message_https_bind
DIREXTALK_TURN_EXTERNAL_IP=$turn_external_ip
DIREXTALK_COTURN_CONFIG_FILE=$out/turnserver.conf
DIREXTALK_TURN_SHARED_SECRET_FILE=$out/turn-shared-secret
DIREXTALK_AGENT_CONFIG_FILE=$out/agent-config.yaml
DIREXTALK_MESSAGE_POSTGRES_PASSWORD_FILE=$out/message-postgres-password
DIREXTALK_AGENT_POSTGRES_PASSWORD_FILE=$out/agent-postgres-password
DIREXTALK_MESSAGE_DATABASE_URL_FILE=$out/message-database-url
DIREXTALK_MESSAGE_REGISTRATION_SHARED_SECRET_FILE=$out/message-registration-shared-secret
DIREXTALK_MESSAGE_PORTAL_PASSWORD_FILE=$out/message-portal-password
DIREXTALK_AGENT_DATABASE_URL_FILE=$out/agent-database-url
DIREXTALK_OPENROUTER_API_KEY_FILE=$out/openrouter-api-key
DIREXTALK_EMBEDDING_API_KEY_FILE=$out/embedding-api-key
DIREXTALK_TAVILY_API_KEY_FILE=$out/tavily-api-key
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
DIREXTALK_RUNNER_HOST_PREPARED=$runner_host_prepared
DIREXTALK_EXTENSION_RUNNER_UNIT=$extension_runner_unit
DIREXTALK_CORE_RUNNER_UNIT=$core_runner_unit
DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH=$extension_fragment_path
DIREXTALK_CORE_RUNNER_FRAGMENT_PATH=$core_fragment_path
DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256=$extension_fragment_sha256
DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256=$core_fragment_sha256
DIREXTALK_RUNNER_APPARMOR_PROFILE=$runner_apparmor_profile
DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH=$runner_apparmor_profile_path
DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256=$runner_apparmor_profile_sha256
DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH=$runner_apparmor_manager_path
DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256=$runner_apparmor_manager_sha256
DIREXTALK_RUNNER_PREP_HELPER_PATH=$runner_prep_helper_path
DIREXTALK_RUNNER_PREP_HELPER_SHA256=$runner_prep_helper_sha256
DIREXTALK_RUNNER_PREP_MACHINE_ID=$runner_prep_machine_id
DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID=$runner_prep_docker_engine_id
DIREXTALK_EXTENSION_CONTROL_GROUP=$extension_control_group
DIREXTALK_CORE_RUNNER_CONTROL_GROUP=$core_control_group
DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT=$extension_parent_root
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT=$core_parent_root
DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS=$extension_parent_procs
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS=$core_parent_procs
DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER=$extension_parent_procs_owner
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER=$core_parent_procs_owner
DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE=$extension_parent_procs_mode
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE=$core_parent_procs_mode
DIREXTALK_EXTENSION_RUNNER_USER=$extension_runner_user
DIREXTALK_CORE_RUNNER_USER=$core_runner_user
DIREXTALK_CORE_EXTENSION_ENABLED=$core_extension_enabled
DIREXTALK_CORE_WORKLOAD_ENABLED=$core_workload_enabled
DIREXTALK_CORE_AWS_ENABLED=$core_aws_enabled
EOF
if [ "$core_aws_ssm_enabled" = true ]; then
  cat >>"$out/.env" <<EOF
DIREXTALK_CORE_AWS_SSM_ENABLED=true
DIREXTALK_CORE_AWS_SSM_CREDENTIAL_REFERENCE=$core_aws_ssm_credential_reference
DIREXTALK_CORE_AWS_SSM_REGION=$core_aws_ssm_region
DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID=$core_aws_ssm_account_id
DIREXTALK_CORE_AWS_SSM_INSTANCE_ID=$core_aws_ssm_instance_id
DIREXTALK_CORE_AWS_SSM_DOCUMENT_VERSION=$core_aws_ssm_document_version
DIREXTALK_CORE_AWS_SSM_SYSTEMD_SERVICE=$core_aws_ssm_systemd_service
DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_KEY=$core_aws_ssm_required_tag_key
DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_VALUE=$core_aws_ssm_required_tag_value
EOF
else
  cat >>"$out/.env" <<EOF
DIREXTALK_CORE_AWS_SSM_ENABLED=false
EOF
fi
cat >>"$out/.env" <<EOF
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
compose_mode=$compose_mode
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
message_tls_mode=$message_tls_mode
message_server_name=$message_server_name
turn_external_ip=$turn_external_ip
turn_config_path=$out/turnserver.conf
turn_config_device=$(stat -c '%d' "$out/turnserver.conf")
turn_config_inode=$(stat -c '%i' "$out/turnserver.conf")
turn_config_uid=$(stat -c '%u' "$out/turnserver.conf")
turn_secret_path=$out/turn-shared-secret
turn_secret_device=$(stat -c '%d' "$out/turn-shared-secret")
turn_secret_inode=$(stat -c '%i' "$out/turn-shared-secret")
turn_secret_uid=$(stat -c '%u' "$out/turn-shared-secret")
message_tls_cert_path=$message_tls_cert_file
message_tls_cert_device=$message_tls_cert_device
message_tls_cert_inode=$message_tls_cert_inode
message_tls_cert_uid=$message_tls_cert_uid
message_tls_cert_sha256=$message_tls_cert_sha256
message_tls_key_path=$message_tls_key_file
message_tls_key_device=$message_tls_key_device
message_tls_key_inode=$message_tls_key_inode
message_tls_key_uid=$message_tls_key_uid
message_tls_key_sha256=$message_tls_key_sha256
image_attestation_path=$image_attestation_file
image_attestation_device=$image_attestation_device
image_attestation_inode=$image_attestation_inode
image_attestation_uid=$image_attestation_uid
image_attestation_sha256=$image_attestation_sha256
runner_host_prepared=$runner_host_prepared
core_extension_enabled=$core_extension_enabled
core_workload_enabled=$core_workload_enabled
runner.extension.unit=$extension_runner_unit
runner.extension.user=$extension_runner_user
runner.extension.group=$extension_runner_user
runner.extension.uid=$extension_runner_uid
runner.extension.gid=$extension_runner_uid
runner.extension.parent=$extension_cgroup_parent
runner.extension.root=$extension_cgroup_root
runner.extension.control_group=$extension_control_group
runner.extension.parent_root=$extension_parent_root
runner.extension.parent_procs=$extension_parent_procs
runner.extension.parent_procs_owner=$extension_parent_procs_owner
runner.extension.parent_procs_mode=$extension_parent_procs_mode
runner.extension.fragment_path=$extension_fragment_path
runner.extension.fragment_sha256=$extension_fragment_sha256
runner.helper.path=$runner_prep_helper_path
runner.helper.sha256=$runner_prep_helper_sha256
runner.apparmor.profile=$runner_apparmor_profile
runner.apparmor.path=$runner_apparmor_profile_path
runner.apparmor.sha256=$runner_apparmor_profile_sha256
runner.apparmor.manager_path=$runner_apparmor_manager_path
runner.apparmor.manager_sha256=$runner_apparmor_manager_sha256
runner.machine_id=$runner_prep_machine_id
runner.docker_engine_id=$runner_prep_docker_engine_id
runner.core.unit=$core_runner_unit
runner.core.user=$core_runner_user
runner.core.group=$core_runner_user
runner.core.uid=$workload_runner_uid
runner.core.gid=$workload_runner_uid
runner.core.parent=$workload_cgroup_parent
runner.core.root=$core_runner_cgroup_root
runner.core.control_group=$core_control_group
runner.core.parent_root=$core_parent_root
runner.core.parent_procs=$core_parent_procs
runner.core.parent_procs_owner=$core_parent_procs_owner
runner.core.parent_procs_mode=$core_parent_procs_mode
runner.core.fragment_path=$core_fragment_path
runner.core.fragment_sha256=$core_fragment_sha256
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
