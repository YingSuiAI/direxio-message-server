#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
script=$script_dir/cleanup-local.sh
[ -x "$script" ] || { echo "cleanup-local.sh must be executable" >&2; exit 1; }
bash -n "$script"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x "$script"
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-cleanup-local.XXXXXX")
trap 'rm -rf -- "$tmp_dir"' EXIT

network_suffixes=(message-private message-public message-db agent-private agent-db agent-caller agent-egress)
volume_suffixes=(
  message-postgres message-config message-data message-plugins agent-postgres agent-secrets
  agent-config agent-core-data agent-extension-socket agent-extension-install agent-extension-staging
  agent-extension-workspaces agent-extension-runner-workspaces agent-extension-runner-state
  agent-knowledge-content agent-knowledge-mount agent-qdrant capability-authority capability-shared
  capability-private core-runner-socket core-runner-installs core-runner-workspaces core-runner-state
)
machine_id=$(tr -d '[:space:]' </etc/machine-id)
engine_id=engine-cleanup-test-1
unset DIREXTALK_FAKE_NETWORK_ERROR DIREXTALK_FAKE_VOLUME_ERROR DIREXTALK_FAKE_VOLUME_REPLACE DIREXTALK_FAKE_NETWORK_REPLACE
stack_name=d-aaaaaaaaaaaaaaaaaaaaaaaaaa

