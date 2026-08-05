#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/adopt-edge.sh
[ -x "$script" ] || { echo "adopt-edge.sh must be executable" >&2; exit 1; }
bash -n "$script"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-edge-adopt-test.XXXXXX")
cleanup() { rm -rf -- "$tmp_dir"; }
trap cleanup EXIT
mkdir -m 700 "$tmp_dir/bin" "$tmp_dir/state" "$tmp_dir/probe" "$tmp_dir/output"

legacy_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidate_id=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
candidate_image=docker.io/library/caddy@sha256:4444444444444444444444444444444444444444444444444444444444444444
message_network=legacy-message-public
data_volume=caddy-data
config_volume=caddy-config
operation=adopt-op-1
revision=rev-1

cat >"$tmp_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DIREXTALK_MOCK_LOG"
state=$DIREXTALK_MOCK_STATE
legacy_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
replacement_id=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
candidate_id=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
legacy_config_image=caddy:2
candidate_image=docker.io/library/caddy@sha256:4444444444444444444444444444444444444444444444444444444444444444
network=legacy-message-public
network_id=net-legacy-123
data_volume=caddy-data
config_volume=caddy-config
legacy_image_id=sha256:1111111111111111111111111111111111111111111111111111111111111111
candidate_image_id=sha256:2222222222222222222222222222222222222222222222222222222222222222
legacy_repo_digest=caddy@sha256:3333333333333333333333333333333333333333333333333333333333333333
candidate_repo_digest=$candidate_image
engine_id=engine-adoption-1

if [ "$1" = compose ]; then
  shift
  case " $* " in
    *' config --quiet '*) exit 0 ;;
    *' config --format json '*)
      cat <<JSON
{"name":"edge-new","services":{"caddy":{"image":"$candidate_image","read_only":true,"cap_drop":["ALL"],"security_opt":["no-new-privileges:true"],"ports":[{"published":"80","target":80},{"published":"443","target":443}],"healthcheck":{"test":["CMD-SHELL","wget -q -O - http://127.0.0.1:2019/config/ >/dev/null"]},"networks":["message_public"],"volumes":[{"type":"volume","source":"caddy_data","target":"/data"},{"type":"volume","source":"caddy_config","target":"/config"}]}},"networks":{"message_public":{"name":"$network","external":true}},"volumes":{"caddy_data":{"name":"$data_volume","external":true},"caddy_config":{"name":"$config_volume","external":true}}}
JSON
      exit 0
      ;;
    *' ps -aq caddy '*) [ -f "$state/candidate-created" ] && printf '%s\n' "$candidate_id"; exit 0 ;;
    *' ps -q caddy '*) [ -f "$state/candidate-created" ] && printf '%s\n' "$candidate_id"; exit 0 ;;
    *' create --no-start caddy '*) [ "${DIREXTALK_MOCK_CREATE_FAIL:-false}" != true ] || exit 40; : >"$state/candidate-created"; exit 0 ;;
    *) exit 0 ;;
  esac
fi

case "${1:-}" in
  info) printf '%s\n' "$engine_id" ;;
  inspect)
    object=${2:-}
    if [ "$object" = "$legacy_id" ] && [ "${DIREXTALK_MOCK_REPLACED:-false}" = true ]; then object=$replacement_id; fi
    case "$object" in
      "$legacy_id"|"$replacement_id")
        status=running; health=healthy
        [ -f "$state/legacy-stopped" ] && status=exited
        [ -f "$state/legacy-started" ] && status=running
        if [ "$object" = "$legacy_id" ] && [ "$status" = exited ] && [ "${DIREXTALK_MOCK_POST_STOP_INSPECT_FAIL:-false}" = true ] && [ ! -f "$state/post-stop-inspect-failed" ]; then
          : >"$state/post-stop-inspect-failed"
          exit 91
        fi
        legacy_healthcheck=''
        legacy_state="{\"Status\":\"$status\",\"Health\":{\"Status\":\"$health\"}}"
        if [ "${DIREXTALK_MOCK_LEGACY_NO_HEALTH:-false}" = true ]; then
          legacy_state="{\"Status\":\"$status\"}"
        elif [ "${DIREXTALK_MOCK_LEGACY_INCOMPLETE_HEALTH:-false}" = true ]; then
          legacy_healthcheck=',"Healthcheck":{"Test":["CMD-SHELL","wget -q -O - http://127.0.0.1:2019/config/ >/dev/null"]}'
          legacy_state="{\"Status\":\"$status\"}"
        fi
        cat <<JSON
