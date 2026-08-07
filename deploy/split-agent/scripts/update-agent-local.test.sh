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
shared_old_id=$(printf '5%.0s' {1..64})
old_oneshot_id=$(printf '4%.0s' {1..64})
old_ref=docker.io/dirextalk/agent@sha256:$old_digest
target_ref=docker.io/dirextalk/agent@sha256:$target_digest
message_ref=docker.io/dirextalk/message-server@sha256:$(printf '3%.0s' {1..64})
postgres_ref=docker.io/library/postgres:18@sha256:$(printf '7%.0s' {1..64})
utility_ref=docker.io/library/alpine:3.22@sha256:$(printf '8%.0s' {1..64})
qdrant_ref=docker.io/qdrant/qdrant:v1.18.3@sha256:$(printf '9%.0s' {1..64})
coturn_ref=docker.io/coturn/coturn:4.6.3-alpine@sha256:$(printf 'a%.0s' {1..64})
machine_id=$(tr -d '[:space:]' </etc/machine-id)

cat >"$tmp/bin/stop" <<'EOF'
#!/usr/bin/env bash
printf 'stop\n' >>"$FAKE_DOCKER_LOG"
[ "${FAKE_STOP_PARTIAL:-false}" != true ] || printf partial >"$FAKE_DOCKER_STATE"
exit "${FAKE_STOP_STATUS:-0}"
EOF
cat >"$tmp/bin/sync" <<'EOF'
#!/usr/bin/env bash
printf 'sync %s\n' "$*" >>"$FAKE_DOCKER_LOG"
if [ -n "${FAKE_SYNC_FAIL_MATCH:-}" ] && [[ "$*" == *"$FAKE_SYNC_FAIL_MATCH"* ]]; then exit 1; fi
exec /usr/bin/sync "$@"
EOF
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s|image=%s\n' "$*" "${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}" >>"$FAKE_DOCKER_LOG"
if [ "$1" = context ]; then printf 'unix:///run/docker.sock\n'; exit 0; fi
if [ "$1" = info ]; then printf '%s\n' 'test-engine'; exit 0; fi
if [ "$1 $2" = 'image rm' ]; then [ "${FAKE_IMAGE_RM_FAIL:-false}" != true ]; exit; fi
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
        *RepoTags*)
          printf '%s\n' 'docker.io/dirextalk/agent:v1.0.0' "$FAKE_OLD_REF"
          [ "${FAKE_SHARED_ALIAS:-false}" != true ] || printf '%s\n' 'other.example/shared/agent:keep'
          ;;
        *image.revision*) printf '%s\n' "$FAKE_OLD_REVISION" ;;
        *) printf '%s\n' "$FAKE_CURRENT_AGENT_VERSION" ;;
      esac
      ;;
    "$FAKE_OLD_REF")
      case "$format" in
        *'{{.Id}}'*) printf '%s\n' "$FAKE_OLD_IMAGE_ID" ;;
        *) printf '%s\n' "$FAKE_OLD_REVISION" ;;
      esac
      ;;
    docker.io/dirextalk/agent:v1.0.0)
      case "$format" in
        *'{{.Id}}'*)
          if [ "${FAKE_RETARGET_FIXED_REF:-false}" = true ]; then printf '%s\n' "$FAKE_TARGET_IMAGE_ID"; else printf '%s\n' "$FAKE_OLD_IMAGE_ID"; fi
          ;;
        *) exit 1 ;;
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
  id=$2; image=$FAKE_OLD_IMAGE_ID; ref=$FAKE_OLD_REF; service=agent
  case "$id" in
    "$FAKE_MESSAGE_ID") image=$FAKE_MESSAGE_IMAGE_ID; ref=$FAKE_MESSAGE_REF; service=message-server ;;
    "$FAKE_OLD_AGENT_ID") service=agent ;;
    "$FAKE_OLD_EXTENSION_ID") service=extension-runner ;;
    "$FAKE_OLD_CORE_ID") service=core-runner ;;
    "$FAKE_NEW_AGENT_ID") service=agent
      if [ "$(cat "$FAKE_DOCKER_STATE" 2>/dev/null || true)" = target ]; then image=$FAKE_TARGET_IMAGE_ID; ref=$FAKE_TARGET_REF; fi ;;
    "$FAKE_NEW_EXTENSION_ID") service=extension-runner
      if [ "$(cat "$FAKE_DOCKER_STATE" 2>/dev/null || true)" = target ]; then image=$FAKE_TARGET_IMAGE_ID; ref=$FAKE_TARGET_REF; fi ;;
    "$FAKE_NEW_CORE_ID") service=core-runner
      if [ "$(cat "$FAKE_DOCKER_STATE" 2>/dev/null || true)" = target ]; then image=$FAKE_TARGET_IMAGE_ID; ref=$FAKE_TARGET_REF; fi ;;
    "$FAKE_SHARED_OLD_ID")
      printf '[{"Id":"%s","Image":"%s","Config":{"Image":"%s","Labels":{"com.docker.compose.project":"other-stack","com.docker.compose.service":"agent"}},"State":{"Status":"running","Health":{"Status":"healthy"}}}]\n' "$id" "$image" "$ref"
      exit 0
      ;;
    "$FAKE_OLD_ONESHOT_ID")
      printf '[{"Id":"%s","Image":"%s","Config":{"Image":"%s","Labels":{"com.docker.compose.project":"d-abcdefghijklmnopqrstuvwxyz","com.docker.compose.service":"agent-migrate"}},"State":{"Status":"exited"}}]\n' "$id" "$image" "$ref"
      exit 0
      ;;
  esac
  printf '[{"Id":"%s","Image":"%s","Config":{"Image":"%s","Labels":{"com.docker.compose.project":"d-abcdefghijklmnopqrstuvwxyz","com.docker.compose.service":"%s"}},"State":{"Status":"running","Health":{"Status":"healthy"}}}]\n' "$id" "$image" "$ref" "$service"
  exit 0
