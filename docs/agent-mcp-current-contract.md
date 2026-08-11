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
- `agent.config.get` returns two mode-specific Agent identities: `native_agent_identity` for Ying / Native Agent and `online_agent_identity` for Your Agent / Matrix bridge Agent. Each identity contains `display_name` and `avatar_url`. Top-level `display_name` and `avatar_url` remain legacy compatibility fields and mirror the effective Native Agent identity. A legacy `agent.config.update` that only sends non-empty top-level identity fields updates both mode identities; nested identity fields merge per mode and take precedence for that mode. Missing fields and empty strings do not clear existing names or avatars.
- Native Agent runtime config receives only the effective `native_agent_identity` through the legacy top-level `display_name` and `avatar_url` shape. The runtime must not receive or persist `online_agent_identity`.
- `agent.matrix_session.create` uses `online_agent_identity` for the Matrix Agent session profile. Updating `online_agent_identity` also synchronizes the local `@agent:<server>` Matrix global profile and the configured Agent room's `m.room.member` profile. Updating only `native_agent_identity` does not touch Matrix. If the desired config is saved but Matrix profile/member synchronization fails, `agent.config.update` returns `agent_identity_sync_failed`; a later `agent.config.get` still returns the saved desired identity.

## Native Agent Ownership

- `dirextalk-agent` owns Native Agent conversations, models, encrypted provider credentials, knowledge and long-term memory, tasks, schedules, Skills/MCP lifecycle, Execution V2, AWS state, and runtime data. `dirextalk-message-server` owns the Flutter-facing owner-authenticated `agent.*` actions and `client.native_agent_stream` / `server.native_agent_stream.*` frames, and proxies them to the external Agent Capability gRPC service. Flutter never connects to `dirextalk-agent` directly.
- The message server exposes product contacts, rooms, members, messages, and channel content back to the Agent through the separate Product Capability gRPC service. A Product Capability handler must not synchronously call back into Agent. Both directions carry the authenticated owner, account generation, granted scopes, operation identity, and call-chain fence; loops fail closed.
- Native Agent is not installed, enabled, configured, or invoked through `plugins.*`. Backend `plugins.*` actions remain for non-Agent plugins.
- Model-backed Native Agent `agent.chat` and `agent.chat.stream` require the owner-selected `model_profile_id` together with positive `model_profile_revision` and `credential_version` pins. The durable stream may also carry explicit local MCP selections bound to an installed/enabled installation UUID, its exact source version or commit, active content digest, and a non-empty exact tool allowlist. The model chooses among only those selected local tools and server-owned intrinsics; local execution failure never authorizes a Cloud Worker retry. The message server forwards these immutable pins unchanged; inline profiles, tool credentials, client profile aliases, and default-profile fallback are rejected before capability execution. Compression and other server-owned workflows retain their separately documented default behavior. Flutter configures profiles through the proxied model-profile actions and sends only profile identifiers and exact revision pins; it does not persist or send inline API keys after server-profile synchronization. Model API keys are write-only and must not be returned, logged, or injected into unrelated extension/runtime state.
- Native Agent BYOK web search uses an Agent-owned encrypted Tavily credential.
  Owners read safe state with `agent.web_search.config.get`, update it with
  revision-checked `agent.web_search.config.update`, and verify it with
  `agent.web_search.test`. The API key is accepted only by config update, is
  write-only, and is never returned through config, test, chat, logs, errors,
  durable turns, or events. Readback exposes only configured state and the
  non-secret fixed hint `configured`.
- The current Web Search provider set is exactly `tavily`. Additional providers
  require provider-specific schemas, validation, adapters, and encrypted
  fields; neither ProductCore nor Agent exposes an arbitrary key/value
  credential store.
- Chat and stream requests do not accept `tool_credentials`; the compiled
  `web_search` Eino tool is available only from enabled, configured Agent-owned
  web-search state. This is a single credential path, not a request fallback.
