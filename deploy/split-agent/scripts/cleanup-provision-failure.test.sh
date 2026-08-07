#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/cleanup-provision-failure.sh
[ -x "$script" ] || { echo "cleanup-provision-failure.sh must be executable" >&2; exit 1; }
bash -n "$script"
command -v shellcheck >/dev/null 2>&1 || { echo "shellcheck is required" >&2; exit 1; }
shellcheck -x "$script"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-provision-failure-cleanup.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/apparmor.d"
cp -- "$script_dir/../apparmor.d/dirextalk-runner-userns" "$tmp/apparmor.d/dirextalk-runner-userns"
chmod 0644 "$tmp/apparmor.d/dirextalk-runner-userns"
printf 'dirextalk-runner-userns (unconfined)\n' >"$tmp/apparmor-profiles"
export DIREXTALK_SPLIT_TEST_MODE=true
export DIREXTALK_APPARMOR_TARGET_DIR="$tmp/apparmor.d"
export DIREXTALK_APPARMOR_LOADED_PROFILES="$tmp/apparmor-profiles"
stack=d-aaaaaaaaaaaaaaaaaaaaaaaaaa
machine=$(tr -d '[:space:]' </etc/machine-id)
engine=cleanup-failure-engine
fragment=/usr/lib/systemd/system/systemd-journald.service
fragment_hash=$(sha256sum -- "$fragment" | awk '{print $1}')
network_keys=(message_private message_public message_database agent_private agent_database agent_caller agent_egress)
volume_keys=(postgres message_config message_data message_plugins agent_secrets agent_config agent_core_data agent_extension_socket agent_extension_install agent_extension_staging agent_runner_workspaces agent_runner_state agent_knowledge_content agent_knowledge_mount capability_authority capability_shared capability_private core_runner_socket core_runner_installs core_runner_workspaces core_runner_state)

