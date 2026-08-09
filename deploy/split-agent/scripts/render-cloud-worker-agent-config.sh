#!/bin/sh
set -eu

die() { printf '%s\n' "$*" >&2; exit 64; }
req() { eval "v=\${$1:-}"; [ -n "$v" ] || die "$1 is required"; }
match() { eval "v=\${$1}"; printf '%s' "$v" | grep -Eq "$2" || die "$1 is invalid"; }
single_line() { eval "v=\${$1}"; [ -n "$v" ] && [ "$(printf '%s' "$v" | wc -l)" -eq 0 ] && ! printf '%s' "$v" | grep -q '[[:cntrl:]]' || die "$1 must be one non-empty line"; }

[ "$#" -eq 1 ] || die "usage: render-cloud-worker-agent-config.sh OUTPUT_FILE"
out=$1
[ ! -e "$out" ] || die "output file already exists: $out"

required='ACCOUNT_ID REGION CREDENTIAL_ID CREDENTIAL_REVISION INSTANCE_TYPE ARCHITECTURE ROOT_DEVICE_NAME VOLUME_GIB VOLUME_TYPE VOLUME_IOPS VOLUME_THROUGHPUT_MIB AMI_ID AMI_DIGEST WORKER_RELEASE_DIGEST PI_RUNTIME_DIGEST HOST_NETWORK_POLICY_SHA256 VPC_ID SUBNET_ID DNS_RESOLVER_CIDRS TLS_PROXY_CIDRS ALLOWED_FQDNS CONTROL_HOSTNAME MODEL_RELAY_HOSTNAME PROXY_HOSTNAME OUTBOUND_PROXY_TRUST_SHA256 ARTIFACT_BUCKET ARTIFACT_BASE_PREFIX ARTIFACT_KMS_KEY_ARN ARTIFACT_RETENTION WORKER_CONTROL_TRUST_SHA256 MODEL_RELAY_TRUST_SHA256 PRICING_CATALOG_SHA256 RUNTIME_QUALIFICATION_SHA256 QUOTE_TTL MAXIMUM_CATALOG_AGE CONTINGENCY_BASIS_POINTS ABSOLUTE_HARD_LIMIT_MICROS MAX_RUNTIME MAX_TOKENS MAX_OUTPUT_BYTES CONTROLLER_POLL_INTERVAL WORKER_HEARTBEAT_INTERVAL REAPER_INTERVAL COMPLETION_OUTBOX_INTERVAL'
for suffix in $required; do req "DIREXTALK_CLOUD_WORKER_$suffix"; done
for suffix in $required; do
  case "$suffix" in DNS_RESOLVER_CIDRS|TLS_PROXY_CIDRS|ALLOWED_FQDNS) ;; *) single_line "DIREXTALK_CLOUD_WORKER_$suffix" ;; esac
done

match DIREXTALK_CLOUD_WORKER_ACCOUNT_ID '^[0-9]{12}$'
match DIREXTALK_CLOUD_WORKER_REGION '^[a-z]{2}(-gov)?-[a-z]+-[0-9]+$'
match DIREXTALK_CLOUD_WORKER_CREDENTIAL_ID '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
for name in CREDENTIAL_REVISION VOLUME_GIB VOLUME_IOPS VOLUME_THROUGHPUT_MIB CONTINGENCY_BASIS_POINTS ABSOLUTE_HARD_LIMIT_MICROS MAX_TOKENS MAX_OUTPUT_BYTES; do
  match "DIREXTALK_CLOUD_WORKER_$name" '^[1-9][0-9]*$'
done
for name in AMI_DIGEST WORKER_RELEASE_DIGEST PI_RUNTIME_DIGEST HOST_NETWORK_POLICY_SHA256 OUTBOUND_PROXY_TRUST_SHA256 WORKER_CONTROL_TRUST_SHA256 MODEL_RELAY_TRUST_SHA256 PRICING_CATALOG_SHA256 RUNTIME_QUALIFICATION_SHA256; do
  match "DIREXTALK_CLOUD_WORKER_$name" '^[0-9a-f]{64}$'