fi
if [ "$1" = ps ]; then
  [ "${FAKE_OLD_ONESHOT:-false}" != true ] || printf '%s\n' "$FAKE_OLD_ONESHOT_ID"
  [ "${FAKE_SHARED_OLD_IMAGE:-false}" != true ] || printf '%s\n' "$FAKE_SHARED_OLD_ID"
  exit 0
fi
if [ "$1 $2" = 'container rm' ]; then exit 0; fi
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
  if [ "$command" = run ]; then
    service=${*: -1}
    case "$service" in
      extension-socket-init|extension-runner-storage-init|core-runner-socket-init|core-runner-storage-init)
        if [ "${FAKE_INIT_FAIL_SERVICE:-}" = "$service" ] &&
           { [ -z "${FAKE_INIT_FAIL_REF:-}" ] || [ "${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}" = "$FAKE_INIT_FAIL_REF" ]; }; then
          exit 1
        fi
        ;;
      agent-migrate) [ "${FAKE_MIGRATE_FAIL:-false}" != true ] || exit 1 ;;
      *) exit 1 ;;
    esac
    exit 0
  fi
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
cat >"$tmp/bin/prepare-runner-cgroups" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'prepare|stack=%s|image=%s\n' "$1" "${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}" >>"$FAKE_PREP_LOG"
if [ -n "${FAKE_PREP_FAIL_REF:-}" ] && [ "${DIREXTALK_AGENT_IMAGE_IMMUTABLE:-}" = "$FAKE_PREP_FAIL_REF" ]; then
  exit 1
fi
keys='DIREXTALK_EXTENSION_CGROUP_ROOT DIREXTALK_CORE_RUNNER_CGROUP_ROOT DIREXTALK_EXTENSION_CGROUP_PARENT DIREXTALK_CORE_RUNNER_CGROUP_PARENT DIREXTALK_CORE_EXTENSION_RUNNER_UID DIREXTALK_CORE_WORKLOAD_RUNNER_UID DIREXTALK_EXTENSION_RUNNER_UNIT DIREXTALK_CORE_RUNNER_UNIT DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH DIREXTALK_CORE_RUNNER_FRAGMENT_PATH DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256 DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256 DIREXTALK_RUNNER_APPARMOR_PROFILE DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256 DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256 DIREXTALK_RUNNER_PREP_HELPER_PATH DIREXTALK_RUNNER_PREP_HELPER_SHA256 DIREXTALK_RUNNER_PREP_MACHINE_ID DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID DIREXTALK_EXTENSION_CONTROL_GROUP DIREXTALK_CORE_RUNNER_CONTROL_GROUP DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE'
for key in $keys; do
  awk -F= -v wanted="$key" 'index($0,wanted "=")==1 {print; found=1; exit} END {if (!found) exit 1}' "$FAKE_RUNNER_ENV_FILE"