write_fixture() {
  local dir=$1 key
  mkdir -m 700 -- "$dir"
  {
    printf 'DIREXTALK_SPLIT_STACK_NAME=%s\nDIREXTALK_RUNNER_PREP_MACHINE_ID=%s\nDIREXTALK_RUNNER_PREP_DOCKER_ENGINE_ID=%s\n' "$stack" "$machine" "$engine"
    printf 'DIREXTALK_AGENT_INSTANCE_ID=11111111-1111-4111-8111-111111111111\nDIREXTALK_MESSAGE_SERVER_INSTANCE_ID=22222222-2222-4222-8222-222222222222\nDIREXTALK_ACCOUNT_GENERATION=1\n'
    printf 'DIREXTALK_EXTENSION_RUNNER_UNIT=dirextalk-extension-runner@%s.service\nDIREXTALK_CORE_RUNNER_UNIT=dirextalk-core-runner@%s.service\n' "$stack" "$stack"
    printf 'DIREXTALK_EXTENSION_RUNNER_FRAGMENT_PATH=%s\nDIREXTALK_CORE_RUNNER_FRAGMENT_PATH=%s\nDIREXTALK_EXTENSION_RUNNER_FRAGMENT_SHA256=%s\nDIREXTALK_CORE_RUNNER_FRAGMENT_SHA256=%s\n' "$fragment" "$fragment" "$fragment_hash" "$fragment_hash"
    printf 'DIREXTALK_EXTENSION_CGROUP_PARENT=%s-extension.slice\nDIREXTALK_CORE_RUNNER_CGROUP_PARENT=%s-core-runner.slice\n' "$stack" "$stack"
    printf 'DIREXTALK_EXTENSION_CONTROL_GROUP=/d.slice/%s.slice/%s-extension.slice/dirextalk-extension-runner@%s.service\nDIREXTALK_CORE_RUNNER_CONTROL_GROUP=/d.slice/%s.slice/%s-core-runner.slice/dirextalk-core-runner@%s.service\n' "$stack" "$stack" "$stack" "$stack" "$stack" "$stack"
    printf 'DIREXTALK_EXTENSION_CGROUP_ROOT=/sys/fs/cgroup/d.slice/%s.slice/%s-extension.slice/dirextalk-extension-runner@%s.service\nDIREXTALK_CORE_RUNNER_CGROUP_ROOT=/sys/fs/cgroup/d.slice/%s.slice/%s-core-runner.slice/dirextalk-core-runner@%s.service\n' "$stack" "$stack" "$stack" "$stack" "$stack" "$stack"
    printf 'DIREXTALK_EXTENSION_CGROUP_PARENT_ROOT=/sys/fs/cgroup/d.slice/%s.slice/%s-extension.slice\nDIREXTALK_CORE_RUNNER_CGROUP_PARENT_ROOT=/sys/fs/cgroup/d.slice/%s.slice/%s-core-runner.slice\n' "$stack" "$stack" "$stack" "$stack"
    printf 'DIREXTALK_EXTENSION_CGROUP_PARENT_PROCS=/sys/fs/cgroup/d.slice/%s.slice/%s-extension.slice/cgroup.procs\nDIREXTALK_CORE_RUNNER_CGROUP_PARENT_PROCS=/sys/fs/cgroup/d.slice/%s.slice/%s-core-runner.slice/cgroup.procs\n' "$stack" "$stack" "$stack" "$stack"
    for key in "${network_keys[@]}"; do
      printf 'DIREXTALK_%s_NETWORK=%s-%s\n' "$(printf '%s' "$key" | tr '[:lower:]' '[:upper:]' | tr '-' '_')" "$stack" "$key"
    done
  } >"$dir/.env"
  for key in "${volume_keys[@]}"; do
    case "$key" in
      agent_secrets) env_key=DIREXTALK_AGENT_SECRET_VOLUME ;;
      agent_extension_socket) env_key=DIREXTALK_AGENT_SOCKET_VOLUME ;;
      agent_extension_install) env_key=DIREXTALK_AGENT_INSTALL_VOLUME ;;
      agent_extension_staging) env_key=DIREXTALK_AGENT_STAGING_VOLUME ;;
      agent_runner_workspaces) env_key=DIREXTALK_AGENT_RUNNER_WORKSPACE_VOLUME ;;
      core_runner_installs) env_key=DIREXTALK_CORE_RUNNER_INSTALL_VOLUME ;;
      core_runner_workspaces) env_key=DIREXTALK_CORE_RUNNER_WORKSPACE_VOLUME ;;
      *) env_key=DIREXTALK_$(printf '%s' "$key" | tr '[:lower:]' '[:upper:]' | tr '-' '_')_VOLUME ;;
    esac
    printf '%s=%s-%s\n' "$env_key" "$stack" "$key" >>"$dir/.env"
  done
  chmod 400 -- "$dir/.env"
  {
    printf '# dirextalk-split-manifest-v1\nstack_name=%s\nstack_nonce=%s\nagent_instance_id=11111111-1111-4111-8111-111111111111\nmessage_instance_id=22222222-2222-4222-8222-222222222222\naccount_generation=1\nrunner.machine_id=%s\nrunner.docker_engine_id=%s\n' "$stack" "${stack#d-}" "$machine" "$engine"
    printf 'runner.extension.unit=dirextalk-extension-runner@%s.service\nrunner.extension.parent=%s-extension.slice\nrunner.extension.control_group=/d.slice/%s.slice/%s-extension.slice/dirextalk-extension-runner@%s.service\nrunner.extension.fragment_path=%s\nrunner.extension.fragment_sha256=%s\n' "$stack" "$stack" "$stack" "$stack" "$stack" "$fragment" "$fragment_hash"
    printf 'runner.extension.parent_root=/sys/fs/cgroup/d.slice/%s.slice/%s-extension.slice\nrunner.extension.parent_procs=/sys/fs/cgroup/d.slice/%s.slice/%s-extension.slice/cgroup.procs\nrunner.extension.parent_procs_owner=65531:65531\nrunner.extension.parent_procs_mode=644\n' "$stack" "$stack" "$stack" "$stack"
    printf 'runner.core.unit=dirextalk-core-runner@%s.service\nrunner.core.parent=%s-core-runner.slice\nrunner.core.control_group=/d.slice/%s.slice/%s-core-runner.slice/dirextalk-core-runner@%s.service\nrunner.core.fragment_path=%s\nrunner.core.fragment_sha256=%s\n' "$stack" "$stack" "$stack" "$stack" "$stack" "$fragment" "$fragment_hash"
    printf 'runner.core.parent_root=/sys/fs/cgroup/d.slice/%s.slice/%s-core-runner.slice\nrunner.core.parent_procs=/sys/fs/cgroup/d.slice/%s.slice/%s-core-runner.slice/cgroup.procs\nrunner.core.parent_procs_owner=65530:65530\nrunner.core.parent_procs_mode=644\n' "$stack" "$stack" "$stack" "$stack"
    for key in "${network_keys[@]}"; do printf 'resource.network.%s=%s-%s\n' "$key" "$stack" "$key"; done
    for key in "${volume_keys[@]}"; do printf 'resource.volume.%s=%s-%s\n' "$key" "$stack" "$key"; done
  } >"$dir/.manifest"
  chmod 400 -- "$dir/.manifest"
  mkdir -p -- "$dir/bin" "$dir/state"
  cat >"$dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = --context ]; then
  [ "$2" = default ] || exit 2
  shift 2
