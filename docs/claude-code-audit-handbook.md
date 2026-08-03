# Claude Code Audit Handbook

Last updated: 2026-07-09

This handbook is an executable audit guide for running Claude Code against the Dirextalk Message Server repository. It is intentionally written as an audit playbook rather than a design proposal. The goal is to find business-logic vulnerabilities, code-level vulnerabilities, contract drift, documentation staleness, test gaps, and review blind spots without changing product behavior by accident.

## Scope

Audit the repository as one Dirextalk Message Server monolith:

- Matrix-compatible homeserver behavior under `/_matrix/*`, `/_synapse/*`, `/_dendrite/*`, federation, sync, media, roomserver, userapi, and storage.
- Dirextalk product action surface under `POST /_p2p/query`, `POST /_p2p/command`, `GET /_p2p/ws`, `POST /mcp`, `GET /_p2p/health`, and `GET /.well-known/portal/owner.json`.
- Native Agent, standard MCP endpoint, plugin compatibility actions, Docker/Ops plugin boundaries, product projections, product policy, Matrix state events, Matrix transport writes, and documentation artifacts.

Do not treat P2P, Matrix, Agent, MCP, storage, and docs as separate silos. For each finding, trace the full path from entry point to auth, state transition, Matrix write/read, projection, persistence, sync/federation visibility, docs, and verification.

## Open-Source Reference

Use these references during the audit:

- Primary reference: [element-hq/synapse](https://github.com/element-hq/synapse). It is the actively maintained Matrix homeserver reference point for operational maturity, Matrix semantics, admin/security surface, database migrations, and documentation discipline. As of the checked source page, Synapse is an open source Matrix homeserver maintained by Element, with latest GitHub release `v1.156.0` on July 7, 2026.
- Lineage reference: [element-hq/dendrite](https://github.com/element-hq/dendrite). It is the Go Matrix homeserver lineage this fork comes from. Use it for inherited architecture and upstream behavior comparison, but do not use it as the sole maturity benchmark because the checked page says Dendrite is in maintenance mode and its latest listed release is `0.15.2` on August 15, 2025.
- Protocol reference: [Matrix Specification latest](https://spec.matrix.org/latest/), currently showing specification version `v1.19` on the checked page. Pay special attention to event trust, room IDs, federation, membership, device/session semantics, Client-Server API, Server-Server API, and Push Gateway API.
- MCP reference: [Model Context Protocol Streamable HTTP transport 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports) and [MCP authorization 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization). Use these for `/mcp` Origin validation, POST/GET behavior, bearer-token handling, and token audience/confused-deputy checks.
- Claude Code reference: [Claude Code overview](https://code.claude.com/docs/en/overview), [hooks guide](https://code.claude.com/docs/en/hooks-guide), and [permissions guide](https://code.claude.com/docs/en/permissions). Claude Code can read code, edit files, run commands, use `CLAUDE.md`, hooks, permissions, MCP, and multi-agent workflows. For this audit, prefer read-only review prompts unless the task explicitly asks Claude Code to patch.

Important license note: Synapse and current Dendrite repositories are AGPL/commercial dual-licensed. Use them as behavioral and review references. Do not copy code into this repository unless licensing has been explicitly approved.

## Repository Fact Sources

Claude Code must treat these files as the local source of truth before drawing conclusions:

- `AGENTS.md`: project-wide rules and current business constraints.
- `docs/current-project-documentation.md`: current product and architecture fact source.
- `docs/api-interface-change-record.md`: historical contract changes; current sections at the top supersede older entries.
- `docs/product-action-contract.json`: generated product action auth and transport artifact.
- `p2p/serviceapi/actions.go`: canonical product action metadata source.
- `p2p/action_registry.go`: action handler registration.
- `p2p/routing.go`: HTTP body-action routes, bearer auth, CORS, and action dispatch.
- `p2p/realtime_ws.go`: owner WS ticket, session state, product event streaming, WS request handling, native agent stream frames, and push suppression context.
- `p2p/routing_mcp.go`: standard MCP Streamable HTTP endpoint.
- `internal/dirextalkmcp/service.go`: MCP tool registry and capability action mapping.
- `internal/productpolicy`: Matrix Client-Server write policy for Dirextalk rooms.
- `internal/dirextalktransport` and `internal/dirextalktransport/dendrite`: product-originated Matrix writes.
- `internal/dirextalkmatrix`: Matrix Client-Server profile/history readers used by MCP and channel backfill.
- `p2p/storage` and package storage migrations: durable product read models and restart recovery.

## Claude Code Safety Setup

Run audit sessions from the repository root:

```powershell
cd C:\Users\84960\Desktop\dirextalk\dirextalk-message-server
git status --short
claude
```

Recommended Claude Code operating rules:

- Start in read-only review mode. Do not let the first audit pass edit files.
- Ask Claude Code to produce evidence with file and line references for every finding.
- Ask it to distinguish confirmed bugs from suspicious patterns and unresolved questions.
- Keep destructive commands blocked or ask-only in Claude Code permissions.
- If using hooks, configure deterministic checks such as `gofmt`, `go test`, `git diff --check`, JSON validation, and forbidden-command guards.
- Split work into focused sessions. This repository is too large for a single reliable pass.

Suggested branch/worktree pattern:

```powershell
git switch -c audit/claude-code-YYYYMMDD
```

For patching later, create one branch per accepted finding cluster. Do not let a broad audit branch become a mixed refactor branch.

## Master Audit Prompt

Paste this into Claude Code first:

```text
You are auditing the Dirextalk Message Server repository. Work in read-only review mode unless I explicitly ask for fixes.

Read AGENTS.md, docs/current-project-documentation.md, docs/api-interface-change-record.md, docs/product-action-contract.json, p2p/serviceapi/actions.go, p2p/action_registry.go, p2p/routing.go, p2p/realtime_ws.go, p2p/routing_mcp.go, internal/dirextalkmcp/service.go, internal/productpolicy, internal/dirextalktransport, internal/dirextalkmatrix, and p2p/storage before conclusions.

Audit across the full path: route or Matrix entry point, auth, business rule, Matrix write/read, storage, projection, websocket/sync/federation visibility, docs, tests, and generated artifacts.

Prioritize:
1. Business-logic vulnerabilities and privilege escalation.
2. Token, secret, MCP, Native Agent, plugin, and Docker boundary bugs.
3. Matrix state/source-of-truth violations.
4. Cross-node public channel and contact recovery edge cases.
5. Storage durability, restart recovery, and migration problems.
6. Documentation and generated contract drift from code.
7. Missing tests for current public contracts.

For each finding, output:
- Severity: Critical, High, Medium, Low, or Info.
- Title.
- Evidence: file path and exact line references.
- Impact: concrete user/business/security consequence.
- Reproduction or reasoning path.
- Expected behavior from AGENTS.md/current docs.
- Suggested focused verification.
- Whether this is confirmed, likely, or a question.

Do not report a public action as a vulnerability merely because it is public. Check whether it is intentionally public and whether it still validates room ID, remote_node_base_url, sender identity, membership, and callback semantics.

Do not propose broad refactors. If a cleaner design requires wider change, report the tradeoff separately.
```

## Audit Passes

Run the following passes as separate Claude Code sessions or subagents. Each pass should produce its own Markdown report under `docs/audit/` or a temporary local note, then a final lead report should deduplicate findings.

### Pass 1: Contract And Route Drift

Objective: prove that every exposed action and route matches current docs and generated artifacts.

Claude prompt:

```text
Audit product action and route contract drift.

Compare p2p/serviceapi/actions.go, p2p/action_registry.go, p2p/routing.go, p2p/realtime_ws.go, p2p/routing_mcp.go, docs/product-action-contract.json, docs/current-project-documentation.md, and docs/api-interface-change-record.md.

Find any action whose auth, transport, handler registration, HTTP/WS availability, generated contract metadata, or docs disagree. Pay special attention to removed fixed mcp.* body actions, agent.matrix_session.create, realtime.ws_ticket.create, portal.account.delete, rooms.reactivate, channels.public.join_result, Native Agent stream actions, plugin compatibility actions, and public channel actions.
```

Review questions:

- Does every `serviceapi.ActionSpec` have exactly one expected handler except the special WS ticket action?
- Does every handler have metadata?
- Are `http_only`, `http_and_ws_request`, `ws_stream_only`, and `internal_only` enforced consistently in HTTP and WS?
- Are public actions exactly the current documented set?
- Is `docs/product-action-contract.json` stale compared with `p2p/serviceapi.ActionSpecs`?

Relevant local checks:

```powershell
go test ./p2p ./p2p/serviceapi -count=1
go run ./cmd/dirextalk-action-contract | Out-File -Encoding utf8 docs/product-action-contract.generated.json
git diff --no-index docs/product-action-contract.json docs/product-action-contract.generated.json
Remove-Item docs/product-action-contract.generated.json
git diff --check
```

### Pass 2: Auth, Token, Session, And CORS Boundaries

Objective: catch privilege escalation and token confusion.

Claude prompt:

```text
Audit auth, token, session, and CORS boundaries.

Trace Service.Authorize, publicAction, AgentAction, bearerToken parsing, HTTP route auth, WS ticket creation/consumption, MCP bearer auth, Matrix session issuance, portal auth/password/session refresh, account deletion, plugin actions, and Native Agent actions.

Find cases where an agent_token, public action, WS ticket, Matrix access token, query-string token, model API key, plugin secret, or MCP bearer token can be used outside its intended audience.
```

High-risk checks:

- `agent_token` must only authorize product body-action `agent.matrix_session.create` and standard `POST /mcp`.
- `agent_token` must not create realtime WS tickets.
- `GET /_p2p/ws` must authenticate a short-lived single-use owner WS ticket, not a bearer token.
- Owner `access_token` must not be accepted by `/mcp` when the current contract says agent token only.
- `/mcp` must reject tokens in query strings.
- `/mcp` must validate `Origin` when present and return 403 for invalid origins.
- MCP inbound bearer token must not be passed downstream to tool calls.
- Model provider API keys must stay request-scoped and must not be persisted, returned, logged, injected into plugin env, or saved in Native Agent config.
- Portal auth/password device-session behavior must evict only owner devices; `agent.matrix_session.create` must not evict owner phone/browser sessions.
- `portal.account.delete` must require `confirm="delete_account"`, owner access token, and must stop before destructive cleanup if critical dissolve/leave/deactivate steps fail.
- CORS echo with credentials should be reviewed against deployment assumptions. If broad origin support is intentional, document the operational boundary and verify auth still carries the risk.

### Pass 3: MCP And Native Agent Security

Objective: verify MCP and Native Agent cannot become an unbounded owner-equivalent automation channel.

Claude prompt:

```text
Audit the standard /mcp endpoint, internal/dirextalkmcp registry, p2p MCP adapter, Native Agent actions, runtime shell/install actions, MCP server install/enable/disable, skills install/enable/disable, and model provider request handling.

Use the MCP 2025-11-25 Streamable HTTP and authorization specs as reference. Find confused-deputy, DNS rebinding, token passthrough, prompt/tool injection, data exfiltration, room authorization, tool registry drift, dangerous shell/runtime install, and secret persistence bugs.
```

High-risk checks:

- `mcp_blocked_room_ids` must filter room search and reject direct room access with 403.
- Built-in Dirextalk MCP tools and Native Agent Dirextalk tools must share `internal/dirextalkmcp`; no forked business logic in Native Agent or HTTP transport.
- Channel posts/comments and ordinary channel chat must stay separate.
- MCP pagination must use `from_time`, `to_time`, `cursor`, and readable response fields, not old `ts` or `last_ts`.
- `dirextalk_messages_send` and comment creation must go through approved Matrix transport/product policy, not direct SQL.
- Runtime shell must be gated by owner auth and config, bounded by timeout and working directory, and must not leak server secrets.
- MCP server and skill installation should be constrained by registry/source trust, install path, overwrite behavior, and audit logs.
- Prompt-injected content from Matrix rooms, MCP results, docs, or runtime output must not silently instruct the Native Agent to reveal tokens or run dangerous tools.

### Pass 4: Matrix Product State And Policy

Objective: verify Matrix state remains the source of truth where required.

Claude prompt:

```text
Audit Matrix-native product state and product policy.

Trace direct contacts, groups, channels, posts/comments/reactions, account deletion, blocklist, reports, agent room, system room, push context, and Matrix Client-Server writes.

Find any place that bypasses p2p.Transport, writes Matrix SQL directly for product-originated room/member/state/message/redaction behavior, treats product projection as source-of-truth when Matrix membership/state should decide, or allows Matrix Client-Server writes to violate internal/productpolicy.
```

High-risk checks:

- Matrix `m.room.member membership=join` is final joined fact.
- Product roles are only `owner` and `member`.
- New group rooms set `m.room.history_visibility=joined`.
- New or bound channel rooms set `m.room.history_visibility=shared`.
- `channels.update` ignores `channel_type` for old-client compatibility and behavior must not branch on `chat` vs `post`.
- Ordinary timeline/media/history/search/redaction use Matrix Client-Server APIs, not old P2P message stores.
- Removed legacy product state must not be generated, read, or projected as current behavior.
- `agent_room_id` is a real private Matrix room. Agent bridge messages use Matrix sync/send/edit as `@agent:<server>`, not legacy pseudo events.
- `system_room_id` for reports should notify owner; do not install empty-action push rule for system room.
- Agent room should default to no system push through a room-level Matrix push rule with empty actions, preserving explicit rules.

### Pass 5: Cross-Node Business Flows

Objective: catch bugs that only appear with federation, remote callbacks, or node rebuild.

Claude prompt:

```text
Audit cross-node Dirextalk flows.

Focus on public channel remote lookup and join approval, contacts delete/reactivate, rooms.reactivate, rebuilt group/private-channel members, stale membership removal, reports.submit, remote_node_base_url validation, Matrix ID validation, server_names, federation visibility, and projection updates.

Find SSRF, private-host access, fake Matrix sender, premature joined projection, stale membership, callback spoofing, idempotency, and restart recovery bugs.
```

High-risk checks:

- Remote public lookup must validate Matrix IDs and require caller-provided `remote_node_base_url`; never derive outbound URLs from Matrix room IDs.
- Reject malformed room IDs, URL-shaped server names, and untrusted private/internal hosts unless test config explicitly allows private hosts.
- Missing/private channels must not create placeholder records.
- Public channel lookup must be read-only.
- Public channel membership must not become `joined` until Matrix join succeeds.
- Remote approval must call the requester node and pass `server_names` for remote joins.
- Room IDs must preserve domains and ports.
- Rich channel metadata must not be overwritten by sparse federated defaults.
- Rebuilt group/private-channel member recovery must send real Matrix invite and pending notice, not silently join.
- Public channel rebuild recovery must use `channels.public.join_request` and `channels.public.join_result`.
- Direct contact recovery must preserve deleted direct room identity where possible and must not let profile metadata spoof the true Matrix sender.

Suggested regression:

```powershell
$env:P2P_DUAL_PUBLIC_HOST = if ($env:P2P_DUAL_PUBLIC_HOST) { $env:P2P_DUAL_PUBLIC_HOST } else { "host.docker.internal" }
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python scripts/p2p-three-node-regression.py
```

### Pass 6: Storage, Migrations, Restart Recovery

Objective: catch memory-only state and migration gaps.

Claude prompt:

```text
Audit durable storage and restart recovery.

Trace p2p/storage migrations, portal state, credentials file writing, plugin secrets, Native Agent config migration, MCP blocked rooms, reports, read markers, channel content/reactions/favorites, projection tables, and test databases.

Find behavior that must survive restart but exists only in memory, migrations that do not backfill or preserve existing data, secrets persisted in clear response/config paths, broken indexes, non-idempotent startup repair, and tests that do not cover PostgreSQL behavior.
```

High-risk checks:

- New durable behavior needs migrations and restart tests.
- Portal credentials file must preserve/update required state and not expose extra secrets beyond intended bootstrap file behavior.
- Native Agent migrated config must strip `api_key` and `api_key_ref`.
- Plugin secrets storage must not be returned in config APIs.
- Account deletion must clear configured local databases only after critical Matrix/product cleanup succeeds.
- Test databases must be isolated and dropped.
- PostgreSQL is the only supported server database. SQLite/file DSNs must be rejected rather than silently falling back to memory state.

### Pass 7: Code Vulnerability And Robustness

Objective: find ordinary code vulnerabilities and operational hazards.

Claude prompt:

```text
Audit code-level vulnerabilities and robustness across Go code, Docker, compose, Helm, scripts, and docs.

Look for injection, path traversal, unsafe archive extraction, SSRF, secret logging, unsafe command execution, missing timeouts, unbounded request bodies, goroutine leaks, race-prone shared maps, nil panics, context cancellation loss, weak random tokens, direct SQL product writes, malformed JSON assumptions, and missing authorization checks.
```

Focus areas:

- `remoteHTTPClient`, remote lookup, callback forwarding, and URL validation.
- Native Agent runtime shell and package install actions.
- Docker plugin runner, official Ops plugin, Docker socket mounts, backup/restore paths.
- File upload and knowledge upload placeholders.
- Media and archive extraction.
- HTTP clients without timeouts.
- WebSocket stream cancellation and per-session cleanup.
- Generated docs and route indexes.
- Use of `json.Number`, `map[string]any`, and type assertions around untrusted input.
- Use of Matrix event bodies, which the Matrix spec treats as untrusted and not guaranteed to have expected fields.

### Pass 8: Documentation Freshness

Objective: prove docs describe the current product and do not resurrect removed behavior.

Claude prompt:

```text
Audit documentation freshness.

Compare docs/current-project-documentation.md, docs/agent-mcp-current-contract.md, docs/agent-core-integration-development-contract.md, docs/api-interface-change-record.md, docs/dirextalk-message-server.md, docs/dirextalk-push-gateway.md, AGENTS.md, and .codex/skills/*/SKILL.md against current code.

Find stale references, removed endpoints/actions, wrong token rules, wrong WS fallback rules, outdated Agent/plugin/MCP descriptions, examples that cannot import or run, and missing updates required by current behavior.
```

Current known contract anchors:

- Fixed `mcp.*` body actions are removed from `/_p2p/query` and `/_p2p/command`.
- External MCP clients use `POST /mcp` JSON-RPC.
- `agent_token` is limited to product body-action `agent.matrix_session.create` and standard `POST /mcp`.
- Native Agent is first-class `agent.*`, not `io.dirextalk.agent` plugin management.
- Native Agent streaming uses dedicated WS frames, not legacy plugin stream or agent room stream frames.
- `portal.account.delete` is HTTP-only owner command.
- `GET /_p2p/ws` uses short-lived owner WS tickets only.
- `rooms.reactivate` and `channels.public.join_result` are public HTTP-only internal callbacks.
- Ordinary messages remain Matrix Client-Server backed.

## Severity Guide

Use this severity model:

- Critical: unauthenticated or low-privilege remote actor can obtain owner/agent secrets, run arbitrary server commands, destroy account data, bypass Matrix membership, or write as another user across nodes.
- High: authenticated wrong-role actor can escalate privileges, agent token can call owner actions, public callback can spoof membership/state, remote URL can SSRF private services, secrets persist/echo unexpectedly, or destructive cleanup can run without required confirmation/safety.
- Medium: contract drift that breaks clients, missing restart durability for product state, incorrect joined/pending projection, CORS/deployment risk needing config hardening, missing tests for high-risk boundaries, or docs that direct clients to removed behavior.
- Low: code style, naming, minor validation gaps without clear exploit path, confusing docs, or non-blocking operational issues.
- Info: questions, assumptions, improvement ideas, and upstream comparison notes.

## Finding Template

Claude Code should use this exact template:

```markdown
## [Severity] Title

Status: Confirmed | Likely | Question

Evidence:
- `path/to/file.go:123`: what the code does.
- `docs/current-project-documentation.md:45`: expected behavior.

Impact:
Concrete security, data integrity, business, or client compatibility consequence.

Reasoning:
Step-by-step path from entry point to outcome.

Expected:
The current intended behavior from AGENTS.md/current docs/protocol reference.

Verification:
Focused command or test scenario that would prove or disprove the issue.

Suggested fix shape:
Smallest safe change. Do not include broad refactors unless necessary.
```

## Common False Positives To Avoid

- Public actions are not automatically vulnerabilities. Some public actions are intended node-to-node callbacks or public discovery actions.
- Historical entries in `docs/api-interface-change-record.md` may describe removed behavior. Current dated entries and `docs/current-project-documentation.md` win.
- `mcp.*` strings may still exist as internal capability IDs inside `internal/dirextalkmcp`; that does not mean they are public product body actions.
- Plugin actions are retained for compatibility/future non-Agent plugins, but Native Agent is not managed as `io.dirextalk.agent`.
- Matrix room ID domains do not prove where a room is hosted or where to call HTTP. Remote product calls must use validated request-provided `remote_node_base_url`.
- Projection tables are not automatically sources of truth. Membership and many room facts come from Matrix state.
- Inherited Dendrite TODOs may be real maintenance items, but do not label them as current Dirextalk business bugs without a current path.
- A missing full `go test ./...` result is not itself a product bug because inherited demo/upgrade packages are outside the default build surface unless tagged.

## Expected Audit Outputs

The final Claude Code audit report should include:

- Executive summary with top 5 risks.
- Confirmed findings table sorted by severity.
- Likely issues needing reproduction.
- Explicit no-finding areas for high-risk boundaries that were checked.
- Contract drift matrix covering code, generated JSON, and docs.
- Test gap list with suggested focused tests.
- Docs update list.
- Commands run and results.
- Open decisions that require business input.

Minimum report table:

```markdown
| Severity | Area | Finding | Evidence | Verification | Owner decision needed |
| --- | --- | --- | --- | --- | --- |
| High | MCP auth | ... | `p2p/routing_mcp.go:...` | `go test ...` | No |
```

## Baseline Verification Commands

Run focused commands first:

```powershell
gofmt -w <touched-go-files>
go test ./p2p ./p2p/serviceapi ./internal/dirextalkmcp ./internal/productpolicy -count=1
go test ./internal/httputil ./setup -count=1
go build ./cmd/dirextalk-message-server
git diff --check
docker compose -f docker-compose.p2p.yml config
docker compose -f docker-compose.p2p-dual.yml config
```

Run broader checks when environment allows:

```powershell
govulncheck ./...
```

Do not include inherited Dendrite demo and upgrade-test tools in default `./...` conclusions unless intentionally testing with the required tags.

## Audit Problem Scenarios

Use these scenarios to force concrete reasoning:

- Agent-token escape: an external MCP client gets `agent_token` and attempts owner product actions, WS ticket creation, plugin actions, Native Agent runtime install, and Matrix owner session creation.
- Public callback spoof: an attacker calls `channels.public.join_result` or `rooms.reactivate` directly with fabricated Matrix IDs and remote URL data.
- Remote lookup SSRF: a public channel request uses `remote_node_base_url` pointing to localhost, cloud metadata, Docker bridge, file-like schemes, internal hosts, or a URL-shaped Matrix room domain.
- Premature join: a public channel approval returns `joined` before requester-node Matrix join succeeds.
- Stale membership recovery: owner node still has old joined membership after member rebuild and tries to invite silently.
- Direct contact spoof: inbound direct invite carries profile fields that claim a different requester than the real Matrix sender.
- MCP blocked room leak: a blocked room appears in room search or direct `messages.list`, `room_members.list`, post/comment list, or send/create calls.
- Request-scoped key leak: `model_profile.api_key` appears in config save/load, plugin env, runtime env, logs, traces, docs, or API responses.
- Account delete partial failure: one dissolve/leave/deactivate step fails but the server still clears local DB or writes misleading deprovision state.
- Native Agent command execution: owner invokes chat/tool path that runs shell commands with unbounded timeout, unexpected working directory, dangerous env, or secret access.
- Docs resurrection: a client follows docs and calls removed `mcp.*` body actions or legacy agent stream frames.

## Follow-Up Fix Policy

After the audit, fix only confirmed or explicitly accepted likely findings. For each fix:

- Keep the patch narrow.
- Add or update tests at the owning boundary.
- Update contract-critical docs in the same change when API/auth/route/storage behavior changes.
- Regenerate `docs/product-action-contract.json` if `p2p/serviceapi.ActionSpecs` changes.
- Run focused verification and `git diff --check`.
- Commit the fix on a dedicated branch.
