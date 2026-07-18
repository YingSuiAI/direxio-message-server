# P2P Integrated AS Implementation

## Goal

This fork embeds the P2P product API into the Dirextalk Message Server monolith instead of running a separate AS process. Matrix protocol endpoints remain under `/_matrix/*`. Product APIs primarily use a small body-action surface under `/_p2p/*`; the current URL-shaped exception is the standard MCP Streamable HTTP endpoint `POST /mcp`.

## Current Architecture

```text
p2p-client
  |
  | POST /_p2p/query   {"action":"...", "params":{...}}
  | POST /_p2p/command {"action":"...", "params":{...}}
  v
Dirextalk Message Server monolith
  |
  +-- Matrix client/federation/media APIs: unchanged
  |
  +-- p2p.Service
        - portal bootstrap/auth session envelope
        - owner profile projection
        - sync bootstrap product metadata and read markers
        - contact/group/channel/call/favorites/follows action catalog
        - Dirextalk Message Server-managed P2P database store for portal, profile,
          read markers, contacts, groups, channels, posts, comments,
          reactions, members, calls, favorites, and follows
        - internal dirextalktransport DendriteTransport for outgoing Matrix
          room creation and member lifecycle/redaction submission through
          roomserver APIs
        - P2P roomserver output projector for Dirextalk native product
          state, channel post/comment messages, reactions, members,
          and legacy p2p.* state events
        - shared helpers for group/channel room creation and owner
          membership seeding, so product actions reuse the same lifecycle code
```

Ordinary private, group, and channel chat messages have a single source of truth: Matrix Client-Server event storage and timeline APIs. Product P2P tables no longer mirror ordinary `m.room.message` events.

## Backend Changes

New package:

- `p2p/routing.go`
- `p2p/service.go`
- `p2p/storage.go`
- `p2p/transport.go`
- `p2p/dendrite_transport.go`
- `internal/dirextalktransport`
- `internal/dirextalktransport/dendrite`
- `internal/dirextalkmatrix`
- `p2p/projector.go`
- `p2p/consumer.go`
- `p2p/routing_test.go`
- `p2p/storage_test.go`
- `p2p/transport_test.go`
- `p2p/projector_test.go`
- `p2p/business_state_test.go`

Dirextalk Message Server route integration:

- `internal/httputil/paths.go`: adds `P2PPathPrefix = "/_p2p/"`.
- `internal/httputil/routing.go`: adds a dedicated `Routers.P2P` mux.
- `setup/base/base.go`: mounts the mux on the external HTTP server.
- `setup/monolith.go`: creates the P2P store, creates `DendriteTransport`, and registers `p2p.Service`.
- `docker-compose.p2p.yml`: builds the local Dirextalk Message Server fork, starts PostgreSQL 18, generates config/key material once, and exposes the integrated service on port `8008`.
- `docker-compose.p2p-dual.yml`: starts two independent Dirextalk Message Server+P2P instances with two PostgreSQL 18 databases for local federation/member-sync testing.

Storage integration:

- `p2p.NewDatabaseStore()` reuses Dirextalk Message Server `sqlutil.Connections`.
- Production persistence is expected to use PostgreSQL 18. The P2P store uses the global Dirextalk Message Server database when `global.database.connection_string` is configured; otherwise it falls back to the roomserver database. This keeps single-database PostgreSQL deployments and per-component development configs working without adding a new config section.
- The P2P schema is created through Dirextalk Message Server `sqlutil.Migrator`.
- Current persistent tables:
  - `p2p_portal`
  - `p2p_read_markers`
  - `p2p_contacts`
  - `p2p_groups`
  - `p2p_channels`
  - `p2p_channel_posts`
  - `p2p_channel_comments`
  - `p2p_reactions`
- `p2p_members`
  - `p2p_calls`
  - `p2p_favorites`
- `p2p_follows`
- Business indexes are created with `CREATE INDEX IF NOT EXISTS` for product list, channel post/comment, reaction, member, contact, call, favorite, and follow paths. Ordinary message timelines, search, unread data, and redactions use Matrix/syncapi storage.
- If the store cannot be opened, the service logs a warning and falls back to in-memory state so the Matrix server can still start.
- `p2p_contacts.avatar_url` stores peer avatar metadata for `contacts.list`, contact mutation responses, direct conversation views, and `sync.bootstrap.contacts`.
- `p2p_members.avatar_url` stores projected Matrix member avatar state so member lists can refresh nickname/avatar changes from other homeservers.

## Docker Compose

The local development deployment is:

```bash
docker compose -f docker-compose.p2p.yml up --build
```

Services:

- `postgres`: `postgres:18`, persisted in the `p2p_postgres_data` volume and exposed on host port `15432`.
- `dendrite-init`: one-shot initializer that creates `/etc/dirextalk-message-server/matrix_key.pem` and `/etc/dirextalk-message-server/message-server.yaml` if they do not already exist.
- `dendrite`: local image built from this fork's root `Dockerfile`, persisted config/data volumes, HTTP exposed on host port `8008`, and `P2P_PORTAL_CREDENTIALS_FILE=/var/dirextalk-message-server/p2p/bootstrap.json`.

Read the generated local login credentials from the running container:

```bash
docker compose -f docker-compose.p2p.yml exec message-server cat /var/dirextalk-message-server/p2p/bootstrap.json
```

The file contains `password`, unified `access_token`, `agent_token`, `owner_user_id`, `homeserver`, and `agent_room_id`. `portal.auth` uses `password`; Matrix client calls use `access_token`; logged-in product actions use WS `client.request` after creating a `realtime.ws_ticket.create` ticket with the owner `access_token` and receiving `server.ready`. If WS is not ready or disconnected at click time, clients use owner HTTP body-action fallback immediately and let realtime WS reconnect in the background. `agent.matrix_session.create` remains an HTTP body action and is not migrated into WS. Standard external MCP clients use `POST /mcp`. `agent_token` is accepted only for product body-action `agent.matrix_session.create` and `POST /mcp`.

Dual-instance federation test deployment:

```bash
export P2P_DUAL_PUBLIC_HOST="${P2P_DUAL_PUBLIC_HOST:-host.docker.internal}"
docker compose -f docker-compose.p2p-dual.yml up --build
```

Services:

