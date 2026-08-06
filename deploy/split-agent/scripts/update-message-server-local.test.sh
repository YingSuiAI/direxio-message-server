#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/update-message-server-local.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-message-update.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

log=$tmp/docker.log
state=$tmp/docker.state
old_digest=$(printf '1%.0s' {1..64})
target_digest=$(printf '2%.0s' {1..64})
old_image_id=sha256:$(printf 'a%.0s' {1..64})
target_image_id=sha256:$(printf 'b%.0s' {1..64})
old_message_id=$(printf 'c%.0s' {1..64})
new_message_id=$(printf 'd%.0s' {1..64})
agent_id=$(printf 'e%.0s' {1..64})
postgres_id=$(printf 'f%.0s' {1..64})
old_ref=docker.io/dirextalk/message-server@sha256:$old_digest
target_ref=docker.io/dirextalk/message-server@sha256:$target_digest
old_revision=$(printf '4%.0s' {1..40})
target_revision=$(printf '5%.0s' {1..40})
machine_id=$(tr -d '[:space:]' </etc/machine-id)

cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s|image=%s\n' "$*" "${DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE:-}" >>"$FAKE_DOCKER_LOG"
case "$1" in
  context)
    printf 'unix:///run/docker.sock\n'
    ;;
  info)
    printf '%s\n' "$FAKE_ENGINE_ID"
    ;;
  pull)
    [ "${FAKE_PULL_FAIL:-false}" != true ]
    ;;
  image)
    [ "$2" = inspect ]
    if [ "${5:-}" = '{{index .Config.Labels "org.opencontainers.image.revision"}}' ]; then
      case "$3" in
        "$FAKE_OLD_REF") printf '%s\n' "$FAKE_OLD_REVISION" ;;
        "$FAKE_TARGET_REF") printf '%s\n' "$FAKE_TARGET_REVISION" ;;
        *) exit 1 ;;
      esac
      exit 0
    fi
    if [ "${5:-}" = '{{index .Config.Labels "org.opencontainers.image.version"}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}' ]; then
      case "$3" in
        "$FAKE_OLD_REF") printf '%s|%s|%s\n' "$FAKE_OLD_VERSION" "$FAKE_OLD_IMAGE_ID" "$FAKE_OLD_REVISION" ;;
        "$FAKE_TARGET_REF") printf '%s|%s|%s\n' "$FAKE_TARGET_VERSION" "$FAKE_TARGET_IMAGE_ID" "$FAKE_TARGET_REVISION" ;;
        *) exit 1 ;;
      esac
      exit 0
    fi
    case "$3" in
      "$FAKE_OLD_REF") printf '%s|%s|%s|%s\n' "$FAKE_OLD_VERSION" "$FAKE_OLD_IMAGE_ID" "$FAKE_OLD_REVISION" "$FAKE_OLD_REF" ;;
      "$FAKE_TARGET_REF"|docker.io/dirextalk/message-server:*)
        printf '%s|%s|%s|%s\n' "$FAKE_TARGET_VERSION" "$FAKE_TARGET_IMAGE_ID" "$FAKE_TARGET_REVISION" "$FAKE_TARGET_REF"
        ;;
      *) exit 1 ;;
    esac
    ;;
  inspect)
    id=$2
    if [ "$id" = "$FAKE_OLD_MESSAGE_ID" ]; then
      [ "$(cat "$FAKE_DOCKER_STATE" 2>/dev/null || true)" != target ] || exit 1
      image=$FAKE_OLD_IMAGE_ID; ref=$FAKE_OLD_REF
    elif [ "$id" = "$FAKE_NEW_MESSAGE_ID" ]; then
      if [ "$(cat "$FAKE_DOCKER_STATE" 2>/dev/null || true)" = target ]; then
        image=$FAKE_TARGET_IMAGE_ID; ref=$FAKE_TARGET_REF
      else
        image=$FAKE_OLD_IMAGE_ID; ref=$FAKE_OLD_REF
      fi
    else
      exit 1
    fi
    health=healthy
    [ "${FAKE_TARGET_UNHEALTHY:-false}" != true ] || [ "$image" != "$FAKE_TARGET_IMAGE_ID" ] || health=unhealthy
    printf '[{"Id":"%s","Image":"%s","Name":"/message","Config":{"Image":"%s","Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"message-server"}},"State":{"Status":"running","Health":{"Status":"%s"}}}]\n' \
      "$id" "$image" "$ref" "$FAKE_STACK" "$health"
    ;;
  ps)
    printf '%s\n' "$FAKE_NEW_MESSAGE_ID"
    ;;
  compose)
    shift
    files=''
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --env-file|--project-name) shift 2 ;;
        -f) files="$files $2"; shift 2 ;;
        *) break ;;
      esac
    done
    case "$1" in
      config) exit 0 ;;
      up)
        shift
        [ "${*: -1}" = message-server ]
        args=" $* "
        case "$args" in *' --no-deps '*) ;; *) exit 41 ;; esac
        case "$args" in *' --force-recreate '*) ;; *) exit 41 ;; esac
        case "$args" in *' --no-build '*) ;; *) exit 41 ;; esac
        case "$args" in *' --pull never '*) ;; *) exit 41 ;; esac
        case "$files" in *'/compose.yaml'*) ;; *) exit 42 ;; esac
        case "$files" in *'/compose.production.yaml'*) ;; *) exit 42 ;; esac
        case "${DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE:-}" in
          "$FAKE_TARGET_REF") printf target >"$FAKE_DOCKER_STATE" ;;
          "$FAKE_OLD_REF") printf old >"$FAKE_DOCKER_STATE" ;;
          *) exit 43 ;;
        esac
        ;;
      ps)
        [ "$2" = -q ] && [ "$3" = message-server ]
        printf '%s\n' "$FAKE_NEW_MESSAGE_ID"
        ;;
      *) exit 1 ;;
    esac
    ;;
  *) exit 1 ;;
