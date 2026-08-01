# API Interface Change Record

## 2026-08-01 Native Agent deployment tools, server context, and titles

Embedded Native Agent now treats the allowlisted Execution V2 analysis, target, plan, run, status, event, and Service Binding tools as compiled control-plane capabilities. A persisted pre-V2 `enabled_tools` list no longer hides them after upgrade; confirmation, raw SSM/SSH, AWS SDK passthrough, and arbitrary URL tools remain unavailable. Deployment intent selects at most three immutable planning skills: project intake, AWS target advice, and one container or source/systemd deployment recipe.

When persistent server conversation memory is ready, chat requests with a `conversation_id` accept only the current prompt/message and attachment references. Client-supplied `messages` history is rejected; the server loads, token-budgets, summarizes, and stores authoritative context. Model summarization is automatic when a configured model is available, with deterministic bounded text compaction as fallback. Context windows are interpreted as token capacity rather than message count.

After the first successful persistent turn, the server best-effort generates a short title with the active conversation model and updates only an empty title. Provider failure falls back to a redacted, bounded title derived from the first user instruction; explicit user renames always win.

Last updated: 2026-07-31

## 2026-07-31 Execution V2 reset and direct-final schema

The unreleased workload/provision/deployment V1 surface was removed rather
than migrated. There is no V1 read, reconcile, destroy, retry, dashboard, or
compatibility adapter, and no V1 row is converted into V2. GeoLibre exists
only as a declarative recipe/test fixture.

AWS credential CRUD/test remains available as reusable control-plane state.
Execution V2 actions and capabilities stay unpublished until their
PostgreSQL coordinator, durable dispatch fence, typed transport, and strict
client contract are ready together.

The migration history from upstream `main` through v77 is unchanged. All
branch-only Agent and Execution V2 schema is created by one direct-final v78
migration; there is no branch compatibility/backfill migration chain.

## 2026-07-30 Channel Post Visibility And Public-Post Paging

`channels.posts.create` accepts optional `visibility="public"|"private"`.
Missing visibility defaults to `private`; other explicit values return `400`.
The canonical visibility is written into the Matrix channel-post event and its
PostgreSQL projection.

`channels.posts.list` keeps its authenticated unfiltered behavior when paging
is omitted. Optional `page`/`page_size` enables newest-first paging. The new
unauthenticated `channels.public.posts.list` action always returns only public
posts, supports owner-node forwarding, and rejects mismatched or non-public
remote results. Visitor results remain non-personalized.

Channel content mutations require a joined member projection before Matrix or
ProductCore writes. Missing reaction targets return `404`. Empty detail fields
on `channels.update` and `groups.update` no longer clear existing values.

## 2026-07-31 Offline legacy model-secret upgrade

`agent-secretctl upgrade` is the explicit one-shot migration for legacy
model-profile credential rows. It is not part of `message-server-init`,
Compose, or server startup. Operators must stop the persistent service before
the run and create one restorable backup set containing the PostgreSQL
database, the v1 keyring file, and the legacy model-profile raw-key file.
`agent-secretctl init` creates or validates the v1 keyring; `upgrade` then uses
only environment-provided paths and the database DSN:

```text
P2P_AGENT_SECRET_KEYRING_FILE
P2P_AGENT_SECRET_DATABASE_DSN
P2P_AGENT_MODEL_PROFILE_KEY_FILE
```

Key material and database credentials are never accepted as command-line
arguments. The upgrade takes the maintenance fence, processes current and
historical rows with row locks/CAS, and overwrites their shared nonce and
ciphertext columns with the v1 envelope and metadata. It does not clear those
columns or store plaintext. Afterward, run `agent-secretctl verify` with the
legacy-key environment unset; verification must succeed using only the
keyring. An interrupted or failed run is resumable, while rollback must restore
the PostgreSQL, keyring, and legacy-key backups atomically rather than rolling
back only the image. Destructive v110 cleanup remains deferred until the fleet
has converged and the rollback window has closed.

The same image exposes the offline commands:

```text
agent-secretctl init|upgrade|verify|rotate
```

`rotate` remains the separate stopped-service active-key rotation workflow and
is not a substitute for the legacy model-secret upgrade.

## 2026-07-30 Native Agent selected model pins

Server-managed Native Agent chat accepts the selected `model_profile_id`
together with a complete `model_profile_revision` / `credential_version` pair.
The durable turn resolves that immutable profile and credential snapshot before
reservation and stores only the pin and secret-free request digest. Supplying
only one version field, or conflicting request/context pins, fails before a
turn is reserved. Omitting the profile still resolves the owner's default
conversation profile.

## 2026-07-29 Single-service Embedded Eino ownership (historical)

The selectable/deployment-bound Agent Core service is retired. Message Server
embeds Eino and publishes current Agent and management readiness only through
`agent.backends.get.embedded`. The legacy `core` discovery object remains
temporarily as an unavailable compatibility projection, and legacy
`agent.core.*` action names remain wire-compatible names while implementations
migrate in-process; neither implies a second service.

The V1 workload/deployment paragraphs from this entry are superseded by the
2026-07-31 reset above. Task, schedule, model, remote HTTPS MCP, and reusable
AWS credential control remain in-process; project execution is rebuilt only
on the V2 plan/run/stage contract.

Remote MCP discovery is bounded to the fixed Official Registry, Smithery,
Glama and GitHub catalogs. Existing `agent.core.mcp.*` DTOs keep their
hyphenated source/transport/purpose names and flat installation/version
projections. Install/update re-inspects the immutable source pin, secret
inputs remain write-only, and tool schemas are fixed by a transactional
empty-to-pinned CAS before execution.

Agent secrets use the versioned AES-256-GCM keyring selected by
`P2P_AGENT_SECRET_KEYRING_FILE`; keyring failure removes secret-dependent Agent
capabilities without disabling ordinary Matrix/ProductCore routes. The same
image exposes one-shot `agent-secretctl init|upgrade|verify|rotate`; legacy
model rows are not upgraded automatically at startup.

## 2026-07-28 Agent Core deployment ledger and dashboard (removed)

This unreleased V1 contract and its tables/actions were deleted by the
2026-07-31 V2 reset. It is retained here only as historical context and is not
an advertised or callable interface.

## 2026-07-28 Native Agent model input modalities and managed knowledge

`agent.models.list` now preserves upstream `input_modalities` metadata only
when the provider explicitly returns it at the model or architecture level.
The server normalizes the declared `text` and `image` values, ignores unknown
modality labels, and does not infer image support from a model ID, display
name, provider, or endpoint. Clients may use the returned `image` value to
preselect image-input support; existing saved profile metadata remains
authoritative until the user changes it.

Native Agent knowledge is server-managed and owner-scoped. The V1 source
surface is `agent.knowledge.sources.list`, `.delete`,
`agent.knowledge.upload.start`, `.chunk`, and `.finish`. Uploads accept only
valid UTF-8 `text/plain`, `text/markdown`, `text/csv`, or `application/json`
content (the client may use `.txt`, `.md`, `.markdown`, `.csv`, or `.json`
filenames), are capped at 10 MiB, and use canonical base64 chunks no larger
than 256 KiB. Upload responses expose byte `progress`; the embedding stage is
not reported as complete until all vectors and the ready source record are
committed atomically. Indexing resolves the server's current owner default
embedding profile. Knowledge actions never accept or return provider, base
URL, model credentials, or embedding API keys.

`agent.knowledge.memory.create` is the singular Native Agent remember tool.
The editable durable-memory surface is `agent.knowledge.memories.list`,
`.update`, and `.delete`; these records are the Eino remember/recall records
and are distinct from conversation-summary memory and uploaded source chunks.
Memory updates and deletes use owner scope, revision preconditions, and
idempotency keys. The canonical action metadata is generated in
`docs/product-action-contract.json`.

## 2026-07-27 Embedded schedules (Stage A)

Added owner-only `agent.schedules.create`, `.update`, `.get`, `.list`,
`.delete`, `.enable`, `.disable`, `.run_now` and `agent.schedule_runs.list`
and `.get`. Schedules accept one RFC3339 occurrence or a five-field cron with
an explicit IANA timezone, persist only server-owned model profile revision and
credential version, and expose bounded opaque-cursor readbacks. PostgreSQL
claims due work with row locks and fencing epochs; run output and errors are
bounded and secret-free. The restricted runner and scheduler are fail-closed:
the capability is unavailable until durable storage, profile-key readiness and
an explicit read-only runner are wired. Stage B chat confirmation is not part
of this surface.

## 2026-07-27 Embedded Native Agent schedule chat confirmation (Stage B)

Added an internal Native Agent chat-tool adapter over the Stage A schedule
handlers. Schedule list/get and run list/get are read-only, and disable remains
direct. Create/update/enable/delete/run_now first create a durable proposal;
they do not mutate a schedule or run until a later owner-authored Native turn
in the same `conversation_id` exactly normalizes to `确认执行 <code>`. Model
arguments, previous-turn text, caller booleans, and `dangerous_tools_confirm`
cannot substitute for that phrase. Confirmation rows contain only bounded,
canonical, secret-free parameters, a digest, deterministic Stage A idempotency
key, summary, expiry, revision, and approval code; no API key or token is
stored or returned. Claim/complete fencing and Stage A receipts provide
restart and concurrent replay of one authoritative terminal result. This is
an internal Native tool surface: no public ProductCore confirmation action was
added, the restricted scheduled runner allowlist is unchanged, and Online
Agent Matrix conversations remain a separate transport.

## 2026-07-27 Server-owned Native Agent model profiles

Added owner-only `agent.model_profiles.sync`, `.list`, `.get`, and `.delete`
actions. Profiles are persisted in PostgreSQL with an independently managed
AES-GCM keyring in the mounted Message Server Agent data volume. The serving
Message Server initializes/verifies the protected keyring after PostgreSQL
migrations at `P2P_AGENT_SECRET_KEYRING_FILE` (normally
`/var/dirextalk-message-server/agent/secret-keyring.json`) when the file is
absent and no keyring-bound ciphertext exists. The keyring volume and
PostgreSQL must be restored or reset atomically; losing only the keyring while
retaining encrypted rows fails closed. No updater, external Agent, or second
persistent service participates in this initialization. API keys are write-only and are
never returned; reads expose `api_key_configured`, credential version, and an
optional read-only `api_key_hint` mask (for example `sk-********0420`) for
non-speech profiles. The hint is never accepted as a credential or stored in
the profile table; a redacted idempotency response cache may retain the mask
for replay. Speech profiles expose only their `provider_secret_status`.
Profile revisions are separate from retained encrypted credential versions so
long-running work can resolve a pinned version after restart. Server model
profile resolution is selected by a profile ID; `agent.models.list` may add
the request-scoped provider/base URL rules documented below. The previous
inline `model_profile` plus request key remains accepted during the rolling
upgrade window. `agent.backends.get.embedded.capabilities` advertises
`model_profiles.server` only when the encrypted store is ready.

For a local, non-publishing Agent-v1 image, run
`bash scripts/release/build-agent-v1.sh` from a clean checkout. It always uses
`--no-cache --pull`, tags `dirextalk/message-server:agent-v1`, and verifies its
version, full commit label, build-time label, variant label, and both server
executables. The normal Compose default remains `dirextalk/message-server:latest`;
set `MESSAGE_SERVER_IMAGE=dirextalk/message-server:agent-v1` to select the
verified local tag for both `message-server-init` and `message-server`. For a
published deployment, set that variable to the corresponding immutable image
digest (`dirextalk/message-server@sha256:...`) after independently verifying
the same labels.

Profile edits are retained as immutable snapshots, logical deletion preserves
historical profile and credential resolution, and concurrent identical sync
idempotency keys return the committed response. Credential history is protected
by a real profile foreign key and parent rows are committed before credentials
or revision snapshots. Delete idempotency claims a request digest atomically;
same-payload retries replay safely while a different payload conflicts. List
cursors are opaque `(client_profile_id, profile_id)` cursors and only the
requested page is decrypted. Numeric DTO fields reject loose strings,
fractional integers, and non-finite values while preserving explicit zero.
Durable Native Agent turns persist the selected profile revision and credential
version without storing credentials; queued and replayed execution resolves
the immutable pinned snapshot even after profile rotation, deletion, or
reactivation. Legacy inline `model_profile` turns remain compatible.

## 2026-07-25 Agent Core control-plane bridge