- Instance A: Matrix/P2P API `http://127.0.0.1:18008`, federation TLS `18448`, PostgreSQL 18 host port `15433`, server name `dendrite-a:8448`.
- Instance B: Matrix/P2P API `http://127.0.0.1:28008`, federation TLS `28448`, PostgreSQL 18 host port `15434`, server name `dendrite-b:8448`.
- The local dual config generates TLS cert/key material and clears Dirextalk Message Server's private-network federation deny list only for this local test topology.

Read dual-instance credentials:

```bash
docker compose -f docker-compose.p2p-dual.yml exec dendrite-a cat /var/dirextalk-message-server/p2p/bootstrap.json
docker compose -f docker-compose.p2p-dual.yml exec dendrite-b cat /var/dirextalk-message-server/p2p/bootstrap.json
```

Override the Matrix server name when generating a fresh config:

```bash
export MESSAGE_SERVER_NAME="matrix.example.com"
docker compose -f docker-compose.p2p.yml up --build
```

If the config volume already exists, remove it before regenerating with a different server name:

```bash
docker compose -f docker-compose.p2p.yml down
docker volume rm dirextalk-p2p_p2p_dendrite_config
```

Matrix transport integration:

- `p2p.Transport` is the boundary between product actions and Matrix.
- `p2p.DendriteTransport` calls `roomserverAPI.PerformCreateRoom` for contact, group, and channel room creation.
- `p2p.DendriteTransport` calls `roomserverAPI.PerformInvite`, `PerformJoin`, and `PerformLeave` for group/channel invite, join, leave, and member removal paths.
- Ordinary text/media sends, message history, unread data, search, and redaction are Matrix Client-Server API responsibilities. Product code gates those Matrix writes through ProductPolicy for Dirextalk rooms.
- Per-user local history hiding is implemented as `POST /_matrix/client/v1/io.dirextalk/rooms/{roomID}/local_delete`, backed by syncapi local hide storage and read-path filtering. This hides events for the requesting user only and does not redact events for other users or nodes.
- `profile.update` updates local profile storage, local joined member rows, and publishes refreshed `m.room.member` state containing `displayname` and `avatar_url` to joined Matrix rooms.
- `DendriteTransport.GetRoomChannel` reads native `io.dirextalk.room.profile` state by room ID, and `DendriteTransport.ListRoomMembers` reads current `m.room.member` state so a remote join can backfill channel metadata and all current members after federation.
- Remote public channel lookup requires `remote_node_base_url` in the request and validates Matrix room IDs plus the remote URL before outbound calls. TLS verification is enabled by default; `P2P_REMOTE_NODE_INSECURE_SKIP_TLS_VERIFY=true` is only for local self-signed test nodes.
- New direct, group, and channel room creation writes `m.room.create.content.type` as `io.dirextalk.room.direct`, `io.dirextalk.room.group`, or `io.dirextalk.room.channel`. New group rooms also write `m.room.history_visibility=joined`, so later members only receive ordinary timeline events from their own join point. New channel rooms are unified post+chat rooms and explicitly write `m.room.history_visibility=shared` on creation and when binding an existing room, so joined members can see existing channel posts and comments. Direct/group/channel creation and group/channel updates write native `io.dirextalk.room.profile` state. Channel type is legacy immutable metadata, and `channels.update` ignores `channel_type` for old-client compatibility; current channel behavior must not branch on `chat` vs `post`. Direct profile stripped state carries `requester_mxid`, `target_mxid`, requester display/avatar, requester domain, and pending request `remark`, but inbound direct-contact projection treats the Matrix membership event sender as the authoritative peer identity and derives the peer domain from that MXID.
- Direct-room `m.room.member` join/profile updates from the peer refresh the projected contact display name/avatar and direct conversation view on the current node.
- Repeated `contacts.request` calls for an existing pending outbound peer re-send a Matrix direct invite to the stored room. If the target node had rejected or deleted the prior relationship, the new invite projection reopens the contact as `pending_inbound` for pending friend request notices and does not join the target user until they accept. If the target node still has an accepted contact for the real Matrix sender but the requester created a fresh direct room because its local data was deleted or rebuilt, the target node first re-invites that sender to the retained accepted `room_id` instead of trusting the new invite metadata. If Matrix reports that retained room as already joined but the rebuilt sender cannot rejoin its old invite-only room state, both nodes fall back to the real sender's new direct room as the accepted relationship; old direct history is not copied into that replacement room. A requester that previously deleted the contact may restore the old direct room without peer approval only when the peer node still retains the accepted relationship and can re-invite the requester through `contacts.reactivate`; if the peer node has the old non-accepted contact, `contacts.reactivate` records `pending_inbound` there and the requester keeps `pending_outbound` in the same old `room_id` without attempting a Matrix invite from a user that already left. If the peer no longer has a matching contact record, the stored old `room_id` is kept and the request remains `pending_outbound`. Direct invite projection emits `contact.requested` into the P2P outbox when it creates or reopens a pending inbound contact, allowing WS clients to refresh request badges without polling. Pending friend requests preserve optional request text as `remark` through contact responses, bootstrap pending notices, and WS `server.event` payloads until the contact is accepted.
- Group and channel creation now share `ensureProductRoom` for Matrix room creation/fallback room IDs and `saveOwnerMember` for owner membership seeding. This keeps the body-action API concise while avoiding separate duplicated URL-handler logic for group and channel lifecycles.
- Channel public join requests use a Matrix-first application lifecycle: approval channels store `pending` and write `io.dirextalk.join_request`; open public requests and approved requests write approved state and then call `Transport.JoinRoom` locally or call the requester node through `channels.public.join_result`. Final `join` is recorded only after Matrix join succeeds. If approval reaches `join_failed` because the requester-node callback temporarily failed, `channels.join_request.approve` can be called again to retry. `channels.join_request.reject` records `reject` locally, writes rejected join-request state, calls the requester node for remote users, and keeps the rejected requester out of normal member lists. If a group or private-channel member node was rebuilt and the owner node still sees that user as already joined, repeated `groups.invite` or `channels.invite` calls remove the stale joined membership, send a fresh Matrix invite, and use internal `rooms.reactivate` to restore an invite/pending card on the rebuilt node; the rebuilt user must still call `groups.join` or `channels.join` before local joined projections are written. Rebuilt public-channel members still reapply through `channels.public.join_request`; when the owner has stale joined membership for the requester, the owner removes it and sends the fresh Matrix invite before requester-node `channels.public.join_result`.
- After a channel Matrix join succeeds, `channels.join` and approved public join-result paths backfill historical `channel_post`, `channel_comment`, and `m.reaction` events from Matrix `/messages` into the local product projections. This applies to every channel room using shared history: Matrix federation carries the historical events, and the local ProductCore list APIs read the replayed projection rows. Ordinary channel chat messages remain Matrix timeline data and do not update post/comment/reaction projections.
- Main group/channel list payloads are joined-only. `sync.bootstrap.groups`, `sync.bootstrap.channels`, `groups.list`, and `channels.list` only return rooms where the local owner has true Matrix-projected `membership=join`; `invite` and `pending` records are reserved for `pending.group_invites` and `pending.channel_notices`.
- Channel invite-card sharing uses `channels.invite_grant.create`. The action persists a room-scoped grant in `p2p_channel_invite_grants`, requires the owner to be joined to the share room and owner of the target channel, then Matrix-invites current joined non-owner members of the share room to the channel. `channels.join` accepts `grant_id` plus `share_room_id`/`via_room_id` so private or shared channel cards can join without public lookup; public search still uses `channels.public.join_request`.
- `ProductPolicy` is shared by Matrix Client-Server routes and `p2p.DendriteTransport` for product room messages, reactions, redactions, joins, invites, leaves, kicks, and bans. In group/channel rooms, ProductCore roles are owner/member only. In channel rooms, `p2p_kind=channel_post` requires owner, `p2p_kind=channel_comment` and `m.reaction` obey `comments_enabled`, and plain `m.room.message` is not blocked solely by disabled comments. Sender mute enforcement reads `io.dirextalk.member.policy`; any room-profile `muted` field is projection/UI metadata, not the sender mute authority. P2P transition facade writes map ProductPolicy transport errors back to their policy status, and local message/reaction/post/comment projections are written or removed only after the Matrix write/redaction succeeds. Non-product Matrix rooms continue through Matrix-native authorization.
- The current policy index mode is `matrix_state`: ProductPolicy reads Dirextalk Message Server roomserver current state directly instead of trusting P2P projection tables. `portal.status.policy_index_ready` is true only when Matrix transport is wired; no-transport fallback mode reports `policy_index_mode=unavailable`.
- With Matrix transport enabled, channel post/comment recall authorization is delegated to `DendriteTransport.RedactEvent` and ProductPolicy/Matrix authorization; local `p2p_members` owner-role projection is only a no-transport fallback.
- When no transport is configured, the service keeps the local deterministic fallback used by unit tests and degraded startup.