esac
EOF
chmod 755 "$tmp/bin/docker"

make_stack() {
  local name=$1 include_alias=${2:-true} root env_identity env_sha manifest_identity manifest_sha attestation_identity attestation_sha
  root=$tmp/$name
  mkdir -m 700 "$root"
  {
    printf 'DIREXTALK_SPLIT_STACK_NAME=d-abcdefghijklmnopqrstuvwxyz\n'
    [ "$include_alias" != true ] || printf 'MESSAGE_SERVER_IMAGE=%s\n' "$old_ref"
    printf 'DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=%s\n' "$old_ref"
    printf 'DIREXTALK_AGENT_IMAGE_IMMUTABLE=docker.io/dirextalk/agent@sha256:%s\n' "$(printf '3%.0s' {1..64})"
    printf 'DIREXTALK_IMAGE_ATTESTATION_FILE=%s/image-attestation\n' "$root"
  } >"$root/.env"
  cat >"$root/image-attestation" <<EOF
# dirextalk-image-attestation-v2
capability_api_version=v1.0.3
capability_api_source=published
message_source_revision=$old_revision
agent_source_revision=$(printf '6%.0s' {1..40})
image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$old_ref
image.DIREXTALK_AGENT_IMAGE_IMMUTABLE=docker.io/dirextalk/agent@sha256:$(printf '3%.0s' {1..64})
EOF
  chmod 400 "$root/image-attestation"
  attestation_identity=$(stat -c '%d:%i:%u' "$root/image-attestation")
  attestation_sha=$(sha256sum "$root/image-attestation" | awk '{print $1}')
  cat >"$root/.manifest" <<EOF
# dirextalk-split-manifest-v1
stack_name=d-abcdefghijklmnopqrstuvwxyz
compose_mode=production
runner.machine_id=$machine_id
runner.docker_engine_id=engine-message-update-test
image_attestation_path=$root/image-attestation
image_attestation_device=${attestation_identity%%:*}
image_attestation_inode=$(stat -c '%i' "$root/image-attestation")
image_attestation_uid=$(id -u)
image_attestation_sha256=$attestation_sha
EOF
  chmod 400 "$root/.env" "$root/.manifest"
  env_identity=$(stat -c '%d:%i:%u' "$root/.env")
  env_sha=$(sha256sum "$root/.env" | awk '{print $1}')
  manifest_identity=$(stat -c '%d:%i:%u' "$root/.manifest")
  manifest_sha=$(sha256sum "$root/.manifest" | awk '{print $1}')
  cat >"$root/.cleanup-receipt" <<EOF
# dirextalk-split-cleanup-receipt-v1
state=complete
stack_name=d-abcdefghijklmnopqrstuvwxyz
control.env_identity=$env_identity
control.manifest_identity=$manifest_identity
control.env_sha256=$env_sha
control.manifest_sha256=$manifest_sha
host.machine_id=$machine_id
docker.engine_id=engine-message-update-test
docker.context_endpoint=unix:///run/docker.sock
docker.context_socket=/run/docker.sock
container.count=3
container.0.id=$old_message_id
container.0.name=message
container.0.service=message-server
container.0.project=d-abcdefghijklmnopqrstuvwxyz
container.1.id=$agent_id
container.1.name=agent
container.1.service=agent
container.1.project=d-abcdefghijklmnopqrstuvwxyz
container.2.id=$postgres_id
container.2.name=postgres
container.2.service=message-postgres
container.2.project=d-abcdefghijklmnopqrstuvwxyz
EOF
  chmod 400 "$root/.cleanup-receipt"
  printf '%s\n' "$root"
}

