# Current Agent and MCP Contract

> Agent Core integration source behavior is recorded in
> [`agent-core-integration-development-contract.md`](agent-core-integration-development-contract.md)
> and is capability/readiness gated. This document remains the backend-owned
> contract for the currently active Agent room, Native Agent, and external MCP
> paths; it does not claim live Core or AWS activation.

This document is the backend-owned current contract for Dirextalk Agent state, Native Agent, and external MCP access. It describes existing behavior; it does not add a compatibility surface.

## External MCP

- External MCP clients use the standard Streamable HTTP endpoint `POST /mcp`. `/_p2p/mcp` is unavailable.
- Requests are MCP JSON-RPC and currently support `initialize`, `tools/list`, and `tools/call`.
- Authentication is `Authorization: Bearer <agent_token>`. The owner access token is not accepted, bearer tokens in query parameters are rejected, and the inbound bearer token must not be forwarded in tool arguments or to downstream services.
- The endpoint validates `Origin`. `GET /mcp` returns method not allowed while server-to-client streaming is unused.
- Fixed `mcp.*` body actions are removed from `/_p2p/query` and `/_p2p/command`. Any `mcp.*` action identifiers that remain in backend packages are internal capability identifiers, not callable product actions.
- `POST /mcp` and Native Agent built-in Dirextalk tools share the backend `internal/dirextalkmcp` registry, schemas, pagination, room authorization, DTOs, and invocation service. Durable member `membership` uses the Matrix enum and only `join` means joined; `joined` is reserved for operation/result `status`. Room and contact discovery return only active joined conversations. Message history/send, room-member listing, channel-post listing, and channel-comment listing/creation reject pending, joining, left, unknown, or otherwise non-joined rooms before any Matrix read or write. Room-member results contain only `membership=join` members. The configured real Agent room is checked against its current Matrix `join` membership. Room IDs in `mcp_blocked_room_ids` are filtered from discovery and rejected on direct access.

## Agent Room Status

- The configured `agent_room_id` is a real private Matrix room containing the owner and local `@agent:<server>` user.
- Bridge availability is Matrix room state type `io.dirextalk.agent.status`, state key `@agent:<server>`, with content field `online`.
- The running bridge publishes `online=true` or `online=false` through its Matrix session. Server startup or repair and `agent.config.update enabled=false` may publish `online=false` as a safe fallback.
- The server does not infer bridge availability from Agent configuration, `/sync`, realtime WebSocket lifetime, or Matrix presence. `sync.bootstrap` returns `agent_room_id`, not `agent_online`, and does not emit `agent.presence`.

## Native Agent Ownership

- Native Agent is owned by `dirextalk-message-server`. The backend owns native `agent.*` actions, `client.native_agent_stream` / `server.native_agent_stream.*` frames, model-provider request handling, skills, external MCP client wiring, runtime CLI tools, orchestration, built-in Dirextalk tools, native config storage, and sanitized migration from the former hidden Agent plugin config.
- Native Agent is not installed, enabled, configured, or invoked through `plugins.*`. Backend `plugins.*` actions remain for non-Agent plugins.
- Each model-backed chat or compression request must provide a request-scoped `model_profile` with non-empty `provider`, `model`, `base_url`, and `api_key`. The message server does not fill missing model IDs or API addresses from provider defaults or legacy server config; missing required fields are request errors. Model API keys must not be persisted, returned, or injected into plugin or runtime environment state.
- Supported model-provider identifiers are `openai`, `anthropic`, `deepseek`, `gemini`, `xai`, `openai_compatible`, and `openrouter`. `litellm`, `vertex`, and unknown identifiers are rejected; clients use `openai_compatible` for custom compatible endpoints.
- Successful `agent.chat` responses and Native Agent stream `done` payloads may include additive `references[]` derived deterministically from the full successful built-in Dirextalk tool results from that run. Room references use `kind=room`, `room_id`, optional `room_type=direct|group|channel`, `title`, and optional `preview`; channel-post references use `kind=channel_post`, `room_id`, `channel_id`, `post_id`, `title`, and optional `preview`. References preserve tool/result order, deduplicate rooms and posts, never include message `event_id`, and are not inferred from model-authored text or third-party/runtime tool output.
- `mcp.channel_posts.list` and the embedded `dirextalk_channel_posts_list` result envelope include both top-level `channel_id` and `room_id`, allowing a post reference to identify its product channel and Matrix room without parsing post content.

## Consumer Boundaries

- `dirextalk-connect` owns the local conversation bridge. It consumes the Matrix session and real `agent_room_id` for message sync/send and consumes the deployed `https://<server>/mcp` endpoint only through a runtime capability that supports connect-managed MCP. Host-managed runtimes keep MCP enrollment in their host runtime.
- `dirextalk-deployer` creates the Agent Matrix session, writes service-scoped `dirextalk-connect` configuration, records the canonical `/mcp` endpoint and Agent bearer credential, and generates only the runtime-specific MCP artifacts allowed by the capability registry.
- Neither consumer owns MCP business logic. They must not recreate a local MCP CLI, daemon, proxy, stdio bridge, listening port, fixed `mcp.*` product action path, or alternate endpoint.
- Flutter reads Online Agent availability from Matrix state in `agent_room_id` and uses backend-owned `agent.*` actions and native stream frames for Native Agent. It does not call fixed `mcp.*` product actions.

## vNext Legacy Matrix Gateway Foundation (Release Gate M)

- The internal Gateway adapter accepts only owner-authored `io.dirextalk.agent.invoke.v1` timeline events from the configured real `agent_room_id`. Its consumer uses an independent JetStream durable, so an Agent Control outage cannot block normal ProductCore projections.
- Invoke content is capped at 16 KiB and strictly contains `request_id`, `installation_id`, optional `preferred_connector_id`, `dispatch_mode`, `grant_version`, `input_event_id`, `required_capabilities`, and `idempotency_key`. UUIDs are canonical UUIDv7; capabilities are bounded, lowercase, unique, and sorted. Unknown/duplicate fields, trailing JSON, unsafe grant versions, the wrong room, and non-owner senders are ignored without creating a Run.
- PostgreSQL migration v38 stores one reservation per `(matrix_room_id, request_id)`, with unique source event and tenant/room/idempotency digest constraints. It stores the local Matrix input reference and normalized routing facts, but never the prompt body or raw idempotency key. Crash replay returns the first generated opaque request event and request digest; accepted/rejected terminal facts are source-digest fenced and immutable.
- Agent Control is called through the frozen `dirextalk.agent_gateway.v1.AgentRunIngress/CreateAgentRun` contract. The client requires TLS 1.3, HTTP/2, explicit server roots, a clientAuth-only certificate with exactly `spiffe://dirextalk.internal/v1/tenants/<tenant>/services/legacy-matrix-gateway`, 64 KiB message limits, and a deadline no longer than 20 seconds. Request and idempotency digests use the frozen LP/COMMIT transcript.
- The production monolith does not expose a startup switch for this adapter yet. Activation remains deliberately unavailable until deployment can prove the old Connect room consumer is stopped and fenced; otherwise one Matrix input could execute through both paths.
- This foundation durably creates or replays an Agent Run. Exclusive-consumer cutover, Run completion/evidence ingress, `io.dirextalk.agent.result.v1` / `io.dirextalk.agent.error.v1` projection, and restricted plain-text fallback remain later Release Gate M work; the server must not fabricate completion or evidence from an admission receipt.