Inbound projector integration:

- `p2p.OutputRoomEventConsumer` consumes the Dirextalk Message Server roomserver output stream with its own durable consumer.
- `p2p.Service.ProjectOutputEvent()` handles `OutputTypeNewRoomEvent`.
- Ordinary `m.room.message` events are not projected into P2P message storage. They remain in Matrix and are read through Matrix `/sync`, `/rooms/{roomID}/messages`, and `/search`.
- Messages with `p2p_kind = channel_post` are projected into `p2p_channel_posts`; Matrix SDK senders must be owner for ProductPolicy to accept the write.
- Messages with `p2p_kind = channel_comment` are projected into `p2p_channel_comments`; ProductPolicy rejects these and `m.reaction` when channel comments are disabled for ordinary members.
- The join-time channel content backfill reuses the same projection helpers and resolves Matrix reaction `m.relates_to.event_id` back to the corresponding post or comment event before writing `p2p_reactions`. Product reaction toggles carry the final `active` flag so historical backfill and live projection can sync both likes and unlikes.
- Native `io.dirextalk.room.profile` state is projected into `p2p_channels` and `p2p_groups`, and direct-room profile stripped state projects inbound contact requests. Removed legacy product state is ignored by current projectors.
- `io.dirextalk.member.policy` state projects member `role`/`muted` policy into `p2p_members`, while `m.room.member` remains the membership source of truth.
- `io.dirextalk.join_request` state projects pending/approved/rejected channel join-request status into `p2p_members` and emits `channel.join_request.changed` outbox notifications.
- Ordinary chat messages do not emit P2P outbox notifications. Clients receive ordinary message deltas from Matrix `/sync`; WS `server.event` carries product projections such as channel join requests and channel post/comment/reaction state.
- Offline device push is Matrix-native. Clients register HTTP pushers with a standalone Dirextalk Push Gateway, and this Dirextalk Message Server fork posts Matrix Push Gateway notifications from UserAPI; see `docs/dirextalk-push-gateway.md`.
- `m.reaction` events are projected into `p2p_reactions`.
- `m.room.member` state events are projected into `p2p_members`.
- For remote member events that only carry `room_id`, the projector resolves `room_id -> channel_id` from the projected channel table before writing `p2p_members`, so channel member queries and room member queries return the same membership set.
- `OutputTypeRedactedEvent` removes corresponding channel post/comment projections by Matrix event ID. Ordinary redactions are reflected by Matrix timeline/search APIs.
- Projector startup failure is logged as a warning and does not block Matrix service startup.

The route contract is intentionally small:

```http
POST /_p2p/query
POST /_p2p/command
POST /mcp
GET  /_p2p/ws
GET  /_p2p/health
```

`POST /mcp` is the standard MCP Streamable HTTP JSON-RPC endpoint for external MCP clients. It supports `initialize`, `tools/list`, and `tools/call`, requires `Authorization: Bearer <agent_token>`, rejects query-string tokens, validates `Origin`, returns 405 for GET/SSE while server-to-client streaming is unused, and does not pass the inbound bearer token downstream.

Request envelope:

```json
{
  "action": "contacts.list",
  "params": {
    "status": "accepted"
  }
}
```

Protected actions require:

```http
Authorization: Bearer <access_token>
```

The exception is `POST /mcp`, which requires `Authorization: Bearer <agent_token>` and rejects owner `access_token`.

Public actions currently allowed without bearer:

- `portal.bootstrap`
- `portal.auth`
- `portal.status`
- `contacts.reactivate`
- `rooms.reactivate`
- `reports.submit`
- `channels.public.get`
- `channels.public.join_request`
- `channels.public.join_result`
- `channels.public.search`
- `users.public_channels`

The complete action auth and transport contract is generated from `p2p/serviceapi.ActionSpecs` into `docs/product-action-contract.json`. `rooms.reactivate` and `channels.public.join_result` remain public callbacks but are HTTP-only and must not be called through WS `client.request`.