[{"Id":"$object","Image":"$legacy_image_id","Config":{"Image":"$legacy_config_image","Labels":{"com.docker.compose.project":"legacy-message","com.docker.compose.service":"caddy"}$legacy_healthcheck},"NetworkSettings":{"Networks":{"$network":{}}},"Mounts":[{"Type":"volume","Name":"$data_volume","Destination":"/data"},{"Type":"volume","Name":"$config_volume","Destination":"/config"}],"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}]},"ReadonlyRootfs":true,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges:true"]},"State":$legacy_state}]
JSON
        ;;
      "$candidate_id")
        status=created; health=starting
        [ -f "$state/candidate-started" ] && status=running && health=healthy
        [ -f "$state/candidate-removed" ] && status=exited
        candidate_healthcheck=',"Healthcheck":{"Test":["CMD-SHELL","wget -q -O - http://127.0.0.1:2019/config/ >/dev/null"]}'
        candidate_state="{\"Status\":\"$status\",\"Health\":{\"Status\":\"$health\"}}"
        if [ "${DIREXTALK_MOCK_CANDIDATE_NO_HEALTH:-false}" = true ]; then
          candidate_healthcheck=''
          candidate_state="{\"Status\":\"$status\"}"
        fi
        cat <<JSON
[{"Id":"$candidate_id","Image":"$candidate_image_id","Config":{"Image":"$candidate_image","Labels":{"com.docker.compose.project":"edge-new","com.docker.compose.service":"caddy"}$candidate_healthcheck},"NetworkSettings":{"Networks":{"$network":{}}},"Mounts":[{"Type":"volume","Name":"$data_volume","Destination":"/data"},{"Type":"volume","Name":"$config_volume","Destination":"/config"}],"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"80"}],"443/tcp":[{"HostPort":"443"}]},"ReadonlyRootfs":true,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges:true"]},"State":$candidate_state}]
JSON
        ;;
      *3333333333333333333333333333333333333333333333333333333333333333) printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$legacy_image_id" "$legacy_repo_digest" ;;
      *4444444444444444444444444444444444444444444444444444444444444444) printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$candidate_image_id" "$candidate_repo_digest" ;;
      *) exit 1 ;;
    esac
    ;;
  image)
    [ "${2:-}" = inspect ] || exit 1
    image=${3:-}
    case "$image" in
      "$legacy_config_image"|"$legacy_image_id")
        if [ "${DIREXTALK_MOCK_LEGACY_NO_REPO_DIGEST:-false}" = true ]; then
          printf '[{"Id":"%s","RepoDigests":[]}]\n' "$legacy_image_id"
        else
          printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$legacy_image_id" "$legacy_repo_digest"
        fi
        ;;
      "$candidate_image") printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$candidate_image_id" "$candidate_repo_digest" ;;
      *3333333333333333333333333333333333333333333333333333333333333333) printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$legacy_image_id" "$legacy_repo_digest" ;;
      *4444444444444444444444444444444444444444444444444444444444444444) printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$candidate_image_id" "$candidate_repo_digest" ;;
      *) exit 1 ;;
    esac
    ;;
  network)
    [ "${2:-}" = inspect ] || exit 1
    [ "${3:-}" = "$network" ] || exit 1
    containers="\"$legacy_id\":{}"
    [ -f "$state/candidate-created" ] && containers="$containers,\"$candidate_id\":{}"
    printf '[{"Id":"%s","Name":"%s","Labels":{"com.docker.compose.project":"legacy-message","com.docker.compose.network":"message_public"},"Containers":{%s}}]\n' "$network_id" "$network" "$containers"
    ;;
  volume)
    [ "${2:-}" = inspect ] || exit 1
    case "${3:-}" in
      "$data_volume") printf '[{"Name":"%s","Driver":"local","Scope":"local","CreatedAt":"2026-08-05T00:00:00Z","Mountpoint":"/var/lib/docker/volumes/%s/_data","Options":{},"Labels":{"com.docker.compose.project":"legacy-message"}}]\n' "$data_volume" "$data_volume" ;;
      "$config_volume") printf '[{"Name":"%s","Driver":"local","Scope":"local","CreatedAt":"2026-08-05T00:00:01Z","Mountpoint":"/var/lib/docker/volumes/%s/_data","Options":{},"Labels":{"com.docker.compose.project":"legacy-message"}}]\n' "$config_volume" "$config_volume" ;;
      *) exit 1 ;;
    esac
    ;;
  stop)
    case "${2:-}" in
      "$legacy_id") : >"$state/legacy-stopped" ;;
      "$candidate_id") : >"$state/candidate-removed" ;;
      *) exit 1 ;;
    esac
    ;;
  start)
    object=${2:-}
    if [ "$object" = "$candidate_id" ]; then
      [ "${DIREXTALK_MOCK_CANDIDATE_START_FAIL:-false}" != true ] || exit 41
      : >"$state/candidate-started"
    elif [ "$object" = "$legacy_id" ]; then
      [ "${DIREXTALK_MOCK_LEGACY_START_FAIL:-false}" != true ] || exit 42
      : >"$state/legacy-started"
    else exit 1
    fi
    ;;
  rm)
    [ "${2:-}" = -f ] && [ "${3:-}" = "$candidate_id" ] || exit 1
    [ "${DIREXTALK_MOCK_RM_FAIL:-false}" != true ] || exit 46
    : >"$state/candidate-removed"
    ;;
  *) exit 0 ;;
