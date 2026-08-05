#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/cutover-edge.sh
[ -x "$script" ] || { echo "cutover-edge.sh must be executable" >&2; exit 1; }
bash -n "$script"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-edge-cutover-test.XXXXXX")
cleanup() { rm -rf -- "$tmp_dir"; }
trap cleanup EXIT
mkdir -p "$tmp_dir/bin" "$tmp_dir/state" "$tmp_dir/output"
chmod 700 "$tmp_dir/bin" "$tmp_dir/state" "$tmp_dir/output"

old_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
new_id=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
message_image=docker.io/dirextalk/message-server@sha256:1111111111111111111111111111111111111111111111111111111111111111
agent_image=docker.io/dirextalk/agent@sha256:2222222222222222222222222222222222222222222222222222222222222222
old_image=docker.io/library/caddy@sha256:3333333333333333333333333333333333333333333333333333333333333333
caddy_image=docker.io/library/caddy@sha256:4444444444444444444444444444444444444444444444444444444444444444
old_image_id=sha256:5555555555555555555555555555555555555555555555555555555555555555
network=split-message-public
old_network=old-message-public
agent_network=split-agent-private
old_network_id=old-network-id
data_volume_fingerprint=9a48926b5af33a8a62f311a8f8801ae77c23887775cca9cbf037183bfa688cf1
config_volume_fingerprint=ff9261a91fb2a1409a56e53eb9a55ed0eccf4fa955c79085d7f66332ee56f20b

cat >"$tmp_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DIREXTALK_MOCK_LOG"
state=$DIREXTALK_MOCK_STATE
old_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
message_id=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
agent_id=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
new_id=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
replacement_id=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
message_image=docker.io/dirextalk/message-server@sha256:1111111111111111111111111111111111111111111111111111111111111111
agent_image=docker.io/dirextalk/agent@sha256:2222222222222222222222222222222222222222222222222222222222222222
old_image=docker.io/library/caddy@sha256:3333333333333333333333333333333333333333333333333333333333333333
caddy_image=docker.io/library/caddy@sha256:4444444444444444444444444444444444444444444444444444444444444444
old_image_id=sha256:5555555555555555555555555555555555555555555555555555555555555555
new_image_id=sha256:6666666666666666666666666666666666666666666666666666666666666666
network=split-message-public
old_network=old-message-public
agent_network=split-agent-private
old_network_id=old-network-id
new_network_id=new-network-id

if [ "$1" = compose ]; then
  shift
  case " $* " in
    *' config --quiet '*)
      [ "${DIREXTALK_MOCK_CONFIG_FAIL:-false}" != true ] || exit 37
      if [ "${DIREXTALK_MOCK_BLOCK_CONFIG:-false}" = true ]; then
        : >"$state/config-blocked"
        while [ ! -f "$state/config-release" ]; do sleep 0.05; done
      fi
      exit 0
      ;;
    *' config --format json '*)
      [ "${DIREXTALK_MOCK_CONFIG_FAIL:-false}" != true ] || exit 37
      case " $* " in
        *edge-compose.yaml*)
          edge_cap_add='["NET_BIND_SERVICE"]'
          case "${DIREXTALK_MOCK_EDGE_CAP_ADD:-}" in
            missing) edge_cap_add='[]' ;;
            extra) edge_cap_add='["NET_BIND_SERVICE","SYS_ADMIN"]' ;;
          esac
          cat <<JSON
{"name":"edge-test","services":{"caddy":{"image":"$caddy_image","networks":["message_public"],"ports":[{"published":"80","target":80},{"published":"443","target":443}],"read_only":true,"cap_drop":["ALL"],"cap_add":$edge_cap_add,"security_opt":["no-new-privileges:true"],"healthcheck":{"test":["CMD-SHELL","wget -q -O - http://127.0.0.1:2019/config/ >/dev/null"]},"volumes":[{"type":"volume","source":"caddy_data","target":"/data"},{"type":"volume","source":"caddy_config","target":"/config"}]}},"networks":{"message_public":{"name":"$network","external":true}},"volumes":{"caddy_data":{"name":"caddy-data","external":true},"caddy_config":{"name":"caddy-config","external":true}}}
JSON
          ;;
        *)
          cat <<JSON
{"name":"message-test","services":{"message-server":{"image":"$message_image","ports":[{"published":"18008","host_ip":"127.0.0.1"},{"published":"18448","host_ip":"127.0.0.1"}]},"agent":{"image":"$agent_image"}}}
JSON
          ;;
      esac
      exit 0
      ;;
    *' ps -q message-server '*) printf '%s\n' "$message_id"; exit 0 ;;
    *' ps -q agent '*) printf '%s\n' "$agent_id"; exit 0 ;;
    *' create caddy '*) : >"$state/new-created"; printf '%s\n' "$new_id" >/dev/null; exit 0 ;;
    *' ps -q caddy '*) [ -f "$state/new-created" ] || [ -f "$state/new-up" ] && printf '%s\n' "$new_id"; exit 0 ;;
    *' ps -aq caddy '*)
      if [ "${DIREXTALK_MOCK_EDGE_EXISTS:-false}" = true ] || [ -f "$state/new-created" ]; then
        printf '%s\n' "$new_id"
      fi
      exit 0
      ;;
    *' up -d --wait caddy '*) [ "${DIREXTALK_MOCK_UP_FAIL:-false}" != true ] || exit 41; : >"$state/new-up"; exit 0 ;;
    *) exit 99 ;;
  esac
