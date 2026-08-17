#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/update-agent-local.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-agent-recovery.XXXXXX")
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

message_id=$(printf '1%.0s' {1..64})
live_message_id=$(printf '6%.0s' {1..64})
new_message_id=$(printf '0%.0s' {1..64})
old_agent_id=$(printf '2%.0s' {1..64})
old_extension_id=$(printf '3%.0s' {1..64})
old_core_id=$(printf '4%.0s' {1..64})
live_agent_id=$(printf 'a%.0s' {1..64})
live_extension_id=$(printf 'b%.0s' {1..64})
live_core_id=$(printf 'c%.0s' {1..64})
new_agent_id=$(printf 'd%.0s' {1..64})
new_extension_id=$(printf 'e%.0s' {1..64})
new_core_id=$(printf 'f%.0s' {1..64})
message_image_id=sha256:$(printf '1%.0s' {1..64})
live_image_id=sha256:$(printf '9%.0s' {1..64})
other_image_id=sha256:$(printf '8%.0s' {1..64})
target_image_id=sha256:$(printf '7%.0s' {1..64})
revision_90=$(printf '8%.0s' {1..40})
revision_91=$(printf '9%.0s' {1..40})
revision_92=$(printf '7%.0s' {1..40})

fake_bin=$tmp/bin
mkdir -p "$fake_bin"
cat >"$fake_bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
log() { printf '%s\n' "$*" >>"$DOCKER_FIXTURE_LOG"; }
container_json() {
  local id=$1 service=$2 image_id=$3 health=$4 project=test-stack config_image=docker.io/dirextalk/agent:v1.0.91 status=running
  [ "$service" != message-server ] || config_image=docker.io/dirextalk/message-server:v1.1.39
  case "$id" in "$NEW_AGENT_ID"|"$NEW_EXTENSION_ID"|"$NEW_CORE_ID") config_image=docker.io/dirextalk/agent:v1.0.92 ;; esac
  if [ "$service" = agent ]; then
    case "$DOCKER_FIXTURE_SCENARIO" in
      agent_restarting) status=restarting ;;
      agent_exited) status=exited ;;
      agent_dead) status=dead ;;
      agent_paused) status=paused ;;
    esac
  elif [ "$service" = extension-runner ] && [ "$DOCKER_FIXTURE_SCENARIO" = runner_restarting ]; then
    status=restarting
  fi
  [ "$DOCKER_FIXTURE_SCENARIO" != bad_project ] || [ "$service" != agent ] || project=other-stack
  [ "$DOCKER_FIXTURE_SCENARIO" != bad_service ] || [ "$service" != extension-runner ] || service=agent
  [ "$DOCKER_FIXTURE_SCENARIO" != bad_config_image ] || [ "$id" != "$LIVE_CORE_ID" ] || config_image=docker.io/dirextalk/agent:v1.0.90
  printf '[{"Id":"%s","Image":"%s","Config":{"Image":"%s","Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"%s"}},"State":{"Status":"%s","Health":{"Status":"%s"}}}]\n' \
    "$id" "$image_id" "$config_image" "$project" "$service" "$status" "$health"
}