done
match DIREXTALK_CLOUD_WORKER_AMI_ID '^ami-[0-9a-f]{17}$'
match DIREXTALK_CLOUD_WORKER_VPC_ID '^vpc-[0-9a-f]{17}$'
match DIREXTALK_CLOUD_WORKER_SUBNET_ID '^subnet-[0-9a-f]{17}$'
match DIREXTALK_CLOUD_WORKER_INSTANCE_TYPE '^[a-z][a-z0-9]*[0-9][a-z0-9.]*$'
match DIREXTALK_CLOUD_WORKER_ROOT_DEVICE_NAME '^/dev/[a-z0-9]+$'
match DIREXTALK_CLOUD_WORKER_ARTIFACT_BUCKET '^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$'
match DIREXTALK_CLOUD_WORKER_ARTIFACT_BASE_PREFIX '^[A-Za-z0-9][A-Za-z0-9._/-]*/$'
case "$DIREXTALK_CLOUD_WORKER_ARTIFACT_BASE_PREFIX" in *..*) die "ARTIFACT_BASE_PREFIX must not contain .." ;; esac
match DIREXTALK_CLOUD_WORKER_ARTIFACT_KMS_KEY_ARN "^arn:aws:kms:$DIREXTALK_CLOUD_WORKER_REGION:$DIREXTALK_CLOUD_WORKER_ACCOUNT_ID:key/[0-9a-f-]{36}$"
[ "$DIREXTALK_CLOUD_WORKER_ABSOLUTE_HARD_LIMIT_MICROS" -le 20000000 ] || die "ABSOLUTE_HARD_LIMIT_MICROS exceeds the authorized 20 USD ceiling"
for suffix in ARTIFACT_RETENTION QUOTE_TTL MAXIMUM_CATALOG_AGE MAX_RUNTIME CONTROLLER_POLL_INTERVAL WORKER_HEARTBEAT_INTERVAL REAPER_INTERVAL COMPLETION_OUTBOX_INTERVAL; do
  match "DIREXTALK_CLOUD_WORKER_$suffix" '^[1-9][0-9]*(ms|s|m|h)$'
done
[ "$DIREXTALK_CLOUD_WORKER_ARCHITECTURE" = x86_64 ] || [ "$DIREXTALK_CLOUD_WORKER_ARCHITECTURE" = arm64 ] || die "ARCHITECTURE is invalid"
[ "$DIREXTALK_CLOUD_WORKER_VOLUME_TYPE" = gp3 ] || die "VOLUME_TYPE must be gp3"

for suffix in CONTROL_HOSTNAME MODEL_RELAY_HOSTNAME PROXY_HOSTNAME; do
  match "DIREXTALK_CLOUD_WORKER_$suffix" '^[a-z0-9][a-z0-9.-]*\.[a-z0-9.-]*[a-z0-9]$'
done
case " $DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS " in
  *" $DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME "*) ;;
  *) die "ALLOWED_FQDNS must contain CONTROL_HOSTNAME" ;;
esac

validate_cidr_list() {
  name=$1
  eval "values=\${$name}"
  seen=
  for value in $values; do
    printf '%s' "$value" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}/([0-9]|[12][0-9]|3[0-2])$' || die "$name contains an invalid IPv4 CIDR"
    [ "$value" != 0.0.0.0/0 ] || die "$name must not contain 0.0.0.0/0"
    case " $seen " in *" $value "*) die "$name contains a duplicate" ;; esac
    seen="${seen:+$seen }$value"
  done
}
validate_cidr_list DIREXTALK_CLOUD_WORKER_DNS_RESOLVER_CIDRS
validate_cidr_list DIREXTALK_CLOUD_WORKER_TLS_PROXY_CIDRS

seen_fqdns=
for value in $DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS; do
  printf '%s' "$value" | grep -Eq '^[a-z0-9][a-z0-9.-]*\.[a-z0-9.-]*[a-z0-9]$' || die "ALLOWED_FQDNS contains an invalid exact hostname"
  case " $seen_fqdns " in *" $value "*) die "ALLOWED_FQDNS contains a duplicate" ;; esac
  seen_fqdns="${seen_fqdns:+$seen_fqdns }$value"
done
case " $DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS " in
  *" $DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME "*) ;;
  *) die "ALLOWED_FQDNS must contain MODEL_RELAY_HOSTNAME" ;;
esac

for suffix in CONTROL_TLS_CERT_FILE CONTROL_TLS_KEY_FILE MODEL_RELAY_TLS_CERT_FILE MODEL_RELAY_TLS_KEY_FILE IID_CERTIFICATE_FILE PRICING_CATALOG_FILE RUNTIME_QUALIFICATION_FILE; do
  req "DIREXTALK_CLOUD_WORKER_$suffix"
  eval "path=\${DIREXTALK_CLOUD_WORKER_$suffix}"
  [ -f "$path" ] && [ ! -L "$path" ] || die "$suffix must be a regular non-symlink file"