fi

case "$1" in
  info)
    printf 'engine-cutover-test\n'
    ;;
  inspect)
    id=${2:-}
    if [ "$id" = "$old_id" ] && [ "${DIREXTALK_MOCK_REPLACED:-false}" = true ]; then id=$replacement_id; fi
    status=running
    [ -f "$state/old-stopped" ] && [ "$id" = "$old_id" ] && status=exited
    [ -f "$state/old-started" ] && [ "$id" = "$old_id" ] && status=running
    if [ "$id" = "$old_id" ] && [ "$status" = exited ] && [ "${DIREXTALK_MOCK_POST_STOP_INSPECT_FAIL:-false}" = true ] && [ ! -f "$state/post-stop-inspect-failed" ]; then
      : >"$state/post-stop-inspect-failed"
      exit 91
    fi
    case "$id" in
      "$old_id"|"$replacement_id")
        cat <<JSON
[{"Id":"$id","Image":"$old_image_id","Config":{"Image":"$old_image","Healthcheck":{"Test":["CMD-SHELL","wget -q -O - http://127.0.0.1:2019/config/ >/dev/null"]},"Labels":{"com.docker.compose.project":"edge-old","com.docker.compose.service":"caddy"}},"NetworkSettings":{"Networks":{"$old_network":{}}},"Mounts":[{"Type":"volume","Name":"caddy-data","Destination":"/data"},{"Type":"volume","Name":"caddy-config","Destination":"/config"}],"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}]},"ReadonlyRootfs":true,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges:true"]},"State":{"Status":"$status","Health":{"Status":"healthy"}}}]
JSON
        ;;
      "$message_id")
        cat <<JSON
[{"Id":"$message_id","Image":"sha256:1111111111111111111111111111111111111111111111111111111111111111","Config":{"Image":"$message_image","Labels":{"com.docker.compose.project":"message-test","com.docker.compose.service":"message-server"}},"NetworkSettings":{"Networks":{"$network":{}}},"Mounts":[],"HostConfig":{"PortBindings":{"8008/tcp":[{"HostPort":"18008","HostIp":"127.0.0.1"}],"8448/tcp":[{"HostPort":"18448","HostIp":"127.0.0.1"}]}},"State":{"Status":"running","Health":{"Status":"healthy"}}}]
JSON
        ;;
      "$agent_id")
        cat <<JSON
[{"Id":"$agent_id","Image":"sha256:2222222222222222222222222222222222222222222222222222222222222222","Config":{"Image":"$agent_image","Labels":{"com.docker.compose.project":"message-test","com.docker.compose.service":"agent"}},"NetworkSettings":{"Networks":{"$agent_network":{}}},"Mounts":[],"HostConfig":{"PortBindings":{}},"State":{"Status":"running","Health":{"Status":"healthy"}}}]
JSON
        ;;
      "$new_id")
        status=created
        [ -f "$state/new-up" ] && status=running
        [ -f "$state/new-removed" ] && status=exited
        [ "${DIREXTALK_MOCK_NEW_RESTARTING:-false}" = true ] && status=restarting
        new_cap_add='["NET_BIND_SERVICE"]'
        case "${DIREXTALK_MOCK_NEW_INSPECT_CAP_ADD:-}" in
          missing) new_cap_add='[]' ;;
          extra) new_cap_add='["NET_BIND_SERVICE","SYS_ADMIN"]' ;;
        esac
        cat <<JSON