done
EOF
chmod +x "$tmp/bin/docker" "$tmp/bin/stop" "$tmp/bin/sync" "$tmp/bin/prepare-runner-cgroups" "$script"
prepare_hash=$(sha256sum "$tmp/bin/prepare-runner-cgroups" | awk '{print $1}')

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
DIREXTALK_RELEASE_CATALOG_ORIGIN=https://imadmin.dirextalk.ai
DIREXTALK_QDRANT_IMAGE_IMMUTABLE=$qdrant_ref
DIREXTALK_COTURN_IMAGE_IMMUTABLE=$coturn_ref
DIREXTALK_IMAGE_ATTESTATION_FILE=$root/image-attestation
DIREXTALK_EXTENSION_CGROUP_ROOT=/sys/fs/cgroup/extension
DIREXTALK_CORE_RUNNER_CGROUP_ROOT=/sys/fs/cgroup/core
DIREXTALK_EXTENSION_CGROUP_PARENT=d-test-extension.slice
DIREXTALK_CORE_RUNNER_CGROUP_PARENT=d-test-core.slice
DIREXTALK_CORE_EXTENSION_RUNNER_UID=65531
DIREXTALK_CORE_WORKLOAD_RUNNER_UID=65530
DIREXTALK_EXTENSION_RUNNER_UNIT=dirextalk-extension-runner@d-test.service
DIREXTALK_CORE_RUNNER_UNIT=dirextalk-core-runner@d-test.service
DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH=/etc/systemd/system/dirextalk-extension-runner@.service
DIREXTALK_CORE_RUNNER_FRAGMENT_PATH=/etc/systemd/system/dirextalk-core-runner@.service
DIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256=$(printf 'a%.0s' {1..64})
DIREXTALK_CORE_RUNNER_FRAGMENT_SHA256=$(printf 'b%.0s' {1..64})
DIREXTALK_RUNNER_APPARMOR_PROFILE=dirextalk-runner-userns
DIREXTALK_RUNNER_APPARMOR_PROFILE_PATH=/etc/apparmor.d/dirextalk-runner-userns
DIREXTALK_RUNNER_APPARMOR_PROFILE_SHA256=$(printf 'c%.0s' {1..64})
DIREXTALK_RUNNER_APPARMOR_MANAGER_PATH=/test/manage-runner-apparmor.sh
DIREXTALK_RUNNER_APPARMOR_MANAGER_SHA256=$(printf 'd%.0s' {1..64})
DIREXTALK_RUNNER_PREP_HELPER_PATH=$tmp/bin/prepare-runner-cgroups
DIREXTALK_RUNNER_PREP_HELPER_SHA256=$prepare_hash
DIREXTALK_RUNNER_PREP_MACHINE_ID=$(printf 'e%.0s' {1..32})
DIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID=test-engine
DIREXTALK_EXTENSION_CONTROL_GROUP=/d.slice/extension
DIREXTALK_CORE_RUNNER_CONTROL_GROUP=/d.slice/core
DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT=/sys/fs/cgroup/d.slice/extension
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT=/sys/fs/cgroup/d.slice/core
DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS=/sys/fs/cgroup/d.slice/extension/cgroup.procs
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS=/sys/fs/cgroup/d.slice/core/cgroup.procs
DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_OWNER=65531:65531
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_OWNER=65530:65530
DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS_MODE=644
DIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS_MODE=644
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
runner.machine_id=$machine_id
runner.docker_engine_id=test-engine
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
host.machine_id=$machine_id
docker.engine_id=test-engine
docker.context_endpoint=unix:///run/docker.sock
docker.context_socket=/run/docker.sock
container.count=5
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
container.4.id=$old_oneshot_id
container.4.name=agent-migrate
container.4.service=agent-migrate
container.4.project=d-abcdefghijklmnopqrstuvwxyz
EOF
  chmod 400 "$root/.cleanup-receipt"
  printf '%s\n' "$root"
}
rebind_env_receipt() {
  local stack_root=$1 env_hash
  env_hash=$(sha256sum "$stack_root/.env" | awk '{print $1}')
  sed -i \
    -e "s|^control.env_identity=.*|control.env_identity=$(stat -c '%d:%i:%u' "$stack_root/.env")|" \
    -e "s|^control.env_sha256=.*|control.env_sha256=$env_hash|" \
    "$stack_root/.cleanup-receipt"
  chmod 400 "$stack_root/.env" "$stack_root/.cleanup-receipt"
}
run_update() {
  PATH="$tmp/bin:$PATH" FAKE_DOCKER_LOG=$log FAKE_DOCKER_STATE=$state \
  FAKE_MESSAGE_ID=$message_id FAKE_MESSAGE_IMAGE_ID=$message_image_id FAKE_SERVER_VERSION="${FAKE_SERVER_VERSION:-v1.0.0}" \
  FAKE_MESSAGE_REF=$message_ref FAKE_MESSAGE_REVISION=$message_revision \
  FAKE_OLD_IMAGE_ID=$old_image_id FAKE_OLD_REF=$old_ref FAKE_OLD_REVISION=$old_revision FAKE_CURRENT_AGENT_VERSION=v1.0.0 \
  FAKE_TARGET_VERSION=v1.0.1 FAKE_TARGET_IMAGE_ID=$target_image_id FAKE_TARGET_REF=$target_ref FAKE_TARGET_REVISION=$target_revision \
  FAKE_OLD_AGENT_ID=$agent_id FAKE_OLD_EXTENSION_ID=$extension_id FAKE_OLD_CORE_ID=$core_id \
  FAKE_NEW_AGENT_ID=$new_agent_id FAKE_NEW_EXTENSION_ID=$new_extension_id FAKE_NEW_CORE_ID=$new_core_id \
  FAKE_SHARED_OLD_ID=$shared_old_id \
  FAKE_OLD_ONESHOT_ID=$old_oneshot_id \
  FAKE_RUNNER_ENV_FILE=$root/.env FAKE_PREP_LOG=$log \
  DIREXTALK_AGENT_UPDATE_TEST_FIXTURE=true DIREXTALK_AGENT_UPDATE_STOP_WRAPPER=$tmp/bin/stop \
  DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS=1 \
  "$script" "$@"
}
log_line() {
  local pattern=$1
  grep -n -F -m1 -- "$pattern" "$log" | cut -d: -f1
}
assert_before() {
  local first=$1 second=$2 first_line second_line
  first_line=$(log_line "$first")
  second_line=$(log_line "$second")
  [ "$first_line" -lt "$second_line" ] || { echo "expected '$first' before '$second'" >&2; exit 1; }
}
assert_failure_evidence() {
  local expected_phase=$1 files
  shopt -s nullglob
  files=("$root"/.agent-update-failure.*)
  shopt -u nullglob
  [ "${#files[@]}" -eq 1 ] || { echo "expected one Agent update failure record, got ${#files[@]}" >&2; exit 1; }
  [ "$(stat -c '%a' "${files[0]}")" = 400 ]
  grep -Fqx '# dirextalk-agent-update-failure-v1' "${files[0]}"
  grep -Fqx 'state=failed' "${files[0]}"
  grep -Fqx "phase=$expected_phase" "${files[0]}"
  grep -Fqx 'exit_status=1' "${files[0]}"
  grep -Fqx "target_version=v1.0.1" "${files[0]}"
  grep -Fqx "target_image=$target_ref" "${files[0]}"
  grep -Fqx "cleanup_receipt_sha256=$(sha256sum "$root/.cleanup-receipt" | awk '{print $1}')" "${files[0]}"
  # Assert the literal durability command in the wrapper.
  # shellcheck disable=SC2016
  grep -Fq 'sync -f "$evidence"' "$script"
}
assert_no_old_image_mutation() {
  if grep -Fq "image=$old_ref" "$log"; then
    echo 'one-way update attempted an old-image mutation' >&2
    exit 1
  fi
}