Startup now performs default portal initialization. When no `p2p_portal` row exists, the integrated service creates owner/agent tokens, owner profile metadata, and a default password automatically, then persists that state to PostgreSQL when a store is available. `P2P_PORTAL_PASSWORD` can override the default password; otherwise a new portal starts with a random 8-digit numeric password and writes it to the credentials file. `P2P_PORTAL_CREDENTIALS_FILE` writes the current credential JSON after startup and after password/session token changes. `portal.account.delete` first publishes the direct-contact account-deleted profile state, leaves direct/member rooms, dissolves owned groups/channels, then overwrites that credentials file with a non-secret `deprovisioned` marker before clearing local databases.

The login/password response exposes one setup flag: `initialized`. It is `false` while the generated initial password is still in use and becomes `true` after `portal.password` successfully changes that password. Profile completion is not part of setup state; Dirextalk Flutter stores `access_token` and routes by `initialized` only.

## Frontend Migration Target

The frontend should treat this server as a Matrix-native Dirextalk backend, not as the older URL-shaped AS facade. Current migration work is tracked in `C:\Users\84960\Desktop\dirextalk\p2p-client`.

P2P product calls use the body-action envelope:

```json
{
  "action": "stable.action.name",
  "params": {
    "room_id": "...",
    "channel_id": "...",
    "post_id": "...",
    "body": "..."
  }
}
```

Required client direction:

- `defaultAdminBaseUri()` should point to `/_p2p` on the same browser-accessible homeserver origin. New code should not target `/_as` or URL-shaped product routes.
- Ordinary room text, media, reaction, redaction, and Matrix membership operations should use Matrix SDK APIs. ProductPolicy now runs on those Matrix Client-Server writes for Dirextalk product rooms.
- Channel post/comment Matrix sends must include `p2p_kind=channel_post` or `p2p_kind=channel_comment`; media events keep their Matrix `msgtype` such as `m.image`, `m.video`, `m.audio`, or `m.file`.
- Product management and projection queries use P2P body-action names. Logged-in clients should send them as WS `client.request` frames when the owner WS has sent `server.ready`: contacts, public channel discovery and approval, group/channel management, channel post/comment/reaction records, favorites, follows, owner-directed group/channel reports, Agent config/password, `sync.bootstrap`, and read markers. If WS is not ready or disconnected at click time, clients send the same owner-authenticated action envelope to HTTP `/query` or `/command` immediately as fallback while realtime WS reconnects in the background. If a WS request was already sent and the response is lost, clients only HTTP-fallback actions that are safe to repeat. `agent.matrix_session.create` remains an HTTP body action and does not move into WS. Standard MCP clients use `POST /mcp` JSON-RPC with `agent_token`; fixed `mcp.*` body actions are removed from `/_p2p/query` and `/_p2p/command`. Friend and official report submission remains on the signed imadmin public API; owner-directed group/channel reports use ProductCore `reports.submit`, persist `p2p_reports`, and send Matrix report notices into `system_room_id`.
- `GET /_p2p/ws` is the product request/response and realtime refresh path. Clients create a short-lived ticket with `realtime.ws_ticket.create`, connect with `?ticket=...`, send `client.hello` with the persisted `since` cursor, wait for `server.ready`, send product queries/mutations as `client.request` when ready, and then ack handled events with `client.ack`. The old `client.command` compatibility alias is removed and returns `400 unsupported frame type`; clients must use `client.request`. When a non-zero cursor is older than the retained event window, WS emits `server.cursor_reset`; clients should clear local product projections, call `sync.bootstrap` once over ready WS or owner HTTP fallback when WS is not ready, and then continue from the newest handled `seq`.
- Current clients report app lifecycle and focused room over WS using `client.lifecycle` and `client.focus`. `client.lifecycle` carries `foreground` plus optional `state`, `hidden`, and `flags`; `client.focus` carries `room_id` plus optional `focused` and `flags`. The server uses this server-timestamped session state only to decide same-room foreground push suppression; hidden/background state keeps normal push behavior. It is not user-visible presence. Matrix global account data `io.dirextalk.push.context` remains a migration fallback when there is no fresh WS session.
- Agent bridge message intake, streaming previews, edits, and final replies use the `@agent:<server>` Matrix Client-Server session in the configured real `agent_room_id`. Agent bridge traffic is not exposed as `agent_room.message`, `client.agent_stream`, or `server.agent_stream` on `/_p2p/ws`.
- Ordinary chat/media history, unread counts, local hiding, search, and recall are Matrix responsibilities. Use Matrix send, `/sync`, `/rooms/{roomID}/messages`, `/search`, Matrix redaction, and Dirextalk Matrix `local_delete`.

User profile pages now call `getUserPublicChannels()` when opening a real contact/friend profile. The existing "她的频道" section is reused without layout changes and shows only public channels returned by the backend, including avatar, name, and `room_id`; private channels are filtered server-side.

The Web smoke path also keeps the existing pages intact:

- `app_router.dart` avoids `io.Platform.environment` and executable argument reads on Web, using `String.fromEnvironment('P2P_INITIAL_ROUTE')` where applicable.
- `auth_provider.dart` uses the Matrix SDK's `MatrixSdkDatabase('portal_im_db')` IndexedDB path on Web and keeps the existing SQLite/path_provider storage on native platforms.
- When the browser logs into a local Docker homeserver such as `http://127.0.0.1:18008`, `auth_provider.dart` keeps that browser-accessible URL even if AS returns an internal federation name such as `https://dendrite-a:8448`.

## Action Catalog

Backend product actions include:

