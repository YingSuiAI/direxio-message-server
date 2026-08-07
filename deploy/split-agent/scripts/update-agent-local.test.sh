#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/update-agent-local.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-agent-update.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
log=$tmp/docker.log; state=$tmp/docker.state
old_digest=$(printf '1%.0s' {1..64}); target_digest=$(printf '2%.0s' {1..64})
old_image_id=sha256:$(printf 'a%.0s' {1..64}); target_image_id=sha256:$(printf 'b%.0s' {1..64})
message_image_id=sha256:$(printf 'c%.0s' {1..64})
old_revision=$(printf '4%.0s' {1..40}); target_revision=$(printf '5%.0s' {1..40}); message_revision=$(printf '6%.0s' {1..40})
message_id=$(printf 'd%.0s' {1..64}); agent_id=$(printf 'e%.0s' {1..64}); extension_id=$(printf 'f%.0s' {1..64}); core_id=$(printf '9%.0s' {1..64})
new_agent_id=$(printf '8%.0s' {1..64}); new_extension_id=$(printf '7%.0s' {1..64}); new_core_id=$(printf '6%.0s' {1..64})
old_ref=docker.io/dirextalk/agent@sha256:$old_digest
target_ref=docker.io/dirextalk/agent@sha256:$target_digest
message_ref=docker.io/dirextalk/message-server@sha256:$(printf '3%.0s' {1..64})
postgres_ref=docker.io/library/postgres:18@sha256:$(printf '7%.0s' {1..64})
utility_ref=docker.io/library/alpine:3.22@sha256:$(printf '8%.0s' {1..64})
qdrant_ref=docker.io/qdrant/qdrant:v1.18.3@sha256:$(printf '9%.0s' {1..64})
coturn_ref=docker.io/coturn/coturn:4.6.3-alpine@sha256:$(printf 'a%.0s' {1..64})

cat >"$tmp/bin/stop" <<'EOF'
#!/usr/bin/env bash
exit "${FAKE_STOP_STATUS:-0}"
EOF
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s|image=%s\n' "$*" "${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}" >>"$FAKE_DOCKER_LOG"
if [ "$1 $2" = 'image inspect' ]; then
  format=${5:-}
  case "$3" in
    "$FAKE_MESSAGE_IMAGE_ID") printf '%s\n' "$FAKE_SERVER_VERSION" ;;
    "$FAKE_MESSAGE_REF")
      case "$format" in
        *Config.User*) printf '%s' '' ;;
        *) printf '%s\n' "$FAKE_MESSAGE_REVISION" ;;
      esac
      ;;
    "$FAKE_OLD_IMAGE_ID")
      case "$format" in
        *image.revision*) printf '%s\n' "$FAKE_OLD_REVISION" ;;
        *) printf '%s\n' "$FAKE_CURRENT_AGENT_VERSION" ;;
      esac
      ;;
    "$FAKE_TARGET_REF") printf '%s\n' "$FAKE_TARGET_REVISION" ;;
    docker.io/dirextalk/agent:*)
      [ "${FAKE_PULL_FAIL:-false}" != true ] || exit 1
      printf '%s|%s|%s|%s\n' "$FAKE_TARGET_VERSION" "$FAKE_TARGET_IMAGE_ID" "$FAKE_TARGET_REVISION" "$FAKE_TARGET_REF"
      ;;
    *) exit 1 ;;
  esac
  exit 0
fi
if [ "$1" = pull ]; then [ "${FAKE_PULL_FAIL:-false}" != true ]; exit; fi
if [ "$1" = inspect ]; then
  id=$2; image=$FAKE_OLD_IMAGE_ID; ref=$FAKE_OLD_REF
  case "$id" in
    "$FAKE_MESSAGE_ID") image=$FAKE_MESSAGE_IMAGE_ID; ref=$FAKE_MESSAGE_REF ;;
    "$FAKE_NEW_AGENT_ID"|"$FAKE_NEW_EXTENSION_ID"|"$FAKE_NEW_CORE_ID")
      if [ "$(cat "$FAKE_DOCKER_STATE" 2>/dev/null || true)" = target ]; then image=$FAKE_TARGET_IMAGE_ID; ref=$FAKE_TARGET_REF; fi ;;
  esac
  printf '[{"Id":"%s","Image":"%s","Config":{"Image":"%s"},"State":{"Status":"running","Health":{"Status":"healthy"}}}]\n' "$id" "$image" "$ref"
  exit 0