esac
EOF
chmod 755 "$tmp_dir/bin/docker"

cat >"$tmp_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DIREXTALK_CURL_LOG"
[ "${DIREXTALK_MOCK_PUBLIC_FAIL:-false}" != true ]
EOF
chmod 755 "$tmp_dir/bin/curl"

caddyfile=$tmp_dir/Caddyfile
edge_compose=$tmp_dir/edge-compose.yaml
printf 'reverse_proxy message-server:8008\n' >"$caddyfile"
printf 'name: edge-new\n' >"$edge_compose"
chmod 400 "$caddyfile" "$edge_compose"

edge_env=$tmp_dir/edge.env
cat >"$edge_env" <<EOF
DIREXTALK_EDGE_STACK_NAME=edge-new
DIREXTALK_PUBLIC_DOMAIN=edge.example
DIREXTALK_MESSAGE_PUBLIC_NETWORK=$message_network
DIREXTALK_CADDY_IMAGE_IMMUTABLE=$candidate_image
DIREXTALK_CADDY_DATA_VOLUME=$data_volume
DIREXTALK_CADDY_CONFIG_VOLUME=$config_volume
DIREXTALK_CADDYFILE=$caddyfile
DIREXTALK_EDGE_COMPOSE_FILE=$edge_compose
DIREXTALK_LEGACY_MESSAGE_STACK_NAME=legacy-message
EOF
chmod 400 "$edge_env"

export PATH="$tmp_dir/bin:$PATH"
export DIREXTALK_DOCKER_BIN=docker DIREXTALK_CURL_BIN=curl
export DIREXTALK_MOCK_LOG=$tmp_dir/docker.log DIREXTALK_CURL_LOG=$tmp_dir/curl.log DIREXTALK_MOCK_STATE=$tmp_dir/state

run_probe() {
  rm -f -- "$DIREXTALK_MOCK_LOG" "$DIREXTALK_CURL_LOG"
  : >"$DIREXTALK_MOCK_LOG"; : >"$DIREXTALK_CURL_LOG"
  "$script" probe "$edge_env" "$legacy_id" "$tmp_dir/probe/probe.receipt" "$operation" "$revision" >/dev/null
}