- Portal/profile/sync/realtime: `portal.bootstrap`, `portal.auth`, `portal.status`, `portal.password`, `portal.account.delete`, `profile.get`, `profile.update`, `sync.bootstrap`, `sync.read_marker`, `realtime.ws_ticket.create`
- Contacts/blocks: `contacts.list`, `contacts.request`, `contacts.requests.accept`, `contacts.requests.reject`, `contacts.requests.delete`, `contacts.delete`, `blocks.add`, `blocks.list`, `blocks.remove`
- Groups: `groups.create`, `groups.update`, `groups.invite`, `groups.invite.reject`, `groups.members`, `groups.join`, `groups.leave`, `groups.mute`, `groups.unmute`, `groups.member.remove`, `groups.member.mute`, `groups.member.unmute`, `groups.invite_policy.update`
- Channels/users: `channels.create`, `channels.list`, `channels.public.get`, `channels.public.join_request`, `users.public_channels`, `channels.join`, `channels.update`, `channels.invite`, `channels.invite_grant.create`, `channels.leave`, `channels.members`, `channels.member.remove`, `channels.member.mute`, `channels.member.unmute`, `channels.join_request.approve`, `channels.join_request.reject`, `channels.posts.list`, `channels.posts.create`, `channels.posts.recall`, `channels.comments.list`, `channels.comments.create`, `channels.comments.recall`, `channels.post_reaction.toggle`, `channels.comment_reaction.toggle`, `channels.read_marker`, `channels.my_comments`, `channels.my_reactions`
- Calls/favorites/follows/agent/plugins: `calls.create`, `calls.get`, `calls.active`, `calls.list`, `calls.incoming`, `calls.event`, `favorites.list`, `favorites.add`, `favorites.delete`, `favorites.delete_batch`, `follows.list`, `follows.add`, `follows.remove`, `agent.password`, `agent.matrix_session.create`, `agent.config.get`, `agent.config.update`, `agent.runtime.profile.get`, `agent.runtime.profile.update`, `agent.chat`, `agent.chat.stream`, `agent.models.list`, `agent.runtime.inspect`, `agent.runtime.install`, `agent.runtime.which`, `agent.runtime.run`, `agent.skills.list`, `agent.skills.install`, `agent.skills.enable`, `agent.skills.disable`, `agent.skills.uninstall`, `agent.skills.registry.search`, `agent.mcp.servers.list`, `agent.mcp.servers.install`, `agent.mcp.servers.enable`, `agent.mcp.servers.disable`, `agent.mcp.servers.uninstall`, `agent.mcp.registry.search`, `agent.context.compress`, `agent.config.propose_patch`, `plugins.catalog.list`, `plugins.installed.list`, `plugins.install`, `plugins.enable`, `plugins.disable`, `plugins.uninstall`, `plugins.config.get`, `plugins.config.update`, `plugins.job.get`, `plugins.health`, `plugins.logs.tail`, `plugins.invoke`, `plugins.invoke.stream`

Contact, group/channel invite, and member mutation actions return `operation` and, when a ProductCore conversation exists, the hydrated `conversation` so clients can refresh, open, or close the current route without reconstructing room state from names, member counts, or Matrix room metadata.

Owner block actions persist a contact blacklist in the P2P store. Contacts are keyed by `peer_mxid`. Each row stores `display_name` and `avatar_url` for client display, filled from existing contact metadata when omitted. `blocks.list` returns a `contacts` array for the user settings page. Friend requests, inbound direct invite projection, and inviting a blocked user to a group/channel check the blacklist before Matrix writes or pending request projection and return `403 already blocked` when the contact has already been blocked.

Agent authorization is fixed: owner `access_token` may call protected actions and create realtime WS tickets, while `agent_token` may call only product body-action `agent.matrix_session.create` and `POST /mcp`. Dynamic Agent permission endpoints are removed.

MCP capabilities are owner-scoped proxy operations. `agent_token` authorizes `POST /mcp`, and MCP handlers read/write product data from the portal owner view. `agent.matrix_session.create` returns a Matrix Client-Server session for the local `@agent:<server>` bridge user. Default owner-scoped ordinary MCP message send rejects the configured `agent_room_id`; agent-room replies are reserved for the internal gateway marker path, which sends as `@agent:<server>` and marks the event to avoid gateway loops. Ordinary MCP history reads reuse the current owner `access_token` for Matrix Client-Server history and never create or refresh a Matrix session, so they cannot evict the owner's active clients. MCP message history exposes Matrix sender identity fields, and room member listing returns member identities only for known Dirextalk product rooms or conversations, enriching stale projections from Matrix member/profile state without exposing arbitrary roomserver state. `agent.config.get/update` persists `avatar_url` for client display and `mcp_blocked_room_ids` for MCP room access control; blocked rooms are filtered out of MCP room search, and MCP reads/writes that directly target a blocked room or a post in one return 403. Native Agent built-in Dirextalk tools and `POST /mcp` share the same `internal/dirextalkmcp` registry, schemas, pagination, room authorization, DTOs, errors, and invocation service. Fixed `mcp.*` body actions are removed from product action routing; remaining `mcp.*` strings are internal capability action IDs.

Plugin management actions are owner-only for Ops and future non-Agent plugins. Native Agent uses first-class owner `agent.*` actions and `client.native_agent_stream` realtime frames, and `io.dirextalk.agent` is not returned or accepted by plugin catalog/list/lifecycle/config/invoke/health/log surfaces. Ops and future non-Agent plugins still use official Docker images from the `dirextalk` Docker Hub organization; digest metadata is optional and used only when explicitly present. The message-server keeps durable plugin/job/config state for non-Agent Docker plugins and runs those operations from the single-node `docker-compose.p2p.yml` deployment, which enables `P2P_PLUGIN_DOCKER_ENABLED=true`, mounts `/var/run/docker.sock`, and sets `P2P_PLUGIN_DOCKER_NETWORK=dirextalk-p2p_default`. Ops receives `DIREXTALK_BASE_URL`, `OPS_BACKUP_ROOT`, `OPS_MAX_BACKUPS`, `OPS_MESSAGE_SERVER_CONTAINER`, `OPS_POSTGRES_CONTAINER`, `OPS_POSTGRES_USER`, and `OPS_POSTGRES_PASSWORD`; it is the only official plugin allowed to mount Docker socket and the Ops backup volume.

The independent Agent owns its immutable, secret-free model-profile catalog and durable selection. Owner clients read and update only the catalog ID and bounded public tuning through HTTP-only, no-store `agent.runtime.profile.get/update`; both actions expose the exact de-secreted `{available, configured, revision, available_profile_ids, profile}` state and never expose owner or credential facts. Remote Chat rejects request-scoped `model_profile_id` and `model_profile`. The Message Server's Agent service key requires `runtime.read`, `runtime.write`, and `runtime.chat`, while the model credential enters Agent only from an operator-provisioned read-only mounted secret file. An initial selection creates the baseline of 48 context messages, 12 memory messages, and 24 steps. An ambiguous client update is reconciled through GET; clients never rebuild and blindly replay a mutation from stale state.

