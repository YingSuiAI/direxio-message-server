# Embedded Agent Control Contract

Status: authoritative since 2026-07-29
Migration source: `dirextalk-agent@0fdc0fa2f3836b8c419557ed4fea09bd2ac01669`

This document is the sole server contract for Agent task, confirmation,
schedule, remote MCP, AWS, workload and deployment-management behavior.
The former standalone Agent Core service is retired. Its repository is a
read-only migration source and is not a runtime, fallback or release target.

## 1. Topology and ownership

- `dirextalk-message-server` is the only persistent Dirextalk application
  process and the only published application image.
- PostgreSQL remains external infrastructure. Schema migration and Agent
  secret administration are one-shot commands from the same image.
- Agent task workers, confirmations, schedules, AWS control, workload
  providers, remote MCP execution and deployment reads run in-process.
- Execution Orchestration V2 permits a server-owned deterministic coordinator
  that reaches remote targets through typed transports, but that path is a
  contract-only, capability-gated design
  until its route, storage, executor and acceptance tests are enabled.
- There is no Agent Core address, token, CA, port, gRPC client, sidecar,
  Docker socket or Core Runner.
- The Message Server never falls back to an external Agent service.
- Online Agent remains the private Matrix Agent-room conversation. Native
  Agent remains `client.native_agent_stream`; neither uses the retired Core
  conversation stream.

All Agent control records are owner-scoped. Public actions first authenticate
the normal owner session, and no credential or plaintext secret is returned
through ProductCore, realtime events, logs or deployment objects.

## 2. Backend discovery and actions

`agent.backends.get` always returns:

- `embedded`: the only available backend. Its capability list contains only
  dependencies that are ready at that instant.
- `core`: an unavailable compatibility projection. It cannot be selected and
  has no endpoint or health probe.

The possible embedded capability tokens are:

- `model_profiles.server`
- `model_roles.server`
- `memory.server`
- `task`
- `schedules.server`
- `confirmation`
- `mcp`
- `aws.control`
- `workload.aws_ssm`
- `workload.aws_ecs`
- `deployments.server`

`skill` and `workload.core_runner` are never advertised. Readiness is checked
again when every capability-owned action is invoked; discovery is not an
authorization or availability cache.

The existing `agent.core.*` action names remain wire-compatible management
names but invoke in-process modules directly:

- tasks and confirmations use the PostgreSQL Agent runtime;
- schedules use the one generic occurrence/task materializer;
- model profiles use the existing Message Server model-profile store;
- MCP accepts pinned HTTPS Streamable HTTP installations only;
- AWS and workloads use typed in-process providers;
- dashboard/deployments read the canonical workload operation/event tables.

`client.agent_core_stream` and every Core conversation request are rejected.
`client.native_agent_stream` remains the Native Agent transport. Server-managed
chat may carry `model_profile_id` and a complete
`model_profile_revision`/`credential_version` pair; the server pins that
immutable snapshot before durable execution. The public `POST /mcp` contract
is unchanged.

## 3. PostgreSQL runtime

The deployment migration registry owns one version each:

- v97: secret envelopes, key usage and model-credential compatibility;
- v98: task, event, replay, lease/concurrency, execution snapshot, model/tool
  rounds, confirmation and reservation state;
- v99: pinned MCP extension versions, secret revisions, execution receipts and
  uncertain outcomes;
- v100: AWS credential revisions and CloudFormation plans/changes/events;
- v101: workload plans, persistent quotes, workloads, operations and events;
- v102: generic schedule templates, occurrence/task links and deployment
  event cursors.

Duplicate migration versions are a startup error. Agent tables carry
`owner_id`, immutable revision/digest bindings, owner-scoped foreign keys and
idempotency constraints.

Workers claim due tasks with `FOR UPDATE SKIP LOCKED`. Every running mutation
is fenced by task revision, attempt, lease epoch, holder and expiry. A crashed
worker's expired task is transactionally requeued while its concurrency slot
is repaired; the successor receives a new lease epoch. Domain terminal state,
generic task terminal state, task event, confirmation-reservation release and
concurrency decrement commit in the same transaction.

Shutdown stops new claims, waits for the configured grace period, then lets
uncompleted work recover through lease expiry. It does not fabricate failure
after the grace period.

## 4. Schedules

There is one schedule engine:

- `agent.schedules.*` maps `prompt` and `model_profile_id` into a simple Agent
  task template and preserves the legacy `run` projection.
- `agent.core.schedules.*` preserves the full task template, active/paused
  state and typed trigger DTO.