run_probe
[ "$(stat -c '%a' "$tmp_dir/probe")" = 700 ]
[ "$(stat -c '%a' "$tmp_dir/probe/probe.receipt")" = 400 ]
grep -Fq '# dirextalk-edge-adoption-probe-v1' "$tmp_dir/probe/probe.receipt"
if grep -Eq '(^| )(stop|start|rm|create)( |$)' "$DIREXTALK_MOCK_LOG"; then
  echo "probe performed a Docker mutation" >&2
  exit 1
fi

active=$tmp_dir/output/active.receipt
snapshot=$tmp_dir/output/legacy.snapshot
"$script" commit "$edge_env" "$tmp_dir/probe/probe.receipt" "$operation" "$revision" "$active" "$snapshot" >/dev/null
[ "$(stat -c '%a' "$active")" = 400 ]
[ "$(stat -c '%a' "$snapshot")" = 400 ]
grep -Fq '# dirextalk-edge-receipt-v1' "$active"
grep -Fq "active_caddy_container_id=$candidate_id" "$active"
grep -Eq '^active_caddy_network_labels_sha256=[0-9a-f]{64}$' "$active"
grep -Fq "legacy_caddy_container_id=$legacy_id" "$snapshot"
grep -Fq -- "stop $legacy_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $candidate_id" "$DIREXTALK_MOCK_LOG"
[ -e "$tmp_dir/probe/.adopt-edge-committed.$operation" ]
[ "$(stat -c '%a' "$tmp_dir/probe/.adopt-edge-committed.$operation")" = 400 ]

# A running legacy Caddy without any configured Docker healthcheck can be
# adopted only after the real public health, well-known, and TLS probes pass.
no_health_dir=$tmp_dir/probe/legacy-no-health
mkdir -m 700 "$no_health_dir"
rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG" "$DIREXTALK_CURL_LOG"
export DIREXTALK_MOCK_LEGACY_NO_HEALTH=true
export DIREXTALK_MOCK_PUBLIC_FAIL=true
if "$script" probe "$edge_env" "$legacy_id" "$no_health_dir/public-fail.receipt" legacy-no-health-public-fail rev-no-health >/dev/null 2>&1; then
  echo "unconfigured legacy health unexpectedly bypassed the public probe" >&2
  exit 1
fi
[ ! -e "$no_health_dir/public-fail.receipt" ]
if grep -Eq '(^| )(stop|start|rm|create)( |$)' "$DIREXTALK_MOCK_LOG"; then
  echo "failed unconfigured legacy public probe caused a Docker mutation" >&2
  exit 1
fi
unset DIREXTALK_MOCK_PUBLIC_FAIL
legacy_no_health_probe=$no_health_dir/probe.receipt
legacy_no_health_active=$no_health_dir/active.receipt
legacy_no_health_snapshot=$no_health_dir/legacy.snapshot
"$script" probe "$edge_env" "$legacy_id" "$legacy_no_health_probe" legacy-no-health rev-no-health >/dev/null
grep -Fq 'legacy_health=unconfigured-public-probe' "$legacy_no_health_probe"
"$script" commit "$edge_env" "$legacy_no_health_probe" legacy-no-health rev-no-health "$legacy_no_health_active" "$legacy_no_health_snapshot" >/dev/null
grep -Fq -- "stop $legacy_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $candidate_id" "$DIREXTALK_MOCK_LOG"
unset DIREXTALK_MOCK_LEGACY_NO_HEALTH

# A partially configured legacy health state is neither healthy nor truly
# unconfigured and must fail before a receipt or mutation can be produced.
rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_LEGACY_INCOMPLETE_HEALTH=true
if "$script" probe "$edge_env" "$legacy_id" "$no_health_dir/incomplete.receipt" legacy-incomplete-health rev-incomplete >/dev/null 2>&1; then
  echo "partially configured legacy health unexpectedly accepted" >&2
  exit 1
fi
[ ! -e "$no_health_dir/incomplete.receipt" ]
if grep -Eq '(^| )(stop|start|rm|create)( |$)' "$DIREXTALK_MOCK_LOG"; then
  echo "partially configured legacy health caused a Docker mutation" >&2
  exit 1