write_fixture() {
  local fixture=$1
  local tls_mode=${2:-local} server_name=${3:-localhost}
  local client_base_url
  local env_file manifest receipt master_key env_identity manifest_identity
  local suffix index id name fingerprint_json fingerprint
  local extension_fragment=/usr/lib/systemd/system/systemd-journald.service
  local extension_hash
  if [ "$tls_mode" = external ]; then
    client_base_url=https://$server_name
  else
    client_base_url=http://localhost:18008
  fi
  mkdir -m 700 -- "$fixture"
  master_key=$fixture/core-secret-master-key
  dd if=/dev/zero of="$master_key" bs=32 count=1 status=none
  chmod 400 -- "$master_key"
  env_file=$fixture/.env
  manifest=$fixture/.manifest
  receipt=$fixture/.cleanup-receipt

  {
    printf 'DIREXTALK_SPLIT_STACK_NAME=%s\n' "$stack_name"
    printf 'DIREXTALK_AGENT_INSTANCE_ID=11111111-1111-4111-8111-111111111111\n'
    printf 'DIREXTALK_MESSAGE_SERVER_INSTANCE_ID=22222222-2222-4222-8222-222222222222\n'
    printf 'DIREXTALK_ACCOUNT_GENERATION=1\n'
    printf 'DIREXTALK_MESSAGE_HTTP_BIND=18008\nDIREXTALK_MESSAGE_HTTPS_BIND=18448\n'
    printf 'DIREXTALK_MESSAGE_CLIENT_BASE_URL=%s\n' "$client_base_url"
    printf 'DIREXTALK_CORE_SECRET_MASTER_KEY_FILE=%s\n' "$master_key"
  } >"$env_file"
  for suffix in "${network_suffixes[@]}"; do
    key=$(printf '%s' "$suffix" | tr '[:lower:]-' '[:upper:]_')
    case "$suffix" in
      message-private) env_key=DIREXTALK_MESSAGE_PRIVATE_NETWORK ;;
      message-public) env_key=DIREXTALK_MESSAGE_PUBLIC_NETWORK ;;
      message-db) env_key=DIREXTALK_MESSAGE_DATABASE_NETWORK ;;
      agent-private) env_key=DIREXTALK_AGENT_PRIVATE_NETWORK ;;
      agent-db) env_key=DIREXTALK_AGENT_DATABASE_NETWORK ;;
      agent-caller) env_key=DIREXTALK_AGENT_CALLER_NETWORK ;;
      agent-egress) env_key=DIREXTALK_AGENT_EGRESS_NETWORK ;;
    esac
    printf '%s=%s-%s\n' "$env_key" "$stack_name" "$suffix" >>"$env_file"
  done
  for suffix in "${volume_suffixes[@]}"; do
    case "$suffix" in
      message-postgres) env_key=DIREXTALK_MESSAGE_POSTGRES_VOLUME ;;
      message-config) env_key=DIREXTALK_MESSAGE_CONFIG_VOLUME ;;
      message-data) env_key=DIREXTALK_MESSAGE_DATA_VOLUME ;;
      message-plugins) env_key=DIREXTALK_MESSAGE_PLUGINS_VOLUME ;;
      agent-postgres) env_key=DIREXTALK_AGENT_POSTGRES_VOLUME ;;
      agent-secrets) env_key=DIREXTALK_AGENT_SECRET_VOLUME ;;
      agent-config) env_key=DIREXTALK_AGENT_CONFIG_VOLUME ;;
      agent-core-data) env_key=DIREXTALK_AGENT_CORE_DATA_VOLUME ;;
      agent-extension-socket) env_key=DIREXTALK_AGENT_SOCKET_VOLUME ;;
      agent-extension-install) env_key=DIREXTALK_AGENT_INSTALL_VOLUME ;;
      agent-extension-staging) env_key=DIREXTALK_AGENT_STAGING_VOLUME ;;
      agent-extension-workspaces) env_key=DIREXTALK_AGENT_WORKSPACE_VOLUME ;;
      agent-extension-runner-workspaces) env_key=DIREXTALK_AGENT_RUNNER_WORKSPACE_VOLUME ;;
      agent-extension-runner-state) env_key=DIREXTALK_AGENT_RUNNER_STATE_VOLUME ;;
      agent-knowledge-content) env_key=DIREXTALK_AGENT_KNOWLEDGE_CONTENT_VOLUME ;;
      agent-knowledge-mount) env_key=DIREXTALK_AGENT_KNOWLEDGE_MOUNT_VOLUME ;;
      agent-qdrant) env_key=DIREXTALK_AGENT_QDRANT_VOLUME ;;
      capability-authority) env_key=DIREXTALK_CAPABILITY_AUTHORITY_VOLUME ;;
      capability-shared) env_key=DIREXTALK_CAPABILITY_SHARED_VOLUME ;;
      capability-private) env_key=DIREXTALK_CAPABILITY_PRIVATE_VOLUME ;;
      core-runner-socket) env_key=DIREXTALK_CORE_RUNNER_SOCKET_VOLUME ;;
      core-runner-installs) env_key=DIREXTALK_CORE_RUNNER_INSTALL_VOLUME ;;
      core-runner-workspaces) env_key=DIREXTALK_CORE_RUNNER_WORKSPACE_VOLUME ;;
      core-runner-state) env_key=DIREXTALK_CORE_RUNNER_STATE_VOLUME ;;
    esac
    printf '%s=%s-%s\n' "$env_key" "$stack_name" "$suffix" >>"$env_file"
  done
  chmod 400 -- "$env_file"

  {
    printf 'stack_name=%s\nstack_nonce=%s\n' "$stack_name" "${stack_name#d-}"
    printf 'agent_instance_id=11111111-1111-4111-8111-111111111111\nmessage_instance_id=22222222-2222-4222-8222-222222222222\naccount_generation=1\n'
    printf 'core_secret_master_key_path=%s\n' "$master_key"
    printf 'core_secret_master_key_device=%s\n' "$(stat -c '%d' "$master_key")"
    printf 'core_secret_master_key_inode=%s\n' "$(stat -c '%i' "$master_key")"
    printf 'core_secret_master_key_uid=%s\n' "$(stat -c '%u' "$master_key")"
    printf 'message_http_bind=18008\nmessage_https_bind=18448\nmessage_client_base_url=%s\nmessage_tls_mode=%s\nmessage_server_name=%s\n' "$client_base_url" "$tls_mode" "$server_name"
    printf 'runner.machine_id=%s\nrunner.docker_engine_id=%s\n' "$machine_id" "$engine_id"
  } >"$manifest"
  for suffix in "${network_suffixes[@]}"; do
    case "$suffix" in
      message-private) key=message_private ;;
      message-public) key=message_public ;;
      message-db) key=message_database ;;
      agent-private) key=agent_private ;;
      agent-db) key=agent_database ;;
      agent-caller) key=agent_caller ;;
      agent-egress) key=agent_egress ;;
    esac
    printf 'resource.network.%s=%s-%s\n' "$key" "$stack_name" "$suffix" >>"$manifest"
  done
  for suffix in "${volume_suffixes[@]}"; do
    case "$suffix" in
      message-postgres) key=message_postgres ;;
      message-config) key=message_config ;;
      message-data) key=message_data ;;
      message-plugins) key=message_plugins ;;
      agent-postgres) key=agent_postgres ;;
      agent-secrets) key=agent_secrets ;;
      agent-config) key=agent_config ;;
      agent-core-data) key=agent_core_data ;;
      agent-extension-socket) key=agent_extension_socket ;;
      agent-extension-install) key=agent_extension_install ;;
      agent-extension-staging) key=agent_extension_staging ;;
      agent-extension-workspaces) key=agent_extension_workspaces ;;
      agent-extension-runner-workspaces) key=agent_runner_workspaces ;;
      agent-extension-runner-state) key=agent_runner_state ;;
      agent-knowledge-content) key=agent_knowledge_content ;;
      agent-knowledge-mount) key=agent_knowledge_mount ;;
      agent-qdrant) key=agent_qdrant ;;
      capability-authority) key=capability_authority ;;
      capability-shared) key=capability_shared ;;
      capability-private) key=capability_private ;;
      core-runner-socket) key=core_runner_socket ;;
      core-runner-installs) key=core_runner_installs ;;
      core-runner-workspaces) key=core_runner_workspaces ;;
      core-runner-state) key=core_runner_state ;;
    esac
    printf 'resource.volume.%s=%s-%s\n' "$key" "$stack_name" "$suffix" >>"$manifest"
  done
  chmod 400 -- "$manifest"
  env_identity=$(stat -c '%d:%i:%u' "$env_file")
  manifest_identity=$(stat -c '%d:%i:%u' "$manifest")
  extension_hash=$(sha256sum -- "$extension_fragment" | awk '{print $1}')
  {
    printf '# dirextalk-split-cleanup-receipt-v1\nstack_name=%s\nstate=complete\n' "$stack_name"
    printf 'control.env_identity=%s\ncontrol.manifest_identity=%s\n' "$env_identity" "$manifest_identity"
    printf 'control.env_sha256=%s\ncontrol.manifest_sha256=%s\n' "$(sha256sum -- "$env_file" | awk '{print $1}')" "$(sha256sum -- "$manifest" | awk '{print $1}')"
    printf 'host.machine_id=%s\ndocker.engine_id=%s\ndocker.context_endpoint=unix:///run/docker.sock\ndocker.context_socket=/run/docker.sock\n' "$machine_id" "$engine_id"
    printf 'container.count=2\n'
    printf 'container.0.id=%s\ncontainer.0.name=%s-message-server-1\ncontainer.0.service=message-server\ncontainer.0.project=%s\n' "$(printf '1%.0s' {1..64})" "$stack_name" "$stack_name"
    printf 'container.1.id=%s\ncontainer.1.name=%s-agent-1\ncontainer.1.service=agent\ncontainer.1.project=%s\n' "$(printf '2%.0s' {1..64})" "$stack_name" "$stack_name"
    printf 'network.count=%s\n' "${#network_suffixes[@]}"
    index=0
    for suffix in "${network_suffixes[@]}"; do
      id=$(printf '%064x' "$((index + 10))")
      printf 'network.%s.id=%s\nnetwork.%s.name=%s-%s\nnetwork.%s.project=%s\n' "$index" "$id" "$index" "$stack_name" "$suffix" "$index" "$stack_name"
      index=$((index + 1))
    done
    printf 'volume.count=%s\n' "${#volume_suffixes[@]}"
    index=0
    for suffix in "${volume_suffixes[@]}"; do
      name=$stack_name-$suffix
      fingerprint_json=$(printf '{"Name":"%s","Driver":"local","Scope":"local","CreatedAt":"2026-08-05T00:00:00Z","Mountpoint":"/var/lib/docker/volumes/%s/_data","Options":{},"Labels":{"com.docker.compose.project":"%s"}}' "$name" "$name" "$stack_name" | jq -c '{Name,Driver,Scope,CreatedAt,Mountpoint,Labels,Options}')
      fingerprint=$(printf '%s' "$fingerprint_json" | sha256sum | awk '{print $1}')
      printf 'volume.%s.name=%s\nvolume.%s.project=%s\nvolume.%s.fingerprint_sha256=%s\n' "$index" "$name" "$index" "$stack_name" "$index" "$fingerprint"
      index=$((index + 1))
    done
    printf 'runner.extension.unit=dirextalk-extension-runner@%s.service\nrunner.extension.control_group=/%s-extension.slice/dirextalk-extension-runner@%s.service\nrunner.extension.main_pid=101\nrunner.extension.fragment_path=%s\nrunner.extension.fragment_sha256=%s\n' "$stack_name" "$stack_name" "$stack_name" "$extension_fragment" "$extension_hash"
    printf 'runner.core.unit=dirextalk-core-runner@%s.service\nrunner.core.control_group=/%s-core-runner.slice/dirextalk-core-runner@%s.service\nrunner.core.main_pid=102\nrunner.core.fragment_path=%s\nrunner.core.fragment_sha256=%s\n' "$stack_name" "$stack_name" "$stack_name" "$extension_fragment" "$extension_hash"
  } >"$receipt"
  chmod 400 -- "$receipt"

  mkdir -p -- "$fixture/bin" "$fixture/state"
  : >"$fixture/state/docker.log"
  printf '%s|%s-message-server-1|message-server\n%s|%s-agent-1|agent\n' "$(printf '1%.0s' {1..64})" "$stack_name" "$(printf '2%.0s' {1..64})" "$stack_name" >"$fixture/state/containers"
  index=0
  : >"$fixture/state/networks"
  for suffix in "${network_suffixes[@]}"; do
    id=$(printf '%064x' "$((index + 10))")
    printf '%s-%s|%s\n' "$stack_name" "$suffix" "$id" >>"$fixture/state/networks"
    index=$((index + 1))
  done
  : >"$fixture/state/volumes"
  for suffix in "${volume_suffixes[@]}"; do
    name=$stack_name-$suffix
    printf '%s\n' "$name" >>"$fixture/state/volumes"
  done
  cat >"$fixture/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