- due and run-now triggers transactionally create one occurrence, one schedule
  run and one generic task.
- the Core-compatible trigger projection exposes the same
  `occurrence_id/task_id`; it does not invoke a second executor.

## 5. Secret keyring

`P2P_AGENT_SECRET_KEYRING_FILE` points to the version-1 keyring. Production
Compose uses:

`/var/dirextalk-message-server/agent/secret-keyring.json`

The directory is mode `0700`; the file is mode `0600`. A key is a random
32-byte AES-256 key. Exactly one key is active and older retained keys are
decrypt-only.

Every encrypted row stores `envelope_version`, `aad_version`, `key_id`, a
12-byte nonce and AES-GCM ciphertext including its authentication tag. The
canonical length-prefixed AAD binds secret domain, owner, entity,
revision/version, purpose/reference and binding digest.

Writes always use the active key. Reads select the row's exact key ID.
Unknown keys, authentication failure, wrong AAD or unsupported versions fail
the whole Agent secret subsystem closed. Agent secret-dependent capabilities
are omitted, while ordinary Matrix and ProductCore service remains available.

Model, AWS and extension credentials are immutable secret revisions. Tasks,
confirmations and workload records retain references and digests only.
Existing legacy model credentials are upgraded only by an explicit offline
`agent-secretctl upgrade` run. Before the migration, stop the persistent
service and make one restorable backup set containing PostgreSQL, the keyring,
and the legacy model-profile raw-key file. A serving process initializes an
absent v1 keyring after PostgreSQL migrations only when no keyring-bound
ciphertext exists; `agent-secretctl init` remains an optional explicit
preflight. Then run `upgrade` with
`P2P_AGENT_SECRET_KEYRING_FILE`, `P2P_AGENT_SECRET_DATABASE_DSN`, and
`P2P_AGENT_MODEL_PROFILE_KEY_FILE` supplied through the environment; key
material and database credentials are never command-line arguments. The
upgrade uses row locks and CAS and overwrites the shared model-profile nonce
and ciphertext columns with the v1 envelope and metadata; it does not clear
those columns. It is never run automatically by Compose or server startup.
Run `agent-secretctl verify` again with the legacy-key environment unset so
verification succeeds using only the keyring. If migration or verification
fails, rollback is an atomic restore of the PostgreSQL, keyring, and legacy-key
backup set, not an image-only rollback. Destructive v110 cleanup of legacy
compatibility material is deferred until fleet convergence and the rollback
window has closed.

The same image provides:

```text
agent-secretctl init
agent-secretctl upgrade
agent-secretctl verify
agent-secretctl rotate
```

Keys are read from the keyring file, never CLI arguments or PostgreSQL.
Rotation requires the persistent service to be stopped, is resumable, uses
row locks/CAS, and verifies the full database before an old decrypt-only key
may be removed.

## 6. AWS and workloads

AWS credentials are encrypted and revisioned; read APIs are strictly redacted.
Every plan/change pins an exact credential revision.

CloudFormation mutation and readback are typed. A lost or ambiguous response
enters reconciliation and performs typed describe/readback before any further
decision; external side effects are never blindly repeated.

Supported workload targets are:

- `AWS_EC2_SSM`: exact account/region, Linux EC2 identity, required tags, SSM
  Online state and the fixed `AWS-RunShellScript` document contract.
- `AWS_ECS`: exact cluster/network/task family/roles/target group and image
  digest, followed by independent typed readback.

`CORE_RUNNER` is rejected before side effects and is never advertised.
Quotes are PostgreSQL records with IDs and expiry. Workload events allocate
both per-operation sequence and public per-workload cursor under locked
counters; `MAX+1` is forbidden. Deployment dashboard/list/get/events read
`core_workloads`, `core_workload_operations` and `core_workload_events`
directly—there is no external reconciliation loop.

### Execution Orchestration V2 (contract gate; not live)

The normative decision record is
[`adr/2026-07-31-execution-orchestration-v2.md`](adr/2026-07-31-execution-orchestration-v2.md).

The server may include built-in declarative Planning Skills/Recipes. They are
trusted, versioned planning inputs only: each may produce immutable,
canonical, secret-free plan fragments bound to a content digest and revision
references, but may not execute shell/code/skills, call a provider, fetch
third-party content, or mutate state. GeoLibre is a fixture/recipe for this
boundary, not a product target or public contract.

The boundaries are fixed:

- the **planner** validates declarative requests and produces candidate plan
  fragments without credentials or side effects;
