#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
mkdir "$tmp/bin"
cat >"$tmp/bin/stat" <<'EOF'
#!/bin/sh
if [ "${1:-}" = -c ] && [ "${2:-}" = '%a:%u' ]; then
  printf '%s\n' '400:65532'
  exit 0
fi
exec /usr/bin/stat "$@"
EOF
chmod 0755 "$tmp/bin/stat"

for name in control-cert control-key relay-cert relay-key iid pricing qualification; do
  install -m 0400 /dev/null "$tmp/$name"
done

export DIREXTALK_CLOUD_WORKER_ACCOUNT_ID=066107820442
export DIREXTALK_CLOUD_WORKER_REGION=ap-east-1
export DIREXTALK_CLOUD_WORKER_CREDENTIAL_ID=00000000-0000-4000-8000-000000000001
export DIREXTALK_CLOUD_WORKER_CREDENTIAL_REVISION=1
export DIREXTALK_CLOUD_WORKER_INSTANCE_TYPE=t3.small
export DIREXTALK_CLOUD_WORKER_ARCHITECTURE=x86_64
export DIREXTALK_CLOUD_WORKER_ROOT_DEVICE_NAME=/dev/xvda
export DIREXTALK_CLOUD_WORKER_VOLUME_GIB=16
export DIREXTALK_CLOUD_WORKER_VOLUME_TYPE=gp3
export DIREXTALK_CLOUD_WORKER_VOLUME_IOPS=3000
export DIREXTALK_CLOUD_WORKER_VOLUME_THROUGHPUT_MIB=125
export DIREXTALK_CLOUD_WORKER_AMI_ID=ami-0123456789abcdef0
digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export DIREXTALK_CLOUD_WORKER_AMI_DIGEST=$digest
export DIREXTALK_CLOUD_WORKER_WORKER_RELEASE_DIGEST=$digest
export DIREXTALK_CLOUD_WORKER_PI_RUNTIME_DIGEST=$digest
export DIREXTALK_CLOUD_WORKER_HOST_NETWORK_POLICY_SHA256=$digest
export DIREXTALK_CLOUD_WORKER_VPC_ID=vpc-0123456789abcdef0
export DIREXTALK_CLOUD_WORKER_SUBNET_ID=subnet-0123456789abcdef0
export DIREXTALK_CLOUD_WORKER_DNS_RESOLVER_CIDRS='10.0.0.2/32'
export DIREXTALK_CLOUD_WORKER_TLS_PROXY_CIDRS='10.0.0.3/32'
export DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS='worker.example.test relay.example.test bucket.s3.ap-east-1.amazonaws.com sts.ap-east-1.amazonaws.com'
export DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME=worker.example.test
export DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME=relay.example.test
export DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME=proxy.example.test
export DIREXTALK_CLOUD_WORKER_OUTBOUND_PROXY_TRUST_SHA256=$digest
export DIREXTALK_CLOUD_WORKER_ARTIFACT_BUCKET=dirextalk-worker-test
export DIREXTALK_CLOUD_WORKER_ARTIFACT_BASE_PREFIX=cloud-worker/
export DIREXTALK_CLOUD_WORKER_ARTIFACT_KMS_KEY_ARN=arn:aws:kms:ap-east-1:066107820442:key/00000000-0000-4000-8000-000000000001
export DIREXTALK_CLOUD_WORKER_ARTIFACT_RETENTION=1h
export DIREXTALK_CLOUD_WORKER_WORKER_CONTROL_TRUST_SHA256=$digest
export DIREXTALK_CLOUD_WORKER_MODEL_RELAY_TRUST_SHA256=$digest
export DIREXTALK_CLOUD_WORKER_PRICING_CATALOG_SHA256=$digest
export DIREXTALK_CLOUD_WORKER_RUNTIME_QUALIFICATION_SHA256=$digest
export DIREXTALK_CLOUD_WORKER_QUOTE_TTL=5m
export DIREXTALK_CLOUD_WORKER_MAXIMUM_CATALOG_AGE=5m
export DIREXTALK_CLOUD_WORKER_CONTINGENCY_BASIS_POINTS=1000
export DIREXTALK_CLOUD_WORKER_ABSOLUTE_HARD_LIMIT_MICROS=20000000
export DIREXTALK_CLOUD_WORKER_MAX_RUNTIME=30m
export DIREXTALK_CLOUD_WORKER_MAX_TOKENS=8192
export DIREXTALK_CLOUD_WORKER_MAX_OUTPUT_BYTES=1048576
export DIREXTALK_CLOUD_WORKER_CONTROLLER_POLL_INTERVAL=500ms
export DIREXTALK_CLOUD_WORKER_WORKER_HEARTBEAT_INTERVAL=10s
export DIREXTALK_CLOUD_WORKER_REAPER_INTERVAL=30s
export DIREXTALK_CLOUD_WORKER_COMPLETION_OUTBOX_INTERVAL=1s
export DIREXTALK_CLOUD_WORKER_CONTROL_TLS_CERT_FILE=$tmp/control-cert
export DIREXTALK_CLOUD_WORKER_CONTROL_TLS_KEY_FILE=$tmp/control-key
export DIREXTALK_CLOUD_WORKER_MODEL_RELAY_TLS_CERT_FILE=$tmp/relay-cert
export DIREXTALK_CLOUD_WORKER_MODEL_RELAY_TLS_KEY_FILE=$tmp/relay-key
export DIREXTALK_CLOUD_WORKER_IID_CERTIFICATE_FILE=$tmp/iid
export DIREXTALK_CLOUD_WORKER_PRICING_CATALOG_FILE=$tmp/pricing
export DIREXTALK_CLOUD_WORKER_RUNTIME_QUALIFICATION_FILE=$tmp/qualification

PATH="$tmp/bin:$PATH" "$script_dir/render-cloud-worker-agent-config.sh" "$tmp/config.yaml"
grep -Fqx 'core_aws_enabled: true' "$tmp/config.yaml"
grep -Fqx 'core_execution_v2_enabled: true' "$tmp/config.yaml"
grep -Fqx '  account_id: "066107820442"' "$tmp/config.yaml"
grep -Fqx '  absolute_hard_limit_micros: 20000000' "$tmp/config.yaml"
grep -Fqx '  worker_control_endpoint: https://worker.example.test:443' "$tmp/config.yaml"
grep -Fqx '  model_relay_endpoint: https://relay.example.test:443' "$tmp/config.yaml"
[ "$(stat -c '%a' "$tmp/config.yaml")" = 400 ]

if DIREXTALK_CLOUD_WORKER_ARTIFACT_BUCKET="bad
bucket" PATH="$tmp/bin:$PATH" "$script_dir/render-cloud-worker-agent-config.sh" "$tmp/injected.yaml" >/dev/null 2>&1; then
  echo 'newline YAML injection unexpectedly passed' >&2
  exit 1
fi

printf '%s\n' 'Cloud Worker Agent config rendering tests passed'