[{"Id":"$new_id","Image":"$new_image_id","Config":{"Image":"$caddy_image","Healthcheck":{"Test":["CMD-SHELL","wget -q -O - http://127.0.0.1:2019/config/ >/dev/null"]},"Labels":{"com.docker.compose.project":"edge-test","com.docker.compose.service":"caddy"}},"NetworkSettings":{"Networks":{"$network":{}}},"Mounts":[{"Type":"volume","Name":"caddy-data","Destination":"/data"},{"Type":"volume","Name":"caddy-config","Destination":"/config"}],"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}]},"ReadonlyRootfs":true,"CapDrop":["ALL"],"CapAdd":$new_cap_add,"SecurityOpt":["no-new-privileges:true"]},"State":{"Status":"$status","Health":{"Status":"healthy"}}}]
JSON
        ;;
      *) exit 1 ;;
    esac
    ;;
  network)
    [ "${2:-}" = inspect ] || exit 1
    network_name=${3:-}
    case "$network_name" in
      "$network")
        containers="\"$message_id\":{}"
        [ -f "$state/new-created" ] || [ -f "$state/new-up" ] && containers="$containers,\"$new_id\":{}"
        printf '[{"Id":"%s","Name":"%s","Labels":{"com.docker.compose.project":"message-test","com.docker.compose.network":"message_public"},"Containers":{%s}}]\n' "$new_network_id" "$network" "$containers"
        ;;
      "$old_network") printf '[{"Id":"%s","Name":"%s","Labels":{},"Containers":{"%s":{}}}]\n' "$old_network_id" "$old_network" "$old_id" ;;
      "$agent_network") printf '[{"Id":"agent-network-id","Name":"%s","Labels":{"com.docker.compose.project":"message-test"},"Containers":{"%s":{}}}]\n' "$agent_network" "$agent_id" ;;
      *) exit 1 ;;
    esac
    ;;
  image)
    [ "${2:-}" = inspect ] || exit 1
    case "${3:-}" in
      "$old_image") printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$old_image_id" "$old_image" ;;
      "$caddy_image") printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$new_image_id" "$caddy_image" ;;
      "$message_image") printf '[{"Id":"sha256:1111111111111111111111111111111111111111111111111111111111111111","RepoDigests":["%s"]}]\n' "$message_image" ;;
      "$agent_image") printf '[{"Id":"sha256:2222222222222222222222222222222222222222222222222222222222222222","RepoDigests":["%s"]}]\n' "$agent_image" ;;
      *) exit 1 ;;
    esac
    ;;
  volume)
    [ "${2:-}" = inspect ] || exit 1
    case "${3:-}" in
      caddy-data) printf '[{"Name":"caddy-data","Driver":"local","Scope":"local","CreatedAt":"2026-08-05T00:00:00Z","Mountpoint":"/var/lib/docker/volumes/caddy-data/_data","Options":{},"Labels":{}}]\n' ;;
      caddy-config) printf '[{"Name":"caddy-config","Driver":"local","Scope":"local","CreatedAt":"2026-08-05T00:00:01Z","Mountpoint":"/var/lib/docker/volumes/caddy-config/_data","Options":{},"Labels":{}}]\n' ;;
      *) exit 1 ;;
    esac
    ;;
  stop)
    case "${2:-}" in
      "$old_id")
        : >"$state/old-stopped"
        [ "${DIREXTALK_MOCK_STOP_APPLIED_ERROR:-false}" != true ] || exit 44
        ;;
      "$new_id")
        : >"$state/new-removed"
        ;;
      *) exit 1 ;;
    esac
    exit 0
    ;;
  start)
    case "${2:-}" in
      "$old_id")
        [ "${DIREXTALK_MOCK_START_FAIL:-false}" != true ] || exit 43
        : >"$state/old-started"
        ;;
      "$new_id")
        [ "${DIREXTALK_MOCK_NEW_START_FAIL:-false}" != true ] || exit 45
        : >"$state/new-up"
        ;;
      *) exit 1 ;;
    esac
    ;;
  rm)
    [ "${2:-}" = -f ] && [ "${3:-}" = "$new_id" ] || exit 1
    [ "${DIREXTALK_MOCK_RM_FAIL:-false}" != true ] || exit 46
    : >"$state/new-removed"
    exit 0
    ;;
  *) exit 0 ;;