log=$DIREXTALK_FAKE_STATE/docker.log
printf '%s\n' "$*" >>"$log"
lookup_network() { awk -F'|' -v wanted="$1" '$1 == wanted {print $2; exit}' "$DIREXTALK_FAKE_STATE/networks"; }
case "$1" in
  context) printf 'unix:///run/docker.sock\n' ;;
  info) printf '%s\n' "$DIREXTALK_FAKE_ENGINE" ;;
  ps) awk -F'|' '{print $1}' "$DIREXTALK_FAKE_STATE/containers" ;;
  inspect)
    target=${!#}
    case "$target" in
      1111111111111111111111111111111111111111111111111111111111111111)
        printf '[{"Id":"%s","Name":"/%s-message-server-1","Config":{"Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"message-server"}}}]\n' "$target" "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" ;;
      2222222222222222222222222222222222222222222222222222222222222222)
        printf '[{"Id":"%s","Name":"/%s-agent-1","Config":{"Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"agent"}}}]\n' "$target" "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" ;;
      *) printf 'Error: No such object: %s\n' "$target" >&2; exit 1 ;;
    esac
    ;;
  network)
    target=$3
    case "${DIREXTALK_FAKE_NETWORK_ERROR:-}" in
      permission) printf 'permission denied while inspecting network\n' >&2; exit 1 ;;
      daemon) printf 'Docker daemon unavailable while inspecting network\n' >&2; exit 125 ;;
    esac
    id=$(lookup_network "$target")
    [ -n "$id" ] || { id=$target; target=$(awk -F'|' -v wanted="$id" '$2 == wanted {print $1; exit}' "$DIREXTALK_FAKE_STATE/networks"); }
    [ -n "$target" ] || { printf 'Error response from daemon: No such network: %s\n' "$3" >&2; exit 1; }
    [ "${DIREXTALK_FAKE_NETWORK_REPLACE:-false}" != true ] || id=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
    printf '[{"Id":"%s","Name":"%s","Labels":{"com.docker.compose.project":"%s"}}]\n' "$id" "$target" "$DIREXTALK_FAKE_STACK" ;;
  volume)
    target=$3
    case "${DIREXTALK_FAKE_VOLUME_ERROR:-}" in
      permission) printf 'permission denied while inspecting volume\n' >&2; exit 1 ;;
      daemon) printf 'Docker daemon unavailable while inspecting volume\n' >&2; exit 125 ;;
    esac
    grep -Fxq "$target" "$DIREXTALK_FAKE_STATE/volumes" || { printf 'Error response from daemon: get %s: no such volume\n' "$target" >&2; exit 1; }
    created_at=2026-08-05T00:00:00Z
    [ "${DIREXTALK_FAKE_VOLUME_REPLACE:-false}" != true ] || created_at=2026-08-06T00:00:00Z
    printf '[{"Name":"%s","Driver":"local","Scope":"local","Mountpoint":"/var/lib/docker/volumes/%s/_data","CreatedAt":"%s","Options":{},"Labels":{"com.docker.compose.project":"%s"}}]\n' "$target" "$target" "$created_at" "$DIREXTALK_FAKE_STACK" ;;
  container|network|volume)
    printf 'mutation %s\n' "$*" >>"$log" ;;
  *) printf 'unexpected docker command: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
  chmod 755 -- "$fixture/bin/docker"
  cat >"$fixture/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'systemctl %s\n' "$*" >>"$DIREXTALK_FAKE_STATE/docker.log"