root=$(make_stack minimum-server)
before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if run_update "$root" v1.0.1 v1.1.0 >/dev/null 2>&1; then echo 'minimum server gate unexpectedly passed' >&2; exit 1; else status=$?; fi
[ "$status" -eq 3 ] || { echo "minimum server gate returned $status, want 3" >&2; exit 1; }
! grep -Eq '(^|\|)(pull|compose)( |\|)' "$log" || { echo 'minimum server gate performed Docker mutation' >&2; exit 1; }
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]

for origin_case in wrong missing; do
  root=$(make_stack "catalog-origin-$origin_case")
  if [ "$origin_case" = wrong ]; then
    sed -i 's#^DIREXTALK_RELEASE_CATALOG_ORIGIN=.*#DIREXTALK_RELEASE_CATALOG_ORIGIN=https://wrong.invalid#' "$root/.env"
  else
    sed -i '/^DIREXTALK_RELEASE_CATALOG_ORIGIN=/d' "$root/.env"
  fi
  rebind_env_receipt "$root"
  : >"$log"
  if run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
    echo "$origin_case release catalog origin unexpectedly passed" >&2; exit 1
  else
    status=$?
  fi
  [ "$status" -eq 1 ]
  if grep -Eq '(^|\|)(pull|compose)( |\|)' "$log"; then
    echo "$origin_case release catalog origin crossed the Docker mutation boundary" >&2; exit 1
  fi