esac
EOF
chmod 755 "$tmp_dir/bin/docker"

cat >"$tmp_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DIREXTALK_CURL_LOG"
case " $* " in
  *'https://edge.example/_'*) [ "${DIREXTALK_MOCK_PUBLIC_FAIL:-false}" != true ] || exit 51 ;;
esac
exit 0
EOF
chmod 755 "$tmp_dir/bin/curl"

cat >"$tmp_dir/bin/kill-lock-holder" <<'EOF'
#!/usr/bin/env bash
set +e
script=$1
shift
"$script" "$@" >/dev/null 2>&1 &
child_pid=$!
for _ in $(seq 1 100); do
  [ -f "$DIREXTALK_CUTOVER_TEST_LOCK_READY_FILE" ] && break
  sleep 0.05
done
[ -f "$DIREXTALK_CUTOVER_TEST_LOCK_READY_FILE" ] || exit 1
kill -KILL "$child_pid"
wait "$child_pid" 2>/dev/null
child_status=$?
[ "$child_status" -ne 0 ]
EOF
chmod 755 "$tmp_dir/bin/kill-lock-holder"

stack_env=$tmp_dir/stack.env
edge_env=$tmp_dir/edge.env
receipt=$tmp_dir/active.receipt
caddyfile=$tmp_dir/Caddyfile
tls_cert=$tmp_dir/message.crt
test_host_name=$(hostname)
test_machine_id=$(tr -d '[:space:]' </etc/machine-id)
printf 'reverse_proxy message-server:8008\n' >"$caddyfile"
printf 'test-certificate\n' >"$tls_cert"
cat >"$stack_env" <<EOF
DIREXTALK_SPLIT_STACK_NAME=message-test
DIREXTALK_MESSAGE_PUBLIC_NETWORK=$network
DIREXTALK_MESSAGE_PRIVATE_NETWORK=split-message-private
DIREXTALK_AGENT_PRIVATE_NETWORK=$agent_network
DIREXTALK_MESSAGE_HTTP_BIND=18008
DIREXTALK_MESSAGE_HTTPS_BIND=18448
DIREXTALK_MESSAGE_SERVER_NAME=edge.example
DIREXTALK_MESSAGE_TLS_MODE=external
DIREXTALK_MESSAGE_TLS_CERT_FILE=$tls_cert
DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=$message_image
DIREXTALK_AGENT_IMAGE_IMMUTABLE=$agent_image
DIREXTALK_MESSAGE_COMPOSE_FILE=$script_dir/../compose.yaml
EOF
cat >"$edge_env" <<EOF
DIREXTALK_EDGE_STACK_NAME=edge-test
DIREXTALK_PUBLIC_DOMAIN=edge.example
DIREXTALK_MESSAGE_PUBLIC_NETWORK=$network
DIREXTALK_CADDY_IMAGE_IMMUTABLE=$caddy_image
DIREXTALK_CADDY_DATA_VOLUME=caddy-data
DIREXTALK_CADDY_CONFIG_VOLUME=caddy-config
DIREXTALK_CADDYFILE=$caddyfile
DIREXTALK_EDGE_COMPOSE_FILE=$script_dir/../edge-compose.yaml
EOF
cat >"$receipt" <<EOF
# dirextalk-edge-receipt-v1
receipt_kind=adopted-edge-v1
owner_uid=$(id -u)
operation_id=adopt-op
revision=rev-1
host_name=$test_host_name
machine_id=$test_machine_id
docker_engine_id=engine-cutover-test
edge_stack_name=edge-old
message_stack_name=message-old
domain=edge.example
message_public_network=$old_network
active_caddy_container_id=$old_id
active_caddy_project=edge-old
active_caddy_image=$old_image
active_caddy_image_id=$old_image_id
active_caddy_repo_digest=$old_image
active_caddy_data_volume=caddy-data
active_caddy_data_volume_fingerprint_sha256=$data_volume_fingerprint
active_caddy_config_volume=caddy-config
active_caddy_config_volume_fingerprint_sha256=$config_volume_fingerprint
active_caddy_network=$old_network
active_caddy_network_id=$old_network_id
active_caddy_network_labels_sha256=37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570
active_caddy_http_port=80
active_caddy_https_port=443
EOF
chmod 400 "$stack_env" "$edge_env" "$receipt" "$caddyfile" "$tls_cert"
export PATH="$tmp_dir/bin:$PATH"
export DIREXTALK_DOCKER_BIN=docker DIREXTALK_CURL_BIN=curl
export DIREXTALK_MOCK_LOG=$tmp_dir/docker.log DIREXTALK_CURL_LOG=$tmp_dir/curl.log
export DIREXTALK_MOCK_STATE=$tmp_dir/state