unit_marker() { printf '%s' "$1" | tr '/@.' '___'; }
if [ "$1" = disable ]; then touch "$DIREXTALK_FAKE_STATE/runner-disabled-$(unit_marker "$3")"; exit 0; fi
if [ "$1" = is-enabled ]; then [ -f "$DIREXTALK_FAKE_STATE/runner-disabled-$(unit_marker "$2")" ] && printf 'disabled\n' || printf 'enabled\n'; exit 0; fi
if [ "$1" = is-active ]; then [ -f "$DIREXTALK_FAKE_STATE/runner-disabled-$(unit_marker "$2")" ] && printf 'inactive\n' || printf 'active\n'; exit 0; fi
property=${3#--property=}
case "$property" in
  LoadState) printf 'loaded\n' ;;
  ActiveState) printf 'active\n' ;;
  SubState) printf 'running\n' ;;
  FragmentPath) printf '/usr/lib/systemd/system/systemd-journald.service\n' ;;
  ControlGroup) case "$2" in *extension*) printf '/%s-extension.slice/dirextalk-extension-runner@%s.service\n' "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" ;; *) printf '/%s-core-runner.slice/dirextalk-core-runner@%s.service\n' "$DIREXTALK_FAKE_STACK" "$DIREXTALK_FAKE_STACK" ;; esac ;;
  MainPID) case "$2" in *extension*) printf '101\n' ;; *) printf '102\n' ;; esac ;;
  *) printf '\n' ;;