The local Runner retains legacy request-local model profiles only as compatibility while it remains supported. Its config, including shared display/avatar settings, `mcp_blocked_room_ids`, skills, third-party MCP client metadata, runtime CLI metadata, and memory settings, is stored in native portal Agent config JSON; any sanitized legacy model-profile fields are compatibility-only. Startup performs an idempotent sanitized import from old hidden `io.dirextalk.agent` plugin config when present; API key and API key ref fields are stripped during import and on local config save/load. Runtime CLI files, installed skill files, MCP working data, local server-side memory, and context-compression artifacts live in the native Agent data dir configured by `P2P_NATIVE_AGENT_DATA_DIR`. Agent knowledge action names are reserved but first-version runtime reports unsupported and does not load vector index dependencies. The backend keeps the standard `POST /mcp` endpoint for agent-token MCP access. Ops owns status, backup, migration export, cleanup planning, backup progress polling, chunked backup download, and explicit confirmed restore from existing backup records. Direct SQL purge of Matrix event history is not part of the first-version Ops contract.

当前 x1 本机部署固定落在单节点 `docker-compose.p2p.yml` 的 `message-server` 容器上，插件内网回调地址使用 `http://message-server:8008`。三节点 `dendrite-a`/`b`/`c` 仅用于 multi-node regression，不承载 x1 的实际对外域名，也不安装或承载插件。

Owner clients read Agent bridge online state from native Matrix room state in the real `agent_room_id`: event type `io.dirextalk.agent.status`, state key `@agent:<server>`, and content field `online`. The running local bridge writes `online=true/false` through its Matrix session. The server may write `online=false` when it creates or repairs the agents room and when config is disabled, but it does not write `online=true` from config or WS state. `sync.bootstrap` only returns `agent_room_id`; it does not mirror the online bit, and WS `server.event` does not emit `agent.presence`. `agent.status`/`agents.status` are removed; configuration details stay under `agent.config.get`.

## Verification

Backend:

```bash
go test ./p2p ./internal/httputil ./setup -count=1
go build ./cmd/dirextalk-message-server
docker compose -f docker-compose.p2p.yml config
docker compose -f docker-compose.p2p-dual.yml config
codegraph status
```

This includes route-envelope tests, `TestDatabaseStoreRestoresPortalAndBusinessState`, transport tests, projector tests, and business-state tests. The tests verify portal auth tokens, profile, contacts, groups, channels, posts, comments, reactions, members, calls, favorites, and follows survive service reconstruction against the same database API; verify that group/channel/member/product redaction actions use the configured Matrix transport; verify removed P2P message/search/backup actions are not present; verify invite/join/leave/remove/mute and channel join-request member lifecycle state; and verify that inbound Matrix room events and roomserver redaction outputs are projected into P2P product tables where appropriate.

Additional targeted regression checks cover default portal initialization, setup-free startup, credential JSON writing and password-rotation refresh, password rotation, Agent password/config/status actions, Agent API permission enable/disable enforcement, contact list/request deletion, favorite batch deletion, owner channel comment history, Matrix-only ordinary message handling, and syncapi local hidden-event filtering.

Group and channel moderation actions now persist product-level `muted` state and update ordinary joined members in the target room. Owners are excluded from whole-room mute toggles. `groups.invite_policy.update` persists `invite_policy` and survives PostgreSQL-backed service reloads.

Product metadata updates are field-preserving: `groups.update` and `channels.update` first load the current record and only overwrite mutable fields present in the action params. Channel type is legacy creation metadata and `channels.update` ignores `channel_type`; current channel behavior is unified post+chat regardless of stored `chat` or `post`. Public channel detail lookup is read-only and returns `404` for missing or private channels instead of creating placeholder records.

Public profile/channel lookup behavior:

- `channels.public.get` is read-only and returns `404` for private channels.
- `channels.public.join_request` can be called without bearer. Approval-policy channels store a local `pending` member record; open channels and owner approval write approved state and then trigger Matrix join locally or through requester-node `channels.public.join_result`.
- `users.public_channels` can be called without bearer and returns only channels where the target user has owner membership and the channel visibility is `public`. When `remote_node_base_url` is present, the local node forwards the public query to that owner node. The response includes `avatar_url`, `name`, `room_id`, `channel_id`, `member_count`, and the normal channel metadata consumed by the client.

Group/channel member behavior:

- `p2p_members.joined_at` stores first seen join/order time and is indexed by room/channel.
- `groups.members` and `channels.members` return visible members ordered by `joined_at, user_id`.
- Member rows include `avatar_url`, `display_name`, `role`, `membership/status`, and `muted`; owner profile changes publish `m.room.member` state and update local owner member rows.
- Channel owners use the same `channels.members` action and see the same sorted member settings/status as group owners.

Post deletion behavior:

- `channels.posts.recall` removes the post projection from `p2p_channel_posts`, sends Matrix redaction when a transport/event is available, and the deletion survives service reload.
- Ordinary room local delete is Matrix-local hiding through `/_matrix/client/v1/io.dirextalk/rooms/{roomID}/local_delete`; it filters this user's Matrix read paths and does not synchronize to other users or nodes. Distributed recall uses Matrix redaction.

Call detail lookup is also read-only: `calls.get` returns `404` for unknown calls and does not create sessions. `calls.event` only updates existing calls and accepts terminal/progress events `connected`, `ended`, `rejected`, `missed`, and `failed`; missing calls return `404`. Call records persist `answered_at`, `ended_at`, `ended_by_mxid`, `end_reason`, and `duration_ms`, and every call write emits a `call.changed` P2P event whose payload contains the latest call record under `call`. Once a call reaches `ended`, `rejected`, `missed`, or `failed`, stale writes with the same `call_id` do not reopen it.

Live smoke:

- Started the `docker-compose.p2p.yml` stack from the local `postgres:18` image and the locally built Dirextalk Message Server image.
- Verified the database version as `PostgreSQL 18.4`.
- Generated persisted Dirextalk Message Server config/key material through the one-shot `dendrite-init` service.
- Started the integrated Dirextalk Message Server service on `127.0.0.1:8008`.
- Verified `GET /_p2p/health` returned `{"status":"ok"}`.
- Verified body-action API calls through `POST /_p2p/command` and `POST /_p2p/query` for `portal.bootstrap`, `profile.get`, and `groups.create`.
- Queried PostgreSQL 18 and verified `p2p_portal` and `p2p_groups` rows were written.
- Restarted Dirextalk Message Server and verified `portal.auth` and `groups.list` restored the owner session and group state from PostgreSQL 18.
- Verified PostgreSQL contained 13 `p2p_%` tables.
- Verified PostgreSQL contained 21 `p2p_*_idx` business indexes.
- Rebuilt the Docker image after the Matrix-local-delete migration and verified local hide filters the requesting user's Matrix timeline/search results without Matrix redaction or P2P message rows.
- Added regression coverage for channel public join requests: pending request persistence, approve-to-invite, reject hiding from normal member lists, and Matrix invite emission during approval.
- Rebuilt the Docker image after the join-request refactor and verified `channels.public.join_request` writes `pending` or approved/joining state, `channels.join_request.approve` auto-joins through Matrix when the requester node is reachable, and `channels.join_request.reject` records `reject` while notifying the requester node.
- Confirmed startup logs no longer contain `P2P integrated AS store unavailable`.
- Started `docker-compose.p2p-dual.yml` and verified two independent PostgreSQL 18-backed Dirextalk Message Server instances can federate locally.
- Verified `portal.auth` returns a real Matrix access token and Matrix `/sync` returns `200` plus a `next_batch` token.
- Created a channel on instance A, joined from instance B with `server_names: ["dendrite-a:8448"]`, and verified B backfilled native channel profile projection plus both members.
- Updated B profile through `profile.update` with a new nickname and avatar URL. Instance A projected the remote `m.room.member` update and returned the new display/avatar in `channels.members`; instance B also returned both owner and remote-self members by both `room_id` and `channel_id`.
- SQL verification on A showed `p2p_members` rows for both `@owner:dendrite-a:8448` and `@owner:dendrite-b:8448`, with B's updated `display_name` and `avatar_url`.
- SQL verification on B showed the projected `p2p_channels.channel_id` for the remote room and two `p2p_members` rows sharing that `channel_id`.
- Verified browser CORS preflight for `/_p2p/command` returns `204` with `Access-Control-Allow-Origin` echoing the frontend origin and credentials enabled.
- Initialized CodeGraph for the new backend checkout. Current index status: 752 files, 13,967 nodes, 44,381 edges, up to date.

Current targeted verification for the latest migration round:

```bash
go test ./p2p -count=1
flutter test test/http_as_client_test.dart test/contact_home_relationship_test.dart
```

This verifies public channel actions without bearer, user public-channel lookup, private-channel filtering, member avatar/join-order sorting, persisted post recall deletion, credential file writing/update, unified public query mapping, and visitor profile public-channel rendering.

2026-06-19 regression and coverage audit:

```bash
go test ./p2p
export P2P_DUAL_PUBLIC_HOST="${P2P_DUAL_PUBLIC_HOST:-host.docker.internal}"
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python3 scripts/p2p-three-node-regression.py
flutter analyze lib/data/as_client.dart lib/presentation/providers/auth_provider.dart test/auth_provider_test.dart test/http_as_client_test.dart
flutter test test/auth_provider_test.dart --plain-name "portal login"
flutter test test/auth_provider_test.dart --plain-name "password change"
flutter test test/auth_provider_test.dart --plain-name "new device login uses its own device and old device expires"
flutter test test/widget_test.dart --plain-name "new friend badge counts AS pending friend request notices"
flutter test test/widget_test.dart --plain-name "new friends page refreshes AS pending notices after Matrix sync"
flutter test test/channel_search_page_test.dart
flutter test test/contact_home_relationship_test.dart
flutter test test/widget_test.dart --plain-name "group info edits remark, pins, nickname, and clears room history"
flutter test test/widget_test.dart --plain-name "private chat recalls own message through AS"
flutter test test/widget_test.dart --plain-name "group chat recalls own message through AS"
flutter test test/widget_test.dart --plain-name "contact detail updates remark without dialog disposal crash"
flutter test test/http_as_client_test.dart --plain-name "joinChannel"
flutter test test/http_as_client_test.dart --plain-name "Channel"
flutter test test/channel_page_real_test.dart
```

2026-06-19 final requirement audit:

```bash
gopls version
go test ./p2p -run "TestProjectDirectInviteCreatesPendingInboundContact|TestProjectOutputNewInviteCreatesPendingInboundContact|TestContactRequestCreatesDirectInviteRoomThroughTransport|TestContactRemarkUpdatePersistsAfterReload|TestContactRequestPreservesPeerDomainWithPort|TestAgentConfigContactsFavoritesAndSyncMessagesActions" -count=1
go test ./p2p -run "TestGroupsAndChannelsExposeOwnerMember|TestGroupAndChannelMemberLifecycle|TestGroupAndChannelWideMuteAndInvitePolicyActions|TestGroupProfileUpdatePreservesFields|TestKickedGroupMemberCannotRejoinButLeaverCan|TestGroupMemberLeaveActionCanRejoin|TestMembersListIncludesAvatarsAndJoinOrder|TestGroupJoinCreatesLocalGroupRecord|TestServiceUsesTransportForMemberLifecycle|TestJoinRefreshesCurrentRoomMembersFromTransport" -count=1
go test ./p2p -run "TestChannelJoinRequestPersistsPendingMemberAndResolves|TestPublicOpenChannelJoinRequestAutoJoins|TestPrivateChannelRejectsPublicJoinRequest|TestKickedChannelMemberJoinRequestRejectedButLeaverCanReapply|TestChannelMemberLeaveActionCanReapply|TestRemotePublicChannelJoinRequestForwardsToOwnerNode|TestChannelJoinRequestApprovalInvitesThroughTransport|TestChannelJoinRequestResolutionReturnsChannelForClientRefresh" -count=1
flutter test test/widget_test.dart --plain-name "new friend badge refreshes AS pending notices after Matrix sync"
flutter test test/widget_test.dart --plain-name "new friends page refreshes AS pending notices after Matrix sync"
flutter test test/widget_test.dart --plain-name "new friends page exposes accept and reject actions"
flutter test test/widget_test.dart --name "group creation invites only selected accepted contacts|group detail invites accepted non-members through AS|group info invite button posts member invites through AS|group info edits remark, pins, nickname, and clears room history|group management edits group name through AS|group management updates invite policy through AS"
flutter test test/channel_search_page_test.dart
flutter test test/channel_page_real_test.dart --name "channel detail loads public AS channel when not in bootstrap|channel detail loads public AS channel by room id|channel detail passes remote node URL for local dual node room id|owned channel member avatar opens visitor public channels|channel leave and dissolve use shared confirm dialog|owned channel member management renders members tab"
flutter test test/contact_home_relationship_test.dart --name "visitor home renders public channels returned by AS|visitor public channel opens channel detail for joining"
```

The audit covers the user's 11 explicit acceptance areas as follows:

- Device switching and single-session behavior: `auth_provider_test` portal login/password-change/new-device tests verify fresh-device recovery, old-device expiration, and no key-upload failure loop.
- Friend requests: the legacy full smoke recorded A->B `contacts.request`, B `sync.bootstrap.pending.friend_requests`, accept on B, and accepted contact state; Flutter widget tests verify B-side pending notice badge and refresh after Matrix sync.
- Group creation/invites/info: the legacy full smoke recorded `groups.create`, `groups.update`, `groups.invite`, `groups.join`, `groups.members`, mute policy, member mute/remove, member leave restrictions, owner leave rejection, and owner-only `groups.dissolve`; Flutter group info tests verify invite, remark/pin/nickname, clear-history, member removal UI paths, and owner dissolve UI calling the AS action.
- Public/private channels: the legacy full smoke recorded public room-id lookup/search across A/B, open and approval join policies, approval/rejection, invite flow, private-channel public join rejection, member listing, owner removal rejection, member leave/removal behavior, removed-member reapply rejection, and owner-only `channels.dissolve`. Flutter channel search and channel page tests cover room-id discovery, local dual-node room-id target mapping, public details, owner/member role UI, member management, dissolve UI, and visitor public-channel navigation.
- Channel post surfaces: the legacy full smoke recorded post create/list/recall, comment create/list/recall, comment replies, mentions metadata, reactions, favorites, and per-user comment/reaction history. Flutter channel page and HTTP client tests cover post publish, owner recall, like/reaction, detail comments, collapsed comments, comment/reaction APIs, and history parsing.
- Chat message deletion/recall: the legacy full smoke recorded ordinary messages send/read through Matrix, Matrix `local_delete` staying local, and Matrix redaction propagation across nodes. Client private/group recall and clear-history flows should call Matrix redaction and Dirextalk Matrix `local_delete`, not P2P message actions.
- Contact remarks and profile surfaces: the legacy full smoke recorded `contacts.update` after B accepts A's friend request and verifies the persisted remark through `contacts.list`; Flutter tests verify remark update without dialog disposal issues, visitor public-channel list rendering, accepted-contact delete action, Matrix member nickname/avatar refresh, and channel/member avatar navigation to visitor public channels.
- Deleted/rejected relationship rules: the legacy full smoke recorded deleted-contact auto-reject, rejected direct messages, removed channel/group member auto-reject/rejoin rejection, and owner-only dissolve restrictions.

One intentional correction from this audit was test-only: `channel owner does not see report action from role` now asserts the current title text `频道信息(2)` instead of the stale text `产品公告（2）`; the business assertions that owners do not see report/leave actions remain unchanged.

Multi-node regression:

PowerShell:

```powershell
$env:P2P_DUAL_PUBLIC_HOST = if ($env:P2P_DUAL_PUBLIC_HOST) { $env:P2P_DUAL_PUBLIC_HOST } else { "host.docker.internal" }
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python scripts/p2p-three-node-regression.py
```

Bash:

```bash
export P2P_DUAL_PUBLIC_HOST="${P2P_DUAL_PUBLIC_HOST:-host.docker.internal}"
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python3 scripts/p2p-three-node-regression.py
```

The Python regression authenticates local nodes from `/var/dirextalk-message-server/p2p/bootstrap.json`, updates profiles, exercises contact request/accept/delete/re-add behavior, group invite/join/projection, ordinary Matrix message history, channel capability projection, and restart recovery across the local multi-node topology.

The legacy `scripts/p2p-dual-smoke.ps1` previously carried broader static action coverage for all 93 P2P actions and 86 permission-controlled API actions. Do not use it as the default validation path; port any required missing coverage to the Python regression before relying on it for current validation. Invite and join-approval paths must use real portal owners from the compose topology; fabricated MXIDs are not valid Matrix users and can make federation resolve an invalid destination.

Latest legacy full-smoke image:

```text
dirextalk/message-server@sha256:884571b0c8fc46dbba63445ffe4d2b281942edf3a89f4387d4eeb187a307bdc1
```

Latest legacy full-smoke result shape:

```text
status=ok suffix=<run-suffix> image=<local-or-pushed-image> p2p_actions=93 api_actions=86
```

Frontend:

```bash
flutter analyze lib/presentation/providers/auth_provider.dart lib/core/router/app_router.dart lib/data/as_client.dart lib/data/http_as_client.dart test/app_router_test.dart test/http_as_client_test.dart test/auth_homeserver_resolution_test.dart
flutter test test/app_router_test.dart test/http_as_client_test.dart test/auth_homeserver_resolution_test.dart
flutter build web --dart-define=P2P_MATRIX_MOCK_AUTH=false
```

Browser smoke:

- Served the production Web build locally and opened `http://127.0.0.1:3001/?hs=http%3A%2F%2F127.0.0.1%3A18008#/login` in the in-app browser.
- Logged into instance A with `dual-a-secret` through the real `/_p2p/command` auth endpoint.
- Verified the browser navigated to `#/home` and rendered the unchanged Chats screen.
- The only browser console error observed after login was Matrix SDK encryption initialization for missing Web Olm (`Olm is not defined`); non-encrypted Matrix sync and product API auth still completed.

## Dragonfly/Redis Assessment

Current recommendation: do not add Dragonfly or Redis for this architecture yet.

Reasoning:

- The target deployment model is one backend owner user per instance, with many groups/channels connected through Matrix. That keeps write ownership simple and keeps product state naturally local to PostgreSQL plus Dirextalk Message Server's existing roomserver/sync paths.
- The hot product queries are relational and now indexed: message timelines by `room_id/origin_server_ts`, channel posts/comments by channel/post/time, member lists by room/channel/membership/join order, reactions by target/user, calls by room/state, and contacts/favorites by lookup keys.
- Adding Redis/Dragonfly would introduce another persistence/cache-invalidation plane for limited gain unless measured pressure appears from high fanout unread counters, presence-like ephemeral state, rate limiting, queues, or expensive cross-room aggregate feeds.
- For the current feature set, PostgreSQL 18 indexes plus in-process projection are simpler and more reliable. The next optimization step should be measurement first: `pg_stat_statements`, slow query logs, endpoint latency histograms, and row-count/load tests for large channel/member/message tables.

Revisit Dragonfly/Redis only if measured data shows one of these:

- repeated read amplification from cross-room aggregate feeds that cannot be solved with PostgreSQL indexes or materialized tables;
- high-frequency ephemeral state such as typing, presence, online counters, or short-lived locks where PostgreSQL writes become wasteful;
- distributed rate limiting or background job queues across multiple app processes;
- p95/p99 endpoint latency dominated by PostgreSQL despite correct indexes and query plans.