run_case() {
  : >"$DIREXTALK_MOCK_LOG"
  : >"$DIREXTALK_CURL_LOG"
  rm -f -- "$DIREXTALK_MOCK_STATE"/old-stopped "$DIREXTALK_MOCK_STATE"/old-started "$DIREXTALK_MOCK_STATE"/new-created "$DIREXTALK_MOCK_STATE"/new-up "$DIREXTALK_MOCK_STATE"/new-removed "$DIREXTALK_MOCK_STATE"/config-blocked "$DIREXTALK_MOCK_STATE"/config-release "$DIREXTALK_MOCK_STATE"/post-stop-inspect-failed
  unset DIREXTALK_MOCK_CONFIG_FAIL DIREXTALK_MOCK_UP_FAIL DIREXTALK_MOCK_PUBLIC_FAIL DIREXTALK_MOCK_START_FAIL DIREXTALK_MOCK_NEW_START_FAIL DIREXTALK_MOCK_NEW_RESTARTING DIREXTALK_MOCK_STOP_APPLIED_ERROR DIREXTALK_MOCK_RM_FAIL DIREXTALK_MOCK_BLOCK_CONFIG DIREXTALK_MOCK_POST_STOP_INSPECT_FAIL DIREXTALK_MOCK_EDGE_CAP_ADD DIREXTALK_MOCK_NEW_INSPECT_CAP_ADD DIREXTALK_CUTOVER_TEST_BLOCK_AFTER_LOCK DIREXTALK_CUTOVER_TEST_LOCK_READY_FILE DIREXTALK_CUTOVER_TEST_LOCK_RELEASE_FILE DIREXTALK_CUTOVER_TEST_CRASH_AFTER_OLD_STOP DIREXTALK_CUTOVER_TEST_CRASH_AFTER_CANDIDATE_START DIREXTALK_CUTOVER_TEST_CRASH_AFTER_RECEIPT_INSTALL DIREXTALK_MOCK_EDGE_EXISTS DIREXTALK_MOCK_REPLACED
  rm -f -- "$1"
}

success_receipt=$tmp_dir/output/success.receipt
run_case "$success_receipt"
"$script" "$stack_env" "$edge_env" "$receipt" "$success_receipt" >/dev/null
[ "$(stat -c '%a' "$success_receipt")" = 400 ]
grep -Fq "old_caddy_container_id=$old_id" "$success_receipt"
grep -Fq "new_caddy_container_id=$new_id" "$success_receipt"
grep -Fq "active_caddy_container_id=$new_id" "$success_receipt"
grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $new_id" "$DIREXTALK_MOCK_LOG"
if grep -Fq -- "rm -f $new_id" "$DIREXTALK_MOCK_LOG"; then
  echo "successful cutover unexpectedly removed new Caddy" >&2
  exit 1
fi
[ ! -e "$tmp_dir/.cutover-edge-txn" ] || { echo "successful cutover left transaction" >&2; exit 1; }

# Rendered edge hardening requires exactly NET_BIND_SERVICE: missing and extra
# capabilities must fail before the old Caddy stop boundary.
for cap_case in missing extra; do
  cap_receipt=$tmp_dir/output/cap-render-$cap_case.receipt
  run_case "$cap_receipt"
  export DIREXTALK_MOCK_EDGE_CAP_ADD=$cap_case
  if "$script" "$stack_env" "$edge_env" "$receipt" "$cap_receipt" >/dev/null 2>&1; then
    echo "$cap_case edge capability render unexpectedly succeeded" >&2
    exit 1
  fi
  unset DIREXTALK_MOCK_EDGE_CAP_ADD
  [ ! -e "$cap_receipt" ]
  if grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"; then
    echo "$cap_case edge capability render crossed the old stop boundary" >&2
    exit 1
  fi
done