- Web search performs one bounded HTTPS Tavily request (a local injected endpoint is only a test seam), rejects redirects, trims queries to 1,000 Unicode characters, clamps and re-enforces `max_results` to 1–10, limits provider bodies to 2 MiB, and applies a 15-second timeout. Responses contain only bounded answer/title/content previews, URLs, scores, and provider metadata. Provider bodies and credential values are not returned on errors.
- Typed selection tools are Agent-owned and are exposed only through the
  owner-authenticated ProductCore actions `agent.text_tools.config.get`,
  `agent.text_tools.config.update`, and `agent.text_tools.execute`. Config get
  has empty parameters. Config update is a revision-checked, idempotent
  full-list replacement of at most 32 ordered tools, with at most six enabled;
  it may add, delete, or reorder stable default IDs (`translation`, `summary`, `explanation`,
  `search`) and UUID custom IDs. Execute accepts exactly `tool_id`, a
  bounded selected-text value, and required `output_language` (`zh` or `en`),
  which fixes the output language for both default and custom tools; it never accepts a prompt, model/profile,
  history, or credential field. Results expose only bounded output and at most
  five bounded `{title,url,snippet}` sources. Message Server has no text-tool
  database or runtime fallback, and a possibly dispatched config mutation is
  reconciled with config get rather than automatically replayed.
- `text_tools.server` is advertised only when the registered
  `agent.text_tools.v1` capability is ready with all three operations and the
  exact pinned request/result schema identities.
- Image text tools are a separate owner-authenticated, one-shot typed surface:
  `agent.image_tools.upload.begin`, `agent.image_tools.upload.append`,
  `agent.image_tools.upload.commit`, `agent.image_tools.extract_text`, and
  `agent.image_tools.translate_text`. Flutter downloads and decrypts Matrix
  media, then uploads only JPEG, PNG, or WebP bytes through the bounded 8 MiB,
  1 MiB-chunk flow. The contract never accepts an MXC/HTTP URL, local path,
  data URI, inline prompt/history, selected text, model/profile identifier, or
  credential field. A committed source has revision 1 and is owner/account-
  generation scoped, expires after the Agent-defined short lifetime, and is
  consumed by exactly one extraction or translation execution. Translation
  requires a canonical BCP-47 target locale; successful execution returns only
  the request/source identity, optional target locale, and at most 64 KiB of
  UTF-8 text (which may be empty). Message Server stores no image or OCR state,
  pins `agent.image_tools.v1` schemas on each live lookup, and deliberately does
  not add this optional capability to the ordinary model/chat readiness gate.
- Durable Native Agent turn digests and events are secret-free. Reconnect and
  resume requests never carry, persist, or reconstruct a credential from turn
  state; the Agent resolves web-search configuration through its encrypted
  owner-scoped store.
- With persistent server conversation memory, the client sends only the current `message`, canonical `conversation_id`, one canonical `idempotency_key`, and committed attachment references. Reconnect reuses the same idempotency key with the latest `after_seq`; `turn_id`, `client_message_id`, `request_id`, and `prompt` are not start-stream aliases. It does not replay `messages`; the server rejects such history, loads the authoritative transcript, automatically summarizes older context against the model token budget, and generates the first successful conversation title with a redacted first-instruction fallback.
- Agent-authored durable progress is the only source of the internal turn
  identity. Every business event, including `accepted`, carries the same
  canonical `idempotency_key`, `conversation_id`, `turn_id`, and a positive
  authoritative `revision`; the ProductCore request correlation `id` and the
  capability protocol operation ID are never projected as `turn_id`. The
  `stream_chat` result and nested done response use `idempotency_key`, not a
  `request_id` alias. Input, result, and event schema digests are all pinned by
  Message Server readiness. A local MCP/Skill confirmation pause is projected
  as the non-terminal `waiting_confirmation` event with exact
  `confirmation_id`, `execution_id`, and
  `status=waiting_confirmation`; it is never represented as an empty tool
  event or inferred from model text. `attempt_id` is not part of the public
  durable event. Non-waiting events cannot carry confirmation, execution, or
  waiting status authority, and waiting events cannot mix text, tool call,
  tool result, response, or error fields.
