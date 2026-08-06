#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
stop_script=$script_dir/stop-agent-local.sh
restart_script=$script_dir/restart-agent-local.sh
common_script=$script_dir/agent-runtime-local-common.sh
for script in "$stop_script" "$restart_script" "$common_script"; do
  [ -x "$script" ] || { echo "$script must be executable" >&2; exit 1; }
  bash -n "$script"
done
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x "$common_script" "$stop_script" "$restart_script"
fi

tmp_dir=$(mktemp -d /tmp/dirextalk-agent-runtime-local.XXXXXX)
trap 'rm -rf -- "$tmp_dir"' EXIT
stack_name=d-aaaaaaaaaaaaaaaaaaaaaaaaaa
engine_id=engine-agent-runtime-test-1
machine_id=$(tr -d '[:space:]' </etc/machine-id)
agent_id=$(printf '1%.0s' {1..64})
extension_id=$(printf '2%.0s' {1..64})
core_id=$(printf '3%.0s' {1..64})
message_id=$(printf '4%.0s' {1..64})
agent_image=dirextalk-agent:split-local
agent_image_id=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

write_fixture() {
  local fixture=$1 env_file manifest receipt env_identity manifest_identity
  mkdir -m 700 -- "$fixture"
  env_file=$fixture/.env
  manifest=$fixture/.manifest
  receipt=$fixture/.cleanup-receipt
  printf 'DIREXTALK_SPLIT_STACK_NAME=%s\nDIREXTALK_AGENT_IMAGE_LOCAL=%s\nDIREXTALK_AGENT_IMAGE_IMMUTABLE=registry.example/agent@sha256:%064d\n' \
    "$stack_name" "$agent_image" 1 >"$env_file"
  chmod 400 -- "$env_file"
  printf 'stack_name=%s\ncompose_mode=local\nrunner.machine_id=%s\nrunner.docker_engine_id=%s\n' \
    "$stack_name" "$machine_id" "$engine_id" >"$manifest"
  chmod 400 -- "$manifest"
  env_identity=$(stat -c '%d:%i:%u' -- "$env_file")
  manifest_identity=$(stat -c '%d:%i:%u' -- "$manifest")
  {
    printf '# dirextalk-split-cleanup-receipt-v1\nstack_name=%s\nstate=complete\n' "$stack_name"
    printf 'control.env_identity=%s\ncontrol.manifest_identity=%s\n' "$env_identity" "$manifest_identity"
    printf 'control.env_sha256=%s\ncontrol.manifest_sha256=%s\n' \
      "$(sha256sum -- "$env_file" | awk '{print $1}')" "$(sha256sum -- "$manifest" | awk '{print $1}')"
    printf 'host.machine_id=%s\ndocker.engine_id=%s\ndocker.context_endpoint=unix:///run/docker.sock\ndocker.context_socket=/run/docker.sock\n' "$machine_id" "$engine_id"
    printf 'container.count=4\n'
    printf 'container.0.id=%s\ncontainer.0.name=%s-message-server-1\ncontainer.0.service=message-server\ncontainer.0.project=%s\n' "$message_id" "$stack_name" "$stack_name"
    printf 'container.1.id=%s\ncontainer.1.name=%s-agent-1\ncontainer.1.service=agent\ncontainer.1.project=%s\n' "$agent_id" "$stack_name" "$stack_name"
    printf 'container.2.id=%s\ncontainer.2.name=%s-extension-runner-1\ncontainer.2.service=extension-runner\ncontainer.2.project=%s\n' "$extension_id" "$stack_name" "$stack_name"
    printf 'container.3.id=%s\ncontainer.3.name=%s-core-runner-1\ncontainer.3.service=core-runner\ncontainer.3.project=%s\n' "$core_id" "$stack_name" "$stack_name"
  } >"$receipt"
  chmod 400 -- "$receipt"
  mkdir -p -- "$fixture/bin" "$fixture/state"
  cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|running|healthy
extension-runner|$extension_id|running|healthy
core-runner|$core_id|running|healthy
EOF
  : >"$fixture/state/docker.log"
  cat >"$fixture/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
log=$DIREXTALK_FAKE_STATE/docker.log
printf '%s\n' "$*" >>"$log"
state=$DIREXTALK_FAKE_STATE/containers
lookup_by_id() { awk -F'|' -v wanted="$1" '$2 == wanted { print; exit }' "$state"; }
lookup_by_name() {
  case "$1" in
    *-agent-1) printf 'agent\n' ;;
    *-extension-runner-1) printf 'extension-runner\n' ;;
    *-core-runner-1) printf 'core-runner\n' ;;
    *-message-server-1) printf 'message\n' ;;
    *) return 1 ;;
  esac
}
update_status() {
  role=$1
  next_status=$2
  next_health=$3
  tmp=$(mktemp "$DIREXTALK_FAKE_STATE/containers.XXXXXX")
  awk -F'|' -v wanted="$role" -v status="$next_status" -v health="$next_health" \
    'BEGIN { OFS="|" } $1 == wanted { $3=status; $4=health } { print }' "$state" >"$tmp"
  mv -- "$tmp" "$state"
}
json_for_role() {
  record=$(awk -F'|' -v wanted="$1" '$1 == wanted { print; exit }' "$state")
  [ -n "$record" ] || return 1
  IFS='|' read -r role id status health <<<"$record"
  case "$role" in
    message) name=$DIREXTALK_FAKE_STACK-message-server-1; service=message-server; image=dirextalk-message-server:split-local; image_id=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
    agent) name=$DIREXTALK_FAKE_STACK-agent-1; service=agent; image=$DIREXTALK_FAKE_AGENT_IMAGE; image_id=$DIREXTALK_FAKE_AGENT_IMAGE_ID ;;
    extension-runner) name=$DIREXTALK_FAKE_STACK-extension-runner-1; service=extension-runner; image=$DIREXTALK_FAKE_AGENT_IMAGE; image_id=$DIREXTALK_FAKE_AGENT_IMAGE_ID ;;
    core-runner) name=$DIREXTALK_FAKE_STACK-core-runner-1; service=core-runner; image=$DIREXTALK_FAKE_AGENT_IMAGE; image_id=$DIREXTALK_FAKE_AGENT_IMAGE_ID ;;
    *) return 1 ;;
  esac
  printf '[{"Id":"%s","Name":"/%s","Config":{"Image":"%s","Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"%s"}},"Image":"%s","State":{"Status":"%s","Health":{"Status":"%s"}}}]\n' \
    "$id" "$name" "$image" "$DIREXTALK_FAKE_STACK" "$service" "$image_id" "$status" "$health"
}
case "$1" in
  context) printf 'unix:///run/docker.sock\n' ;;
  info) printf '%s\n' "$DIREXTALK_FAKE_ENGINE" ;;
  image)
    [ "$2" = inspect ] || exit 1
    printf '%s\n' "$DIREXTALK_FAKE_AGENT_IMAGE_ID"
    ;;
  inspect)
    target=$2
    record=$(lookup_by_id "$target" || true)
    if [ -n "$record" ]; then
      role=$(printf '%s' "$record" | cut -d'|' -f1)
      json_for_role "$role"
      exit 0
    fi
    replacement_role=$DIREXTALK_FAKE_REPLACEMENT_ROLE
    if [ -n "$replacement_role" ] && \
       name_role=$(lookup_by_name "$target" 2>/dev/null) && \
       [ "$name_role" = "$replacement_role" ]; then
      replacement_id=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
      printf '[{"Id":"%s","Name":"/%s-%s-1","Config":{"Image":"%s","Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"%s"}},"Image":"%s","State":{"Status":"running","Health":{"Status":"healthy"}}}]\n' \
        "$replacement_id" "$DIREXTALK_FAKE_STACK" "$name_role" "$DIREXTALK_FAKE_AGENT_IMAGE" "$DIREXTALK_FAKE_STACK" "$name_role" "$DIREXTALK_FAKE_AGENT_IMAGE_ID"
      exit 0
    fi
    printf 'Error: No such object: %s\n' "$target" >&2
    exit 1
    ;;
  container)
    action=$2
    target=$3
    record=$(lookup_by_id "$target" || true)
    [ -n "$record" ] || { printf 'No such container: %s\n' "$target" >&2; exit 1; }
    role=$(printf '%s' "$record" | cut -d'|' -f1)
    [ "$role" != message ] || { printf 'message-server is outside Agent runtime scope\n' >&2; exit 1; }
    if [ "$action" = stop ]; then
      [ "$DIREXTALK_FAKE_FAIL_STOP_ROLE" != "$role" ] || { printf 'injected stop failure\n' >&2; exit 1; }
      update_status "$role" exited none
    elif [ "$action" = start ]; then
      [ "$DIREXTALK_FAKE_FAIL_START_ROLE" != "$role" ] || { printf 'injected start failure\n' >&2; exit 1; }
      update_status "$role" running healthy
    else
      printf 'unexpected container action: %s\n' "$action" >&2
      exit 1
    fi
    ;;
  *) printf 'unexpected docker command: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
  chmod 755 -- "$fixture/bin/docker"
}