case "${1:-}" in
  inspect)
    id=${2:-}
    if [ "$id" = "$MESSAGE_ID" ]; then
      [ "$DOCKER_FIXTURE_SCENARIO" != message_missing ] || exit 1
      container_json "$id" message-server "$MESSAGE_IMAGE_ID" healthy
      exit 0
    fi
    case "$id" in
      "$LIVE_MESSAGE_ID"|"$NEW_MESSAGE_ID") container_json "$id" message-server "$MESSAGE_IMAGE_ID" healthy ;;
      "$OLD_AGENT_ID"|"$OLD_EXTENSION_ID"|"$OLD_CORE_ID") exit 1 ;;
      "$LIVE_AGENT_ID") container_json "$id" agent "$LIVE_IMAGE_ID" unhealthy ;;
      "$LIVE_EXTENSION_ID")
        health=healthy
        [ "$DOCKER_FIXTURE_SCENARIO" != extension_unhealthy ] || health=unhealthy
        container_json "$id" extension-runner "$LIVE_IMAGE_ID" "$health"
        ;;
      "$LIVE_CORE_ID")
        image_id=$LIVE_IMAGE_ID
        [ "$DOCKER_FIXTURE_SCENARIO" != bad_image_id ] || image_id=$OTHER_IMAGE_ID
        container_json "$id" core-runner "$image_id" healthy
        ;;
      "$NEW_AGENT_ID") container_json "$id" agent "$TARGET_IMAGE_ID" healthy ;;
      "$NEW_EXTENSION_ID") container_json "$id" extension-runner "$TARGET_IMAGE_ID" healthy ;;
      "$NEW_CORE_ID") container_json "$id" core-runner "$TARGET_IMAGE_ID" healthy ;;
      *) exit 1 ;;
    esac
    ;;
  image)
    case "${2:-}" in
      inspect)
        ref=${3:-}
        case "$ref" in
          "$MESSAGE_IMAGE_ID") printf 'v1.1.39\n' ;;
          "$LIVE_IMAGE_ID")
            case "${4:-}" in
              --format)
                if [[ "${5:-}" == *revision* ]]; then printf 'v1.0.91|%s\n' "$REVISION_91"; else printf 'v1.0.91\n'; fi
                ;;
              *) printf '[]\n' ;;
            esac
            ;;
          "$TARGET_IMAGE_ID")
            case "${4:-}" in
              --format)
                if [[ "${5:-}" == *revision* ]]; then printf 'v1.0.92|%s\n' "$REVISION_92"; else printf 'v1.0.92\n'; fi
                ;;
              *) printf '[]\n' ;;
            esac
            ;;
          fixture-target) printf 'v1.0.92|%s|%s\n' "$TARGET_IMAGE_ID" "$REVISION_92" ;;
          *) exit 1 ;;
        esac
        ;;
      tag) log "image tag ${3:-} ${4:-}" ;;
      rm) log "image rm ${3:-}" ;;
      *) exit 2 ;;
    esac
    ;;
  exec)
    case "${2:-}" in
      "$MESSAGE_ID"|"$LIVE_MESSAGE_ID"|"$NEW_MESSAGE_ID") printf 'v1.1.39\n' ;;
      "$LIVE_AGENT_ID"|"$LIVE_EXTENSION_ID"|"$LIVE_CORE_ID") printf 'v1.0.91\n' ;;
      "$NEW_AGENT_ID"|"$NEW_EXTENSION_ID"|"$NEW_CORE_ID") printf 'v1.0.92\n' ;;
      *) exit 1 ;;
    esac
    ;;
  run)
    case " $* " in *" $LIVE_IMAGE_ID "*) printf 'v1.0.91\n';; *) printf 'v1.0.92\n';; esac
    ;;
  compose)
    shift
    while [ "$#" -gt 0 ]; do
      case "$1" in ps|run|up) compose_command=$1; shift; break;; *) shift;; esac
    done
    case "${compose_command:-}" in
      ps)
        service=${@: -1}
        state=initial
        [ ! -f "$DOCKER_FIXTURE_STATE" ] || state=$(cat "$DOCKER_FIXTURE_STATE")
        if [ "$service" = message-server ]; then
          if [ "$state" = message ]; then
            printf '%s\n' "$NEW_MESSAGE_ID"
          elif [ "$DOCKER_FIXTURE_SCENARIO" = message_missing ]; then
            printf '%s\n' "$LIVE_MESSAGE_ID"
          else
            printf '%s\n' "$MESSAGE_ID"
          fi
        elif [ "$DOCKER_FIXTURE_SCENARIO" = interrupted_target ] || [ "$state" != initial ]; then
          case "$service" in agent) printf '%s\n' "$NEW_AGENT_ID";; extension-runner) printf '%s\n' "$NEW_EXTENSION_ID";; core-runner) printf '%s\n' "$NEW_CORE_ID";; *) exit 1;; esac
        else
          case "$service" in agent) printf '%s\n' "$LIVE_AGENT_ID";; extension-runner) printf '%s\n' "$LIVE_EXTENSION_ID";; core-runner) printf '%s\n' "$LIVE_CORE_ID";; *) exit 1;; esac
        fi
        ;;
      run)
        service=${@: -1}
        [ "$service" = agent-migrate ]
        grep -Fqx 'DIREXTALK_AGENT_VERSION=v1.0.91' "$DOCKER_FIXTURE_OUT/.env"
        grep -Fqx 'DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:v1.0.91' "$DOCKER_FIXTURE_OUT/.env"
        grep -Fqx "DIREXTALK_AGENT_SOURCE_REVISION=$REVISION_91" "$DOCKER_FIXTURE_OUT/.env"
        grep -Fqx "container.0.id=$EXPECTED_RECOVERED_MESSAGE_ID" "$DOCKER_FIXTURE_OUT/.cleanup-receipt"
        grep -Fqx "container.1.id=$LIVE_AGENT_ID" "$DOCKER_FIXTURE_OUT/.cleanup-receipt"
        grep -Fqx "container.2.id=$LIVE_EXTENSION_ID" "$DOCKER_FIXTURE_OUT/.cleanup-receipt"
        grep -Fqx "container.3.id=$LIVE_CORE_ID" "$DOCKER_FIXTURE_OUT/.cleanup-receipt"
        env_identity=$(stat -c '%d:%i:%u' "$DOCKER_FIXTURE_OUT/.env")
        env_sha=$(sha256sum "$DOCKER_FIXTURE_OUT/.env" | awk '{print $1}')
        grep -Fqx "control.env_identity=$env_identity" "$DOCKER_FIXTURE_OUT/.cleanup-receipt"
        grep -Fqx "control.env_sha256=$env_sha" "$DOCKER_FIXTURE_OUT/.cleanup-receipt"
        log 'migration saw recovered v1.0.91 baseline'
        ;;
      up)
        log "compose up:$*"
        case " $* " in
          *' message-server '*) printf 'message\n' >"$DOCKER_FIXTURE_STATE" ;;
          *) printf 'agent\n' >"$DOCKER_FIXTURE_STATE" ;;
        esac
        ;;
      *) exit 2 ;;
    esac
    ;;
  ps) exit 0 ;;
  *) exit 2 ;;