done

root=$(make_stack concurrent-lock)
: >"$log"
: >"$root/.agent-update.lock"
chmod 600 "$root/.agent-update.lock"
exec 8<>"$root/.agent-update.lock"
flock -n 8
if run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'concurrent Agent updater unexpectedly acquired the held lock' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
if grep -Eq '(^|\|)(pull|compose)( |\|)' "$log"; then echo 'lock contention crossed the Docker mutation boundary' >&2; exit 1; fi
exec 8>&-

root=$(make_stack journal-durability-failure)
: >"$log"
if FAKE_SYNC_FAIL_MATCH=.agent-update.transaction. run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'Agent journal fsync failure unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
if grep -Fq 'stop' "$log"; then echo 'Agent journal fsync failure reached the stop boundary' >&2; exit 1; fi
[ ! -e "$root/.agent-update.transaction" ]

root=$(make_stack stop-expected-negative)
: >"$log"
if FAKE_STOP_STATUS=3 run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'Agent stop expected-negative unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 3 ]
[ ! -e "$root/.agent-update.transaction" ]

root=$(make_stack partial-stop-failure)
: >"$log"; : >"$state"
before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_STOP_STATUS=1 FAKE_STOP_PARTIAL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'partial Agent stop failure unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
[ "$(cat "$state")" = partial ]
[ -f "$root/.agent-update.transaction" ]
grep -Fqx 'state=prepared' "$root/.agent-update.transaction"
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
assert_failure_evidence runtime-stop

: >"$log"; root=$(make_stack success)
FAKE_OLD_ONESHOT=true run_update "$root" v1.0.1 v1.0.0 >/dev/null
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
grep -Fqx 'state=complete' "$root/.cleanup-receipt"
grep -Fq "image rm $old_ref" "$log"
grep -Fq 'image rm docker.io/dirextalk/agent:v1.0.0' "$log"
grep -Fq "container rm $old_oneshot_id" "$log"
assert_before "prepare|stack=d-abcdefghijklmnopqrstuvwxyz|image=$target_ref" 'run --rm --no-deps --pull never -T --interactive=false extension-socket-init'
assert_before 'run --rm --no-deps --pull never -T --interactive=false extension-socket-init' 'run --rm --no-deps --pull never -T --interactive=false extension-runner-storage-init'
assert_before 'run --rm --no-deps --pull never -T --interactive=false extension-runner-storage-init' 'run --rm --no-deps --pull never -T --interactive=false core-runner-socket-init'
assert_before 'run --rm --no-deps --pull never -T --interactive=false core-runner-socket-init' 'run --rm --no-deps --pull never -T --interactive=false core-runner-storage-init'
assert_before 'run --rm --no-deps --pull never -T --interactive=false core-runner-storage-init' 'run --rm --no-deps --pull never -T --interactive=false agent-migrate'