fi
if [ "$1" = compose ]; then
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in --env-file|-f|--project-name) shift 2;; *) break;; esac
  done
  command=$1; shift
  if [ "$command" = config ]; then
    agent_uid=65532:65532
    [ "${FAKE_TOPOLOGY_FAIL:-false}" != true ] || agent_uid=65531:65531
    cat <<JSON
{"services":{"agent":{"image":"$FAKE_TARGET_REF","user":"$agent_uid"},"extension-runner":{"image":"$FAKE_TARGET_REF","network_mode":"none","user":"65531:65531"},"core-runner":{"image":"$FAKE_TARGET_REF","network_mode":"none","user":"65530:65530"}}}
JSON
    exit 0
  fi
  if [ "$command" = ps ]; then
    [ "$1" = -q ]; service=$2
    case "$service" in agent) printf '%s\n' "$FAKE_NEW_AGENT_ID";; extension-runner) printf '%s\n' "$FAKE_NEW_EXTENSION_ID";; core-runner) printf '%s\n' "$FAKE_NEW_CORE_ID";; esac
    exit 0
  fi
  if [ "$command" = run ]; then [ "${FAKE_MIGRATE_FAIL:-false}" != true ]; exit; fi
  if [ "$command" = up ]; then
    case "${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}" in "$FAKE_TARGET_REF") printf target >"$FAKE_DOCKER_STATE";; *) printf old >"$FAKE_DOCKER_STATE";; esac
    if [ "${FAKE_TARGET_AGENT_FAIL:-false}" = true ] && [ "${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}" = "$FAKE_TARGET_REF" ] && [ "${*: -1}" = agent ]; then exit 1; fi
    exit 0
  fi
fi
if [ "$1" = run ]; then
  [ "${FAKE_PRODUCTION_GATE_FAIL:-false}" != true ] || exit 125
  entrypoint=''
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --rm) shift ;;
      --entrypoint) entrypoint=$2; shift 2 ;;
      *) shift; break ;;
    esac
  done
  case "$entrypoint" in
    /usr/local/bin/dirextalk-agent) printf '%s\n' 'usage: dirextalk-agent' >&2; exit 1 ;;
    /usr/local/bin/dirextalk-extension-runner) printf '%s\n' 'usage: dirextalk-extension-runner' >&2; exit 2 ;;
    /usr/local/bin/dirextalk-core-runner) printf '%s\n' 'usage: dirextalk-core-runner' >&2; exit 2 ;;
  esac
fi
exit 1
EOF
chmod +x "$tmp/bin/docker" "$tmp/bin/stop" "$script"

