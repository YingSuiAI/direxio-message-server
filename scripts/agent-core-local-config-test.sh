#!/bin/sh
# Static boundary assertions for the two-project local integration baseline.
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
compose=$root/docker-compose.agent-core-local.yml
agent_compose=${DIREXTALK_AGENT_COMPOSE:-$root/../agent/deploy/container/compose.local.yaml}

fail() { echo "agent-core-local config assertion failed: $*" >&2; exit 1; }
contains() { rg -q --fixed-strings "$1" "$2" || fail "$1 missing from $2"; }

contains 'name: ${DIREXTALK_MESSAGE_SERVER_PROJECT:?set the per-run Message Server project name}' "$compose"
contains 'name: dirextalk-agent-core-local' "$agent_compose"
contains 'P2P_AGENT_CORE_ADDRESS: ${P2P_AGENT_CORE_ADDRESS:-core:9443}' "$compose"
contains 'P2P_AGENT_CORE_SERVER_NAME: ${P2P_AGENT_CORE_SERVER_NAME:?set the Agent Core TLS SNI}' "$compose"
contains 'P2P_AGENT_CORE_CA_FILE: /run/agent-core/ca-cert' "$compose"
contains 'P2P_AGENT_CORE_TOKEN_FILE: /run/agent-core/service-token' "$compose"
contains 'P2P_AGENT_CORE_EXPECTED_INSTANCE_ID: ${P2P_AGENT_CORE_EXPECTED_INSTANCE_ID:?set the Agent Core instance ID}' "$compose"
contains 'P2P_PLUGIN_DOCKER_ENABLED: "false"' "$compose"
contains 'agent_caller:' "$compose"
contains 'external: true' "$compose"
contains 'DIREXTALK_MESSAGE_SERVER_PRIVATE_NETWORK:?set the per-run Message Server private network' "$compose"
contains 'DIREXTALK_MESSAGE_AGENT_CORE_MATERIAL_VOLUME:?set the per-run Agent Core material volume' "$compose"
contains 'networks: [message_server_private, agent_caller, message_server_public]' "$compose"
contains 'message_server_public:' "$compose"
contains 'DIREXTALK_MESSAGE_SERVER_PUBLIC_NETWORK:?set the per-run Message Server public network' "$compose"
contains 'networks: [agent_private]' "$agent_compose"
contains 'networks: [agent_private, agent_caller, agent_egress]' "$agent_compose"

if rg -q 'docker\.sock|/var/run/docker' "$compose"; then
  fail 'Message Server must not receive a Docker socket'
fi
if rg -q 'ports:.*9443|9443:9443' "$agent_compose"; then
  fail 'Agent gRPC must not be published to the host'
fi

# Count service declarations rather than image references: this catches a
# future accidental Runner DB or second PostgreSQL service in either project.
postgres_count=$(rg -c '^  postgres:' "$compose" "$agent_compose" | awk -F: '{sum += $NF} END {print sum + 0}')
[ "$postgres_count" -eq 2 ] || fail "expected exactly two postgres services, found $postgres_count"
if rg -n 'agent_database|agent-db|runner.*postgres|runner_database' "$compose" "$agent_compose" >/dev/null; then
  fail 'Agent database-private or Runner database network leaked into baseline'
fi
if rg -q 'dirextalk-message-server-private\}|dirextalk-message-server-postgres-data\}' "$compose"; then
  fail 'Compose contains a fixed global resource fallback'
fi

echo "agent-core-local compose boundary assertions passed"