- Durable turn reconciliation uses `agent.chat.turns.list` with one canonical
  conversation UUID, an optional opaque page token of at most 4,096 bytes, and
  an optional limit from 1 through 1,000. Each returned turn is the exact
  ten-field public metadata projection: internal `turn_id`, original start
  `idempotency_key`, `conversation_id`, `state`, `revision`, `last_sequence`,
  `terminal_code`, `terminal_summary`, `created_at`, and `updated_at`. Prompt,
  request fingerprints, model/profile data, credential material, and execution
  snapshots remain Agent-private; aliases, extra fields, and malformed UUIDs
  fail closed at both proxy boundaries. `agent.chat.turn.stop` is the typed
  `agent.chat.v1/stop_turn` mutation and accepts exactly `idempotency_key`,
  internal `turn_id`, and `expected_revision`; it returns the same exact
  ten-field authoritative metadata. It never calls the generic capability
  `CancelOperation` RPC.
- Generating Native turns accept same-turn guidance only through
  `agent.chat.turn.steer`, bound to `agent.chat.v1/steer_turn`. The mutation
  sends exactly a new UUID `idempotency_key`, the authoritative `turn_id`, its
  positive `expected_revision`, and one bounded non-empty `instruction`.
  Agent Core persists the instruction, invalidates the current provider lease,
  and regenerates the same turn immediately. The result retains the original
  start idempotency identity and adds the exact `steer_idempotency_key`
  receipt. Message Server never creates, queues, or starts a successor turn.
- Native Agent deployment planning treats an empty target inventory as a signal
  to compare and reserve a new AWS target, not as a terminal error. The bounded
  target-reservation tool creates only a logical revision-1 reservation; EC2
  creation remains an owner-confirmed `resource_purchase` stage. AI runtime
  plans ask the owner to choose an existing server-owned API-key secret
  reference or an interactive authorization gate and never collect secret
  plaintext in chat.
- If AWS credential inventory is empty, Native Agent fails closed and does not
  reserve a target. The Agent-owned AWS credential API contract remains, but
  the current Flutter release does not expose its management UI. A listed
  credential is eligible only when `verified_revision == revision`. Credential
  and model-secret readback exposes configured state and conservative display
  hints only; display masks are never accepted as replacement secret values.
- `skills.server` is advertised only when the external Agent registry publishes
  the full Skills lifecycle. `mcp` is advertised only when MCP lifecycle
  operations are published. A product-capability bridge by itself advertises
  neither token.
- The current Flutter release hides AWS management/planning UI, and the
  external Agent registry release-hides AWS-specific Skills: they are not
  listed, selected explicitly or by intent, added to bootstrap metadata, or
  injected into the Native Agent prompt. Message Server does not recreate an
  embedded Planning Skill catalog or runtime fallback for this visibility gate.
- Supported model-provider identifiers are `openai`, `anthropic`, `deepseek`, `gemini`, `xai`, `openai_compatible`, and `openrouter`. `litellm`, `vertex`, and unknown identifiers are rejected; clients use `openai_compatible` for custom compatible endpoints.
- `agent.models.list` is the provider/runtime catalog backed by Agent Core
  `agent.info.v1/list_models`; it returns `models` and `providers` and remains
  separate from `agent.model_profiles.list` and
  `agent.core.model_profiles.list`, which return persisted `profiles` from
  `agent.models.v1/list_models`. An omitted `model_kind` is canonicalized to
  `conversation` at the gateway boundary. The catalog preserves upstream
  `input_modalities` only when the
  provider explicitly returns it on the model or its `architecture`. The
  server normalizes known `text` and `image` values and never infers image
  support from a model ID, name, provider, or URL. A client may preselect its
  image-input control from this metadata, while an existing saved profile's
  explicit modalities remain authoritative.