esac
DOCKER
chmod 755 "$fake_bin/docker"

make_fixture() {
  local root=$1 out=$1/out env_identity env_sha
  mkdir -p "$out"
  chmod 700 "$out"
  cat >"$out/.env" <<EOF
DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:v1.0.90
DIREXTALK_MESSAGE_SERVER_IMAGE=docker.io/dirextalk/message-server:v1.1.39
DIREXTALK_AGENT_VERSION=v1.0.90
DIREXTALK_AGENT_SOURCE_REVISION=$revision_90
DIREXTALK_AGENT_CONFIG_FILE=$out/agent-config.yaml
UNRELATED_ENV=preserve-me
EOF
  cat >"$out/agent-config.yaml" <<EOF
instance_id: test
agent_http_enabled: true
agent_http_listen: 0.0.0.0:8082
EOF
  chmod 400 "$out/.env"
  env_identity=$(stat -c '%d:%i:%u' "$out/.env")
  env_sha=$(sha256sum "$out/.env" | awk '{print $1}')
  cat >"$out/.manifest" <<EOF
compose_mode=production
stack_name=test-stack
EOF
  cat >"$out/.cleanup-receipt" <<EOF
state=complete
control.env_identity=$env_identity
control.env_sha256=$env_sha
container.count=4
container.0.service=message-server
container.0.id=$message_id
container.0.project=test-stack
container.1.service=agent
container.1.id=$old_agent_id
container.1.project=test-stack
container.2.service=extension-runner
container.2.id=$old_extension_id
container.2.project=test-stack
container.3.service=core-runner
container.3.id=$old_core_id
container.3.project=test-stack
unrelated.receipt=preserve-me
EOF
  chmod 400 "$out/.manifest" "$out/.cleanup-receipt"
  chmod 400 "$out/agent-config.yaml"
}

run_wrapper() {
  local root=$1 scenario=$2 recovered_message_id=$message_id
  [ "$scenario" != message_missing ] || recovered_message_id=$live_message_id
  PATH="$fake_bin:$PATH" \
    DIREXTALK_AGENT_UPDATE_TEST_FIXTURE=true \
    DIREXTALK_AGENT_UPDATE_HEALTH_ATTEMPTS=1 \
    DIREXTALK_AGENT_LOCAL_IMAGE_REF=fixture-target \
    DOCKER_FIXTURE_SCENARIO="$scenario" \
    DOCKER_FIXTURE_LOG="$root/docker.log" \
    DOCKER_FIXTURE_STATE="$root/docker.state" \
    DOCKER_FIXTURE_OUT="$root/out" \
    MESSAGE_ID="$message_id" LIVE_MESSAGE_ID="$live_message_id" NEW_MESSAGE_ID="$new_message_id" EXPECTED_RECOVERED_MESSAGE_ID="$recovered_message_id" \
    OLD_AGENT_ID="$old_agent_id" OLD_EXTENSION_ID="$old_extension_id" OLD_CORE_ID="$old_core_id" \
    LIVE_AGENT_ID="$live_agent_id" LIVE_EXTENSION_ID="$live_extension_id" LIVE_CORE_ID="$live_core_id" \
    NEW_AGENT_ID="$new_agent_id" NEW_EXTENSION_ID="$new_extension_id" NEW_CORE_ID="$new_core_id" \
    MESSAGE_IMAGE_ID="$message_image_id" LIVE_IMAGE_ID="$live_image_id" OTHER_IMAGE_ID="$other_image_id" TARGET_IMAGE_ID="$target_image_id" \
    REVISION_91="$revision_91" REVISION_92="$revision_92" \
    "$script" "$root/out" v1.0.92 v1.1.39
}