# Container hardening independently rejects missing and extra CapAdd entries
# even when the rendered Compose definition is correct.
for cap_case in missing extra; do
  cap_receipt=$tmp_dir/output/cap-inspect-$cap_case.receipt
  run_case "$cap_receipt"
  export DIREXTALK_MOCK_NEW_INSPECT_CAP_ADD=$cap_case
  if "$script" "$stack_env" "$edge_env" "$receipt" "$cap_receipt" >/dev/null 2>&1; then
    echo "$cap_case edge capability inspect unexpectedly succeeded" >&2
    exit 1
  fi
  unset DIREXTALK_MOCK_NEW_INSPECT_CAP_ADD
  [ ! -e "$cap_receipt" ]
  grep -Fq -- "rm -f $new_id" "$DIREXTALK_MOCK_LOG"
  if grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"; then
    echo "$cap_case edge capability inspect crossed the old stop boundary" >&2
    exit 1
  fi
done

sigkill_first=$tmp_dir/output/sigkill-first.receipt
sigkill_second=$tmp_dir/output/sigkill-second.receipt
sigkill_ready=$tmp_dir/state/sigkill-lock-ready
sigkill_release=$tmp_dir/state/sigkill-lock-release
run_case "$sigkill_first"
export DIREXTALK_CUTOVER_TEST_BLOCK_AFTER_LOCK=true
export DIREXTALK_CUTOVER_TEST_LOCK_READY_FILE=$sigkill_ready
export DIREXTALK_CUTOVER_TEST_LOCK_RELEASE_FILE=$sigkill_release
"$tmp_dir/bin/kill-lock-holder" "$script" "$stack_env" "$edge_env" "$receipt" "$sigkill_first"
unset DIREXTALK_CUTOVER_TEST_BLOCK_AFTER_LOCK DIREXTALK_CUTOVER_TEST_LOCK_READY_FILE DIREXTALK_CUTOVER_TEST_LOCK_RELEASE_FILE
[ ! -e "$sigkill_first" ]
"$script" "$stack_env" "$edge_env" "$receipt" "$sigkill_second" >/dev/null
[ "$(stat -c '%a' "$sigkill_second")" = 400 ]

concurrent_first=$tmp_dir/output/concurrent-first.receipt
concurrent_second=$tmp_dir/output/concurrent-second.receipt
run_case "$concurrent_first"
export DIREXTALK_MOCK_BLOCK_CONFIG=true
"$script" "$stack_env" "$edge_env" "$receipt" "$concurrent_first" >"$tmp_dir/concurrent-first.stdout" 2>"$tmp_dir/concurrent-first.stderr" &
first_pid=$!
for _ in $(seq 1 100); do
  [ -f "$DIREXTALK_MOCK_STATE/config-blocked" ] && break
  sleep 0.05
done
[ -f "$DIREXTALK_MOCK_STATE/config-blocked" ]
if "$script" "$stack_env" "$edge_env" "$receipt" "$concurrent_second" >/dev/null 2>&1; then
  echo "concurrent public-edge cutover unexpectedly acquired the scope lock" >&2
  exit 1
fi
if grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"; then
  echo "concurrent lock rejection mutated the public edge" >&2
  exit 1
fi
: >"$DIREXTALK_MOCK_STATE/config-release"
wait "$first_pid"
unset DIREXTALK_MOCK_BLOCK_CONFIG
[ "$(stat -c '%a' "$concurrent_first")" = 400 ]
[ ! -e "$concurrent_second" ]
[ -f "$tmp_dir/.dirextalk-edge-public.lock" ]
[ "$(stat -c '%a' "$tmp_dir/.dirextalk-edge-public.lock")" = 600 ]

existing_receipt=$tmp_dir/output/existing.receipt
run_case "$existing_receipt"
printf 'prior-audit-receipt\n' >"$existing_receipt"
chmod 400 "$existing_receipt"
if "$script" "$stack_env" "$edge_env" "$receipt" "$existing_receipt" >/dev/null 2>&1; then
  echo "existing output receipt unexpectedly overwritten" >&2
  exit 1
fi
grep -Fq -- 'prior-audit-receipt' "$existing_receipt"
if grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"; then
  echo "existing receipt rejection mutated the edge" >&2
  exit 1
fi

volume_mismatch_env=$tmp_dir/edge-volume-mismatch.env
sed 's/^DIREXTALK_CADDY_DATA_VOLUME=.*/DIREXTALK_CADDY_DATA_VOLUME=other-caddy-data/' "$edge_env" >"$volume_mismatch_env"
chmod 400 "$volume_mismatch_env"
volume_receipt=$tmp_dir/output/volume-mismatch.receipt
run_case "$volume_receipt"
if "$script" "$stack_env" "$volume_mismatch_env" "$receipt" "$volume_receipt" >/dev/null 2>&1; then
  echo "Caddy volume replacement unexpectedly accepted" >&2
  exit 1