: >"$log"; : >"$state"; root=$(make_stack shared-old-image)
FAKE_SHARED_OLD_IMAGE=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1
grep -Fqx "container.1.id=$new_agent_id" "$root/.cleanup-receipt"
grep -Fq "image rm $old_ref" "$log"
if grep -Fq "image rm $old_image_id" "$log"; then echo 'shared old Agent image ID was removed' >&2; exit 1; fi

: >"$log"; : >"$state"; root=$(make_stack shared-old-alias)
FAKE_SHARED_ALIAS=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1
grep -Fq 'image rm docker.io/dirextalk/agent:v1.0.0' "$log"
if grep -Fq 'image rm other.example/shared/agent:keep' "$log"; then echo 'foreign Agent alias was removed' >&2; exit 1; fi
if grep -Fq "image rm $old_image_id" "$log"; then echo 'foreign-aliased Agent image ID was removed' >&2; exit 1; fi

: >"$log"; : >"$state"; root=$(make_stack retargeted-fixed-ref)
if FAKE_RETARGET_FIXED_REF=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'retargeted Agent repository ref unexpectedly passed cleanup' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
grep -Fqx 'state=cleanup-pending' "$root/.cleanup-receipt"
[ -f "$root/.agent-update.transaction" ]
if grep -Fq 'image rm docker.io/dirextalk/agent:v1.0.0' "$log"; then echo 'retargeted Agent repository ref was removed' >&2; exit 1; fi

: >"$log"; : >"$state"; root=$(make_stack old-image-cleanup-failure)
if FAKE_IMAGE_RM_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'old Agent image cleanup failure unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
grep -Fqx "DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref" "$root/.env"
grep -Fqx 'state=cleanup-pending' "$root/.cleanup-receipt"
grep -Fqx "container.1.id=$new_agent_id" "$root/.cleanup-receipt"
assert_failure_evidence old-image-cleanup
: >"$log"
if FAKE_STOP_STATUS=3 run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'unfinished Agent update evidence unexpectedly retried' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
if grep -Fq 'stop' "$log"; then echo 'unfinished Agent update reached stop wrapper' >&2; exit 1; fi

# Host cgroup preparation is a required infrastructure step. A target-side
# failure must stop before volume initialization without attempting the old
# image or publishing a replacement receipt.
: >"$log"; : >"$state"; root=$(make_stack cgroup-preparation-failure)
before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_PREP_FAIL_REF=$target_ref run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'runner cgroup preparation failure unexpectedly passed' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ] || { echo "runner cgroup preparation failure returned $status, want 1" >&2; exit 1; }
grep -Fq "prepare|stack=d-abcdefghijklmnopqrstuvwxyz|image=$target_ref" "$log"
assert_no_old_image_mutation
if grep -Fq "run --rm --no-deps --pull never -T --interactive=false extension-socket-init|image=$target_ref" "$log"; then
  echo 'failed cgroup preparation crossed the target volume initializer boundary' >&2
  exit 1
fi
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
assert_failure_evidence runner-preparation

# A one-shot initializer failure is an infrastructure failure. The target
# runners must not be recreated and no old-image recovery is attempted.
: >"$log"; : >"$state"; root=$(make_stack initializer-failure)
before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_INIT_FAIL_SERVICE=core-runner-socket-init FAKE_INIT_FAIL_REF=$target_ref run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'initializer infrastructure failure unexpectedly passed' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ] || { echo "initializer failure returned $status, want 1" >&2; exit 1; }
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
[ "$(grep -Fc 'run --rm --no-deps --pull never -T --interactive=false extension-socket-init' "$log")" -eq 1 ]
assert_no_old_image_mutation
if grep -Fq "up -d --no-deps --force-recreate --no-build --pull never extension-runner core-runner|image=$target_ref" "$log"; then
  echo 'initializer failure crossed the target runner recreation boundary' >&2
  exit 1