Added owner-only `agent.core.tasks.*`, `agent.core.schedules.*`,
`agent.core.confirmations.*`, `agent.core.mcp.*`, and `agent.core.skills.*`
actions over the deployment-bound Agent v1 TLS/token channel. Task events are
bounded replay reads; mutation requests carry idempotency keys and optional
revision preconditions, and all pagination, enum, and timestamp values are
normalized at the ProductCore boundary. Core status capabilities gate every
optional family, so absent token capabilities return a precondition result.
Projections omit upstream errors, credentials, token/endpoint values,
secret bytes, install material, and runtime/artifact paths; summaries are
bounded and sanitized. The external Agent gRPC client surface is retired and
unavailable; only the internal Release Gate M parser/store compatibility
remains. Core gRPC statuses use the stable `agent_core_*` mapping documented by
the integration contract for historical compatibility only. The versioned
Schedule, Confirmation, Task, and Extension protobuf snapshots are copied from
Agent baseline `11eed51e2a9e6431f28039a542f2424f290e6fff`.

## 2026-07-25 Agent Core model profiles

Added owner-only `agent.core.model_profiles.sync`, `.list`, `.get`, and
`.delete` actions. The external TLS/gRPC Core client path is retired and
unavailable; the current adapter uses the embedded Message Server runtime and
its local persistence only. Sync forwards one atomic batch, complete
non-secret settings, optional expected revisions, the selected default client
profile, and API-key presence only when explicitly supplied; keys are write-
only and are never persisted, logged, echoed, or included in responses.
Profile projections expose only safe metadata and `api_key_configured`. Core
gRPC statuses map to stable ProductCore status/code pairs
(`agent_core_invalid_argument`, `agent_core_trust_failed`,
`agent_core_not_found`, `agent_core_conflict`,
`agent_core_precondition_failed`, `agent_core_unavailable`, and
`agent_core_upstream_failed`) without forwarding upstream text. The
`model.profile` capability is now implemented; Core remains incompatible and
has no live client surface.

## 2026-07-25 Agent Core Discovery

Owner-only `agent.backends.get` and `agent.core.status.get` expose the
deployment-bound Agent Core discovery projection. The external TLS 1.3 gRPC
`AgentService` v1 client is retired and unavailable; the embedded runtime now
reports only the local capability intersection and model provider identifiers.
Responses never expose Core addresses, certificate/token paths, tokens, or
upstream error text. Backend selection remains client-local, but the only live
backend is embedded.

## 2026-07-23 Native Agent Anthropic, Gemini, And xAI Model Lists

`agent.models.list` now fetches Anthropic's real `GET /v1/models` endpoint with the request-scoped `x-api-key` and `anthropic-version` headers. `gemini` and `xai` are now supported Native Agent providers. Gemini exposes `https://generativelanguage.googleapis.com/v1beta` as its client-visible provider base and uses its native `/models`, `generateContent`, and `streamGenerateContent` routes with `x-goog-api-key`; custom Gemini-native gateways use the same contract and a root URL is normalized to `/v1beta`. xAI uses `https://api.x.ai/v1`. Provider API keys remain request-scoped on the server and are never returned.

Model-backed `agent.chat`, Native Agent streaming, and model compression resolve the owner default conversation profile when `model_profile` is omitted. Legacy inline `model_profile` remains compatible; provider-specific model-list fetches still require explicit `base_url`.