- Native Agent knowledge is an owner-scoped server surface. Source upload uses
  `agent.knowledge.sources.list`, `.delete`, `agent.knowledge.upload.start`,
  `.chunk`, and `.finish`; V1 accepts valid UTF-8 `text/plain`,
  `text/markdown`, and `application/json` files up to 16 MiB, with
  canonical base64 chunks no larger than 256 KiB. `upload.start` requires the
  complete content SHA-256 before any session is created; upload progress is
  byte-based, and a source is `ready` only after all vectors and the source
  record commit atomically. The current owner default embedding profile is
  resolved by the server, and knowledge actions never accept or return model
  credentials, provider settings, or base URLs. Config reads/writes expose the
  non-secret profile id/revision, model, collection digest, and config revision;
  search pages additionally expose the exact embedding generation and replay
  those values from opaque cursor snapshots. Retained Knowledge content has a
  64 MiB owner quota. `agent.knowledge.status` strictly projects the Agent-owned
  source lifecycle counters `ready_count`, `uploading_count`, `indexing_count`,
  `failed_count`, and `cleanup_pending_count`, their `checked_at` timestamp, and
  the `quota_used_bytes`, `quota_limit_bytes`, `quota_remaining_bytes`, and
  `max_source_bytes` counters. Agent `RESOURCE_EXHAUSTED` failures carrying
  `details.code=knowledge_quota_exceeded` map to ProductCore HTTP 413 with both
  `code` and `error_code` set to `knowledge_quota_exceeded`.
- `agent.knowledge.memory.create` is the singular Eino remember/recall write
  tool. `agent.knowledge.memories.list`, `.update`, and `.delete` expose the
  editable durable-memory records; they are distinct from conversation
  summaries and uploaded source chunks. Managed mutations are owner-scoped,
  revision-checked, and idempotent.
- Successful `agent.chat` responses and Native Agent stream `done` payloads may include additive `related_task_ids`, `related_plan_ids`, and strict `references[]`. Message Server promotes only fields authored by Agent at the top level, on the assistant message, or in the nested stream response; it never synthesizes a reference from a related id. Room references derived from successful built-in Dirextalk tool results use `kind=room`, `room_id`, optional `room_type=direct|group|channel`, `title`, and optional `preview`; channel-post references use `kind=channel_post`, `room_id`, `channel_id`, `post_id`, `title`, and optional `preview`. Execution references use `kind=execution_plan|execution_run|execution_confirmation` and require the complete account-generation, task/plan/run/confirmation UUID, revision, and digest linkage authored by Agent. They are informational projections, not confirmation authority. References preserve producer order, reject duplicates or unknown fields/kinds, never include message `event_id`, and are not inferred from model-authored text or third-party/runtime tool output.
- `mcp.channel_posts.list` and the Agent-side `dirextalk_channel_posts_list` result envelope include both top-level `channel_id` and `room_id`, allowing a post reference to identify its product channel and Matrix room without parsing post content.

### Native Agent schedule chat tools

- Interactive Native Agent turns expose bounded read-only
  `native_agent_schedules_list`, `native_agent_schedules_get`,
  `native_agent_schedule_runs_list`, and `native_agent_schedule_runs_get`
  tools. When the Agent conversation/schedule store is composed, a durable
  turn also receives the Core-owned `agent.schedule.create` intrinsic for
  natural-language creation of one-time or recurring schedules.
- `agent.schedule.create` is internal to Agent Core. It is not a new
  ProductCore action or a Product Capability callback. The model supplies only
  bounded schedule intent, trigger, timezone, and timeout arguments; Core
  injects owner, account generation, conversation, and pinned model profile
  authority from the fenced `TurnLease` and commits the schedule with the turn
  receipt.
- ProductCore `agent.core.schedules.*` actions are the only owner-authenticated
  CRUD/runtime surface. Native turns do not expose schedule update, pause,
  resume, delete, or trigger mutations to the model. The read tools and Core
  create intrinsic remain separate from the restricted scheduled-runner
  allowlist and from the Online Agent Matrix room/timeline; the scheduled
  runner cannot call mutation tools.