run_expect() {
  local expected=$1 script=$2 fixture=$3 status output
  shift 3
  if output=$(env "$@" "$script" "$fixture" 2>&1); then
    status=0
  else
    status=$?
  fi
  [ "$status" -eq "$expected" ] || { printf '%s\n' "$output" >&2; echo "expected status $expected, got $status" >&2; exit 1; }
}

fixture=$tmp_dir/normal
write_fixture "$fixture"
export PATH=$fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$fixture/state DIREXTALK_FAKE_STACK=$stack_name DIREXTALK_FAKE_ENGINE=$engine_id
export DIREXTALK_FAKE_AGENT_IMAGE=$agent_image DIREXTALK_FAKE_AGENT_IMAGE_ID=$agent_image_id
export DIREXTALK_FAKE_REPLACEMENT_ROLE='' DIREXTALK_FAKE_FAIL_STOP_ROLE='' DIREXTALK_FAKE_FAIL_START_ROLE=''

tampered_env_fixture=$tmp_dir/tampered-env
write_fixture "$tampered_env_fixture"
chmod 600 -- "$tampered_env_fixture/.env"
printf 'DIREXTALK_TAMPERED=true\n' >>"$tampered_env_fixture/.env"
chmod 400 -- "$tampered_env_fixture/.env"
run_expect 1 "$stop_script" "$tampered_env_fixture" DIREXTALK_FAKE_STATE="$tampered_env_fixture/state"
if grep -Eq '^container (stop|start) ' "$tampered_env_fixture/state/docker.log"; then
  echo 'pre-call .env tampering reached Docker mutation' >&2
  exit 1