for invalid_config in retired missing wrong duplicate; do
  root=$tmp/config_$invalid_config
  make_fixture "$root"
  chmod 600 "$root/out/agent-config.yaml"
  case "$invalid_config" in
    retired) printf '%s\n' 'capability_enabled: true' >>"$root/out/agent-config.yaml" ;;
    missing) sed -i '/^agent_http_enabled:/d' "$root/out/agent-config.yaml" ;;
    wrong) sed -i 's/^agent_http_enabled: true$/agent_http_enabled: false/' "$root/out/agent-config.yaml" ;;
    duplicate) printf '%s\n' 'agent_http_listen: 0.0.0.0:8082' >>"$root/out/agent-config.yaml" ;;
  esac
  chmod 400 "$root/out/agent-config.yaml"
  cp "$root/out/agent-config.yaml" "$root/original.agent-config.yaml"
  if run_wrapper "$root" success >"$root/stdout" 2>"$root/stderr"; then
    echo "$invalid_config Agent config unexpectedly passed" >&2
    exit 1
  fi
  cmp "$root/original.agent-config.yaml" "$root/out/agent-config.yaml"
  [ ! -f "$root/docker.log" ] || { echo "$invalid_config Agent config reached Docker" >&2; exit 1; }
done

success_root=$tmp/success
make_fixture "$success_root"
cp "$success_root/out/.env" "$success_root/original.env"
cp "$success_root/out/.cleanup-receipt" "$success_root/original.receipt"
cp "$success_root/out/agent-config.yaml" "$success_root/original.agent-config.yaml"
run_wrapper "$success_root" success >"$success_root/stdout" 2>"$success_root/stderr"
grep -Fqx 'migration saw recovered v1.0.91 baseline' "$success_root/docker.log"
grep -Fqx 'DIREXTALK_AGENT_VERSION=v1.0.92' "$success_root/out/.env"
grep -Fqx 'DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:v1.0.92' "$success_root/out/.env"
grep -Fqx "DIREXTALK_AGENT_SOURCE_REVISION=$revision_92" "$success_root/out/.env"
grep -Fqx 'UNRELATED_ENV=preserve-me' "$success_root/out/.env"
grep -Fqx "container.0.id=$new_message_id" "$success_root/out/.cleanup-receipt"
grep -Fqx "container.1.id=$new_agent_id" "$success_root/out/.cleanup-receipt"
grep -Fqx "container.2.id=$new_extension_id" "$success_root/out/.cleanup-receipt"
grep -Fqx "container.3.id=$new_core_id" "$success_root/out/.cleanup-receipt"
grep -Fqx 'unrelated.receipt=preserve-me' "$success_root/out/.cleanup-receipt"
env_identity=$(stat -c '%d:%i:%u' "$success_root/out/.env")
env_sha=$(sha256sum "$success_root/out/.env" | awk '{print $1}')
grep -Fqx "control.env_identity=$env_identity" "$success_root/out/.cleanup-receipt"
grep -Fqx "control.env_sha256=$env_sha" "$success_root/out/.cleanup-receipt"
sed -E '/^DIREXTALK_AGENT_(IMAGE|VERSION|SOURCE_REVISION)=/d' "$success_root/original.env" >"$success_root/original.env.stable"
sed -E '/^DIREXTALK_AGENT_(IMAGE|VERSION|SOURCE_REVISION)=/d' "$success_root/out/.env" >"$success_root/final.env.stable"
cmp "$success_root/original.env.stable" "$success_root/final.env.stable"
sed -E '/^control\.env_(identity|sha256)=|^container\.[0123]\.id=/d' "$success_root/original.receipt" >"$success_root/original.receipt.stable"
sed -E '/^control\.env_(identity|sha256)=|^container\.[0123]\.id=/d' "$success_root/out/.cleanup-receipt" >"$success_root/final.receipt.stable"
cmp "$success_root/original.receipt.stable" "$success_root/final.receipt.stable"
cmp "$success_root/original.agent-config.yaml" "$success_root/out/agent-config.yaml"
grep -Fqx 'agent_http_enabled: true' "$success_root/out/agent-config.yaml"
grep -Fqx 'agent_http_listen: 0.0.0.0:8082' "$success_root/out/agent-config.yaml"
grep -Fqx 'compose up:-d --no-deps --force-recreate --no-build --pull never extension-runner core-runner agent' "$success_root/docker.log"
grep -Fqx 'compose up:-d --no-deps --force-recreate --no-build --pull never message-server' "$success_root/docker.log"