- the **compiler** merges and canonicalizes fragments into a typed stage graph
  and immutable plan digest, without dispatch or mutation retry;
- **policy** authenticates the owner and checks capability, target,
  credential/configuration revisions, risk, expiry, quota, and idempotency;
- the server-owned deterministic **coordinator** resolves only frozen typed
  plans/stages, dispatches them to remote targets through a versioned typed
  transport, and persists typed receipt/progress/readback/error evidence.

The Message Server does not execute local third-party shell/code/skills, expose
raw SSM/SSH/AWS passthrough, or provide a local fallback for the coordinator.
Native Agent confirmation cannot authorize an execution.v2 mutation. The
frozen plan and each mutating stage require an owner-scoped control-plane
confirmation containing the exact plan/stage digests, policy/revision facts,
expiry, and idempotency key. If dispatch is lost or ambiguous, the mutation is
recorded as `uncertain`; it is never replayed, sent over another transport, or
run locally. Only typed read-only reconciliation or explicitly authorized
destroy/recovery may proceed against that uncertainty.

V1 create remains until V2 acceptance is complete. It may be retired only
after the V2 route, durable storage, typed executor/transport, focused tests,
and deployment enablement all pass. V1 read, observe, reconcile, and destroy
remain retained for compatibility and recovery.

The first production slice is AWS SSM long-running services through the typed
coordinator. SSH, generic HTTP, DNS, TLS, and Coding Worker are deferred and
must not be advertised. `execution.v2.*` capabilities and actions are omitted
until their server route, storage, executor, tests, and enablement are all
ready; this contract does not claim any unimplemented phase is live.

## 7. Remote MCP and Skills

Only HTTPS Streamable HTTP MCP is accepted. stdio, local MCP, subprocesses and
third-party Skill execution are rejected before side effects. The built-in
declarative Planning Skills/Recipes described above are the narrow exception:
they can only emit immutable plan fragments and never execute a skill or
provider action.

Discovery is bounded to the fixed Official Registry, Smithery, Glama and
GitHub authorities. Candidate pins retain the public hyphenated values
(`official-registry`, `streamable-http`, `mcp-credential`) and are re-inspected
at install/update. The first successful remote `tools/list` transaction pins
canonical tool schema digests to the active immutable version; later drift is
a conflict, and execution rechecks the pinned list before `tools/call`.

An execution is bound to owner, installation, immutable version, content and
manifest digests, tool schema, network grants, secret grants and canonical
input digest. All deterministic validation finishes before confirmation
consumption. After consumption:

- success atomically writes the receipt and terminal task state;
- an ambiguous response atomically writes `uncertain`;
- a successor lease that finds an already-consumed confirmation records
  `uncertain` without replaying the remote tool.

`agent.core.skills.*` returns stable unavailable behavior and no `skill`
capability is published. Built-in Native Agent skills are a separate
Message Server feature.

The public `POST /mcp` remains the independent Dirextalk MCP server and keeps
agent-token, Origin and room-permission enforcement. Third-party MCP
installations do not share its lifecycle.

## 8. Cutover and rollback

No standalone Agent database is imported and no dual-write period exists.
Before cutover, the retired service must have zero non-terminal tasks, pending
confirmations, AWS changes, workload operations and unresolved uncertain
operations. Preserve a read-only backup.

Production order:

1. back up PostgreSQL, data directory and keyring;
2. deploy additive v97-v102 schema/runtime with capabilities not ready;
3. initialize the keyring and upgrade/verify model credentials;
4. enable capabilities only after secret/runtime readiness succeeds;
5. remove and revoke retired Agent Core credentials, endpoints and image;
6. archive the Agent repository read-only.

Rollback after secret upgrade must atomically restore both PostgreSQL and the
matching keyring backup; rolling back only the image is invalid.

## 9. Required verification

- PostgreSQL migrations, duplicate-version rejection, idempotency conflict,
  lease takeover, concurrency, confirmation reservation and restart recovery.
- AES-GCM round trip, wrong AAD fields, tamper, unknown key, dual-key reads,
  resumable upgrade and rotation; plaintext scans across DB/log/event/API.
- typed CloudFormation/SSM/ECS mocks including response loss, reconciliation
  and destroy readback; real-account acceptance before production enablement.
- HTTPS MCP initialize/tools-list/tools-call, grants, confirmation, timeout and
  ambiguous response; negative stdio/Skill/Core Runner cases.
- legacy `agent.core.*` DTOs, Native Agent stream and public `/mcp`.
- one-image Compose restart recovery with no Agent service/port/gRPC settings.