esac
EOF
  chmod 755 -- "$fixture/bin/systemctl"
}

fixture=$tmp_dir/normal
write_fixture "$fixture"
export PATH=$fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$fixture/state DIREXTALK_FAKE_STACK=$stack_name DIREXTALK_FAKE_ENGINE=$engine_id
if ! "$script" "$fixture" >/dev/null; then
  echo "normal exact cleanup unexpectedly failed" >&2
  exit 1
fi
if grep -Eq 'compose|docker (stop|rm)| system prune|volume prune' "$fixture/state/docker.log"; then
  echo "cleanup used a broad/name-only Docker deletion path" >&2
  exit 1
fi
grep -Eq '^container rm -f [0-9a-f]{64}$' "$fixture/state/docker.log"
grep -Eq '^network rm [0-9a-f]{64}$' "$fixture/state/docker.log"
if grep -Eq '^volume rm ' "$fixture/state/docker.log"; then
  echo "normal cleanup removed a volume without --purge" >&2
  exit 1
fi
grep -Fq "systemctl disable --now dirextalk-extension-runner@$stack_name.service" "$fixture/state/docker.log"
grep -Fq "systemctl disable --now dirextalk-core-runner@$stack_name.service" "$fixture/state/docker.log"

missing_fixture=$tmp_dir/missing
write_fixture "$missing_fixture"
sed -i '1d' "$missing_fixture/state/networks"
sed -i '1d' "$missing_fixture/state/volumes"
export PATH=$missing_fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$missing_fixture/state
if ! "$script" "$missing_fixture" >"$missing_fixture/output" 2>"$missing_fixture/error"; then
  echo "explicit Docker not-found cleanup unexpectedly failed" >&2
  exit 1
