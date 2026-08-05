# Current Agent and MCP Contract

> Current code, generated contract metadata, and focused tests are the authority.
> Detailed external Agent Core and Execution V2 requirements are recorded in
> [`agent-core-integration-development-contract.md`](agent-core-integration-development-contract.md)
> and [`adr/2026-07-31-execution-orchestration-v2.md`](adr/2026-07-31-execution-orchestration-v2.md).
> Native Agent execution and data are owned by the separately deployed
> `dirextalk-agent` service and are capability/readiness gated.

This document is the backend-owned current contract for Dirextalk Agent state, Native Agent, and external MCP access. It describes existing behavior; it does not add a compatibility surface.

Registration, schemas, or documentation alone never make an action or
capability live.

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

- `dirextalk-agent` owns Native Agent conversations, models, encrypted provider credentials, knowledge and long-term memory, tasks, schedules, Skills/MCP lifecycle, Execution V2, AWS state, and runtime data. `dirextalk-message-server` owns the Flutter-facing owner-authenticated `agent.*` actions and `client.native_agent_stream` / `server.native_agent_stream.*` frames, and proxies them to the external Agent Capability gRPC service. Flutter never connects to `dirextalk-agent` directly.
- The message server exposes product contacts, rooms, members, messages, and channel content back to the Agent through the separate Product Capability gRPC service. A Product Capability handler must not synchronously call back into Agent. Both directions carry the authenticated owner, account generation, granted scopes, operation identity, and call-chain fence; loops fail closed.
- Native Agent is not installed, enabled, configured, or invoked through `plugins.*`. Backend `plugins.*` actions remain for non-Agent plugins.
- Model-backed Native Agent chat and compression resolve the owner’s Agent-owned default `conversation` model profile. Flutter configures profiles through the proxied model-profile actions and sends only profile identifiers and exact revision pins; it does not persist or send inline API keys after server-profile synchronization. Model API keys are write-only and must not be returned, logged, or injected into unrelated extension/runtime state.
- Native Agent BYOK web search is request-scoped. The owner may call `agent.web_search.test` over HTTP or the owner WebSocket with `tool_credentials.web_search.enabled=true`, `provider=tavily`, and a Tavily `api_key`; the key is write-only and is never stored in Agent config, model profiles, durable turns, conversation memory, logs, errors, or results. The same valid request credential adds the compiled `web_search` Eino tool to that chat turn; without it, the tool is absent.
- Web search performs one bounded HTTPS Tavily request (a local injected endpoint is only a test seam), rejects redirects, trims queries to 1,000 Unicode characters, clamps and re-enforces `max_results` to 1–10, limits provider bodies to 2 MiB, and applies a 15-second timeout. Responses contain only bounded answer/title/content previews, URLs, scores, and provider metadata. Provider bodies and credential values are not returned on errors.
- Durable Native Agent turn digests and events are secret-free. A reconnect/resume request may subscribe to an existing turn without resending or recovering `tool_credentials`; credentials remain available only to the original in-memory request execution and are never reconstructed from durable state.
- With persistent server conversation memory, the client sends only the current prompt, `conversation_id`, durable `turn_id`, and attachment references. It does not replay `messages`; the server rejects such history, loads the authoritative transcript, automatically summarizes older context against the model token budget, and generates the first successful conversation title with a redacted first-instruction fallback.
- Native Agent deployment planning treats an empty target inventory as a signal
  to compare and reserve a new AWS target, not as a terminal error. The bounded
  target-reservation tool creates only a logical revision-1 reservation; EC2
  creation remains an owner-confirmed `resource_purchase` stage. AI runtime
  plans ask the owner to choose an existing server-owned API-key secret
  reference or an interactive authorization gate and never collect secret
  plaintext in chat.
- If AWS credential inventory is empty, Native Agent directs the owner to the
  AWS credential management surface and does not reserve a target. A listed
  credential is eligible only when `verified_revision == revision`. Credential
  and model-secret readback exposes configured state and conservative display
  hints only; display masks are never accepted as replacement secret values.
- `skills.server` is advertised only when the external Agent registry publishes
  the full Skills lifecycle. `mcp` is advertised only when MCP lifecycle
  operations are published. A product-capability bridge by itself advertises
  neither token.
- Supported model-provider identifiers are `openai`, `anthropic`, `deepseek`, `gemini`, `xai`, `openai_compatible`, and `openrouter`. `litellm`, `vertex`, and unknown identifiers are rejected; clients use `openai_compatible` for custom compatible endpoints.
- `agent.models.list` preserves upstream `input_modalities` only when the
  provider explicitly returns it on the model or its `architecture`. The
  server normalizes known `text` and `image` values and never infers image
  support from a model ID, name, provider, or URL. A client may preselect its
  image-input control from this metadata, while an existing saved profile's
  explicit modalities remain authoritative.