fi
[ ! -e "$volume_receipt" ]
if grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"; then
  echo "volume mismatch rejection mutated the edge" >&2
  exit 1
fi

infra_receipt=$tmp_dir/output/infra.receipt
run_case "$infra_receipt"
export DIREXTALK_MOCK_CONFIG_FAIL=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$infra_receipt" >/dev/null 2>&1; then
  echo "compose render infrastructure failure unexpectedly succeeded" >&2
  exit 1
fi
[ ! -e "$infra_receipt" ]
if grep -Eq -- "stop $old_id|up -d --wait caddy" "$DIREXTALK_MOCK_LOG"; then
  echo "preflight infrastructure failure mutated the edge" >&2
  exit 1
fi

up_receipt=$tmp_dir/output/up-failure.receipt
run_case "$up_receipt"
export DIREXTALK_MOCK_NEW_START_FAIL=true
export DIREXTALK_MOCK_RM_FAIL=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$up_receipt" >/dev/null 2>&1; then
  echo "edge startup failure unexpectedly succeeded" >&2
  exit 1
fi
[ ! -e "$up_receipt" ]
grep -Fq -- "start $new_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "rm -f $new_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "stop $new_id" "$DIREXTALK_MOCK_LOG"
[ ! -e "$tmp_dir/.cutover-edge-txn" ]

crash_old_receipt=$tmp_dir/output/crash-old-stop.receipt
run_case "$crash_old_receipt"
export DIREXTALK_CUTOVER_TEST_CRASH_AFTER_OLD_STOP=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$crash_old_receipt" >/dev/null 2>&1; then
  echo "old-stop crash injection unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_CUTOVER_TEST_CRASH_AFTER_OLD_STOP
[ -f "$tmp_dir/.cutover-edge-txn/journal" ]
"$script" "$stack_env" "$edge_env" "$receipt" "$crash_old_receipt" >/dev/null
[ "$(stat -c '%a' "$crash_old_receipt")" = 400 ]
[ ! -e "$tmp_dir/.cutover-edge-txn" ]

uncertain_stop_receipt=$tmp_dir/output/uncertain-stop.receipt
run_case "$uncertain_stop_receipt"
export DIREXTALK_MOCK_POST_STOP_INSPECT_FAIL=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$uncertain_stop_receipt" >/dev/null 2>&1; then
  echo "post-stop inspect uncertainty unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_MOCK_POST_STOP_INSPECT_FAIL
[ -f "$tmp_dir/.cutover-edge-txn/journal" ]
[ -f "$DIREXTALK_MOCK_STATE/new-created" ]
if grep -Fq -- "rm -f $new_id" "$DIREXTALK_MOCK_LOG"; then
  echo "post-stop inspect uncertainty removed the candidate" >&2
  exit 1
fi
"$script" "$stack_env" "$edge_env" "$receipt" "$uncertain_stop_receipt" >/dev/null
[ "$(stat -c '%a' "$uncertain_stop_receipt")" = 400 ]
[ ! -e "$tmp_dir/.cutover-edge-txn" ]

crash_start_receipt=$tmp_dir/output/crash-candidate-start.receipt
run_case "$crash_start_receipt"
export DIREXTALK_CUTOVER_TEST_CRASH_AFTER_CANDIDATE_START=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$crash_start_receipt" >/dev/null 2>&1; then
  echo "candidate-start crash injection unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_CUTOVER_TEST_CRASH_AFTER_CANDIDATE_START
[ -f "$tmp_dir/.cutover-edge-txn/journal" ]
"$script" "$stack_env" "$edge_env" "$receipt" "$crash_start_receipt" >/dev/null
[ "$(stat -c '%a' "$crash_start_receipt")" = 400 ]
[ ! -e "$tmp_dir/.cutover-edge-txn" ]

# A journal-bound restarting candidate with legacy stopped must be removed by
# exact ID, old Caddy restored, and recovery must return negative.
restarting_recovery_receipt=$tmp_dir/output/restarting-recovery.receipt
run_case "$restarting_recovery_receipt"
export DIREXTALK_CUTOVER_TEST_CRASH_AFTER_CANDIDATE_START=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$restarting_recovery_receipt" >/dev/null 2>&1; then
  echo "restarting recovery crash injection unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_CUTOVER_TEST_CRASH_AFTER_CANDIDATE_START
