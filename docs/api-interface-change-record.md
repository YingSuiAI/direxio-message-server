# API Interface Change Record

> Historical audit only (non-current): this file records selected interface
> changes and decisions; it is not a current contract. Current code, tests,
> generated contract metadata, `README.md`, and maintained current docs take
> precedence. Git history is the complete audit trail.

## 2026-08-07 Federated channel post settings

`channels.posts.update` now publishes the post's complete mutable settings as
`io.dirextalk.channel.post.settings` Matrix room state keyed by `post_id`.
Every joined member node projects that state into durable PostgreSQL storage,
including when the state arrives before the original post history. A failed
Matrix state write leaves the local post projection unchanged.

For `p2p_kind=channel_comment`, the sending node's Matrix ProductPolicy checks
the target post state and the receiving projector independently rejects a
comment when that post has `comments_enabled=false`. The setting remains
post-scoped: channel discussion/chat, reactions, favorites, and other posts are
unaffected. The same state synchronizes post `visibility` between nodes.

## 2026-08-06 Release V2 development server versions

`release.v2.apply.target_version` now accepts canonical stable `vX.Y.Z` and
development `devX.Y.Z` server versions. The central server `version`, updater
status/current version, and active-job server versions use the same format.
Upgrade channels are isolated: a running `v` server accepts only newer `v`
targets, while a running `dev` server accepts only newer `dev` targets. Cross-
channel upgrades are rejected. Client versions and the central server
`preVersion` remain stable-only `vX.Y.Z` values.

## 2026-08-03 Dual Agent identities

`agent.config.get` and `agent.config.update` support mode-specific Agent identity objects. `native_agent_identity` owns Ying / Native Agent `display_name` and `avatar_url`; `online_agent_identity` owns Your Agent / Matrix bridge `display_name` and `avatar_url`. The existing top-level `display_name` and `avatar_url` remain for legacy clients and mirror the effective Native Agent identity in responses.

A legacy update that sends only non-empty top-level identity fields synchronizes both mode identities. When nested identities are sent, updates merge per mode so one mode can change without clearing or overwriting the other; nested values take precedence for their mode. Missing fields and empty strings do not clear an existing name or avatar.

Native Agent runtime config exposes only Ying's effective identity through the legacy top-level `display_name` and `avatar_url` keys and must not receive or persist `online_agent_identity`.

`agent.matrix_session.create` uses `online_agent_identity` when creating or ensuring the Matrix Agent session. Updating `online_agent_identity` also synchronizes the local `@agent:<server>` Matrix global profile and the Agent room `m.room.member` profile. Updating only `native_agent_identity` does not touch Matrix. If the desired config is saved but Matrix profile/member synchronization fails, `agent.config.update` returns `agent_identity_sync_failed`; `agent.config.get` continues to return the saved desired identity.

## 2026-08-02 Native Agent request-scoped BYOK web search

Added owner action `agent.web_search.test` over HTTP and owner WebSocket. The
request shape is `tool_credentials.web_search` with `enabled`, optional
`provider` (default `tavily`), and write-only `api_key`. The key is accepted
only for that request; it is never copied into Agent config, model profiles,
durable turn records, conversation memory, logs, errors, or result payloads.

When a chat/stream request carries the same valid Tavily credential, the
compiled Native Agent `web_search` tool is available for that turn. It is
absent for missing, disabled, or unsupported credentials. The adapter sends a
bounded HTTPS Tavily request that rejects redirects, with a 1,000-character
query limit, a server-enforced 1–10 result bound, 2 MiB response cap, and
15-second timeout. Results and provider errors are reduced to safe summaries;
provider response bodies and API keys are never returned.

Durable turn request digests and runtime events remain secret-free. Reconnect
and resume attach to an existing turn without persisting or rehydrating
`tool_credentials`; only the original in-memory request may use its key.

## 2026-08-01 Native Agent new-target planning and built-in Skills

An empty `agent.execution.v2.targets.list` result is an inventory state, not a
deployment-planning terminal. When AWS control and V2 provisioning are ready,
the embedded Native Agent can call the bounded
`native_agent_execution_v2_targets_reserve` tool. Reservation creates only a
revision-1 logical AWS compute target and does not call AWS or incur cost. The
Agent then creates an `aws-ec2-provision` plan/run; the owner-facing R2
`resource_purchase` confirmation remains the only path that creates the EC2
resource and promotes the target to its executable revision.