make_stack() {
  local name=$1 root env_identity env_sha manifest_identity manifest_sha attestation_identity attestation_sha
  root=$tmp/$name
  mkdir -m 700 "$root"
  cat >"$root/.env" <<EOF
DIREXTALK_SPLIT_STACK_NAME=d-abcdefghijklmnopqrstuvwxyz
DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=$postgres_ref
DIREXTALK_UTILITY_IMAGE_IMMUTABLE=$utility_ref
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$message_ref
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$old_ref
DIREXTALK_QDRANT_IMAGE_IMMUTABLE=$qdrant_ref
DIREXTALK_COTURN_IMAGE_IMMUTABLE=$coturn_ref
DIREXTALK_IMAGE_ATTESTATION_FILE=$root/image-attestation
EOF
  cat >"$root/image-attestation" <<EOF
# dirextalk-image-attestation-v2
capability_api_version=v1.0.3
capability_api_source=published
message_source_revision=$message_revision
agent_source_revision=$old_revision
image.DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=$postgres_ref
image.DIREXTALK_UTILITY_IMAGE_IMMUTABLE=$utility_ref
image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$message_ref
image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=$old_ref
image.DIREXTALK_QDRANT_IMAGE_IMMUTABLE=$qdrant_ref
image.DIREXTALK_COTURN_IMAGE_IMMUTABLE=$coturn_ref
EOF
  chmod 400 "$root/.env" "$root/image-attestation"
  attestation_identity=$(stat -c '%d:%i:%u' "$root/image-attestation")
  attestation_sha=$(sha256sum "$root/image-attestation" | awk '{print $1}')
  cat >"$root/.manifest" <<EOF
# dirextalk-split-manifest-v1
stack_name=d-abcdefghijklmnopqrstuvwxyz
compose_mode=production
image_attestation_path=$root/image-attestation
image_attestation_device=${attestation_identity%%:*}
image_attestation_inode=$(stat -c '%i' "$root/image-attestation")
image_attestation_uid=$(stat -c '%u' "$root/image-attestation")
image_attestation_sha256=$attestation_sha
EOF
  chmod 400 "$root/.manifest"
  env_identity=$(stat -c '%d:%i:%u' "$root/.env"); env_sha=$(sha256sum "$root/.env"|awk '{print $1}')
  manifest_identity=$(stat -c '%d:%i:%u' "$root/.manifest"); manifest_sha=$(sha256sum "$root/.manifest"|awk '{print $1}')
  cat >"$root/.cleanup-receipt" <<EOF
# dirextalk-split-cleanup-receipt-v1
state=complete
stack_name=d-abcdefghijklmnopqrstuvwxyz
control.env_identity=$env_identity
control.manifest_identity=$manifest_identity
control.env_sha256=$env_sha
control.manifest_sha256=$manifest_sha
container.count=4
container.0.id=$message_id
container.0.name=message
container.0.service=message-server
container.0.project=d-abcdefghijklmnopqrstuvwxyz
container.1.id=$agent_id
container.1.name=agent
container.1.service=agent
container.1.project=d-abcdefghijklmnopqrstuvwxyz
container.2.id=$extension_id
container.2.name=extension
container.2.service=extension-runner
container.2.project=d-abcdefghijklmnopqrstuvwxyz
container.3.id=$core_id
container.3.name=core
container.3.service=core-runner
container.3.project=d-abcdefghijklmnopqrstuvwxyz
EOF
  chmod 400 "$root/.cleanup-receipt"
  printf '%s\n' "$root"
}
run_update() {
  PATH="$tmp/bin:$PATH" FAKE_DOCKER_LOG=$log FAKE_DOCKER_STATE=$state \
  FAKE_MESSAGE_ID=$message_id FAKE_MESSAGE_IMAGE_ID=$message_image_id FAKE_SERVER_VERSION="${FAKE_SERVER_VERSION:-v1.0.0}" \
  FAKE_MESSAGE_REF=$message_ref FAKE_MESSAGE_REVISION=$message_revision \
  FAKE_OLD_IMAGE_ID=$old_image_id FAKE_OLD_REF=$old_ref FAKE_OLD_REVISION=$old_revision FAKE_CURRENT_AGENT_VERSION=v1.0.0 \
  FAKE_TARGET_VERSION=v1.0.1 FAKE_TARGET_IMAGE_ID=$target_image_id FAKE_TARGET_REF=$target_ref FAKE_TARGET_REVISION=$target_revision \
  FAKE_NEW_AGENT_ID=$new_agent_id FAKE_NEW_EXTENSION_ID=$new_extension_id FAKE_NEW_CORE_ID=$new_core_id \
  DIREXTALK_AGENT_UPDATE_TEST_FIXTURE=true DIREXTALK_AGENT_UPDATE_STOP_WRAPPER=$tmp/bin/stop \
  DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS=1 \
  "$script" "$@"
}

root=$(make_stack minimum-server)
before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if run_update "$root" v1.0.1 v1.1.0 >/dev/null 2>&1; then echo 'minimum server gate unexpectedly passed' >&2; exit 1; else status=$?; fi
[ "$status" -eq 3 ] || { echo "minimum server gate returned $status, want 3" >&2; exit 1; }
! grep -Eq '(^|\|)(pull|compose)( |\|)' "$log" || { echo 'minimum server gate performed Docker mutation' >&2; exit 1; }
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]

