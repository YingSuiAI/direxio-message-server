#!/bin/sh
# Static and dry-run guard checks for the two-stack acceptance orchestrator.
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)
script=$root/scripts/agent-core-e2e.sh
compose=$root/docker-compose.agent-core-local.yml
override=$root/docker-compose.agent-core-e2e.yml
agent_compose=${DIREXTALK_AGENT_COMPOSE:-$root/../agent/deploy/container/compose.local.yaml}
fail() { echo "agent-core-e2e assertion failed: $*" >&2; exit 1; }
contains() { rg -q --fixed-strings -- "$1" "$2" || fail "$1 missing from $2"; }

contains '.run.agent-core-e2e' "$script"
contains 'COMPOSE_PROFILES=extensions,core-runner' "$script"
contains 'DIREXTALK_CORE_EXTENSION_ENABLED' "$script"
contains 'DIREXTALK_CORE_WORKLOAD_ENABLED' "$script"
contains '/usr/local/bin/dirextalk-agent healthcheck' "$script"
contains '--expect-instance-id' "$script"
for capability in agent.info mcp skill conversation.extensions workload.core_runner; do
  contains "--require-capability $capability" "$script"
done
contains 'dirextalk-agent-e2e-$stack_id' "$script"
contains 'dirextalk-message-server-e2e-$stack_id' "$script"
contains 'down --volumes --remove-orphans' "$script"
contains 'refusing cleanup' "$script"
contains 'docker compose' "$script"
contains 'message-server loopback port' "$script"

if rg -n 'docker (rm|volume rm|network rm)|docker system prune|docker compose down$' "$script" >/dev/null; then
  fail 'orchestrator contains a broad Docker deletion path'
fi
if rg -n 'echo .*\$(token|password|secret)|cat .*\$(token|password|secret)' "$script" >/dev/null; then
  fail 'orchestrator prints or reads secret values'
fi
if rg -n 'DIREXTALK_(AGENT|MESSAGE).*PASSWORD[^_A-Z].*=' "$script" >/dev/null; then
  fail 'orchestrator accepts password values instead of file references'
fi

contains 'profiles: ["extensions"]' "$agent_compose"
contains 'profiles: ["core-runner"]' "$agent_compose"
contains 'name: ${DIREXTALK_MESSAGE_SERVER_PROJECT:?set the per-run Message Server project name}' "$compose"
contains '127.0.0.1::8008' "$compose"
contains 'POSTGRES_PASSWORD_FILE: /run/secrets/message_postgres_password' "$override"
contains 'message_postgres_password' "$override"
contains 'PGPASSFILE: /run/secrets/message_postgres_pgpass' "$override"
contains 'DIREXTALK_MESSAGE_POSTGRES_PGPASS_FILE' "$override"
contains '-db "postgres://${DIREXTALK_MESSAGE_POSTGRES_USER:-dirextalk_message_server}@postgres/' "$override"
if rg -n 'DIREXTALK_E2E_CORE_CAPABILITIES_PROBE|PASSWORD_PLACEHOLDER|sed -f' "$script" "$override" >/dev/null; then
  fail 'E2E uses an arbitrary capability probe or interpolates a database password into config'
fi
if rg -n 'POSTGRES_PASSWORD: \$\{|generate-config.*\$\{.*PASSWORD' "$override" >/dev/null; then
  fail 'E2E PostgreSQL password is exposed through Compose environment/argv'
fi
contains 'external: true' "$compose"

# The Message Server opens PostgreSQL through lib/pq. Keep the passwordless
# DSN contract tied to the actual driver behavior rather than assuming that
# every Go PostgreSQL driver implements libpq's password-file lookup.
pq_dir=$(go list -m -f '{{.Dir}}' github.com/lib/pq 2>/dev/null || true)
[ -n "$pq_dir" ] && [ -f "$pq_dir/conn.go" ] || fail 'lib/pq module source is unavailable for PGPASSFILE validation'
rg -q 'os\.Getenv\("PGPASSFILE"\)' "$pq_dir/conn.go" || fail 'lib/pq no longer honors PGPASSFILE'

postgres_count=$(rg -c '^  postgres:' "$compose" "$agent_compose" | awk -F: '{sum += $NF} END {print sum + 0}')
[ "$postgres_count" -eq 2 ] || fail "expected exactly two product PostgreSQL services, found $postgres_count"
if rg -n 'docker\.sock|/var/run/docker' "$compose" "$agent_compose" >/dev/null; then
  fail 'Compose topology exposes a Docker socket'
fi

if "$script" config 'Bad-ID' >/dev/null 2>&1; then
  fail 'invalid STACK_ID unexpectedly accepted'
fi
if "$script" status >/dev/null 2>&1; then
  fail 'status without STACK_ID unexpectedly accepted'
fi

