#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/provision-local.sh
[ -x "$script" ] || { echo "provision-local.sh must be executable" >&2; exit 1; }
bash -n "$script"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x "$script"
fi
if grep -Fq -- "[ -w \"\$value/cgroup.subtree_control\" ]" "$script" || \
   grep -Fq -- "[ -w \"\$value/cgroup.procs\" ]" "$script"; then
  echo "provision-local.sh must not test cgroup writability as the current user" >&2
  exit 1
fi
grep -Fq -- 'validate_target_write_access' "$script"
grep -Fq -- 'getfacl -cp' "$script"

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

attestation_source=$tmp_dir/image-attestation
image_digest=1111111111111111111111111111111111111111111111111111111111111111
cat >"$attestation_source" <<EOF
# dirextalk-image-attestation-v2
capability_api_version=v1.0.3
capability_api_source=published
message_source_revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
agent_source_revision=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
EOF
chmod 400 "$attestation_source"

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
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=docker.io/dirextalk/message-server@sha256:$image_digest \
DIREXTALK_AGENT_IMAGE_IMMUTABLE=docker.io/dirextalk/agent@sha256:$image_digest \
DIREXTALK_IMAGE_ATTESTATION_SOURCE_FILE=$attestation_source \
DIREXTALK_MESSAGE_TLS_MODE=external \
DIREXTALK_MESSAGE_SERVER_NAME=message.example.com \
DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE=$tls_cert_source \
DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE=$tls_key_source \
  "$script" "$production_dir" >/dev/null
grep -Fqx 'DIREXTALK_SPLIT_COMPOSE_MODE=production' "$production_dir/.env"
grep -Fqx 'compose_mode=production' "$production_dir/.manifest"
grep -Fqx "DIREXTALK_IMAGE_ATTESTATION_FILE=$production_dir/image-attestation" "$production_dir/.env"
cmp -- "$attestation_source" "$production_dir/image-attestation"
[ "$(stat -c '%a' "$production_dir/image-attestation")" = 400 ]

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

ssm_credential=11111111-1111-4111-8111-111111111111
ssm_region=us-east-1
ssm_account=123456789012
ssm_instance=i-0123456789abcdef0
ssm_document=1
ssm_service=dirextalk-agent.service
ssm_tag_key=managed
ssm_tag_value=true

false_dir=$tmp_dir/aws-without-ssm
if DIREXTALK_CORE_AWS_ENABLED=true \
  DIREXTALK_CORE_AWS_SSM_ENABLED=false \
  DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID=not-an-account \
  DIREXTALK_CORE_AWS_SSM_INSTANCE_ID=not-an-instance \
  "$script" "$false_dir" >/dev/null 2>"$tmp_dir/aws-without-ssm.stderr"; then
  :
else
  echo "aws=true with ssm=false must not validate or require SSM readiness metadata" >&2
  exit 1
fi
if grep -Fq 'core_aws_ssm_readiness:' "$false_dir/agent-config.yaml"; then
  echo "aws=true with ssm=false rendered an SSM readiness block" >&2
  exit 1
fi
grep -Fqx 'DIREXTALK_CORE_AWS_SSM_ENABLED=false' "$false_dir/.env"
if grep -Eq '^DIREXTALK_CORE_AWS_SSM_(CREDENTIAL_REFERENCE|REGION|ACCOUNT_ID|INSTANCE_ID|DOCUMENT_VERSION|SYSTEMD_SERVICE|REQUIRED_TAG_KEY|REQUIRED_TAG_VALUE)=' "$false_dir/.env"; then
  echo "aws=true with ssm=false rendered SSM readiness metadata" >&2
  exit 1
fi

true_dir=$tmp_dir/aws-with-ssm
DIREXTALK_CORE_AWS_ENABLED=true \
  DIREXTALK_CORE_AWS_SSM_ENABLED=true \
  DIREXTALK_CORE_AWS_SSM_CREDENTIAL_REFERENCE=$ssm_credential \
  DIREXTALK_CORE_AWS_SSM_REGION=$ssm_region \
  DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID=$ssm_account \
  DIREXTALK_CORE_AWS_SSM_INSTANCE_ID=$ssm_instance \
  DIREXTALK_CORE_AWS_SSM_DOCUMENT_VERSION=$ssm_document \
  DIREXTALK_CORE_AWS_SSM_SYSTEMD_SERVICE=$ssm_service \
  DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_KEY=$ssm_tag_key \
  DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_VALUE=$ssm_tag_value \
  "$script" "$true_dir" >/dev/null
grep -Fqx 'DIREXTALK_CORE_AWS_SSM_ENABLED=true' "$true_dir/.env"
grep -Fq 'core_aws_ssm_readiness:' "$true_dir/agent-config.yaml"
grep -Fqx "DIREXTALK_CORE_AWS_SSM_CREDENTIAL_REFERENCE=$ssm_credential" "$true_dir/.env"
grep -Fqx "DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID=$ssm_account" "$true_dir/.env"
grep -Fqx "DIREXTALK_CORE_AWS_SSM_INSTANCE_ID=$ssm_instance" "$true_dir/.env"
grep -Fqx "DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_KEY=$ssm_tag_key" "$true_dir/.env"
grep -Fqx "DIREXTALK_CORE_AWS_SSM_REQUIRED_TAG_VALUE=$ssm_tag_value" "$true_dir/.env"

if DIREXTALK_CORE_AWS_ENABLED=false DIREXTALK_CORE_AWS_SSM_ENABLED=true "$script" "$tmp_dir/invalid-ssm" >/dev/null 2>&1; then
  echo "ssm=true without aws=true was unexpectedly accepted" >&2
  exit 1
fi

echo "provision-local password and AWS SSM gating tests passed"