fi

replaced_manifest_fixture=$tmp_dir/replaced-manifest
write_fixture "$replaced_manifest_fixture"
cp -- "$replaced_manifest_fixture/.manifest" "$replaced_manifest_fixture/.manifest.replacement"
chmod 400 -- "$replaced_manifest_fixture/.manifest.replacement"
mv -- "$replaced_manifest_fixture/.manifest.replacement" "$replaced_manifest_fixture/.manifest"
run_expect 1 "$stop_script" "$replaced_manifest_fixture" DIREXTALK_FAKE_STATE="$replaced_manifest_fixture/state"
if grep -Eq '^container (stop|start) ' "$replaced_manifest_fixture/state/docker.log"; then
  echo 'pre-call manifest replacement reached Docker mutation' >&2
  exit 1
fi

run_expect 0 "$stop_script" "$fixture"
sequence=$(grep -E '^(container stop|container start)' "$fixture/state/docker.log" | tr '\n' ' ')
[ "$sequence" = "container stop $agent_id container stop $extension_id container stop $core_id " ]
run_expect 3 "$stop_script" "$fixture"

# Docker may report an exact restart-policy container as `restarting` while
# both runners remain stopped. It is an active state that must first cross the
# same exact-ID stop boundary before the ordered healthy restart.
cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|restarting|starting
extension-runner|$extension_id|exited|none
core-runner|$core_id|exited|none
EOF
: >"$fixture/state/docker.log"
run_expect 0 "$restart_script" "$fixture"
sequence=$(grep -E '^(container stop|container start)' "$fixture/state/docker.log" | tr '\n' ' ')
[ "$sequence" = "container stop $agent_id container start $extension_id container start $core_id container start $agent_id " ]

cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|restarting|starting
extension-runner|$extension_id|exited|none
core-runner|$core_id|exited|none
EOF
: >"$fixture/state/docker.log"
run_expect 0 "$stop_script" "$fixture"
sequence=$(grep -E '^(container stop|container start)' "$fixture/state/docker.log" | tr '\n' ' ')
[ "$sequence" = "container stop $agent_id " ]
run_expect 3 "$stop_script" "$fixture"

cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|exited|none
extension-runner|$extension_id|exited|none
core-runner|$core_id|exited|none
EOF
run_expect 0 "$restart_script" "$fixture"
grep -Fq "container start $extension_id" "$fixture/state/docker.log"
grep -Fq "container start $core_id" "$fixture/state/docker.log"
grep -Fq "container start $agent_id" "$fixture/state/docker.log"
if grep -Fq "container stop $message_id" "$fixture/state/docker.log" || grep -Fq "container start $message_id" "$fixture/state/docker.log"; then
  echo 'message-server was unexpectedly mutated' >&2
  exit 1
fi
: >"$fixture/state/docker.log"
run_expect 0 "$restart_script" "$fixture"
sequence=$(grep -E '^(container stop|container start)' "$fixture/state/docker.log" | tr '\n' ' ')
[ "$sequence" = "container stop $agent_id container stop $extension_id container stop $core_id container start $extension_id container start $core_id container start $agent_id " ]

cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|running|healthy
extension-runner|$extension_id|exited|none
core-runner|$core_id|running|healthy
EOF
: >"$fixture/state/docker.log"
run_expect 0 "$restart_script" "$fixture"
sequence=$(grep -E '^(container stop|container start)' "$fixture/state/docker.log" | tr '\n' ' ')
[ "$sequence" = "container stop $agent_id container stop $core_id container start $extension_id container start $core_id container start $agent_id " ]

cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
extension-runner|$extension_id|running|healthy
core-runner|$core_id|running|healthy
EOF
: >"$fixture/state/docker.log"
run_expect 1 "$stop_script" "$fixture" DIREXTALK_FAKE_REPLACEMENT_ROLE=agent
if grep -Eq '^container (stop|start) ' "$fixture/state/docker.log"; then
  exit 1
fi

cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|dead|none
extension-runner|$extension_id|running|healthy
core-runner|$core_id|running|healthy
EOF
: >"$fixture/state/docker.log"
run_expect 1 "$stop_script" "$fixture"
if grep -Eq '^container (stop|start) ' "$fixture/state/docker.log"; then
  exit 1
fi

cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|exited|none
extension-runner|$extension_id|exited|none
core-runner|$core_id|exited|none
EOF
: >"$fixture/state/docker.log"
run_expect 1 "$restart_script" "$fixture" DIREXTALK_FAKE_FAIL_START_ROLE=core-runner
run_expect 0 "$restart_script" "$fixture"

cat >"$fixture/state/containers" <<EOF
message|$message_id|running|healthy
agent|$agent_id|running|healthy
extension-runner|$extension_id|running|healthy
core-runner|$core_id|running|healthy
EOF
: >"$fixture/state/docker.log"
run_expect 1 "$stop_script" "$fixture" DIREXTALK_FAKE_ENGINE=engine-replaced
if grep -Eq '^container (stop|start) ' "$fixture/state/docker.log"; then
  exit 1
fi

printf 'Agent runtime receipt-bound stop/start ordering, identity, recovery, and scope verified\n'