## Execution Orchestration V2 (contract and release gate)

The detailed normative contract is
[`agent-core-integration-development-contract.md`](agent-core-integration-development-contract.md);
the accepted architecture decision is
[`adr/2026-07-31-execution-orchestration-v2.md`](adr/2026-07-31-execution-orchestration-v2.md).
V2 planning is declarative and side-effect free; remote mutations use the
Agent-owned typed coordinator with durable receipts and explicit uncertainty.
The Message Server is only the authenticated product proxy and does not execute
third-party shell/code/Skills or expose raw SSM/SSH/AWS passthrough. Agent's
single Pi Cloud Worker path preserves local Native Agent MCP/Skills for local
work but does not copy those installations, their credentials, or the Extension
Runner into an ephemeral Worker.

Every `execution.v2.*` capability and ProductCore action is published only
after its authenticated route, durable PostgreSQL state, typed
executor/transport, focused tests, and explicit readiness/enablement all pass.
The final Execution V2 schema is registered by the Agent migration registry,
but runtime capability/readiness remains separately gated; action registration,
schemas, and docs alone are not live. Cloud Worker offers originate only in an
Agent conversation and use generic `agent.core.confirmations.*`; the former
Execution V2 confirmation aliases and public `runs.reconcile` are absent.
Worker completion reaches Message Server only through the private fixed
`product.agent_execution.v1/record_completion` receipt callback after Agent
result validation and verified cleanup.

Verified Cloud Worker output is retrievable only through the owner-authenticated
`agent.execution.v2.artifacts.download` proxy. It is a bounded 512 KiB,
offset-based read with exact per-chunk and whole-artifact SHA-256 metadata.
Message Server returns the validated bytes and public identity/range fields;
it never exposes the Agent's S3 locator, a pre-signed storage URL, retention
ledger, Worker diagnostics, or provider credentials.

Cloud Worker `agent.execution.v2.runs.events` returns the exact
`events`/`next_sequence`/`history_truncated` envelope. Agent retains a bounded
4096-event history per run; `history_truncated=true` means the requested
`after_sequence` precedes the retained window and the returned cursor starts at
the oldest retained event. A non-truncated page starts at `after_sequence+1`;
all events after the first item in either page form are contiguous.
`worker_progress` is the only event type that may
carry `progress`, and it must carry the complete secret-free snapshot:
`phase`, `elapsed_ms`, `last_activity_at`, `cpu_time_ms`,
`memory_high_water_bytes`, `invocation_count`, `uploaded_bytes`, and
`output_truncated`. Phase is one of `claimed`, `preparing_inputs`, `running_pi`,
`uploading_result`, or `completing`; all counters are nonnegative and bounded by
the generated ProductCore contract. A zero CPU or memory value means the Worker
had no verified runtime metric source; it does not assert zero resource use.
Lifecycle events must not carry progress.
Model text, stderr, paths, environment values, secrets, and object-storage
identities are rejected rather than projected to Message Server or clients.

## Consumer Boundaries

- `dirextalk-connect` owns the local conversation bridge. It consumes the Matrix session and real `agent_room_id` for message sync/send and consumes the deployed `https://<server>/mcp` endpoint only through a runtime capability that supports connect-managed MCP. Host-managed runtimes keep MCP enrollment in their host runtime.
- `dirextalk-deployer` creates the Agent Matrix session, writes service-scoped `dirextalk-connect` configuration, records the canonical `/mcp` endpoint and Agent bearer credential, and generates only the runtime-specific MCP artifacts allowed by the capability registry.
- Neither consumer owns MCP business logic. They must not recreate a local MCP CLI, daemon, proxy, stdio bridge, listening port, fixed `mcp.*` product action path, or alternate endpoint.
- Flutter reads Online Agent availability from Matrix state in `agent_room_id` and uses backend-owned `agent.*` actions and native stream frames for Native Agent. It does not call fixed `mcp.*` product actions.