run_update() {
  PATH="$tmp/bin:$PATH" \
  FAKE_DOCKER_LOG=$log FAKE_DOCKER_STATE=$state FAKE_ENGINE_ID=engine-message-update-test \
  FAKE_STACK=d-abcdefghijklmnopqrstuvwxyz FAKE_OLD_MESSAGE_ID=$old_message_id FAKE_NEW_MESSAGE_ID=$new_message_id \
  FAKE_OLD_REF=$old_ref FAKE_OLD_IMAGE_ID=$old_image_id FAKE_OLD_VERSION=v1.0.0 \
  FAKE_OLD_REVISION=$old_revision FAKE_TARGET_REVISION=$target_revision \
  FAKE_TARGET_REF=$target_ref FAKE_TARGET_IMAGE_ID=$target_image_id FAKE_TARGET_VERSION=v1.0.1 \
  DIREXTALK_MESSAGE_SERVER_UPDATE_TEST_FIXTURE=true \
  DIREXTALK_MESSAGE_SERVER_UPDATE_HEALTH_ATTEMPTS=1 \
  "$script" "$@"
}

[ -x "$script" ] || { echo 'update-message-server-local.sh must be executable' >&2; exit 1; }

root=$(make_stack not-newer)
: >"$log"
before=$(sha256sum "$root/.env" "$root/.cleanup-receipt")
if run_update "$root" v1.0.0 >/dev/null 2>&1; then
  echo 'non-newer target unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 3 ] || { echo "non-newer target returned $status, want 3" >&2; exit 1; }
if grep -Eq '(^|\|)(pull|compose)( |\|)' "$log"; then
  echo 'non-newer target crossed the Docker mutation boundary' >&2; exit 1
fi
[ "$before" = "$(sha256sum "$root/.env" "$root/.cleanup-receipt")" ]