fi
grep -Fq 'split-stack cleanup complete' "$missing_fixture/output"
missing_network_id=$(printf '%064x' 10)
missing_volume_name=$stack_name-message-postgres
if grep -Fqx "network rm $missing_network_id" "$missing_fixture/state/docker.log" || grep -Fqx "volume rm $missing_volume_name" "$missing_fixture/state/docker.log"; then
  echo "missing Docker objects were mutated" >&2
  exit 1
fi

external_fixture=$tmp_dir/external
write_fixture "$external_fixture" external chat.example.test
export PATH=$external_fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$external_fixture/state
"$script" "$external_fixture" >/dev/null

purge_fixture=$tmp_dir/purge
write_fixture "$purge_fixture"
export PATH=$purge_fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$purge_fixture/state
"$script" --purge "$purge_fixture" >/dev/null
[ "$(grep -c '^volume rm ' "$purge_fixture/state/docker.log")" -eq "${#volume_suffixes[@]}" ]

identity_fixture=$tmp_dir/identity
write_fixture "$identity_fixture"
export PATH=$identity_fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$identity_fixture/state
if DIREXTALK_FAKE_ENGINE=engine-replaced "$script" "$identity_fixture" >/dev/null 2>"$identity_fixture/error"; then
  echo "Docker Engine replacement was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'Docker Engine ID changed since startup' "$identity_fixture/error"
if grep -Eq '^mutation ' "$identity_fixture/state/docker.log"; then
  echo "Engine identity failure mutated Docker state" >&2
  exit 1
fi

replacement_fixture=$tmp_dir/replacement
write_fixture "$replacement_fixture"
export PATH=$replacement_fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$replacement_fixture/state
if DIREXTALK_FAKE_NETWORK_REPLACE=true "$script" "$replacement_fixture" >/dev/null 2>"$replacement_fixture/error"; then
  echo "same-name network replacement was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'Compose network ID changed' "$replacement_fixture/error"
if grep -Eq '^mutation ' "$replacement_fixture/state/docker.log"; then
  echo "network replacement failure mutated Docker state" >&2
  exit 1
fi