fi
unset DIREXTALK_MOCK_LEGACY_INCOMPLETE_HEALTH

# A legacy tag is acceptable only when the exact container image object still
# exposes one matching Docker Hub Caddy RepoDigest.
rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_LEGACY_NO_REPO_DIGEST=true
if "$script" probe "$edge_env" "$legacy_id" "$no_health_dir/no-repo-digest.receipt" legacy-no-repo-digest rev-no-repo >/dev/null 2>&1; then
  echo "legacy image without a RepoDigest unexpectedly accepted" >&2
  exit 1
fi
[ ! -e "$no_health_dir/no-repo-digest.receipt" ]
if grep -Eq '(^| )(stop|start|rm|create)( |$)' "$DIREXTALK_MOCK_LOG"; then
  echo "legacy image without a RepoDigest caused a Docker mutation" >&2
  exit 1
fi
unset DIREXTALK_MOCK_LEGACY_NO_REPO_DIGEST

# Crash after each receipt install leaves the protected transaction journal;
# rerunning the same operation revalidates exact IDs and installs only the
# missing receipt/marker bytes.
for crash_point in LEGACY ACTIVE; do
  crash_dir=$tmp_dir/probe/crash-$crash_point
  rm -rf -- "$crash_dir"; mkdir -m 700 "$crash_dir"
  rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG"
  crash_probe=$crash_dir/probe.receipt
  crash_active=$crash_dir/active.receipt
  crash_snapshot=$crash_dir/legacy.snapshot
  "$script" probe "$edge_env" "$legacy_id" "$crash_probe" "crash-$crash_point" rev-crash >/dev/null
  if [ "$crash_point" = LEGACY ]; then
    export DIREXTALK_ADOPT_TEST_CRASH_AFTER_LEGACY_RECEIPT=true
  else
    export DIREXTALK_ADOPT_TEST_CRASH_AFTER_ACTIVE_RECEIPT=true
  fi
  if "$script" commit "$edge_env" "$crash_probe" "crash-$crash_point" rev-crash "$crash_active" "$crash_snapshot" >/dev/null 2>&1; then
    echo "receipt crash injection unexpectedly succeeded" >&2
    exit 1
  fi
  unset DIREXTALK_ADOPT_TEST_CRASH_AFTER_LEGACY_RECEIPT DIREXTALK_ADOPT_TEST_CRASH_AFTER_ACTIVE_RECEIPT
  [ -f "$crash_dir/.adopt-edge-txn.crash-$crash_point/journal" ]
  "$script" commit "$edge_env" "$crash_probe" "crash-$crash_point" rev-crash "$crash_active" "$crash_snapshot" >/dev/null
  [ -f "$crash_active" ] && [ -f "$crash_snapshot" ]
  [ -f "$crash_dir/.adopt-edge-committed.crash-$crash_point" ]
done

# A stop can apply while its exact post-stop inspect is temporarily
# unavailable.  The journal must survive that uncertainty; rerun recovery
# uses the exact stopped legacy/candidate IDs and completes the receipts.
rm -rf -- "$tmp_dir/probe/post-stop-uncertain"; mkdir -m 700 "$tmp_dir/probe/post-stop-uncertain"
rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG"
uncertain_probe=$tmp_dir/probe/post-stop-uncertain/probe.receipt
uncertain_active=$tmp_dir/probe/post-stop-uncertain/active.receipt
uncertain_snapshot=$tmp_dir/probe/post-stop-uncertain/legacy.snapshot
"$script" probe "$edge_env" "$legacy_id" "$uncertain_probe" uncertain-op uncertain-rev >/dev/null
: >"$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_POST_STOP_INSPECT_FAIL=true
if "$script" commit "$edge_env" "$uncertain_probe" uncertain-op uncertain-rev "$uncertain_active" "$uncertain_snapshot" >/dev/null 2>&1; then
  echo "uncertain post-stop inspect unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_MOCK_POST_STOP_INSPECT_FAIL