fi
case "$1" in
  context)
    case "$2" in
      inspect) printf 'unix:///var/run/docker.sock\n' ;;
      show) printf '%s\n' "${DIREXTALK_FAKE_CONTEXT:-default}" ;;
      *) exit 2 ;;
    esac
    ;;
  info) printf '%s\n' "$DIREXTALK_FAKE_ENGINE" ;;
  ps)
    case "${DIREXTALK_FAKE_CONTAINER:-}" in
      label) [[ "$*" = *"label=com.docker.compose.project="* ]] && printf 'container-id\n' ;;
      prefix) [[ "$*" = *"name=^/"* ]] && printf 'container-id\n' ;;
    esac
    :
    ;;
  network|volume)
    [ "${DIREXTALK_FAKE_INSPECT_ERROR:-false}" != true ] || { echo 'daemon unavailable' >&2; exit 125; }
    [ "${DIREXTALK_FAKE_OBJECT:-}" = "$1" ] && printf '[{"Name":"object"}]\n' || { echo "Error response from daemon: No such $1: $3" >&2; exit 1; }
    ;;
  *) exit 2 ;;
esac
EOF
  chmod 755 -- "$dir/bin/docker"
  cat >"$dir/bin/apparmor_parser" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --remove ] || exit 2
: >"$DIREXTALK_APPARMOR_LOADED_PROFILES"
EOF
  chmod 755 -- "$dir/bin/apparmor_parser"
  cat >"$dir/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
log=$DIREXTALK_FAKE_SYSTEMCTL_LOG
printf '%s\n' "$*" >>"$log"
unit=${2:-}
[ "$1" != disable ] || unit=${!#}
marker=$DIREXTALK_FAKE_STATE/disabled-${unit//[^A-Za-z0-9]/_}
if [ "$1" = disable ]; then touch "$marker"; exit 0; fi
if [ "$1" = is-enabled ]; then
  if [ -e "$marker" ] || [ "${DIREXTALK_FAKE_DISABLED_ACTIVE_UNIT:-}" = "$unit" ]; then printf 'disabled\n'; exit 1; fi
  printf 'enabled\n'; exit 0
fi
if [ "$1" = is-active ]; then [ -e "$marker" ] && { printf 'inactive\n'; exit 3; } || { printf 'active\n'; exit 0; }; fi
property=${3#--property=}
case "$property" in
  LoadState) printf 'loaded\n' ;;
  FragmentPath) printf '%s\n' "$DIREXTALK_FAKE_FRAGMENT" ;;
  ControlGroup) case "$unit" in *extension*) printf '/d.slice/%s.slice/%s-extension.slice/dirextalk-extension-runner@%s.service\n' "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" ;; *) printf '/d.slice/%s.slice/%s-core-runner.slice/dirextalk-core-runner@%s.service\n' "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" ;; esac ;;
  Slice) case "$unit" in *extension*) printf '%s-extension.slice\n' "$DIREXTALK_FAKE_STACK" ;; *) printf '%s-core-runner.slice\n' "$DIREXTALK_FAKE_STACK" ;; esac ;;
  *) printf '\n' ;;
esac
EOF
  chmod 755 -- "$dir/bin/systemctl"
  : >"$dir/state/systemctl.log"
  : >"$dir/state/docker.log"
}

fixture=$tmp/absent
write_fixture "$fixture"
printf 'prior audit entry\n' >"$fixture/.provision-failure-cleanup.log"
chmod 400 -- "$fixture/.provision-failure-cleanup.log"
export PATH=$fixture/bin:$PATH DIREXTALK_FAKE_ENGINE=$engine DIREXTALK_FAKE_STACK=$stack DIREXTALK_FAKE_FRAGMENT=$fragment DIREXTALK_FAKE_STATE=$fixture/state DIREXTALK_FAKE_SYSTEMCTL_LOG=$fixture/state/systemctl.log
unset DIREXTALK_FAKE_OBJECT DIREXTALK_FAKE_INSPECT_ERROR DIREXTALK_FAKE_CONTAINER DIREXTALK_FAKE_CONTEXT DIREXTALK_FAKE_DISABLED_ACTIVE_UNIT
"$script" "$fixture" >/dev/null
grep -Fq "disable --now dirextalk-extension-runner@$stack.service" "$fixture/state/systemctl.log"
grep -Fq "disable --now dirextalk-core-runner@$stack.service" "$fixture/state/systemctl.log"
grep -Fqx 'prior audit entry' "$fixture/.provision-failure-cleanup.log"
[ "$(stat -c '%a' "$fixture/.provision-failure-cleanup.log")" = 400 ]