volume_replacement_fixture=$tmp_dir/volume-replacement
write_fixture "$volume_replacement_fixture"
export PATH=$volume_replacement_fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$volume_replacement_fixture/state
if DIREXTALK_FAKE_VOLUME_REPLACE=true "$script" "$volume_replacement_fixture" >"$volume_replacement_fixture/output" 2>"$volume_replacement_fixture/error"; then
  echo "same-name volume replacement was unexpectedly accepted" >&2
  exit 1
fi
grep -Fq 'Compose volume fingerprint changed' "$volume_replacement_fixture/error"
if grep -Eq '^mutation ' "$volume_replacement_fixture/state/docker.log"; then
  echo "volume replacement failure mutated Docker state" >&2
  exit 1
fi

for inspect_error in permission daemon; do
  inspect_error_fixture=$tmp_dir/network-error-$inspect_error
  write_fixture "$inspect_error_fixture"
  export PATH=$inspect_error_fixture/bin:$PATH
  export DIREXTALK_FAKE_STATE=$inspect_error_fixture/state
  if DIREXTALK_FAKE_NETWORK_ERROR=$inspect_error "$script" "$inspect_error_fixture" >"$inspect_error_fixture/output" 2>"$inspect_error_fixture/error"; then
    echo "Docker network infrastructure error was unexpectedly accepted: $inspect_error" >&2
    exit 1
  fi
  if grep -Fq 'split-stack cleanup complete' "$inspect_error_fixture/output" "$inspect_error_fixture/error"; then
    echo "Docker network infrastructure error printed cleanup completion: $inspect_error" >&2
    exit 1
  fi
  if grep -Eq '^mutation ' "$inspect_error_fixture/state/docker.log"; then
    echo "network infrastructure error mutated Docker state: $inspect_error" >&2
    exit 1
  fi
done

for inspect_error in permission daemon; do
  inspect_error_fixture=$tmp_dir/volume-error-$inspect_error
  write_fixture "$inspect_error_fixture"
  export PATH=$inspect_error_fixture/bin:$PATH
  export DIREXTALK_FAKE_STATE=$inspect_error_fixture/state
  if DIREXTALK_FAKE_VOLUME_ERROR=$inspect_error "$script" "$inspect_error_fixture" >"$inspect_error_fixture/output" 2>"$inspect_error_fixture/error"; then
    echo "Docker volume infrastructure error was unexpectedly accepted: $inspect_error" >&2
    exit 1
  fi
  if grep -Fq 'split-stack cleanup complete' "$inspect_error_fixture/output" "$inspect_error_fixture/error"; then
    echo "Docker volume infrastructure error printed cleanup completion: $inspect_error" >&2
    exit 1
  fi
  if grep -Eq '^mutation ' "$inspect_error_fixture/state/docker.log"; then
    echo "volume infrastructure error mutated Docker state: $inspect_error" >&2
    exit 1
  fi
done

incomplete_fixture=$tmp_dir/incomplete
write_fixture "$incomplete_fixture"
chmod 600 -- "$incomplete_fixture/.cleanup-receipt"
sed -i -e 's/^state=complete$/state=incomplete/' \
  -e 's/^container.count=.*/container.count=0/' \
  -e 's/^network.count=.*/network.count=0/' \
  -e 's/^volume.count=.*/volume.count=0/' "$incomplete_fixture/.cleanup-receipt"
chmod 400 -- "$incomplete_fixture/.cleanup-receipt"
export PATH=$incomplete_fixture/bin:$PATH
export DIREXTALK_FAKE_STATE=$incomplete_fixture/state
"$script" "$incomplete_fixture" >/dev/null
if grep -Eq '^mutation ' "$incomplete_fixture/state/docker.log"; then
  echo "incomplete receipt selected an unrecorded Docker object" >&2
  exit 1
fi
grep -Fq 'write_start_journal' "$script_dir/start-local.sh"
grep -Fq 'capture_incomplete_receipt' "$script_dir/start-local.sh"

printf 'cleanup-local exact receipt, identity, and purge guards verified\n'
