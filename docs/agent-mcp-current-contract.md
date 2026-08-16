# Current Agent and MCP contract

Current code and generated contract metadata are authoritative. The cross-
service topology is defined in
[`agent-core-integration-development-contract.md`](agent-core-integration-development-contract.md).

## External MCP

- External MCP clients use Streamable HTTP at `POST /mcp`; `/_p2p/mcp` is not
  available.
- Requests are MCP JSON-RPC and support `initialize`, `tools/list`, and
  `tools/call`.
- Authentication is `Authorization: Bearer <agent_token>`. Owner access tokens
  and query-string bearer tokens are rejected. The inbound token is never
  forwarded in arguments or downstream requests.
- Origin is validated. `GET /mcp` is method-not-allowed while server-to-client
  MCP streaming is unused.
- Fixed `mcp.*` ProductCore body actions remain deleted.
- `POST /mcp` and Agent's Dirextalk product tools share
  `internal/dirextalkmcp` registry schemas, pagination, authorization, DTOs,
  and invocation logic. Matrix membership must be exactly `join`; pending,
  joining, left, unknown, and blocked rooms are rejected before Matrix reads or
  writes.

## Matrix Agent room

- `agent_room_id` is a private Matrix room containing the owner and local
  `@agent:<server>` user.
- Bridge availability is Matrix state `io.dirextalk.agent.status` with state key
  `@agent:<server>` and one `online` field. Message Server does not infer it
  from Native Agent HTTP/SSE connections.
- `agent.matrix_session.create` is an Agent-token control action that returns a
  Matrix session for the local Agent identity. It never returns the owner
  Matrix session, portal password, or Agent bearer token.
- `agent.password` is owner-only account control. It is not a Native Agent
  runtime credential.

## Native Agent owner data plane

- Flutter first calls owner-only ProductCore action `agent.session.create`, then
  sends the returned 15-minute ticket to same-origin `/agent/v1/*` routes.
- Message Server no longer registers or proxies Native Agent business actions.
  Chat, attachments, confirmations, Workers, models/config, credentials,
  memory/Knowledge, Tasks/schedules, Skills/MCP, static sites, image/text/web
  tools, voice, runtime, AWS, and Execution V2 are direct Agent HTTP APIs.
- Writes are ordinary POST admissions returning `202`; observation is separate
  resumable SSE; history and state are authoritative Agent GETs. SSE disconnect
  cancels only the watch.
- Client retries preserve the exact operation identity and idempotency tuple.
  Stop uses turn plus idempotency identity; revision CAS is reserved for steer,
  attachments, and other genuinely concurrent mutations.
- Agent owns prompt/profile validation, encrypted credentials, reasoning and
  transcript persistence, long-term memory, Worker progress, domains/TLS,
  static-site releases, and artifact bytes. Message Server does not inspect,
  reshape, or store them.
- Native Agent is not installed, enabled, configured, or invoked through
  `plugins.*`; those actions remain for non-Agent plugins only.

## Product Capability callback

Agent reaches contacts, rooms, members, messages, and channel content through
the private Product Capability gRPC service. It is an Agent-to-Message-Server
direction with mTLS, direction token, peer/account-generation identity,
operation identity, scoped grants, and call-chain loop rejection.

Handlers never synchronously call Agent. Execution completion is an async,
private callback that records only a minimal receipt and Product event. The
client then reads authoritative conversation, Worker, and artifact state from
Agent.

## Consumer boundaries

- ProductCore exposes exactly five `agent.*` control actions:
  `agent.password`, `agent.matrix_session.create`, `agent.session.create`,
  `agent.config.get`, and `agent.config.update`. The config pair is limited to
  Message Server-owned Online Matrix identity, enablement, and MCP blocked-room
  policy; it is not a Native Agent runtime proxy.
- Agent bearer token remains valid only for `agent.matrix_session.create` and
  standard `POST /mcp`; it cannot mint an owner Agent data-plane ticket.
- Native Agent tickets are short-lived, owner/account-generation/scope bound,
  and never persisted or logged by Message Server.
- No removed ProductCore Agent action, gRPC owner proxy, schema pin table,
  synchronous Watch, or compatibility alias may be reintroduced.
