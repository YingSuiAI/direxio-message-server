#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ENV_FILE ATTESTATION_FILE" >&2
  exit 2
}

die() {
  echo "production image gate: $*" >&2
  exit 1
}

[ "$#" -eq 2 ] || usage
env_file=$1
attestation_file=$2
[ -f "$env_file" ] && [ ! -L "$env_file" ] || die "environment file must be a regular non-symlink file"
[ -f "$attestation_file" ] && [ ! -L "$attestation_file" ] || die "attestation file must be a regular non-symlink file"
[ "$(stat -c '%a' "$env_file")" = 400 ] || die "environment file must be mode 0400"
[ "$(stat -c '%a' "$attestation_file")" = 400 ] || die "attestation file must be mode 0400"

read_env_value() {
  local key=$1 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$env_file")
  [ "$count" -eq 1 ] || die "environment file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$env_file")
  [ -n "$value" ] || die "$key is empty"
  printf '%s' "$value"
}

read_attestation_value() {
  local key=$1 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$attestation_file")
  [ "$count" -eq 1 ] || die "attestation must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$attestation_file")
  [ -n "$value" ] || die "$key is empty"
  printf '%s' "$value"
}

validate_image_ref() {
  local name=$1 value=$2 digest
  printf '%s\n' "$value" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' || die "$name must be a lowercase immutable digest reference"
  case "$value" in
    *registry.invalid*|*0000000000000000000000000000000000000000000000000000000000000000) die "$name is a placeholder or untrusted registry reference" ;;
  esac
  digest=${value##*@sha256:}
  [ "${digest//0/}" ] || die "$name uses an all-zero digest"
}

image_keys=(
  DIREXTALK_POSTGRES_IMAGE_IMMUTABLE
  DIREXTALK_UTILITY_IMAGE_IMMUTABLE
  DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE
  DIREXTALK_AGENT_IMAGE_IMMUTABLE
  DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE
  DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE
  DIREXTALK_QDRANT_IMAGE_IMMUTABLE
)

[ "$(sed -n '1p' "$attestation_file")" = '# dirextalk-image-attestation-v1' ] || die "attestation header is invalid"
attestation_version=$(read_attestation_value capability_api_version)
[ "$attestation_version" = v1.0.1 ] || die "unsupported capability API attestation version: $attestation_version"
source_revision=$(read_attestation_value source_revision)
printf '%s\n' "$source_revision" | grep -Eq '^[A-Za-z0-9._/-]+$' || die "source_revision contains unsafe characters"
capability_api_source=$(read_attestation_value capability_api_source)
case "$capability_api_source" in
  published) ;;
  local-relative-replace)
    die "capability-api v1.0.1 remote publication is pending; local relative replace cannot pass the production image gate" ;;
  *)
    die "capability_api_source must be published (local development is not a production release)" ;;
esac

for key in "${image_keys[@]}"; do
  env_value=$(read_env_value "$key")
  validate_image_ref "$key" "$env_value"
  attestation_value=$(read_attestation_value "image.$key")
  [ "$attestation_value" = "$env_value" ] || die "$key does not match its signed image attestation"
done

awk -F= '
  NR == 1 && $0 == "# dirextalk-image-attestation-v1" { next }
  /^[[:space:]]*#/ { next }
  /^[[:space:]]*$/ { next }
  $1 !~ /^(capability_api_version|source_revision|capability_api_source|image\.DIREXTALK_(POSTGRES|UTILITY|MESSAGE_SERVER|AGENT|EXTENSION_RUNNER|CORE_RUNNER|QDRANT)_IMAGE_IMMUTABLE)$/ { exit 1 }
' "$attestation_file" || die "attestation contains an unknown or malformed field"

printf 'production immutable image and attestation checks passed\n'