message_recovery_root=$tmp/message_missing
make_fixture "$message_recovery_root"
run_wrapper "$message_recovery_root" message_missing >"$message_recovery_root/stdout" 2>"$message_recovery_root/stderr"
grep -Fqx 'migration saw recovered v1.0.91 baseline' "$message_recovery_root/docker.log"
grep -Fqx "container.0.id=$new_message_id" "$message_recovery_root/out/.cleanup-receipt"

interrupted_target_root=$tmp/interrupted_target
make_fixture "$interrupted_target_root"
run_wrapper "$interrupted_target_root" interrupted_target >"$interrupted_target_root/stdout" 2>"$interrupted_target_root/stderr"
if grep -q '^migration ' "$interrupted_target_root/docker.log"; then
  echo 'interrupted target recovery repeated Agent migration' >&2
  exit 1
fi
if grep -Fq 'extension-runner core-runner agent' "$interrupted_target_root/docker.log"; then
  echo 'interrupted target recovery repeated Agent recreate' >&2
  exit 1
fi
grep -Fqx 'compose up:-d --no-deps --force-recreate --no-build --pull never message-server' "$interrupted_target_root/docker.log"
grep -Fqx "container.0.id=$new_message_id" "$interrupted_target_root/out/.cleanup-receipt"
grep -Fqx "container.1.id=$new_agent_id" "$interrupted_target_root/out/.cleanup-receipt"
grep -Fqx 'DIREXTALK_AGENT_VERSION=v1.0.92' "$interrupted_target_root/out/.env"

restarting_root=$tmp/agent_restarting
make_fixture "$restarting_root"
run_wrapper "$restarting_root" agent_restarting >"$restarting_root/stdout" 2>"$restarting_root/stderr"
grep -Fqx 'migration saw recovered v1.0.91 baseline' "$restarting_root/docker.log"
grep -Fqx 'DIREXTALK_AGENT_VERSION=v1.0.92' "$restarting_root/out/.env"

unhealthy_extension_root=$tmp/extension_unhealthy
make_fixture "$unhealthy_extension_root"
run_wrapper "$unhealthy_extension_root" extension_unhealthy >"$unhealthy_extension_root/stdout" 2>"$unhealthy_extension_root/stderr"
grep -Fqx 'migration saw recovered v1.0.91 baseline' "$unhealthy_extension_root/docker.log"
grep -Fqx 'DIREXTALK_AGENT_VERSION=v1.0.92' "$unhealthy_extension_root/out/.env"

for scenario in bad_project bad_service bad_config_image bad_image_id agent_exited agent_dead agent_paused runner_restarting; do
  root=$tmp/$scenario
  make_fixture "$root"
  cp "$root/out/.env" "$root/original.env"
  cp "$root/out/.cleanup-receipt" "$root/original.receipt"
  cp "$root/out/agent-config.yaml" "$root/original.agent-config.yaml"
  if run_wrapper "$root" "$scenario" >"$root/stdout" 2>"$root/stderr"; then
    echo "$scenario recovery unexpectedly succeeded" >&2
    exit 1
  fi
  cmp "$root/original.env" "$root/out/.env"
  cmp "$root/original.receipt" "$root/out/.cleanup-receipt"
  cmp "$root/original.agent-config.yaml" "$root/out/agent-config.yaml"
  if [ -f "$root/docker.log" ] && grep -Eq 'migration|compose up' "$root/docker.log"; then
    echo "$scenario reached migration or recreate" >&2
    exit 1
  fi
done

printf 'Agent interrupted-update recovery fixture passed\n'