- Native Agent knowledge is an owner-scoped server surface. Source upload uses
  `agent.knowledge.sources.list`, `.delete`, `agent.knowledge.upload.start`,
  `.chunk`, and `.finish`; V1 accepts valid UTF-8 `text/plain`,
  `text/markdown`, `text/csv`, and `application/json` files up to 10 MiB, with
  canonical base64 chunks no larger than 256 KiB. Upload progress is byte-based;
  a source is `ready` only after all vectors and the source record commit
  atomically. The current owner default embedding profile is resolved by the
  server, and knowledge actions never accept or return model credentials,
  provider settings, or base URLs.
- `agent.knowledge.memory.create` is the singular Eino remember/recall write
  tool. `agent.knowledge.memories.list`, `.update`, and `.delete` expose the
  editable durable-memory records; they are distinct from conversation
  summaries and uploaded source chunks. Managed mutations are owner-scoped,
  revision-checked, and idempotent.
- Successful `agent.chat` responses and Native Agent stream `done` payloads may include additive `references[]` derived deterministically from the full successful built-in Dirextalk tool results from that run. Room references use `kind=room`, `room_id`, optional `room_type=direct|group|channel`, `title`, and optional `preview`; channel-post references use `kind=channel_post`, `room_id`, `channel_id`, `post_id`, `title`, and optional `preview`. References preserve tool/result order, deduplicate rooms and posts, never include message `event_id`, and are not inferred from model-authored text or third-party/runtime tool output.
- `mcp.channel_posts.list` and the Agent-side `dirextalk_channel_posts_list` result envelope include both top-level `channel_id` and `room_id`, allowing a post reference to identify its product channel and Matrix room without parsing post content.

### Native Agent schedule chat tools

- Interactive Native Agent turns expose only bounded read-only `native_agent_schedules_list`, `native_agent_schedules_get`, `native_agent_schedule_runs_list`, and `native_agent_schedule_runs_get` tools. ProductCore `agent.schedules.*` actions remain the owner-authenticated CRUD/runtime surface.
- The former Native Agent schedule write proposal/confirmation flow (`native_agent_schedules_confirm` and its durable schedule-confirmation store) has been retired and removed. Native Agent no longer stages or confirms create, update, enable, delete, or run-now mutations, and no dormant compatibility path remains.
- These read-only tools are separate from the restricted scheduled runner allowlist and from the Online Agent Matrix room/timeline. The scheduled runner cannot call mutation tools.

## Execution Orchestration V2 (contract and release gate)

The detailed normative contract is
[`agent-core-integration-development-contract.md`](agent-core-integration-development-contract.md);
the accepted architecture decision is
[`adr/2026-07-31-execution-orchestration-v2.md`](adr/2026-07-31-execution-orchestration-v2.md).
V2 planning is declarative and side-effect free; remote mutations use the
Agent-owned typed coordinator with durable receipts and explicit uncertainty.
The Message Server is only the authenticated product proxy and does not execute
third-party shell/code/Skills or expose raw SSM/SSH/AWS passthrough.

Every `execution.v2.*` capability and ProductCore action is published only
after its authenticated route, durable PostgreSQL state, typed
executor/transport, focused tests, and explicit readiness/enablement all pass.
The final Execution V2 schema is registered by the Agent migration registry,
but runtime capability/readiness remains separately gated; action registration,
schemas, and docs alone are not live. AWS SSM is the first production slice; SSH,
generic HTTP, DNS, TLS, and Coding Worker remain deferred and must not be
advertised.

## Consumer Boundaries

- `dirextalk-connect` owns the local conversation bridge. It consumes the Matrix session and real `agent_room_id` for message sync/send and consumes the deployed `https://<server>/mcp` endpoint only through a runtime capability that supports connect-managed MCP. Host-managed runtimes keep MCP enrollment in their host runtime.
- `dirextalk-deployer` creates the Agent Matrix session, writes service-scoped `dirextalk-connect` configuration, records the canonical `/mcp` endpoint and Agent bearer credential, and generates only the runtime-specific MCP artifacts allowed by the capability registry.
- Neither consumer owns MCP business logic. They must not recreate a local MCP CLI, daemon, proxy, stdio bridge, listening port, fixed `mcp.*` product action path, or alternate endpoint.
- Flutter reads Online Agent availability from Matrix state in `agent_room_id` and uses backend-owned `agent.*` actions and native stream frames for Native Agent. It does not call fixed `mcp.*` product actions.

## Retired Legacy Matrix Gateway (Release Gate M history)

- The Release Gate M Legacy Matrix Gateway adapter, public facade, runtime consumer, and reservation storage implementation have been removed. No startup switch, client path, or dormant runtime module remains in Message Server; the historical `dirextalk.agent_gateway.v1`/`io.dirextalk.agent.invoke.v1` contract is not exposed.
- PostgreSQL migration v38 and its `p2p_legacy_agent_invocations` table DDL remain registered solely so upgraded databases can open and retain historical schema. No current runtime reads or writes that table.