already_clean_fixture=$tmp/already-clean
write_fixture "$already_clean_fixture"
touch "$already_clean_fixture/state/disabled-dirextalk_extension_runner_${stack//[^A-Za-z0-9]/_}_service"
touch "$already_clean_fixture/state/disabled-dirextalk_core_runner_${stack//[^A-Za-z0-9]/_}_service"
export DIREXTALK_FAKE_STATE=$already_clean_fixture/state DIREXTALK_FAKE_SYSTEMCTL_LOG=$already_clean_fixture/state/systemctl.log
set +e
"$script" "$already_clean_fixture" >/dev/null 2>"$already_clean_fixture/error"
status=$?
set -e
[ "$status" -eq 3 ]
if grep -q 'disable --now' "$already_clean_fixture/state/systemctl.log"; then
  echo 'already-clean path mutated runner units' >&2
  exit 1
fi

disabled_active_fixture=$tmp/disabled-active
write_fixture "$disabled_active_fixture"
core_unit=dirextalk-core-runner@$stack.service
extension_unit=dirextalk-extension-runner@$stack.service
touch "$disabled_active_fixture/state/disabled-${core_unit//[^A-Za-z0-9]/_}"
export DIREXTALK_FAKE_STATE=$disabled_active_fixture/state DIREXTALK_FAKE_SYSTEMCTL_LOG=$disabled_active_fixture/state/systemctl.log
export DIREXTALK_FAKE_DISABLED_ACTIVE_UNIT=$extension_unit
"$script" "$disabled_active_fixture" >/dev/null
grep -Fq "disable --now $extension_unit" "$disabled_active_fixture/state/systemctl.log"
if grep -Fq "disable --now $core_unit" "$disabled_active_fixture/state/systemctl.log"; then
  echo 'already-inactive Core unit was mutated' >&2
  exit 1
fi
unset DIREXTALK_FAKE_DISABLED_ACTIVE_UNIT

for failure in network-object volume-object inspect label-container prefix-container; do
  fixture=$tmp/$failure
  write_fixture "$fixture"
  fake_object=
  [ "$failure" = network-object ] && fake_object=network
  [ "$failure" = volume-object ] && fake_object=volume
  fake_inspect_error=false
  [ "$failure" = inspect ] && fake_inspect_error=true
  fake_container=
  [ "$failure" = label-container ] && fake_container=label
  [ "$failure" = prefix-container ] && fake_container=prefix
  export DIREXTALK_FAKE_OBJECT=$fake_object DIREXTALK_FAKE_INSPECT_ERROR=$fake_inspect_error DIREXTALK_FAKE_CONTAINER=$fake_container
  export DIREXTALK_FAKE_STATE=$fixture/state DIREXTALK_FAKE_SYSTEMCTL_LOG=$fixture/state/systemctl.log
  set +e
  "$script" "$fixture" >/dev/null 2>"$fixture/error"
  status=$?
  set -e
  case "$failure" in
    inspect) [ "$status" -eq 1 ] ;;
    *) [ "$status" -eq 3 ] ;;
  esac
  [ -f "$fixture/.provision-failure-cleanup.log" ] || { echo "Docker $failure path did not preserve cleanup log" >&2; exit 1; }
  [ "$(stat -c '%a' "$fixture/.provision-failure-cleanup.log")" = 400 ] || exit 1
  if grep -q 'disable --now' "$fixture/state/systemctl.log"; then
    echo "Docker $failure path mutated runner units" >&2
    exit 1
  fi
done

receipt_fixture=$tmp/receipt
write_fixture "$receipt_fixture"
: >"$receipt_fixture/.cleanup-receipt"
chmod 400 -- "$receipt_fixture/.cleanup-receipt"
set +e
"$script" "$receipt_fixture" >/dev/null 2>"$receipt_fixture/error"
status=$?
set -e
[ "$status" -eq 3 ]
if grep -q 'disable --now' "$receipt_fixture/state/systemctl.log"; then
  echo 'cleanup-receipt path mutated runner units' >&2
  exit 1
fi

tamper_fixture=$tmp/tamper
write_fixture "$tamper_fixture"
chmod 600 -- "$tamper_fixture/.env"
sed -i "s/^DIREXTALK_SPLIT_STACK_NAME=.*/DIREXTALK_SPLIT_STACK_NAME=d-bbbbbbbbbbbbbbbbbbbbbbbbbb/" "$tamper_fixture/.env"
chmod 400 -- "$tamper_fixture/.env"
set +e
"$script" "$tamper_fixture" >/dev/null 2>"$tamper_fixture/error"
status=$?
set -e
[ "$status" -eq 1 ]
if grep -q 'disable --now' "$tamper_fixture/state/systemctl.log"; then
  echo 'tampered env path mutated runner units' >&2
  exit 1
fi

echo 'provision-failure cleanup identity and Docker-absence guards verified'