: >"$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_NEW_RESTARTING=true
export DIREXTALK_MOCK_NEW_INSPECT_CAP_ADD=missing
if "$script" "$stack_env" "$edge_env" "$receipt" "$restarting_recovery_receipt" >/dev/null 2>&1; then
  echo "restarting cutover recovery unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_MOCK_NEW_RESTARTING DIREXTALK_MOCK_NEW_INSPECT_CAP_ADD
grep -Fq -- "rm -f $new_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $old_id" "$DIREXTALK_MOCK_LOG"
[ ! -e "$restarting_recovery_receipt" ]
[ ! -e "$tmp_dir/.cutover-edge-txn" ]

# A journal-bound created candidate whose recovery start fails must take the
# same exact rollback path and leave no transaction or output receipt.
created_fail_recovery_receipt=$tmp_dir/output/created-start-fail-recovery.receipt
run_case "$created_fail_recovery_receipt"
export DIREXTALK_CUTOVER_TEST_CRASH_AFTER_OLD_STOP=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$created_fail_recovery_receipt" >/dev/null 2>&1; then
  echo "created start-fail crash injection unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_CUTOVER_TEST_CRASH_AFTER_OLD_STOP
: >"$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_NEW_START_FAIL=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$created_fail_recovery_receipt" >/dev/null 2>&1; then
  echo "created start-fail cutover recovery unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_MOCK_NEW_START_FAIL
grep -Fq -- "start $new_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "rm -f $new_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $old_id" "$DIREXTALK_MOCK_LOG"
[ ! -e "$created_fail_recovery_receipt" ]
[ ! -e "$tmp_dir/.cutover-edge-txn" ]

crash_receipt_install=$tmp_dir/output/crash-receipt-install.receipt
run_case "$crash_receipt_install"
export DIREXTALK_CUTOVER_TEST_CRASH_AFTER_RECEIPT_INSTALL=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$crash_receipt_install" >/dev/null 2>&1; then
  echo "receipt-install crash injection unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_CUTOVER_TEST_CRASH_AFTER_RECEIPT_INSTALL
[ -f "$tmp_dir/.cutover-edge-txn/journal" ]
"$script" "$stack_env" "$edge_env" "$receipt" "$crash_receipt_install" >/dev/null
[ "$(stat -c '%a' "$crash_receipt_install")" = 400 ]
[ ! -e "$tmp_dir/.cutover-edge-txn" ]

stop_applied_receipt=$tmp_dir/output/stop-applied-error.receipt
run_case "$stop_applied_receipt"
export DIREXTALK_MOCK_STOP_APPLIED_ERROR=true
"$script" "$stack_env" "$edge_env" "$receipt" "$stop_applied_receipt" >/dev/null
[ "$(stat -c '%a' "$stop_applied_receipt")" = 400 ]
grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $new_id" "$DIREXTALK_MOCK_LOG"

rollback_receipt=$tmp_dir/output/rollback.receipt
run_case "$rollback_receipt"
export DIREXTALK_MOCK_PUBLIC_FAIL=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$rollback_receipt" >/dev/null 2>&1; then
  echo "public verification failure unexpectedly succeeded" >&2
  exit 1
fi
[ ! -e "$rollback_receipt" ]
grep -Fq -- "rm -f $new_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $old_id" "$DIREXTALK_MOCK_LOG"

replacement_receipt=$tmp_dir/output/replacement.receipt
run_case "$replacement_receipt"
export DIREXTALK_MOCK_REPLACED=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$replacement_receipt" >/dev/null 2>&1; then
  echo "same-name replacement unexpectedly accepted" >&2
  exit 1
fi
[ ! -e "$replacement_receipt" ]
if grep -Fq -- "stop $old_id" "$DIREXTALK_MOCK_LOG"; then
  echo "identity mismatch stopped a mutable name" >&2
  exit 1
fi

edge_exists_receipt=$tmp_dir/output/edge-exists.receipt
run_case "$edge_exists_receipt"
export DIREXTALK_MOCK_EDGE_EXISTS=true
if "$script" "$stack_env" "$edge_env" "$receipt" "$edge_exists_receipt" >/dev/null 2>&1; then
  echo "pre-existing new edge container unexpectedly accepted" >&2
  exit 1
fi
[ ! -e "$edge_exists_receipt" ]

printf 'edge cutover success, negative, infrastructure, rollback, and identity guards verified\n'