done
for suffix in CONTROL_TLS_KEY_FILE MODEL_RELAY_TLS_KEY_FILE; do
  eval "path=\${DIREXTALK_CLOUD_WORKER_$suffix}"
  [ "$(stat -c '%a:%u' "$path")" = 400:65532 ] || die "$suffix must be mode 0400 and UID 65532"
done

list_yaml() { for item in $2; do printf '    - %s\n' "$item"; done; }
umask 077
{
printf '%s\n' 'core_aws_enabled: true' 'core_execution_v2_enabled: true' 'core_cloud_worker:' '  enabled: true'
for pair in \
  region:REGION credential_id:CREDENTIAL_ID credential_revision:CREDENTIAL_REVISION \
  instance_type:INSTANCE_TYPE architecture:ARCHITECTURE root_device_name:ROOT_DEVICE_NAME volume_gib:VOLUME_GIB volume_type:VOLUME_TYPE volume_iops:VOLUME_IOPS volume_throughput_mib:VOLUME_THROUGHPUT_MIB \
  ami_id:AMI_ID ami_digest:AMI_DIGEST worker_release_digest:WORKER_RELEASE_DIGEST pi_runtime_digest:PI_RUNTIME_DIGEST host_network_policy_sha256:HOST_NETWORK_POLICY_SHA256 \
  vpc_id:VPC_ID subnet_id:SUBNET_ID outbound_proxy_trust_bundle_sha256:OUTBOUND_PROXY_TRUST_SHA256 artifact_bucket:ARTIFACT_BUCKET artifact_base_prefix:ARTIFACT_BASE_PREFIX artifact_kms_key_arn:ARTIFACT_KMS_KEY_ARN artifact_retention:ARTIFACT_RETENTION \
  worker_control_trust_bundle_sha256:WORKER_CONTROL_TRUST_SHA256 model_relay_trust_bundle_sha256:MODEL_RELAY_TRUST_SHA256 pricing_catalog_sha256:PRICING_CATALOG_SHA256 runtime_qualification_sha256:RUNTIME_QUALIFICATION_SHA256 \
  quote_ttl:QUOTE_TTL maximum_catalog_age:MAXIMUM_CATALOG_AGE contingency_basis_points:CONTINGENCY_BASIS_POINTS absolute_hard_limit_micros:ABSOLUTE_HARD_LIMIT_MICROS max_runtime:MAX_RUNTIME max_tokens:MAX_TOKENS max_output_bytes:MAX_OUTPUT_BYTES controller_poll_interval:CONTROLLER_POLL_INTERVAL worker_heartbeat_interval:WORKER_HEARTBEAT_INTERVAL reaper_interval:REAPER_INTERVAL completion_outbox_interval:COMPLETION_OUTBOX_INTERVAL; do
  key=${pair%%:*}; suffix=${pair#*:}; eval "value=\${DIREXTALK_CLOUD_WORKER_$suffix}"; printf '  %s: %s\n' "$key" "$value"
done
printf '  account_id: "%s"\n' "$DIREXTALK_CLOUD_WORKER_ACCOUNT_ID"
printf '%s\n' '  dns_resolver_cidrs:'
list_yaml dns "$DIREXTALK_CLOUD_WORKER_DNS_RESOLVER_CIDRS"
printf '%s\n' '  tls_proxy_cidrs:'
list_yaml proxy "$DIREXTALK_CLOUD_WORKER_TLS_PROXY_CIDRS"
printf '%s\n' '  allowed_fqdns:'
list_yaml fqdn "$DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS"
cat <<EOF
  outbound_proxy_url: https://$DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME:443
  outbound_proxy_server_name: $DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME
  worker_control_listen: ":10443"
  worker_control_endpoint: https://$DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME:443
  worker_control_server_name: $DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME
  worker_control_tls_cert_file: /run/cloud-worker/worker-control-cert.pem
  worker_control_tls_key_file: /run/cloud-worker/worker-control-key.pem
  worker_control_max_concurrent_rpc: 64
  model_relay_listen: ":11443"
  model_relay_endpoint: https://$DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME:443
  model_relay_server_name: $DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME
  model_relay_tls_cert_file: /run/cloud-worker/model-relay-cert.pem
  model_relay_tls_key_file: /run/cloud-worker/model-relay-key.pem
  iid_certificate_file: /run/cloud-worker/aws-iid-cert.pem
  pricing_catalog_file: /run/cloud-worker/pricing-catalog.json
  runtime_qualification_file: /run/cloud-worker/runtime-qualification.json
EOF
} >"$out"
chmod 0400 "$out"
