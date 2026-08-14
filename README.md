# Dirextalk Message Server

Dirextalk Message Server is Dirextalk's backend contract authority. It combines
a Matrix-compatible homeserver, ProductCore actions, product policy,
projections, external MCP access, and the authenticated proxy to the separately
deployed Native Agent runtime in one Go service.

It is based on Element Dendrite, but this repository is maintained as a Dirextalk product server rather than a general-purpose Matrix homeserver distribution.

![Dirextalk Message Server overview](docs/images/dirextalk-message-server-overview.png)

[中文说明](README_zh.md)

Each personal node keeps one contract boundary for Matrix, ProductCore, and
Native Agent:

- Matrix is the source of truth for rooms, membership, ordinary messages,
  media, history, search, unread state, and redaction.
- ProductCore actions provide the authenticated product facade for validation,
  remote forwarding, Matrix write orchestration, and projection reads.
- PostgreSQL-backed P2P tables are projection/read models unless a current
  contract explicitly makes a record authoritative.
- Flutter connects only to Message Server. The external `dirextalk-agent`
  service owns Native Agent execution and data; Message Server preserves the
  existing owner-authenticated `agent.*` actions and Native Agent WS frames.
- `POST /mcp` and Agent Product Capability callbacks reuse Message Server's
  product/Matrix services. They do not make Native Agent part of the plugin
  lifecycle or create a second client-facing API.

## Runtime

- Production entry point: `cmd/dirextalk-message-server`
- Docker image: `dirextalk/message-server:latest`
- Default config path in Docker: `/etc/dirextalk-message-server/message-server.yaml`
- Default data path in Docker: `/var/dirextalk-message-server`
- Go module: `github.com/YingSuiAI/dirextalk-message-server`
- Go version: `1.26.5`
- Server database: PostgreSQL only; SQLite and file DSNs are unsupported.
- Docker development database: PostgreSQL 18

## API Surface

Matrix protocol routes remain under:

- `/_matrix/*`
- `/_synapse/*`
- `/_dendrite/*`
- `/.well-known/matrix/*`

Dirextalk product APIs use the body-action surface:

- `GET /_p2p/health`
- `POST /_p2p/query`
- `POST /_p2p/command`
- `GET /_p2p/events`
- `GET /.well-known/portal/owner.json`

The generated ProductCore action metadata is
[docs/product-action-contract.json](docs/product-action-contract.json); it is
the checkable action list and may change as capabilities are added. Owner
clients use HTTP query/command, and subscribe to Product events with SSE.
External MCP
clients use JSON-RPC over `POST /mcp` with an `agent_token`.

Authentication and transport boundaries:

- Owner ProductCore actions use `Authorization: Bearer <access_token>`.
- `GET /_p2p/events` accepts the owner bearer token and resumes with
  `after_seq` or `Last-Event-ID`.
- `agent_token` is limited to `agent.matrix_session.create` and the standard
  `POST /mcp` endpoint; it cannot call owner actions.

Product requests use this envelope:

```json
{
  "action": "channels.public.get",
  "params": {
    "room_id": "!room:dendrite-a:8448",
    "remote_node_base_url": "https://dendrite-a:8448/_p2p"
  }
}
```

## Local Development

Run commands from the repository root. PowerShell, Bash on Linux, Bash on macOS, and Bash in WSL are all supported; choose the command form that matches the shell you are using.

Build the server:

```bash
go build ./cmd/dirextalk-message-server
go build ./cmd/dendrite
```

Run the single-node Docker stack:

```bash
docker compose -f docker-compose.p2p.yml up --build
docker compose -f docker-compose.p2p.yml exec message-server cat /var/dirextalk-message-server/p2p/bootstrap.json
```

Run the three-node regression stack.

PowerShell:

```powershell
$env:P2P_DUAL_PUBLIC_HOST = if ($env:P2P_DUAL_PUBLIC_HOST) { $env:P2P_DUAL_PUBLIC_HOST } else { "host.docker.internal" }
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python scripts/p2p-three-node-regression.py
```

Bash on Linux, macOS, or WSL:

```bash
export P2P_DUAL_PUBLIC_HOST="${P2P_DUAL_PUBLIC_HOST:-host.docker.internal}"
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python3 scripts/p2p-three-node-regression.py
```

Run tests against a local PostgreSQL instance:

PowerShell:

```powershell
$env:POSTGRES_USER = "postgres"
$env:POSTGRES_PASSWORD = "123789"
$env:POSTGRES_HOST = "localhost"
$env:POSTGRES_PORT = "5432"
$env:POSTGRES_DB = "postgres"
go test ./p2p ./internal/productpolicy -count=1
```

Bash:

```bash
export POSTGRES_USER=postgres
export POSTGRES_PASSWORD=123789
export POSTGRES_HOST=localhost
export POSTGRES_PORT=5432
export POSTGRES_DB=postgres
go test ./p2p ./internal/productpolicy -count=1
```

The Go test helper creates isolated `dendrite_test_*` databases and drops them when each test finishes.

## Contract Sources

Read these maintained current sources before changing clients, deployment
tooling, or Agent/MCP behavior:

- [Generated ProductCore action contract](docs/product-action-contract.json)
- [Current project documentation](docs/current-project-documentation.md)
- [Current Agent and MCP contract](docs/agent-mcp-current-contract.md)
- [External Agent Core integration contract](docs/agent-core-integration-development-contract.md)
- [Execution V2 ADR](docs/adr/2026-07-31-execution-orchestration-v2.md)

## Documentation

Current maintained docs are intentionally small. Historical Dendrite site docs, obsolete trackers, and one-off implementation plans are not maintained in this fork.

- [Current project documentation](docs/current-project-documentation.md)
- [Current Agent and MCP contract](docs/agent-mcp-current-contract.md)
- [Release notes](release/RELEASE_NOTES.md)
- [Docker image notes](docs/dirextalk-message-server.md)
- [Push gateway contract](docs/dirextalk-push-gateway.md)
- [Split Agent deployment](deploy/split-agent/README.md)

## License

This project retains upstream license and copyright notices where code originates from Element Dendrite. See [LICENSE](LICENSE) and [LICENSE-COMMERCIAL](LICENSE-COMMERCIAL).
