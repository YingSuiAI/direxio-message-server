# Native Agent data-plane contract

Status: current split-service contract

This document defines the Message Server boundary. Agent routes and payloads
are owned by `dirextalk-agent`; the Flutter client consumes those routes through
the same public origin.

## Topology and ownership

- Message Server owns login, portal/account control, Matrix/ProductCore, the
  standard external `POST /mcp`, and short-lived Agent session ticket issuance.
- Agent owns its HTTP data plane: conversations, turns, reasoning/events,
  attachments, confirmations, Worker lifecycle, models and credentials,
  memory/Knowledge, Tasks/schedules, Skills/MCP, static sites, runtime tools,
  AWS state, and Execution V2 artifacts.
- Caddy routes `/agent/v1/*` directly to the Docker service name `agent:8082`
  without stripping the path. The Agent port is exposed only to the shared
  Compose edge network and is never host-published or sent to the client.
- Flutter uses the public Message Server domain for both control and data
  planes. It never receives an Agent internal address, long-lived Agent service
  token, the portal-owned external MCP `agent_token`, client certificate, or
  database credential. Portal login responses contain only the owner session;
  the host credentials file retains `agent_token` for trusted operator tools.
- Message Server contains no Agent business-action proxy, schema projection,
  synchronous Watch, conversation store, Worker adapter, or catalog digest
  table. Superseded ProductCore `agent.*` business actions are not aliases for
  the direct data plane.

## Session ticket

An authenticated owner obtains a ticket through the existing ProductCore
envelope with action `agent.session.create`. The optional request field
`session_id` is a canonical UUID; omitting it creates a new UUID. The exact
response fields are:

- `ticket`: compact Ed25519 JWS/JWT;
- `expires_at`: UTC RFC3339 expiry;
- `server_time`: UTC RFC3339 ticket issuance time;
- `base_path`: `/agent/v1`;
- `session_id`: canonical UUID;
- `scopes`: the explicit current owner-client Agent scope set.

The ticket lifetime is 15 minutes. Its claims are exactly `iss`, `aud`, `sub`,
`account_generation`, `session_id`, `nonce`, `scope`, `iat`, and `exp`.
`iss` is `dirextalk-message-server`, `aud` is `dirextalk-agent-data`, `sub` is
the portal owner MXID, and owner/account generation are server-derived. The
signing key is the existing capability-grant Ed25519 key; only its public key
is mounted into Agent.

Agent rejects expired tickets with `401 AGENT_TICKET_EXPIRED`. Missing scope,
foreign owner, or stale account generation is `403`. The client cannot choose
or widen scopes. Tickets and Authorization headers must not be logged.

The immutable `dirextalk-capability-api` v1.2.0 tag is the shared contract
authority. Its generated `agentdatav2.AgentSessionResponse` and
`conformance/agent-data-plane/v2` vectors freeze the session response and
owner data-plane wire shapes consumed by Message Server, Agent, and Flutter.
Ticket lifetime and signing behavior remain Message Server policy.
The generated owner scope set includes `agent:servers:read`,
`agent:servers:write`, and `agent:servers:destroy` for the Agent-owned server
inventory lifecycle; Message Server issues those scopes but does not proxy or
store server inventory.

## HTTP and SSE semantics

- A business mutation is a normal HTTP `POST`. Agent persists admission before
  returning `202 Accepted` with the frozen `operation_id`, `turn_id` where
  applicable, and `idempotency_key`; the request does not wait for execution or
  stream completion.
- An unknown POST outcome is resolved by authoritative GET or by replaying the
  identical frozen request and idempotency tuple. A retry never creates a new
  key, revision, or parameter set.
- Streaming output uses a separate `text/event-stream` GET. Event sequence is
  durable and resumes after `Last-Event-ID` or `after_seq`; Agent history and
  status GETs are authoritative after reconnect or app restart.
- Caddy preserves the full path, Host, forwarding headers, and Authorization.
  Its Agent reverse proxy uses `flush_interval -1` and no short stream read
  timeout or response buffering.
- Disconnecting an SSE request cancels only that observation. It never cancels,
  repeats, or changes the durable operation.
- Stop binds only the original `turn_id` and `idempotency_key`. Steer and
  attachment mutations use revision CAS only where their concurrent state
  actually requires it.
- Reasoning is Agent-owned history. Each turn segment remains independently
  recoverable and foldable by the client; a later steer does not overwrite an
  earlier reasoning segment.

## Product collaboration

Agent may call Message Server's private Product Capability gRPC service to read
or mutate Matrix/ProductCore state. That direction retains mTLS, direction
token, Agent peer identity, account-generation fence, operation identity, and
call-chain loop rejection. Product Capability handlers never synchronously
call Agent. Follow-up work is an Agent-owned async event/callback.

The private `product.agent_execution.v1/record_completion` callback stores only
the minimal completion receipt and one owner realtime invalidation. Result
text, Worker state, artifacts, quotes, and AWS details remain Agent authority;
Flutter treats the Product event as an invalidation and reads Agent state.

The external MCP endpoint is separate: `POST /mcp` authenticates the existing
Agent bearer token and exposes Message Server-owned Dirextalk tools. It is not
the Native Agent data plane and does not restore removed ProductCore actions.

## Data, deletion, and deployment

Message Server and Agent may share a PostgreSQL cluster only through separate
roles and databases/schemas. Neither service reads or mutates the other's
tables. Agent attachments, credentials, conversations, Worker records, and
artifacts never enter Message Server storage.

For account deletion the client first completes the Agent direct
`agent:account:deprovision` operation, then invokes Message Server portal
account deletion. Message Server does not synchronously call Agent during its
destructive control-plane transaction.

Fresh deployment enables Agent HTTP on `0.0.0.0:8082`, joins Agent to
`message_public`, and exposes but does not publish container port 8082. Caddy's
`/agent/v1/*` handler precedes the Message Server catch-all while preserving
the `/.sites/*` static-site handler. Agent starts and becomes healthy before
Message Server. Update flows replace Agent first, then restart/update Message
Server so downstream Product Capability and public routing observe the current
Agent.

The integration is fresh-state only. Do not add embedded runtime fallback,
dual routes, action aliases, proxy compatibility, legacy import, or schema
recalculation in Message Server.

## Focused acceptance

- `agent.session.create` signature, owner, account-generation, scopes,
  session/nonce, and 15-minute expiry tests;
- ProductCore routing proves owner acceptance and Agent-token rejection for
  ticket issuance;
- generated ProductCore action contract contains only the five Message
  Server-owned control actions: `agent.password`,
  `agent.matrix_session.create`, `agent.session.create`, `agent.config.get`,
  and `agent.config.update`; the two config actions own only Online Matrix
  identity, enablement, and MCP blocked-room policy, never Agent runtime data;
- split Compose/provision/Caddy tests prove `agent:8082`, same-origin path
  preservation, unbuffered SSE, no host-published Agent port, and Agent-before-
  Message-Server startup;
- Agent, Flutter, and real device/emulator tests own the direct POST, SSE
  resume, history, attachment, confirmation, and Worker workflows.