fi
grep -Fqx "container.1.id=$agent_id" "$root/.cleanup-receipt"
assert_failure_evidence volume-normalization

: >"$log"; root=$(make_stack pull-failure); before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_PULL_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then echo 'pull failure unexpectedly passed' >&2; exit 1; else status=$?; fi
[ "$status" -eq 1 ] || { echo "pull failure returned $status, want 1" >&2; exit 1; }
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
if grep -Fq 'compose ' "$log"; then echo 'pull failure crossed the Compose mutation boundary' >&2; exit 1; fi
shopt -s nullglob; failure_files=("$root"/.agent-update-failure.*); shopt -u nullglob
[ "${#failure_files[@]}" -eq 0 ] || { echo 'pre-mutation pull failure wrote a mutation failure record' >&2; exit 1; }

# Target start failure leaves the target-side state untouched, preserves the
# old receipt, and records the exact failed phase. No old image is started.
: >"$log"; : >"$state"; root=$(make_stack target-start-failure); before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_TARGET_AGENT_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then echo 'target health/start failure unexpectedly passed' >&2; exit 1; fi
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
assert_no_old_image_mutation
[ "$(cat "$state")" = target ]
grep -Fqx "container.1.id=$agent_id" "$root/.cleanup-receipt"
if grep -Fq "image rm $old_ref" "$log"; then echo 'failed Agent start cleaned the old image' >&2; exit 1; fi
assert_failure_evidence agent-start

# Inject failure after the new .env has replaced its path but before the new
# receipt is committed. The one-way updater keeps the target controls and old
# receipt mismatch fail closed, and records the failed commit phase.
: >"$log"; : >"$state"; root=$(make_stack receipt-commit-failure)
if DIREXTALK_AGENT_UPDATE_FAIL_RECEIPT_COMMIT=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'injected receipt commit failure unexpectedly passed' >&2
  exit 1
fi
grep -Fqx "DIREXTALK_AGENT_IMAGE_IMMUTABLE=$target_ref" "$root/.env"
grep -Fqx "container.1.id=$agent_id" "$root/.cleanup-receipt"
[ "$(cat "$state")" = target ]
assert_no_old_image_mutation
assert_failure_evidence control-commit

# The real update wrapper owns the post-update production image and topology
# gates before protected controls are committed. A gate failure preserves all
# protected controls and records the failure without old-image recovery.
: >"$log"; : >"$state"; root=$(make_stack production-gate-failure)
before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_PRODUCTION_GATE_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'production image gate infrastructure failure unexpectedly passed' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ] || { echo "production image gate failure returned $status, want 1" >&2; exit 1; }
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
assert_no_old_image_mutation
[ "$(cat "$state")" = target ]
grep -Fqx "container.1.id=$agent_id" "$root/.cleanup-receipt"
assert_failure_evidence production-gates

: >"$log"; : >"$state"; root=$(make_stack topology-gate-failure)
before=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")
if FAKE_TOPOLOGY_FAIL=true run_update "$root" v1.0.1 v1.0.0 >/dev/null 2>&1; then
  echo 'production topology gate failure unexpectedly passed' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ] || { echo "production topology gate failure returned $status, want 1" >&2; exit 1; }
[ "$before" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" "$root/.cleanup-receipt")" ]
assert_no_old_image_mutation
[ "$(cat "$state")" = target ]
grep -Fqx "container.1.id=$agent_id" "$root/.cleanup-receipt"
assert_failure_evidence production-gates

root=$(make_stack strict-arguments)
assert_usage() {
  if run_update "$@" >/dev/null 2>&1; then echo 'invalid arguments unexpectedly passed' >&2; exit 1; else status=$?; fi
  [ "$status" -eq 1 ] || { echo "invalid arguments returned $status, want 1" >&2; exit 1; }
}
assert_usage "$root" v1.0.1
assert_usage "$root" v1.0.1 v1.0.0 unexpected
assert_usage "$root" 1.0.1 v1.0.0
assert_usage "$root" v1.0.1 dev1.0.0

printf 'Agent one-way update attestation, production gates, expected-negative, infrastructure, and audit-failure paths verified\n'