The global Native Agent rules require comparison of existing targets with a
new reservation and prohibit instructing the owner to prepare a target merely
because inventory is empty. Codex CLI, OpenClaw, Claude Code, and other AI
runtime plans must ask the owner to choose either an existing server-owned API
key secret reference or an interactive authorization gate. Secret plaintext is
never accepted in the chat or plan.

Embedded backend discovery now advertises `planning.skills` and
`agent.skills.list` returns the immutable built-in Planning Skill metadata.
This is a read-only catalog; mutable third-party/local Skill installation and
Message Server host execution remain unavailable. Placement and sizing intent
selection resolve to the consolidated immutable `aws-target-advisor` manifest.
The active catalog contains seven manifests; retired `placement-advisor`/
`resource-sizing` selections and exact `aws-target-advisor@1.1.0` pins remain
resolvable from an immutable archive, but are never listed or selected for new
intent matching.

## 2026-08-01 Native Agent deployment tools, server context, and titles

Embedded Native Agent now treats the allowlisted Execution V2 analysis, target,
plan, run, status, event, and Service Binding tools as compiled control-plane
capabilities. A persisted pre-V2 `enabled_tools` list no longer hides them
after upgrade; confirmation, raw SSM/SSH, AWS SDK passthrough, and arbitrary URL
tools remain unavailable. Deployment intent selects at most three immutable
planning skills: project intake, AWS target advice, and one container or
source/systemd deployment recipe.

When persistent server conversation memory is ready, chat requests with a
`conversation_id` accept only the current prompt/message and attachment
references. Client-supplied `messages` history is rejected; the server loads,
token-budgets, summarizes, and stores authoritative context. Model
summarization is automatic when a configured model is available, with
deterministic bounded text compaction as fallback. Context windows are
interpreted as token capacity rather than message count.

After the first successful persistent turn, the server best-effort generates a
short title with the active conversation model and updates only an empty title.
Provider failure falls back to a redacted, bounded title derived from the first
user instruction; explicit user renames always win.

## 2026-07-31 Execution V2 reset and direct-final schema

The unreleased workload/provision/deployment V1 surface was removed rather
than migrated. There is no V1 read, reconcile, destroy, retry, dashboard, or
compatibility adapter, and no V1 row is converted into V2. GeoLibre exists only
as a declarative recipe/test fixture.

AWS credential CRUD/test remains available as reusable control-plane state.
Execution V2 actions and capabilities stay unpublished until their PostgreSQL
coordinator, durable dispatch fence, typed transport, and strict client
contract are ready together.

The migration history from upstream `main` through v77 is unchanged. All
branch-only Agent and Execution V2 schema is created by one direct-final v78
migration; there is no branch compatibility/backfill migration chain.

## 2026-07-31 Offline legacy model-secret upgrade

`agent-secretctl upgrade` is the explicit one-shot migration for legacy
model-profile credential rows. It is not part of `message-server-init`, Compose,
or server startup. Operators must stop the persistent service before the run
and create one restorable backup set containing the PostgreSQL database, the v1
keyring file, and the legacy model-profile raw-key file. `agent-secretctl init`
creates or validates the v1 keyring; `upgrade` then uses only environment-
provided paths and the database DSN:

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

## 2026-08-06 Channel Post Settings And AWS Skill Release Gate

The new owner action `channels.posts.update` accepts `post_id` and at least one
of `visibility="public"|"private"` or boolean `comments_enabled`. Authorization
is limited to the post author or channel owner. Disabling comments rejects new
comments only for that post; it does not mute channel chat, the discussion
surface, or other posts. PostgreSQL stores the settings independently from the
immutable Matrix post event, and replay/backfill preserves explicit updates.

AWS-specific built-in Planning Skills are hidden for this release. They are
excluded from `agent.skills.list`, bootstrap metadata, explicit and
intent-based selection, and prompt injection. The built-in Native Agent system
prompt no longer advertises AWS workflows. Non-Skill AWS credential and
Execution V2 contracts are unchanged.

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

## 2026-07-30 Native Agent selected model pins

Server-managed Native Agent chat accepts the selected `model_profile_id`
together with a complete `model_profile_revision` / `credential_version` pair.
The durable turn resolves that immutable profile and credential snapshot before
reservation and stores only the pin and secret-free request digest. Supplying
only one version field, or conflicting request/context pins, fails before a
turn is reserved. Omitting the profile still resolves the owner's default
conversation profile.