root=$(make_stack success false)
: >"$log"; : >"$state"
run_update "$root" v1.0.1 >/dev/null
grep -Fqx "MESSAGE_SERVER_IMAGE=$target_ref" "$root/.env"
grep -Fqx "DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$target_ref" "$root/.env"
grep -Fqx "message_source_revision=$target_revision" "$root/image-attestation"
grep -Fqx "image.DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$target_ref" "$root/image-attestation"
grep -Fqx "image_attestation_device=$(stat -c '%d' "$root/image-attestation")" "$root/.manifest"
grep -Fqx "image_attestation_inode=$(stat -c '%i' "$root/image-attestation")" "$root/.manifest"
grep -Fqx "image_attestation_uid=$(stat -c '%u' "$root/image-attestation")" "$root/.manifest"
grep -Fqx "image_attestation_sha256=$(sha256sum "$root/image-attestation" | awk '{print $1}')" "$root/.manifest"
grep -Fqx "control.env_identity=$(stat -c '%d:%i:%u' "$root/.env")" "$root/.cleanup-receipt"
grep -Fqx "control.env_sha256=$(sha256sum "$root/.env" | awk '{print $1}')" "$root/.cleanup-receipt"
grep -Fqx "control.manifest_identity=$(stat -c '%d:%i:%u' "$root/.manifest")" "$root/.cleanup-receipt"
grep -Fqx "control.manifest_sha256=$(sha256sum "$root/.manifest" | awk '{print $1}')" "$root/.cleanup-receipt"
grep -Fq "container.0.id=$new_message_id" "$root/.cleanup-receipt"
grep -Fq "container.1.id=$agent_id" "$root/.cleanup-receipt"
grep -Fq "container.2.id=$postgres_id" "$root/.cleanup-receipt"
[ "$(cat "$state")" = target ]
[ "$(stat -c '%u:%g:%a' "$root/.message-server-update.lock")" = "$(id -u):$(id -g):600" ]
if grep -Eq 'up .* (agent|message-postgres)( |$)' "$log"; then
  echo 'message-server update mutated Agent or PostgreSQL' >&2; exit 1
fi

root=$(make_stack pull-failure)
: >"$log"; : >"$state"
before=$(sha256sum "$root/.env" "$root/.cleanup-receipt")
if FAKE_PULL_FAIL=true run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'pull failure unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
[ "$before" = "$(sha256sum "$root/.env" "$root/.cleanup-receipt")" ]
if grep -Eq 'compose .* up ' "$log"; then
  echo 'pull failure crossed the Compose mutation boundary' >&2; exit 1
fi

root=$(make_stack hard-kill-resume)
: >"$log"; : >"$state"
if DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_RECREATE=true \
   run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'hard-kill injection unexpectedly returned success' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 137 ] || { echo "hard-kill injection returned $status, want 137" >&2; exit 1; }
[ -f "$root/.message-server-update.transaction" ]
grep -Fqx 'state=prepared' "$root/.message-server-update.transaction"
run_update "$root" v1.0.1 >/dev/null
[ ! -e "$root/.message-server-update.transaction" ]
grep -Fqx "DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$target_ref" "$root/.env"
grep -Fqx "message_source_revision=$target_revision" "$root/image-attestation"
grep -Fq "container.0.id=$new_message_id" "$root/.cleanup-receipt"

root=$(make_stack hard-kill-target-mismatch)
: >"$log"; : >"$state"
if DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_RECREATE=true \
   run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'target-mismatch hard-kill injection unexpectedly returned success' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 137 ]
journal_before=$(sha256sum "$root/.message-server-update.transaction" | awk '{print $1}')
if run_update "$root" v1.0.2 >/dev/null 2>&1; then
  echo 'different resume target unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
[ "$journal_before" = "$(sha256sum "$root/.message-server-update.transaction" | awk '{print $1}')" ]
[ "$(cat "$state")" = target ]
run_update "$root" v1.0.1 >/dev/null
[ ! -e "$root/.message-server-update.transaction" ]

root=$(make_stack hard-kill-partial-control-resume)
: >"$log"; : >"$state"
if DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_CONTROL_COMMIT=manifest \
   run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'partial-control hard-kill injection unexpectedly returned success' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 137 ]