[ -f "$tmp_dir/probe/post-stop-uncertain/.adopt-edge-txn.uncertain-op/journal" ]
[ ! -e "$uncertain_active" ] && [ ! -e "$uncertain_snapshot" ]
"$script" commit "$edge_env" "$uncertain_probe" uncertain-op uncertain-rev "$uncertain_active" "$uncertain_snapshot" >/dev/null
[ -f "$uncertain_active" ] && [ -f "$uncertain_snapshot" ]
[ ! -e "$tmp_dir/probe/post-stop-uncertain/.adopt-edge-txn.uncertain-op" ]

replay=$tmp_dir/output/replay.receipt
if "$script" commit "$edge_env" "$tmp_dir/probe/probe.receipt" "$operation" "$revision" "$replay" "$tmp_dir/output/replay.snapshot" >/dev/null 2>&1; then
  echo "successful adoption unexpectedly replayed" >&2
  exit 1
fi

collision=$tmp_dir/output/collision.receipt
printf 'prior-audit\n' >"$collision"
chmod 400 "$collision"
before=$(sha256sum "$collision" | awk '{print $1}')
if "$script" commit "$edge_env" "$tmp_dir/probe/probe.receipt" "$operation" "$revision" "$collision" "$tmp_dir/output/collision.snapshot" >/dev/null 2>&1; then
  echo "receipt collision unexpectedly accepted" >&2
  exit 1
fi
[ "$before" = "$(sha256sum "$collision" | awk '{print $1}')" ]

rm -rf -- "$tmp_dir/probe/stale"; mkdir -m 700 "$tmp_dir/probe/stale"
unset DIREXTALK_MOCK_REPLACED

# The protected edge environment is part of the probe identity; changing a
# valid domain/path after probe must not reach candidate creation or stop.
rm -rf -- "$tmp_dir/probe/edge-env-tamper"; mkdir -m 700 "$tmp_dir/probe/edge-env-tamper"
rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG"
tampered_edge_env=$tmp_dir/probe/edge-env-tamper/edge.env
cp -- "$edge_env" "$tampered_edge_env"
chmod 400 "$tampered_edge_env"
tampered_env_probe=$tmp_dir/probe/edge-env-tamper/probe.receipt
"$script" probe "$tampered_edge_env" "$legacy_id" "$tampered_env_probe" edge-env-tamper rev-edge-env-tamper >/dev/null
chmod 600 "$tampered_edge_env"
sed -i 's/^DIREXTALK_PUBLIC_DOMAIN=.*/DIREXTALK_PUBLIC_DOMAIN=changed.example/' "$tampered_edge_env"
chmod 400 "$tampered_edge_env"
: >"$DIREXTALK_MOCK_LOG"
if "$script" commit "$tampered_edge_env" "$tampered_env_probe" edge-env-tamper rev-edge-env-tamper "$tmp_dir/output/edge-env-tamper.active" "$tmp_dir/output/edge-env-tamper.snapshot" >/dev/null 2>&1; then
  echo "changed edge env identity unexpectedly accepted" >&2
  exit 1
fi
if grep -Eq '(^| )(stop|start|rm|create)( |$)' "$DIREXTALK_MOCK_LOG"; then
  echo "changed edge env identity caused a Docker mutation" >&2
  exit 1
