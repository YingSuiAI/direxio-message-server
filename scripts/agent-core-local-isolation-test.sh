#!/bin/sh
# Dry-run two independent stack identities and prove Compose expands distinct
# project/network/volume resources before any container is created.
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
compose=$root/docker-compose.agent-core-local.yml
orchestrator=$root/scripts/agent-core-local.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for id in alpha beta; do
  run=$tmp/$id
  mkdir -p "$run"
  printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' > "$run/token"
  printf '%s\n' 'not-a-real-ca-for-config-only' > "$run/ca"
  chmod 0400 "$run/token" "$run/ca"
  printf '%s\n' "DIREXTALK_MESSAGE_SERVER_PROJECT=dirextalk-message-server-$id" \
    "DIREXTALK_MESSAGE_SERVER_PRIVATE_NETWORK=dirextalk-message-private-$id" \
    "DIREXTALK_MESSAGE_SERVER_PUBLIC_NETWORK=dirextalk-message-public-$id" \
    "DIREXTALK_MESSAGE_POSTGRES_VOLUME=dirextalk-message-postgres-$id" \
    "DIREXTALK_MESSAGE_SERVER_CONFIG_VOLUME=dirextalk-message-config-$id" \
    "DIREXTALK_MESSAGE_SERVER_DATA_VOLUME=dirextalk-message-data-$id" \
    "DIREXTALK_MESSAGE_AGENT_CORE_MATERIAL_VOLUME=dirextalk-message-material-$id" \
    "DIREXTALK_AGENT_CALLER_NETWORK_NAME=dirextalk-agent-caller-$id" \
    "P2P_AGENT_CORE_SERVER_NAME=core.local" \
    "P2P_AGENT_CORE_EXPECTED_INSTANCE_ID=00000000-0000-4000-8000-000000000000" \
    "DIREXTALK_AGENT_SERVICE_TOKEN_FILE=$run/token" \
    "DIREXTALK_AGENT_TLS_CERT_FILE=$run/ca" > "$run/.env"
  docker compose -p "dry-$id" --env-file "$run/.env" -f "$compose" config --format json > "$run/config.json"
  public_services=$(jq -r '.services | to_entries[] | select((.value.networks // {}) | has("message_server_public")) | .key' "$run/config.json")
  [ "$public_services" = "message-server" ] || { echo "public network leaked to another service" >&2; exit 1; }
  jq -r '[.name, (.networks // {} | to_entries[].value.name), (.volumes // {} | to_entries[].value.name)] | .[]' "$run/config.json" \
    | sort > "$run/resources"
done
if comm -12 "$tmp/alpha/resources" "$tmp/beta/resources" | grep -q .; then
  echo 'dry-run isolation failed: resource names overlap' >&2
  exit 1
fi
mkdir -p "$tmp/path-a/same" "$tmp/path-b/same"
DIREXTALK_AGENT_ROOT=${DIREXTALK_AGENT_ROOT:-$root/../agent} "$orchestrator" down "$tmp/path-a/same" >/dev/null 2>&1
DIREXTALK_AGENT_ROOT=${DIREXTALK_AGENT_ROOT:-$root/../agent} "$orchestrator" down "$tmp/path-b/same" >/dev/null 2>&1
if cmp "$tmp/path-a/same/.stack-id" "$tmp/path-b/same/.stack-id" >/dev/null 2>&1; then
  echo 'same-basename isolation failed: persisted stack identities collided' >&2
  exit 1
fi
echo 'agent-core-local parallel dry-run isolation passed'