grep -Fqx 'state=target-ready' "$root/.message-server-update.transaction"
run_update "$root" v1.0.1 >/dev/null
[ ! -e "$root/.message-server-update.transaction" ]
grep -Fqx "DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$target_ref" "$root/.env"
grep -Fqx "message_source_revision=$target_revision" "$root/image-attestation"
grep -Fq "container.0.id=$new_message_id" "$root/.cleanup-receipt"

root=$(make_stack hard-kill-rollback)
: >"$log"; : >"$state"
if DIREXTALK_MESSAGE_SERVER_UPDATE_HARD_KILL_AFTER_RECREATE=true \
   run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'hard-kill rollback injection unexpectedly returned success' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 137 ]
if FAKE_TARGET_UNHEALTHY=true run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'unhealthy interrupted target unexpectedly completed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
[ "$(cat "$state")" = old ]
[ ! -e "$root/.message-server-update.transaction" ]
grep -Fqx "DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$old_ref" "$root/.env"
grep -Fqx "message_source_revision=$old_revision" "$root/image-attestation"
grep -Fq "container.0.id=$new_message_id" "$root/.cleanup-receipt"

root=$(make_stack health-rollback)
: >"$log"; : >"$state"
before_env=$(sha256sum "$root/.env" | awk '{print $1}')
if FAKE_TARGET_UNHEALTHY=true run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'unhealthy target unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
[ "$(cat "$state")" = old ]
[ "$before_env" = "$(sha256sum "$root/.env" | awk '{print $1}')" ]
grep -Fq "container.0.id=$new_message_id" "$root/.cleanup-receipt"
grep -Fq "container.1.id=$agent_id" "$root/.cleanup-receipt"
grep -Fq "container.2.id=$postgres_id" "$root/.cleanup-receipt"
grep -Fq "image=$old_ref" "$log"

for failed_control in attestation manifest env receipt; do
  root=$(make_stack "$failed_control-commit-rollback")
  : >"$log"; : >"$state"
  before_controls=$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" | awk '{print $1}')
  if DIREXTALK_MESSAGE_SERVER_UPDATE_FAIL_CONTROL_COMMIT=$failed_control \
     run_update "$root" v1.0.1 >/dev/null 2>&1; then
    echo "$failed_control commit failure unexpectedly passed" >&2; exit 1
  else
    status=$?
  fi
  [ "$status" -eq 1 ]
  [ "$(cat "$state")" = old ]
  [ "$before_controls" = "$(sha256sum "$root/image-attestation" "$root/.manifest" "$root/.env" | awk '{print $1}')" ]
  grep -Fq "container.0.id=$new_message_id" "$root/.cleanup-receipt"
  grep -Fq "container.1.id=$agent_id" "$root/.cleanup-receipt"
  grep -Fq "container.2.id=$postgres_id" "$root/.cleanup-receipt"
  grep -Fqx "control.env_identity=$(stat -c '%d:%i:%u' "$root/.env")" "$root/.cleanup-receipt"
  grep -Fqx "control.manifest_identity=$(stat -c '%d:%i:%u' "$root/.manifest")" "$root/.cleanup-receipt"
done

root=$(make_stack receipt-mismatch)
sed -i 's/^docker.engine_id=.*/docker.engine_id=replacement-engine/' "$root/.cleanup-receipt"
chmod 400 "$root/.cleanup-receipt"
: >"$log"
if run_update "$root" v1.0.1 >/dev/null 2>&1; then
  echo 'receipt Engine mismatch unexpectedly passed' >&2; exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
if grep -Eq '(^|\|)(pull|compose)( |\|)' "$log"; then
  echo 'receipt mismatch crossed the Docker mutation boundary' >&2; exit 1
fi

printf 'Message-server receipt-bound update, isolation, three-state result, and rollback verified\n'