# Prepare and expand two independent stacks without starting a container. The
# bootstrap emits only paths/metadata; this test never opens a secret file.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export DIREXTALK_E2E_RUN_ROOT=$tmp/runs
export DIREXTALK_AGENT_IMAGE_IMMUTABLE=example/agent@sha256:1111111111111111111111111111111111111111111111111111111111111111
export DIREXTALK_EXTENSION_RUNNER_IMAGE_IMMUTABLE=example/runner@sha256:2222222222222222222222222222222222222222222222222222222222222222
export DIREXTALK_CORE_RUNNER_IMAGE_IMMUTABLE=example/core-runner@sha256:3333333333333333333333333333333333333333333333333333333333333333
export DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=postgres@sha256:4444444444444444444444444444444444444444444444444444444444444444
export DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=example/server@sha256:5555555555555555555555555555555555555555555555555555555555555555
for id in alpha beta; do
  "$script" prepare "$id" >/dev/null 2>&1
  "$script" config "$id" >/dev/null 2>&1
  [ -r "$tmp/runs/$id/agent/.env" ] || fail "missing Agent env for $id"
  [ -r "$tmp/runs/$id/server.env" ] || fail "missing Server env for $id"
  agent_name=$(docker compose --profile extensions --profile core-runner --env-file "$tmp/runs/$id/agent/.env" -f "$agent_compose" config --format json | jq -r '.name')
  [ "$agent_name" = "dirextalk-agent-e2e-$id" ] || fail "Agent Compose project name is not stack-scoped for $id"
  server_name=$(docker compose --profile extensions --profile core-runner --env-file "$tmp/runs/$id/server.env" -f "$compose" -f "$override" config --format json | jq -r '.name')
  [ "$server_name" = "dirextalk-message-server-e2e-$id" ] || fail "Server Compose project name is not stack-scoped for $id"
  server_json=$(docker compose --profile extensions --profile core-runner --env-file "$tmp/runs/$id/server.env" -f "$compose" -f "$override" config --format json)
  printf '%s' "$server_json" | jq -e '.services["message-server-init"].build == null and .services["message-server"].build == null' >/dev/null ||
    fail 'E2E overlay unexpectedly retains Message Server build entries'
  printf '%s' "$server_json" | jq -e '.services["message-server-init"].environment.PGPASSFILE == "/run/secrets/message_postgres_pgpass" and .services["message-server"].environment.PGPASSFILE == "/run/secrets/message_postgres_pgpass"' >/dev/null ||
    fail 'PGPASSFILE is not mounted into every Message Server DB consumer'
done
grep -Fqx 'DIREXTALK_MESSAGE_SERVER_PROJECT=dirextalk-message-server-e2e-alpha' "$tmp/runs/alpha/server.env" || fail 'alpha project name is not stack-scoped'
grep -Fqx 'DIREXTALK_MESSAGE_SERVER_PROJECT=dirextalk-message-server-e2e-beta' "$tmp/runs/beta/server.env" || fail 'beta project name is not stack-scoped'
if cmp "$tmp/runs/alpha/agent/.env" "$tmp/runs/beta/agent/.env" >/dev/null 2>&1; then
  fail 'two stack environments unexpectedly overlap'
fi
grep -Fqx 'DIREXTALK_MESSAGE_POSTGRES_PASSWORD_FILE='"$tmp/runs/alpha/message-postgres-password" "$tmp/runs/alpha/server.env" ||
  fail 'Message Server PostgreSQL password is not file-backed'
grep -Fqx 'DIREXTALK_MESSAGE_POSTGRES_PGPASS_FILE='"$tmp/runs/alpha/message-postgres-pgpass" "$tmp/runs/alpha/server.env" ||
  fail 'Message Server PostgreSQL pgpass is not file-backed'
[ "$(stat -c '%a' "$tmp/runs/alpha/message-postgres-pgpass")" = 400 ] || fail 'pgpass file mode is unsafe'
grep -Eq '^postgres:5432:dirextalk_message_server:dirextalk_message_server:[A-Za-z0-9._~-]+$' "$tmp/runs/alpha/message-postgres-pgpass" ||
  fail 'generated pgpass entry is malformed'
runner_line=$(rg -n 'up -d extension-runner core-runner' "$script" | cut -d: -f1)
core_line=$(rg -n 'up -d core$' "$script" | cut -d: -f1)
server_line=$(rg -n 'up -d$' "$script" | tail -n 1 | cut -d: -f1)
[ "$runner_line" -lt "$core_line" ] && [ "$core_line" -lt "$server_line" ] ||
  fail 'staged runner-before-core-before-server order is missing'
mkdir -p "$tmp/normal-extension" "$tmp/normal-workload"
if DIREXTALK_E2E_EXTENSION_CGROUP_ROOT=$tmp/normal-extension \
  DIREXTALK_E2E_CORE_RUNNER_CGROUP_ROOT=$tmp/normal-workload \
  "$script" up alpha >/dev/null 2>&1; then
  fail 'ordinary directories were accepted as delegated cgroup roots'
fi
chmod u+w "$tmp/runs/beta/agent/.env"
printf '%s\n' '# tampered' >> "$tmp/runs/beta/agent/.env"
if "$script" config beta >/dev/null 2>&1; then
  fail 'runtime manifest tampering unexpectedly accepted'
fi

echo 'agent-core-e2e isolation/config/secret guards passed'