`agent.models.list` may instead select a server-owned encrypted profile with
`model_profile_id` or `client_model_profile_id`. In that form the request may
omit `provider` or repeat the stored provider (case-insensitively), and may
provide a temporary `base_url` override for this read only when it has the
same scheme, hostname, and effective port as the stored profile URL. Malformed
URLs, userinfo, and cross-origin overrides are rejected before any outbound
request. A request `api_key` is rejected; the server uses the decrypted
profile key for the outbound lookup. The optional `model_kind` defaults to
`conversation`; V1 `embedding` lists are OpenRouter-only and use
`{base_url}/embeddings/models`, while conversation lists request OpenRouter's
text-output filter (and SiliconFlow's `type=text&sub_type=chat` filter) and
remove explicitly non-text models. The action never
persists or mutates the selected profile.

Native Agent stream failures now preserve a safe provider/runtime error for the client while redacting the request API key. This replaces the generic `native agent turn failed` result for errors that occur after a durable turn has already been accepted.

Native Agent provider selection now uses an explicit allowlist. `litellm` has been removed from provider metadata and model-list routing, while `litellm`, `vertex`, and other unknown provider identifiers are rejected by model-backed requests. Custom OpenAI-compatible services use the existing `openai_compatible` provider.

## 2026-07-21 Durable Native Agent Turns v1

`server.ready` now advertises `native_agent_turns=1`. Existing `client.native_agent_stream` requests without `params.turn_id` retain their connection-scoped behavior and wire frames. A request with `params.turn_id` uses the durable owner-scoped turn lifecycle. `turn_id` and the resolved `conversation_id` are restricted to 1-256 characters from the server's identifier alphabet. The server commits the unique `(owner_id, turn_id)` reservation before sending `server.native_agent_stream.accepted`; an exact secret-free request-digest replay attaches to the existing turn, while reuse with a different action, conversation, or digest returns HTTP-equivalent status `409` with `code=M_TURN_ID_REUSED`.

Durable turn states are `accepted -> running -> succeeded|failed|stopped|interrupted`. Accepted work runs from a service-owned context, not the WebSocket context. Closing the page/socket or sending `client.native_agent_stream.cancel` detaches that attachment and does not stop execution; the cancelled reply includes `execution_continues=true`. The owner-only HTTP and realtime-request action `agent.chat.turn.stop` accepts `turn_id`, is idempotent, and is the only durable execution cancellation. `agent.chat.turns.list` accepts optional `conversation_id` and `limit` and returns the owner's turns in newest-first order.

Turns for the same owner and conversation execute serially in durable acceptance order, and an exact replay never executes twice. PostgreSQL is authoritative for state and replay events. A server restart changes any unsafe `accepted` or `running` row to terminal `interrupted` and never retries its possibly side-effecting runtime work. Terminal state plus its final `done`, `failed`, `stopped`, or `interrupted` record is committed before it is emitted.

A durable attachment may include `params.after_seq`. The server first emits `server.native_agent_stream.accepted`, then replays stored records with `seq > after_seq`, and then follows live records. Durable `server.native_agent_stream.event` and `server.native_agent_stream.error` frames include `turn_id`, `conversation_id`, and monotonic per-turn `seq`. Event persistence recursively removes API keys, access/bearer tokens, authorization headers, passwords, secrets, and private keys. Full request params and request-scoped model profiles are never stored; PostgreSQL stores only their secret-free SHA-256 digest. Prompts remain governed by the existing authorized Native Agent conversation-memory contract rather than a second turn-input store.

When a durable turn selects a server-owned model profile by ID, acceptance
captures its immutable profile revision and retained credential version. Runtime
execution resolves that exact pair, so edits, rotation, logical deletion, and
reactivation cannot change an already accepted turn. Inline legacy profiles
continue to use the existing secret-free digest and are not converted to the
server-owned profile path.

Native Agent conversation-memory persistence is now part of successful completion: if saving the authorized model context fails, `agent.chat` returns an error and a stream does not emit successful `done`. Runtime memory remains model context only and is not the durable turn source.

## 2026-07-20 Canonical Membership And MCP Joined-Room Authorization

Durable ProductCore member records now use the Matrix membership enum consistently: the final joined value is only `membership=join`. `joined` remains valid only as an operation or API result `status`. PostgreSQL migration v76 canonicalizes legacy `joined` member rows to `join`, and all member persistence paths normalize future inputs before storage.

MCP room discovery and room-scoped tools now require the current owner or actual Matrix sender to have `membership=join`. Message history/send, room-member listing, channel-post listing, and channel-comment listing/creation reject pending, joining, left, unknown, or otherwise non-joined rooms. Member-list results expose only current `membership=join` members. The configured Agent room is no longer trusted by identity alone; its relevant sender membership is verified from Matrix room state.

## 2026-07-17 Central Version Direct Upgrade Contract

`release.v2.status` and `release.v2.apply` are owner-token, HTTP-only ProductCore actions. They are not valid realtime `client.request` actions and `agent_token` is rejected. `release.v2.status` accepts no parameters and returns the local running `current_version`, current portal-device `client_version`, `available`, `updater_available`, `updater_ready`, `desired_state`, a token-free optional `active_job`, and sanitized `watchdog` status. It never performs GitHub release discovery, returns a release plan, or exposes an image, digest, command, path, plan token, or job bearer.

`release.v2.apply` accepts exactly `target_version`, lowercase canonical UUID `idempotency_key`, and `confirm="apply_release_change"`. `target_version` is canonical stable `vX.Y.Z`; image, digest, URL, plan token, shell, Compose, service, and all unknown fields are rejected. Before creating a job, the server queries the fixed central `appId=1&channelId=server` record, requires HTTP success plus business `code=0`, `appId="1"`, `channelId="server"`, canonical `version`/`preVersion`, an exact target match, and current reported client version at least `preVersion`. The message server then sends only those three safe fields to the updater's Unix control interface. Failed central validation returns `central_version_invalid`; a temporary central failure returns `central_version_unavailable`; incompatible client and changed-target failures are structured and create no updater job.

V1 release actions remain registered for existing clients, but central direct upgrades use V2 and do not depend on GitHub discovery or rollback operations.

## 2026-07-17 Group And Channel Member Ordering

When the durable conversation projection contains the room creator's Matrix ID, `groups.members` and `channels.members` now place only that exact full MXID first and return only that member with `role: "owner"`. Other personal-node identities such as `@owner:another.example` remain ordinary members even though they share the `owner` localpart.

Remaining members are ordered by ascending persisted `joined_at`, with missing/zero timestamps last and the full Matrix `user_id` as the deterministic tie-breaker. Legacy or not-yet-hydrated rooms without a projected creator use that join order for the whole list rather than trusting a stored owner role. Clients should preserve the returned order and must not infer the room creator from the `@owner` localpart.

For ordering purposes, a member's persisted `joined_at` advances when Matrix membership transitions from a non-joined state to `join`, using the authoritative join event timestamp when projected. Repeated join events and member-profile updates preserve the established join time, so an earlier invite, pending request, or prior membership generation cannot masquerade as the current room-entry time.


The creator projection is assigned only from the authoritative `m.room.create` sender when that raw sender is already a validated full MXID. Binding an existing `room_id`, the current node identity, later room-profile event senders, and the non-authoritative legacy `content.creator` field do not infer or replace the creator. Member reads lazily resolve current create-event sender IDs through the roomserver, persist the resolved MXID, and clear stale projected creators when current create state cannot identify one; unresolved rooms use the join-order fallback.

Matrix product-write policy uses the same create-sender authority for the privileged owner role. A member-policy `role` value cannot promote a non-creator to owner or demote the confirmed creator; member-policy mute state remains authoritative.

The `dirextalk_room_members_list` MCP tool applies the same rule once, after merging product projections with Matrix room-state members: exact creator first, then ascending `joined_at`, missing timestamps last, and full MXID as the tie-breaker.

## 2026-07-16 Native Agent Room And Post References

Successful non-stream `agent.chat` responses and realtime Native Agent stream `done` payloads may now include additive `references[]`. The server derives these references only from full, successful built-in Dirextalk tool results produced during that run; it does not parse the model's final Markdown or accept third-party MCP/runtime output as a navigation contract.

Contact list/search and room search results produce deduplicated room references. A messages-list result produces one reference for its containing room and does not expose or target a message `event_id`. Channel-post list results produce one post reference per valid post. References preserve tool/result order and use either `{kind:"room",room_id,room_type?,title?,preview?}` or `{kind:"channel_post",room_id,channel_id,post_id,title?,preview?}`. `room_type`, when present, is normalized to `direct`, `group`, or `channel`.

`mcp.channel_posts.list` and the embedded `dirextalk_channel_posts_list` result envelope now include top-level `channel_id` alongside `room_id`, `name`, `posts`, and pagination fields. This is additive and lets clients open an exact channel post without inferring channel identity from content.

## 2026-07-13 Join And Decision Recovery Contract

`groups.join`, `groups.invite.reject`, `contacts.request`, `contacts.requests.accept/reject`, `channels.join`, and `channels.join_request.approve/reject` now accept an optional `operation_id`. Old clients may omit it; the server derives and durably stores a stable ID for the current request/invite generation. Internal cross-node `channels.public.join_request/join_result` carry the same durable generation as optional `request_id`. Successful HTTP results expose additive top-level `operation_id` and `current_room_id`; successful WS results expose them inside `server.response.result`. These fields do not move into or replace the existing ProductCore `operation` object.

HTTP and WS error envelopes preserve the existing `code` field and add the same value as `error_code`, plus optional `operation_id` and `current_room_id`. Stable recovery codes are `request_not_found` (404), `request_expired` (410), `matrix_join_unconfirmed`, `join_result_unconfirmed`, `matrix_join_failed`, `operation_id_invalid` (400), `operation_id_conflict` (409), and `operation_recovery_failed`. The last code means a committed/recoverable operation still needs projection or persistence reconciliation; it is not limited to one database write.

Matrix `m.room.member membership=join` remains the final joined fact. A repeated mutation checks Matrix membership and repairs ProductCore member/contact/conversation/channel projections. It never treats an `already joined` error string alone as success, and ordinary group/channel invite or channel approval retries never kick a joined member merely to rebuild the flow. Direct-room creation uses a hashed internal idempotency key so an operation-state write failure or restart does not create another room.

`groups.invite` and `channels.invite` accept optional `rebuild_generation` only for an explicit retained-room empty-state rebuild. It is 1-128 characters from `[A-Za-z0-9._:-]`, and an explicit rebuild targets exactly one user. Old clients omit it: when Matrix confirms that the target is already joined, the server returns the current joined member and performs no kick, replacement invite, or `rooms.reactivate` callback. With a valid generation, the owner first calls the target node's `rooms.reactivate`; that node checks its own Matrix joined fact before changing ProductCore state and returns the same `rebuild_generation` plus `needs_rebuild=false,status=joined` or `needs_rebuild=true,status=invite`. The owner may kick and re-invite only after an exact generation echo with `needs_rebuild=true`. A missing, malformed, stale, or mismatched confirmation never authorizes the kick. The generation is also the canonical durable operation identity, so response loss, restart, or a different caller-supplied `operation_id` cannot repeat the kick. While that generation is workflow-busy or in flight, replay returns `status=joining,error_code=operation_recovery_failed` with the canonical operation and room IDs; the retained room's old Matrix join cannot be misreported as completed rebuild success. Only a completed cached operation whose member generation and current Matrix fact still match may return success. This additive recovery parameter does not require a Flutter client change.

Contact decision results are:

- accept confirmed: `200 status=accepted`;
- accept dispatched but not locally confirmed: `200 status=joining,error_code=matrix_join_unconfirmed`; the durable `joining` contact remains in `sync.bootstrap.contacts` and `pending.friend_requests` and may be retried with the same room/peer;
- reject a pending request: `200 status=rejected`;
- reject while current state is accepted: `200 status=accepted`;
- reject while current state is joining: `200 status=joining,error_code=matrix_join_unconfirmed`;
- repeated reject: `200 status=rejected`.

`contacts.requests.accept` does not return `200 join_failed` in v1.0.3. Old `room_id` plus the stable `peer_mxid` resolves a replacement direct room and returns that authoritative room in both `room_id` and `current_room_id`.
If `room_id` is omitted, accept/reject may resolve an existing request by the stable `peer_mxid`; a missing request returns `404 request_not_found` and never creates a contact or conversation without a Matrix room.

Channel decision replays no longer map `joining`, `join_failed`, `join/joined`, or `reject/rejected` to a synthetic 404. Recovering approval may return `approved`, local `joining + matrix_join_unconfirmed`, callback `joining + join_result_unconfirmed`, `joined`, or `join_failed + matrix_join_failed`; approving a current rejected generation returns `200 status=rejected`. Reject returns the current terminal/recoverable state: rejected, approved, joining, join_failed, or joined. If Matrix is joined and the joined member fact can be persisted but a downstream projection refresh fails, the result remains `status=joined` with `error_code=operation_recovery_failed` so clients can honor the Matrix fact while continuing background reconciliation. A total persistence failure still returns a structured error rather than claiming a durable success. Only network/5xx callback ambiguity becomes `join_result_unconfirmed`; stable remote 4xx errors remain terminal error envelopes.
The requester callback base URL is generation-scoped: a new application generation may replace the previous terminal generation's address, while an active-generation replay cannot redirect its callback. Approval and rejection always use the address persisted with that generation; a request parameter is only a compatibility fallback for legacy records with no stored address. This prevents node-address changes from stranding approval while keeping same-generation replay or a stale decision payload from hijacking the callback target.

Operation execution uses a durable database claim and revision CAS so two server instances sharing one node database cannot perform the same Matrix mutation concurrently. The operation also stores the request generation observed before a transition, avoiding process-clock ordering when a failed generation races a newer one. Member callback settlement uses the persisted `request_id` as a generation CAS: a delayed generation A callback may report generation B's current state, but cannot write over it. The requester applies the owner node's join-request response with the same generation-and-membership CAS, so a delayed `pending` or `approved` response cannot downgrade a callback or Matrix-confirmed `join`. Legacy clients that omit IDs receive a deterministic next generation after a rejected public-channel application or deleted contact; retries reuse it until persistence succeeds.

Unauthenticated `channels.public.join_request/join_result` validate their Matrix target, user and bounded request identifier before durable execution, including cached-operation replays. Their stored `operation_id` is always canonicalized from the server-owned request generation: an active member reuses its persisted `request_id`, while an initial or terminal application derives the deterministic next generation from the room, channel projection identity, user and current persisted request. Caller-supplied `operation_id` and `request_id` are validation-only inputs and never select the durable key, so concurrent callers cannot create parallel operation rows for one generation. Error envelopes likewise expose the local durable operation ID rather than trusting a nested or remote value.

## 2026-07-09 PostgreSQL-Only Storage And Postman Deprecation

Server storage is now PostgreSQL-only. SQLite/file database connection strings are rejected during configuration or storage initialization, and the monolith no longer falls back to in-memory P2P product state when the persistent store cannot open. Product read models, Matrix component storage, portal/runtime state, and local tests must use PostgreSQL-backed stores.

Postman collections are no longer maintained as contract artifacts. Current action metadata remains generated into `docs/product-action-contract.json`; contract-critical changes must update that artifact, current docs, focused tests, and project-local skills instead of Postman examples.

## 2026-07-09 Native Agent Tool Confirmation Rollback

The request-scoped `dangerous_tools_confirm` gate is deprecated and no longer controls Native Agent tool exposure. `agent.chat` and realtime `client.native_agent_stream` may expose all configured model-callable tools, including write tools, `native_agent_skills_*` mutation tools, `native_agent_mcp_servers_*` mutation tools, external MCP server tools, installed runtime CLI tools, and the built-in `runtime__shell` tool.

Clients must not send `dangerous_tools_confirm` for Native Agent chat/stream calls. The server's built-in Native Agent system prompt now treats shell, runtime CLI, skill/MCP mutation tools, external MCP tools, message sends, and channel comment writes as high-risk capabilities and instructs the Agent to warn the user and summarize the exact action/result, but this warning is not an authorization gate.

OpenAI-compatible model calls now forward non-empty `params.model_profile.reasoning_mode` as `reasoning_effort`. Empty, `none`, and `off` values are omitted so provider defaults apply.

Native Agent subprocesses no longer inherit the full message-server process environment. Runtime CLI/shell commands receive a reduced runtime environment with runtime `PATH` plus minimal OS execution variables. Stdio MCP servers receive the same reduced environment plus explicitly configured MCP server `env`. Model provider API keys and server credentials must not be inherited into those child processes.

Native Agent skill URL installation now fetches only HTTPS public hosts, rejects localhost/private/link-local targets, and does not follow redirects. Installing a skill from inline `content` is unchanged.

Realtime `client.native_agent_stream` now validates the requested stream action against `p2p/serviceapi.ActionSpecs` before entering the Native Agent runner. Non-stream or non-Agent stream actions are rejected at the WS boundary instead of relying only on the downstream runtime allowlist.

## 2026-07-09 MCP Body-Action Compatibility Removal

MCP-D is complete for the fixed Dirextalk body-action wrapper surface. The old fixed `mcp.*` actions are removed from `/_p2p/query`, `/_p2p/command`, the product action registry, and `serviceapi.AgentAction`.

`agent_token` is now accepted only for product body-action `agent.matrix_session.create` and the standard `POST /mcp` endpoint. Owner `access_token` also cannot call fixed `mcp.*` body actions through `/_p2p/query` or `/_p2p/command`; those requests are `400 unknown action`.

External MCP clients must call `POST /mcp` using MCP Streamable HTTP JSON-RPC. Native Agent built-in Dirextalk tools and `POST /mcp` continue to share the same `internal/dirextalkmcp` registry, schemas, pagination, room authorization, DTOs, errors, and p2p adapter invocation. Remaining `mcp.*` strings are internal capability action IDs, not public product body actions.

## 2026-07-08 Standard Dirextalk MCP HTTP Endpoint

External standard MCP clients can now call `POST /mcp` using MCP Streamable HTTP JSON-RPC instead of the Dirextalk `{ "action": "...", "params": ... }` body-action envelope. The first supported lifecycle is `initialize`, `tools/list`, and `tools/call`.

The endpoint requires `Authorization: Bearer <agent_token>`. Owner `access_token` is intentionally rejected on this endpoint, access tokens and agent tokens are not accepted in query strings, and the inbound bearer token is not forwarded into downstream Dirextalk MCP capability calls. The endpoint validates `Origin`; HTTP GET/SSE returns `405` while server-to-client streaming is not used.

`tools/list` is generated from the shared `internal/dirextalkmcp` registry. `tools/call` maps MCP tool names such as `dirextalk_messages_list` to the same unified capability service used by Native Agent built-in Dirextalk tools. Existing MCP rules still apply: `mcp_blocked_room_ids` hides/rejects blocked rooms, ordinary message history remains Matrix Client-Server backed, sends go through `p2p.Transport`, channel posts/comments remain separate from channel chat, pagination uses `from_time`/`to_time`/`cursor`, and old `from_ts`/`to_ts`/`ts`/`last_ts` fields remain unsupported.

## 2026-07-08 Native Agent Backend Contract

Native Agent is now exposed through first-class owner `agent.*` product actions instead of the legacy Agent plugin invoke envelope. The direct action surface includes `agent.chat`, `agent.models.list`, `agent.runtime.inspect/install/which/run`, `agent.skills.list/install/enable/disable/uninstall/registry.search`, `agent.mcp.servers.list/install/enable/disable/uninstall`, `agent.mcp.registry.search`, `agent.context.compress`, `agent.config.propose_patch`, reserved knowledge actions, and built-in Dirextalk tool actions such as `agent.contacts.list`, `agent.rooms.search`, `agent.messages.list/send`, and channel post/comment actions. These are owner-token actions; `agent_token` remains limited to product body-action `agent.matrix_session.create` and `POST /mcp`.

Native Agent streaming moved to dedicated realtime frames:

```json
{
  "type": "client.native_agent_stream",
  "id": "native-agent-stream-1",
  "action": "agent.chat",
  "params": {
    "prompt": "Summarize this conversation"
  }
}
```

The server maps `agent.chat` to the native runtime stream action `agent.chat.stream` and emits `server.native_agent_stream.event` frames for `delta`, `trace`, and `done`, `server.native_agent_stream.error` on failure, and `server.native_agent_stream.cancelled` after `client.native_agent_stream.cancel`. OpenAI-compatible reasoning streams may include explicit `reasoning_content` in `delta.data` and the final `done.data`; clients may display that provider-returned reasoning text, but must not synthesize hidden chain-of-thought. This stream is not the real Matrix `agent_room_id`; Online Agent bridge messages still use Matrix Client-Server sync/send/edit and are not mirrored through Native Agent stream frames.

`io.dirextalk.agent` is removed from plugin management. `plugins.catalog.list` and `plugins.installed.list` do not return it, and `plugins.install/enable/disable/uninstall/config/health/logs/invoke` reject it as not found or unsupported. Non-Agent plugin management is deprecated/inactive for the current product surface; retained `plugins.*` compatibility actions are for future reactivation and should not be used as current client acceptance scope.

Native Agent runtime config uses native portal Agent config storage rather than the hidden legacy Agent plugin record. On startup, any old `io.dirextalk.agent` plugin config is imported into native Agent config once in a sanitized, idempotent way. Root `api_key`/`api_key_ref` fields and model profile API key/ref fields are stripped during migration and native config save/load; model provider API keys remain request-scoped only.

## 2026-07-08 Native Agent Runtime Shell Tool

Native Agent `agent.chat` introduced a built-in `runtime__shell` Eino tool. The tool accepts `command`/`cmd` and optional `timeout_seconds`, runs inside the message-server container's Native Agent runtime directory, and returns the same observable `ok`, `stdout`, `stderr`, and `exit_code` shape as other runtime command execution. It may be model-callable whenever runtime shell is enabled; high-risk operation warnings are handled by the built-in Native Agent prompt rather than a request confirmation field.

Operators may disable the chat shell tool with Agent config `runtime_shell_enabled=false`. The final Docker runtime image now installs `bash` in addition to `/bin/sh`, so bash-based deployment/runtime scripts can run in the container when those scripts are present in the Agent runtime environment.

Native Agent ReAct execution now defaults to a 48-tool-call / 100-graph-step budget and accepts `max_tool_calls` or `max_steps` in Agent config or request params. This lets deployment-style shell and multi-skill install workflows complete multiple command/tool rounds without `[GraphRunError] exceeds max steps`; explicit `max_steps` is capped at 240 server-side to prevent unbounded loops.

## 2026-07-08 Native Agent Dialogue Management Tools

Native Agent `agent.chat` can now expose owner-scoped management tools to the model for explicit user requests to install, list, enable, disable, or uninstall native skills and MCP servers. The tool names are `native_agent_skills_*` and `native_agent_mcp_servers_*`; they call the same native runtime handlers as first-class `agent.skills.*` and `agent.mcp.*` actions.

Skills installed from dialogue are cached as static `SKILL.md` content and do not execute remote skill scripts. A newly installed skill affects the next Agent turn after the system prompt is rebuilt. MCP servers installed from dialogue may discover tools immediately, but those tools become callable on the next Agent turn after the Eino tool list is rebuilt.

Native Agent chat now prepends built-in Dirextalk product rules before any configured or request-scoped system prompt. These rules tell the model to prefer first-class Native Agent management tools over shell commands, to translate `npx skills add <repo> --skill <name>` examples into `native_agent_skills_install` calls, and to keep install/deploy workflows step-efficient. `agent.skills.install` also accepts GitHub owner/repo shorthand and, when given `repo_url` plus `name` or `id` without an explicit path, tries common skills monorepo locations before root `SKILL.md`.

## 2026-07-08 Native Agent Observable Trace

Native Agent `agent.chat` responses now include `steps` and `trace` fields. `steps` is a compact list of observable execution steps such as context loading, tool calls, tool results, assistant messages, and final output previews. `trace` wraps those steps with framework metadata, context usage, tool calls, and the final answer text.

Native Agent WS `client.native_agent_stream` for `agent.chat` emits a `server.native_agent_stream.event` with `event="trace"` before the final `done` event. The `done` payload also includes `steps` and `trace` alongside the existing `text` and `tool_calls` fields.

The trace is an auditable progress/tool/result display surface for clients. It must not be treated as hidden model chain-of-thought; the payload includes a disclaimer and only exposes observable runtime state and model/tool outputs.

## 2026-07-07 Native Agent Runtime

`io.dirextalk.agent` was temporarily represented as an embedded native message-server runtime in the plugin catalog. This plugin-surface contract is superseded by the 2026-07-08 Native Agent Backend Contract: Native Agent now uses first-class `agent.*` actions and dedicated native stream frames, and is not returned or managed as a plugin. Ops and future non-Agent plugins still depend on the Docker runner.


Model calls resolve the owner default conversation profile for chat, stream, and compression when model fields are omitted. Legacy inline `model_profile` remains compatibility-only; model API keys are server-owned encrypted credentials. `agent.models.list` remains an explicit request-scoped lookup and does not persist profiles.
Native Agent chat responses and `agent.chat.stream` done payloads include `native=true` and `framework="eino"` so clients and smoke tests can verify the embedded Eino runtime path is active.

Native Agent now owns dynamic skills, third-party MCP clients, runtime CLI tools, orchestration loops, server-side conversation memory, context compression, and built-in Dirextalk tools. Eino ReAct is the single orchestration path; model providers use maintained Eino OpenAI and DeepSeek components, Anthropic is direct API only through an Eino `ToolCallingChatModel` adapter, third-party MCP uses Eino official MCP, and installed runtime CLI tools are exposed as Eino tools for in-loop execution and summarization. Built-in tools proxy contacts, rooms, ordinary messages, room members, channel posts/comments, summaries, and message/comment writes through existing P2P/Matrix boundaries. Homeserver/sync DB reads are read-only; Matrix writes continue through `p2p.Transport`/roomserver.

## 2026-07-08 MCP Unified Channel Time Pagination

MCP read actions now use readable UTC RFC3339/RFC3339Nano timestamps and stable snapshot cursors. `mcp.messages.list`, `mcp.channel_posts.list`, and `mcp.channel_comments.list` accept `from_time`, `to_time`, `cursor`, and `limit`; legacy `from_ts` and `to_ts` are rejected with `400`.

The default order is newest first. Cursor pages keep the first-page snapshot fixed, so posts, comments, or messages inserted after the first page do not appear in that cursor chain. Clients must start a fresh query without `cursor` to fetch newer content.

MCP responses no longer return `ts` or `last_ts`. Message, post, comment, send, and comment-create summaries return `created_at`; room summaries return `last_message_at`; member summaries return string `joined_at`.

`mcp.channel_posts.list` post summaries now include `comment_count`, `like_count`, `favorite_count`, and `favorited_by_me`. `channels.posts.list` exposes the same favorite fields. Channel favorites use the existing Matrix `m.reaction` projection with `reaction="favorite"`: `favorite_count` is the distinct active-user count and `favorited_by_me` is request-owner-specific, so both converge through normal room refresh/backfill. Clients toggle this state through `channels.post_reaction.toggle` with `reaction="favorite"`; legacy `favorites.*` remains the owner-local saved-message surface. Channel ordinary chat remains separate and is read through `mcp.messages.list`, which continues to filter out product `p2p_kind` post/comment events.

The embedded Native Agent Dirextalk tools use the same contract: `dirextalk_messages_list`, `dirextalk_channel_posts_list`, and `dirextalk_channel_comments_list` expose `from_time`, `to_time`, `cursor`, and `limit` instead of legacy millisecond timestamp fields.

## 2026-07-05 Official Ops Plugin

Added official catalog plugin `io.dirextalk.ops` for single-node private deployment operations. It uses `docker.io/dirextalk/ops-plugin:latest` and exposes owner-invoked plugin actions through existing `plugins.invoke`:

- `ops.status.get`
- `ops.containers.list`
- `ops.logs.tail`
- `ops.backups.list`
- `ops.backup.create`
- `ops.backup.status`
- `ops.backup.download_chunk`
- `ops.backup.delete`
- `ops.cleanup.plan`
- `ops.cleanup.run`
- `ops.rooms.cleanup.plan`
- `ops.rooms.cleanup.run`
- `ops.media.orphans.plan`
- `ops.migration.export`
- `ops.restore.plan`
- `ops.restore.run`

The server treats Ops as the only official plugin allowed to receive privileged Docker runner mounts. When enabled, Ops receives the Docker socket mount and a dedicated backup volume, plus `OPS_BACKUP_ROOT`, `OPS_MAX_BACKUPS`, `OPS_MESSAGE_SERVER_CONTAINER`, `OPS_POSTGRES_CONTAINER`, `OPS_POSTGRES_USER`, and `OPS_POSTGRES_PASSWORD`. Ops does not receive owner access token or `DIREXTALK_AGENT_TOKEN`. Non-Ops plugins are rejected if they request privileged mounts.

Backup creation can run asynchronously and expose progress through `ops.backup.status`; backup files are downloaded through `ops.backup.download_chunk`. `ops.restore.run` requires `confirm="restore_backup"` and restores the Postgres dump from a selected backup package. Cleanup contracts are intentionally plan-first. `ops.cleanup.plan`, `ops.rooms.cleanup.plan`, and `ops.media.orphans.plan` estimate impact before execution. `chat_purge_physical` and direct SQL deletion of Matrix event tables are not part of the first-version Ops plugin; room history cleanup is limited to cache cleanup, local hiding/archive planning, and backend-controlled safe actions.

## 2026-07-04 Official Plugin Manager And Agent MCP Boundary

Agent-specific Docker/container details in this section were superseded by the 2026-07-07 Native Agent runtime. Non-Agent Docker plugin manager details still apply.

Added protected owner-only plugin management actions on the existing body-action surface:

- `plugins.catalog.list`
- `plugins.installed.list`
- `plugins.install`
- `plugins.enable`
- `plugins.disable`
- `plugins.uninstall`
- `plugins.config.get`
- `plugins.config.update`
- `plugins.job.get`
- `plugins.health`
- `plugins.logs.tail`
- `plugins.invoke`
- `plugins.invoke.stream`

These actions require owner `access_token`. `agent_token` cannot call plugin management or plugin invoke actions. `plugins.catalog.list` returns an empty `plugins` list when the Docker plugin runner is not enabled, so clients should hide plugin entry points until catalog entries are available. Agent-specific plugin catalog/config/invoke details in this historical section are superseded by the 2026-07-08 Native Agent Backend Contract. Current plugin install/enable/disable/uninstall jobs are for non-Agent official plugins such as `io.dirextalk.ops`, and must use official catalog metadata whose Docker image belongs to the official `dirextalk` Docker Hub organization. Digest metadata is optional and is not required for first-version installs.

Native Agent action details now live on the first-class `agent.*` product action surface. `agent.models.list` uses the request-scoped `provider`, `base_url`, and `api_key` to fetch the real model list from supported providers and returns provider-reported `models[]` entries such as `id`, `name`, and any raw capability fields the provider actually supplies, for example `context_length`, `max_output_tokens`, `temperature`, `top_p`, `reasoning_modes`, or `reasoning_effort_options`; it must not persist or echo API keys, and must not invent model defaults or capabilities. Clients should render optional model parameters from returned metadata when present, keep missing values unset, allow manual model IDs when listing is unavailable, and omit unset tuning parameters from chat requests so provider defaults apply. `agent.runtime.inspect` resolves request-scoped model settings without returning API keys and reports runtime status/tool counts for configured third-party MCP servers and CLI tools; model calls can also use read-only `native_agent_runtime_inspect` without dangerous-tools confirmation. `agent.runtime.install` installs allowed runtime CLI/package-manager capabilities, such as `agent-reach`, without expanding `agent_token` permissions. When durable knowledge storage is ready, Eino exposes exactly `native_agent_memory_remember` and `native_agent_memory_search` (aliases `memory_remember`/`memory_search` and `remember`/`recall`); owner and deterministic idempotency are server-derived. Explicit remember phrases are required for writes, recall phrases require a search before answering, and ordinary conversation is never silently stored. The Native Agent owns standard MCP client orchestration and ships package-manager launch support for third-party MCP servers installed from registry metadata (`npx` for npm packages and `uvx` for Python packages), while the message-server exposes the standard `POST /mcp` endpoint to `agent_token`.

`plugins.invoke` calls an enabled official non-Agent plugin over the first-version Docker HTTP runner and returns `{ "plugin_id", "action", "result" }`. `plugins.invoke.stream` remains registered only to return `400 action requires websocket`; Native Agent streaming uses `client.native_agent_stream`, not the legacy Agent plugin stream frame. `client.request` remains unavailable for `plugins.*`.

The backend remains the Dirextalk capability boundary for Agent/MCP access: Native Agent built-in tools and the standard `POST /mcp` endpoint share owner-scoped access control, Matrix transport writes, product projections, and `mcp_blocked_room_ids` enforcement through `internal/dirextalkmcp`. Contact list/search capabilities expose accepted direct contacts to local Agent tooling without requiring a room search fallback. Native Agent skills, model/provider request handling, MCP client wiring, and orchestration are embedded in message-server behind owner `agent.*` actions. External standard MCP clients should use `POST /mcp` JSON-RPC instead of the Dirextalk action envelope.

## 2026-07-03 Unified Channel Post+Chat

Channels are now a unified post+chat surface in one Matrix room. `channels.create` defaults missing or invalid `channel_type` to `post`, but current server behavior does not branch on legacy `chat` vs `post` values. New channel rooms, including existing-room channel bindings, write `m.room.history_visibility=shared`. Joined channel conversations expose post/comment/reaction capabilities according to room role and comments settings instead of `channel_type`.

Product post/comment/reaction events remain identified by Matrix content metadata such as `p2p_kind`; ordinary channel chat messages stay as Matrix timeline events and do not update post/comment/reaction projections. Conversation activity for channels is updated by ordinary chat messages, while post/comment projection events do not pollute ordinary conversation activity.

HTTP Push Gateway delivery is suppressed for all channel room events. This is Push Gateway suppression only; Matrix sync, room timelines, unread state, read markers, and local client navigation still operate normally.

## 2026-07-02 Owner Blocks

Added protected owner actions `blocks.add`, `blocks.list`, and `blocks.remove` for the user contact blacklist. `blocks.add` accepts `target_type: "contact"` with `peer_mxid`/`mxid`; group, channel, and room targets are not part of the current product contract. Each block stores a `display_name` and `avatar_url` display snapshot; when omitted, the server fills it from existing contact metadata or falls back to the target ID. `blocks.list` returns a `contacts` array for the user settings blacklist. `blocks.remove` cancels a contact block using the same identifiers. A stored contact block filters new messages in the matching direct room without changing Matrix membership or deleting history; removing the block restores normal sending.

When an owner tries to send a friend request to an already blocked contact, the action fails before Matrix writes with:

```json
{
  "error": "already blocked"
}
```

The HTTP/WS response status is `403`. These actions require owner `access_token`; they are not public actions and are not available to `agent_token`.

Inbound Matrix direct invites from blocked contacts are ignored by projection and do not appear as pending friend requests.

## 2026-07-01 Agent Config Avatar And MCP Room Blacklist

`agent.config.get` and `agent.config.update` now include two owner-managed fields:

```json
{
  "avatar_url": "mxc://example/agent",
  "mcp_blocked_room_ids": ["!room:example.com"]
}
```

`avatar_url` is a display-only Agent profile setting for clients. `mcp_blocked_room_ids` is a durable room blacklist under Agent config. `agent.config.update` replaces the blacklist with the supplied normalized list; omitted fields keep their previous values.

At that point, fixed MCP actions remained HTTP-only and owner-scoped. Current MCP clients use `POST /mcp`, which enforces the same room blacklist rules. Rooms in `mcp_blocked_room_ids` are not returned by MCP room search; direct MCP access to blocked rooms through ordinary message send/list, member list, channel post list, channel comment list, or channel comment creation is rejected with `403 room is blocked for MCP`.

## 2026-06-30 Owner HTTP Fallback For Product Actions

Logged-in client product actions now use ready-WS first instead of WS-only. Clients should use owner `GET /_p2p/ws` `client.request` only after the realtime transport has received `server.ready`. When WS is not ready or disconnected at click time, clients should send the same body-action envelope to `POST /_p2p/query` or `POST /_p2p/command` with `Authorization: Bearer <access_token>` immediately and let realtime WS reconnect in the background. Transport failure before a response may also use owner HTTP fallback for safe repeated actions.

Business errors returned by WS, such as permission or validation failures, should not be retried over HTTP. Clients should also de-duplicate identical in-flight user actions such as `contacts.requests.accept` or `groups.join` so duplicate taps do not send duplicate mutations or show duplicate success UI. If a WS request was already sent and the response is lost, clients should only HTTP-fallback actions that are safe to repeat, such as contact decisions, joins, read markers, and product queries.

`agent_token` permissions did not change for product body actions in this pass: it remained limited to `agent.matrix_session.create` and fixed `mcp.*` HTTP actions at that point. This was superseded by the 2026-07-09 MCP-D removal: current servers accept `agent_token` for product body-action `agent.matrix_session.create` and standard `POST /mcp`; fixed `mcp.*` body actions are unknown.

Realtime WS owner tickets now advertise `expires_in_ms: 120000` to tolerate mobile weak-network upgrade delays. A failed HTTP request to `GET /_p2p/ws?ticket=...` that never completes the WebSocket upgrade no longer consumes the ticket; accepted WebSocket upgrades remain single-use.

## 2026-06-30 Retained Room Reactivation For Rebuilt Members

Added internal public action `rooms.reactivate` for node-to-node recovery when a group or private-channel member node has been rebuilt and lost local product/Matrix projections while the owner node still retains the member in the Matrix room. It is not a normal client workflow entry.

The original implementation removed an already-joined membership before asking the target whether recovery was needed. That unsafe ordering is superseded by the 2026-07-13 contract above: ordinary repeats preserve joined state, and an explicit `rebuild_generation` must be confirmed by the target before kick plus replacement invite. After a confirmed rebuild, the target records an invite/pending room card only; it does not silently join. The user must still confirm by calling `groups.join` or `channels.join`, and joined state is recorded only after Matrix join succeeds. Public channels continue to recover through `channels.public.join_request` and the normal open/approval flow.

For rebuilt direct-contact nodes, `contacts.request` still first asks the retained peer to re-invite the old accepted direct room. If the retained room cannot be rejoined because the rebuilt node lost its old Matrix room/key state, including a missing local room version after database loss, the requester creates a replacement direct request room. The retained peer accepts that replacement only from the real Matrix invite sender and preserves local contact remarks; old direct-room history is not copied into the replacement.

## 2026-06-30 Contact Re-Request Replacement Room

When both sides of a direct contact have left the retained old direct room, or the peer node no longer retains the old relationship, a new `contacts.request` creates a replacement direct request room instead of binding the pending request to the old room. The returned contact `room_id` may therefore differ from the previous direct room. Clients should use the latest `room_id` from `contacts.request`, `contacts.list`, `sync.bootstrap.contacts`, or contact mutation responses when accepting or opening the conversation.

For historical pending requests that still point at an unrejoinable old direct room, `contacts.requests.accept` falls back to creating a replacement direct room and returns the accepted contact with the new `room_id`.

## 2026-06-30 Contact Display Name Override

`contacts.update` now marks the supplied `display_name` as a local contact remark. Contact records returned from `contacts.update`, `contacts.list`, and `sync.bootstrap.contacts` may include `display_name_override: true` when the displayed name is owner-managed.

Remote Matrix member profile updates still refresh peer avatar metadata, but they no longer overwrite an accepted contact's `display_name` while `display_name_override` is true. Contacts without a local override keep the previous Matrix-native behavior and continue to follow the peer's latest Matrix member display name.

## 2026-06-30 Agent Bridge Transport Returns To Matrix

Agent bridge online display remains Matrix-native room state in the real `agent_room_id`: event type `io.dirextalk.agent.status`, state key `@agent:<server>`, and content field `online`. The running local bridge writes `online=true/false` through its `@agent:<server>` Matrix session. The server no longer treats `agent.config.enabled=true` or an agent-token WS session as online; startup/agents-room repair and `agent.config.update enabled=false` only publish `online=false` as a fallback.

`agent_token` no longer creates realtime WS tickets. `realtime.ws_ticket.create` is owner-token only; `agent_token` remains limited to product body-action `agent.matrix_session.create` and current `POST /mcp`.

`agent.matrix_session.create` remains a retained HTTP body action and may be called with either owner `access_token` or `agent_token`. It returns a Matrix Client-Server session for the local `@agent:<server>` bridge user so dirextalk-connect can bootstrap Matrix-native Agent room sync/send/edit without owner credentials. It must not be migrated into Product WS and must not evict owner devices.

Agent room messages, previews, edits, and final replies are transported through Matrix Client-Server APIs as `@agent:<server>`. `agent_room.message`, `client.agent_stream`, and `server.agent_stream` are no longer current protocol frames/events.

No response fields change: `sync.bootstrap` still returns only `agent_room_id` for Agent room discovery and does not return `agent_online`; `agent.status`/`agents.status` remain removed.

## 2026-06-30 MCP HTTP Boundary And WS Client State Flags

At this point, fixed MCP actions remained HTTP body actions on `POST /_p2p/query` or `POST /_p2p/command`; they were not migrated into WS `client.request`. That compatibility surface was removed on 2026-07-09. `agent.matrix_session.create` remains HTTP-only. If an owner or agent WS session sends a `client.request` for `agent.matrix_session.create`, the server returns:

```json
{
  "type": "server.response",
  "id": "req-1",
  "action": "agent.matrix_session.create",
  "ok": false,
  "status": 400,
  "error": "action requires http"
}
```

WS lifecycle and focus frames now accept extra client-state fields for future push decisions while preserving the existing `foreground` and `room_id` fields:

```json
{
  "type": "client.lifecycle",
  "foreground": false,
  "state": "hidden",
  "hidden": true,
  "flags": {
    "hidden": true,
    "background": true
  }
}
```

```json
{
  "type": "client.focus",
  "room_id": "!room:server",
  "focused": true,
  "flags": {
    "focused": true
  }
}
```

Push suppression requires a fresh foreground WS session that is not hidden and has the same focused room as the push candidate. Hidden/background/disconnected/expired/different-room state keeps normal push behavior.

## 2026-06-30 WS Product API Full Migration

Logged-in Dirextalk client/product actions now use `GET /_p2p/ws` request/response frames instead of HTTP body-action calls. HTTP `/query` and `/command` remain for portal bootstrap/auth/status/password, `agent.matrix_session.create`, `realtime.ws_ticket.create`, and node-to-node public/callback actions. Standard MCP clients use `POST /mcp`.

This WS-only HTTP rejection rule was superseded later on 2026-06-30 by the owner HTTP fallback contract above. Current clients are WS-first, not WS-only.

Client request frame:

```json
{
  "type": "client.request",
  "id": "req-1",
  "action": "contacts.list",
  "params": {}
}
```

Successful response frame:

```json
{
  "type": "server.response",
  "id": "req-1",
  "action": "contacts.list",
  "ok": true,
  "result": {}
}
```

Error response frame:

```json
{
  "type": "server.response",
  "id": "req-1",
  "action": "contacts.list",
  "ok": false,
  "status": 401,
  "error": "M_UNKNOWN_TOKEN"
}
```

`client.command` was retained only as a one-release compatibility alias and mapped to the same `server.response` path. That compatibility alias is now removed; clients must send `client.request`.

`GET /_p2p/events` is removed. The P2P outbox remains durable because WS `server.event` replay and cursor recovery still use it. Cursor retention gaps are reported only through WS `server.cursor_reset`; clients must recover by issuing `sync.bootstrap` over WS.

Owner WS sessions may call protected logged-in product actions except `realtime.ws_ticket.create` and `agent.matrix_session.create`. Agent-token callers cannot create WS sessions; Agent bridge bootstrap stays on HTTP body actions, and MCP clients use `POST /mcp`. Matrix Client-Server remains the protocol source for ordinary timeline, media, history, search, redaction, local delete, and Agent bridge room traffic.

HTTP `/query` and `/command` now return an explicit error for non-retained logged-in client actions:

```json
{
  "error": "action requires websocket"
}
```

## 2026-06-30 Transitional Realtime WS Commands And Agent Stream Frames

This transitional contract was superseded later the same day by the WS Product API full migration above. During the transition, `GET /_p2p/ws` accepted owner-session `client.command` frames for lightweight product commands. The initial allowlist was:

- `sync.read_marker`
- `channels.read_marker`

Frame shape:

```json
{
  "type": "client.command",
  "id": "cmd-1",
  "action": "sync.read_marker",
  "params": {
    "room_id": "!room:server",
    "event_id": "$event",
    "origin_server_ts": 1710000000000
  }
}
```

Successful commands returned `server.command_result` with `id`, `action`, and `result`. Validation, auth, and action errors returned `server.command_error` with `id`, `status`, and `error`. Current servers reject `client.command` with `400 unsupported frame type`; clients must use `client.request`. Agent-token WS sessions cannot call owner commands.

This transitional agent stream contract was removed later the same day. Current Agent bridge previews and final replies use Matrix Client-Server messages/edits from `@agent:<server>`; current clients must not emit agent stream WS frames and current servers must not expose Agent bridge traffic on Product WS.

## 2026-06-29 WebSocket Realtime Sync

Added protected body action `realtime.ws_ticket.create`, normally sent to `POST /_p2p/query` with an empty `params` object:

```json
{
  "action": "realtime.ws_ticket.create",
  "params": {}
}
```

The action accepts owner `access_token` only. It returns:

```json
{
  "ticket": "ws_ticket_...",
  "expires_in_ms": 120000
}
```

The ticket is server-local, short-lived, and single-use. It is consumed only after `GET /_p2p/ws?ticket=<ticket>` completes WebSocket upgrade. The WS route does not accept bearer tokens directly.

The first client text frame must be `client.hello` with optional `since`, `client`, and `platform` fields. Subsequent client frames are:

- `client.lifecycle`: `{ "foreground": true|false }`
- `client.focus`: `{ "room_id": "!room:server" }`, or empty `room_id` to clear focus
- `client.ack`: `{ "seq": 123 }`
- `client.ping`

Server frames are:

- `server.ready`
- `server.event` with the existing P2P event payload in `event`
- `server.cursor_reset` with the same recovery payload shape as the SSE `p2p.cursor_reset` event
- `server.pong`
- `server.error`

Owner WS sessions receive the normal product event stream. The initial implementation also allowed agent-token WS/SSE streams for `agent_room.message`; that path was later removed in favor of Matrix Client-Server bridge sync/send/edit.

Push suppression now prefers fresh WS session state. A connected foreground WS session suppresses unread notification insertion and HTTP push gateway delivery only when its focused room matches the room that produced the push candidate. Background, disconnected, expired, no-focus, or different-room state keeps normal background push behavior. The server timestamps and expires WS session state with server time; clients do not send expiry timestamps.

## 2026-06-29 Matrix Account-Data Foreground Fallback And Agent Room Defaults

Dirextalk clients that have not established a fresh WS session may still suppress foreground system pushes by writing global Matrix account data type `io.dirextalk.push.context` through the existing Matrix account data route. The expected body is:

```json
{
  "foreground": true
}
```

The Matrix account data write path stamps foreground writes with a server-clock 60-second expiry. While the stamped foreground state is fresh and no fresh WS session exists for the user, the userapi roomserver consumer does not create an unread notification row and does not call the HTTP push gateway for matching Matrix push-rule notifications. Missing, malformed, expired, or `foreground=false` context fails open and keeps normal background push behavior. This is a migration fallback only; the server does not infer foreground/background from `/sync`, read receipts, or pusher registration.

Clients should prefer WS `client.lifecycle` and `client.focus`. During migration, clients may continue refreshing this account data every 30 seconds with `{"foreground": true}` and write `{"foreground": false}` when entering background; if that write is missed, the previous foreground state naturally expires after the server-stamped 60 seconds.

Backend startup now also ensures the portal owner has a room-level Matrix push rule for the real `agent_room_id` with empty actions, so new or repaired agents rooms default to no system push. Existing explicit room push rules for the same room are preserved.

## 2026-07-07 Owner Report Notifications

Reintroduced `reports.submit` as a public ProductCore action for owner-directed
group/channel reports only. Friend reports and official report submissions stay
on signed imadmin public APIs. The owner node validates the target
group/channel, persists a `p2p_reports` row, and sends a Matrix `m.notice` into
the durable `system_room_id` with `msg_type=report`,
`p2p_kind=system_report`, reporter metadata, target metadata, reason/body, and
Matrix media `image_urls`. Portal auth and `sync.bootstrap` now return
`system_room_id`. The system room is intentionally not given the agent room's
empty-action push rule because owner report cards should notify.

## 2026-06-29 P2P Reports Submit Removed

Removed `reports.submit` from the message-server P2P action surface. User-facing report submission remains on the signed imadmin public API, so this server no longer registers the P2P report action or persists P2P report rows.

## 2026-06-29 P2P Event Cursor Reset Signal

`GET /_p2p/events` now detects a non-zero `since` cursor that is older than the retained `p2p_events` window. The stream stays HTTP 200 and replays retained events, but it first emits an SSE control event `event: p2p.cursor_reset` without advancing the SSE event id.

The control payload contains `type`, `since`, `min_seq`, `max_seq`, `count`, and `recovery: "bootstrap_required"`. The response also sets `X-Dirextalk-P2P-Events-Cursor-Reset: true`, `X-Dirextalk-P2P-Events-Min-Seq`, `X-Dirextalk-P2P-Events-Max-Seq`, and `X-Dirextalk-P2P-Events-Count` before streaming begins.

Clients should treat this as a product cache gap: clear local product projections, call `sync.bootstrap` once, persist the newest handled event `seq`, and then continue normal WS delta consumption. SSE fallback clients continue with `GET /_p2p/events?since=<seq>`.

## 2026-06-29 MCP Room Member Identities

Added protected MCP action `mcp.room_members.list` on `POST /_p2p/query`. Owner `access_token` and fixed MCP `agent_token` could call it at that point. This body-action endpoint was removed on 2026-07-09; current clients use the standard MCP tool over `POST /mcp`. The action accepts `room_id` or `channel_id`, optional `status`/`membership`, optional `role`, and optional `limit`; it returns `room_id`, `name`, `count`, and concise member identities with `user_id`, `user_mxid`, `localpart`, `domain`, `display_name`, `avatar_url`, `membership`, `role`, and `joined_at`.

`mcp.room_members.list` is owner-scoped and only reads known Dirextalk product rooms or conversations. It may enrich stale product projections from current Matrix `m.room.member` state and Matrix profile fallback data, but it rejects unknown room IDs instead of exposing arbitrary roomserver state through the MCP surface.

`mcp.messages.list` message summaries now expose sender identity fields: `sender_mxid`, `sender_display_name`, `sender_domain`, and `sender_localpart`. The legacy `sender` field is preserved and is upgraded to a readable display name when Matrix member/profile data is available.

`mcp.rooms.search` may use current Matrix member state to display fresher group/channel member counts when product read-model counts are stale.

## 2026-06-27 MCP Owner-Scoped Message History

MCP actions remain a fixed `agent_token` allowlist, but their product behavior is owner-scoped: room search, default ordinary message send, ordinary message list, channel post/comment list, and channel comment create operate from the portal owner view instead of exposing the local Agent Matrix account as an independent product user.

`mcp.messages.list` now reuses the current owner `access_token` for Matrix history reads. It does not call `agent.matrix_session.create`, does not create a `DIREXTALK_MATRIX_HISTORY` device, and does not refresh the portal owner's Matrix session, so MCP history reads cannot evict the owner's phone or browser session.

Default owner-scoped `mcp.messages.send` now rejects the configured `agent_room_id`. Agent-room replies remain supported only through the internal gateway marker path (`agent_gateway=true` or `gateway_source`), where the local `@agent:<server>` user sends the reply and marks the event to prevent gateway loops.

## 2026-06-26 Agent Matrix Session Identity

`agent.matrix_session.create` now creates and returns a Matrix Client-Server session for the local agent user `@agent:<server>` instead of the portal owner. The response fields remain `access_token`, `device_id`, `user_id`, and `homeserver`; `user_id` is now the local agent MXID. Current servers accept either owner `access_token` or `agent_token` for this HTTP-only bootstrap action.

The helper still uses `revokeExistingDevices=false`, so creating a cc-connect or local gateway Matrix session does not evict the portal owner's phone or browser sessions.

## 2026-06-26 Agent Matrix Room State Status

Owner clients now receive Agent bridge online state from native Matrix room state in the real `agent_room_id`: event type `io.dirextalk.agent.status`, state key `@agent:<server>`, and content field `online`.

The server writes this state when creating or repairing the agents room and when `agent.config.update` changes `enabled`. This was later narrowed: the server only writes `online=false` fallbacks, while the running local bridge writes true/false through Matrix. `sync.bootstrap` still returns the real `agent_room_id` so clients can locate the room, but it no longer returns `agent_online` or any `agent_presence` mirror. `agent.status` and `agents.status` are removed.

Matrix `m.presence` is not part of the Agent online contract, and Dirextalk monolith startup no longer enables Matrix outbound presence for this path. New generated, sample, and Helm configs default both inbound and outbound presence to `false`.

## 2026-06-25 Agent Token Event Stream Access

`GET /_p2p/events` previously accepted bearer `agent_token` as well as owner `access_token` as a narrow passive gateway exception. This path was later removed with SSE and the Agent bridge returned to Matrix Client-Server transport.

Non-MCP protected body actions still reject `agent_token` except the HTTP-only `agent.matrix_session.create` bridge bootstrap action. The fixed MCP action allowlist mentioned in this historical entry was removed on 2026-07-09.

## 2026-06-25 Immutable Channel Type

`channels.update` now ignores `channel_type`. Channel type is creation-time metadata and cannot be changed after a channel exists. Requests that include `channel_type` continue to apply other mutable fields but leave the stored `channel_type` unchanged.

Clients may send `channel_type` only in `channels.create`; missing or invalid values now default to `post`. Since 2026-07-03, all new channel rooms get shared Matrix history visibility at creation or when binding an existing room as a channel, regardless of legacy `channel_type`.

## 2026-06-25 Agent Token And CLI Cleanup

Agent-token dynamic permission management is removed. `apis.list` and `apis.status` are no longer P2P actions and calls to those action names return `unknown action`.

Protected product actions require bearer `access_token`. At this point, `agent_token` was accepted only for `agent.matrix_session.create` and fixed MCP actions: `mcp.rooms.search`, `mcp.messages.send`, `mcp.messages.list`, `mcp.channel_posts.list`, `mcp.channel_comments.list`, and `mcp.channel_comments.create`. `GET /_p2p/events` was a route-level exception for passive gateway listening at the time and was later removed; other protected body actions reject `agent_token`. The later standard `POST /mcp` endpoint is recorded in the 2026-07-08 entry.

The first-party CLI module and its helper package are removed: `cmd/dirextalk-cli`, `internal/agentclient`, CLI build scripts, CLI agent-skill docs, and the project-local `dirextalk-cli` Codex skill.

## 2026-06-25 Matrix Push Gateway Metadata

Matrix event pushes sent to HTTP push gateways now include optional Dirextalk display/routing metadata when the room has Dirextalk product room state. Normal direct and group message pushes can include `notification.title`, `notification.push_type=message`, `notification.room_id`, `notification.event_id`, and short `notification.room_type` (`direct` or `group`). The gateway owns the visible body text and sets it to `Send you a new message`.

Channel rooms (`notification.room_type=channel`) are not sent to HTTP push gateways. Matrix `m.call.invite` events in Dirextalk rooms use `push_type=call` and add `call_id` plus `call_kind=voice`; product `calls.create` / `calls.incoming` actions remain P2P event/call-record flows unless represented as Matrix call invite events.

## 2026-06-24 Portal Single-Device Login

`portal.bootstrap`, `portal.auth`, and `portal.password` now create an exclusive Matrix device session for the portal owner. After the new session is created, the server deletes the owner's other Matrix devices while preserving the current `device_id`, so previous phones receive Matrix `M_UNKNOWN_TOKEN` on later authenticated requests and must ask the user to log in manually.

`agent.matrix_session.create` remains an internal Matrix session helper and does not evict the portal user's phone session. As of 2026-06-26, it returns a session for the local `@agent:<server>` user. Current servers accept either owner `access_token` or `agent_token` for this HTTP-only bridge bootstrap action.

## 2026-06-24 User Public Channel Lookup

`users.public_channels` now returns only public channels owned by the target user. Public channels where the target user is only a normal member are no longer included in the "user's channels" list.

`users.public_channels` also accepts optional `remote_node_base_url` and forwards the public query to that owner node, matching remote public channel discovery flows. The forwarded request strips `remote_node_base_url` before reaching the target node.

## 2026-06-24 Channel Room Projection Guard

Matrix room state is now treated as a channel projection source only when `io.dirextalk.room.profile.room_type` is explicitly `io.dirextalk.room.channel` and `channel_id` is an explicit product channel id. Empty profiles, group/direct room profiles, missing `channel_id`, and Matrix-room-id-shaped `channel_id` values are ignored by channel refresh logic.

`groups.join` no longer calls the channel room refresh path after Matrix join. Group member refresh still runs for the joined group, but it cannot create or update a `channels` read-model row. This prevents group chats with empty profile state from appearing in `channels.list` or `sync.bootstrap.channels`.

## 2026-06-24 Channel Reaction History Snapshots

`channels.my_reactions` still returns `{ "reactions": [...] }`, but each item is now a display history snapshot object instead of a bare reaction row. The item contains:

- `reaction`: the original reaction record with `target_type`, `target_id`, `channel_id`, `post_id`, `comment_id`, `reaction`, `user_id`, `active`, and `created_at`.
- `channel`: the current channel snapshot when available, including `name`, `avatar_url`, `channel_type`, `member_count`, and normal channel metadata.
- `post`: the parent post snapshot when available, enriched with comment/reaction counts and `reacted_by_me`.
- `comment`: the comment snapshot for comment reactions when available, enriched with reaction count and `reacted_by_me`.

Clients must not synthesize fake channel/post display data from a bare reaction row. If a snapshot is missing, show an unavailable or syncing state instead of fallback labels such as `频道`, `文字`, or `频道帖子`.

`channels.public.get`, `channels.public.search`, and `users.public_channels` refresh public channel `member_count`/`pending_join_count` from persisted ProductCore membership before returning a channel when membership rows are available. This keeps public detail and public list views aligned with the owner node's joined member facts.

## 2026-06-23 Realtime Call Lifecycle

`calls.event` now accepts `rejected` in addition to `connected`, `ended`, `missed`, and `failed`. Call records persist `answered_at`, `ended_at`, `ended_by_mxid`, `end_reason`, and `duration_ms` in `p2p_calls`, so call start/end timing survives restart.

Every `calls.create`, `calls.incoming`, and `calls.event` write appends a `call.changed` event to `GET /_p2p/events`. The event payload contains the current call record under `payload.call`, allowing clients to update active call UI immediately when the other party rejects or hangs up.

Terminal call states are not reopened by later stale `calls.create`, `calls.incoming`, or non-terminal `calls.event` writes with the same `call_id`. Clients that arrive late after `missed`, `ended`, `rejected`, or `failed` receive the terminal snapshot and must not join that call.

## 2026-06-23 Agents Room Gateway

This section records the original gateway behavior from June 23. Current behavior supersedes it: Agent bridge traffic no longer uses SSE/P2P outbox events and is transported through Matrix Client-Server sync/send/edit as `@agent:<server>`.

Backend startup now creates a real private Matrix agents room when the stored `agent_room_id` is empty or still uses the legacy pseudo form `!agent:<server>`. The real room id is persisted in portal state and written to the bootstrap credentials file as `agent_room_id`. The room contains the portal owner and the local agent user `@agent:<server>`; existing real agents rooms are repaired on startup by joining the local agent user if needed.

`portal.bootstrap`, `portal.auth`, and `sync.bootstrap` expose the current real `agent_room_id` so app and gateway clients can restore the Agent conversation from either login/session metadata or first-screen metadata.

`GET /_p2p/events` can now emit `agent_room.message` for ordinary `m.room.message` events in the configured agents room only. Payload fields are `room_id`, `event_id`, `sender_mxid`, `body`, `msgtype`, and `origin_server_ts`. Ordinary messages in other non-product rooms still do not produce P2P events or P2P message records.

`mcp.messages.send` accepts internal optional gateway marker params, including `agent_gateway=true` and `gateway_source`. Marked replies are sent by the local agent user, written as Matrix messages with `io.dirextalk.agent_gateway` metadata, and are not re-emitted as inbound `agent_room.message` events, preventing gateway reply loops. `mcp.messages.list` returns the agents room name as `Agents` and displays messages from `@agent:<server>` using the configured agent `display_name`.

## 2026-06-23 Channel Join Request Approval Retry

`channels.join_request.approve` now treats an existing `join_failed` or `approved` channel join request as retryable approval state instead of returning `404 join request not found`. This lets a channel owner retry approval after the requester-node `channels.public.join_result` callback temporarily failed. Ordinary channel invites still are not accepted by the join-request approval action.

## 2026-06-22 Dirextalk Local MCP Backend Actions

Added six protected MCP-oriented P2P actions for the local Dirextalk MCP adapter:

- `mcp.rooms.search` on `POST /_p2p/query`
- `mcp.messages.list` on `POST /_p2p/query`
- `mcp.channel_posts.list` on `POST /_p2p/query`
- `mcp.channel_comments.list` on `POST /_p2p/query`
- `mcp.messages.send` on `POST /_p2p/command`
- `mcp.channel_comments.create` on `POST /_p2p/command`

All six required bearer auth. Owner `access_token` could call them, and `agent_token` was accepted for these fixed MCP actions at that point. The fixed body-action endpoints were removed on 2026-07-09; current clients use `POST /mcp`. The response contracts are intentionally concise for MCP tooling and do not expose full `conversationView`, `channelPostRecord`, `channelCommentRecord`, Matrix event payloads, projection state, capability maps, or internal Matrix tokens.

Ordinary MCP message send/list remains separate from channel post/comment product content. `mcp.messages.send` writes a plain `m.room.message` without `p2p_kind`; `mcp.messages.list` reads ordinary Matrix timeline messages through a server-side Matrix reader and filters out events carrying product `p2p_kind`. No P2P ordinary-message store was added.

At that point, the P2P body-action count was 91. Current action metadata is generated into `docs/product-action-contract.json`.

## 2026-07-03 Account Deletion

Added protected owner HTTP-only command `portal.account.delete` on `POST /_p2p/command`.

Request:

```json
{
  "action": "portal.account.delete",
  "params": {
    "confirm": "delete_account"
  }
}
```

Behavior:

- Requires owner `access_token`; `agent_token` is rejected.
- Cannot be called through `GET /_p2p/ws` `client.request`; WS returns `action requires http`.
- Before database reset, the server publishes `io.dirextalk.room.profile` direct-room account-deleted dissolve state for accepted direct contacts so peers hide the deleted account, leaves accepted direct-contact rooms, dissolves groups/channels owned by the portal owner, leaves groups/channels where the owner is only a member, and deactivates local owner/agent Matrix accounts.
- If a critical leave/dissolve/deactivation step fails, the server returns an error and does not clear databases.
- On success, the server writes a non-secret deprovision marker to the portal credentials file, clears configured local databases, clears in-memory product/session state, and schedules local message-server shutdown. It does not destroy AWS/cloud instances.

Response includes `status: "deprovisioned"`, operation counts such as `contacts_left`, `groups_dissolved`, `channels_dissolved`, `accounts_deactivated`, and `database_reset: true`.

## 2026-06-22 Matrix-First Cleanup

This pass removes the remaining ambiguous compatibility surface from current code, examples, and skills.

Breaking removals and contract changes:

- `portal.setup` is no longer a P2P action. Portal initialization is automatic; clients use `portal.bootstrap`, `portal.auth`, `portal.status`, and `portal.password`.
- `P2P_BOOTSTRAP_CREDENTIALS_FILE` is no longer a compatibility alias. Use `P2P_PORTAL_CREDENTIALS_FILE`.
- Removed legacy Matrix product state is no longer generated, read, or projected. Current product state is `io.dirextalk.room.profile`, `io.dirextalk.member.policy`, and `io.dirextalk.join_request`.
- Public channel approval no longer exposes Matrix invite as the product workflow. Approval writes `io.dirextalk.join_request status=approved`; the requester homeserver performs Matrix join.
- New public internal action `channels.public.join_result` carries owner-node approval results to the requester node. Params: `room_id`, `channel_id`, `user_id`, `status`, `reason`, `server_names`, and `request_id`.
- Public channel join response status is one of `pending`, `rejected`, `approved`, `joining`, `joined`, or `join_failed`.
- Added protected action `agent.matrix_session.create` on `POST /_p2p/command`. It initially required bearer `access_token`; current servers accept owner `access_token` or `agent_token`. It returns a Matrix Client-Server session: `access_token`, `device_id`, `user_id`, and `homeserver`.
- `portal.bootstrap`, `portal.auth`, and `portal.password` return one setup state field: `initialized`. It is `false` while the generated initial password is still in use and becomes `true` after `portal.password` changes that password. Clients should store `access_token` and route by `initialized`; profile completion is independent.

The live P2P body-action contract is generated from `p2p/serviceapi.ActionSpecs` into `docs/product-action-contract.json`. Public actions are `portal.bootstrap`, `portal.auth`, `portal.status`, `contacts.reactivate`, `rooms.reactivate`, `reports.submit`, `channels.public.search`, `channels.public.get`, `channels.public.posts.list`, `channels.public.join_request`, `channels.public.join_result`, and `users.public_channels`. `rooms.reactivate` and `channels.public.join_result` are public HTTP-only node-to-node callbacks and are not valid WS `client.request` actions.

## Current Pass

This pass completes the Matrix-only ordinary message migration for Dirextalk product rooms. There is now one ordinary message source of truth: Matrix Client-Server event storage and timelines. P2P product APIs keep product metadata, contact/group/channel state, channel post/comment projections, calls, favorites, follows, Agent configuration, and bootstrap metadata.

Breaking removals from the P2P body-action surface:

- `sync.messages`
- `sync.unread`
- `search`
- `rooms.send`
- `rooms.send_media`
- `rooms.messages.delete`
- `rooms.messages.delete_batch`
- `rooms.messages.delete_range`
- `rooms.messages.recall`
- `contacts.export`
- `contacts.download`
- `contacts.import`

The removed actions are absent from `p2p.Service.Handle` and the dual-node smoke business flow. Calls to those names are treated as unknown P2P actions. Clients must not use them as deprecated compatibility paths.

## Matrix Message Contract

Ordinary private chat, group chat, and channel chat use Matrix Client-Server APIs:

- Send text/media: `PUT /_matrix/client/v3/rooms/{roomID}/send/m.room.message/{txnID}`
- Incremental sync and unread data: `GET /_matrix/client/v3/sync`
- Offline/history reads: `GET /_matrix/client/v3/rooms/{roomID}/messages`
- Search: `POST /_matrix/client/v3/search`
- Distributed recall: Matrix redaction routes
- Per-user local hide/clear: `POST /_matrix/client/v1/io.dirextalk/rooms/{roomID}/local_delete`

`local_delete` request forms:

```json
{ "event_ids": ["$event:server"] }
```

```json
{ "clear": true }
```

`event_ids` hides specific Matrix events from the requesting user's Matrix read paths. `clear=true` hides room events through the current sync stream position. Neither form sends a Matrix redaction or changes other users' history.

The local hide state is persisted in syncapi storage and filtered from:

- `/sync`
- `/rooms/{roomID}/messages`
- `/rooms/{roomID}/event/{eventID}`
- `/rooms/{roomID}/context/{eventID}`
- `/rooms/{roomID}/relations/...`
- `/search`

## Product Room Classification

Room classification remains a product metadata concern and is not rebuilt from message history:

- Direct/private chats: `contacts.list`, `sync.bootstrap.contacts`, pending friend requests, and Dirextalk direct room profile state.
- Groups: `groups.list`, `sync.bootstrap.groups`, pending group invites, and `io.dirextalk.room.profile` with group type.
- Channels: `channels.list`, `sync.bootstrap.channels`, pending channel notices, public channel actions, and `io.dirextalk.room.profile` with channel type.

`sync.bootstrap.rooms` was removed. `sync.bootstrap` now returns product metadata sections only; clients should combine those sections with Matrix room timelines from `/sync` instead of consuming a P2P-derived room list.

## Channel Posts And Comments

Channel post/comment product content still uses Matrix events, but carries product classification:

- `p2p_kind=channel_post` projects to `p2p_channel_posts`.
- `p2p_kind=channel_comment` projects to `p2p_channel_comments`.
- Matrix ProductPolicy enforces channel owner/comment rules before write. ProductCore group/channel roles are owner/member only.
- Channel post/comment recall uses Matrix redaction and removes the product projection.

Ordinary `m.room.message` events without channel post/comment product markers are not mirrored into P2P message tables and do not emit P2P ordinary-message SSE events.

## P2P Product Surface

The product route contract remains:

- `GET /_p2p/health`
- `POST /_p2p/query`
- `POST /_p2p/command`
- `GET /_p2p/events`
- `GET /.well-known/portal/owner.json`

At that point, protected product actions required bearer `access_token`, while `agent_token` was accepted only for fixed `mcp.*` actions and `GET /_p2p/events`. Current servers have removed `GET /_p2p/events` and accept `agent_token` only for product body-action `agent.matrix_session.create` and standard `POST /mcp`. Current public actions are generated into `docs/product-action-contract.json` and include `portal.bootstrap`, `portal.auth`, `portal.status`, `contacts.reactivate`, `rooms.reactivate`, `reports.submit`, `channels.public.search`, `channels.public.get`, `channels.public.posts.list`, `channels.public.join_request`, `channels.public.join_result`, and `users.public_channels`.

Current action metadata is generated into `docs/product-action-contract.json`.

## ProductCore Conversation Contract

`conversations.list` and `conversations.get` expose ProductCore conversation identity for clients. The response keeps the existing stable fields:

- `conversation_id`
- `matrix_room_id`
- `kind`
- `lifecycle`
- `peer_mxid`
- `title`
- `avatar_url`
- `last_event_id`
- `last_activity_at`
- `projection_state`
- `projection_reason`

This pass adds hydrated membership and relationship fields to the conversation view:

- `member_count`: direct conversations return `2`; group and channel conversations return the joined member count from ProductCore membership state when available.
- `membership`: the current owner membership in this conversation. Direct accepted contacts map to `join`; pending direct contacts map to `pending`.
- `relationship_status`: direct-contact relationship state such as `accepted`, `pending_inbound`, or `pending_outbound`.
- `role`: current owner role in the conversation, for example `member` or `owner`.
- `hydration_state`: `ready` when ProductCore has enough state to open the conversation, otherwise `pending`, `conflict`, or `failed`.
- `hydration_reason`: machine-readable reason when hydration is not ready, for example `owner_membership_missing`.
- `capabilities`: server-derived operation flags. Current keys are `open`, `send`, `send_media`, `call`, `invite`, `manage_members`, `rename`, `remove_members`, `leave`, `delete`, `post_create`, `comment_create`, `reaction_toggle`, `post_recall`, `comment_recall`, and `comments_enabled`. Group/channel management and post capabilities are true only when the current owner is joined with role `owner`.

Clients should use these ProductCore fields instead of inferring room type or owner membership from Matrix timeline shape, display names, or member-count text.

## ProductCore Create/Join Mutation Result

`groups.create`, `groups.join`, and `channels.join` now return the ProductCore conversation created or hydrated by the mutation path:

- `operation`: `{action, status, room_id, conversation_id}` for the completed mutation.
- `conversation`: the same `ConversationView` shape returned by `conversations.list/get` when a conversation record exists for the created or joined room.

Clients should open the returned `conversation.conversation_id` / `conversation.matrix_room_id` directly after a successful create or join. They should not reconstruct a chat route from group/channel names, member counts, or Matrix room aliases.

## Contact Re-Request Semantics

`contacts.request` is idempotent by `mxid`. When a non-deleted contact already exists for the same peer, the action returns the stored contact and does not create a second direct Matrix room. Existing pending contacts re-send a pending invite in the stored room. Existing accepted contacts normally return unchanged; when `remote_node_base_url` is supplied and the peer node reports that it no longer retains the relationship, the contact becomes `pending_outbound` in the stored room and waits for peer approval.

Inbound direct invite projection now treats the Matrix membership event sender as the authoritative requester identity. `io.dirextalk.room.profile` stripped-state fields such as `requester_mxid` and `domain` cannot override the projected `peer_mxid` or peer domain; if they conflict with the event sender, profile display fields from that direct profile are ignored. This prevents a third user from making a pending friend request appear to come from another Matrix user or domain.

`contacts.request` restores an existing `deleted` contact for the same peer only when the peer still retains the accepted relationship. The response preserves the original `room_id`, refreshes supplied display/domain metadata, returns `status: "accepted"`, and rejoins the original direct Matrix room through the P2P transport when transport is configured. If the requester has left the old invite-only direct room, the requester node calls the peer node `contacts.reactivate`; the peer node re-invites the requester only when it still has an accepted contact for the same `peer_mxid` and `room_id`. This lets the side that deleted a contact intentionally restore that old direct conversation without peer approval. If the peer node has an existing non-accepted contact for the same requester and old `room_id`, `contacts.reactivate` records `pending_inbound` on the peer node and returns `status: "pending_inbound"`; the requester node preserves the original `room_id`, returns `pending_outbound`, does not try to invite from a user that already left the direct room, and does not join or restore chat until the peer accepts. If the peer no longer has a matching contact record, `contacts.request` preserves the original `room_id`, returns `pending_outbound`, sends a direct invite for that old room, and waits for peer acceptance. Requests to add the local owner and self `contacts.reactivate` calls are rejected with `400`.

If a node still has an accepted contact for the real Matrix sender and receives a fresh direct invite for a different room, it does not create a new pending contact from the supplied invite metadata. Instead, it re-invites that real sender to the retained accepted `room_id`, allowing a peer whose local contact data was deleted or rebuilt to recover the old direct room. `contacts.reactivate` also ignores caller-supplied profile fields for non-accepted retained contacts; missing local display/domain values are derived from `requester_mxid`.

When `contacts.request` is called again for an existing `pending_outbound` peer, the requester node now re-sends a direct Matrix invite to the stored direct room instead of only returning the cached contact. A target node that previously stored the peer as `rejected` now accepts the new direct invite projection and changes the contact back to `pending_inbound`, so pending friend request notices can appear again.

When a direct invite projection creates or reopens a local `pending_inbound` contact, `/_p2p/events` now emits `contact.requested` with `room_id`, `peer_mxid`, `display_name`, `avatar_url`, `domain`, and `status: "pending_inbound"`. Existing pending/accepted contacts remain de-duplicated and do not emit another contact request event.

`contacts.request` accepts optional friend-request text as `remark` and also recognizes `request_message`, `message`, or `reason` for compatibility. Pending contact responses, `contacts.list`, `sync.bootstrap.contacts`, `sync.bootstrap.pending.friend_requests`, and `contact.requested` events expose the text as `remark` while the request is pending. The value is carried in native direct-room profile state for invite projection and is cleared when the contact becomes accepted so it is not reused as a contact display remark or conversation title.

`contacts.requests.accept` is idempotent for an already accepted contact, but first confirms the local owner's Matrix membership. A confirmed join returns the stored contact without another Matrix join; a stale accepted projection re-enters the recovery path and repairs the direct-room/contact/conversation state.

P2P contact persistence now enforces one row per `peer_mxid`. Existing duplicate contact rows are compacted during migration, preferring `accepted`, then `pending_inbound`, then `pending_outbound`, then rejected/deleted records.

Contact responses now expose peer avatar metadata through `avatar_url`. This applies to `contacts.list`, contact mutation responses, and the `contacts` array returned by `sync.bootstrap`. Direct-contact conversations derived from contact records also carry the same `avatar_url` so clients can render the peer avatar consistently after bootstrap or contact mutations.

Contact mutation responses now include a ProductCore `operation` object and attach the hydrated direct `conversation` when the contact has a `room_id`. This applies to `contacts.request`, `contacts.reactivate`, `contacts.requests.accept`, `contacts.requests.reject`, `contacts.requests.delete`, `contacts.delete`, and `contacts.update`. Clients should consume the returned `conversation_id` / `matrix_room_id` instead of reconstructing a direct chat route from peer display names or Matrix direct-room heuristics.

## Group Invite Reject And Stored Member Role Semantics

`groups.invite.reject` records the current local user's pending group invite as `membership: "reject"` and returns `{status: "rejected", member}`. Rejected group invites are hidden from `groups.members` and `groups.list`, matching the first-version ProductCore rule that hidden memberships (`leave`, `remove`, `reject`, `ban`) are not ordinary visible members.

Group and channel member mutations now load the existing ProductCore member record before applying leave/remove/mute/unmute/reject transitions. Owner protection is therefore based on persisted `role` and `membership`, including after a service reload backed by PostgreSQL, instead of relying on an in-memory default member record. ProductCore group/channel roles are owner/member only.

Group/channel invite and member mutation responses now include a ProductCore `operation` object and attach the hydrated `conversation` when the mutated room has a `p2p_conversations` record. This applies to `groups.invite`, `groups.invite.reject`, `groups.leave`, `groups.member.remove`, `groups.member.mute`, `groups.member.unmute`, `channels.invite`, `channels.leave`, `channels.member.remove`, `channels.member.mute`, `channels.member.unmute`, `channels.join_request.approve`, and `channels.join_request.reject`.

## Client Migration Notes

Clients should align as follows:

- Message list, offline history, search, unread, and recall use Matrix SDK calls.
- Local clear-history/delete-for-me uses the Dirextalk Matrix `local_delete` extension.
- Conversation placement still uses product metadata: contacts for private chats, groups for groups, channels for channels.
- `sync.bootstrap` is still useful for product metadata and pending notices, but no longer provides a `rooms` array.
- Agent API allow-lists must not include removed message/search/backup actions.

## Updated Artifacts

- P2P action registry and fixed Agent MCP allowlist.
- P2P storage migration dropping the legacy ordinary-message mirror table.
- Syncapi local hide storage and Matrix read-path filtering.
- Roomserver projector rules for ordinary messages, channel posts/comments, reactions, and redactions.
- Dual-node smoke script using Matrix send/history/search/redaction/local_delete.
- Manual example set with removed P2P actions deleted and `local_delete` examples added.
- Feature inventory and implementation notes.

## 2026-07-10 — Server release control v1

Added three protected owner Product actions:

- `client.version.report` over HTTP or owner WS, with required stable `client_version` and optional short `build_number`/`platform`. The server normalizes an omitted `v` prefix and stores the report only on the current portal device row.
- `release.v1.status` over HTTP or owner WS. It returns additive running build/schema and reported client fields plus updater-authoritative `available`, `release_available`, `update_available`, `discovery_status`, `compatibility`, stable `reasons`, `operations`, and release metadata. Updater connection failure is a successful parseable unavailable response.
- `release.v1.apply` over HTTP only. It accepts exactly `plan_token`, UUID `idempotency_key`, and `confirm="apply_release_change"`; all unknown and infrastructure-shaped fields are rejected with structured code `release_apply_invalid_params`.

Owner access tokens are used only at the Product API boundary. The Unix updater client reads its fixed mounted control token file, and neither owner/control/plan/job tokens are written to release storage or errors. Account deletion now sets updater desired state `deprovisioned` before destructive work and aborts if that step fails.

Hardening follow-up: client reports now carry the authenticated portal device/session from HTTP authorization or WS ticket creation and reject stale sessions with `client_session_stale`. Persistence uses a narrow device-CAS update, while same-device full portal saves preserve client build fields and device switches clear them atomically. Status always overwrites updater `current_version`/`client_version` echoes with local facts. If account deletion fails after setting `deprovisioned`, the backend best-effort restores `running`; a failed restoration returns `account_delete_watchdog_restore_failed` without upstream details.

Same-device password-rotation follow-up: `portal.password` serializes its access-token/session-generation mutation and portal persistence with `client.version.report` validation/CAS. The lock is released before Matrix-session refresh, preventing both stale-report persistence and recursive mutex acquisition without changing the public action envelope.

## 2026-07-31 — execution.v2 owner action envelope

The ProductCore registry now defines the owner-only HTTP+WS action names
`projects.analyze`, `analyses.*`, `targets.*`, `plans.*`, `deployments.*`,
`runs.*`, `confirmations.*`, `artifacts.get`, and `service_bindings.*`.
Mutation requests require UUID `idempotency_key`; revisioned changes require
`expected_revision`. Unknown fields and client-supplied digests are rejected.
The dedicated execution port is readiness-gated and returns stable
`execution_v2_not_ready` until a typed store/coordinator/provider dependency is
explicitly enabled. No SSH or generic HTTP provider capability is advertised.

The bootstrap path is also server-authoritative. A successful
`projects.analyze` creates the stable project row in the same idempotent
transaction as its first immutable analysis, so no separate client-created
project prerequisite exists. `targets.import` accepts only an exact verified
AWS credential revision, EC2 instance ID, and idempotency key; account, region,
profile, capabilities, target/observation identities, revisions, and digests
are derived by the server. The action succeeds only after STS account binding,
running Linux EC2 identity, and SSM Online state are verified and the initial
observation is durably recorded.

`targets.reserve` is an additive owner-only reservation action for the V2
two-plan EC2 bootstrap. Its request contains only an exact verified AWS
credential revision, an instance-type selection, volume size, and UUID
idempotency key. The server owns account, region, infrastructure profile, AMI
parameter, architecture, SSM management mode, network policy, cost quote,
target identity, revision, and digests. It creates only revision 1 with
`kind=aws_compute_reservation`; it does not call AWS. The corresponding
`aws-ec2-provision` Recipe and typed CloudFormation intent/readback contract
are production-wired. `execution.v2.provision` is advertised only while the
durable SQL intent store, live AWS reservation catalog, typed CloudFormation
provider, worker lifecycle, and provider reconciliation loop are all ready.

Imported EC2 targets now receive the `observed_https_egress` network envelope
only after `DescribeSecurityGroups` proves public TCP/443 egress for the exact
attached security-group set. The target and observation bind that proof by
digest, and SSM pre-dispatch inspection must prove it again; running EC2 and
SSM Online status alone never authorizes registry HTTPS access.

Watchdog follow-up: `release.v1.status` now includes an additive `watchdog` object with `status`, derived `degraded`, optional RFC3339 `cooldown_until` / `last_observed_at`, and stable `error_code`. The backend allowlists these fields from the Unix updater response, normalizes timestamps, derives `degraded` from the allowlisted status, and never forwards repair attempt history, service/image input, control data, or updater-only fields. Older or unavailable updater responses map to `watchdog.status="unknown"` with no repair operation inferred by the client.
