---
name: dirextalk-backend-contract-state-storage
description: Use when Dirextalk backend work changes ProductCore or MCP contracts, authentication, realtime WS behavior, Matrix events or state, projection, durable storage, reports, Agent/system rooms, groups, channels, or cross-node behavior.
---

# Backend Contract, State, And Storage

## Establish The Contract

Read the relevant generated/action contract, `docs/agent-mcp-current-contract.md`, focused tests, and current implementation before editing. Treat hand-written action lists as commentary, not authority.

- Product requests use the existing `{action, params}` envelope through HTTP or owner realtime WS. After `server.ready`, logged-in non-MCP actions may use WS `client.request`; when WS is not ready, use the same envelope through HTTP immediately. Retry a lost WS response over HTTP only for safe repeated actions, never for a WS business error.
- `GET /_p2p/ws` accepts a short-lived single-use owner ticket. Owner bearer tokens protect HTTP ProductCore calls. `agent_token` is limited to `agent.matrix_session.create` and `POST /mcp`.
- `POST /mcp` is bearer-authenticated Streamable HTTP JSON-RPC (`initialize`, `tools/list`, `tools/call`), not a ProductCore action. Keep fixed `mcp.*` body actions removed and never forward the inbound bearer token.
- `release.v1.apply` and `portal.account.delete` remain owner HTTP-only destructive commands. Release compatibility and operations come from the host updater, not local SemVer guesses.
- When a field, auth rule, route, or transport changes, update the generated contract/focused docs, server tests, and affected consumers together.
- Flutter connects only to Message Server. The external Agent owns its runtime,
  data, secrets, and runners; Message Server owns authentication, ProductCore
  actions/streams, and Product Capability callbacks.
- Native chat accepts only the complete server profile ID/revision/credential
  version pins. Reject inline profiles, history, tool credentials, and nested
  credential-like keys before HTTP, WS, replay, or cancellation dispatch.
- Product Capability handlers never synchronously call Agent. Preserve owner,
  account-generation, scope, operation, and call-chain fences in both gRPC
  directions; loops fail closed.

## Reuse Matrix

- Ordinary message/media/history/search/unread/read-marker/redaction behavior stays on Matrix APIs.
- Product room type/profile/member policy/join requests use native Matrix state. Matrix `membership=join` is the final joined fact.
- New groups use `history_visibility=joined`; current channels are unified post+chat rooms and use `shared`. `channel_type` metadata is not a behavior switch.
- `agent_room_id` and `system_room_id` are real durable Matrix rooms. Agent availability is `io.dirextalk.agent.status` state keyed by `@agent:<server>`; owner reports are `msg_type=report` timeline events.
- Native Agent tools and external `/mcp` share `internal/dirextalkmcp` schemas, authorization, pagination, DTOs, errors, and invocation. Adapt dependencies in `p2p`; do not fork MCP business logic.
- Native Agent navigation references are derived only from successful built-in Dirextalk tool-result envelopes, not model text or third-party/runtime tools. Keep room/post identity fields additive, deterministic, ordered, deduplicated, and free of message `event_id` unless the product contract explicitly expands to message-level navigation.
- Remote public lookup uses the supplied `remote_node_base_url`. Approval is not joined until the requester node completes Matrix join.

## Realtime And Push

- Product deltas are WS `server.event` messages with persisted sequence handling and `client.ack`; reset uses `server.cursor_reset` followed by a fresh metadata bootstrap.
- WS `client.lifecycle` and `client.focus` are the primary foreground/current-room signals for notification suppression. Global account data `io.dirextalk.push.context` is only a secondary fallback.
- Keep bootstrap metadata-only; message bodies and timeline history remain Matrix responsibilities.

## Persist Correctly

- Persist restart-relevant product facts in PostgreSQL. Update interfaces, migrations, implementations, callers, and tests together; keep migrations additive/idempotent where practical.
- Product projections follow Matrix output. Do not mutate a projection as an independent source of truth unless the domain contract explicitly says so.
- Account deletion first persists updater desired state `deprovisioned`; abort destructive work if that fails. Later failure best-effort restores `running` and returns a stable safe error if restoration also fails.
- Add restart/reopen coverage when recovery changes, and multi-node coverage when federation, remote join, or projection convergence changes.
- Agent integration uses a fresh schema owned by the Agent role. Do not add an
  embedded store, compatibility import, or cross-role table access.