fi
rm -f -- "$DIREXTALK_MOCK_STATE"/*
probe_stale=$tmp_dir/probe/stale/probe.receipt
"$script" probe "$edge_env" "$legacy_id" "$probe_stale" stale-op stale-rev >/dev/null
: >"$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_REPLACED=true
if "$script" commit "$edge_env" "$probe_stale" stale-op stale-rev "$tmp_dir/output/stale.active" "$tmp_dir/output/stale.snapshot" >/dev/null 2>&1; then
  echo "replaced legacy identity unexpectedly accepted" >&2
  exit 1
fi
if grep -Fq -- "stop $legacy_id" "$DIREXTALK_MOCK_LOG"; then
  echo "replaced identity crossed the stop boundary" >&2
  exit 1
fi
unset DIREXTALK_MOCK_REPLACED

# A public failure after candidate creation but immediately before the exact
# stop keeps the legacy edge running. The protected journal then drives an
# exact candidate cleanup on retry without crossing the stop boundary.
rm -rf -- "$tmp_dir/probe/pre-stop-public-fail"; mkdir -m 700 "$tmp_dir/probe/pre-stop-public-fail"
rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG" "$DIREXTALK_CURL_LOG"
pre_stop_probe=$tmp_dir/probe/pre-stop-public-fail/probe.receipt
pre_stop_active=$tmp_dir/probe/pre-stop-public-fail/active.receipt
pre_stop_snapshot=$tmp_dir/probe/pre-stop-public-fail/legacy.snapshot
"$script" probe "$edge_env" "$legacy_id" "$pre_stop_probe" pre-stop-public-fail rev-pre-stop >/dev/null
: >"$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_PUBLIC_FAIL=true
if "$script" commit "$edge_env" "$pre_stop_probe" pre-stop-public-fail rev-pre-stop "$pre_stop_active" "$pre_stop_snapshot" >/dev/null 2>&1; then
  echo "pre-stop public failure unexpectedly succeeded" >&2
  exit 1
fi
unset DIREXTALK_MOCK_PUBLIC_FAIL
grep -Fq -- 'create --no-start caddy' "$DIREXTALK_MOCK_LOG"
if grep -Fq -- "stop $legacy_id" "$DIREXTALK_MOCK_LOG"; then
  echo "pre-stop public failure crossed the legacy stop boundary" >&2
  exit 1
fi
[ -f "$tmp_dir/probe/pre-stop-public-fail/.adopt-edge-txn.pre-stop-public-fail/journal" ]
if "$script" commit "$edge_env" "$pre_stop_probe" pre-stop-public-fail rev-pre-stop "$pre_stop_active" "$pre_stop_snapshot" >/dev/null 2>&1; then
  echo "uncommitted pre-stop candidate recovery unexpectedly reported success" >&2
  exit 1
fi
grep -Fq -- "rm -f $candidate_id" "$DIREXTALK_MOCK_LOG"
if grep -Fq -- "stop $legacy_id" "$DIREXTALK_MOCK_LOG"; then
  echo "pre-stop candidate recovery crossed the legacy stop boundary" >&2
  exit 1
fi
[ ! -e "$tmp_dir/probe/pre-stop-public-fail/.adopt-edge-txn.pre-stop-public-fail" ]

rm -rf -- "$tmp_dir/probe/failure"; mkdir -m 700 "$tmp_dir/probe/failure"
rm -f -- "$DIREXTALK_MOCK_STATE"/*
probe_failure=$tmp_dir/probe/failure/probe.receipt
"$script" probe "$edge_env" "$legacy_id" "$probe_failure" fail-op fail-rev >/dev/null
: >"$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_CANDIDATE_START_FAIL=true
export DIREXTALK_MOCK_RM_FAIL=true
if "$script" commit "$edge_env" "$probe_failure" fail-op fail-rev "$tmp_dir/output/failure.active" "$tmp_dir/output/failure.snapshot" >/dev/null 2>&1; then
  echo "candidate startup failure unexpectedly succeeded" >&2
  exit 1
fi
grep -Fq -- "rm -f $candidate_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "stop $candidate_id" "$DIREXTALK_MOCK_LOG"
grep -Fq -- "start $legacy_id" "$DIREXTALK_MOCK_LOG"
unset DIREXTALK_MOCK_CANDIDATE_START_FAIL DIREXTALK_MOCK_RM_FAIL

rm -rf -- "$tmp_dir/probe/nohealth"; mkdir -m 700 "$tmp_dir/probe/nohealth"
rm -f -- "$DIREXTALK_MOCK_STATE"/*
probe_nohealth=$tmp_dir/probe/nohealth/probe.receipt
"$script" probe "$edge_env" "$legacy_id" "$probe_nohealth" nohealth-op nohealth-rev >/dev/null
sed 's/^legacy_health=healthy$/legacy_health=starting/' "$probe_nohealth" >"$tmp_dir/probe/nohealth/mutable"
chmod 400 "$tmp_dir/probe/nohealth/mutable"
: >"$DIREXTALK_MOCK_LOG"
if "$script" commit "$edge_env" "$tmp_dir/probe/nohealth/mutable" nohealth-op nohealth-rev "$tmp_dir/output/nohealth.active" "$tmp_dir/output/nohealth.snapshot" >/dev/null 2>&1; then
  echo "mutable/no-health probe receipt unexpectedly accepted" >&2
  exit 1
fi
if grep -Eq -- "stop|start|create|rm" "$DIREXTALK_MOCK_LOG"; then
  echo "invalid receipt caused a Docker mutation" >&2
  exit 1
fi

# The legacy-only exception must never weaken the candidate healthcheck gate.
rm -rf -- "$tmp_dir/probe/candidate-no-health"; mkdir -m 700 "$tmp_dir/probe/candidate-no-health"
rm -f -- "$DIREXTALK_MOCK_STATE"/* "$DIREXTALK_MOCK_LOG"
candidate_no_health_probe=$tmp_dir/probe/candidate-no-health/probe.receipt
"$script" probe "$edge_env" "$legacy_id" "$candidate_no_health_probe" candidate-no-health rev-candidate-no-health >/dev/null
: >"$DIREXTALK_MOCK_LOG"
export DIREXTALK_MOCK_CANDIDATE_NO_HEALTH=true
if "$script" commit "$edge_env" "$candidate_no_health_probe" candidate-no-health rev-candidate-no-health "$tmp_dir/output/candidate-no-health.active" "$tmp_dir/output/candidate-no-health.snapshot" >/dev/null 2>&1; then
  echo "candidate without Docker healthcheck unexpectedly accepted" >&2
  exit 1
fi
if grep -Fq -- "stop $legacy_id" "$DIREXTALK_MOCK_LOG"; then
  echo "candidate without Docker healthcheck crossed the legacy stop boundary" >&2
  exit 1
fi
unset DIREXTALK_MOCK_CANDIDATE_NO_HEALTH

# Edge env parsing must reject values that would be unsafe in curl URLs or
# silently change defaults when an optional key is malformed.
expect_probe_rejected() {
  local label=$1 env_file=$2 receipt="$tmp_dir/probe/$1.receipt"
  : >"$DIREXTALK_MOCK_LOG"
  : >"$DIREXTALK_CURL_LOG"
  if "$script" probe "$env_file" "$legacy_id" "$receipt" "invalid-$label" rev-invalid >/dev/null 2>&1; then
    echo "$label input unexpectedly accepted" >&2
    exit 1
  fi
  if grep -Eq '(^| )(stop|start|rm|create)( |$)' "$DIREXTALK_MOCK_LOG"; then
    echo "$label input caused a Docker mutation" >&2
    exit 1
  fi
  [ ! -e "$receipt" ]
}

invalid_domain_env=$tmp_dir/edge-invalid-domain.env
sed 's/^DIREXTALK_PUBLIC_DOMAIN=.*/DIREXTALK_PUBLIC_DOMAIN=edge.example\/unsafe/' "$edge_env" >"$invalid_domain_env"
chmod 400 "$invalid_domain_env"
expect_probe_rejected invalid-domain "$invalid_domain_env"

duplicate_optional_env=$tmp_dir/edge-duplicate-optional.env
{
  cat "$edge_env"
  printf 'DIREXTALK_WELL_KNOWN_PATH=/.well-known/matrix/server\n'
  printf 'DIREXTALK_WELL_KNOWN_PATH=/.well-known/matrix/server\n'
} >"$duplicate_optional_env"
chmod 400 "$duplicate_optional_env"
expect_probe_rejected duplicate-optional "$duplicate_optional_env"

empty_optional_env=$tmp_dir/edge-empty-optional.env
{
  cat "$edge_env"
  printf 'DIREXTALK_PUBLIC_HEALTH_PATH=\n'
} >"$empty_optional_env"
chmod 400 "$empty_optional_env"
expect_probe_rejected empty-optional "$empty_optional_env"

printf 'edge adoption probe, success, stale identity, rollback, replay, receipt, and no-health guards verified\n'