: >"$log"; root=$(make_stack success)
run_update "$root" v1.0.1 v1.0.0 >/dev/null
grep -Fqx "DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref" "$root/.env"
grep -Fqx "agent_source_revision=$target_revision" "$root/image-attestation"
grep -Fqx "image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref" "$root/image-attestation"
grep -Fqx "image_attestation_device=$(stat -c '%d' "$root/image-attestation")" "$root/.manifest"
grep -Fqx "image_attestation_inode=$(stat -c '%i' "$root/image-attestation")" "$root/.manifest"
grep -Fqx "image_attestation_sha256=$(sha256sum "$root/image-attestation" | awk '{print $1}')" "$root/.manifest"
grep -Fq "image inspect $target_ref" "$log"
grep -Fq 'config --format json' "$log"
grep -Fq "container.1.id=$new_agent_id" "$root/.cleanup-receipt"
grep -Fq "container.2.id=$new_extension_id" "$root/.cleanup-receipt"
grep -Fq "container.3.id=$new_core_id" "$root/.cleanup-receipt"

: >"$log"; root=$(make_stack pull-failure); before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_PULL_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then echo 'pull failure unexpectedly passed' >&2; exit 1; else status=$?; fi
[ "$status" -eq 1 ] || { echo "pull failure returned $status, want 1" >&2; exit 1; }
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
if grep -Fq 'compose ' "$log"; then echo 'pull failure crossed the Compose mutation boundary' >&2; exit 1; fi

: >"$log"; : >"$state"; root=$(make_stack rollback); before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_TARGET_AGENT_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then echo 'target health/start failure unexpectedly passed' >&2; exit 1; fi
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
grep -Fq "image=$old_ref" "$log"
[ "$(cat "$state")" = old ]

# Inject failure after the new .env has replaced its path but before the new
# receipt is committed. The transaction trap must restore the exact original
# control-file identities as well as the previous runtime image.
: >"$log"; : >"$state"; root=$(make_stack receipt-commit-failure)
before=$(stat -c '%d:%i:%u' "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt"; sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if DIREXTALK_AGENT_UPDATE_FAIL_RECEIPT_COMMIT=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'injected receipt commit failure unexpectedly passed' >&2
  exit 1
fi
after=$(stat -c '%d:%i:%u' "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt"; sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
[ "$before" = "$after" ] || { echo 'receipt commit failure did not restore exact protected control files' >&2; exit 1; }
grep -Fq "image=$old_ref" "$log"
[ "$(cat "$state")" = old ]

# The real update wrapper owns the post-update production image and topology
# gates. A Docker infrastructure failure in that consumer path must restore
# the exact four protected controls and the previous three-container runtime.
: >"$log"; : >"$state"; root=$(make_stack production-gate-failure)
before=$(stat -c '%d:%i:%u' "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt"; sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_PRODUCTION_GATE_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'production image gate infrastructure failure unexpectedly passed' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ] || { echo "production image gate failure returned $status, want 1" >&2; exit 1; }
after=$(stat -c '%d:%i:%u' "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt"; sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
[ "$before" = "$after" ] || { echo 'production gate failure did not restore exact protected controls' >&2; exit 1; }
grep -Fq "image=$old_ref" "$log"
[ "$(cat "$state")" = old ]

: >"$log"; : >"$state"; root=$(make_stack topology-gate-failure)
before=$(stat -c '%d:%i:%u' "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt"; sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_TOPOLOGY_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'production topology gate failure unexpectedly passed' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ] || { echo "production topology gate failure returned $status, want 1" >&2; exit 1; }
after=$(stat -c '%d:%i:%u' "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt"; sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
[ "$before" = "$after" ] || { echo 'topology gate failure did not restore exact protected controls' >&2; exit 1; }
grep -Fq "image=$old_ref" "$log"
[ "$(cat "$state")" = old ]

root=$(make_stack strict-arguments)
assert_usage() {
  if run_update "$@" >/dev/null 2>&1; then echo 'invalid arguments unexpectedly passed' >&2; exit 1; else status=$?; fi
  [ "$status" -eq 2 ] || { echo "invalid arguments returned $status, want 2" >&2; exit 1; }
}
assert_usage "$root" v1.0.1
assert_usage "$root" v1.0.1 v1.0.0 unexpected
assert_usage "$root" 1.0.1 v1.0.0
assert_usage "$root" v1.0.1 dev1.0.0

printf 'Agent update attestation, production gates, expected-negative, infrastructure, and rollback paths verified\n'
